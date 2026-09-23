// Unit tests for the font-coverage-gate's pure core and CLI contract (index.ts).
//
// This gate is the oracle that lets RIG-3742 retire the Unifont fallback pin,
// so this suite defends the machine-readable contract: the cmap parser reads
// the REAL font faces (format 4 for Space Mono, format 12 for Departure Mono's
// astral coverage), the source scanner ignores comments/tests but flags
// rendered characters, and the evaluate/mode cores behave. The font assertions
// (U+2212 present in Space Mono, U+27E9 absent, U+27E9 present in Departure)
// are measured facts, not guesses.

import { describe, expect, test } from "bun:test";
import { cp, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import {
	cmapCodepoints,
	evaluate,
	type Finding,
	resolveMode,
	scanSource,
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
		// Exact cardinality: this is the covered set every finding clears, so a
		// parser change that quietly adds or drops codepoints moves the gate's
		// verdict. Pinning the count makes that a test failure, not a silent
		// re-baseline. Update it only alongside a deliberate parser change.
		expect(cp.size).toBe(624);
	});

	test("Departure Mono (format 12 / CFF OTTO): covers U+27E9", async () => {
		const cp = cmapCodepoints(await bytes("DepartureMono-Regular.otf"));
		// U+27E9 is absent from Space Mono but present here — proving both the
		// OTTO/CFF sfnt path and the format-12 branch actually run.
		expect(cp.has(0x27e9)).toBe(true);
		// Departure has astral coverage; a codepoint > U+FFFF can only come from
		// a format-12 subtable, so any such hit confirms the branch.
		expect([...cp].some((c) => c > 0xffff)).toBe(true);
		// Exact cardinality, as above. Departure's cmap is duplicated across
		// four subtables (two format 4, two format 12), so this also pins that
		// the union de-duplicates rather than double-counting.
		expect(cp.size).toBe(1079);
	});

	test("throws on a truncated font rather than returning a partial set", () => {
		expect(() => cmapCodepoints(new Uint8Array([0, 1, 0, 0, 0, 1]))).toThrow();
	});
});

// ---------------------------------------------------------------------------
// cmapCodepoints — hostile cmap ranges (RIG-3742 T8 review finding).
//
// A font file is committed DATA and its range bounds are read straight out of
// it. Structure bounds how many range RECORDS a file can hold, but nothing
// bounds how wide each one CLAIMS to be — one 12-byte format-12 group saying
// 0..0xffffffff is 4.29 billion Set inserts out of a font that parses clean.
// These build such fonts synthetically and prove the parser rejects a
// pathological range instead of walking it.
// ---------------------------------------------------------------------------

type Range = readonly [start: number, end: number];

/** Build a format-4 subtable covering `segments`, every glyph id non-zero. */
const format4 = (segments: readonly Range[]): Uint8Array => {
	const segCount = segments.length;
	const body = new Uint8Array(14 + segCount * 8 + 2);
	const dv = new DataView(body.buffer);
	dv.setUint16(0, 4); // format
	dv.setUint16(2, body.length);
	dv.setUint16(6, segCount * 2); // segCountX2
	const endBase = 14;
	const startBase = endBase + segCount * 2 + 2; // +2 reservedPad
	const deltaBase = startBase + segCount * 2;
	for (const [i, [start, end]] of segments.entries()) {
		dv.setUint16(endBase + i * 2, end);
		dv.setUint16(startBase + i * 2, start);
		// idRangeOffset is left 0, so the glyph id is c + idDelta: never zero
		// below 0xffff, which is what makes each codepoint "covered".
		dv.setInt16(deltaBase + i * 2, 1);
	}
	return body;
};

/**
 * Build a format-12 subtable covering `groups`. `startGlyphId` is the glyph id
 * of each group's FIRST codepoint (ids ascend from there), so the default of 1
 * keeps every codepoint covered; pass 0 to make a group's first codepoint map
 * to .notdef.
 */
const format12 = (groups: readonly Range[], startGlyphId = 1): Uint8Array => {
	const body = new Uint8Array(16 + groups.length * 12);
	const dv = new DataView(body.buffer);
	dv.setUint16(0, 12); // format
	dv.setUint32(4, body.length);
	dv.setUint32(12, groups.length); // numGroups
	for (const [g, [start, end]] of groups.entries()) {
		const rec = 16 + g * 12;
		dv.setUint32(rec, start);
		dv.setUint32(rec + 4, end);
		dv.setUint32(rec + 8, startGlyphId);
	}
	return body;
};

/** Wrap cmap subtables in the smallest sfnt the parser will walk. */
const font = (
	subtables: readonly Uint8Array[],
	numGlyphs = 0xffff,
): Uint8Array => {
	const recordsSize = 2 * 16;
	const bodiesSize = subtables.reduce((n, s) => n + s.length, 0);
	const maxpOffset = 12 + recordsSize;
	const cmapOffset = maxpOffset + 6;
	const records = 4 + subtables.length * 8;
	const cmapSize = records + bodiesSize;
	const out = new Uint8Array(cmapOffset + cmapSize);
	const dv = new DataView(out.buffer);
	dv.setUint32(0, 0x00010000);
	dv.setUint16(4, 2);
	for (const [i, ch] of [..."maxp"].entries()) out[12 + i] = ch.charCodeAt(0);
	dv.setUint32(20, maxpOffset);
	dv.setUint32(24, 6);
	dv.setUint32(maxpOffset, 0x00010000);
	dv.setUint16(maxpOffset + 4, numGlyphs);
	for (const [i, ch] of [..."cmap"].entries()) out[28 + i] = ch.charCodeAt(0);
	dv.setUint32(36, cmapOffset);
	dv.setUint32(40, cmapSize);
	dv.setUint16(cmapOffset + 2, subtables.length);
	let bodyAt = records;
	for (const [i, body] of subtables.entries()) {
		const rec = cmapOffset + 4 + i * 8;
		dv.setUint16(rec, 3);
		dv.setUint16(rec + 2, 10);
		dv.setUint32(rec + 4, bodyAt);
		out.set(body, cmapOffset + bodyAt);
		bodyAt += body.length;
	}
	return out;
};

describe("cmapCodepoints — hostile cmap ranges", () => {
	test("format 12 group past U+10FFFF throws instead of walking it", () => {
		// Walking that group would be 4.29e9 Set inserts; the throw is what
		// proves the range was rejected rather than expanded.
		expect(() => cmapCodepoints(font([format12([[0, 0xffffffff]])]))).toThrow(
			/runs past U\+10FFFF/,
		);
	});
	test("bounds cmap encoding records to cmap table length", () => {
		const data = font([
			format4([
				[0x41, 0x41],
				[0xffff, 0xffff],
			]),
		]);
		new DataView(data.buffer).setUint32(40, 4); // header only; record bytes remain in file
		expect(() => cmapCodepoints(data)).toThrow(/cmap end/);
	});

	test("rejects subtable offsets outside cmap table", () => {
		const data = font([
			format4([
				[0x41, 0x41],
				[0xffff, 0xffff],
			]),
		]);
		const dv = new DataView(data.buffer);
		dv.setUint32(40, 12); // header + one encoding record, no subtable body
		expect(() => cmapCodepoints(data)).toThrow(/cmap end/);
	});

	test("bounds unsupported subtable format reads to cmap end", () => {
		const data = font([new Uint8Array([99, 99, 99, 99])]);
		const dv = new DataView(data.buffer);
		dv.setUint32(40, 12); // format bytes lie beyond declared cmap table
		expect(() => cmapCodepoints(data)).toThrow(/cmap end/);
	});

	test("rejects a format-4 subtable that reads beyond its declared length", () => {
		const subtable = format4([
			[0x41, 0x41],
			[0xffff, 0xffff],
		]);
		new DataView(subtable.buffer).setUint16(2, 16); // segment data lies after declaration
		expect(() => cmapCodepoints(font([subtable]))).toThrow(/declared length/);
	});

	test("rejects format-12 glyph IDs that overflow uint32", () => {
		expect(() =>
			cmapCodepoints(font([format12([[0x41, 0x42]], 0xffffffff)])),
		).toThrow(/glyph id overflows uint32/);
	});

	test("format 12 groups summing past the expansion budget throw", () => {
		// Each group is individually legal; repeating a maximal one buys no new
		// coverage and costs a full codespace walk apiece. Five is past budget.
		const maximal: Range = [0, 0x10ffff];
		const groups = [maximal, maximal, maximal, maximal, maximal];
		expect(() => cmapCodepoints(font([format12(groups)]))).toThrow(
			/expansion budget/,
		);
	});

	test("format 4 segments summing past the expansion budget throw", () => {
		// 100 maximal BMP segments is 6.55M codepoints of walking out of an
		// 816-byte table: cheap to commit, expensive to parse. Format 4's bounds
		// are uint16 so they cannot exceed U+10FFFF — the budget is the guard.
		const segments: Range[] = Array.from({ length: 100 }, () => [0, 0xfffe]);
		expect(() => cmapCodepoints(font([format4(segments)]))).toThrow(
			/expansion budget/,
		);
	});

	test("the budget is font-wide: duplicated subtables cannot each spend it", () => {
		// Each subtable is individually in budget (3 and 2 maximal codespaces,
		// against a 4-codespace ceiling), so this throws ONLY if the budget
		// spans subtables. A budget reset per subtable would let a font repeat
		// an in-budget subtable without limit — the cheapest way to restore the
		// unbounded walk the budget exists to stop.
		const maximal: Range = [0, 0x10ffff];
		expect(() =>
			cmapCodepoints(
				font([
					format12([maximal, maximal, maximal]),
					format12([maximal, maximal]),
				]),
			),
		).toThrow(/expansion budget/);
	});

	test("format 12 startGlyphId 0 leaves the group's first codepoint uncovered", () => {
		// Glyph 0 is .notdef: format 4 already treats it as uncovered, and a
		// format-12 group's ids ascend from startGlyphId, so a group starting
		// at 0 maps its FIRST codepoint to .notdef and the rest to real glyphs.
		// Counting that first codepoint as covered would clear a rendered
		// character that actually renders tofu — a false green in the gate's
		// one direction that matters.
		const covered = cmapCodepoints(font([format12([[0x2000, 0x2002]], 0)]));
		expect([...covered].sort((a, b) => a - b)).toEqual([0x2001, 0x2002]);
	});

	test("the widest legal group (0..U+10FFFF) is still accepted", () => {
		// The bound is inclusive of U+10FFFF: an off-by-one here would reject a
		// legitimate pan-Unicode face and fail the gate closed on a good font.
		expect(cmapCodepoints(font([format12([[0, 0x10ffff]])])).size).toBe(
			0xffff - 1,
		);
	});

	test("an in-budget font parses both formats to exact coverage", () => {
		const covered = cmapCodepoints(
			font([
				format4([
					[0x41, 0x43],
					[0xffff, 0xffff], // the required terminator segment
				]),
				format12([[0x1f600, 0x1f601]]),
			]),
		);
		expect([...covered].sort((a, b) => a - b)).toEqual([
			0x41, 0x42, 0x43, 0x1f600, 0x1f601,
		]);
	});
});

// ---------------------------------------------------------------------------
// scanSource — token awareness: comments excluded, rendered chars located.
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

	test("reports the correct line after a multi-line block comment", () => {
		// Positions come from the source file's own line map, so the ■ is on
		// line 4 — not line 2, as a scan of comment-free text would report.
		const text = ["/* a", "   b", "*/", 'const t = "■";', ""].join("\n");
		const found = scanSource("apps/ui/src/a.ts", text);
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(4);
		expect(found[0]?.codepoint).toBe(0x25a0);
	});

	test("column is 1-based within the line", () => {
		const found = scanSource("apps/ui/src/a.ts", 'x="▸"\n');
		expect(found[0]?.column).toBe(4);
	});

	test("skips *.test.ts and *.test.tsx entirely", () => {
		expect(scanSource("apps/ui/src/a.test.ts", 'const s = "▸";\n')).toEqual([]);
		expect(scanSource("apps/ui/src/a.test.tsx", 'const s = "▸";\n')).toEqual(
			[],
		);
	});

	test("a // inside a string literal is not a comment", () => {
		const found = scanSource("apps/ui/src/a.ts", 'const s = "a\\" // ▸";\n');
		expect(found.map((f) => f.codepoint)).toEqual([0x25b8]);
	});

	test("locates a glyph inside a multi-line template literal", () => {
		const found = scanSource(
			"apps/ui/src/a.ts",
			"const s = `intro\n  body ▸ tail`;\n",
		);
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(2);
		expect(found[0]?.column).toBe(8);
	});

	test("a leading comment does not shift the reported column", () => {
		const found = scanSource("apps/ui/src/a.ts", '// note\nconst s = "▸";\n');
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(2);
		expect(found[0]?.column).toBe(12);
	});

	test("locates a glyph in multi-line JSX text", () => {
		const text = "const e = (\n  <p>\n    go ▸\n  </p>\n);\n";
		const found = scanSource("apps/ui/src/a.tsx", text);
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(3);
		expect(found[0]?.column).toBe(8);
	});

	test("an apostrophe in JSX text does not hide a glyph on a later line", () => {
		// An apostrophe in JSX text is prose, not a string delimiter, so it must
		// not affect how later lines are scanned.
		const text = [
			"<p>Matt's row</p>;",
			"<a href={'https://x.dev'}>go ▸</a>;",
			"",
		].join("\n");
		const found = scanSource("apps/ui/src/a.tsx", text);
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(2);
		expect(found[0]?.codepoint).toBe(0x25b8);
	});

	test("an apostrophe in JSX text does not turn a later doc comment into a finding", () => {
		// A doc comment renders nothing, so its em-dash is not a finding.
		const text = [
			"<p>Matt's row</p>;",
			"/** A pane — see notes. */",
			"const x = 1;",
			"",
		].join("\n");
		expect(scanSource("apps/ui/src/a.tsx", text)).toEqual([]);
	});
	test("a // inside a regex literal does not blank the rest of the line", () => {
		// The `//` is regex syntax, not a comment, so the ▸ after it is rendered.
		const found = scanSource(
			"apps/ui/src/a.ts",
			'const re = /https?:\\/\\//; const s = "▸";\n',
		);
		expect(found).toHaveLength(1);
		expect(found[0]?.line).toBe(1);
		expect(found[0]?.codepoint).toBe(0x25b8);
	});

	test("an astral character is one finding, not two surrogate halves", () => {
		const found = scanSource("apps/ui/src/a.ts", 'const s = "𝄞 ▸";\n');
		expect(found.map((f) => f.codepoint)).toEqual([0x1d11e, 0x25b8]);
		expect(found[1]?.column).toBe(15);
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

// resolveMode — default ERROR; explicit WARN override; invalid values fail closed.
// ---------------------------------------------------------------------------

describe("resolveMode", () => {
	test("unset defaults to error", () => {
		expect(resolveMode({})).toBe("error");
	});

	test("explicit error selects error", () => {
		expect(resolveMode({ FONT_COVERAGE_GATE: "error" })).toBe("error");
	});

	test("explicit warn selects warn case-insensitively", () => {
		expect(resolveMode({ FONT_COVERAGE_GATE: "WaRn" })).toBe("warn");
	});

	test.each(["", "loud", " WARN "])(
		"invalid value %j fails closed to error",
		(value) => {
			expect(resolveMode({ FONT_COVERAGE_GATE: value })).toBe("error");
		},
	);
});

// ---------------------------------------------------------------------------
// CLI process contract — the gate as CI actually invokes it.
//
// Each case runs the real CLI as a subprocess against an isolated temp root
// holding one UI source file and copies of the real font faces, so the exit
// code and the printed report are observed, not inferred from the pure core.
// ---------------------------------------------------------------------------

/** Run the CLI over a temp root containing `source` as the only UI file. */
const runGate = async (
	source: string,
	mode: string | undefined,
): Promise<{ exitCode: number | null; output: string }> => {
	const root = await mkdtemp(join(tmpdir(), "font-coverage-gate-"));
	try {
		await mkdir(join(root, "apps/ui/src"), { recursive: true });
		await mkdir(join(root, "apps/eng-docs/public/fonts"), { recursive: true });
		await writeFile(join(root, "apps/ui/src/index.ts"), source);
		await Promise.all(
			["SpaceMono-Regular.ttf", "DepartureMono-Regular.otf"].map((name) =>
				cp(join(FONTS, name), join(root, "apps/eng-docs/public/fonts", name)),
			),
		);

		const proc = Bun.spawn(
			[process.execPath, resolve(import.meta.dir, "index.ts")],
			{
				env: {
					...process.env,
					GATE_ROOT: root,
					...(mode === undefined ? {} : { FONT_COVERAGE_GATE: mode }),
				},
				stdout: "pipe",
				stderr: "pipe",
			},
		);
		const [stdout, stderr] = await Promise.all([
			new Response(proc.stdout).text(),
			new Response(proc.stderr).text(),
		]);
		await proc.exited;
		return { exitCode: proc.exitCode, output: `${stdout}\n${stderr}` };
	} finally {
		await rm(root, { recursive: true, force: true });
	}
};

// U+27E9 is absent from Space Mono (the covered set findings clear), so a
// source containing it is one uncovered rendered character.
const UNCOVERED_SOURCE = 'const rendered = "⟩";\n';

describe("CLI process contract", () => {
	test("invalid mode fails closed on an isolated fixture", async () => {
		const { exitCode, output } = await runGate(UNCOVERED_SOURCE, "invalid");
		expect(exitCode).toBe(1);
		expect(output).toContain("[ERROR mode]");
		expect(output).toContain("1 uncovered rendered char(s)");
	});

	test("explicit WARN reports an uncovered char and still exits 0", async () => {
		// WARN is the non-blocking opt-in: the same fixture that exits 1 under
		// ERROR must print the finding and exit 0 here, or the opt-in either
		// blocks CI or hides what it was asked to report.
		const { exitCode, output } = await runGate(UNCOVERED_SOURCE, "warn");
		expect(exitCode).toBe(0);
		expect(output).toContain("[WARN mode]");
		expect(output).toContain("1 uncovered rendered char(s)");
		expect(output).toContain("U+27E9");
		expect(output).toContain("not blocking");
	});

	test("a fully covered fixture exits 0 with zero findings in ERROR mode", async () => {
		// The clean control for the fail-closed default: without it, every
		// exit-1 case above is also satisfied by a gate that flags everything.
		// U+2212 is in Space Mono's cmap, so it is rendered AND covered.
		const { exitCode, output } = await runGate(
			'const rendered = "a − b";\n',
			undefined,
		);
		expect(exitCode).toBe(0);
		expect(output).toContain("0 uncovered rendered char(s)");
		expect(output).toContain("[ERROR mode]");
		expect(output).not.toContain("not blocking");
	});
});
