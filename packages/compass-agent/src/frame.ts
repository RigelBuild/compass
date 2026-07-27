// The frame boundary: the seam between the typed compass.v1 payloads the agent
// produces/consumes and the bytes on the stdio channel the Runner drives.
//
// The frame CONTRACT is frozen (design compass-0.6 §T5, spine-inversion):
//   - stdout: `AgentFrame` — oneof frame {
//         MessagePosted conversation_posted; MessageUpdated conversation_updated;
//         SessionFrame session }
//     The set oneof field IS the type discriminator; an unset/unrecognized
//     field is the "unknown frame" the Runner logs + counts. CONVERSATION
//     (text/ask) rides MessagePosted/MessageUpdated (each wraps a Message of
//     MessageBlocks) → comms; the opaque OMP-native execution trace + board
//     lifecycle ride the single `session` variant (SessionFrame) → the
//     session-tail stream. Dual-surface split: the Runner write-throughs each
//     variant to the surface that owns it.
//   - stdin: `AgentControl` — oneof control {
//         PromptControl prompt; SteerControl steer; AskAnswerControl ask_answer;
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
	type MessagePosted,
	type MessageUpdated,
	type SessionFrame,
	toJson,
} from "./compassv1";

// One outbound frame the agent emits — exactly one frozen `AgentFrame` oneof
// variant. A discriminated union so the envelope has a single place to stamp the
// oneof field, and the reader a single field to classify on.
export type OutboundFrame =
	| { readonly kind: "conversationPosted"; readonly value: MessagePosted }
	| { readonly kind: "conversationUpdated"; readonly value: MessageUpdated }
	| { readonly kind: "session"; readonly value: SessionFrame };

// The sink the agent writes outbound frames to. The wire envelope lives
// entirely behind this interface.
export interface FrameSink {
	emit(frame: OutboundFrame): void;
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
}
