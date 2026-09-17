// Unit tests for the font-coverage-gate's pure core (index.ts).
//
// This gate is the oracle that lets RIG-3742 retire the Unifont fallback pin,
// so this suite defends the machine-readable contract: the cmap parser reads
// the REAL font faces (format 4 for Space Mono, format 12 for Departure Mono's
// astral coverage), the source scanner ignores comments/tests but flags
// rendered characters, and the evaluate/mode cores behave. The font assertions
// (U+2212 present in Space Mono, U+27E9 absent, U+27E9 present in Departure)
// are measured facts, not guesses.

import { describe, expect, test } from "bun:test";
import {
	cmapCodepoints,
	evaluate,
	type Finding,
	resolveMode,
	scanSource,
	stripComments,
} from "./index.ts";

const FONTS = `${import.meta.dir}/../../apps/eng-docs/public/fonts`;
const bytes = async (name: string): Promise<Uint8Array> =>
	new Uint8Array(await Bun.file(`${FONTS}/${name}`).arrayBuffer());

// ---------------------------------------------------------------------------
// cmapCodepoints — against the REAL font files.
// ---------------------------------------------------------------------------

describe("cmapCodepoints", () => {
	test("Space Mono (format 4): covers U+2212 / U+2192, lacks U+27E9 / U+2387", async () => {
		const cp = cmapCodepoints(await bytes("SpaceMono-Regular.ttf"));
		expect(cp.has(0x2212)).toBe(true); // MINUS SIGN
		expect(cp.has(0x2192)).toBe(true); // RIGHTWARDS ARROW
		expect(cp.has(0x27e9)).toBe(false); // MATHEMATICAL RIGHT ANGLE BRACKET
		expect(cp.has(0x2387)).toBe(false); // ALTERNATIVE KEY SYMBOL
	});

	test("Departure Mono (format 12 / CFF OTTO): covers U+27E9", async () => {
		const cp = cmapCodepoints(await bytes("DepartureMono-Regular.otf"));
		// U+27E9 is absent from Space Mono but present here — proving both the
		// OTTO/CFF sfnt path and the format-12 branch actually run.
		expect(cp.has(0x27e9)).toBe(true);
		// Departure has astral coverage; a codepoint > U+FFFF can only come from
		// a format-12 subtable, so any such hit confirms the branch.
		expect([...cp].some((c) => c > 0xffff)).toBe(true);
	});

	test("throws on a truncated font rather than returning a partial set", () => {
		expect(() => cmapCodepoints(new Uint8Array([0, 1, 0, 0, 0, 1]))).toThrow();
	});
});

// ---------------------------------------------------------------------------
// stripComments / scanSource — comment awareness + line preservation.
// ---------------------------------------------------------------------------

describe("scanSource", () => {
	test("does not flag a non-ASCII char in a line comment", () => {
		expect(scanSource("apps/ui/src/a.ts", "const x = 1; // arrow ▸\n")).toEqual(
			[],
		);
	});

	test("does not flag a non-ASCII char in a block comment", () => {
		expect(
			scanSource("apps/ui/src/a.ts", "/* bracket ⟩ */\nconst x = 1;\n"),
		).toEqual([]);
	});

	test("flags a non-ASCII char in a string that contains //", () => {
		// The `//` here is inside a string literal, so it is NOT a comment; the
		// bracket after it is a rendered character and must be flagged.
		const found = scanSource("apps/ui/src/a.ts", 'const s = "http:// ⟩";\n');
		expect(found.map((f) => f.codepoint)).toEqual([0x27e9]);
	});

	test("reports the correct line AFTER stripping a multi-line block comment", () => {
		// A naive strip that DELETES comment lines would report the ■ on line 2,
		// not 4. The space-preserving strip keeps it on line 4.
		const text = ["/* a", "   b", "*/", 'const t = "■";', ""].join("\n");
		const found = scanSource("apps/ui/src/a.ts", text);
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(4);
		expect(found[0]?.codepoint).toBe(0x25a0);
	});

	test("column is 1-based within the (stripped) line", () => {
		const found = scanSource("apps/ui/src/a.ts", 'x="▸"\n');
		expect(found[0]?.column).toBe(4);
	});

	test("skips *.test.ts and *.test.tsx entirely", () => {
		expect(scanSource("apps/ui/src/a.test.ts", 'const s = "▸";\n')).toEqual([]);
		expect(scanSource("apps/ui/src/a.test.tsx", 'const s = "▸";\n')).toEqual(
			[],
		);
	});

	test("an escaped quote does not end a string, so // inside stays a string", () => {
		// The \" does not close the string; the // and the ▸ are still inside it.
		const found = scanSource("apps/ui/src/a.ts", 'const s = "a\\" // ▸";\n');
		expect(found.map((f) => f.codepoint)).toEqual([0x25b8]);
	});
});

// ---------------------------------------------------------------------------
// stripComments — direct: line count is invariant.
// ---------------------------------------------------------------------------

describe("stripComments", () => {
	test("preserves the line count of a block comment", () => {
		const text = "/* a\nb\nc */\nx";
		expect(stripComments(text).split("\n")).toHaveLength(4);
	});
});

// ---------------------------------------------------------------------------
// evaluate — filters covered, keeps uncovered.
// ---------------------------------------------------------------------------

describe("evaluate", () => {
	test("keeps only findings whose codepoint is absent from covered", () => {
		const findings: Finding[] = [
			{ path: "a.ts", line: 1, column: 1, char: "−", codepoint: 0x2212 },
			{ path: "a.ts", line: 2, column: 1, char: "⟩", codepoint: 0x27e9 },
		];
		const covered = new Set<number>([0x2212]);
		expect(evaluate(findings, covered).map((f) => f.codepoint)).toEqual([
			0x27e9,
		]);
	});
});

// ---------------------------------------------------------------------------
// resolveMode — default WARN; env override.
// ---------------------------------------------------------------------------

describe("resolveMode", () => {
	test("defaults to warn", () => {
		expect(resolveMode({})).toBe("warn");
	});

	test("FONT_COVERAGE_GATE=error selects error", () => {
		expect(resolveMode({ FONT_COVERAGE_GATE: "error" })).toBe("error");
	});

	test("any other value stays warn", () => {
		expect(resolveMode({ FONT_COVERAGE_GATE: "loud" })).toBe("warn");
	});
});
