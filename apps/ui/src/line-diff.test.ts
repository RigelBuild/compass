import { describe, expect, test } from "bun:test";
import {
	DEFAULT_MAX_EDIT_LENGTH,
	type DiffRow,
	diffRows,
	splitLines,
} from "./line-diff";

// RED acceptance spec for T1 (design record §235-339): the pure line-diff
// module `line-diff.ts` does not exist yet, so the import above fails and this
// whole file is RED until the implementer builds it. The fixtures below are
// committed VERBATIM from the design record and pin the P1/P2 defects the real
// jsdiff-backed diff fixes: reordered lines, duplicate lines, trailing-newline
// handling, new-file all-adds, and the edit-distance budget guard (full
// granularity under budget vs. coarse fallback over budget).

describe("splitLines", () => {
	// Newline-terminated split: a trailing "\n" does NOT yield a phantom empty
	// final line, but an interior/EOF blank line IS preserved.
	test.each([
		["", []],
		["a", ["a"]],
		["a\n", ["a"]],
		["a\nb", ["a", "b"]],
		["a\n\n", ["a", ""]],
	])("splitLines(%o) -> %o", (input, expected) => {
		expect(splitLines(input)).toEqual(expected);
	});
});

describe("diffRows", () => {
	test("reorder: two rows, one del + one add, same text in {a,b}", () => {
		const rows = diffRows("a\nb", "b\na");
		expect(rows.length).toBe(2);
		const dels = rows.filter((r) => r.kind === "del");
		const adds = rows.filter((r) => r.kind === "add");
		expect(dels.length).toBe(1);
		expect(adds.length).toBe(1);
		// del precedes add (standard hunk order).
		expect(rows[0]?.kind).toBe("del");
		expect(rows[1]?.kind).toBe("add");
		// Which line Myers keeps is an undocumented tie-break, so assert only the
		// del.text === add.text invariant and membership, not a pinned line.
		expect(dels[0]?.text).toBe(adds[0]?.text as string);
		expect(["a", "b"]).toContain(dels[0]?.text);
		// jsdiff-9.0.0-pinned: its removal-leaning tie-break keeps `b` and emits
		// `[{del "a"},{add "a"}]`. A future major could legally flip this while
		// staying minimal — the invariant assertions above are the real contract.
		expect(dels[0]?.text).toBe("a");
	});

	test("dup-line: adding a duplicate line yields one add", () => {
		expect(diffRows("a", "a\na")).toEqual([{ kind: "add", text: "a" }]);
	});

	test("dup-line: removing a duplicate line yields one del", () => {
		expect(diffRows("a\na", "a")).toEqual([{ kind: "del", text: "a" }]);
	});

	test("empty: both empty -> no rows", () => {
		expect(diffRows("", "")).toEqual([]);
	});

	test("empty: null old, empty new -> no rows", () => {
		expect(diffRows(null, "")).toEqual([]);
	});

	test("empty: old text, empty new -> one del", () => {
		expect(diffRows("a", "")).toEqual([{ kind: "del", text: "a" }]);
	});

	test("trailing-newline: added line, no phantom blank row", () => {
		expect(diffRows("a\n", "a\nb\n")).toEqual([{ kind: "add", text: "b" }]);
	});

	test("trailing-newline: adding only a trailing newline -> no rows (accepted blind spot)", () => {
		// Deliberate: `splitLines` trims the trailing "\n", so "a" and "a\n" split
		// to the same tokens. This blind spot is accepted (Approach §Line
		// splitting) and pinned here so the choice cannot regress silently.
		expect(diffRows("a", "a\n")).toEqual([]);
	});

	test("trailing-newline: a real EOF blank line IS rendered", () => {
		expect(diffRows("a\n", "a\n\n")).toEqual([{ kind: "add", text: "" }]);
	});

	test("new file: null old -> two adds in order", () => {
		expect(diffRows(null, "x\ny")).toEqual([
			{ kind: "add", text: "x" },
			{ kind: "add", text: "y" },
		]);
	});

	test("changed-run ordering: del then add", () => {
		expect(diffRows("a\nold\nz", "a\nnew\nz")).toEqual([
			{ kind: "del", text: "old" },
			{ kind: "add", text: "new" },
		]);
	});

	test("large input under budget: full granularity drops unchanged lines", () => {
		// old and new share every even-indexed line and differ on every
		// odd-indexed line (changed lines interleaved with unchanged). The shared
		// lines keep the edit distance well below the disjoint worst case.
		const N = 2000;
		const old = Array.from({ length: N }, (_, i) =>
			i % 2 === 0 ? `shared-${i}` : `old-${i}`,
		).join("\n");
		const next = Array.from({ length: N }, (_, i) =>
			i % 2 === 0 ? `shared-${i}` : `new-${i}`,
		).join("\n");
		// Explicit budget ABOVE the resulting edit distance so diffArrays runs the
		// real diff (does NOT bail to the coarse fallback).
		const rows = diffRows(old, next, 10_000);
		// A known unchanged even line renders in NEITHER a del NOR an add row.
		// Full granularity omits unchanged lines; the coarse fallback would emit
		// it (as a del AND an add) — so this assertion discriminates the
		// non-fallback path.
		const shared0 = rows.filter((r) => r.text === "shared-0");
		expect(shared0.length).toBe(0);
	});

	test("over-budget fallback: disjoint input yields coarse all-dels-then-all-adds", () => {
		// Fully disjoint 2000-line inputs sharing no line: edit distance D ≈ 4000.
		const N = 2000;
		const old = Array.from({ length: N }, (_, i) => `old-${i}`).join("\n");
		const next = Array.from({ length: N }, (_, i) => `new-${i}`).join("\n");
		// Explicit budget BELOW D forces diffArrays to return undefined and
		// diffRows to fall back to the coarse rendering without a freeze.
		const rows = diffRows(old, next, 100);
		expect(rows.length).toBe(2 * N);
		const dels = rows.slice(0, N);
		const adds = rows.slice(N);
		// All 2000 dels precede all 2000 adds.
		expect(dels.every((r) => r.kind === "del")).toBe(true);
		expect(adds.every((r) => r.kind === "add")).toBe(true);
		expect(dels[0]).toEqual({ kind: "del", text: "old-0" });
		expect(adds[0]).toEqual({ kind: "add", text: "new-0" });
		expect(dels.at(-1)).toEqual({ kind: "del", text: `old-${N - 1}` });
		expect(adds.at(-1)).toEqual({ kind: "add", text: `new-${N - 1}` });
	});
});

describe("DEFAULT_MAX_EDIT_LENGTH", () => {
	// The concrete value is Open Question 2 (parked for Matt), but the export must
	// exist as a positive finite bound so the no-budget call path is guarded.
	test("is a positive finite number", () => {
		expect(Number.isFinite(DEFAULT_MAX_EDIT_LENGTH)).toBe(true);
		expect(DEFAULT_MAX_EDIT_LENGTH).toBeGreaterThan(0);
	});
});

// Type-level anchor: DiffRow is imported and used to type the rows above.
const _typeAnchor: DiffRow = { kind: "add", text: "" };
void _typeAnchor;
