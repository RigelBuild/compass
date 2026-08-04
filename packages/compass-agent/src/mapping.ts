// The mapping: the agent's `AgentSessionEvent` stream → compass.v1 `AgentFrame`s.
// This is the agent's own testable surface (design compass-0.6 §T5): there is no
// Runner-side translator, so the agent-side map is where SDK semantics become
// compass.v1 wire payloads, and it is exhaustively unit-tested against event
// fixtures.
//
// The event shapes are pinned to @oh-my-pi/pi-coding-agent (`AgentSessionEvent`,
// the session-driving superset of the core `AgentEvent`) and @oh-my-pi/pi-ai
// (`AssistantMessageEvent`, the streaming inner union). `AgentEvent.args`/`result`
// on tool events are typed `any` by the SDK; every read of them is runtime-
// narrowed (`typeof`/`in`, via the `isRecord` guard and the extractors below),
// never an inline cast.
//
// Session-surface mapping (§T5, spine-inversion + compass-0.8 typed renderer).
// The execution trace — assistant-text chunks, thinking chunks, tool calls +
// their updates (with file diffs), plans, and notices — rides
// `SessionFrame.typed_event` as a typed `SessionEvent`, which Compass renders in
// its first-party session pane. This supersedes v0.6's opaque `bytes event`
// passthrough: the mapper now TYPES the trace (design compass-0.8 §"First-party
// typed session renderer"), it does not relay opaque bytes. Board lifecycle
// transitions ride the same variant as `SessionFrame.state` (typed_event empty).
//
// The streaming conversation write-through (MessagePosted/MessageUpdated → comms)
// is REMOVED (SEA-1708): a streamed assistant `text_delta` produces only a live
// session `assistant_text` chunk per delta; `text_end` settles no comms block.
// Thinking is session-only (no comms counterpart).
//
// Dumb emitter. The emitter sets `message_id` per
// streamed assistant message and `event_id` per event, and does NOT buffer or
// coalesce the session stream — one `SessionEvent` per delta. Coalescing by
// `message_id` is `foldSession`'s job on the render side. `message_id` is a
// monotonic per-message counter (the SDK carries no stable message id); `event_id`
// a monotonic per-event counter. `atUnixMs` comes from an injectable clock so
// tests are deterministic.

import type { AssistantMessageEvent } from "@oh-my-pi/pi-ai";
import type { AgentSessionEvent } from "@oh-my-pi/pi-coding-agent";
import {
	type AgentPlanEntry,
	AgentPlanEntrySchema,
	AgentPlanEntryStatus,
	AgentSessionState,
	AgentToolCallStatus,
	create,
	SessionAssistantTextSchema,
	type SessionEvent,
	SessionEventSchema,
	type SessionFileDiff,
	SessionFileDiffSchema,
	type SessionFrame,
	SessionFrameSchema,
	SessionPlanSchema,
	SessionThinkingSchema,
	SessionToolCallSchema,
	SessionToolCallUpdateSchema,
} from "./compassv1";
import type { OutboundFrame } from "./frame";

// A frame the mapper could not produce a compass.v1 payload for — surfaced, never
// silently dropped (the frozen "unknown frame types logged + counted" rule
// applies symmetrically on the produce side). The agent logs + counts these; a
// growing count means a session-event type the map does not yet cover.
export interface UnmappedEvent {
	readonly kind: "unmapped";
	readonly eventType: string;
	readonly reason: string;
}

export type MapOutput = OutboundFrame | UnmappedEvent;

// Injectable wall-clock for `SessionEvent.at_unix_ms` (epoch ms). Defaults to
// `Date.now`; tests inject a deterministic clock so emitted timestamps are
// asserted exactly.
export type Clock = () => number;

// Cap on emitted tool-call `output` text, mirroring the ACP mapper's ACP_TEXT_LIMIT
// (acp-event-mapper.ts:130) so a pathological tool result does not put an unbounded
// string on the wire. The session renderer shows a disclosure, not the full blob.
const OUTPUT_TEXT_LIMIT = 4_000;

// Maps the agent's session-event stream to compass.v1 frames. Stateful on the
// session ids: a monotonic message counter (bumped per `message_start`) labels
// the assistant-text / thinking chunks of one logical message so `foldSession`
// coalesces them; a monotonic event counter labels every SessionEvent.
export class EventMapper {
	// Monotonic per-message counter → the `message_id` on assistant-text/thinking
	// session chunks. Bumped at `message_start`; the pre-first-message value labels
	// any stray chunk that arrives before a `message_start` (defensive, never
	// expected). One logical message = one id, so `foldSession` coalesces its
	// streamed chunks into a single rendered block.
	#messageSeq = 0;
	// Monotonic per-event counter → the `event_id` on every SessionEvent. Assigned
	// as each event is emitted so ids are stable and ordered across the stream.
	#eventSeq = 0;
	// Injectable wall-clock for `at_unix_ms`.
	readonly #now: Clock;

	constructor(now: Clock = Date.now) {
		this.#now = now;
	}

	// Map one session event to zero or more compass.v1 frames. Zero frames is
	// normal (delta accumulation, turn boundaries); an event the map does not cover
	// yields a single UnmappedEvent so the caller can log + count it.
	map(event: AgentSessionEvent): MapOutput[] {
		switch (event.type) {
			case "agent_start":
			case "turn_start":
				return [this.#sessionState(AgentSessionState.WORKING)];
			case "message_start":
				// A new assistant message begins — bump the message id so its chunks
				// coalesce under a fresh id, then signal WORKING.
				this.#messageSeq++;
				return [this.#sessionState(AgentSessionState.WORKING)];
			case "agent_end":
				return [this.#sessionState(AgentSessionState.READY)];
			case "message_update":
				return this.#onMessageUpdate(event.assistantMessageEvent);
			case "tool_execution_start":
				return [
					this.#sessionEvent({
						case: "toolCall",
						value: create(SessionToolCallSchema, {
							toolCallId: event.toolCallId,
							title: toolTitle(event.toolName, event.args, event.intent),
							status: AgentToolCallStatus.IN_PROGRESS,
						}),
					}),
				];
			case "tool_execution_update":
				return [
					this.#sessionEvent({
						case: "toolCallUpdate",
						value: create(SessionToolCallUpdateSchema, {
							toolCallId: event.toolCallId,
							status: AgentToolCallStatus.IN_PROGRESS,
							output: extractReadableText(event.partialResult) ?? "",
							diffs: extractDiffs(event.partialResult),
						}),
					}),
				];
			case "tool_execution_end": {
				const out: MapOutput[] = [
					this.#sessionEvent({
						case: "toolCallUpdate",
						value: create(SessionToolCallUpdateSchema, {
							toolCallId: event.toolCallId,
							status: event.isError
								? AgentToolCallStatus.FAILED
								: AgentToolCallStatus.COMPLETED,
							output: extractReadableText(event.result) ?? "",
							diffs: extractDiffs(event.result),
						}),
					}),
				];
				// The `todo` tool's result is the authoritative plan snapshot — emit a
				// SessionPlan alongside the tool-call settle (mirrors the ACP mapper's
				// mapTodoResultToPlanUpdate, acp-event-mapper.ts:378). A failed todo or
				// an unparseable result yields no plan (the extractor returns undefined).
				if (event.toolName === "todo" && !event.isError) {
					const entries = extractPlanEntries(event.result);
					if (entries !== undefined) {
						out.push(
							this.#sessionEvent({
								case: "plan",
								value: create(SessionPlanSchema, { entries }),
							}),
						);
					}
				}
				return out;
			}
			case "todo_reminder":
				// A periodic plan nudge carrying the current todos — emit the plan
				// snapshot (mirrors the ACP mapper's todo_reminder arm,
				// acp-event-mapper.ts:247). The todos are already typed on the event.
				return [
					this.#sessionEvent({
						case: "plan",
						value: create(SessionPlanSchema, {
							entries: event.todos.map((t) =>
								create(AgentPlanEntrySchema, {
									content: t.content,
									status: planStatus(t.status),
								}),
							),
						}),
					}),
				];
			case "todo_auto_clear":
				// The todo list was cleared → an empty plan (mirrors
				// acp-event-mapper.ts:255).
				return [
					this.#sessionEvent({
						case: "plan",
						value: create(SessionPlanSchema, { entries: [] }),
					}),
				];
			case "turn_end":
			case "message_end":
				// Boundary events: nothing to emit on their own; any streamed text
				// block already settled on its `text_end`.
				return [];
			default:
				// A session event the map does not cover. This includes the
				// orchestration-only variants of the `AgentSessionEvent` superset
				// (auto_compaction_*, auto_retry_*, retry_fallback_*, ttsr_triggered,
				// irc_message, thinking_level_changed, goal_updated) and the `notice`
				// variant (staged — see below), plus any future variant (the dep is
				// version-ranged). Surface a single UnmappedEvent so it is logged +
				// counted, never silently dropped and never a crash (mapping.ts frozen
				// invariant; symmetric with the inner #onMessageUpdate default arm).
				return [
					{
						kind: "unmapped",
						eventType: (event as { type: string }).type,
						reason: "unmapped session event type",
					},
				];
		}
	}

	// A lifecycle transition: a session frame carrying only the board state, no
	// trace event (SessionFrame.typed_event stays empty — "no trace, state only").
	// The Runner extracts the state into an AgentSessionStatus, stamping the
	// session_id it owns (the agent mints no server ids; comms.proto:230).
	#sessionState(state: AgentSessionState): OutboundFrame {
		const value: SessionFrame = create(SessionFrameSchema, { state });
		return { kind: "session", value };
	}

	// A typed trace frame: one `SessionEvent` on `SessionFrame.typed_event`, with
	// no board-state transition (state stays UNSPECIFIED). Stamps the monotonic
	// `event_id` and the clock `at_unix_ms`. This is the emitter's single point of
	// SessionEvent construction — every trace arm routes through here so id/clock
	// stamping is uniform.
	#sessionEvent(event: SessionEvent["event"]): OutboundFrame {
		const typedEvent: SessionEvent = create(SessionEventSchema, {
			eventId: String(++this.#eventSeq),
			atUnixMs: BigInt(this.#now()),
			event,
		});
		const value: SessionFrame = create(SessionFrameSchema, { typedEvent });
		return { kind: "session", value };
	}

	#onMessageUpdate(inner: AssistantMessageEvent): MapOutput[] {
		// `inner` is a pi-ai AssistantMessageEvent — a discriminated union on
		// `type`. Narrow on the discriminant; read `delta`/`content` only in the
		// arms the union guarantees them. Text streams as a live session chunk per
		// delta; thinking is session-only.
		switch (inner.type) {
			case "text_delta": {
				// Session surface: one assistant_text chunk per delta (dumb emitter).
				// Skip an empty delta — nothing to render, and it would only add an
				// empty row for foldSession to coalesce away.
				if (inner.delta === "") return [];
				return [this.#assistantText(inner.delta)];
			}
			case "text_end": {
				// Comms surface removed (SEA-1708): the streaming conversation
				// write-through is gone, so a settled block emits no frame. The live
				// session `assistant_text` chunks (per delta) are the only text
				// surface.
				return [];
			}
			case "thinking_delta": {
				// Session surface only: one thinking chunk per delta, correlated by
				// the current message id (no comms counterpart — thinking is trace).
				if (inner.delta === "") return [];
				return [this.#thinking(inner.delta)];
			}
			case "error": {
				// The stream surfaced an inner error. `reason` splits the failure
				// class (SDK: "aborted" | "error"):
				//   - "error" = an unexpected inner/provider failure → an ERRORED
				//     lifecycle transition (compass.proto:132 scopes ERRORED to the
				//     OOM/panic/engine-restart class; an inner stream error is that
				//     class).
				//   - "aborted" = a deliberate steer/user cancel, NOT a crash —
				//     conflating it with ERRORED would misreport a normal abort as an
				//     engine failure. Surface it as a counted UnmappedEvent so it is
				//     logged + counted, never dropped, pending the abort-surfacing
				//     contract (a follow-up).
				if (inner.reason === "error") {
					return [this.#sessionState(AgentSessionState.ERRORED)];
				}
				return [
					{
						kind: "unmapped",
						eventType: "message_update:error",
						reason:
							"agent abort (reason=aborted) — not a crash; abort-surfacing staged",
					},
				];
			}
			default:
				// start/text_start/thinking_start/thinking_end/image_end/toolcall_*/
				// done and any future inner variant: no standalone frame (tool calls
				// surface via the tool_execution_* events; start/end are boundaries).
				return [];
		}
	}

	// A session assistant-text chunk, correlated by the current message id so
	// foldSession coalesces the streamed deltas into one rendered block.
	#assistantText(text: string): OutboundFrame {
		return this.#sessionEvent({
			case: "assistantText",
			value: create(SessionAssistantTextSchema, {
				text,
				messageId: this.#messageId(),
			}),
		});
	}

	// A session thinking chunk, correlated by the current message id (same append
	// semantics as assistant text).
	#thinking(text: string): OutboundFrame {
		return this.#sessionEvent({
			case: "thinking",
			value: create(SessionThinkingSchema, {
				text,
				messageId: this.#messageId(),
			}),
		});
	}

	// The current logical-message id. `#messageSeq` is 0 until the first
	// `message_start`; a chunk arriving before any message_start (defensive — never
	// expected) is labelled "0".
	#messageId(): string {
		return String(this.#messageSeq);
	}
}

// ── Runtime-narrowed readers (never an inline cast) ──────────────────────────
// The SDK types tool `args`/`result`/`partialResult` as `any`; these read them
// through the `isRecord` guard so every property access is on a known-object
// value. The logic mirrors the ACP mapper's readers (acp-event-mapper.ts) so the
// two produce equivalent output from the same SDK shapes — but compass-native
// (its own target types), not an ACP dependency.

// The one narrowing primitive: is `value` a non-null object we can index by key?
// Every reader below narrows through this before any property read, so there is
// no inline `as`-cast anywhere in the extraction (mapping.ts convention).
function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null;
}

function readString(value: unknown, key: string): string | undefined {
	if (!isRecord(value)) return undefined;
	const prop = value[key];
	return typeof prop === "string" ? prop : undefined;
}

// A best-effort human-readable rendering of a tool result for the `output` field,
// mirroring acp-event-mapper.ts:913 extractReadableText: a bare string, an
// Error's message, a `text`/`errorMessage`/`message` property, else the JSON of
// the value. Capped at OUTPUT_TEXT_LIMIT. Returns undefined when nothing readable
// is present (the caller defaults to "").
function extractReadableText(value: unknown): string | undefined {
	if (typeof value === "string") return normalizeText(value);
	if (value instanceof Error) return normalizeText(value.message);
	if (!isRecord(value)) return undefined;

	const direct =
		readString(value, "text") ??
		readString(value, "errorMessage") ??
		readString(value, "message");
	if (direct !== undefined) return normalizeText(direct);

	const serialized = safeJsonStringify(value);
	return normalizeText(serialized);
}

function normalizeText(text: string | undefined): string | undefined {
	if (text === undefined) return undefined;
	const trimmed = text.trim();
	if (trimmed.length === 0) return undefined;
	return trimmed.length > OUTPUT_TEXT_LIMIT
		? `${trimmed.slice(0, OUTPUT_TEXT_LIMIT - 1)}…`
		: trimmed;
}

function safeJsonStringify(value: unknown): string | undefined {
	try {
		return JSON.stringify(value);
	} catch {
		return undefined;
	}
}

// Extract file diffs from a tool result, mirroring acp-event-mapper.ts:646
// extractDiffToolCallContent: a `details.perFileResults[]` array (multi-file) or
// the single `details` object, each carrying `path` + `oldText`/`newText`. A
// creation has no `oldText` (SessionFileDiff.old_text is optional). Entries
// flagged `isError` or lacking a path / any text are skipped.
function extractDiffs(result: unknown): SessionFileDiff[] {
	if (!isRecord(result)) return [];
	const details = result.details;
	if (!isRecord(details)) return [];
	const perFile = details.perFileResults;
	const entries: unknown[] = Array.isArray(perFile) ? perFile : [details];
	const diffs: SessionFileDiff[] = [];
	for (const entry of entries) {
		const diff = buildDiff(entry);
		if (diff !== undefined) diffs.push(diff);
	}
	return diffs;
}

function buildDiff(entry: unknown): SessionFileDiff | undefined {
	if (!isRecord(entry)) return undefined;
	if (entry.isError === true) return undefined;
	const path = readString(entry, "path");
	if (path === undefined || path.length === 0) return undefined;
	const oldText = readString(entry, "oldText");
	const newText = readString(entry, "newText");
	if (oldText === undefined && newText === undefined) return undefined;
	return create(SessionFileDiffSchema, {
		path,
		oldText,
		newText: newText ?? "",
	});
}

// Extract plan entries from the `todo` tool result, mirroring
// acp-event-mapper.ts:398 extractTodoPhases + :409 extractTodoEntries: a
// `details.phases[].tasks[]` shape, each task a `{ content, status }`. Returns
// undefined when the result is not a todo snapshot (caller emits no plan), an
// empty array when the snapshot is present but has no valid tasks.
function extractPlanEntries(result: unknown): AgentPlanEntry[] | undefined {
	if (!isRecord(result)) return undefined;
	const details = result.details;
	if (!isRecord(details)) return undefined;
	const phases = details.phases;
	if (!Array.isArray(phases)) return undefined;
	const entries: AgentPlanEntry[] = [];
	for (const phase of phases) {
		if (!isRecord(phase)) continue;
		const tasks = phase.tasks;
		if (!Array.isArray(tasks)) continue;
		for (const task of tasks) {
			const content = readString(task, "content");
			if (content === undefined || content.length === 0) continue;
			const status = isRecord(task) ? task.status : undefined;
			entries.push(
				create(AgentPlanEntrySchema, {
					content,
					status: planStatus(status),
				}),
			);
		}
	}
	return entries;
}

// Map an SDK todo status (string literal) to the compass plan-entry status enum,
// mirroring acp-event-mapper.ts:367 todoStatusMap: "abandoned" folds to COMPLETED
// (the plan enum has no abandoned state), an unknown/absent status → PENDING.
function planStatus(status: unknown): AgentPlanEntryStatus {
	switch (status) {
		case "in_progress":
			return AgentPlanEntryStatus.IN_PROGRESS;
		case "completed":
		case "abandoned":
			return AgentPlanEntryStatus.COMPLETED;
		default:
			return AgentPlanEntryStatus.PENDING;
	}
}

// A display title for a tool call: the caller-supplied `intent` when present
// (the agent's own human-readable label), else the tool name. The ACP mapper
// builds elaborate command/eval/path titles (acp-event-mapper.ts:553); the
// compass session renderer shows a plain title, so intent-or-name is the
// faithful-but-simpler rendering (SessionToolCall.title is a display string).
function toolTitle(
	toolName: string,
	args: unknown,
	intent: string | undefined,
): string {
	const trimmed = intent?.trim();
	if (trimmed !== undefined && trimmed.length > 0) return trimmed;
	// `args` is accepted for parity with the ACP title builder's signature and to
	// leave a grounded seam for richer titles; the dumb emitter does not read it.
	void args;
	return toolName;
}
