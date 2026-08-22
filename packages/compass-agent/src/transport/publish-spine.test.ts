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
	// The budget caps backoff, not send count: the first batch costs its initial
	// send plus PRIORITY_LADDER_RETRIES retries, then the second batch still
	// incurs one publish() call whose failure is a definitive give-up with NO
	// further retry or sleep — so the +2 covers the initial send plus that single
	// give-up send. Wall-clock cost stays O(1) (no more backoff sleeps after the
	// budget is spent); send count is O(batches) of immediate rejects, not
	// one-per-frame and not unbounded.
	expect(publishCalls).toBeLessThanOrEqual(PRIORITY_LADDER_RETRIES + 2);
});

test("reset-on-success gives each recovered batch a fresh retry budget", async () => {
	// Contract: the pump-run-scoped retry budget RESETS to zero on any successful
	// send (publish-spine.ts:205), making it a per-recovery budget, not a
	// per-drain one — each batch that eventually lands gets the FULL ladder again.
	// A single flap-then-recover does NOT distinguish this from "no reset": one
	// batch that recovers within the ladder never re-exhausts the budget, so the
	// reset is inconsequential there. This case forces a TWO-PHASE flap where the
	// second batch can only land if the first's success reset the budget:
	//   - Frame A: fail twice (ladder 0→1→2), deliver on the 3rd send — the reset.
	//   - Frame B, enqueued only AFTER A lands so it forms its OWN batch
	//     (enqueuePriority refuses frames once drain() sets `ended`, so B must go
	//     in before drain): fail three times, deliver on the 4th send. B's third
	//     retry is reachable ONLY on a fresh ladder — i.e. only if A's success
	//     reset priorityRetries to 0.
	//
	// Non-vacuity (mutation-verified): delete the reset (`priorityRetries = 0`,
	// publish-spine.ts:205). A's two failures then leave the budget at 2, so B's
	// FIRST failure hits `priorityRetries >= PRIORITY_BATCH_RETRY_MS.length` and B
	// is given up instead of retried: failedPriorityCount() becomes 1 (not 0) and
	// publishCalls becomes 5 (not 7). A still lands on call 3, so the mutant reds
	// on the assertions rather than hanging.
	let publishCalls = 0;
	let markADelivered!: () => void;
	const aDelivered = new Promise<void>((resolve) => {
		markADelivered = resolve;
	});
	const publish = (): Promise<unknown> => {
		publishCalls += 1;
		// Phase A: calls 1-2 fail, call 3 delivers A and resets the budget.
		if (publishCalls <= 2) return Promise.reject(new Error("flap A"));
		if (publishCalls === 3) {
			markADelivered();
			return Promise.resolve({});
		}
		// Phase B: calls 4-6 fail, call 7 delivers B — reachable only if A's
		// success handed B a fresh 3-retry ladder.
		if (publishCalls <= 6) return Promise.reject(new Error("flap B"));
		return Promise.resolve({});
	};
	const spine = createPublishSpine(publish);
	spine.enqueuePriority(priorityFrame()); // A
	// Wait for A to actually land so B is taken as a SEPARATE batch — a B enqueued
	// while A is still retrying would be re-unshifted into A's batch and defeat
	// the two-phase distinction.
	await aDelivered;
	spine.enqueuePriority(priorityFrame()); // B, fresh batch on the reset budget
	await spine.drain();
	// Both frames were delivered on their recovered send — no never-drop loss, no
	// trace loss.
	expect(spine.failedPriorityCount()).toBe(0);
	expect(spine.droppedTraceCount()).toBe(0);
	// A: 2 fail + 1 ok = 3; B: 3 fail + 1 ok = 4; total 7. Without the reset B is
	// given up after 2 sends, making this 5.
	expect(publishCalls).toBe(7);
});
