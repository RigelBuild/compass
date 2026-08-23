// design-ledger-gate — validate the Compass design-decision ledger (SEA-1187).
//
// The design corpus under docs/designs/product/ freezes on merge; later
// records supersede specific decisions by citation. Supersession was only
// visible forward (the superseding record cites the superseded one, nothing
// points back), so an agent grounding on a single record could not tell a
// decision in it was overturned elsewhere. The fix (design record:
// docs/designs/product/compass-design-ledger/design.md) is a canonical
// read-first ledger (DECISIONS.md), machine-checkable per-record `Status:`
// headers, and this gate against dangling pointers + a forgotten same-PR
// ledger flip.
//
// This gate has TWO legs sharing one pure core (evaluate):
//   * SNAPSHOT — pointer/grammar integrity over a single tree state at
//     GATE_ROOT: ledger row grammar, dangling/self/cyclic supersessions,
//     unresolvable Record links (missing path, dead #anchor, or a >50 KB
//     record link without the required anchor), and every record's `Status:`
//     header (present, grammar-conformant, correct Historical-set membership).
//     Runs on every event (the tool's own `moon run design-ledger-gate:gate`).
//   * TOUCH-COUPLING (DL-Q1) — a PR whose changed set touches a product design
//     record MUST also touch DECISIONS.md, unless it declares `Ledger-impact:`
//     in the PR body. PR-event-only (needs PR context); on non-PR events the
//     changed set is empty and this leg no-ops.
//
// What the SNAPSHOT core does NOT prove (rows are append-only; frozen
// `Decision`-cell prose is immutable-after-append) is review-enforced in v1; a
// fast-follow diff-aware core promotes it to gate-checked (merge-base compare).
//
// Inputs (env):
//   GATE_ROOT  - directory to scan (default: git toplevel). Tests point the
//                injected reads at fixtures instead.
//   REPO, PR_NUMBER, GH_TOKEN - set by the `design-ledger` meta job on
//                pull_request events, for the touch-coupling leg (mirrors
//                tools/spec-impact-gate).
// Exit codes:
//   0 - all checks pass
//   1 - one or more violations (printed one per line as `<file>:<line>: <msg>`)
//   2 - usage / internal error (e.g. cannot read the tree)

import { existsSync, readFileSync } from "node:fs";
import { posix as pathPosix } from "node:path";
import { $ } from "bun";

/** The product design-corpus directory the gate governs. */
export const PRODUCT_DIR = "docs/designs/product";
/** The canonical ledger, parsed as the decision table (never as a record). */
export const DECISIONS_PATH = `${PRODUCT_DIR}/DECISIONS.md`;
/**
 * A Record link into a record larger than this MUST carry a resolving
 * `#anchor`, so rationale is genuinely one hop away, not a hunt through a big
 * file (design record §Approach part 1). ~50 KB.
 */
export const LARGE_RECORD_BYTES = 50 * 1024;

/**
 * The version-narrative chain: a record is `Historical` IFF it is one of
 * these (design record §Approach part 2, §T2). Pinned as literal paths — the
 * set is the thing under test, so deriving it from anything else would let a
 * drifted membership pass silently.
 */
export const HISTORICAL_CHAIN: Record<string, true> = {
	[`${PRODUCT_DIR}/compass.md`]: true,
	[`${PRODUCT_DIR}/compass-0.4/design.md`]: true,
	[`${PRODUCT_DIR}/compass-0.5/design.md`]: true,
	[`${PRODUCT_DIR}/compass-0.5-server/design.md`]: true,
	[`${PRODUCT_DIR}/compass-0.6/design.md`]: true,
	[`${PRODUCT_DIR}/compass-0.7-channel-workspace/design.md`]: true,
	[`${PRODUCT_DIR}/compass-0.8-threading-and-session-renderer/design.md`]: true,
};

/**
 * A `Ledger-impact:` declaration line in a PR body — the touch-coupling escape
 * hatch, mirroring `Spec-impact:` (tools/spec-impact-gate/gate.ts). Must start
 * its own line; a single leading `>` quote (GitHub quoted trailers)
 * or indentation is tolerated; the value must be non-empty.
 */
const LEDGER_IMPACT_RE = /^\s*>?\s*ledger-impact:\s*(\S.*)$/im;

/**
 * Head-branch prefixes exempt from the touch-coupling leg — automation branches
 * that cannot author a `Ledger-impact:` declaration (mirrors
 * tools/spec-impact-gate's EXEMPT_BRANCH_PREFIXES):
 *   - `renovate/`  — Renovate dependency bumps.
 * The SNAPSHOT leg still runs on these events; only touch-coupling is skipped.
 * Everything else — human and agent feature branches alike — must comply.
 */
export const EXEMPT_BRANCH_PREFIXES = ["renovate/"];

/** The anchored record-level `Status:` grammar (design record §Approach part 2). */
const STATUS_RE = /^Status: (Draft|Active|Historical|Superseded by (\S+))$/;

/** A ledger row's `Active (<who>, YYYY-MM-DD)` status cell. */
const ROW_ACTIVE_RE = /^Active \(.+, \d{4}-\d{2}-\d{2}\)$/;
/** A ledger row's `Superseded by DL-<n> (<who>, YYYY-MM-DD)` status cell. */
const ROW_SUPERSEDED_RE = /^Superseded by (DL-\d+) \(.+, \d{4}-\d{2}-\d{2}\)$/;
/**
 * A ledger row's `Retired (<who>, YYYY-MM-DD)` status cell — a decision
 * retracted with NO successor (the ADR/MADR `deprecated`/retired state).
 * Distinct from `Superseded by DL-<n>`, which requires a successor row; a
 * `Retired` row points nowhere, so it never enters supersession resolution.
 */
const ROW_RETIRED_RE = /^Retired \(.+, \d{4}-\d{2}-\d{2}\)$/;

// ---------------------------------------------------------------------------
// Parsed shapes (produced by the pure parsers below, consumed by evaluate).
// ---------------------------------------------------------------------------

/** One parsed ledger table row. */
export interface LedgerRow {
	/** `DL-<n>` id. */
	id: string;
	/** One-line decision paraphrase cell (opaque to the gate). */
	decision: string;
	/** Raw status cell text. */
	status: string;
	/** Raw Record cell text (a markdown link `[label](target)`). */
	recordCell: string;
	/** 1-based line in DECISIONS.md, for error reporting. */
	line: number;
}

/** One record's `Status:` header slot. */
export interface RecordHeader {
	/** Repo-relative record path. */
	path: string;
	/** The raw `Status:` line if the H1's first non-blank successor is one,
	 *  else null (no parseable header present). */
	statusLine: string | null;
	/** 1-based line of the status slot, for error reporting. */
	line: number;
}

/** The diff-aware input for the touch-coupling leg. */
export interface Changed {
	/** Repo-relative paths the PR adds/modifies/deletes. */
	files: string[];
	/** The PR body (touch-coupling escape-hatch scan). Null off PR events. */
	body: string | null;
	/**
	 * The PR's head branch name, for the automation exemption. Empty string off
	 * PR events (the touch-coupling leg no-ops there regardless).
	 */
	headBranch: string;
}

/** What `readRecord` returns for a link/pointer target. */
export interface RecordContent {
	/** GitHub-style slugs of every heading in the target. */
	headings: string[];
	/** Byte size on disk (drives the large-record anchor rule). */
	sizeBytes: number;
}

/** A single gate failure. `line` 0 marks a non-line-specific locus. */
export interface Violation {
	file: string;
	line: number;
	message: string;
}

/** The record-level `Status:` value, parsed. */
export type StatusValue =
	| { kind: "Draft" }
	| { kind: "Active" }
	| { kind: "Historical" }
	| { kind: "Superseded"; path: string };

// ---------------------------------------------------------------------------
// Pure helpers (exported for unit tests).
// ---------------------------------------------------------------------------

/**
 * GitHub-style heading slug (github-slugger algorithm): lowercase, strip
 * punctuation (keep word chars, whitespace, hyphen), each whitespace run's
 * chars each become one hyphen (NO collapse — `Problem / Intent` →
 * `problem--intent`, matching GitHub so ledger anchors resolve for humans too).
 */
export function slugify(heading: string): string {
	return heading
		.trim()
		.toLowerCase()
		.replace(/[^\w\s-]/gu, "")
		.replace(/\s/g, "-");
}

/** Parse a record-level `Status:` line against the anchored grammar. */
export function parseStatusValue(statusLine: string): StatusValue | null {
	const m = STATUS_RE.exec(statusLine.trimEnd());
	if (!m) return null;
	const kw = m[1];
	if (kw === "Draft") return { kind: "Draft" };
	if (kw === "Active") return { kind: "Active" };
	if (kw === "Historical") return { kind: "Historical" };
	// biome-ignore lint/style/noNonNullAssertion: the `Superseded by (\S+)` branch guarantees group 2.
	return { kind: "Superseded", path: m[2]! };
}

/** Split a Record cell's markdown link into its path + optional `#anchor`. */
export function splitLink(
	cell: string,
): { path: string; anchor: string | null } | null {
	const m = /\[[^\]]*\]\(([^)]+)\)/.exec(cell);
	if (!m) return null;
	const target = m[1] ?? "";
	const hash = target.indexOf("#");
	if (hash === -1) return { path: target, anchor: null };
	return { path: target.slice(0, hash), anchor: target.slice(hash + 1) };
}

/** True when a repo-relative path is a product design record (not the ledger). */
export function touchesRecord(file: string): boolean {
	if (!file.startsWith(`${PRODUCT_DIR}/`)) return false;
	if (file === DECISIONS_PATH) return false;
	const rest = file.slice(PRODUCT_DIR.length + 1);
	if (rest.endsWith("/design.md")) return true; // <name>/design.md layout
	if (rest.endsWith(".md") && !rest.includes("/")) return true; // <name>.md layout
	return false;
}

/**
 * Resolve a record-level `Status: Superseded by <path>` pointer to a
 * product-relative path for `readRecord`. That header value is written
 * RECORD-relative (a link a human follows from inside the record's own
 * directory — e.g. `../compass-0.8/design.md` from `compass-0.6/design.md`),
 * unlike a ledger Record cell which is product-relative. `recordProductRelPath`
 * is the superseded record's own product-relative path; the result is
 * normalized product-relative (no leading `./`), or null if the pointer escapes
 * PRODUCT_DIR.
 */
export function resolveRecordRelative(
	recordProductRelPath: string,
	pointer: string,
): string | null {
	const joined = pathPosix.join(
		pathPosix.dirname(recordProductRelPath),
		pointer,
	);
	// A pointer that climbs out of PRODUCT_DIR (leading `..`) can't be a product
	// record; readRecord would resolve it outside the corpus.
	if (joined.startsWith("..")) return null;
	return joined;
}

/**
 * Split a GFM table row on unescaped `|`, resolving cell escapes. In GFM a
 * backslash escapes the following pipe (`\|` is a literal `|` inside the cell),
 * so it must not act as a column delimiter; `\\` resolves to a single
 * backslash. A naive `.split("|")` treats `\|` as a delimiter, inflating the
 * cell count so `parseLedger`'s 4-cell check silently discards the row.
 */
function splitLedgerRow(row: string): string[] {
	const cells: string[] = [];
	let cur = "";
	for (let i = 0; i < row.length; i++) {
		const ch = row[i];
		if (ch === "\\" && (row[i + 1] === "|" || row[i + 1] === "\\")) {
			cur += row[i + 1];
			i++;
			continue;
		}
		if (ch === "|") {
			cells.push(cur);
			cur = "";
			continue;
		}
		cur += ch;
	}
	cells.push(cur);
	return cells;
}

/** Parse DECISIONS.md text into ledger rows (topic headings/prose skipped). */
export function parseLedger(text: string): LedgerRow[] {
	const rows: LedgerRow[] = [];
	text.split("\n").forEach((line, i) => {
		const trimmed = line.trim();
		if (!trimmed.startsWith("|")) return;
		// Escape-aware split: the leading/trailing delimiter pipes become empty
		// edge cells (dropped here), and interior `\|` stays inside its cell.
		const raw = splitLedgerRow(trimmed);
		if (raw[0] === "") raw.shift();
		if (raw.length > 0 && raw[raw.length - 1] === "") raw.pop();
		const cells = raw.map((c) => c.trim());
		if (cells.length !== 4) return;
		const id = cells[0] ?? "";
		if (!/^DL-\d+$/.test(id)) return; // skips header + separator + prose rows
		rows.push({
			id,
			decision: cells[1] ?? "",
			status: cells[2] ?? "",
			recordCell: cells[3] ?? "",
			line: i + 1,
		});
	});
	return rows;
}

/**
 * Parse a record's `Status:` header slot: the first non-blank line after the
 * H1. If that slot is a `Status:`-keyed line it's the header (even when
 * malformed); anything else (a prose preamble) means no header is present.
 */
export function parseRecordHeader(path: string, text: string): RecordHeader {
	const lines = text.split("\n");
	let h1 = -1;
	let inFence = false;
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i] ?? "";
		// Skip `#`-shaped lines inside a fenced code block (a record opens with a
		// real H1, so this only guards a pathological pre-H1 fence).
		if (/^\s*(```|~~~)/.test(line)) {
			inFence = !inFence;
			continue;
		}
		if (!inFence && /^#\s/.test(line)) {
			h1 = i;
			break;
		}
	}
	if (h1 === -1) return { path, statusLine: null, line: 1 };
	for (let i = h1 + 1; i < lines.length; i++) {
		const raw = lines[i] ?? "";
		if (raw.trim() === "") continue;
		if (/^status:/i.test(raw.trim())) {
			return { path, statusLine: raw.trimEnd(), line: i + 1 };
		}
		return { path, statusLine: null, line: i + 1 }; // slot is prose, not a header
	}
	return { path, statusLine: null, line: h1 + 1 };
}

// ---------------------------------------------------------------------------
// The pure decision core.
// ---------------------------------------------------------------------------

/**
 * The gate decision over a single tree snapshot + the PR's changed set. Pure:
 * all I/O is behind `readRecord` (a link/pointer target's slugs + byte size,
 * or null when the path does not resolve). Returns every violation found (the
 * caller sorts + prints); an empty array is a pass.
 */
export function evaluate(
	ledger: LedgerRow[],
	records: RecordHeader[],
	changed: Changed,
	readRecord: (path: string) => RecordContent | null,
): Violation[] {
	const violations: Violation[] = [];
	const v = (file: string, line: number, message: string) =>
		violations.push({ file, line, message });

	// --- Ledger rows: id uniqueness, status grammar, supersession integrity,
	//     and Record-link resolution. ---
	const byId = new Map<string, LedgerRow>();
	for (const row of ledger) {
		if (byId.has(row.id)) {
			v(DECISIONS_PATH, row.line, `duplicate ledger id ${row.id}`);
			continue; // first occurrence is canonical for pointer resolution
		}
		byId.set(row.id, row);
	}

	for (const row of ledger) {
		// Record-link resolution (independent of status).
		const link = splitLink(row.recordCell);
		if (link === null) {
			v(
				DECISIONS_PATH,
				row.line,
				`${row.id}: Record cell is not a markdown link \`[label](path)\``,
			);
		} else {
			const target = readRecord(link.path);
			if (target === null) {
				v(
					DECISIONS_PATH,
					row.line,
					`${row.id}: Record link path does not resolve: ${link.path}`,
				);
			} else if (link.anchor !== null) {
				if (!target.headings.includes(link.anchor)) {
					v(
						DECISIONS_PATH,
						row.line,
						`${row.id}: Record link #anchor not found in ${link.path}: #${link.anchor}`,
					);
				}
			} else if (target.sizeBytes > LARGE_RECORD_BYTES) {
				v(
					DECISIONS_PATH,
					row.line,
					`${row.id}: Record link into a large record (>${Math.floor(
						LARGE_RECORD_BYTES / 1024,
					)} KB) must carry a #anchor: ${link.path}`,
				);
			}
		}

		// Status-cell grammar, then supersession-target integrity. `Active` and
		// `Retired` are terminal (no target to resolve); only `Superseded by
		// DL-<n>` carries a successor that must exist.
		if (ROW_ACTIVE_RE.test(row.status)) continue;
		if (ROW_RETIRED_RE.test(row.status)) continue;
		const sup = ROW_SUPERSEDED_RE.exec(row.status);
		if (sup === null) {
			v(
				DECISIONS_PATH,
				row.line,
				`${row.id}: malformed Status cell (want \`Active (<who>, YYYY-MM-DD)\`, \`Retired (<who>, YYYY-MM-DD)\`, or \`Superseded by DL-<n> (<who>, YYYY-MM-DD)\`): ${row.status}`,
			);
			continue;
		}
		const targetId = sup[1] ?? "";
		if (targetId === row.id) {
			v(DECISIONS_PATH, row.line, `${row.id}: superseded by itself`);
			continue;
		}
		const targetRow = byId.get(targetId);
		if (targetRow === undefined) {
			v(
				DECISIONS_PATH,
				row.line,
				`${row.id}: Superseded by ${targetId}, which is not a ledger row`,
			);
		}
	}

	// Walk each Superseded chain to its terminus. A ledger row cell is only ever
	// `Active` or `Superseded` (`Historical` is a record-level Status, never a
	// row), so a well-formed chain ends at an `Active` row. A chain that never
	// reaches a non-`Superseded` row loops — a 2-cycle, a ≥3-cycle, or any
	// longer loop — so none of its decisions is live current truth, the exact
	// silent drift the ledger exists to prevent. (Self-loops and dangling
	// targets are reported per-row above; a self-loop is a degenerate 1-cycle
	// skipped here to avoid a double report.) Each distinct cycle is reported
	// once, keyed by its member set, at the lowest line number among its members
	// (stable regardless of ledger array order).
	const cyclesReported = new Set<string>();
	for (const start of ledger) {
		if (!ROW_SUPERSEDED_RE.test(start.status)) continue;
		const path: LedgerRow[] = [];
		let cur: LedgerRow | undefined = start;
		while (cur !== undefined) {
			const node = cur;
			const idx = path.findIndex((r) => r.id === node.id);
			if (idx !== -1) {
				const cycle = path.slice(idx);
				const key = cycle
					.map((r) => r.id)
					.sort()
					.join("|");
				if (cycle.length > 1 && !cyclesReported.has(key)) {
					cyclesReported.add(key);
					const ids = cycle.map((r) => r.id);
					v(
						DECISIONS_PATH,
						Math.min(...cycle.map((r) => r.line)),
						`supersession cycle: ${ids.join(" → ")} → ${node.id}`,
					);
				}
				break;
			}
			path.push(node);
			const m = ROW_SUPERSEDED_RE.exec(node.status);
			if (m === null) break;
			cur = byId.get(m[1] ?? "");
		}
	}

	// --- Record `Status:` headers: presence, grammar, Historical-set membership,
	//     and resolvable record-level supersession pointers. ---
	for (const rec of records) {
		if (rec.statusLine === null) {
			v(
				rec.path,
				rec.line,
				"missing a parseable `Status:` header (first non-blank line after the H1)",
			);
			continue;
		}
		const value = parseStatusValue(rec.statusLine);
		if (value === null) {
			v(
				rec.path,
				rec.line,
				"malformed `Status:` header (want `^Status: (Draft|Active|Historical|Superseded by <path>)$`, no trailing text)",
			);
			continue;
		}
		const inChain = rec.path in HISTORICAL_CHAIN;
		if (value.kind === "Historical" && !inChain) {
			v(
				rec.path,
				rec.line,
				"`Status: Historical` but the record is not in the version-narrative chain",
			);
		}
		if (inChain && value.kind !== "Historical" && value.kind !== "Draft") {
			v(
				rec.path,
				rec.line,
				`in-chain record must be \`Status: Historical\` (or Draft), not ${value.kind}`,
			);
		}
		if (value.kind === "Superseded") {
			// `rec.path` is repo-relative; readRecord + resolveRecordRelative work
			// in product-relative space. The Superseded pointer is record-relative
			// (resolved from the record's own directory), unlike a ledger Record cell.
			const recProductRel = rec.path.startsWith(`${PRODUCT_DIR}/`)
				? rec.path.slice(PRODUCT_DIR.length + 1)
				: rec.path;
			const resolved = resolveRecordRelative(recProductRel, value.path);
			if (resolved === null || readRecord(resolved) === null) {
				v(
					rec.path,
					rec.line,
					`\`Status: Superseded by ${value.path}\` does not resolve to a record`,
				);
			}
		}
	}

	// --- Touch-coupling (DL-Q1): PR-event-only. A changed set that touches a
	//     product record but not DECISIONS.md, with no `Ledger-impact:` body
	//     declaration, fails. Off PR events the changed set is empty → no-op.
	//     An automation-exempt head branch (renovate/) skips this leg — it
	//     cannot author a declaration (mirrors spec-impact-gate); the SNAPSHOT
	//     checks above still ran. ---
	const exemptBranch = EXEMPT_BRANCH_PREFIXES.some((p) =>
		changed.headBranch.startsWith(p),
	);
	const touchedRecord = !exemptBranch && changed.files.some(touchesRecord);
	if (touchedRecord) {
		const touchedLedger = changed.files.includes(DECISIONS_PATH);
		const declared = LEDGER_IMPACT_RE.test(changed.body ?? "");
		if (!touchedLedger && !declared) {
			v(
				"(pull request)",
				0,
				"PR touches a docs/designs/product/ design record but neither updates " +
					`${DECISIONS_PATH} nor declares \`Ledger-impact:\` in the body. Append/flip ` +
					"the record's ledger rows in this PR, or add a `Ledger-impact: none` line.",
			);
		}
	}

	return violations;
}

// ---------------------------------------------------------------------------
// I/O wiring.
// ---------------------------------------------------------------------------

export interface Deps {
	root: string;
	/** Read a repo-relative file, or null if it does not exist. */
	readText: (root: string, relPath: string) => Promise<string | null>;
	/** List repo-relative record paths under PRODUCT_DIR (excl. DECISIONS.md). */
	listRecordFiles: (root: string) => Promise<string[]>;
	/** Resolve a link/pointer target (product-relative) to its slugs + bytes. */
	readRecord: (root: string, productRelPath: string) => RecordContent | null;
	/** The PR's changed set (empty off PR events). */
	changed: Changed;
	log: (msg: string) => void;
	err: (msg: string) => void;
}

export async function runOnce(deps: Deps): Promise<number> {
	const { root, readText, listRecordFiles, readRecord, changed, log, err } =
		deps;

	let ledgerText: string | null;
	let recordFiles: string[];
	try {
		ledgerText = await readText(root, DECISIONS_PATH);
		recordFiles = await listRecordFiles(root);
	} catch (error) {
		err(`design-ledger-gate: cannot read the tree at ${root}`);
		err(error instanceof Error ? error.message : String(error));
		return 2;
	}

	const violations: Violation[] = [];
	if (ledgerText === null) {
		violations.push({
			file: DECISIONS_PATH,
			line: 0,
			message: "the ledger DECISIONS.md was not found",
		});
	}
	const ledger = ledgerText === null ? [] : parseLedger(ledgerText);

	const records: RecordHeader[] = [];
	for (const path of recordFiles) {
		const text = await readText(root, path);
		if (text === null) continue; // listed but vanished — ignore
		records.push(parseRecordHeader(path, text));
	}

	violations.push(
		...evaluate(ledger, records, changed, (p) => readRecord(root, p)),
	);

	if (violations.length === 0) {
		log(
			`design-ledger-gate: OK — ${ledger.length} ledger row(s), ${records.length} record header(s) valid.`,
		);
		return 0;
	}

	violations.sort((a, b) => a.file.localeCompare(b.file) || a.line - b.line);
	err("");
	err(`design-ledger-gate: ${violations.length} violation(s):`);
	for (const { file, line, message } of violations) {
		err(line > 0 ? `  ${file}:${line}: ${message}` : `  ${file}: ${message}`);
	}
	err("");
	err("See docs/designs/product/compass-design-ledger/design.md.");
	return 1;
}

/** Compute a target record's heading slugs + byte size from its text. */
export function recordContentFromText(text: string): RecordContent {
	const headings: string[] = [];
	let inFence = false;
	for (const line of text.split("\n")) {
		// A ``` or ~~~ fence line toggles code-block state; a `#`-prefixed line
		// inside a fence is code, not a heading, so it must not become a slug
		// (else a dead ledger #anchor could false-pass against it).
		if (/^\s*(```|~~~)/.test(line)) {
			inFence = !inFence;
			continue;
		}
		if (inFence) continue;
		const m = /^#{1,6}\s+(.*)$/.exec(line);
		if (m) headings.push(slugify(m[1] ?? ""));
	}
	return { headings, sizeBytes: Buffer.byteLength(text, "utf8") };
}

if (import.meta.main) {
	const root =
		process.env.GATE_ROOT ??
		(await $`git rev-parse --show-toplevel`.nothrow().quiet().text()).trim();

	// Touch-coupling needs PR context (mirrors tools/spec-impact-gate). Absent
	// it (push, local `moon ci`), the changed set is empty and the leg no-ops;
	// the snapshot checks still run off GATE_ROOT.
	let changed: Changed = { files: [], body: null, headBranch: "" };
	const repo = process.env.REPO;
	const prNumber = process.env.PR_NUMBER;
	if (repo && prNumber) {
		try {
			const view =
				await $`timeout 30 gh pr view ${prNumber} --repo ${repo} --json headRefName,body`.json();
			const files =
				await $`timeout 60 gh api --paginate repos/${repo}/pulls/${prNumber}/files --jq .[].filename`.text();
			changed = {
				files: files.split("\n").filter((l) => l.length > 0),
				body: view.body,
				headBranch: view.headRefName,
			};
		} catch (error) {
			// Fail closed: a PR-context fetch failure must not silently skip
			// the touch-coupling leg.
			console.error(
				`design-ledger-gate: failed to fetch PR #${prNumber} changed set:`,
				error,
			);
			process.exit(2);
		}
	}

	const readTextReal = async (
		r: string,
		relPath: string,
	): Promise<string | null> => {
		const file = Bun.file(`${r}/${relPath}`);
		return (await file.exists()) ? await file.text() : null;
	};

	process.exit(
		await runOnce({
			root,
			readText: readTextReal,
			listRecordFiles: async (r) => {
				const glob = new Bun.Glob(`${PRODUCT_DIR}/**/*.md`);
				const out: string[] = [];
				for await (const rel of glob.scan({ cwd: r })) {
					const posix = rel.replaceAll("\\", "/");
					if (touchesRecord(posix)) out.push(posix);
				}
				return out.sort();
			},
			readRecord: (r, productRelPath) => {
				const path = `${r}/${PRODUCT_DIR}/${productRelPath}`;
				// Synchronous read (readRecord is sync); missing path → null.
				try {
					if (!existsSync(path)) return null;
					return recordContentFromText(readFileSync(path, "utf8"));
				} catch {
					return null;
				}
			},
			changed,
			log: (msg) => console.log(msg),
			err: (msg) => console.error(msg),
		}),
	);
}
