// font-coverage-gate (RIG-3603 T8 precondition) — prove no rendered UI site
// uses a character absent from the branded font faces.
//
// WHY this exists: the e2e chromium env (tools/toolchain/chromium-e2e-env.nix,
// fontDirs) pins Space Mono + Departure Mono (the two branded faces) plus
// Unifont/unifont_upper purely as a COVERAGE FALLBACK, so an uncovered glyph
// renders a real (dual-width, off-grid) shape instead of baking tofu into a
// visual baseline. RIG-3742 / T8 removes the Unifont pin. That is only safe
// once nothing renders a character the branded faces lack — a claim that was an
// eyeball pass over a hand-written census, proven wrong twice. This gate makes
// it checkable: it parses the real font cmaps and scans the rendered UI source.
//
// Body text resolves --rigel-mono (Space Mono); --rigel-display (Departure
// Mono) is used by exactly one CSS rule. So the effective coverage a rendered
// character must satisfy is Space Mono's cmap; Departure is loaded, counted,
// and plausibility-checked too (it proves the format-12 path and guards the
// future display-face work), but Space Mono is the covered set findings clear.
//
// Adoption posture (RIG-3742 / T8): WARN until T8, then ERROR. There are 8
// known-uncovered rendered characters TODAY, so a fail-closed gate would be red
// on arrival. In WARN it prints findings and exits 0; in ERROR it exits 1 on
// any finding. Mode is FONT_COVERAGE_GATE=warn|error (default warn). THE FLIP:
// at T8 (RIG-3742), change the default below from "warn" to "error" so an
// uncovered rendered glyph fails the build.
//
// KNOWN BLIND SPOTS — the T8 flip assumes these stay absent, so widening the
// gate is cheaper than discovering one after it is fail-closed:
//   * Only LITERAL non-ASCII bytes are seen. A "\u25b8" escape, a &#9656;
//     entity, or String.fromCodePoint renders the glyph invisibly to the scan.
//     The tree writes literal glyphs throughout, which is what makes this safe.
//   * Only UI_SRC_DIR is scanned; apps/ui/e2e authors baselines too.
//   * A .ts file containing JSX is parsed as non-JSX, so JSX text in it is not
//     scanned. The tree keeps JSX in .tsx, which is what makes this safe.
//
// Inputs (env):
//   GATE_ROOT            - directory to scan (default: git toplevel).
//   FONT_COVERAGE_GATE   - "warn" (default) or "error".
// Exit codes:
//   0 - no findings, OR findings in WARN mode (printed, non-blocking)
//   1 - one or more findings in ERROR mode
//   2 - usage / internal error (font missing, truncated, or implausibly small)

import { $ } from "bun";
import ts from "typescript";

/** The UI source root whose rendered characters the gate governs. */
export const UI_SRC_DIR = "apps/ui/src";
/** Where the branded font faces live, repo-relative. */
export const FONTS_DIR = "apps/eng-docs/public/fonts";
/** The primary branded face: body text resolves --rigel-mono (Space Mono). */
export const SPACE_MONO_REL = `${FONTS_DIR}/SpaceMono-Regular.ttf`;
/** The display face (--rigel-display); its cmap exercises the format-12 path. */
export const DEPARTURE_REL = `${FONTS_DIR}/DepartureMono-Regular.otf`;

/** A face parsing to fewer codepoints than this is treated as a false read. */
export const MIN_PLAUSIBLE_CODEPOINTS = 100;

/** The gate's blocking posture. WARN prints but exits 0; ERROR exits 1. */
export type Mode = "warn" | "error";

/** A non-ASCII rendered character, located by file + 1-based line/column. */
export interface Finding {
	path: string;
	line: number;
	/** 1-based UTF-16 code-unit offset within the line. */
	column: number;
	char: string;
	codepoint: number;
}

// ---------------------------------------------------------------------------
// cmap parser (pure, exported).
// ---------------------------------------------------------------------------

/**
 * Union the codepoint coverage of a font's cmap subtables. Parses the sfnt
 * table directory (TrueType 0x00010000 / 'true', or CFF 'OTTO') and reads the
 * `cmap` table, supporting subtable format 4 (BMP) and format 12 (astral);
 * other formats are ignored, not fatal. A malformed/truncated file throws — a
 * partial set would silently read as "uncovered" and green a broken gate.
 */
export function cmapCodepoints(bytes: Uint8Array): Set<number> {
	const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
	const need = (end: number, what: string): void => {
		if (end > dv.byteLength) {
			throw new Error(`cmap: truncated font — ${what} runs past end of file`);
		}
	};
	const u16 = (o: number): number => {
		need(o + 2, "uint16");
		return dv.getUint16(o);
	};
	const i16 = (o: number): number => {
		need(o + 2, "int16");
		return dv.getInt16(o);
	};
	const u32 = (o: number): number => {
		need(o + 4, "uint32");
		return dv.getUint32(o);
	};

	need(12, "sfnt header");
	const numTables = u16(4);
	let cmapOffset = -1;
	for (let t = 0; t < numTables; t++) {
		const rec = 12 + t * 16;
		need(rec + 16, "table record");
		const tag = String.fromCharCode(
			dv.getUint8(rec),
			dv.getUint8(rec + 1),
			dv.getUint8(rec + 2),
			dv.getUint8(rec + 3),
		);
		if (tag === "cmap") {
			cmapOffset = u32(rec + 8);
			break;
		}
	}
	if (cmapOffset < 0) throw new Error("cmap: no cmap table in font");

	const covered = new Set<number>();
	const numSub = u16(cmapOffset + 2);
	for (let s = 0; s < numSub; s++) {
		const rec = cmapOffset + 4 + s * 8;
		const subOffset = cmapOffset + u32(rec + 4);
		const format = u16(subOffset);
		if (format === 4) readFormat4(subOffset, covered, u16, i16);
		else if (format === 12) readFormat12(subOffset, covered, u32);
		// Any other format: ignore, not fatal.
	}
	return covered;
}

/** Format 4: segment-mapped BMP coverage. A codepoint is covered iff its glyph id is non-zero. */
function readFormat4(
	base: number,
	out: Set<number>,
	u16: (o: number) => number,
	i16: (o: number) => number,
): void {
	const segCount = u16(base + 6) / 2;
	const endBase = base + 14;
	const startBase = endBase + segCount * 2 + 2; // +2 reservedPad
	const deltaBase = startBase + segCount * 2;
	const rangeBase = deltaBase + segCount * 2;
	for (let i = 0; i < segCount; i++) {
		const end = u16(endBase + i * 2);
		const start = u16(startBase + i * 2);
		const delta = i16(deltaBase + i * 2);
		const rangeOffset = u16(rangeBase + i * 2);
		if (start > end) continue; // the 0xffff..0xffff terminator degenerates here
		for (let c = start; c <= end && c !== 0xffff; c++) {
			let glyph: number;
			if (rangeOffset === 0) {
				glyph = (c + delta) & 0xffff;
			} else {
				// idRangeOffset indexes into glyphIdArray, measured from the
				// rangeOffset slot itself (the classic TrueType pointer trick).
				const gaddr = rangeBase + i * 2 + rangeOffset + (c - start) * 2;
				glyph = u16(gaddr);
				if (glyph !== 0) glyph = (glyph + delta) & 0xffff;
			}
			if (glyph !== 0) out.add(c);
		}
	}
}

/** Format 12: segmented coverage, full Unicode range (Departure Mono needs this). */
function readFormat12(
	base: number,
	out: Set<number>,
	u32: (o: number) => number,
): void {
	const numGroups = u32(base + 12);
	const groupBase = base + 16;
	for (let g = 0; g < numGroups; g++) {
		const rec = groupBase + g * 12;
		const start = u32(rec);
		const end = u32(rec + 4);
		if (start > end) continue;
		for (let c = start; c <= end; c++) out.add(c);
	}
}

// ---------------------------------------------------------------------------
// Source scanner (pure, exported).
// ---------------------------------------------------------------------------

/**
 * Find every non-ASCII character in a rendered position in one UI source file.
 * The text is parsed by the TypeScript compiler and only leaf TOKENS are read,
 * so comment bodies are excluded as trivia and JSX text, regex literals, and
 * template spans are located as the parser tokenizes them. JSDoc subtrees are
 * skipped. Test files (*.test.ts / *.test.tsx) are skipped entirely.
 */
export function scanSource(relPath: string, text: string): Finding[] {
	if (relPath.endsWith(".test.ts") || relPath.endsWith(".test.tsx")) return [];
	const sourceFile = ts.createSourceFile(
		relPath,
		text,
		ts.ScriptTarget.Latest,
		// No parent pointers: the walk only descends.
		false,
		relPath.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
	);
	const findings: Finding[] = [];
	collectFindings(sourceFile, sourceFile, relPath, findings);
	return findings;
}

/**
 * Recurse to leaf tokens, appending one Finding per non-ASCII codepoint in a
 * leaf's text. Iteration is by codepoint so an astral character is one finding,
 * not two surrogate halves.
 */
function collectFindings(
	node: ts.Node,
	sourceFile: ts.SourceFile,
	relPath: string,
	out: Finding[],
): void {
	// JSDoc is the one comment form the parser surfaces as real nodes rather
	// than trivia, so its subtree is skipped: a doc comment renders nothing.
	if (isJSDocNode(node)) return;
	const children = node.getChildren(sourceFile);
	if (children.length > 0) {
		for (const child of children) {
			collectFindings(child, sourceFile, relPath, out);
		}
		return;
	}
	const start = node.getStart(sourceFile);
	const leaf = node.getText(sourceFile);
	for (let offset = 0; offset < leaf.length; ) {
		const codepoint = leaf.codePointAt(offset);
		if (codepoint === undefined) break;
		const char = String.fromCodePoint(codepoint);
		if (codepoint > 127) {
			const at = sourceFile.getLineAndCharacterOfPosition(start + offset);
			out.push({
				path: relPath,
				line: at.line + 1,
				column: at.character + 1,
				char,
				codepoint,
			});
		}
		offset += char.length;
	}
}

/** True for a JSDoc node or anything inside one (tags, type expressions). */
function isJSDocNode(node: ts.Node): boolean {
	return (
		node.kind >= ts.SyntaxKind.FirstJSDocNode &&
		node.kind <= ts.SyntaxKind.LastJSDocNode
	);
}

// ---------------------------------------------------------------------------
// Evaluate core (pure, exported).
// ---------------------------------------------------------------------------

/** Keep only the findings whose codepoint is absent from the covered set. */
export function evaluate(findings: Finding[], covered: Set<number>): Finding[] {
	return findings.filter((f) => !covered.has(f.codepoint));
}

/** Resolve the blocking posture from env (default WARN). */
export function resolveMode(env: Record<string, string | undefined>): Mode {
	return (env.FONT_COVERAGE_GATE ?? "").toLowerCase() === "error"
		? "error"
		: "warn";
}

/** Render a codepoint as U+XXXX (at least 4 hex digits, uppercase). */
export function formatCodepoint(cp: number): string {
	return `U+${cp.toString(16).toUpperCase().padStart(4, "0")}`;
}

// ---------------------------------------------------------------------------
// I/O wiring.
// ---------------------------------------------------------------------------

if (import.meta.main) {
	const root =
		process.env.GATE_ROOT ??
		(await $`git rev-parse --show-toplevel`.nothrow().quiet().text()).trim();
	const mode = resolveMode(process.env);

	const load = async (rel: string): Promise<Set<number>> => {
		const file = Bun.file(`${root}/${rel}`);
		if (!(await file.exists())) {
			console.error(`font-coverage-gate: font not found: ${rel}`);
			process.exit(2);
		}
		return cmapCodepoints(new Uint8Array(await file.arrayBuffer()));
	};

	let spaceMono: Set<number>;
	let departure: Set<number>;
	try {
		spaceMono = await load(SPACE_MONO_REL);
		departure = await load(DEPARTURE_REL);
	} catch (error) {
		console.error("font-coverage-gate: cannot parse a font face:");
		console.error(error instanceof Error ? error.message : String(error));
		process.exit(2);
	}

	// A zero-finding run that loaded zero codepoints is a false green: a face
	// parsing to an implausibly small set means the parser silently failed, so
	// fail loudly in BOTH modes rather than clear every character.
	if (
		spaceMono.size < MIN_PLAUSIBLE_CODEPOINTS ||
		departure.size < MIN_PLAUSIBLE_CODEPOINTS
	) {
		console.error(
			`font-coverage-gate: implausible cmap — Space Mono ${spaceMono.size}, Departure ${departure.size} (< ${MIN_PLAUSIBLE_CODEPOINTS}); the parser likely failed.`,
		);
		process.exit(2);
	}

	const findings: Finding[] = [];
	const glob = new Bun.Glob(`${UI_SRC_DIR}/**/*.{ts,tsx}`);
	for await (const rel of glob.scan({ cwd: root })) {
		const posix = rel.replaceAll("\\", "/");
		const text = await Bun.file(`${root}/${posix}`).text();
		findings.push(...scanSource(posix, text));
	}

	// Body text resolves Space Mono, so a rendered character must be in its cmap.
	const uncovered = evaluate(findings, spaceMono).sort(
		(a, b) =>
			a.path.localeCompare(b.path) || a.line - b.line || a.column - b.column,
	);

	for (const f of uncovered) {
		console.log(
			`${f.path}:${f.line}:${f.column}  ${formatCodepoint(f.codepoint)}  ${f.char}`,
		);
	}
	console.log(
		`font-coverage-gate: ${uncovered.length} uncovered rendered char(s) [${mode.toUpperCase()} mode]; Space Mono ${spaceMono.size} cp, Departure ${departure.size} cp.`,
	);

	if (mode === "error" && uncovered.length > 0) process.exit(1);
	if (uncovered.length > 0) {
		console.log(
			"font-coverage-gate: WARN mode — reported, not blocking. Flip default to ERROR at T8 (RIG-3742).",
		);
	}
	process.exit(0);
}
