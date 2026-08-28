// The AckCursor's progress signal (RIG-1364 C4b follow-up hardening).
//
// `appliedCount` is not bookkeeping — the Control source's no-progress
// reconnect budget reads it as its ONLY termination signal for a slow-flapping
// socket. If a redelivery can inflate it, a Runner that redelivers the same op
// forever reads as a session making progress and the budget never fires: the
// exact unbounded reconnect the budget replaced a wall-clock window to fix.
//
// So the contract under test is narrow and load-bearing: exactly one increment
// per GENUINELY NEW application, none for a re-ack, at either position relative
// to the contiguous cursor. Unit tests rather than socket-level cases because a
// redelivery landing above the cursor is a Runner behavior the live test server
// would have to fake anyway, and the counter is the thing being pinned.

import { expect, test } from "bun:test";
import type { PublishFrameRequest } from "./../../gen/compass/v1/agent_gateway_pb";
import type { PublishSpine } from "./../publish-spine";
import { AckCursor } from "./ack-cursor";

// A spine that only records. The cursor's emit path is covered at the socket
// level (control-source.test.ts); here the frames are incidental.
function recordingSpine(): {
	spine: PublishSpine;
	frames: PublishFrameRequest[];
} {
	const frames: PublishFrameRequest[] = [];
	return {
		frames,
		spine: {
			enqueueTrace: () => {},
			enqueuePriority: (f) => frames.push(f),
			droppedTraceCount: () => 0,
			failedPriorityCount: () => 0,
			drain: () => Promise.resolve(),
		},
	};
}

test("each genuinely new application increments appliedCount exactly once", () => {
	// Non-vacuity: drop the increment → the count stays 0 → red. Move it outside
	// the novelty branch → the re-ack tests below red instead.
	const { spine } = recordingSpine();
	const acks = new AckCursor(spine);
	expect(acks.appliedCount).toBe(0);
	acks.markApplied(1n);
	acks.markApplied(2n);
	acks.markApplied(3n);
	expect(acks.appliedCount).toBe(3);
	expect(acks.pendingAbove).toBe(0); // contiguous — nothing held above
});

test("a re-ack of a seq AT OR BELOW the contiguous cursor is not progress", () => {
	// The redelivery-of-a-retired-op path (amended OQ-6): the source re-acks so
	// the Runner retires it, and that must not read as the session getting
	// anything done.
	// Non-vacuity: without the `seq > #cursor` guard the count would climb on
	// every redelivery → red.
	const { spine } = recordingSpine();
	const acks = new AckCursor(spine);
	acks.markApplied(1n);
	acks.markApplied(2n);
	expect(acks.appliedCount).toBe(2);
	acks.markApplied(1n); // redelivered, already retired
	acks.markApplied(2n);
	expect(acks.appliedCount).toBe(2);
});

test("a re-ack of a seq applied OUT OF ORDER above the cursor is not progress either", () => {
	// The half `seq > #cursor` alone misses, and the one that matters most: seq 3
	// applied while 2 is still queued sits in `#above`, so it is applied AND
	// above the cursor. A redelivery of it satisfies `seq > #cursor` and would
	// increment on novelty-by-position alone.
	//
	// Non-vacuity (this is the regression this file was added for): drop the
	// `!#above.has(seq)` half of the guard → the redelivery below counts as
	// progress → appliedCount reads 3 instead of 2 → red. That is not a
	// cosmetic miscount: it is the reconnect budget being reset by a Runner
	// redelivering one op forever.
	const { spine } = recordingSpine();
	const acks = new AckCursor(spine);
	acks.markApplied(1n); // cursor → 1
	acks.markApplied(3n); // out of order: above = {3}, cursor stays 1
	expect(acks.appliedCount).toBe(2);
	expect(acks.pendingAbove).toBe(1);
	acks.markApplied(3n); // redelivered while still above the cursor
	expect(acks.appliedCount).toBe(2);
	expect(acks.pendingAbove).toBe(1); // and no duplicate entry
});

test("the contiguous-run prune collapses the set without inflating the count", () => {
	// Applying the gap seq advances the cursor through the whole run at once. The
	// count must move by ONE — the single new application — not by the length of
	// the run it unblocked.
	// Non-vacuity: increment per pruned entry inside the while loop → 5 instead
	// of 4 → red.
	const { spine } = recordingSpine();
	const acks = new AckCursor(spine);
	acks.markApplied(2n);
	acks.markApplied(3n);
	acks.markApplied(4n); // above = {2,3,4}, cursor 0
	expect(acks.appliedCount).toBe(3);
	expect(acks.pendingAbove).toBe(3);
	acks.markApplied(1n); // fills the gap → cursor sweeps to 4, above drains
	expect(acks.appliedCount).toBe(4);
	expect(acks.pendingAbove).toBe(0);
});

test("isApplied and the cursor are unchanged by a re-ack — idempotence still holds", () => {
	// The novelty guard tightened `markApplied`'s WRITE path; its dedup contract
	// (amended OQ-6) must be untouched. A redelivered op still re-acks so the
	// Runner retires it.
	// Non-vacuity: make the guard swallow the emit as well → the frame count
	// stops growing → red.
	const { spine, frames } = recordingSpine();
	const acks = new AckCursor(spine);
	acks.markApplied(1n);
	acks.markApplied(3n);
	const before = frames.length;
	acks.markApplied(3n);
	expect(frames.length).toBe(before + 1); // re-acked, so the Runner retires it
	expect(acks.isApplied(1n)).toBe(true);
	expect(acks.isApplied(3n)).toBe(true);
	expect(acks.isApplied(2n)).toBe(false); // the gap is still redeliverable
});
