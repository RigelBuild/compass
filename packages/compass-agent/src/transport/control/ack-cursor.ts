// The socket ControlSource's selective apply-ack cursor. Split out of
// `control-source.ts`: the contiguous-cursor + applied-above bookkeeping is the
// subtlest invariant in the source (amended OQ-6, invariant 2) and reads better
// beside its own doc than buried mid-file.

import { create } from "@bufbuild/protobuf";
import { PublishFrameRequestSchema } from "./../../gen/compass/v1/agent_gateway_pb";
import {
	AgentFrameSchema,
	ControlAckSchema,
	ReplayCompleteAckSchema,
} from "./../../gen/compass/v1/agent_pb";
import type { PublishSpine } from "./../publish-spine";

// The selective apply-ack cursor: the highest CONTIGUOUS applied `control_seq`
// plus the set of seqs applied out of order above it (`applied_above`,
// invariant 2). That set is UNBOUNDED — it has no cap, and nothing about a turn
// bounds its size. The cursor only advances through a contiguous run, so a
// single queued-but-unapplied iterator op pins it while every immediate op
// above it accumulates — invariant 2's intended interleaving, not a pathology.
// It drains completely once the held op lands, but until then it grows without
// limit. Note `applied_above` is serialized in full into EVERY ack (see
// `#emit`), so the wire cost over a long turn is quadratic: one op held across
// 1999 immediate applies serializes 1,999,000 seqs onto the priority lane,
// which is never dropped — unlike the trace queue's `TRACE_QUEUE_CAP`
// drop-oldest there is no backstop here. Range-encoding `applied_above` is the
// fix and lands in SEA-1466; this file only documents and exposes the growth.
// `markApplied` is idempotent — a redelivered already-applied op
// re-acks (so the Runner retires it) without corrupting the cursor. Every apply
// emits a `ControlAck` on the shared Publish spine's priority lane; the Runner
// retires retained ops up to the cursor and drops the individually-acked ones.
export class AckCursor {
	readonly #spine: PublishSpine;
	#cursor = 0n;
	readonly #above = new Set<bigint>();
	#applied = 0;

	constructor(spine: PublishSpine) {
		this.#spine = spine;
	}

	// Current size of the out-of-order applied set — the observability seam for
	// the unbounded growth documented above; range-encoding lands in SEA-1466.
	get pendingAbove(): number {
		return this.#above.size;
	}

	// Monotonic count of ops actually applied. The Control source's reconnect
	// budget reads this as its progress signal: a re-ack of an already-applied
	// seq is not progress and does not increment it.
	get appliedCount(): number {
		return this.#applied;
	}

	// True once the seq is durably applied from the source's view: at or below the
	// contiguous cursor (retired), or individually applied above it. Drives dedup
	// of a redelivered op.
	isApplied(seq: bigint): boolean {
		return seq <= this.#cursor || this.#above.has(seq);
	}

	// Record an op applied and emit the resulting ControlAck. Advances the
	// contiguous cursor as far as the applied set allows, pruning subsumed
	// out-of-order entries. Idempotent for an already-applied seq (re-ack only).
	markApplied(seq: bigint): void {
		// `seq > #cursor` alone is NOT novelty: a seq already in `#above` (applied
		// out of order, still above the contiguous cursor) satisfies it too, and a
		// redelivery of such an op re-acks through here. Test membership as well,
		// or that re-ack would increment `#applied` and read as progress —
		// resetting the source's no-progress reconnect budget on a Runner that
		// redelivers the same op forever, which is the shape that budget exists to
		// terminate.
		if (seq > this.#cursor && !this.#above.has(seq)) {
			this.#above.add(seq);
			while (this.#above.delete(this.#cursor + 1n)) this.#cursor += 1n;
			this.#applied += 1;
		}
		this.#spine.enqueuePriority(
			create(PublishFrameRequestSchema, {
				frame: create(AgentFrameSchema, {
					frame: {
						case: "controlAck",
						value: create(ControlAckSchema, {
							ackedSeq: this.#cursor,
							appliedAbove: [...this.#above],
						}),
					},
				}),
			}),
		);
	}

	// Emit the replay-barrier ack: on receipt the Runner releases the live ops it
	// held behind the restart replay barrier. Rides the same priority lane, ahead
	// of the ControlAck for the replayComplete op that triggers it.
	emitReplayCompleteAck(): void {
		this.#spine.enqueuePriority(
			create(PublishFrameRequestSchema, {
				frame: create(AgentFrameSchema, {
					frame: {
						case: "replayCompleteAck",
						value: create(ReplayCompleteAckSchema, {}),
					},
				}),
			}),
		);
	}
}
