// The frame boundary: the seam between the typed compass.v1 payloads the agent
// produces/consumes and the bytes on the stdio channel the Runner drives.
//
// The frame CONTRACT is frozen (design: architecture-lineage, spine-inversion;
// extended by SEA-1570 with the transcript-tee lane):
//   - stdout: `AgentFrame` — oneof frame {
//         SessionFrame session; TranscriptEntry transcript_entry;
//         DeliveryAck delivery_ack }
//     The set oneof field IS the type discriminator; an unset/unrecognized
//     field is the "unknown frame" the Runner logs + counts. The opaque
//     OMP-native execution trace + board lifecycle ride the single `session`
//     variant (SessionFrame) → the session-tail Publish spine. TRANSCRIPT
//     (SEA-1570) rides the single `transcript_entry` variant (TranscriptEntry):
//     one committed SDK session entry the tee backend commits locally and
//     forwards on the DURABLE conversation-frame lane (never the droppable
//     Publish spine) so the Server can reconstruct the session on resume.
//   - stdin: `AgentControl` — oneof control {
//         PromptControl prompt; SteerControl steer; DeliverControl deliver;
//         ConfigControl config; TranscriptReplay replay; ReplayComplete
//         replay_complete }.
//
// `AgentFrame` is an internal-only additive proto message generated with the
// agent's proto via the path-filtered gen lane (not the public client surface);
// `AgentFrameSchema` lives in ./gen. `OutboundFrame` below is the typed DOMAIN
// representation the mapping produces — one member per `AgentFrame` oneof
// variant, with `kind` matching the generated oneof `case` names 1:1 — so the
// sink builds the real `AgentFrame` message and protojson-serializes it via
// `toJson(AgentFrameSchema, …)`. The mapping and the CompassAgent class
// produce/consume `OutboundFrame` and stay decoupled from the wire envelope,
// which lives entirely behind this file.

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

// One outbound frame the agent emits — exactly one frozen `AgentFrame` oneof
// variant. A discriminated union so the envelope has a single place to stamp the
// oneof field, and the reader a single field to classify on.
export type OutboundFrame =
	| { readonly kind: "session"; readonly value: SessionFrame }
	// SEA-1570: one committed SDK session entry, teed upstream. `value` is a
	// branded generated message (`create(TranscriptEntrySchema, …)`), and `kind`
	// matches the generated oneof case name 1:1 like every other variant.
	| { readonly kind: "transcriptEntry"; readonly value: TranscriptEntry }
	// SEA-1310 §8: the agent's per-message delivery receipt for a turn-end
	// delivery. `value` is a branded generated message (`create(DeliveryAckSchema,
	// …)`) and `kind` matches the generated oneof case name 1:1 like every other
	// variant, so the sink stamps it generically (no ProtojsonLineSink change).
	| { readonly kind: "deliveryAck"; readonly value: DeliveryAck }
	// RIG-2732 W3: the agent's per-notification forge delivery receipt, emitted at
	// turn-end flush (T6). Correlates to the subscription by id and carries the
	// notified `revision` the Server advances delivered_revision to. `value` is a
	// branded generated message (`create(ForgeNotificationAckSchema, …)`) and
	// `kind` matches the generated oneof case name 1:1, so the sink stamps it
	// generically (no ProtojsonLineSink change).
	| {
			readonly kind: "forgeNotificationAck";
			readonly value: ForgeNotificationAck;
	  };

// The sink the agent writes outbound frames to. The wire envelope lives
// entirely behind this interface.
export interface FrameSink {
	emit(frame: OutboundFrame): void;
	// SEA-1570 transcript lane: send one frame on the DURABLE unary and AWAIT its
	// commit, REJECTING on definitive give-up (inner-retry exhaustion). Unlike
	// `emit()` — which stays void + silent-give-up for the loss-tolerable
	// conversation/session lanes — the tee backend awaits this inside the
	// per-path storage op (so per-session emit order == send order == commit
	// order) and observes a definitive error so it can buffer/retry/fatal (R4).
	// The frame still rides the same delivered-or-erred unary and is retained for
	// drain(); only the give-up signalling differs.
	emitDurable(frame: OutboundFrame): Promise<void>;
	// Teardown barrier: resolve once every durable frame already emitted has been
	// committed (or definitively erred) and the send spine flushed, bounded by
	// the caller's shutdown deadline. Optional because the loss-tolerable
	// stdio-line sink (retired at C5) has nothing to drain; the socket sink, whose
	// conversation frames are delivered-or-erred, awaits its in-flight unaries
	// here so shutdown cannot abandon an uncommitted conversation frame.
	drain?(): Promise<void>;
}

// INTERIM no longer: the sink builds the generated `AgentFrame` message —
// stamping the oneof from the domain variant's `kind` (which matches the
// generated `case` names 1:1) — and renders it with protobuf-es's canonical
// protojson via `toJson(AgentFrameSchema, …)`, one object per newline. The
// reader classifies each line by the single set `oneof` field.
export class ProtojsonLineSink implements FrameSink {
	readonly #write: (line: string) => void;

	constructor(write: (line: string) => void) {
		this.#write = write;
	}

	emit(frame: OutboundFrame): void {
		// OutboundFrame is the generated `AgentFrame.frame` oneof with the
		// discriminant renamed `case`→`kind` (readability across the mapper +
		// tests). The two unions are otherwise identical — the 3 `kind`s are the
		// 3 generated `case`s, and each `value` is the matching payload — so the
		// mapped init is the oneof init. TS can't track a correlated rename, hence
		// the single assertion; it is checked by the frame.test.ts round-trips.
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
