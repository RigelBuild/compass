// The SubscribeEvents stream driver: the orchestrator that turns the live
// server event stream (compass.v1 CompassService.SubscribeEvents) into a
// sequence of domain Issue[] snapshots for the board.
//
// This is the READ half of RIG-1729. It mirrors the comms driver's
// (./stream.ts runCommsStream) snapshot+tail shape: a cold-start subscription
// pairs the durable board re-snapshot read (ListBoardIssues) with the live
// SubscribeEvents tail, unioned into one board map and deduped by issue id
// (compass_pb.ts:326-346, the established snapshot_seq contract). The bus replays
// only a bounded event ring on connect, so the durable read is what covers
// issues older than the ring plus the ARCHIVED set the Done view needs.
//
// Protocol (compass_pb.ts:263-462):
//   1. subscribe(since_seq = last stream seq; 0 on a cold start). since_seq = 0
//      is a cold start: the first response is the snapshot boundary frame
//      carrying the opaque `snapshot_seq`. A positive cursor is a gap-free tail
//      resubscribe from that seq — no re-snapshot.
//   2. on the cold-start boundary, read ListBoardIssues(snapshot_seq) — the
//      whole board in all lifecycle states — and upsert each adapted issue into
//      the driver-local map. Subscribing BEFORE this read means the live tail
//      already covers the read's window; the id-keyed map absorbs the overlap
//      (a re-sent id REPLACES, never appends). The read is best-effort: a
//      failure (e.g. the server has not yet wired the handler) is reported via
//      onError and the driver keeps tailing rather than aborting the board.
//   3. each `{ case: "issue" }` tail payload is an upsert keyed by issue id —
//      apply it to the map and push the new [...map.values()].
//   4. the cursor advances from `SubscribeEventsResponse.seq` (never
//      snapshot_seq); the server's `instance_epoch` is stored and echoed on
//      reconnect so a restart forces a resync.
//   5. a `resync_required` (seq = 0, not a cursor) clears the map + resets the
//      cursor to a cold start and reconnects immediately for a fresh snapshot +
//      re-read; a fresh instance_epoch self-heals the same way.
//
// Only the `issue` payload is consumed here. The agent-lifecycle / server-status
// variants (agentSessionStatus, agentMessageChunk, agentToolCall, agentPlan,
// serverStatus) are gated on other lands and are safely ignored — never mapped
// to a placeholder, never thrown on.
//
// This module owns the I/O and control flow; the wire→domain mapping lives in
// ./adapt (adaptIssue). The store wires this to the `issues` signal
// (store.ts, behind options.compass).

import type { CompassClient, SubscribeEventsResponse } from "@compass/client";
import type { Issue as DomainIssue } from "../stub-data";
import { adaptIssue } from "./adapt";

/** What the driver needs to run: the compass client, the sink for each new
 *  board snapshot, and the abort signal that cancels the whole run (component
 *  unmount / app teardown). `onError` observes a non-fatal stream error before
 *  the driver reconnects (fatal = aborted). Mirrors CommsStreamOptions. */
export interface EventStreamOptions {
	client: CompassClient;
	/** Called with the full current board (upsert-deduped by issue id) after each
	 *  applied issue event. */
	onIssues: (issues: DomainIssue[]) => void;
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
	const { client, onIssues, signal, onError } = opts;
	// The board, deduped by issue id: the durable ListBoardIssues re-snapshot
	// plus live tail upserts both land here, so a re-sent id REPLACES rather
	// than appends — this map IS the union.
	const board = new Map<string, DomainIssue>();
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
		if (payload.case !== "issue") return;
		const issue = adaptIssue(payload.value);
		board.set(issue.id, issue);
		onIssues([...board.values()]);
	};

	// The durable board re-snapshot at the cold-start boundary. ListBoardIssues
	// returns the whole board in all lifecycle states (including ARCHIVED, which
	// the bounded event ring drops but the Done view needs); each issue upserts
	// into the map, unioned with the tail already subscribed above. `snapshotSeq`
	// is the opaque boundary token, passed verbatim — the driver never interprets
	// it. Best-effort: a failure (a server that has not wired the handler yet
	// returns Unimplemented) is reported via onError and the driver keeps tailing
	// rather than aborting the board — the live tail still populates it.
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
			// since_seq = 0 (cold start / post-resync) requests a fresh snapshot
			// boundary; the first response carries the opaque snapshot_seq to read
			// the durable board through. A positive cursor is a gap-free tail
			// resubscribe — no re-read.
			let pendingSnapshot = sinceSeq === 0n;
			const stream = client.subscribeEvents(
				{ sinceSeq, instanceEpoch },
				{ signal },
			);

			for await (const resp of stream) {
				if (resp.payload.case === "resyncRequired") {
					// The server can't serve our cursor gap-free. Clear the board +
					// reset both cursors to a cold start and reconnect immediately for a
					// fresh re-read + re-tail — a resync is a server directive, not a spin.
					board.clear();
					onIssues([]);
					sinceSeq = 0n;
					instanceEpoch = 0n;
					madeProgress = true;
					break;
				}

				// The first response on a cold start is the boundary frame carrying
				// the opaque snapshot_seq; read the durable board through it and union
				// it into the map. Subscribing before this read (above) means the live
				// tail already covers the read's window — dedup-by-id absorbs the
				// overlap. The boundary positions the cursor but is NOT progress (a
				// server that only replays it then closes is the spin the backoff
				// guards against).
				const isBoundary = pendingSnapshot;
				if (pendingSnapshot) {
					await readCatchUp(resp.snapshotSeq);
					pendingSnapshot = false;
				}

				// Advance the cursor from the stream's own seq, guarding against an
				// out-of-order redelivery lowering it. Every positioned response
				// advances it — even one whose payload we don't consume — so the
				// resubscribe cursor stays gap-free. Forward advancement from a real
				// (non-boundary) tail response is genuine progress: connection
				// liveness, not a board update, is the anti-spin signal, so an
				// agent-lifecycle tail on this shared stream keeps the next
				// resubscribe immediate and clears the backoff ceiling exactly as an
				// issue upsert does. The boundary frame positions the cursor but is
				// NOT progress — a server that only replays it then cleanly closes is
				// the tight loop the backoff guards against. Same policy as
				// runCommsStream.
				if (resp.seq > sinceSeq) {
					sinceSeq = resp.seq;
					if (!isBoundary) {
						madeProgress = true;
						failures = 0;
					}
				}
				if (resp.instanceEpoch) instanceEpoch = resp.instanceEpoch;

				// Apply an issue upsert for its board side-effect; a non-issue tail
				// payload is ignored (never mapped, never thrown on). Progress is
				// accounted on the cursor advance above, not on consumption here.
				applyPayload(resp.payload);
			}
			// A clean close with no forward progress is a spin a tight loop would
			// hammer; back off (escalating) before resubscribing. A close after real
			// events, or a resync directive, stays immediate.
			if (!madeProgress) await backoffBeforeReconnect();
		} catch (error) {
			if (signal?.aborted) return;
			onError?.(error);
			await backoffBeforeReconnect();
		}
	}
}
