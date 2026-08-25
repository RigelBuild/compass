// The control boundary: the seam between the bytes on stdin and the typed
// control ops the agent applies.
//
// The control CONTRACT is frozen (design: architecture-lineage, ratified additive
// message `AgentControl`):
//   oneof control {
//     PromptControl prompt; SteerControl steer; DeliverControl deliver;
//     ConfigControl config; TranscriptReplay replay; ReplayComplete
//     replay_complete }
// Replay barrier (frozen): TranscriptReplay is applied to context (never live
// input); the Runner holds live prompt/steer until the agent acks
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

// One decoded control op — exactly one frozen `AgentControl` oneof variant.
export type AgentControl =
	| { readonly kind: "prompt"; readonly input: string }
	| { readonly kind: "steer"; readonly message: AgentMessage }
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
