// The mapping: the agent's `AgentSessionEvent` stream → compass.v1 `AgentFrame`s. The
// agent's own testable surface (design: architecture-lineage): no Runner-side translator, so
// this is where SDK semantics become compass.v1 wire payloads. Event shapes are pinned to the
// SDK packages; the `any`-typed tool args/result are runtime-narrowed, never cast.

// Session-surface mapping (§T5, spine-inversion + typed session renderer). The execution
// trace — assistant-text/thinking chunks, tool calls + updates (with diffs), plans, notices —
// rides `SessionFrame.typed_event` as a typed `SessionEvent` (superseding v0.6's opaque bytes
// passthrough). Board lifecycle rides the same variant as `SessionFrame.state`.

// The streaming conversation write-through is REMOVED (RIG-1708): a streamed `text_delta`
// produces only a live session `assistant_text` chunk; thinking is session-only. Dumb emitter:
// it sets `message_id` per streamed message and `event_id` per event and does NOT coalesce
// (that is `foldSession`'s job); `atUnixMs` comes from an injectable clock for determinism.

import type { AssistantMessage, AssistantMessageEvent } from "@oh-my-pi/pi-ai";
import type { AgentSessionEvent } from "@oh-my-pi/pi-coding-agent";
import {
	type AgentPlanEntry,
	AgentPlanEntrySchema,
	AgentPlanEntryStatus,
	AgentSessionState,
	AgentToolCallStatus,
	create,
	SessionAssistantTextSchema,
	SessionErrorKind,
	SessionErrorSchema,
	type SessionEvent,
	SessionEventSchema,
	type SessionFileDiff,
	SessionFileDiffSchema,
	type SessionFrame,
	SessionFrameSchema,
	type SessionInjectionKind,
	SessionInjectionSchema,
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

// Cap emitted tool-call output at 4,000 characters so pathological results do
// not put an unbounded string on the wire. The renderer shows a disclosure.
const OUTPUT_TEXT_LIMIT = 4_000;

// Maps the agent's session-event stream to compass.v1 frames. Stateful on the
// session ids: a monotonic message counter (bumped per `message_start`) labels
// the assistant-text / thinking chunks of one logical message so `foldSession`
// coalesces them; a monotonic event counter labels every SessionEvent.
export class EventMapper {
	// Monotonic per-message counter → the `message_id` on assistant-text/thinking chunks.
	// Bumped at `message_start`; the pre-first value labels any stray pre-`message_start`
	// chunk (defensive). One logical message = one id, so `foldSession` coalesces its chunks.
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
				// A successful `todo` result is the authoritative plan snapshot. Emit a
				// SessionPlan alongside the settled tool call; invalid results emit none.
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
				// A periodic SDK reminder carries current todos; emit the plan snapshot.
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
				// The SDK cleared the todo list; emit an empty plan.
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
				// A session event the map does not cover: the orchestration-only variants of the
				// `AgentSessionEvent` superset (auto_compaction_*, auto_retry_*, etc.), the
				// `notice` variant, and any future variant. Surface a single UnmappedEvent so it
				// is logged + counted, never dropped and never a crash (frozen invariant).
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

	// A typed trace frame: one `SessionEvent` on `SessionFrame.typed_event`, no board-state
	// transition. Stamps the monotonic `event_id` and clock `at_unix_ms`. The emitter's single
	// point of SessionEvent construction — every trace arm routes here so stamping is uniform.
	#sessionEvent(event: SessionEvent["event"]): OutboundFrame {
		const typedEvent: SessionEvent = create(SessionEventSchema, {
			eventId: String(++this.#eventSeq),
			atUnixMs: BigInt(this.#now()),
			event,
		});
		const value: SessionFrame = create(SessionFrameSchema, { typedEvent });
		return { kind: "session", value };
	}

	// Build one SessionError trace frame from a pi-ai inner-error AssistantMessage. `kind`
	// discriminates an unexpected failure (ERROR, paired with ERRORED) from a deliberate abort
	// (ABORTED, no transition). `status` is set only when the provider surfaced an HTTP status,
	// so a subscriber can tell "no status" from a literal 0. Routes through `#sessionEvent`.
	#sessionError(
		kind: SessionErrorKind,
		error: AssistantMessage,
	): OutboundFrame {
		return this.#sessionEvent({
			case: "sessionError",
			value: create(SessionErrorSchema, {
				kind,
				message: error.errorMessage ?? "",
				...(error.errorStatus !== undefined
					? { status: error.errorStatus }
					: {}),
			}),
		});
	}

	// Build one SessionInjection trace frame — the agent-side observation that a channel
	// message was injected as a steer or deliver (design T1). Public because the injection
	// point is `CompassAgent.steer()`/`deliver()`, not the `map()` stream; routing through
	// `#sessionEvent` orders the observation on the one monotonic sequence.
	sessionInjection(
		opKind: SessionInjectionKind,
		messageId: string,
		fromHandle: string,
		traceparent: string,
	): OutboundFrame {
		return this.#sessionEvent({
			case: "sessionInjection",
			value: create(SessionInjectionSchema, {
				opKind,
				messageId,
				fromHandle,
				traceparent,
			}),
		});
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
				// Comms surface removed (RIG-1708): the streaming conversation
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
				// The stream surfaced an inner error. `reason` splits the class (SDK:
				// "aborted"|"error"), both surfacing content as a SessionError frame (DL-322).
				// "error" = an unexpected failure → SessionError(ERROR) AND the ERRORED
				// transition (content first); "aborted" = a deliberate cancel → ABORTED, no transition.
				if (inner.reason === "error") {
					return [
						this.#sessionError(SessionErrorKind.ERROR, inner.error),
						this.#sessionState(AgentSessionState.ERRORED),
					];
				}
				return [this.#sessionError(SessionErrorKind.ABORTED, inner.error)];
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
// SDK tool payloads are narrowed through `isRecord` before property access.
// The readers emit Compass-native target types.

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

// Render a tool result for `output`: strings, Error messages, known text fields,
// or JSON. Cap at OUTPUT_TEXT_LIMIT; undefined lets the caller default to "".
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

// Extract file diffs from a tool result's `details.perFileResults[]` or `details`.
// Skip errors and entries lacking a path or any text.
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

// Extract plan entries from `details.phases[].tasks[]`. Return undefined when the
// result is not a todo snapshot, or an empty array when it has no valid tasks.
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

// Map an SDK todo status to the Compass plan-entry enum. Unknown or absent status
// becomes PENDING; the enum has no separate abandoned state.
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

// Display the caller's `intent` when present, otherwise the tool name. The
// session renderer uses this plain title rather than elaborating tool arguments.
function toolTitle(
	toolName: string,
	args: unknown,
	intent: string | undefined,
): string {
	const trimmed = intent?.trim();
	if (trimmed !== undefined && trimmed.length > 0) return trimmed;
	// `args` remains part of the mapper signature for future richer titles.
	void args;
	return toolName;
}
