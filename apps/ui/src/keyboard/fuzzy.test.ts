import { describe, expect, test } from "bun:test";
import { fuzzyScore } from "./fuzzy";

// The in-house action-mode fuzzy scorer (RIG-2483, D2). Pure module — run with
// MOON_TOOLCHAIN_FORCE_GLOBALS=true bun test --conditions browser. These defend
// the contract the palette ranking relies on: null = no match, higher = better,
// and the boundary/start/contiguity bonuses that make "bri" → "Bridge" win.

describe("fuzzyScore", () => {
	test("null when the query is not a subsequence of the haystack", () => {
		expect(fuzzyScore("xyz", "Go to Bridge")).toBeNull();
		expect(fuzzyScore("zzz", "Settings")).toBeNull();
	});

	test("an empty (or whitespace) query matches everything with a neutral score", () => {
		expect(fuzzyScore("", "anything")).toBe(0);
		expect(fuzzyScore("   ", "anything")).toBe(0);
	});

	test("a subsequence match scores non-null", () => {
		expect(fuzzyScore("bri", "Bridge")).not.toBeNull();
		expect(fuzzyScore("gtb", "Go to Bridge")).not.toBeNull(); // sparse subsequence
	});

	test("start-of-string outscores a mid-string match of the same length", () => {
		const atStart = fuzzyScore("se", "Settings");
		const midWord = fuzzyScore("se", "Go to Settings");
		expect(atStart).not.toBeNull();
		expect(midWord).not.toBeNull();
		expect(atStart as number).toBeGreaterThan(midWord as number);
	});

	test("a word-boundary match outscores an interior one", () => {
		// "b" at the "Bridge" word boundary beats "b" buried inside "backlog"'s tail.
		const boundary = fuzzyScore("b", "Go to Bridge");
		const interior = fuzzyScore("k", "Backlog"); // 'k' is interior, no boundary
		expect(boundary).not.toBeNull();
		expect(interior).not.toBeNull();
		expect(boundary as number).toBeGreaterThan(interior as number);
	});

	test("contiguous matches outscore scattered ones for the same query", () => {
		const contiguous = fuzzyScore("set", "Settings");
		const scattered = fuzzyScore("set", "Secure entry token"); // s..e..t across words
		expect(contiguous).not.toBeNull();
		expect(scattered).not.toBeNull();
		expect(contiguous as number).toBeGreaterThan(scattered as number);
	});

	test("case-insensitive", () => {
		expect(fuzzyScore("BRIDGE", "bridge")).not.toBeNull();
		expect(fuzzyScore("bridge", "BRIDGE")).not.toBeNull();
	});

	test("camelCase step counts as a word boundary", () => {
		// The 'W' in "agentWorkspace" is a boundary via the lower→upper step.
		const camel = fuzzyScore("w", "agentWorkspace");
		expect(camel).not.toBeNull();
		expect(camel as number).toBeGreaterThan(0);
	});
});
