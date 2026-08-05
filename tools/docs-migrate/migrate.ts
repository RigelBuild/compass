/**
 * One-shot sanitization migration for the Compass engineering-docs corpus
 * (SEA-1766, T4 of the compass-eng-docs migration).
 *
 * When a design record moves from the private `sealed` repo into this PUBLIC
 * repo it carries four artifacts of its private origin. This tool strips them,
 * per the four-class policy frozen in
 * `docs/designs/repo/compass-eng-docs/design.md` (Approach (c)):
 *
 *   1. linear.app tracker links (both inline and reference-definition forms)
 *      → bare `SEA-<n>` provenance IDs. A dead private URL is worse than none.
 *   2. `oss/compass/` path prefixes from sealed's vendored era → stripped; the
 *      public tree is the same layout without the prefix.
 *   3. Links to private seal-the-product records (`seal-*.md`) → plain prose,
 *      keeping the anchor text and dropping the dead link.
 *   4. Security sections (threat-model / security-boundary / egress) are kept
 *      VERBATIM — never edited (Matt's explicit ruling).
 *
 * All four transforms are single-line and line-local (none span lines), so the
 * core is a pure walk over lines with a "am I inside a protected section" bit
 * plus a "am I inside a fenced code block" bit. Fenced code is emitted verbatim
 * (a `#`/`##` comment inside a ```bash fence must NOT be parsed as a heading —
 * doing so would silently close a protected section or open a false one).
 * A post-run grep gate is authoritative over the regexes: a migrated corpus
 * must contain zero `linear.app`, zero `oss/compass`, and zero surviving
 * `seal-*.md` links OUTSIDE protected sections (a security section that
 * legitimately cites one of these is kept verbatim and exempt from the gate).
 *
 * This tool is deleted at the end of the corpus migration (T5); it is not a
 * standing check.
 */

/** A single line-level rewrite recorded for per-file review. */
export interface LineChange {
	/** 1-based line number in the source. */
	line: number;
	before: string;
	/** The rewritten line, or null when the line was dropped entirely. */
	after: string | null;
}

export interface SanitizeResult {
	output: string;
	changes: LineChange[];
}

export interface GateResult {
	ok: boolean;
	residue: string[];
}

/** A linear.app reference-definition line, e.g. `[SEA-1]: https://linear.app/…`. */
const LINEAR_REF_DEF = /^\[[^\]]+\]:\s*https:\/\/linear\.app\//;
/**
 * An inline linear link on a SEA id: `[SEA-1](https://linear.app/…)`.
 *
 * Known cosmetic limit: `[^)]*` stops at the first `)`, so a linear URL that
 * itself contains a literal `)` truncates the strip mid-URL. Left as-is —
 * such URLs do not occur in the corpus and the gate catches any residue.
 */
const LINEAR_INLINE = /\[(SEA-\d+)\]\(https:\/\/linear\.app\/[^)]*\)/g;
/**
 * A shortcut/collapsed reference usage of a SEA id: `[SEA-1]` used as prose.
 * The negative lookahead excludes three non-shortcut usages that must NOT be
 * corrupted into a bare id: an inline link `[SEA-1](…)`, a reference usage
 * `[SEA-1][ref]`/`[SEA-1][]`, and a line-defining `[SEA-1]: …` ref-def.
 */
const LINEAR_SHORTCUT = /\[(SEA-\d+)\](?![([:])/g;
/**
 * A link whose target is a private seal-the-product record. Tolerates an
 * optional `./` prefix, a `#anchor`, and a trailing `"title"` attribute.
 */
const SEAL_LINK =
	/\[([^\]]+)\]\((?:\.\/)?seal-[^)\s]*\.md(?:#[^)\s]*)?(?:\s+"[^"]*")?\)/g;
/** A seal-record reference-definition line, e.g. `[rec]: seal-foo.md`. */
const SEAL_REF_DEF = /^\[[^\]]+\]:\s*(?:\.\/)?seal-[^\s)]*\.md/;
/**
 * Gate backstop for class 3: a surviving seal-record link (inline, with anchor
 * or title) OR a surviving seal ref-def line. Fail-OPEN here would leak a
 * private path, so this is symmetric with the linear/oss checks.
 */
const SEAL_RESIDUE =
	/\]\((?:\.\/)?seal-[^)\s]*\.md|^\[[^\]]+\]:\s*(?:\.\/)?seal-[^\s)]*\.md/m;
/** A markdown ATX heading: capture group 1 is the hashes, group 2 the text. */
const HEADING = /^(#{1,6})\s+(.*)$/;
/** A fenced-code delimiter: capture group 1 is the marker run (``` or ~~~). */
const FENCE = /^\s*(`{3,}|~{3,})/;

/**
 * Whether a heading names a security region whose body must be kept verbatim.
 */
export function isProtectedHeading(headingText: string): boolean {
	// Frozen class-4 vocabulary: threat-model, security (standalone word),
	// security-boundary, or egress. Word-anchored so it never matches inside
	// 'insecurity', and bare 'boundary' alone is NOT a protected keyword.
	return /threat[\s-]?model|\bsecurit(y|ies)\b|\begress\b/i.test(headingText);
}

/**
 * Applies the line-local rewrites to a single non-protected, non-fenced line.
 *
 * Returns null to DROP the line (a linear or seal reference-definition line),
 * otherwise the transformed line. Order matters: the ref-def drops are checked
 * first, then the inline and shortcut linear strips, the `oss/compass` strip,
 * and the seal-record de-link.
 */
export function transformLine(line: string): string | null {
	if (LINEAR_REF_DEF.test(line)) {
		return null;
	}
	if (SEAL_REF_DEF.test(line)) {
		return null;
	}
	let out = line;
	out = out.replace(LINEAR_INLINE, "$1");
	out = out.replace(LINEAR_SHORTCUT, "$1");
	// Strip the `oss/compass/` prefix; a bare `oss/compass` (no trailing path)
	// names the vendored root, which in this repo is `compass`. Order matters:
	// the slash form is stripped first so a path never collapses to `compass/…`.
	out = out.replaceAll("oss/compass/", "");
	// Anchor the bare form so a suffixed token (e.g. `oss/compass-tools`) is not
	// clipped. A negative lookahead (NOT `\b`, which fires before `-`) stops at a
	// following `-` or word char; the slash form is already gone (line 120).
	out = out.replace(/oss\/compass(?![-\w])/g, "compass");
	out = out.replace(SEAL_LINK, "$1");
	return out;
}

/** Per-line classification from the shared fence + protected-section walk. */
interface LineScan {
	line: string;
	/** 1-based line number. */
	lineNumber: number;
	/** True when the line is a fence delimiter or lies inside a fenced block. */
	inFence: boolean;
	/** True when the line is an ATX heading OUTSIDE any fence. */
	isHeading: boolean;
	/** True when the line lies within an active protected (security) section. */
	isProtected: boolean;
}

/**
 * Walks a markdown document once, tracking fenced-code state and
 * protected-section state, yielding a classification per line. The sanitizer
 * and the gate both consume this so they agree on where fences and protected
 * regions are: a `#` comment inside a fence is never mistaken for a heading,
 * and a fence closes only on a matching-or-longer run of the SAME marker char.
 */
function* scanLines(md: string): Generator<LineScan> {
	const lines = md.split("\n");
	// Null when outside a protected section; otherwise the heading level (1..6)
	// that opened the currently-active protected section.
	let protectedLevel: number | null = null;
	// The active opening fence marker run (e.g. "```" or "~~~~"), or null.
	let fenceMarker: string | null = null;

	for (let i = 0; i < lines.length; i++) {
		const line = lines[i] ?? "";
		const lineNumber = i + 1;
		const fence = FENCE.exec(line);
		const marker = fence?.[1] ?? "";

		if (fenceMarker !== null) {
			// Inside a fence: a matching-or-longer run of the SAME marker closes it.
			if (
				fence &&
				marker[0] === fenceMarker[0] &&
				marker.length >= fenceMarker.length
			) {
				fenceMarker = null;
			}
			yield {
				line,
				lineNumber,
				inFence: true,
				isHeading: false,
				isProtected: protectedLevel !== null,
			};
			continue;
		}

		if (fence) {
			// Outside a fence: this delimiter opens one.
			fenceMarker = marker;
			yield {
				line,
				lineNumber,
				inFence: true,
				isHeading: false,
				isProtected: protectedLevel !== null,
			};
			continue;
		}

		const heading = HEADING.exec(line);
		if (heading) {
			const level = heading[1]?.length ?? 0;
			const text = heading[2] ?? "";
			// A heading at or above the protected section's level closes it.
			if (protectedLevel !== null && level <= protectedLevel) {
				protectedLevel = null;
			}
			// A fresh security heading opens a protected section.
			if (protectedLevel === null && isProtectedHeading(text)) {
				protectedLevel = level;
			}
			yield {
				line,
				lineNumber,
				inFence: false,
				isHeading: true,
				isProtected: protectedLevel !== null,
			};
			continue;
		}

		yield {
			line,
			lineNumber,
			inFence: false,
			isHeading: false,
			isProtected: protectedLevel !== null,
		};
	}
}

/**
 * Sanitizes a markdown document, returning the migrated text and the list of
 * per-line changes for review.
 */
export function sanitizeWithChanges(md: string): SanitizeResult {
	const hadTrailingNewline = md.endsWith("\n");
	const changes: LineChange[] = [];
	const survivors: string[] = [];

	for (const { line, lineNumber, isProtected } of scanLines(md)) {
		// Only protected-section bodies are kept verbatim; fenced code and
		// headings are IN transform scope (class 3: code spans included). This
		// makes the transform-exemption set match the gate's scan-exemption set.
		if (isProtected) {
			survivors.push(line);
			continue;
		}

		const transformed = transformLine(line);
		if (transformed === null) {
			changes.push({ line: lineNumber, before: line, after: null });
			continue;
		}
		if (transformed !== line) {
			changes.push({ line: lineNumber, before: line, after: transformed });
		}
		survivors.push(transformed);
	}

	let output = survivors.join("\n");
	if (hadTrailingNewline && !output.endsWith("\n")) {
		output += "\n";
	}
	return { output, changes };
}

/** Sanitizes a markdown document, returning the migrated text. */
export function sanitize(md: string): string {
	return sanitizeWithChanges(md).output;
}

/**
 * The authoritative post-run gate. Independent of the rewrite regexes: it fails
 * if any `linear.app`, `oss/compass`, or surviving `seal-*.md` link literal
 * remains in the output — EXCEPT inside a protected security section (class 4),
 * which is kept verbatim and therefore exempt (scanning it would wedge the run
 * on a literal the policy requires us to preserve). Uses the same fence-aware
 * walk as the sanitizer so a `#` comment inside a fence cannot mis-bound the
 * protected regions.
 */
export function gate(md: string): GateResult {
	const scanned: string[] = [];
	for (const { line, isProtected } of scanLines(md)) {
		if (!isProtected) {
			scanned.push(line);
		}
	}
	const body = scanned.join("\n");

	const residue: string[] = [];
	if (body.includes("linear.app")) {
		residue.push("linear.app");
	}
	// Anchored to match the transform (line 124): a suffixed token like
	// `oss/compass-tools` is deliberately preserved by the sanitizer, so the gate
	// must not flag it as residue, or the two-phase barrier deadlocks. A surviving
	// slash form (`/` is not `[-\w]`) or bare end-of-token `oss/compass` — both of
	// which the sanitizer WOULD have rewritten — are still caught as a real leak.
	if (/oss\/compass(?![-\w])/.test(body)) {
		residue.push("oss/compass");
	}
	if (SEAL_RESIDUE.test(body)) {
		residue.push("seal-*.md");
	}
	return { ok: residue.length === 0, residue };
}

/** Renders a compact per-file review summary of the changes. */
export function formatDiff(path: string, changes: LineChange[]): string {
	if (changes.length === 0) {
		return `${path}: no changes`;
	}
	const body = changes
		.map((c) => {
			if (c.after === null) {
				return `  L${c.line}: - ${c.before}\n         (removed)`;
			}
			return `  L${c.line}: - ${c.before}\n         + ${c.after}`;
		})
		.join("\n");
	const noun = changes.length === 1 ? "change" : "changes";
	return `${path}: ${changes.length} ${noun}\n${body}`;
}

if (import.meta.main) {
	const argv = process.argv.slice(2);
	const write = argv.includes("--write");
	const paths = argv.filter((a) => a !== "--write");

	if (paths.length === 0) {
		console.error(
			"usage: bun migrate.ts [--write] <file.md> [<file.md> ...]\n" +
				"  default is dry-run (prints per-file diff summaries; no files touched)",
		);
		process.exit(2);
	}

	// Two-phase: sanitize + gate EVERY file first, and only write when the whole
	// batch passes. The gate is authoritative, so a gate-failing file must never
	// be committed to disk; and an all-or-nothing write keeps a mid-batch failure
	// from leaving the corpus half-migrated.
	const planned: { path: string; input: string; output: string }[] = [];
	const allResidue: { path: string; residue: string[] }[] = [];

	for (const path of paths) {
		const input = await Bun.file(path).text();
		const { output, changes } = sanitizeWithChanges(input);
		console.log(formatDiff(path, changes));

		const g = gate(output);
		if (!g.ok) {
			allResidue.push({ path, residue: g.residue });
		}
		planned.push({ path, input, output });
	}

	if (allResidue.length > 0) {
		console.error("\ngate FAILED — residue remains after migration:");
		for (const { path, residue } of allResidue) {
			console.error(`  ${path}: ${residue.join(", ")}`);
		}
		console.error("\nNo files written (gate is authoritative).");
		process.exit(1);
	}

	if (write) {
		for (const { path, input, output } of planned) {
			if (output !== input) {
				await Bun.write(path, output);
			}
		}
	}

	console.log(`\ngate OK — ${paths.length} file(s), zero residue.`);
}
