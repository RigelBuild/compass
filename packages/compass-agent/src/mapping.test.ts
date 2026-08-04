// EventMapper: the SDK `AgentSessionEvent` stream → compass.v1 frame map (§T5's
// "own testable surface", spine-inversion + compass-0.8 typed session renderer).
// Each test defends an observable translation contract — what compass.v1 payload
// a given SDK event produces — never the mapper's internals. One payload class:
//   - SESSION (typed trace): assistant-text / thinking chunks, tool calls + their
//     updates (with diffs), and plans ride `SessionFrame.typed_event` as a typed
//     `SessionEvent`; board lifecycle rides `SessionFrame.state`.
// The clock is injected so `at_unix_ms` is asserted exactly. Fixtures are plain
// SDK-typed literals (no network, no timers).

import { describe, expect, test } from "bun:test";
import type { AssistantMessage, AssistantMessageEvent } from "@oh-my-pi/pi-ai";
import type { AgentSessionEvent } from "@oh-my-pi/pi-coding-agent";
import {
	AgentPlanEntryStatus,
	AgentSessionState,
	AgentToolCallStatus,
	type SessionEvent,
} from "./compassv1";
import { EventMapper, type MapOutput } from "./mapping";

// The injected wall-clock value: every trace SessionEvent must stamp exactly
// this on `at_unix_ms` (as a bigint). A picked-out constant, distinct from any
// plausible Date.now, so a mapper that ignored the injected clock reddens.
const FIXED_NOW = 1_700_000_000_000;

// A mapper with the deterministic clock injected — the default constructor for
// every test so timestamps are asserted exactly.
function mapper(): EventMapper {
	return new EventMapper(() => FIXED_NOW);
}

// A minimal, valid AssistantMessage for the `partial`/`message` fields the SDK
// event shapes require. The mapper never reads it — it exists only to satisfy
// the pinned SDK types so the fixtures are honest event literals.
function partial(): AssistantMessage {
	return {
		role: "assistant",
		content: [],
		api: "anthropic-messages",
		provider: "anthropic",
		model: "test-model",
		usage: {
			input: 0,
			output: 0,
			cacheRead: 0,
			cacheWrite: 0,
			totalTokens: 0,
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
		},
		stopReason: "stop",
		timestamp: 0,
	};
}

// Wrap an inner pi-ai AssistantMessageEvent into the SDK `message_update` event.
function upd(inner: AssistantMessageEvent): AgentSessionEvent {
	return {
		type: "message_update",
		message: partial(),
		assistantMessageEvent: inner,
	};
}

// A `text_delta` message_update carrying the given delta.
function textDelta(delta: string): AgentSessionEvent {
	return upd({
		type: "text_delta",
		contentIndex: 0,
		delta,
		partial: partial(),
	});
}

// A `text_end` message_update carrying settled content.
function textEnd(content: string): AgentSessionEvent {
	return upd({
		type: "text_end",
		contentIndex: 0,
		content,
		partial: partial(),
	});
}

// A `message_update` whose inner `text_end` OMITS `content` — exercises the
// mapper's `inner.content ?? buf` fallback (a settle event that carries no
// settled string must not lose the streamed deltas). The SDK type marks
// `content` required, so the cast is deliberate: we are pinning the defensive
// branch.
function textEndNoContent(): AgentSessionEvent {
	return upd({
		type: "text_end",
		contentIndex: 0,
		partial: partial(),
	} as unknown as AssistantMessageEvent);
}

// A `thinking_delta` message_update carrying the given delta.
function thinkingDelta(delta: string): AgentSessionEvent {
	return upd({
		type: "thinking_delta",
		contentIndex: 0,
		delta,
		partial: partial(),
	});
}

// A `message_start` event (bumps the message id + clears the text state).
function messageStart(): AgentSessionEvent {
	return { type: "message_start", message: partial() };
}

// Assert an optional protobuf field is present and return it narrowed — the
// `toBeDefined()` teeth without a `!` on the follow-on access (clears biome
// noNonNullAssertion; mirrors store.test.ts "keep the teeth without a `!`").
function defined<T>(value: T | undefined, what: string): T {
	if (value === undefined) throw new Error(`expected ${what} to be present`);
	return value;
}

// Narrow a single-frame result to a `session` frame carrying a lifecycle STATE
// (no trace event — the board transition rides `state`, `typed_event` stays
// unset). A trace event on a pure lifecycle frame would redden here.
function soleSessionState(out: MapOutput[]): AgentSessionState {
	expect(out).toHaveLength(1);
	const frame = out[0];
	if (frame.kind !== "session")
		throw new Error(`expected session frame, got ${frame.kind}`);
	expect(frame.value.typedEvent).toBeUndefined();
	return frame.value.state;
}

// Narrow a single-frame result to its typed `SessionEvent` (the trace payload on
// `SessionFrame.typed_event`). A trace frame carries no lifecycle transition
// (state UNSPECIFIED); assert that, then return the typed event for the caller
// to narrow on `event.case`/`event.value`.
function soleTyped(out: MapOutput[]): SessionEvent {
	expect(out).toHaveLength(1);
	const frame = out[0];
	if (frame.kind !== "session")
		throw new Error(`expected session frame, got ${frame.kind}`);
	expect(frame.value.state).toBe(AgentSessionState.UNSPECIFIED);
	return defined(frame.value.typedEvent, "typed session event");
}

// Every typed SessionEvent produced by a run, in emission order across frames.
function typedEvents(out: MapOutput[]): SessionEvent[] {
	return out.flatMap((f) =>
		f.kind === "session" && f.value.typedEvent !== undefined
			? [f.value.typedEvent]
			: [],
	);
}

describe("EventMapper — session lifecycle state derivation", () => {
	// Each lifecycle event derives exactly one `session` frame in a specific
	// board state (SessionFrame.state; typed_event unset). A flipped mapping
	// (e.g. agent_end → WORKING) reddens the matching row.
	const rows: {
		name: string;
		event: AgentSessionEvent;
		state: AgentSessionState;
	}[] = [
		{
			name: "agent_start → WORKING",
			event: { type: "agent_start" },
			state: AgentSessionState.WORKING,
		},
		{
			name: "turn_start → WORKING",
			event: { type: "turn_start" },
			state: AgentSessionState.WORKING,
		},
		{
			name: "message_start → WORKING",
			event: messageStart(),
			state: AgentSessionState.WORKING,
		},
		{
			name: "agent_end → READY",
			event: { type: "agent_end", messages: [] },
			state: AgentSessionState.READY,
		},
	];
	for (const row of rows) {
		test(row.name, () => {
			expect(soleSessionState(mapper().map(row.event))).toBe(row.state);
		});
	}

	// Boundary events settle nothing on their own — the streamed block already
	// settled on its own `*_end`. A spurious frame here would double a boundary.
	const boundary: { name: string; event: AgentSessionEvent }[] = [
		{
			name: "turn_end → no frame",
			event: { type: "turn_end", message: partial(), toolResults: [] },
		},
		{
			name: "message_end → no frame",
			event: { type: "message_end", message: partial() },
		},
	];
	for (const row of boundary) {
		test(row.name, () => {
			expect(mapper().map(row.event)).toEqual([]);
		});
	}
});

describe("EventMapper — injected clock stamps at_unix_ms (as bigint)", () => {
	// The mapper takes an injectable clock; every trace SessionEvent stamps its
	// value on `at_unix_ms`. A mapper that ignored the injected clock (reading
	// Date.now) would emit a different value and redden. The field is proto
	// int64 → a JS bigint in the message, so the comparison is against a bigint.
	test("a trace event's at_unix_ms equals the injected clock, typed bigint", () => {
		const m = new EventMapper(() => 42);
		const ev = soleTyped(m.map(textDelta("hi")));
		expect(ev.atUnixMs).toBe(42n);
		expect(typeof ev.atUnixMs).toBe("bigint");
	});
});

describe("EventMapper — monotonic event_id across the stream", () => {
	// event_id is a per-EVENT monotonic counter ("1","2",…) assigned in emission
	// order across the whole stream — only trace SessionEvents consume it
	// (lifecycle frames do not). A reset or shared counter reddens the sequence.
	test("mixed trace stream carries strictly increasing event_ids in emission order", () => {
		const m = mapper();
		const ids = [
			...typedEvents(m.map(messageStart())), // lifecycle only — no event_id
			...typedEvents(m.map(textDelta("a"))),
			...typedEvents(m.map(textDelta("b"))),
			...typedEvents(m.map(thinkingDelta("t"))),
			...typedEvents(
				m.map({
					type: "tool_execution_start",
					toolCallId: "tc-1",
					toolName: "bash",
					args: {},
				}),
			),
		].map((e) => e.eventId);
		expect(ids).toEqual(["1", "2", "3", "4"]);
	});
});

describe("EventMapper — streamed text is session-only (live chunk, no comms settle)", () => {
	// A text_delta emits ONE session assistant_text chunk (live trace); text_end
	// settles NO frame (SEA-1708 removed the comms write-through). The session
	// chunk is the only text surface — a delta that stopped emitting, or a
	// text_end that leaked any frame, reddens.
	test("text_delta → one session assistantText chunk", () => {
		const ev = soleTyped(mapper().map(textDelta("foo")));
		if (ev.event.case !== "assistantText")
			throw new Error(`expected assistantText, got ${ev.event.case}`);
		expect(ev.event.value.text).toBe("foo");
	});

	test("text_end → no frame (comms write-through removed)", () => {
		expect(mapper().map(textEnd("hello"))).toEqual([]);
	});

	test("an empty text_delta emits nothing (no empty chunk to coalesce)", () => {
		expect(mapper().map(textDelta(""))).toEqual([]);
	});

	test("text_end with empty content → no frame", () => {
		expect(mapper().map(textEnd(""))).toEqual([]);
	});

	test("text_end after a delta run still emits no frame", () => {
		const m = mapper();
		m.map(textDelta("foo"));
		m.map(textDelta("bar"));
		expect(m.map(textEndNoContent())).toEqual([]);
	});
});

describe("EventMapper — message_id correlates a logical message's chunks", () => {
	// message_id is a per-MESSAGE counter, bumped at each message_start ("0"
	// before the first, "1" after, …). Two chunks within one message share it;
	// a chunk after a new message_start carries the next id; a thinking chunk in
	// the same message shares the message's id. This is how foldSession coalesces
	// a message's streamed chunks into one rendered block.
	function messageIdOf(out: MapOutput[]): string {
		const ev = soleTyped(out);
		if (ev.event.case === "assistantText" || ev.event.case === "thinking")
			return ev.event.value.messageId;
		throw new Error(`expected a text/thinking chunk, got ${ev.event.case}`);
	}

	test("two text_deltas in one message share the same message_id", () => {
		const m = mapper();
		m.map(messageStart());
		const first = messageIdOf(m.map(textDelta("a")));
		const second = messageIdOf(m.map(textDelta("b")));
		expect(first).toBe("1");
		expect(second).toBe("1");
	});

	test("a delta after a second message_start carries the next message_id", () => {
		const m = mapper();
		m.map(messageStart());
		expect(messageIdOf(m.map(textDelta("a")))).toBe("1");
		m.map(messageStart());
		expect(messageIdOf(m.map(textDelta("b")))).toBe("2");
	});

	test("a thinking chunk shares the current message's id", () => {
		const m = mapper();
		m.map(messageStart());
		expect(messageIdOf(m.map(textDelta("a")))).toBe("1");
		expect(messageIdOf(m.map(thinkingDelta("t")))).toBe("1");
	});
});

describe("EventMapper — thinking is a typed session chunk (session-only)", () => {
	// Thinking is trace, not durable conversation: a thinking_delta emits ONE
	// typed `thinking` SessionEvent and NEVER a comms block. An empty delta emits
	// nothing.
	test("thinking_delta → one typed thinking chunk carrying the delta text", () => {
		const ev = soleTyped(mapper().map(thinkingDelta("hmm")));
		if (ev.event.case !== "thinking")
			throw new Error(`expected thinking, got ${ev.event.case}`);
		expect(ev.event.value.text).toBe("hmm");
	});

	test("an empty thinking_delta emits nothing", () => {
		expect(mapper().map(thinkingDelta(""))).toEqual([]);
	});

	test("thinking never produces a conversation block", () => {
		const out = mapper().map(thinkingDelta("reasoning"));
		expect(out).toHaveLength(1);
		expect(out[0].kind).toBe("session");
	});
});

describe("EventMapper — non-content inner events emit nothing", () => {
	// Stream boundaries and tool-call framing inner variants carry no standalone
	// content (tool calls surface via the tool_execution_* events). They route
	// through the inner default arm → [].
	const inners: AssistantMessageEvent[] = [
		{ type: "start", partial: partial() },
		{ type: "text_start", contentIndex: 0, partial: partial() },
		{ type: "thinking_start", contentIndex: 0, partial: partial() },
		{ type: "thinking_end", contentIndex: 0, content: "x", partial: partial() },
		{ type: "toolcall_start", contentIndex: 0, partial: partial() },
		{ type: "done", reason: "stop", message: partial() },
	];
	for (const inner of inners) {
		test(`inner "${inner.type}" → no frame`, () => {
			expect(mapper().map(upd(inner))).toEqual([]);
		});
	}
});

describe("EventMapper — inner error: crash → session ERRORED vs abort → counted unmapped", () => {
	// The pi-ai `error` variant carries reason "error" | "aborted" — NOT the same
	// failure class:
	//   - "error" = an inner/provider failure → a session frame to ERRORED.
	//   - "aborted" = a deliberate cancel, NOT a crash → surfaced as a counted
	//     UnmappedEvent (never conflated with ERRORED, never dropped).
	test('reason "error" → one session frame in state ERRORED', () => {
		expect(
			soleSessionState(
				mapper().map(upd({ type: "error", reason: "error", error: partial() })),
			),
		).toBe(AgentSessionState.ERRORED);
	});

	test('reason "aborted" → one counted UnmappedEvent, NOT ERRORED', () => {
		const out = mapper().map(
			upd({ type: "error", reason: "aborted", error: partial() }),
		);
		expect(out).toHaveLength(1);
		const frame = out[0];
		expect(frame.kind).toBe("unmapped");
		if (frame.kind === "unmapped")
			expect(frame.eventType).toBe("message_update:error");
	});
});

describe("EventMapper — tool call lifecycle (start / update / end)", () => {
	// tool_execution_start → a toolCall in IN_PROGRESS; update → a toolCallUpdate
	// in IN_PROGRESS; end → a toolCallUpdate in COMPLETED (or FAILED on isError).
	// The tool_call_id threads through unchanged. A flipped status reddens.
	test("tool_execution_start → toolCall IN_PROGRESS carrying the tool_call_id", () => {
		const ev = soleTyped(
			mapper().map({
				type: "tool_execution_start",
				toolCallId: "tc-1",
				toolName: "bash",
				args: {},
			}),
		);
		if (ev.event.case !== "toolCall")
			throw new Error(`expected toolCall, got ${ev.event.case}`);
		expect(ev.event.value.toolCallId).toBe("tc-1");
		expect(ev.event.value.status).toBe(AgentToolCallStatus.IN_PROGRESS);
	});

	test("tool_execution_update → toolCallUpdate IN_PROGRESS carrying output", () => {
		const ev = soleTyped(
			mapper().map({
				type: "tool_execution_update",
				toolCallId: "tc-1",
				toolName: "bash",
				args: {},
				partialResult: "partial output",
			}),
		);
		if (ev.event.case !== "toolCallUpdate")
			throw new Error(`expected toolCallUpdate, got ${ev.event.case}`);
		expect(ev.event.value.toolCallId).toBe("tc-1");
		expect(ev.event.value.status).toBe(AgentToolCallStatus.IN_PROGRESS);
		expect(ev.event.value.output).toBe("partial output");
	});

	test("tool_execution_end (success) → toolCallUpdate COMPLETED", () => {
		const ev = soleTyped(
			mapper().map({
				type: "tool_execution_end",
				toolCallId: "tc-1",
				toolName: "bash",
				result: "done",
				isError: false,
			}),
		);
		if (ev.event.case !== "toolCallUpdate")
			throw new Error(`expected toolCallUpdate, got ${ev.event.case}`);
		expect(ev.event.value.status).toBe(AgentToolCallStatus.COMPLETED);
	});

	test("tool_execution_end (isError) → toolCallUpdate FAILED", () => {
		const ev = soleTyped(
			mapper().map({
				type: "tool_execution_end",
				toolCallId: "tc-1",
				toolName: "bash",
				result: "boom",
				isError: true,
			}),
		);
		if (ev.event.case !== "toolCallUpdate")
			throw new Error(`expected toolCallUpdate, got ${ev.event.case}`);
		expect(ev.event.value.status).toBe(AgentToolCallStatus.FAILED);
	});
});

describe("EventMapper — tool call title is intent-or-toolName", () => {
	// SessionToolCall.title is a display string: the trimmed `intent` when
	// present and non-empty, else the tool name. Each row pins one branch.
	function titleOf(event: AgentSessionEvent): string {
		const ev = soleTyped(mapper().map(event));
		if (ev.event.case !== "toolCall")
			throw new Error(`expected toolCall, got ${ev.event.case}`);
		return ev.event.value.title;
	}

	test("intent present → title is the trimmed intent", () => {
		expect(
			titleOf({
				type: "tool_execution_start",
				toolCallId: "tc-1",
				toolName: "bash",
				args: {},
				intent: "  Reading the config  ",
			}),
		).toBe("Reading the config");
	});

	test("no intent → title falls back to the tool name", () => {
		expect(
			titleOf({
				type: "tool_execution_start",
				toolCallId: "tc-1",
				toolName: "bash",
				args: {},
			}),
		).toBe("bash");
	});

	test("empty/whitespace intent → title falls back to the tool name", () => {
		expect(
			titleOf({
				type: "tool_execution_start",
				toolCallId: "tc-1",
				toolName: "read",
				args: {},
				intent: "   ",
			}),
		).toBe("read");
	});
});

describe("EventMapper — tool result output extraction", () => {
	// The `output` field mirrors the ACP mapper's extractReadableText: a bare
	// string trimmed, an Error's message, a `text`/`errorMessage`/`message`
	// property, and a hard cap at 4000 chars (adding "…").
	function outputOf(result: unknown): string {
		const ev = soleTyped(
			mapper().map({
				type: "tool_execution_end",
				toolCallId: "tc-1",
				toolName: "bash",
				result,
				isError: false,
			}),
		);
		if (ev.event.case !== "toolCallUpdate")
			throw new Error(`expected toolCallUpdate, got ${ev.event.case}`);
		return ev.event.value.output;
	}

	test("a bare string result is trimmed onto output", () => {
		expect(outputOf("  hello world  ")).toBe("hello world");
	});

	test("a { text } result surfaces the text property", () => {
		expect(outputOf({ text: "readable text" })).toBe("readable text");
	});

	test("an Error result surfaces its message", () => {
		expect(outputOf(new Error("it broke"))).toBe("it broke");
	});

	test("output is capped at 4000 chars with a … suffix", () => {
		const out = outputOf("a".repeat(5000));
		expect(out.length).toBe(4000);
		expect(out.endsWith("…")).toBe(true);
	});
});

describe("EventMapper — file diff extraction from a tool result", () => {
	// extractDiffs reads `details.perFileResults[]` (multi-file) or the single
	// `details` object. Each valid entry becomes a SessionFileDiff {path,
	// oldText?, newText}. A creation has no oldText; an isError entry, or one
	// lacking a path / any text, is skipped.
	function diffsOf(result: unknown) {
		const ev = soleTyped(
			mapper().map({
				type: "tool_execution_end",
				toolCallId: "tc-1",
				toolName: "edit",
				result,
				isError: false,
			}),
		);
		if (ev.event.case !== "toolCallUpdate")
			throw new Error(`expected toolCallUpdate, got ${ev.event.case}`);
		return ev.event.value.diffs;
	}

	test("perFileResults → one SessionFileDiff per valid entry (path/oldText/newText)", () => {
		const diffs = diffsOf({
			details: {
				perFileResults: [{ path: "a.ts", oldText: "old", newText: "new" }],
			},
		});
		expect(diffs).toHaveLength(1);
		expect(diffs[0].path).toBe("a.ts");
		expect(diffs[0].oldText).toBe("old");
		expect(diffs[0].newText).toBe("new");
	});

	test("a creation (no oldText) leaves oldText undefined", () => {
		const diffs = diffsOf({
			details: { perFileResults: [{ path: "new.ts", newText: "created" }] },
		});
		expect(diffs).toHaveLength(1);
		expect(diffs[0].oldText).toBeUndefined();
		expect(diffs[0].newText).toBe("created");
	});

	test("an isError entry is skipped", () => {
		const diffs = diffsOf({
			details: {
				perFileResults: [
					{ path: "ok.ts", oldText: "a", newText: "b" },
					{ path: "bad.ts", oldText: "x", newText: "y", isError: true },
				],
			},
		});
		expect(diffs).toHaveLength(1);
		expect(diffs[0].path).toBe("ok.ts");
	});

	test("a single-file details object (no perFileResults) also yields a diff", () => {
		const diffs = diffsOf({
			details: { path: "solo.ts", oldText: "before", newText: "after" },
		});
		expect(diffs).toHaveLength(1);
		expect(diffs[0].path).toBe("solo.ts");
		expect(diffs[0].newText).toBe("after");
	});

	// The guard branches that ELIDE an entry rather than emit a malformed diff:
	// a non-record result or details (extractDiffs early-returns []), and a
	// per-file entry missing a path or carrying neither oldText nor newText
	// (buildDiff returns undefined, so the entry is dropped). Each row asserts
	// the emitted diff set is empty — a regression that stopped guarding would
	// surface a diff with an empty path or no text, reddening the row.
	const elided: { name: string; result: unknown }[] = [
		{ name: "a non-record result", result: "not-a-record" },
		{
			name: "a result whose details is not a record",
			result: { details: "nope" },
		},
		{
			name: "a details with no perFileResults and no path",
			result: { details: {} },
		},
		{
			name: "a per-file entry missing a path",
			result: { details: { perFileResults: [{ oldText: "a", newText: "b" }] } },
		},
		{
			name: "a per-file entry with an empty path",
			result: {
				details: { perFileResults: [{ path: "", oldText: "a", newText: "b" }] },
			},
		},
		{
			name: "a per-file entry with neither oldText nor newText",
			result: { details: { perFileResults: [{ path: "a.ts" }] } },
		},
	];
	for (const row of elided) {
		test(`${row.name} emits no diff`, () => {
			expect(diffsOf(row.result)).toHaveLength(0);
		});
	}
});

describe("EventMapper — plan from a todo tool result", () => {
	// A `tool_execution_end` for the `todo` tool with a `details.phases[].tasks[]`
	// snapshot emits a SESSION plan alongside the tool-call settle → map() returns
	// [toolCallUpdate, plan]. Task statuses map through planStatus (abandoned folds
	// to COMPLETED). A FAILED todo (isError) emits no plan.
	function todoEnd(result: unknown, isError = false): AgentSessionEvent {
		return {
			type: "tool_execution_end",
			toolCallId: "tc-todo",
			toolName: "todo",
			result,
			isError,
		};
	}

	test("todo end → [toolCallUpdate, plan] with entries + statuses mapped (abandoned→COMPLETED)", () => {
		const out = mapper().map(
			todoEnd({
				details: {
					phases: [
						{
							name: "P1",
							tasks: [
								{ content: "do a", status: "in_progress" },
								{ content: "do b", status: "abandoned" },
								{ content: "do c", status: "pending" },
							],
						},
					],
				},
			}),
		);
		const events = typedEvents(out);
		expect(events).toHaveLength(2);
		expect(events[0].event.case).toBe("toolCallUpdate");
		const plan = events[1];
		if (plan.event.case !== "plan")
			throw new Error(`expected plan, got ${plan.event.case}`);
		expect(plan.event.value.entries.map((e) => e.content)).toEqual([
			"do a",
			"do b",
			"do c",
		]);
		expect(plan.event.value.entries.map((e) => e.status)).toEqual([
			AgentPlanEntryStatus.IN_PROGRESS,
			AgentPlanEntryStatus.COMPLETED,
			AgentPlanEntryStatus.PENDING,
		]);
	});

	test("a FAILED todo end → only the toolCallUpdate, no plan", () => {
		const out = mapper().map(
			todoEnd(
				{ details: { phases: [{ name: "P1", tasks: [] }] } },
				true, // isError
			),
		);
		const events = typedEvents(out);
		expect(events).toHaveLength(1);
		expect(events[0].event.case).toBe("toolCallUpdate");
		if (events[0].event.case === "toolCallUpdate")
			expect(events[0].event.value.status).toBe(AgentToolCallStatus.FAILED);
	});

	test("phases that are not an array → extractor returns undefined → no plan", () => {
		// extractPlanEntries early-returns undefined when details.phases is not an
		// array, so map() emits ONLY the toolCallUpdate — no plan frame. A
		// regression that emitted an empty plan instead would push a second event.
		const out = mapper().map(todoEnd({ details: { phases: "not-an-array" } }));
		const events = typedEvents(out);
		expect(events).toHaveLength(1);
		expect(events[0].event.case).toBe("toolCallUpdate");
	});

	test("an empty-content task is skipped, leaving the plan's entries empty", () => {
		// A present snapshot with no VALID task: content is empty, so the task is
		// dropped (continue) but the extractor still returns [] (not undefined),
		// so a plan IS emitted — with zero entries. This pins the distinction
		// between "no snapshot" (no plan) and "snapshot, no valid task" (empty
		// plan): a regression that emitted the empty-content task would surface a
		// content-less plan entry, reddening the length assertion.
		const out = mapper().map(
			todoEnd({
				details: {
					phases: [{ name: "P1", tasks: [{ content: "", status: "pending" }] }],
				},
			}),
		);
		const events = typedEvents(out);
		expect(events).toHaveLength(2);
		const plan = events[1];
		if (plan.event.case !== "plan")
			throw new Error(`expected plan, got ${plan.event.case}`);
		expect(plan.event.value.entries).toHaveLength(0);
	});
});

describe("EventMapper — todo_reminder / todo_auto_clear map to plans", () => {
	// A todo_reminder carries the current todos → a plan snapshot; a
	// todo_auto_clear → an empty plan. The todos are already typed on the event.
	test("todo_reminder → a plan with entries mapped from event.todos", () => {
		const ev = soleTyped(
			mapper().map({
				type: "todo_reminder",
				todos: [
					{ content: "task one", status: "completed" },
					{ content: "task two", status: "pending" },
				],
				attempt: 1,
				maxAttempts: 3,
			}),
		);
		if (ev.event.case !== "plan")
			throw new Error(`expected plan, got ${ev.event.case}`);
		expect(ev.event.value.entries.map((e) => e.content)).toEqual([
			"task one",
			"task two",
		]);
		expect(ev.event.value.entries.map((e) => e.status)).toEqual([
			AgentPlanEntryStatus.COMPLETED,
			AgentPlanEntryStatus.PENDING,
		]);
	});

	test("todo_auto_clear → an empty plan", () => {
		const ev = soleTyped(mapper().map({ type: "todo_auto_clear" }));
		if (ev.event.case !== "plan")
			throw new Error(`expected plan, got ${ev.event.case}`);
		expect(ev.event.value.entries).toEqual([]);
	});
});

describe("EventMapper — unmapped session events (never dropped, never crash)", () => {
	// The default arm surfaces a SINGLE counted UnmappedEvent for any variant the
	// map does not cover: the `notice` variant (staged), the orchestration-only
	// superset variants, and any future/unknown type. The eventType echoes the
	// event's own type.
	const rows: { name: string; event: AgentSessionEvent; eventType: string }[] =
		[
			{
				name: "notice",
				event: { type: "notice", level: "info", message: "heads up" },
				eventType: "notice",
			},
			{
				name: "auto_retry_start (orchestration)",
				event: {
					type: "auto_retry_start",
					attempt: 1,
					maxAttempts: 3,
					delayMs: 100,
					errorMessage: "transient",
				},
				eventType: "auto_retry_start",
			},
		];
	for (const row of rows) {
		test(`${row.name} → one counted UnmappedEvent (${row.eventType})`, () => {
			const out = mapper().map(row.event);
			expect(out).toHaveLength(1);
			const frame = out[0];
			expect(frame.kind).toBe("unmapped");
			if (frame.kind === "unmapped")
				expect(frame.eventType).toBe(row.eventType);
		});
	}

	test("an unrecognized event.type yields exactly one UnmappedEvent, not [] and not a throw", () => {
		const out = mapper().map({
			type: "future_event",
		} as unknown as AgentSessionEvent);
		expect(out).toHaveLength(1);
		const frame = out[0];
		expect(frame.kind).toBe("unmapped");
		if (frame.kind === "unmapped") expect(frame.eventType).toBe("future_event");
	});
});
