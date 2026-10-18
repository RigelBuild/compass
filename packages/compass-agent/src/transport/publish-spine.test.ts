// Direct fake-publish tests pin the spine's retry bound and prove drain settles after a failed-send storm.

import { expect, test } from "bun:test";
import { create } from "@bufbuild/protobuf";
import {
	type PublishFrameRequest,
	PublishFrameRequestSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import { createPublishSpine, PUBLISH_BATCH_MAX } from "./publish-spine";

// A dead socket gets one initial send plus a pump-run-scoped retry ladder; later batches are given up without more sleeps.
const PRIORITY_LADDER_RETRIES = 3;

function priorityFrame(): PublishFrameRequest {
	return create(PublishFrameRequestSchema, {});
}

test("drain() is bounded ~O(1) on a persistently-dead socket, not unbounded", async () => {
	// One pump-run retry budget bounds all priority batches; drain must still resolve after it is exhausted.
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
	// After the retry budget is spent, each remaining batch gets one immediate give-up send and no further sleep.
	expect(publishCalls).toBeLessThanOrEqual(PRIORITY_LADDER_RETRIES + 2);
});

test("reset-on-success gives each recovered batch a fresh retry budget", async () => {
	// A successful send must reset the pump-run budget so the next batch gets a fresh retry ladder.
	// Without reset, the second batch is given up instead of recovering.
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
