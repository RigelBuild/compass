import { describe, expect, test } from "bun:test";
import {
	type FileDiff,
	foldSession,
	type SessionEvent,
} from "./session-events";

// RED until session-events.ts lands (T-U1 impl). The `./session-events` module
// does not exist yet — this import fails to resolve, so every test here is red.
// These tests defend the observable fold contract (record §431-438): the pure
// foldSession(events) -> TraceItem[] reduction. Inputs are built inline (small,
// per-edge) so each assertion pins a fold rule, not today's stub values.

// ── Minimal event builders. `id` and `atUnixMs` are required on every
//    SessionEvent, so they are always supplied explicitly; the kind-specific
//    fields carry the meaning under test. ──────────────────────────────────────
function assistantText(
	id: string,
	atUnixMs: number,
	messageId: string,
	text: string,
): SessionEvent {
	return { id, atUnixMs, kind: "assistant_text", messageId, text };
}
function thinking(
	id: string,
	atUnixMs: number,
	messageId: string,
	text: string,
): SessionEvent {
	return { id, atUnixMs, kind: "thinking", messageId, text };
}
function toolCall(
	id: string,
	atUnixMs: number,
	toolCallId: string,
	title: string,
	status: "pending" | "in_progress" | "completed" | "failed",
): SessionEvent {
	return { id, atUnixMs, kind: "tool_call", toolCallId, title, status };
}
function toolCallUpdate(
	id: string,
	atUnixMs: number,
	toolCallId: string,
	status: "pending" | "in_progress" | "completed" | "failed",
	extra: { output?: string; diffs?: FileDiff[] } = {},
): SessionEvent {
	return {
		id,
		atUnixMs,
		kind: "tool_call_update",
		toolCallId,
		status,
		...extra,
	};
}
function plan(
	id: string,
	atUnixMs: number,
	entries: {
		content: string;
		status: "pending" | "in_progress" | "completed";
	}[],
): SessionEvent {
	return { id, atUnixMs, kind: "plan", entries };
}
function notice(
	id: string,
	atUnixMs: number,
	text: string,
	link?: string,
): SessionEvent {
	return { id, atUnixMs, kind: "notice", text, ...(link ? { link } : {}) };
}

describe("foldSession — tool call folding", () => {
	// A tool_call_update folds into its tool_call by toolCallId: the emitted
	// {kind:"tool"} item carries the LATEST status/output/diffs from the update,
	// and `call` is the originating tool_call event.
	test("update folds into its tool_call by toolCallId", () => {
		const diff: FileDiff = {
			path: "src/foo.ts",
			oldText: null,
			newText: "changed",
		};
		const items = foldSession([
			toolCall("e1", 100, "tc1", "run tests", "pending"),
			toolCallUpdate("e2", 200, "tc1", "completed", {
				output: "ok",
				diffs: [diff],
			}),
		]);

		expect(items).toHaveLength(1);
		const item = items[0];
		expect(item.kind).toBe("tool");
		if (item.kind !== "tool") throw new Error("expected tool item");
		expect(item.toolCallId).toBe("tc1");
		expect(item.status).toBe("completed");
		expect(item.output).toBe("ok");
		expect(item.diffs).toHaveLength(1);
		expect(item.call?.title).toBe("run tests");
	});

	// An orphan tool_call_update (no preceding tool_call for its toolCallId) still
	// becomes its own {kind:"tool"} item keyed by toolCallId, with call undefined.
	test("orphan update becomes its own tool item with call undefined", () => {
		const items = foldSession([
			toolCallUpdate("e1", 100, "tc9", "in_progress"),
		]);

		expect(items).toHaveLength(1);
		const item = items[0];
		expect(item.kind).toBe("tool");
		if (item.kind !== "tool") throw new Error("expected tool item");
		expect(item.toolCallId).toBe("tc9");
		expect(item.status).toBe("in_progress");
		expect(item.call).toBeUndefined();
	});
});

describe("foldSession — plan coalescing", () => {
	// A later plan replaces an earlier one: exactly one plan item, carrying the
	// latest entries. Position is a coalesced trailing slot — the record fixes
	// "one plan item, latest entries", so that is all we assert.
	test("latest plan wins — one plan item carrying the later entries", () => {
		const entriesA = [{ content: "step A", status: "pending" as const }];
		const entriesB = [
			{ content: "step B1", status: "completed" as const },
			{ content: "step B2", status: "in_progress" as const },
		];
		const items = foldSession([
			plan("p1", 100, entriesA),
			plan("p2", 200, entriesB),
		]);

		const planItems = items.filter((i) => i.kind === "plan");
		expect(planItems).toHaveLength(1);
		const planItem = planItems[0];
		if (planItem.kind !== "plan") throw new Error("expected plan item");
		expect(planItem.entries).toEqual(entriesB);
	});
});

describe("foldSession — text/thinking coalescing", () => {
	// Consecutive assistant_text deltas sharing a messageId coalesce into ONE text
	// item with the concatenated text (no separator).
	test("consecutive same-messageId assistant_text deltas coalesce", () => {
		const items = foldSession([
			assistantText("e1", 100, "m1", "Hel"),
			assistantText("e2", 200, "m1", "lo "),
			assistantText("e3", 300, "m1", "world"),
		]);

		expect(items).toHaveLength(1);
		const item = items[0];
		expect(item.kind).toBe("text");
		if (item.kind !== "text") throw new Error("expected text item");
		expect(item.messageId).toBe("m1");
		expect(item.text).toBe("Hello world");
	});

	// Differing messageIds do not coalesce — they stay as separate text items.
	test("differing messageIds stay separate", () => {
		const items = foldSession([
			assistantText("e1", 100, "m1", "a"),
			assistantText("e2", 200, "m2", "b"),
		]);

		const texts = items.filter((i) => i.kind === "text");
		expect(texts).toHaveLength(2);
		expect(texts.map((t) => (t.kind === "text" ? t.text : ""))).toEqual([
			"a",
			"b",
		]);
	});

	// A tool_call interleaved between two same-messageId deltas breaks the merge:
	// the two texts become separate items, with the tool item between them.
	test("interleaved tool_call breaks the merge", () => {
		const items = foldSession([
			assistantText("e1", 100, "m1", "a"),
			toolCall("e2", 200, "tc1", "do thing", "pending"),
			assistantText("e3", 300, "m1", "b"),
		]);

		expect(items.map((i) => i.kind)).toEqual(["text", "tool", "text"]);
		const first = items[0];
		const last = items[2];
		if (first.kind !== "text" || last.kind !== "text") {
			throw new Error("expected text/tool/text");
		}
		expect(first.text).toBe("a");
		expect(last.text).toBe("b");
	});

	// thinking deltas coalesce exactly like assistant_text.
	test("consecutive same-messageId thinking deltas coalesce", () => {
		const items = foldSession([
			thinking("e1", 100, "t1", "I should "),
			thinking("e2", 200, "t1", "reflect."),
		]);

		expect(items).toHaveLength(1);
		const item = items[0];
		expect(item.kind).toBe("thinking");
		if (item.kind !== "thinking") throw new Error("expected thinking item");
		expect(item.messageId).toBe("t1");
		expect(item.text).toBe("I should reflect.");
	});
});

describe("foldSession — notice and ordering", () => {
	// A notice passes through in place, keeping its relative position, and the
	// emitted item carries the notice event itself.
	test("notice passes through in order carrying its event", () => {
		const noticeEvent = notice("e2", 200, "heads up");
		const items = foldSession([
			assistantText("e1", 100, "m1", "before"),
			noticeEvent,
			assistantText("e3", 300, "m2", "after"),
		]);

		expect(items.map((i) => i.kind)).toEqual(["text", "notice", "text"]);
		const noticeItem = items[1];
		if (noticeItem.kind !== "notice") throw new Error("expected notice item");
		expect(noticeItem.event).toBe(noticeEvent);
		expect(
			noticeItem.event.kind === "notice" ? noticeItem.event.text : "",
		).toBe("heads up");
	});

	// A mixed sequence preserves input order in the output items.
	test("stable ordering — mixed sequence preserves input order", () => {
		const items = foldSession([
			assistantText("e1", 100, "m1", "hello"),
			toolCall("e2", 200, "tc1", "run", "pending"),
			plan("e3", 300, [{ content: "s", status: "pending" }]),
			notice("e4", 400, "done"),
		]);

		expect(items.map((i) => i.kind)).toEqual([
			"text",
			"tool",
			"plan",
			"notice",
		]);
	});
});

describe("foldSession — purity (inputs never mutated)", () => {
	// The fold is documented pure: it never mutates an input event and builds
	// every TraceItem fresh (session-events.ts §"Pure: input events are never
	// mutated"). Freezing every event AND the array proves no in-place write
	// happens (a mutating refactor throws in strict mode / on the frozen object),
	// and a deep before/after comparison proves nothing was reshaped. Reddens if
	// any future refactor folds by mutating the incoming event.
	test("frozen inputs are neither thrown on nor mutated by the fold", () => {
		const diff: FileDiff = {
			path: "src/foo.ts",
			oldText: "old",
			newText: "new",
		};
		const events: SessionEvent[] = [
			assistantText("e1", 100, "m1", "Hel"),
			assistantText("e2", 200, "m1", "lo"),
			toolCall("e3", 300, "tc1", "run tests", "pending"),
			toolCallUpdate("e4", 400, "tc1", "completed", {
				output: "ok",
				diffs: [diff],
			}),
			plan("e5", 500, [{ content: "step", status: "pending" }]),
			notice("e6", 600, "heads up"),
		];
		// Freeze every event object (and nested payloads that the fold reads) and
		// the array itself, so any attempt to write through them would throw.
		for (const e of events) {
			Object.freeze(e);
			if (e.kind === "plan")
				for (const entry of e.entries) Object.freeze(entry);
			if (e.kind === "tool_call_update" && e.diffs)
				for (const d of e.diffs) Object.freeze(d);
		}
		Object.freeze(events);

		// Structural snapshot of the inputs BEFORE folding.
		const before = structuredClone(events);

		expect(() => foldSession(events)).not.toThrow();

		// Inputs are byte-for-byte identical after the fold — nothing mutated.
		expect(events).toEqual(before);
	});
});

describe("foldSession — tool update preserves prior output/diffs", () => {
	// A status-only update must fold latest status in WITHOUT nulling an
	// output/diffs set by an earlier update (session-events.ts:133-135 guards on
	// `!== undefined`). Reddens if the guard is dropped and a bare status update
	// clobbers prior output/diffs.
	test("later status-only update retains prior output and diffs", () => {
		const diff: FileDiff = {
			path: "src/foo.ts",
			oldText: null,
			newText: "added",
		};
		const items = foldSession([
			toolCall("e1", 100, "tc1", "run tests", "pending"),
			toolCallUpdate("e2", 200, "tc1", "in_progress", {
				output: "first",
				diffs: [diff],
			}),
			toolCallUpdate("e3", 300, "tc1", "completed"),
		]);

		expect(items).toHaveLength(1);
		const item = items[0];
		if (item.kind !== "tool") throw new Error("expected tool item");
		// Latest status won…
		expect(item.status).toBe("completed");
		// …but the output from update-1 survived the output-less update-2.
		expect(item.output).toBe("first");
		// …and so did the diffs.
		expect(item.diffs).toEqual([diff]);
	});
});
