// The frame boundary: the seam between the typed compass.v1 payloads the agent
// produces/consumes and the stdio bytes the Runner drives. Contract (design:
// spine-inversion + RIG-1570 tee): stdout `AgentFrame` oneof session/transcript_entry/
// delivery_ack, stdin `AgentControl` oneof prompt/steer/deliver/config/replay/complete.

// `OutboundFrame` below is the typed DOMAIN representation (one member per oneof variant,
// `kind` matching the generated `case` 1:1); the sink builds the real `AgentFrame` and
// protojson-serializes it, so the mapper/CompassAgent stay decoupled from the wire.

import {
	type AgentFrame,
	AgentFrameSchema,
	create,
	type DeliveryAck,
	type ForgeNotificationAck,
	type SessionFrame,
	type TranscriptEntry,
	toJson,
} from "./compassv1";

// One outbound frame the agent emits — exactly one `AgentFrame` oneof
// variant. A discriminated union so the envelope has a single place to stamp the
// oneof field, and the reader a single field to classify on.
export type OutboundFrame =
	| { readonly kind: "session"; readonly value: SessionFrame }
	// RIG-1570: one committed SDK session entry, teed upstream. A branded generated
	// message; `kind` matches the generated oneof case name 1:1 like every variant.
	| { readonly kind: "transcriptEntry"; readonly value: TranscriptEntry }
	// RIG-1310 §8: the agent's per-message delivery receipt for a turn-end delivery.
	// A branded generated message; `kind` matches the oneof case name 1:1, so the
	// sink stamps it generically (no ProtojsonLineSink change).
	| { readonly kind: "deliveryAck"; readonly value: DeliveryAck }
	// RIG-2732 W3: the agent's per-notification forge delivery receipt, emitted at
	// turn-end flush (T6). Correlates by subscription id and carries the notified
	// `revision` the Server advances delivered_revision to. A branded generated
	// message; `kind` matches the oneof case name 1:1 (generic stamp).
	| {
			readonly kind: "forgeNotificationAck";
			readonly value: ForgeNotificationAck;
	  };

// The sink the agent writes outbound frames to. The wire envelope lives
// entirely behind this interface.
export interface FrameSink {
	emit(frame: OutboundFrame): void;
	// RIG-1570 transcript lane: send one frame on the DURABLE unary and AWAIT its
	// commit, REJECTING on definitive give-up (inner-retry exhaustion). Unlike
	// `emit()` (void, silent give-up for loss-tolerable lanes), the tee backend
	// awaits this inside the per-path storage op and observes a definitive error.
	emitDurable(frame: OutboundFrame): Promise<void>;
	// Teardown barrier: resolve once every durable frame emitted has committed (or
	// definitively erred) and the send spine flushed, bounded by the shutdown
	// deadline. Optional because the loss-tolerable line sink has nothing to drain;
	// the socket sink awaits its in-flight unaries so shutdown abandons nothing.
	drain?(): Promise<void>;
}

// The sink builds the generated `AgentFrame` message — stamping the oneof from the
// domain variant's `kind` (matching the generated `case` names 1:1) — and renders it
// with canonical protojson via `toJson(AgentFrameSchema, …)`, one object per newline.
export class ProtojsonLineSink implements FrameSink {
	readonly #write: (line: string) => void;

	constructor(write: (line: string) => void) {
		this.#write = write;
	}

	emit(frame: OutboundFrame): void {
		// OutboundFrame is the generated `AgentFrame.frame` oneof with the discriminant
		// renamed `case`→`kind` for readability. The two unions are otherwise identical, so
		// the mapped init is the oneof init. TS cannot track a correlated rename, hence the
		// single assertion; it is checked by the frame.test.ts round-trips.
		const message = create(AgentFrameSchema, {
			frame: { case: frame.kind, value: frame.value } as AgentFrame["frame"],
		});
		this.#write(`${JSON.stringify(toJson(AgentFrameSchema, message))}\n`);
	}

	// The line sink writes in-body and never errs, so the durable lane is the
	// same write, resolved. Kept only to satisfy the FrameSink contract for the
	// retired stdio path; the socket sink carries the real R4 semantics.
	emitDurable(frame: OutboundFrame): Promise<void> {
		this.emit(frame);
		return Promise.resolve();
	}
}
