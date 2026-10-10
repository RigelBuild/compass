// design-ledger-gate: validate docs/designs/decisions/<area>/DL-NNN.md and the
// records they cite; PR runs also check touch coupling (needs REPO, PR_NUMBER).
// GATE_ROOT overrides the scanned root. Exit 0 pass, 1 violations, 2 usage error.
import { existsSync, readFileSync } from "node:fs";
import { posix as pathPosix } from "node:path";
import { $ } from "bun";
import { type DecisionRow, parseDecisionFile } from "./decision-files.ts";

/** The design-corpus root the gate governs (all buckets beneath it). */
export const DESIGNS_ROOT = "docs/designs";
/** Every record lives under exactly one of these governed buckets. */
export const GOVERNED_ROOTS: readonly string[] = [
	"ui",
	"agent",
	"server",
	"meta",
	"infra",
	"observability",
	"repo",
	"platform",
];
/** A Record link into a record larger than this MUST carry a resolving anchor. */
export const LARGE_RECORD_BYTES = 50 * 1024;

/** Records that may legitimately carry `Status: Historical`. */
export const HISTORICAL_CHAIN: Record<string, true> = {};

/** A non-empty `Ledger-impact:` declaration exempts the touch-coupling leg. */
const LEDGER_IMPACT_RE = /^\s*>?\s*ledger-impact:\s*(\S.*)$/im;

/**
 * Automation branches cannot author a `Ledger-impact:` declaration. Each prefix
 * is exempt only for PRs its bot opened from this repo; a spoofed branch fails.
 */
export const EXEMPT_BRANCHES: ReadonlyArray<{
	prefix: string;
	author: string;
}> = [
	{ prefix: "renovate/", author: "app/rigelbuild-renovate" },
	{ prefix: "trunk-merge/", author: "app/trunk-io" },
];

/** The record-level Status grammar is reject-by-default. */
const STATUS_RE = /^Status:\s*(Historical|Superseded\s+by\s+(\S+))$/i;
/** A decision status that names a successor. */
const ROW_SUPERSEDED_RE =
	/^Superseded by (DL-(?:\d{3}|[1-9]\d{3,})) \(.+, \d{4}-\d{2}-\d{2}\)$/;

/** The physical lines of the four decision front-matter keys. */
export const KEY_LINE = { id: 2, decision: 3, status: 4, record: 5 } as const;

/** What a discovered path under docs/designs means to the gate. */
export type DesignPathKind =
	| "decision"
	| "decisions-readme"
	| "legacy-ledger"
	| "misplaced"
	| "other";

/** One rejected decision file or forbidden layout path. */
export interface StrayPath {
	path: string;
	kind: "legacy-ledger" | "misplaced";
}

/** The parsed decisions, malformed files, and forbidden paths in one tree. */
export interface DecisionCorpus {
	rows: DecisionRow[];
	malformed: Array<{ path: string; line: number; reason: string }>;
	strays: StrayPath[];
}

/** One record's `Status:` header slot. */
export interface RecordHeader {
	path: string;
	statusLine: string | null;
	line: number;
}

/** The diff-aware input for the touch-coupling leg. */
export interface Changed {
	files: string[];
	body: string | null;
	headBranch: string;
	/** PR author login as `gh pr view` reports it (`app/<slug>` for apps). */
	author: string;
	/** True when the head branch lives in a fork. */
	crossRepository: boolean;
}

/** What `readRecord` returns for a link/pointer target. */
export interface RecordContent {
	headings: string[];
	sizeBytes: number;
}

/** A single gate failure. `line` 0 marks a non-line-specific locus. */
export interface Violation {
	file: string;
	line: number;
	message: string;
}

/** The surviving record-level `Status:` values. */
export type StatusValue =
	| { kind: "Historical" }
	| { kind: "Superseded"; path: string };

/** Classify one path from the single docs/designs discovery listing. */
export function classifyDesignPath(file: string): DesignPathKind {
	const prefix = `${DESIGNS_ROOT}/`;
	if (!file.startsWith(prefix)) return "other";
	const base = pathPosix.basename(file);
	if (base === "DECISIONS.md") return "legacy-ledger";
	if (file === `${DESIGNS_ROOT}/decisions/README.md`) return "decisions-readme";
	const decisionsPrefix = `${DESIGNS_ROOT}/decisions/`;
	if (file.startsWith(decisionsPrefix)) {
		const parts = file.slice(decisionsPrefix.length).split("/");
		return parts.length === 2 && parts[0] !== "" && /^DL-.*\.md$/.test(base)
			? "decision"
			: "misplaced";
	}
	return /^DL-\d+\.md$/.test(base) ? "misplaced" : "other";
}

/** Build the decision corpus from already-classified paths and file contents. */
export function buildDecisionCorpus(
	files: readonly { path: string; text: string }[],
	strays: readonly StrayPath[] = [],
): DecisionCorpus {
	const rows: DecisionRow[] = [];
	const malformed: DecisionCorpus["malformed"] = [];
	for (const file of files) {
		const parsed = parseDecisionFile(file.path, file.text);
		if (parsed.ok) rows.push(parsed.row);
		else malformed.push(parsed.error);
	}
	return { rows, malformed, strays: [...strays] };
}

/** GitHub-style heading slug used to resolve record anchors. */
export function slugify(heading: string): string {
	return heading
		.trim()
		.toLowerCase()
		.replace(/[^\w\s-]/gu, "")
		.replace(/\s/g, "-");
}

export function parseStatusValue(statusLine: string): StatusValue | null {
	let value = statusLine.trim();
	value = value.replace(/^\s*(?:>\s*)+/, "");
	value = value.replace(/^\*\*\s*/, "").replace(/\s*\*\*\s*$/, "");
	const match = STATUS_RE.exec(value);
	if (!match) return null;
	if (match[1]?.toLowerCase() === "historical") return { kind: "Historical" };
	const pointer = match[2];
	return pointer === undefined ? null : { kind: "Superseded", path: pointer };
}

/**
 * True when a repo-relative path is a governed design record: any `.md` at any
 * depth under a governed root. Matching by filename shape let new document kinds
 * (nested amendments, milestone records) pass the gate unchecked.
 */
export function touchesRecord(file: string): boolean {
	if (!file.endsWith(".md")) return false;
	return GOVERNED_ROOTS.some((root) =>
		file.startsWith(`${DESIGNS_ROOT}/${root}/`),
	);
}

/** Resolve a record-relative supersession pointer under docs/designs. */
export function resolveRecordRelative(
	recordRelPath: string,
	pointer: string,
): string | null {
	const joined = pathPosix.join(pathPosix.dirname(recordRelPath), pointer);
	if (joined.startsWith("..")) return null;
	return joined;
}

/** Unresolved merge markers in a governed file, excluding fenced examples. */
export function conflictMarkerViolations(
	file: string,
	text: string,
): Violation[] {
	const out: Violation[] = [];
	let inFence = false;
	text.split("\n").forEach((line, i) => {
		if (/^\s*(```|~~~)/.test(line)) inFence = !inFence;
		if (inFence) return;
		const marker = /^(<{7,}|>{7,}|%{7,}|\+{7,}|={7})(\s|$)/.exec(line);
		if (marker) {
			out.push({
				file,
				line: i + 1,
				message: `unresolved merge conflict marker: ${marker[1]}`,
			});
		}
	});
	return out;
}

/** Parse Status anywhere in the header zone, skipping fenced examples. */
export function parseRecordHeader(path: string, text: string): RecordHeader {
	const lines = text.split("\n");
	let h1 = -1;
	let inFence = false;
	for (let i = 0; i < lines.length; i++) {
		const line = lines[i] ?? "";
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
	inFence = false;
	for (let i = h1 + 1; i < lines.length; i++) {
		const raw = lines[i] ?? "";
		if (/^\s*(```|~~~)/.test(raw)) {
			inFence = !inFence;
			continue;
		}
		if (inFence) continue;
		if (/^##\s/.test(raw)) break;
		if (/^\s*(?:>\s*)*(?:\*\*)?Status:/i.test(raw)) {
			return { path, statusLine: raw.trimEnd(), line: i + 1 };
		}
	}
	return { path, statusLine: null, line: h1 + 2 };
}

/** Evaluate decision integrity, record headers, completeness, and PR coupling. */
export function evaluate(
	corpus: DecisionCorpus,
	records: RecordHeader[],
	changed: Changed,
	readRecord: (repoRelPath: string) => RecordContent | null,
): Violation[] {
	const violations: Violation[] = [];
	const v = (file: string, line: number, message: string) =>
		violations.push({ file, line, message });
	const { rows } = corpus;

	for (const stray of corpus.strays) {
		v(
			stray.path,
			0,
			stray.kind === "legacy-ledger"
				? "DECISIONS.md is retired: record each decision as docs/designs/decisions/<area>/DL-NNN.md"
				: "misplaced design file: only docs/designs/decisions/README.md or docs/designs/decisions/<area>/DL-*.md may be under the decisions tree; DL-<n>.md files must be in that layout",
		);
	}
	for (const malformed of corpus.malformed) {
		v(
			malformed.path,
			malformed.line,
			`malformed decision file: ${malformed.reason}`,
		);
	}
	if (rows.length === 0) {
		v(`${DESIGNS_ROOT}/decisions`, 0, "no valid decision files were found");
	}

	const byId = new Map<string, DecisionRow>();
	for (const row of rows) {
		const first = byId.get(row.id);
		if (first === undefined) byId.set(row.id, row);
		else {
			v(
				row.path,
				KEY_LINE.id,
				`${row.id}: duplicate decision id (also defined in ${first.path})`,
			);
		}
	}

	for (const row of rows) {
		const decisionArea = row.path.startsWith(`${DESIGNS_ROOT}/decisions/`)
			? row.path.slice(`${DESIGNS_ROOT}/decisions/`.length).split("/")[0]
			: undefined;
		const recordArea = row.recordPath.startsWith(`${DESIGNS_ROOT}/`)
			? row.recordPath.slice(`${DESIGNS_ROOT}/`.length).split("/")[0]
			: undefined;
		if (
			recordArea === undefined ||
			recordArea === "" ||
			recordArea !== decisionArea
		) {
			v(
				row.path,
				KEY_LINE.record,
				`${row.id}: decision area must match the top-level docs/designs directory of its Record path (${row.recordPath})`,
			);
		}

		const target = readRecord(row.recordPath);
		if (target === null) {
			v(
				row.path,
				KEY_LINE.record,
				`${row.id}: Record link path does not resolve: ${row.recordRaw}`,
			);
		} else if (row.recordAnchor !== null) {
			if (!target.headings.includes(row.recordAnchor)) {
				v(
					row.path,
					KEY_LINE.record,
					`${row.id}: Record link #anchor not found in ${row.recordRaw.split("#")[0]}: #${row.recordAnchor}`,
				);
			}
		} else if (target.sizeBytes > LARGE_RECORD_BYTES) {
			v(
				row.path,
				KEY_LINE.record,
				`${row.id}: Record link into a large record (>${Math.floor(LARGE_RECORD_BYTES / 1024)} KB) must carry a #anchor: ${row.recordRaw}`,
			);
		}

		const supersession = ROW_SUPERSEDED_RE.exec(row.status);
		if (supersession === null) continue;
		const targetId = supersession[1] ?? "";
		if (targetId === row.id) {
			v(row.path, KEY_LINE.status, `${row.id}: superseded by itself`);
		} else if (!byId.has(targetId)) {
			v(
				row.path,
				KEY_LINE.status,
				`${row.id}: Superseded by ${targetId}, which is not a decision file`,
			);
		}
	}

	const cyclesReported = new Set<string>();
	for (const start of byId.values()) {
		if (!ROW_SUPERSEDED_RE.test(start.status)) continue;
		const walk: DecisionRow[] = [];
		let current: DecisionRow | undefined = start;
		while (current !== undefined) {
			const node: DecisionRow = current;
			const index = walk.findIndex((candidate) => candidate.id === node.id);
			if (index !== -1) {
				const cycle = walk.slice(index);
				const cycleKey = cycle
					.map((row) => row.id)
					.sort()
					.join("|");
				if (cycle.length > 1 && !cyclesReported.has(cycleKey)) {
					cyclesReported.add(cycleKey);
					const anchor = cycle.reduce((a, b) => (b.id < a.id ? b : a));
					v(
						anchor.path,
						KEY_LINE.status,
						`supersession cycle: ${cycle.map((row) => row.id).join(" → ")} → ${node.id}`,
					);
				}
				break;
			}
			walk.push(node);
			const next: string | undefined = ROW_SUPERSEDED_RE.exec(node.status)?.[1];
			current = next === undefined ? undefined : byId.get(next);
		}
	}

	const decisionAreas = new Set(
		rows.map(
			(row) =>
				row.path.slice(`${DESIGNS_ROOT}/decisions/`.length).split("/")[0] ?? "",
		),
	);
	for (const root of GOVERNED_ROOTS) {
		const hasRecords = records.some((record) =>
			record.path.startsWith(`${DESIGNS_ROOT}/${root}/`),
		);
		if (hasRecords && !decisionAreas.has(root)) {
			v(
				`${DESIGNS_ROOT}/decisions/${root}`,
				0,
				`docs/designs/${root}/ has design records but no valid decision file in docs/designs/decisions/${root}/`,
			);
		}
	}

	const rowByRecord = new Map<string, DecisionRow>();
	for (const row of rows) {
		if (row.recordAnchor === null) rowByRecord.set(row.recordPath, row);
	}
	for (const record of records) {
		if (record.statusLine === null) continue;
		const value = parseStatusValue(record.statusLine);
		if (value === null) {
			v(
				record.path,
				record.line,
				"malformed or prohibited `Status:` header (only `Historical` or resolving `Superseded by <path>` is allowed)",
			);
			continue;
		}
		if (value.kind === "Historical" && !(record.path in HISTORICAL_CHAIN)) {
			v(
				record.path,
				record.line,
				"`Status: Historical` but the record is not in the version-narrative chain",
			);
		}
		if (value.kind === "Superseded") {
			const recordDesignsRel = record.path.startsWith(`${DESIGNS_ROOT}/`)
				? record.path.slice(DESIGNS_ROOT.length + 1)
				: record.path;
			const resolved = resolveRecordRelative(recordDesignsRel, value.path);
			const targetPath =
				resolved === null ? null : `${DESIGNS_ROOT}/${resolved}`;
			if (targetPath === null || readRecord(targetPath) === null) {
				v(
					record.path,
					record.line,
					`Status supersession does not resolve to a record: ${value.path}`,
				);
			}
			const linkedDecision = rowByRecord.get(record.path);
			if (
				linkedDecision !== undefined &&
				!ROW_SUPERSEDED_RE.test(linkedDecision.status)
			) {
				v(
					record.path,
					record.line,
					"record Status supersession disagrees with its decision status",
				);
			}
		}
	}

	const exemptBranch =
		!changed.crossRepository &&
		EXEMPT_BRANCHES.some(
			({ prefix, author }) =>
				changed.headBranch.startsWith(prefix) && changed.author === author,
		);
	const declared = LEDGER_IMPACT_RE.test(changed.body ?? "");
	const touchedRecord = !exemptBranch && changed.files.some(touchesRecord);
	const touchedDecision = changed.files.some(
		(file) => classifyDesignPath(file) === "decision",
	);
	if (touchedRecord && !touchedDecision && !declared) {
		v(
			"(pull request)",
			0,
			"PR touches a governed design record without a changed decision file or Ledger-impact declaration",
		);
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
	/** List every file under docs/designs in one discovery pass. */
	listDesignFiles: (root: string) => Promise<string[]>;
	/** Resolve a repo-relative record path to its slugs + bytes. */
	readRecord: (root: string, repoRelPath: string) => RecordContent | null;
	changed: Changed;
	log: (msg: string) => void;
	err: (msg: string) => void;
}

export async function runOnce(deps: Deps): Promise<number> {
	const { root, readText, listDesignFiles, readRecord, changed, log, err } =
		deps;
	let paths: string[];
	try {
		paths = await listDesignFiles(root);
	} catch (error) {
		err(`design-ledger-gate: cannot read the tree at ${root}`);
		err(error instanceof Error ? error.message : String(error));
		return 2;
	}

	const decisionPaths: string[] = [];
	const recordPaths: string[] = [];
	const strays: StrayPath[] = [];
	for (const path of paths) {
		const kind = classifyDesignPath(path);
		if (kind === "decision") decisionPaths.push(path);
		if (kind === "legacy-ledger" || kind === "misplaced") {
			strays.push({ path, kind });
		}
		if (touchesRecord(path)) recordPaths.push(path);
	}
	const decisionPathSet = new Set(decisionPaths);
	const recordPathSet = new Set(recordPaths);

	const decisionFiles: Array<{ path: string; text: string }> = [];
	const records: RecordHeader[] = [];
	const violations: Violation[] = [];
	try {
		for (const path of [
			...new Set([...decisionPaths, ...recordPaths]),
		].sort()) {
			const text = await readText(root, path);
			if (text === null) continue;
			if (decisionPathSet.has(path)) {
				decisionFiles.push({ path, text });
				violations.push(...conflictMarkerViolations(path, text));
			}
			if (recordPathSet.has(path)) {
				records.push(parseRecordHeader(path, text));
				violations.push(...conflictMarkerViolations(path, text));
			}
		}
	} catch (error) {
		err(`design-ledger-gate: cannot read the tree at ${root}`);
		err(error instanceof Error ? error.message : String(error));
		return 2;
	}

	const corpus = buildDecisionCorpus(decisionFiles, strays);
	violations.push(
		...evaluate(corpus, records, changed, (path) => readRecord(root, path)),
	);
	if (violations.length === 0) {
		log(
			`design-ledger-gate: OK — ${corpus.rows.length} decision file(s), ${records.length} record(s) status-checked.`,
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
	err("See docs/designs/meta/compass-design-ledger/design.md.");
	return 1;
}

/** Whether the touch-coupling leg runs, and against which PR. */
export type PrContext =
	| { kind: "pr"; repo: string; prNumber: string }
	| { kind: "skip" }
	| { kind: "error"; message: string };

/** Require PR coordinates whenever a pull-request event or PR number is present. */
export function prContextFrom(
	env: Readonly<Record<string, string | undefined>>,
): PrContext {
	const repo = env.REPO ?? "";
	const prNumber = env.PR_NUMBER ?? "";
	if (repo !== "" && /^[1-9][0-9]*$/.test(prNumber)) {
		return { kind: "pr", repo, prNumber };
	}
	if (env.GITHUB_EVENT_NAME === "pull_request" || prNumber !== "") {
		return {
			kind: "error",
			message: `the touch-coupling leg needs REPO and a numeric PR_NUMBER (event ${JSON.stringify(env.GITHUB_EVENT_NAME ?? "")}, got REPO=${JSON.stringify(repo)}, PR_NUMBER=${JSON.stringify(prNumber)})`,
		};
	}
	return { kind: "skip" };
}

/** Compute a target record's heading slugs and byte size from its text. */
export function recordContentFromText(text: string): RecordContent {
	const headings: string[] = [];
	let inFence = false;
	for (const line of text.split("\n")) {
		if (/^\s*(```|~~~)/.test(line)) {
			inFence = !inFence;
			continue;
		}
		if (inFence) continue;
		const heading = /^#{1,6}\s+(.*)$/.exec(line);
		if (heading) headings.push(slugify(heading[1] ?? ""));
	}
	return { headings, sizeBytes: Buffer.byteLength(text, "utf8") };
}

if (import.meta.main) {
	const root =
		process.env.GATE_ROOT ??
		(await $`git rev-parse --show-toplevel`.nothrow().quiet().text()).trim();
	let changed: Changed = {
		files: [],
		body: null,
		headBranch: "",
		author: "",
		crossRepository: false,
	};
	const ctx = prContextFrom(process.env);
	if (ctx.kind === "error") {
		console.error(`design-ledger-gate: ${ctx.message}`);
		process.exit(2);
	}
	if (ctx.kind === "pr") {
		const { repo, prNumber } = ctx;
		try {
			const view =
				await $`timeout 30 gh pr view ${prNumber} --repo ${repo} --json headRefName,body,author,isCrossRepository`.json();
			if (
				typeof view.author?.login !== "string" ||
				typeof view.isCrossRepository !== "boolean"
			) {
				throw new Error("gh pr view omitted author.login or isCrossRepository");
			}
			const files =
				await $`timeout 60 gh api --paginate repos/${repo}/pulls/${prNumber}/files --jq .[].filename`.text();
			changed = {
				files: files.split("\n").filter((line) => line.length > 0),
				body: view.body,
				headBranch: view.headRefName,
				author: view.author.login,
				crossRepository: view.isCrossRepository,
			};
		} catch (error) {
			console.error(
				`design-ledger-gate: failed to fetch PR #${prNumber} changed set:`,
				error,
			);
			process.exit(2);
		}
	}

	const readTextReal = async (
		workspaceRoot: string,
		relPath: string,
	): Promise<string | null> => {
		const file = Bun.file(`${workspaceRoot}/${relPath}`);
		return (await file.exists()) ? await file.text() : null;
	};

	process.exit(
		await runOnce({
			root,
			readText: readTextReal,
			listDesignFiles: async (workspaceRoot) => {
				const glob = new Bun.Glob(`${DESIGNS_ROOT}/**`);
				const out: string[] = [];
				for await (const path of glob.scan({
					cwd: workspaceRoot,
					onlyFiles: true,
				})) {
					out.push(path.replaceAll("\\", "/"));
				}
				return out.sort();
			},
			readRecord: (workspaceRoot, repoRelPath) => {
				const file = `${workspaceRoot}/${repoRelPath}`;
				try {
					if (!existsSync(file)) return null;
					return recordContentFromText(readFileSync(file, "utf8"));
				} catch {
					return null;
				}
			},
			changed,
			log: (message) => console.log(message),
			err: (message) => console.error(message),
		}),
	);
}
