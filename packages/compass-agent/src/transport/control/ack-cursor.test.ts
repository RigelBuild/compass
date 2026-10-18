// AckCursor counts only new applications because the reconnect budget uses this as its progress signal.

import { expect, test } from "bun:test";
import { toBinary } from "@bufbuild/protobuf";
import type { PublishFrameRequest } from "./../../gen/compass/v1/agent_gateway_pb";
import { PublishFrameRequestSchema } from "./../../gen/compass/v1/agent_gateway_pb";
import type { ControlAck } from "./../../gen/compass/v1/agent_pb";
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

function controlAcks(frames: PublishFrameRequest[]): ControlAck[] {
	return frames.flatMap((frame) => {
		const value = frame.frame?.frame;
		return value?.case === "controlAck" ? [value.value] : [];
	});
}

test("range-encodes a long applied run in a small ControlAck", () => {
	const { spine, frames } = recordingSpine();
	const acks = new AckCursor(spine);
	for (let seq = 2n; seq <= 2000n; seq += 1n) acks.markApplied(seq);

	const emitted = controlAcks(frames);
	expect(emitted).toHaveLength(1999);
	const lastFrame = frames.at(-1);
	if (!lastFrame) throw new Error("expected final ControlAck frame");
	expect(toBinary(PublishFrameRequestSchema, lastFrame).length).toBeLessThan(
		64,
	);
	for (const ack of emitted) {
		expect(ack.appliedAbove).toEqual([]);
		expect(ack.appliedAboveRanges.length).toBeLessThanOrEqual(2);
	}
});

test("encodes each maximal contiguous applied run once", () => {
	const { spine, frames } = recordingSpine();
	const acks = new AckCursor(spine);
	for (const seq of [3n, 4n, 5n, 8n, 10n, 11n]) acks.markApplied(seq);

	const lastAck = controlAcks(frames).at(-1);
	expect(lastAck?.ackedSeq).toBe(0n);
	expect(lastAck?.appliedAboveRanges).toEqual([3n, 5n, 8n, 8n, 10n, 11n]);
});

test("applying the cursor gap collapses the applied run and clears ranges", () => {
	const { spine, frames } = recordingSpine();
	const acks = new AckCursor(spine);
	for (let seq = 2n; seq <= 2000n; seq += 1n) acks.markApplied(seq);
	acks.markApplied(1n);

	const lastAck = controlAcks(frames).at(-1);
	expect(lastAck?.ackedSeq).toBe(2000n);
	expect(lastAck?.appliedAbove).toEqual([]);
	expect(lastAck?.appliedAboveRanges).toEqual([]);
});

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
	// Re-acks after the cursor advances must not inflate appliedCount.
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
	// An out-of-order applied op is already in #above; re-acking it must not count as new progress.
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
	// Filling a sequence gap advances the cursor but counts only the newly applied op.
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
	// Deduplicating progress must not suppress the ack that retires a redelivered op.
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
