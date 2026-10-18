// Ban inline SQL at pgx call sites; the tokenizer spans newlines because SQL
// literals often start on the line after the call.
// Hoisted identifiers are checked only in pgx SQL slots to avoid unrelated Exec calls.

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { $ } from "bun";

/** The sqlc-generated package: never a subject (it IS the sanctioned path). */
export const GENERATED_PREFIX = "go/internal/store/db/";
/** The Go source glob the gate governs, relative to the scan root. */
export const GO_GLOB = "go/**/*.go";

/** SQL keywords whose presence in a call-site literal marks it as inline SQL. */
const SQL_KEYWORD_RE = /\b(?:SELECT|INSERT|UPDATE|DELETE|WITH|CREATE|DROP)\b/i;
/** pgx query methods. QueryRow before Query so the longer name wins. */
const CALL_RE = /\.(?:QueryRow|Query|Exec)\(/g;
/**
 * The last receiver segment names that ARE a pgx pool/tx/conn handle in this
 * codebase: `s.pool`→pool, `tx`, `conn`, `c` (a *pgx.Conn). Only these carry a
 * SQL slot, so a bare-identifier SQL arg (`q`, `ddl`, `m.sql`) is flagged ONLY
 * at these receivers — the structural exclusion of runtime/compute
 * `r.runtime.Exec(ctx, id, spec)` / `g.client.Exec(ctx, req)` (receivers
 * runtime/client/engine), whose identifier args are not SQL. sqlc's own
 * `s.q.GetFoo(…)` never matches CALL_RE (not Query/QueryRow/Exec).
 */
const PGX_RECEIVERS: Record<string, true> = {
	pool: true,
	tx: true,
	conn: true,
	c: true,
};
/** A bare identifier or dotted selector — `q`, `ddl`, `m.sql`, `query`. */
const IDENT_RE = /^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*/;

/**
 * The ratcheting allowlist: files permitted to carry inline SQL today. Seeded
 * (T1) to every store file the gate actually flags, plus the two PERMANENT raw
 * files. Shrinks by one entry per migration task (T2..T6); the stale-entry
 * check makes leaving a migrated file here a hard failure.
 *
 * PERMANENT (never removed):
 *   - go/internal/store/store.go     — the migration runner (bootstrap DDL +
 *     schema_migrations bookkeeping + pg_advisory_lock; chicken-and-egg with
 *     the schema sqlc compiles against).
 *   - go/internal/pgshare/pgshare.go — the build-tagged test harness; CREATE/
 *     DROP SCHEMA with an interpolated, self-generated identifier.
 *
 * NOTE (T7): the migration is complete — every store domain file's inline SQL
 * moved to sqlc, so the allowlist is down to the two PERMANENT entries below.
 * The identifier-passed-SQL promotion (rule (b) above) is now LIVE: the only
 * remaining bare-identifier SQL at a pgx receiver is the migration runner's
 * `conn.Exec(ctx, ddl)` / `tx.Exec(ctx, m.sql)` in store.go, which is
 * permanently allowlisted — so the gate stays green while banning any NEW
 * const-hoisted SQL (the former `agent_tree.go`/`presence_reads.go` shape)
 * anywhere else in go/.
 */
export const ALLOWLIST: string[] = [
	// Every store domain file's inline SQL has migrated to sqlc (T2..T6,
	// RIG-3034); the two PERMANENT raw-SQL files below are all that remain.
	// Permanent raw-SQL files.
	"go/internal/store/store.go",
	"go/internal/pgshare/pgshare.go",
];

/** A single inline-SQL violation, located by repo-relative file + 1-based line. */
export interface Finding {
	file: string;
	line: number;
	snippet: string;
}

// ---------------------------------------------------------------------------
// The tokenizer (pure; exported for unit tests).
// ---------------------------------------------------------------------------

/**
 * Mark every character index that is CODE (1) vs inside a string literal, rune
 * literal, or comment (0). Used to find call sites without matching a `.Exec(`
 * that appears inside a string or a comment.
 */
function maskCode(text: string): Uint8Array {
	const n = text.length;
	const mask = new Uint8Array(n);
	let i = 0;
	while (i < n) {
		const c = text.charAt(i);
		if (c === '"') {
			i++;
			while (i < n) {
				const d = text.charAt(i);
				if (d === "\\") {
					i += 2;
					continue;
				}
				if (d === '"' || d === "\n") {
					i++;
					break;
				}
				i++;
			}
			continue;
		}
		if (c === "`") {
			i++;
			while (i < n && text.charAt(i) !== "`") i++;
			i++;
			continue;
		}
		if (c === "'") {
			i++;
			while (i < n) {
				const d = text.charAt(i);
				if (d === "\\") {
					i += 2;
					continue;
				}
				if (d === "'" || d === "\n") {
					i++;
					break;
				}
				i++;
			}
			continue;
		}
		if (c === "/" && text.charAt(i + 1) === "/") {
			while (i < n && text.charAt(i) !== "\n") i++;
			continue;
		}
		if (c === "/" && text.charAt(i + 1) === "*") {
			i += 2;
			while (i < n && !(text.charAt(i) === "*" && text.charAt(i + 1) === "/"))
				i++;
			i += 2;
			continue;
		}
		mask[i] = 1;
		i++;
	}
	return mask;
}

/** A parsed top-level call argument and where it starts in the source. */
interface Arg {
	text: string;
	start: number;
}

/**
 * Parse the top-level, comma-separated arguments of a call whose `(` is at
 * `open`, tokenizing ACROSS newlines and respecting nested parens/brackets/
 * braces, string + rune literals, and comments. Returns each argument's raw
 * source text and absolute start index.
 */
function parseArgs(text: string, open: number): Arg[] {
	const n = text.length;
	const args: Arg[] = [];
	let depth = 1;
	let i = open + 1;
	let argStart = i;
	const push = (end: number) => {
		if (end > argStart || args.length > 0)
			args.push({ text: text.slice(argStart, end), start: argStart });
	};
	while (i < n && depth > 0) {
		const c = text.charAt(i);
		if (c === '"') {
			i++;
			while (i < n) {
				const d = text.charAt(i);
				if (d === "\\") {
					i += 2;
					continue;
				}
				if (d === '"' || d === "\n") {
					i++;
					break;
				}
				i++;
			}
			continue;
		}
		if (c === "`") {
			i++;
			while (i < n && text.charAt(i) !== "`") i++;
			i++;
			continue;
		}
		if (c === "'") {
			i++;
			while (i < n) {
				const d = text.charAt(i);
				if (d === "\\") {
					i += 2;
					continue;
				}
				if (d === "'" || d === "\n") {
					i++;
					break;
				}
				i++;
			}
			continue;
		}
		if (c === "/" && text.charAt(i + 1) === "/") {
			while (i < n && text.charAt(i) !== "\n") i++;
			continue;
		}
		if (c === "/" && text.charAt(i + 1) === "*") {
			i += 2;
			while (i < n && !(text.charAt(i) === "*" && text.charAt(i + 1) === "/"))
				i++;
			i += 2;
			continue;
		}
		if (c === "(" || c === "[" || c === "{") {
			depth++;
			i++;
			continue;
		}
		if (c === ")" || c === "]" || c === "}") {
			depth--;
			if (depth === 0) {
				push(i);
				break;
			}
			i++;
			continue;
		}
		if (c === "," && depth === 1) {
			push(i);
			i++;
			argStart = i;
			continue;
		}
		i++;
	}
	return args;
}

/**
 * The index within `arg` of the first non-trivia character (skipping leading
 * whitespace and comments), or -1 if the argument is all trivia.
 */
function firstMeaningfulIndex(arg: string): number {
	let i = 0;
	const n = arg.length;
	while (i < n) {
		const c = arg.charAt(i);
		if (c === " " || c === "\t" || c === "\n" || c === "\r") {
			i++;
			continue;
		}
		if (c === "/" && arg.charAt(i + 1) === "/") {
			while (i < n && arg.charAt(i) !== "\n") i++;
			continue;
		}
		if (c === "/" && arg.charAt(i + 1) === "*") {
			i += 2;
			while (i < n && !(arg.charAt(i) === "*" && arg.charAt(i + 1) === "/"))
				i++;
			i += 2;
			continue;
		}
		return i;
	}
	return -1;
}

/** Concatenate the contents of every string literal in `arg` (drops `+`, etc). */
function stringContents(arg: string): string {
	const n = arg.length;
	let out = "";
	let i = 0;
	while (i < n) {
		const c = arg.charAt(i);
		if (c === '"') {
			i++;
			while (i < n) {
				const d = arg.charAt(i);
				if (d === '"') {
					i++;
					break;
				}
				if (d === "\\") {
					out += arg.charAt(i + 1);
					i += 2;
					continue;
				}
				out += d;
				i++;
			}
			continue;
		}
		if (c === "`") {
			i++;
			while (i < n && arg.charAt(i) !== "`") {
				out += arg.charAt(i);
				i++;
			}
			i++;
			continue;
		}
		i++;
	}
	return out;
}

function lineStarts(text: string): number[] {
	const starts = [0];
	for (let i = 0; i < text.length; i++)
		if (text.charAt(i) === "\n") starts.push(i + 1);
	return starts;
}

function lineOf(index: number, starts: number[]): number {
	let lo = 0;
	let hi = starts.length - 1;
	let ans = 0;
	while (lo <= hi) {
		const mid = (lo + hi) >> 1;
		const s = starts[mid] ?? 0;
		if (s <= index) {
			ans = mid;
			lo = mid + 1;
		} else {
			hi = mid - 1;
		}
	}
	return ans + 1;
}

function sourceLine(text: string, line: number, starts: number[]): string {
	const start = starts[line - 1] ?? 0;
	const next = starts[line];
	const end = next === undefined ? text.length : next - 1;
	return text.slice(start, end);
}

/**
 * The last receiver segment of a `.Query|.QueryRow|.Exec` call — the token
 * immediately before the `.` at `dot`. For `s.pool.Exec(` returns "pool"; for
 * `tx.Exec(` returns "tx"; for `r.runtime.Exec(` returns "runtime". Returns ""
 * when no identifier immediately precedes the dot (e.g. a `).Exec(` chained off
 * a call result), which is never a pgx handle.
 */
function receiverSegment(text: string, dot: number): string {
	let i = dot - 1;
	while (i >= 0 && /[A-Za-z0-9_]/.test(text.charAt(i))) i--;
	return text.slice(i + 1, dot);
}

/**
 * If `s` begins with a bare identifier or dotted selector (`q`, `ddl`, `m.sql`)
 * and nothing else follows it but trivia/comma/close, return that identifier's
 * leading segment; otherwise null. A trailing `(` (call), `[` (index), `+`
 * (concat), or `{` (composite) disqualifies it — those are expressions, not a
 * hoisted-SQL name.
 */
function bareIdentifier(s: string): string | null {
	const m = IDENT_RE.exec(s);
	if (m === null) return null;
	const rest = s.slice(m[0].length);
	const nextMeaningful = firstMeaningfulIndex(rest);
	if (nextMeaningful >= 0) {
		const c = rest.charAt(nextMeaningful);
		if (c !== "," && c !== ")") return null;
	}
	return m[0].split(".")[0] ?? m[0];
}

/**
 * Scan ONE Go file's text for inline-SQL findings. Pure: no I/O, no allowlist,
 * no exit — returns every raw finding so callers can apply the allowlist and
 * the stale-entry check on top.
 */
export function scanText(file: string, text: string): Finding[] {
	const mask = maskCode(text);
	const starts = lineStarts(text);
	const findings: Finding[] = [];
	for (const m of text.matchAll(CALL_RE)) {
		const dot = m.index;
		if (dot === undefined || mask[dot] !== 1) continue;
		const open = dot + m[0].length - 1;
		const args = parseArgs(text, open);
		const push = (index: number) => {
			const line = lineOf(index, starts);
			findings.push({
				file,
				line,
				snippet: sourceLine(text, line, starts).trim(),
			});
		};

		// Rule (a): the first STRING-LITERAL argument is the SQL slot (an
		// identifier or expression — ctx, handle.id, spec — never starts with a
		// delimiter). Flag it iff it carries a SQL keyword. Fires at ANY receiver.
		let flagged = false;
		for (const arg of args) {
			const fm = firstMeaningfulIndex(arg.text);
			if (fm < 0) continue;
			const lead = arg.text.charAt(fm);
			if (lead !== '"' && lead !== "`") continue;
			if (SQL_KEYWORD_RE.test(stringContents(arg.text))) {
				push(arg.start + fm);
				flagged = true;
			}
			break;
		}
		if (flagged) continue;

		// Check only pgx SQL slots: runtime/compute Exec calls and non-SQL parameters
		// are outside this rule; bare identifiers/selectors represent hoisted SQL.
		if (!PGX_RECEIVERS[receiverSegment(text, dot)]) continue;
		const slot = args[1];
		if (slot === undefined) continue;
		const fm = firstMeaningfulIndex(slot.text);
		if (fm < 0) continue;
		const ident = bareIdentifier(slot.text.slice(fm));
		if (ident === null || ident === "ctx") continue;
		push(slot.start + fm);
	}
	return findings;
}

// ---------------------------------------------------------------------------
// Aggregation + allowlist logic (pure).
// ---------------------------------------------------------------------------

/** A file is out of scope if it is the generated package or a test file. */
export function isExcludedPath(rel: string): boolean {
	return rel.startsWith(GENERATED_PREFIX) || rel.endsWith("_test.go");
}

/** Raw findings across every in-scope file (before the allowlist is applied). */
export function scanFiles(files: { path: string; text: string }[]): Finding[] {
	const out: Finding[] = [];
	for (const { path, text } of files) {
		if (isExcludedPath(path)) continue;
		out.push(...scanText(path, text));
	}
	out.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line);
	return out;
}

/**
 * Allowlist entries that matched NO raw finding — the fail-closed stale check.
 * A migrated (or never-flagged) file left in the allowlist is a hard failure,
 * so the ratchet cannot silently over-permit.
 */
export function staleAllowlistEntries(
	rawFindings: Finding[],
	allowlist: string[],
): string[] {
	const flagged = new Set(rawFindings.map((f) => f.file));
	return allowlist.filter((entry) => !flagged.has(entry));
}

// ---------------------------------------------------------------------------
// I/O wiring.
// ---------------------------------------------------------------------------

export interface Deps {
	root: string;
	allowlist: string[];
	/** List repo-relative go/**\/*.go paths under root. */
	listGoFiles: (root: string) => string[];
	/** Read a repo-relative file, or null if it does not exist. */
	readText: (root: string, relPath: string) => string | null;
	log: (msg: string) => void;
	err: (msg: string) => void;
}

export function runOnce(deps: Deps): number {
	const { root, allowlist, listGoFiles, readText, log, err } = deps;

	let paths: string[];
	try {
		paths = listGoFiles(root);
	} catch (error) {
		err(`inline-sql-gate: cannot read the tree at ${root}`);
		err(error instanceof Error ? error.message : String(error));
		return 2;
	}

	const files: { path: string; text: string }[] = [];
	for (const path of paths) {
		if (isExcludedPath(path)) continue;
		const text = readText(root, path);
		if (text === null) continue; // listed but vanished — ignore
		files.push({ path, text });
	}

	const raw = scanFiles(files);
	const allow = new Set(allowlist);
	const active = raw.filter((f) => !allow.has(f.file));
	const stale = staleAllowlistEntries(raw, allowlist);

	if (active.length === 0 && stale.length === 0) {
		log(
			`inline-sql-gate: OK — scanned ${files.length} Go file(s); ${allowlist.length} allowlisted file(s) still carry inline SQL, no new inline SQL.`,
		);
		return 0;
	}

	if (active.length > 0) {
		err("");
		err(`inline-sql-gate: ${active.length} inline-SQL finding(s):`);
		for (const { file, line, snippet } of active) {
			err(`  ${file}:${line}: ${snippet}`);
		}
		err("");
		err(
			"Inline SQL at a pgx call site is banned (sqlc adoption). Move the query",
		);
		err(
			"into go/internal/store/queries/<domain>.sql and call the generated db.Queries method.",
		);
	}

	if (stale.length > 0) {
		err("");
		err(
			`inline-sql-gate: ${stale.length} stale allowlist entr(y/ies) — no inline SQL found, so the entry must be removed:`,
		);
		for (const entry of stale) err(`  ${entry}`);
		err("");
		err(
			"The allowlist ratchets down: a migrated (or never-flagged) file cannot stay allowlisted.",
		);
	}

	return 1;
}

if (import.meta.main) {
	const root =
		process.env.GATE_ROOT ??
		(await $`git rev-parse --show-toplevel`.nothrow().quiet().text()).trim();

	process.exit(
		runOnce({
			root,
			allowlist: ALLOWLIST,
			listGoFiles: (r) => {
				const glob = new Bun.Glob(GO_GLOB);
				const out: string[] = [];
				for (const rel of glob.scanSync({ cwd: r, onlyFiles: true }))
					out.push(rel.replaceAll("\\", "/"));
				return out.sort();
			},
			readText: (r, relPath) => {
				try {
					return readFileSync(join(r, relPath), "utf8");
				} catch {
					return null;
				}
			},
			log: (msg) => console.log(msg),
			err: (msg) => console.error(msg),
		}),
	);
}
