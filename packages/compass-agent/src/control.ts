// The control boundary: the seam between the bytes on stdin and the typed
// control ops the agent applies.
//
// The control CONTRACT is frozen (design compass-0.6 §T5, ratified additive
// message `AgentControl`):
//   oneof control {
//     PromptControl prompt; SteerControl steer; DeliverControl deliver;
//     AskAnswerControl ask_answer; ConfigControl config; TranscriptReplay replay;
//     ReplayComplete replay_complete }
// Replay barrier (frozen): TranscriptReplay is applied to context (never live
// input); the Runner holds live prompt/steer/ask_answer until the agent acks
// ReplayComplete. The set oneof field is the discriminator.
//
// `AgentControl` is an internal-only additive proto message (design §T5). Its
// oneof VARIANTS are frozen (above); its payload message FIELDS are not — the
// record leaves them open, and representing an inbound SDK `AgentMessage` /
// `AgentTool` on a compass.v1 wire is a design decision pending Matt's ruling.
// So — unlike `AgentFrame`, whose proto + gen have landed — `AgentControl` is
// NOT in ./gen and its concrete stdin decoder is a parked follow-up (stacked
// PR once the payload shape is ruled). What is built + tested here is the seam:
// `AgentControl` below is the typed DOMAIN union the CompassAgent consumes (one
// member per frozen variant), and `ControlSource` is the async stream that
// yields them. The barrier + apply logic in CompassAgent switch on this union
// and are decision-independent; only the decoder that produces `AgentControl`
// waits on the ruling.

import type { AgentMessage, AgentTool } from "@oh-my-pi/pi-agent-core";

import type { AskQuestionAnswer } from "./gen/compass/v1/comms_pb";

// One decoded control op — exactly one frozen `AgentControl` oneof variant.
export type AgentControl =
	| { readonly kind: "prompt"; readonly input: string }
	| { readonly kind: "steer"; readonly message: AgentMessage }
	// A structured answer to an in-flight `ask` (frozen 6th variant, design
	// compass-0.6:1405). Carries the ratified wire shape (`AskAnswerControl`,
	// agent_pb.ts): a repeated `AskQuestionAnswer` keys one answer per question
	// (with `customText` for a free-text "Other") — the flat single-question
	// `chosenOptionIds` the wire deliberately superseded cannot represent a
	// multi-question ask, so it is not used here. `#applyControl` delivers the
	// answer to the in-flight SDK `askDialog` promise (RIG-1509): the SDK
	// `AskTool` is exclusive-concurrency, so at most ONE ask is in flight per
	// session and the answer resolves THE pending ask — `askId` is informational
	// (logged), never a correlation key against a keyed registry. An answer that
	// arrives with no pending ask is surfaced as a counted unmapped op, never
	// silently dropped.
	| {
			readonly kind: "askAnswer";
			readonly askId: string;
			readonly answers: readonly AskQuestionAnswer[];
	  }
	| {
			readonly kind: "config";
			readonly systemPrompt?: string[];
			readonly tools?: AgentTool[];
	  }
	| { readonly kind: "replay"; readonly message: AgentMessage }
	| { readonly kind: "replayComplete" };

// The source of inbound control frames (stdin). An async iterable so the agent
// consumes it with `for await`; it ends when stdin closes. The wire decode
// (envelope + protojson) lives entirely behind this.
export type ControlSource = AsyncIterable<AgentControl>;
