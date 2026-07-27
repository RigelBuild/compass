// Dev-only stub for the typed agent-session observation panel (design
// compass-0.8, the typed session renderer T-U1). Supplies the typed
// SessionEvent vocabulary the real fold + renderer consume. Fixtures are keyed
// by agent account id and carry deterministic ids + monotonic epoch-ms
// timestamps (no Date.now) so the panel renders identically every run.
//
// When the live OMP session-event stream is wired, this fixture is replaced.

import type { AgentSession } from "./session-events";

// A representative typed session per agent, keyed by agent account id. Only the
// agents reachable via an agent DM in comms-stub need a session; others resolve
// to an empty trace.
export const STUB_SESSION_EVENTS: Record<string, AgentSession> = {
	"acc-livingstone": {
		agentAccountId: "acc-livingstone",
		running: true,
		events: [
			// A thinking beat before the turn.
			{
				id: "se-l0",
				atUnixMs: 1_753_000_000_000,
				kind: "thinking",
				messageId: "t-l0",
				text: "Comms-in-workspace shell is wired; I should verify the chat renders before the composer lands.",
			},
			// A WHOLE-TURN STREAMING fixture: many one-word assistant_text deltas
			// sharing ONE messageId — exercises the coalescing fold (§436-438).
			{
				id: "se-l1",
				atUnixMs: 1_753_000_001_000,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "Wiring ",
			},
			{
				id: "se-l2",
				atUnixMs: 1_753_000_001_100,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "the ",
			},
			{
				id: "se-l3",
				atUnixMs: 1_753_000_001_200,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "workspace ",
			},
			{
				id: "se-l4",
				atUnixMs: 1_753_000_001_300,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "chat ",
			},
			{
				id: "se-l5",
				atUnixMs: 1_753_000_001_400,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "shell ",
			},
			{
				id: "se-l6",
				atUnixMs: 1_753_000_001_500,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "now — ",
			},
			{
				id: "se-l7",
				atUnixMs: 1_753_000_001_600,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "running ",
			},
			{
				id: "se-l8",
				atUnixMs: 1_753_000_001_700,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "typecheck ",
			},
			{
				id: "se-l9",
				atUnixMs: 1_753_000_001_800,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "and ",
			},
			{
				id: "se-l10",
				atUnixMs: 1_753_000_001_900,
				kind: "assistant_text",
				messageId: "m-l1",
				text: "build.",
			},
			// A tool_call + a tool_call_update for the same toolCallId; the update
			// carries a real diff and output.
			{
				id: "se-l11",
				atUnixMs: 1_753_000_002_000,
				kind: "tool_call",
				toolCallId: "tc-l1",
				title: "moon run compass-ui:typecheck compass-ui:build",
				status: "in_progress",
			},
			{
				id: "se-l12",
				atUnixMs: 1_753_000_003_000,
				kind: "tool_call_update",
				toolCallId: "tc-l1",
				status: "completed",
				output: "31 modules transformed · typecheck clean · build ok",
				diffs: [
					{
						path: "apps/ui/src/App.tsx",
						oldText:
							"export function App() {\n\treturn <WorkspaceBoard />;\n}\n",
						newText:
							"export function App() {\n\treturn (\n\t\t<ShellRouter>\n\t\t\t<WorkspaceBoard />\n\t\t</ShellRouter>\n\t);\n}\n",
					},
				],
			},
			// A plan across statuses.
			{
				id: "se-l13",
				atUnixMs: 1_753_000_004_000,
				kind: "plan",
				entries: [
					{ content: "Half 1 — channel view", status: "completed" },
					{ content: "Half 2 — agent workspace", status: "in_progress" },
					{ content: "Verify + checkpoint with Matt", status: "pending" },
				],
			},
		],
	},
	"acc-drake": {
		agentAccountId: "acc-drake",
		running: false,
		events: [
			{
				id: "se-d1",
				atUnixMs: 1_753_000_000_000,
				kind: "notice",
				text: "Idle — channel-model amendment folded into #767; awaiting franklin's grid.",
			},
		],
	},
	"acc-cook": {
		agentAccountId: "acc-cook",
		running: true,
		events: [
			{
				id: "se-k1",
				atUnixMs: 1_753_000_000_000,
				kind: "tool_call",
				toolCallId: "tc-k1",
				title: "gt submit --no-interactive  (T3b network-door serve wiring)",
				status: "in_progress",
			},
			{
				id: "se-k2",
				atUnixMs: 1_753_000_001_000,
				kind: "tool_call_update",
				toolCallId: "tc-k1",
				status: "completed",
				output: "pushed cook-sea-1195-network-door → CI 5126 firing",
			},
		],
	},
};
