// Only semantic and scale --cx-* tokens may appear outside tokens.css.
// The mark file alone may use the reserved --rigel-purple accent directly.
// WARN reports findings without failing; ERROR makes them blocking.

import { $ } from "bun";

/** The UI source root whose CSS the gate governs. */
export const UI_SRC_DIR = "apps/ui/src";
/** The single file allowed to hold --rigel-* and raw values; never scanned. */
export const TOKENS_REL = "apps/ui/src/design/tokens.css";

/** The categories a finding can fall into. */
export type Kind = "hex" | "rigel" | "duration" | "easing";

/** A single consumption-rule violation, located by file + 1-based line. */
export interface Finding {
	file: string;
	line: number;
	kind: Kind;
	/** The offending token text (e.g. "#45505f", "--rigel-blue", "140ms"). */
	match: string;
	message: string;
}

/** The gate's blocking posture. WARN prints but exits 0; ERROR exits 1. */
export type Mode = "warn" | "error";

// ---------------------------------------------------------------------------
// Pure detectors (exported for unit tests). Each returns its raw match text.
// ---------------------------------------------------------------------------

/**
 * Raw hex colour literals: #rgb, #rgba, #rrggbb, #rrggbbaa.
 *
 * Scope is deliberately hex-only: the frozen record's consumption rule
 * (D2:295-301, D9) enumerates "raw hex" for colours, so a raw rgb()/hsl()/
 * named colour is out of this gate's contract. Widening the colour ban is a
 * scope decision that belongs to the record, not this detector.
 */
const HEX_RE = /#[0-9a-fA-F]{3,8}\b/g;
/** Any primitive-tier reference. Captures the full custom-property name. */
const RIGEL_RE = /--rigel-[a-z0-9-]+/g;
/**
 * Literal time values: 140ms, 12ms, 1.6s, 320ms. Case-insensitive because CSS
 * units are (`140MS` is a valid duration). The bare pattern can match a
 * numeric-prefixed time token inside a `content:` string; that edge is rare,
 * WARN-gated, and left as a known false positive rather than string-stripped.
 */
const DURATION_RE = /\b\d+(?:\.\d+)?m?s\b/gi;
/**
 * Easing literals: the cubic-bezier() function and the named timing keywords.
 * The `(?<!-)` / `(?!-)` guards keep compound identifiers (`--cx-ease-out`,
 * `linear-gradient(`, `ease-…`) from tripping the bare-keyword match — only a
 * keyword that is not part of a longer hyphenated token is a timing literal.
 * Case-insensitive because CSS keywords are (`EASE-OUT` is valid).
 */
const CUBIC_RE = /cubic-bezier\(/gi;
const EASE_KEYWORD_RE =
	/(?<!-)\b(?:ease-in-out|ease-out|ease-in|ease|linear)\b(?!-)/gi;

/**
 * The mark-component allowlist: in a file whose basename matches `mark*.css`,
 * --rigel-purple (and -hi / -lit) is sanctioned. Everything else is not.
 */
export function isMarkFile(relPath: string): boolean {
	const base = relPath.split("/").pop() ?? relPath;
	return /^mark.*\.css$/.test(base);
}

const ALLOWED_MARK_RIGEL: Record<string, true> = {
	"--rigel-purple": true,
	"--rigel-purple-hi": true,
	"--rigel-purple-lit": true,
};

/**
 * Scan one CSS file's text for consumption-rule violations. Pure: no I/O, no
 * exit — returns the findings so tests can assert them directly. `relPath`
 * selects the mark allowlist; it is NOT read from disk here.
 */
export function scanCss(relPath: string, text: string): Finding[] {
	const findings: Finding[] = [];
	const markAllowed = isMarkFile(relPath);
	const lines = text.split("\n");

	for (let i = 0; i < lines.length; i++) {
		const raw = lines[i] ?? "";
		// Comments are scanned too, so copied banned literals cannot evade the gate;
		// provenance comments may therefore be reported.
		const lineNo = i + 1;

		for (const m of raw.matchAll(HEX_RE)) {
			findings.push({
				file: relPath,
				line: lineNo,
				kind: "hex",
				match: m[0],
				message: `raw hex colour ${m[0]} — use a --cx-* colour token`,
			});
		}

		for (const m of raw.matchAll(RIGEL_RE)) {
			const name = m[0];
			if (markAllowed && ALLOWED_MARK_RIGEL[name]) continue;
			findings.push({
				file: relPath,
				line: lineNo,
				kind: "rigel",
				match: name,
				message: markAllowed
					? `${name} is not the sanctioned mark purple — only --rigel-purple[-hi|-lit] is allowed in the mark file`
					: `primitive ${name} — component/base CSS consumes only --cx-* tokens`,
			});
		}

		for (const m of raw.matchAll(DURATION_RE)) {
			findings.push({
				file: relPath,
				line: lineNo,
				kind: "duration",
				match: m[0],
				message: `literal duration ${m[0]} — use a --cx-motion-*/--cx-*-period token`,
			});
		}

		for (const m of raw.matchAll(CUBIC_RE)) {
			findings.push({
				file: relPath,
				line: lineNo,
				kind: "easing",
				match: m[0],
				message: "literal cubic-bezier() — use a --cx-ease-* token",
			});
		}
		for (const m of raw.matchAll(EASE_KEYWORD_RE)) {
			findings.push({
				file: relPath,
				line: lineNo,
				kind: "easing",
				match: m[0],
				message: `literal easing keyword "${m[0]}" — use a --cx-ease-* token`,
			});
		}
	}

	return findings;
}

/**
 * Aggregate the findings across a set of (path, text) CSS files. tokens.css is
 * the authoring file and is never a subject; callers should exclude it before
 * calling, but this filters it defensively too.
 */
export function scanAll(files: { path: string; text: string }[]): Finding[] {
	const out: Finding[] = [];
	for (const { path, text } of files) {
		if (path === TOKENS_REL) continue;
		out.push(...scanCss(path, text));
	}
	out.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line);
	return out;
}

/** Resolve the blocking posture from env/argv (default WARN). */
export function resolveMode(
	env: Record<string, string | undefined>,
	argv: string[],
): Mode {
	if (argv.includes("--error")) return "error";
	if (argv.includes("--warn")) return "warn";
	const v = (env.CX_GATE_MODE ?? "").toLowerCase();
	return v === "error" ? "error" : "warn";
}

// ---------------------------------------------------------------------------
// I/O wiring.
// ---------------------------------------------------------------------------

export interface Deps {
	root: string;
	mode: Mode;
	/** List repo-relative CSS paths under UI_SRC_DIR (excl. tokens.css). */
	listCssFiles: (root: string) => Promise<string[]>;
	/** Read a repo-relative file, or null if it does not exist. */
	readText: (root: string, relPath: string) => Promise<string | null>;
	log: (msg: string) => void;
	err: (msg: string) => void;
}

export async function runOnce(deps: Deps): Promise<number> {
	const { root, mode, listCssFiles, readText, log, err } = deps;

	let cssFiles: string[];
	try {
		cssFiles = await listCssFiles(root);
	} catch (error) {
		err(`cx-token-gate: cannot read the tree at ${root}`);
		err(error instanceof Error ? error.message : String(error));
		return 2;
	}

	const files: { path: string; text: string }[] = [];
	for (const path of cssFiles) {
		if (path === TOKENS_REL) continue;
		const text = await readText(root, path);
		if (text === null) continue; // listed but vanished — ignore
		files.push({ path, text });
	}

	const findings = scanAll(files);

	if (findings.length === 0) {
		log(
			`cx-token-gate: OK — ${files.length} CSS file(s) consume only --cx-* tokens.`,
		);
		return 0;
	}

	err("");
	err(
		`cx-token-gate: ${findings.length} finding(s) [${mode.toUpperCase()} mode]:`,
	);
	for (const { file, line, message } of findings) {
		err(`  ${file}:${line}: ${message}`);
	}
	err("");
	err(
		"Consumption rule (D2/D9): component + base CSS consumes only --cx-* tokens.",
	);
	err("The mark component may name --rigel-purple directly; nothing else may.");

	if (mode === "error") return 1;
	err("");
	err(
		"cx-token-gate: WARN mode — findings reported, not blocking. Flip to ERROR at adoption step 5.",
	);
	return 0;
}

if (import.meta.main) {
	const root =
		process.env.GATE_ROOT ??
		(await $`git rev-parse --show-toplevel`.nothrow().quiet().text()).trim();
	const mode = resolveMode(process.env, process.argv.slice(2));

	process.exit(
		await runOnce({
			root,
			mode,
			listCssFiles: async (r) => {
				const glob = new Bun.Glob(`${UI_SRC_DIR}/**/*.css`);
				const out: string[] = [];
				for await (const rel of glob.scan({ cwd: r })) {
					const posix = rel.replaceAll("\\", "/");
					if (posix !== TOKENS_REL) out.push(posix);
				}
				return out.sort();
			},
			readText: async (r, relPath) => {
				const file = Bun.file(`${r}/${relPath}`);
				return (await file.exists()) ? await file.text() : null;
			},
			log: (msg) => console.log(msg),
			err: (msg) => console.error(msg),
		}),
	);
}
