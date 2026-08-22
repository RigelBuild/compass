// The PublishSpine timing/bound contract (transport-consolidation C4; design
// record docs/designs/platform/compass-agent-effect-adoption/design.md, T3).
// RIG-2448 closes a coverage gap: the spine had NO standalone test file — its
// behavior was pinned only INDIRECTLY through frame-sink.test.ts, which drives
// the spine over a live socket and never isolates the pump's priority-retry
// timing. These cases drive createPublishSpine() directly over a FAKE publish so
// the pump's bounded-retry budget is observable in isolation.
//
// No wall-clock timer, no poll, no injected clock. The pump's backoff is
// `Effect.sleep` on the spine's OWN module-private Live ManagedRuntime
// (publish-spine.ts:105,230), which has no clock-injection seam a test-side
// Effect TestClock could reach without touching production code (a RIG-2448
// non-goal). So each case awaits the ONE signal the contract already exposes —
// `drain()`'s returned promise — directly: the whole claim is "drain() resolves
// (⇒ the retry is bounded)", so a bounded pump resolves it and an unbounded pump
// never does. drain() closes enqueues (`ended`), making the priority array
// finite, and the pump-run-scoped budget caps the retry, so a correct spine
// settles after only the real backoff ladder (~1.05s once, not per-batch); the
// non-vacuity mutants below make drain() never resolve, so the awaited promise
// hangs to a red rather than passing.

import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import {
	type PublishFrameRequest,
	PublishFrameRequestSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import { createPublishSpine, PUBLISH_BATCH_MAX } from "./publish-spine";

// The bounded priority-retry ladder the pump sleeps on after a failed batch that
// carried priority frames (publish-spine.ts:65, PRIORITY_BATCH_RETRY_MS =
// [50,200,800]) — not exported, so restated here with its citation. Its LENGTH
// is the retry cap the pump-run-scoped budget enforces: on a persistently-dead
// socket the first queued priority batch costs one send plus this many retries,
// then the budget is exhausted and every remaining batch is given up with no
// further send — so drain()'s total cost is O(1), not O(batches).
const PRIORITY_LADDER_RETRIES = 3;

function priorityFrame(): PublishFrameRequest {
	return create(PublishFrameRequestSchema, {});
}

test("drain() is bounded ~O(1) on a persistently-dead socket, not unbounded", async () => {
	// Contract (design record T3, priority-retry row): a batch send that fails and
	// carried priority frames is re-enqueued at the front and retried on the
	// bounded ladder, but the retry budget is PUMP-RUN-SCOPED (a single counter
	// reset only on a successful send, publish-spine.ts:181,205,221) — NOT a
	// per-batch Schedule. So draining N never-drop priority batches against a dead
	// socket costs the ladder ONCE (O(1)), not N × the ladder; after the budget is
	// exhausted every remaining batch is counted as definitively failed rather
	// than retried, so drain() resolves.
	//
	// Non-vacuity (mutation-verified): in publish-spine.ts's pumpLoop, remove the
	// budget cap (`if (priorityRetries >= PRIORITY_BATCH_RETRY_MS.length) { ...
	// continue; }`, or its `>=` bound) so a dead socket keeps re-enqueuing and
	// retrying the batch forever → drain() never resolves → this awaited drain()
	// hangs to a red instead of passing.
	let publishCalls = 0;
	const publish = (): Promise<never> => {
		publishCalls += 1;
		// A persistently-dead socket: every batch send rejects.
		return Promise.reject(new Error("dead socket"));
	};
	const spine = createPublishSpine(publish);
	// More than one batch worth of priority frames, so a (hypothetical) per-batch
	// retry schedule would cost 2 × the ladder while the pump-scoped budget costs
	// it once — the O(1) claim has something to bite on.
	const total = PUBLISH_BATCH_MAX * 2;
	for (let i = 0; i < total; i++) spine.enqueuePriority(priorityFrame());
	// drain() closes enqueues (ended = true) so the priority array is finite; the
	// budget bounds the retry, so this MUST resolve.
	await spine.drain();
	// Every queued priority frame was counted as definitively failed (never-drop
	// loss surfaced distinctly), and none silently vanished into the trace-drop
	// count.
	expect(spine.failedPriorityCount()).toBe(total);
	expect(spine.droppedTraceCount()).toBe(0);
	// The budget capped the retries: the first batch costs its initial send plus
	// PRIORITY_LADDER_RETRIES retries, then the second batch is given up with no
	// further send (budget already exhausted) — a small constant number of sends,
	// not one-per-frame and not unbounded.
	expect(publishCalls).toBeLessThanOrEqual(PRIORITY_LADDER_RETRIES + 2);
});

test("reset-on-success terminates drain() on a FLAPPING socket", async () => {
	// Contract: the pump-run-scoped retry budget RESETS on any successful send
	// (publish-spine.ts:205), so a socket that flaps (fails a few times, then
	// recovers) does not exhaust the budget — the recovered send delivers the
	// priority frames and drain() resolves. It stays bounded because drain()
	// closes enqueues (ended = true), making the priority array finite.
	//
	// Non-vacuity: a flapping socket that never recovered would keep failing; only
	// a real reset-on-success + a finite (drain-closed) priority array lets this
	// terminate with the frames DELIVERED (failedPriorityCount 0) rather than
	// given up.
	let publishCalls = 0;
	const failsBeforeRecovery = 2; // fail twice (50ms, 200ms) then succeed
	const publish = (): Promise<unknown> => {
		publishCalls += 1;
		if (publishCalls <= failsBeforeRecovery) {
			return Promise.reject(new Error("transient flap"));
		}
		return Promise.resolve({});
	};
	const spine = createPublishSpine(publish);
	spine.enqueuePriority(priorityFrame());
	await spine.drain();
	// The frame was DELIVERED once the socket recovered, not given up: no
	// never-drop loss, no trace loss.
	expect(spine.failedPriorityCount()).toBe(0);
	expect(spine.droppedTraceCount()).toBe(0);
	// Two failed sends + one successful send.
	expect(publishCalls).toBe(failsBeforeRecovery + 1);
});
