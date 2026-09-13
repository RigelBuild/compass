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

// The selective apply-ack cursor: the highest CONTIGUOUS applied `control_seq` plus the set
// of seqs applied out of order above it (`applied_above`, invariant 2). That set is
// UNBOUNDED: a queued-but-unapplied iterator op pins the cursor while immediate ops above
// accumulate, draining only once the held op lands.

// `applied_above` serializes in full into EVERY ack, so wire cost over a long turn is
// quadratic and never dropped; range-encoding is the fix (RIG-1466). `markApplied` is
// idempotent — a redelivered applied op re-acks without corrupting the cursor. Every apply
// emits a `ControlAck` on the priority lane.
export class AckCursor {
	readonly #spine: PublishSpine;
	#cursor = 0n;
	readonly #above = new Set<bigint>();
	#applied = 0;

	constructor(spine: PublishSpine) {
		this.#spine = spine;
	}

	// Current size of the out-of-order applied set — the observability seam for
	// the unbounded growth documented above; range-encoding lands in RIG-1466.
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
		// `seq > #cursor` alone is NOT novelty: a seq already in `#above` satisfies it too,
		// and a redelivery re-acks through here. Test membership as well, or a re-ack would
		// increment `#applied` and read as progress — resetting the no-progress reconnect
		// budget on a Runner that redelivers forever, the shape that budget exists to terminate.
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
