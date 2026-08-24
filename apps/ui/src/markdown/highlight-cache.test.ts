import { afterEach, describe, expect, test } from "bun:test";
import { clearHighlightCache, isLeadingEdgeHighlight } from "./highlight-cache";

// A window large enough that no snapshot is ever time-pruned within a single
// test — so these cases isolate the classification logic (lang match, strict
// extension, newest-first scan, size cap) from wall-clock timing. The `windowMs
// = 0` case below deliberately exercises the time-window boundary instead.
const WINDOW = 100_000;

describe("isLeadingEdgeHighlight", () => {
	afterEach(() => {
		clearHighlightCache();
	});

	test("a fresh (lang, code) highlights on the leading edge", () => {
		expect(isLeadingEdgeHighlight("ts", "const a", WINDOW)).toBe(true);
	});

	test("re-scheduling the identical (lang, code) is a leading edge, not a growth tick", () => {
		expect(isLeadingEdgeHighlight("ts", "const a", WINDOW)).toBe(true);
		// identical code is not a *strict* extension of its own prior snapshot.
		expect(isLeadingEdgeHighlight("ts", "const a", WINDOW)).toBe(true);
	});

	test("code that strictly extends a recent same-lang snapshot is a debounced growth tick", () => {
		expect(isLeadingEdgeHighlight("ts", "const a", WINDOW)).toBe(true);
		expect(isLeadingEdgeHighlight("ts", "const a = 1", WINDOW)).toBe(false);
	});

	test("a strict extension in a different language is a leading edge (lang must match)", () => {
		expect(isLeadingEdgeHighlight("ts", "const a", WINDOW)).toBe(true);
		expect(isLeadingEdgeHighlight("py", "const a = 1", WINDOW)).toBe(true);
	});

	test("an interleaved sibling fence does not steal a stream's growth classification", () => {
		// Two fences scheduled in one reconcile tick (document order), then both
		// grow in the next — the exact multi-fence case the single-slot gate got
		// wrong. Each grower must match its OWN prior snapshot despite the sibling
		// snapshot appended in between.
		expect(isLeadingEdgeHighlight("ts", "x", WINDOW)).toBe(true); // F1 v1
		expect(isLeadingEdgeHighlight("ts", "a", WINDOW)).toBe(true); // F2 v1
		expect(isLeadingEdgeHighlight("ts", "xy", WINDOW)).toBe(false); // F1 v2 — extends "x"
		expect(isLeadingEdgeHighlight("ts", "ab", WINDOW)).toBe(false); // F2 v2 — extends "a"
	});

	test("windowMs = 0 disables growth detection — every tick is a leading edge", () => {
		expect(isLeadingEdgeHighlight("ts", "const a", 0)).toBe(true);
		// A strict extension, but a zero-width window admits no recent snapshot.
		expect(isLeadingEdgeHighlight("ts", "const a = 1", 0)).toBe(true);
	});

	test("clearHighlightCache resets the snapshots so a later extension reads as fresh", () => {
		expect(isLeadingEdgeHighlight("ts", "const a", WINDOW)).toBe(true);
		clearHighlightCache();
		// Without the reset this would classify as a growth tick (false).
		expect(isLeadingEdgeHighlight("ts", "const a = 1", WINDOW)).toBe(true);
	});

	test("the snapshot list is size-capped — extending a snapshot evicted by the cap reads as a leading edge", () => {
		// Push MAX_RECENT + 1 distinct, non-prefix-related snapshots inside one
		// window (trailing "." keeps e.g. "f1." from being a prefix of "f10.").
		// The size cap keeps the newest 64, so the oldest ("f0.") is evicted.
		const CAP = 64;
		for (let i = 0; i <= CAP; i++) {
			expect(isLeadingEdgeHighlight("ts", `f${i}.`, WINDOW)).toBe(true);
		}
		// Extending the evicted oldest: no surviving snapshot matches → leading edge.
		expect(isLeadingEdgeHighlight("ts", "f0.x", WINDOW)).toBe(true);
		// Extending a snapshot still within the cap: a growth tick.
		expect(isLeadingEdgeHighlight("ts", `f${CAP}.x`, WINDOW)).toBe(false);
	});
});
