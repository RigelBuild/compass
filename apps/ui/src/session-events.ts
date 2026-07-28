// Typed session model + the pure fold that turns an ordered SessionEvent stream
// into render-ready TraceItems (design compass-0.8, §397-438).
//
// A session is an ordered stream of typed SessionEvents (assistant/thinking text
// deltas, tool calls + updates, plans, notices). `foldSession` reduces that raw
// stream into the TraceItem[] a renderer walks: streamed text deltas coalesce,
// tool updates fold into their originating call, and a later plan supersedes an
// earlier one. The fold is pure — it never mutates the input events.

/** Lifecycle of a tool call, latest-wins as updates arrive. */
export type ToolCallStatus = "pending" | "in_progress" | "completed" | "failed";

/** Lifecycle of a single plan entry. */
export type PlanEntryStatus = "pending" | "in_progress" | "completed";

/** One line item in an agent's plan. */
export interface PlanEntry {
	content: string;
	status: PlanEntryStatus;
}

/** A file change carried by a tool update. `oldText` is null for a new file. */
export interface FileDiff {
	path: string;
	oldText: string | null;
	newText: string;
}

/** One typed event in an agent's session stream. Every event carries a stable
 *  `id` and an `atUnixMs` timestamp; the `kind` discriminates the payload. */
export type SessionEvent = { id: string; atUnixMs: number } & (
	| { kind: "assistant_text"; messageId: string; text: string }
	| { kind: "thinking"; messageId: string; text: string }
	| {
			kind: "tool_call";
			toolCallId: string;
			title: string;
			status: ToolCallStatus;
	  }
	| {
			kind: "tool_call_update";
			toolCallId: string;
			status: ToolCallStatus;
			output?: string;
			diffs?: FileDiff[];
	  }
	| { kind: "plan"; entries: PlanEntry[] }
	| { kind: "notice"; text: string; link?: string }
);

/** An agent's live session: whether it is running plus its ordered event stream. */
export interface AgentSession {
	/** The server-minted session id — the cursor StartAgentSession returned, and
	 *  the ONLY field StopAgentSession takes (compass_pb.ts:831-836). Carried
	 *  here because Stop is issued for the observed session, not the account. */
	sessionId: string;
	agentAccountId: string;
	running: boolean;
	events: SessionEvent[];
	/** Set only on hand-written fixture sessions (session-events-stub.ts). A
	 *  fixture's `sessionId` was never minted by a server, so no live RPC may
	 *  carry it: `StopAgentSession` treats an unknown session as an idempotent
	 *  success (go/internal/runner/host.go:217-228), so issuing one would report
	 *  success while stopping nothing. The store refuses instead, and the Stop
	 *  control renders disabled. Absent → the session came from the server. */
	readonly fixture?: true;
}

/** A render-ready item produced by folding the event stream. */
export type TraceItem =
	| { kind: "text" | "thinking"; messageId: string; text: string }
	| { kind: "notice"; event: SessionEvent }
	| {
			kind: "tool";
			toolCallId: string;
			call?: Extract<SessionEvent, { kind: "tool_call" }>;
			status: ToolCallStatus;
			output?: string;
			diffs?: FileDiff[];
	  }
	| { kind: "plan"; entries: PlanEntry[] };

/**
 * Fold an ordered SessionEvent stream into render-ready TraceItems (§431-438).
 *
 * - `assistant_text` / `thinking`: consecutive deltas sharing the SAME kind AND
 *   messageId coalesce into one item, text concatenated with no separator. Any
 *   interleaved item of another kind breaks adjacency → separate items.
 * - `tool_call`: emits a `tool` item, tracked by toolCallId so updates fold in.
 * - `tool_call_update`: folds into its tool item in place (latest status wins,
 *   plus output/diffs when carried); an orphan update becomes its own tool item
 *   with `call` undefined.
 * - `plan`: latest plan wins — a prior plan item is removed before the fresh one
 *   is pushed at the current position.
 * - `notice`: passes through in place, carrying its event.
 *
 * Pure: input events are never mutated; all TraceItems are freshly built.
 */
export function foldSession(events: readonly SessionEvent[]): TraceItem[] {
	const items: TraceItem[] = [];
	const toolsById = new Map<string, Extract<TraceItem, { kind: "tool" }>>();

	for (const event of events) {
		switch (event.kind) {
			case "assistant_text":
			case "thinking": {
				const traceKind = event.kind === "assistant_text" ? "text" : "thinking";
				const last = items[items.length - 1];
				if (
					last &&
					(last.kind === "text" || last.kind === "thinking") &&
					last.kind === traceKind &&
					last.messageId === event.messageId
				) {
					// Coalesce: rebuild the last item with the concatenated text.
					items[items.length - 1] = {
						kind: last.kind,
						messageId: last.messageId,
						text: last.text + event.text,
					};
				} else {
					items.push({
						kind: traceKind,
						messageId: event.messageId,
						text: event.text,
					});
				}
				break;
			}
			case "tool_call": {
				const item: Extract<TraceItem, { kind: "tool" }> = {
					kind: "tool",
					toolCallId: event.toolCallId,
					call: event,
					status: event.status,
				};
				items.push(item);
				toolsById.set(event.toolCallId, item);
				break;
			}
			case "tool_call_update": {
				const existing = toolsById.get(event.toolCallId);
				if (existing) {
					existing.status = event.status;
					if (event.output !== undefined) existing.output = event.output;
					if (event.diffs !== undefined) existing.diffs = event.diffs;
				} else {
					const item: Extract<TraceItem, { kind: "tool" }> = {
						kind: "tool",
						toolCallId: event.toolCallId,
						call: undefined,
						status: event.status,
						...(event.output !== undefined ? { output: event.output } : {}),
						...(event.diffs !== undefined ? { diffs: event.diffs } : {}),
					};
					items.push(item);
					toolsById.set(event.toolCallId, item);
				}
				break;
			}
			case "plan": {
				const priorIndex = items.findIndex((i) => i.kind === "plan");
				if (priorIndex !== -1) items.splice(priorIndex, 1);
				items.push({ kind: "plan", entries: event.entries });
				break;
			}
			case "notice": {
				items.push({ kind: "notice", event });
				break;
			}
		}
	}

	return items;
}
