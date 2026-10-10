// The SubscribeEvents stream driver (READ half of RIG-1729): turns the live server
// event stream into domain Issue[] snapshots for the board. Cold start pairs the durable
// ListBoardIssues re-snapshot with the live tail in one id-keyed map (re-sent id REPLACES);
// `resync_required`/fresh instance_epoch cold-starts. I/O here, wire→domain in ./adapt.

import {
	AgentSessionState,
	type CompassClient,
	type SubscribeEventsResponse,
} from "@compass/client";
import type { Issue as DomainIssue, RuntimeMarker } from "../stub-data";
import { adaptIssue, adaptRuntimeMarker } from "./adapt";
import { createReconnectBackoff } from "./backoff";

/** An agent's session status and most recent WORKING-to-READY event time. */
export interface AccountSession {
	readonly sessionId: string;
	readonly state: AgentSessionState;
	readonly turnEndedAtUnixMs?: number;
}

/** What the driver needs to run: the compass client, the sink for each new
 *  board snapshot, and the abort signal that cancels the whole run (component
 *  unmount / app teardown). `onError` observes a non-fatal stream error before
 *  the driver reconnects (fatal = aborted). Mirrors CommsStreamOptions. */
export interface EventStreamOptions {
	client: CompassClient;
	/** Called with the full current board (upsert-deduped by issue id) after each
	 *  applied issue event. */
	onIssues: (issues: DomainIssue[]) => void;
	/** Called with each agent account's latest runtime marker, keyed by account
	 *  id. A status whose account binding is no longer resolvable carries no
	 *  account and is skipped rather than keyed under an empty id. */
	onRuntime?: (runtime: ReadonlyMap<string, RuntimeMarker>) => void;
	/** Called with account sessions; clears on resync and retains turn-end times. */
	onSessions?: (sessions: ReadonlyMap<string, AccountSession>) => void;
	signal?: AbortSignal;
	onError?: (error: unknown) => void;
}

/** The payload oneof of a SubscribeEventsResponse — an indexed access on the
 *  named wire type so the switch stays exhaustive against the wire cases without
 *  importing every inner event message. */
type SubscribeEventsPayload = SubscribeEventsResponse["payload"];

function turnEndForStatus(
	previous: AccountSession | undefined,
	sessionId: string,
	state: AgentSessionState,
	atUnixMs: number,
): number | undefined {
	if (!previous || previous.sessionId !== sessionId) return undefined;
	if (
		previous.state === AgentSessionState.WORKING &&
		state === AgentSessionState.READY
	)
		return atUnixMs;
	return previous.turnEndedAtUnixMs;
}

/** Run the SubscribeEvents stream until `signal` aborts. Maintains the board as
 *  a driver-local `Map<id, Issue>` plus the single stream cursor + instance
 *  epoch across reconnects. A cold-start subscription (since_seq = 0) reads the
 *  durable board (ListBoardIssues) at the boundary frame and unions it with the
 *  tail — deduped by id in the map; a clean drop resubscribes gap-free from the
 *  cursor; a `resync_required` clears the map + resets the cursor to a cold
 *  start (re-read + re-tail). Each applied issue pushes the full board to
 *  `onIssues`. Resolves only when aborted. */
export async function runEventStream(opts: EventStreamOptions): Promise<void> {
	const { client, onIssues, onRuntime, onSessions, signal, onError } = opts;
	// The board, deduped by issue id: the durable ListBoardIssues re-snapshot
	// plus live tail upserts both land here, so a re-sent id REPLACES rather
	// than appends — this map IS the union.
	const board = new Map<string, DomainIssue>();
	// Each agent account's latest runtime marker, keyed by account id. Same
	// upsert-by-key discipline as the board: a new status for an account
	// replaces its marker.
	const runtime = new Map<string, RuntimeMarker>();
	const sessions = new Map<string, AccountSession>();
	// The single stream cursor (echoed as since_seq) and the server's instance
	// epoch. Both 0 means a cold start → re-read + re-tail. Never persisted.
	let sinceSeq = 0n;
	let instanceEpoch = 0n;
	// Reset the moment a subscribe yields forward progress (the connection is live again).
	const backoff = createReconnectBackoff(signal);
	const backoffBeforeReconnect = backoff.wait;

	const applyPayload = (
		payload: SubscribeEventsPayload,
		atUnixMs: number,
	): void => {
		if (payload.case === "agentSessionStatus") {
			// No resolvable account binding means nothing to key the marker by;
			// keying it under "" would attach one agent's posture to every
			// unbound status.
			const account = payload.value.agentAccountId;
			if (account === "") return;
			runtime.set(account, adaptRuntimeMarker(payload.value));
			onRuntime?.(new Map(runtime));
			const previous = sessions.get(account);
			const turnEndedAtUnixMs = turnEndForStatus(
				previous,
				payload.value.sessionId,
				payload.value.state,
				atUnixMs,
			);
			sessions.set(account, {
				sessionId: payload.value.sessionId,
				state: payload.value.state,
				...(turnEndedAtUnixMs === undefined ? {} : { turnEndedAtUnixMs }),
			});
			onSessions?.(new Map(sessions));
			return;
		}
		if (payload.case !== "issue") return;
		const issue = adaptIssue(payload.value);
		board.set(issue.id, issue);
		onIssues([...board.values()]);
	};

	// The durable board re-snapshot at the cold-start boundary. ListBoardIssues returns
	// the whole board in all lifecycle states (including ARCHIVED, which the event ring
	// drops but the Done view needs); each issue upserts into the map. Best-effort: a
	// failure (unwired handler → Unimplemented) is reported via onError and tailing continues.
	const readCatchUp = async (snapshotSeq: bigint): Promise<void> => {
		try {
			const resp = await client.listBoardIssues({ snapshotSeq }, { signal });
			for (const wire of resp.issues) {
				const issue = adaptIssue(wire);
				board.set(issue.id, issue);
			}
			onIssues([...board.values()]);
		} catch (error) {
			if (signal?.aborted) return;
			onError?.(error);
		}
	};

	while (!signal?.aborted) {
		let madeProgress = false;
		try {
			// since_seq = 0 (cold start / post-resync) requests a fresh snapshot boundary;
			// the first response carries snapshot_seq. A positive cursor is a gap-free
			// tail resubscribe — no re-read.
			let pendingSnapshot = sinceSeq === 0n;
			const stream = client.subscribeEvents(
				{ sinceSeq, instanceEpoch },
				{ signal },
			);

			for await (const resp of stream) {
				if (resp.payload.case === "resyncRequired") {
					// The server can't serve our cursor gap-free. Clear the board + reset both
					// cursors to a cold start and reconnect immediately — a resync is a server
					// directive, not a spin. Runtime markers clear too: a stale posture could
					// show a torn-down host session as still contained.
					board.clear();
					onIssues([]);
					runtime.clear();
					onRuntime?.(new Map());
					sessions.clear();
					onSessions?.(new Map());
					sinceSeq = 0n;
					instanceEpoch = 0n;
					madeProgress = true;
					break;
				}

				// The first cold-start response is the boundary frame carrying snapshot_seq;
				// read the durable board through it and union into the map (dedup-by-id
				// absorbs the tail overlap). The boundary positions the cursor but is NOT
				// progress — a replay-then-close is the spin the backoff guards against.
				const isBoundary = pendingSnapshot;
				if (pendingSnapshot) {
					await readCatchUp(resp.snapshotSeq);
					pendingSnapshot = false;
				}

				// Advance the cursor from the stream's own seq, guarding an out-of-order
				// redelivery from lowering it. Every positioned response advances it (even an
				// unconsumed one) so resubscribe stays gap-free. A real tail response is
				// progress; the boundary frame is NOT. Same policy as runCommsStream.
				if (resp.seq > sinceSeq) {
					sinceSeq = resp.seq;
					if (!isBoundary) {
						madeProgress = true;
						backoff.reset();
					}
				}
				if (resp.instanceEpoch) instanceEpoch = resp.instanceEpoch;

				// Apply an issue upsert for its board side-effect; a non-issue tail payload
				// is ignored. Progress is accounted on the cursor advance above.
				applyPayload(resp.payload, Number(resp.atUnixMs));
			}
			// A clean close with no forward progress is a spin a tight loop would hammer;
			// back off (escalating). A close after real events, or a resync, stays immediate.
			if (!madeProgress) await backoffBeforeReconnect();
		} catch (error) {
			if (signal?.aborted) return;
			onError?.(error);
			await backoffBeforeReconnect();
		}
	}
}
