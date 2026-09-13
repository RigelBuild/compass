// The control boundary: the seam between the bytes on stdin and the typed control ops the
// agent applies. Frozen contract (design §T5, `AgentControl` oneof): prompt/steer/deliver/
// config/replay/replay_complete. Replay barrier: TranscriptReplay applies to context, and
// the Runner holds live prompt/steer until the agent acks ReplayComplete.

// The oneof VARIANTS are frozen; the payload FIELDS are not — representing an inbound
// SDK `AgentMessage`/`AgentTool` on the wire is a pending ruling. So `AgentControl` is
// NOT in ./gen and its stdin decoder is a parked follow-up. Built + tested here is the
// seam: the typed DOMAIN union the CompassAgent consumes and the `ControlSource` stream.

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
