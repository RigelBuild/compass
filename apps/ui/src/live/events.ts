// The SubscribeEvents stream driver (READ half of RIG-1729): turns the live server
// event stream into domain Issue[] snapshots for the board. Cold start pairs the durable
// ListBoardIssues re-snapshot with the live tail in one id-keyed map (re-sent id REPLACES);
// `resync_required`/fresh instance_epoch cold-starts. I/O here, wire→domain in ./adapt.

import type { CompassClient, SubscribeEventsResponse } from "@compass/client";
import type { Issue as DomainIssue, RuntimeMarker } from "../stub-data";
import { adaptIssue, adaptRuntimeMarker } from "./adapt";

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
	signal?: AbortSignal;
	onError?: (error: unknown) => void;
}

/** The payload oneof of a SubscribeEventsResponse — an indexed access on the
 *  named wire type so the switch stays exhaustive against the wire cases without
 *  importing every inner event message. */
type SubscribeEventsPayload = SubscribeEventsResponse["payload"];

/** Reconnect backoff bounds: the first retry waits up to RECONNECT_BASE_MS, each
 *  subsequent one doubles the ceiling up to RECONNECT_CAP_MS. Full jitter (a
 *  uniform draw in [0, ceiling]) spreads a fleet's reconnects so a server
 *  restart doesn't trigger a synchronized thundering herd. Same policy as
 *  runCommsStream. */
const RECONNECT_BASE_MS = 500;
const RECONNECT_CAP_MS = 30_000;

/** Await `ms`, resolving early if `signal` aborts — a teardown during backoff
 *  returns promptly instead of blocking out the full delay. */
function abortableDelay(ms: number, signal?: AbortSignal): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	if (signal?.aborted) {
		resolve();
		return promise;
	}
	// biome-ignore lint/style/noRestrictedGlobals: production abort-aware backoff delay (resolves early on signal), cleared on abort; not a test wait
	const timer = setTimeout(() => {
		signal?.removeEventListener("abort", onAbort);
		resolve();
	}, ms);
	const onAbort = () => {
		clearTimeout(timer);
		resolve();
	};
	signal?.addEventListener("abort", onAbort, { once: true });
	return promise;
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
	const { client, onIssues, onRuntime, signal, onError } = opts;
	// The board, deduped by issue id: the durable ListBoardIssues re-snapshot
	// plus live tail upserts both land here, so a re-sent id REPLACES rather
	// than appends — this map IS the union.
	const board = new Map<string, DomainIssue>();
	// Each agent account's latest runtime marker, keyed by account id. Same
	// upsert-by-key discipline as the board: a new status for an account
	// replaces its marker.
	const runtime = new Map<string, RuntimeMarker>();
	// The single stream cursor (echoed as since_seq) and the server's instance
	// epoch. Both 0 means a cold start → re-read + re-tail. Never persisted.
	let sinceSeq = 0n;
	let instanceEpoch = 0n;
	// Consecutive reconnect failures, driving the backoff ceiling. Reset to 0 the
	// moment a subscribe yields forward progress (the connection is live again).
	let failures = 0;

	const backoffBeforeReconnect = (): Promise<void> => {
		const ceiling = Math.min(
			RECONNECT_CAP_MS,
			RECONNECT_BASE_MS * 2 ** failures,
		);
		failures++;
		return abortableDelay(Math.random() * ceiling, signal);
	};

	const applyPayload = (payload: SubscribeEventsPayload): void => {
		if (payload.case === "agentSessionStatus") {
			// No resolvable account binding means nothing to key the marker by;
			// keying it under "" would attach one agent's posture to every
			// unbound status.
			const account = payload.value.agentAccountId;
			if (account === "") return;
			runtime.set(account, adaptRuntimeMarker(payload.value));
			onRuntime?.(new Map(runtime));
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
						failures = 0;
					}
				}
				if (resp.instanceEpoch) instanceEpoch = resp.instanceEpoch;

				// Apply an issue upsert for its board side-effect; a non-issue tail payload
				// is ignored. Progress is accounted on the cursor advance above.
				applyPayload(resp.payload);
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
