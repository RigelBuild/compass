import { posix as pathPosix } from "node:path";

/** A decision file that parsed cleanly. */
export interface DecisionRow {
	/** Repo-relative decision-file path. */
	path: string;
	/** 1-based physical line of the `id:` key. */
	line: number;
	id: string;
	decision: string;
	status: string;
	/** `record` exactly as written, relative to the decision file. */
	recordRaw: string;
	/** Repo-relative record path, without the anchor. */
	recordPath: string;
	recordAnchor: string | null;
}

/** A decision file the parser rejected. */
export interface MalformedDecision {
	path: string;
	/** 1-based physical line of the fault. */
	line: number;
	reason: string;
}

export type DecisionParse =
	| { ok: true; row: DecisionRow }
	| { ok: false; error: MalformedDecision };

const KEYS = ["id", "decision", "status", "record"] as const;
const ID_RE = /^DL-(?:\d{3}|[1-9]\d{3,})$/;
const FILE_RE = /^(DL-(?:\d{3}|[1-9]\d{3,}))\.md$/;
const STATUS_RES = [
	/^Active \(.+, \d{4}-\d{2}-\d{2}\)$/,
	/^Superseded by DL-(?:\d{3}|[1-9]\d{3,}) \(.+, \d{4}-\d{2}-\d{2}\)$/,
	/^Retired \(.+, \d{4}-\d{2}-\d{2}\)$/,
];
// One-line double-quoted string limited to escapes YAML and JSON decode alike.
const QUOTED_RE =
	/^"(?:[^"\\\p{Cc}\u2028\u2029]|\\["\\/bfnrt]|\\u[0-9A-Fa-f]{4})*"$/u;
const LINE_BREAK_RE = /[\p{Cc}\u2028\u2029]/u;
// Characters that survive as a bare Markdown link destination and a table cell.
const RECORD_RE =
	/^(?:[A-Za-z0-9_-][A-Za-z0-9._-]*\/|\.\.?\/)*[A-Za-z0-9_-][A-Za-z0-9._-]*\.md$/;
// A heading slug: lowercase letters and digits in any script, `_` and `-`.
const ANCHOR_RE = /^[\p{Ll}\p{Nd}_-]+$/u;

/**
 * Parse one `docs/designs/decisions/<area>/DL-NNN.md` file. The
 * front matter is exactly four keys in fixed order; a Markdown body may follow.
 */
export function parseDecisionFile(path: string, text: string): DecisionParse {
	const fail = (line: number, reason: string): DecisionParse => ({
		ok: false,
		error: { path, line, reason },
	});
	const lines = text.replace(/^\uFEFF/, "").split("\n");
	if (lines[0]?.trimEnd() !== "---") {
		return fail(1, "file must start with a `---` front-matter block");
	}
	const close = lines.findIndex((l, i) => i > 0 && l.trimEnd() === "---");
	if (close === -1)
		return fail(1, "front-matter block is not closed with `---`");
	const block = lines.slice(1, close);
	if (block.length !== KEYS.length) {
		return fail(2, `front matter must hold exactly ${KEYS.join(", ")}`);
	}

	const values: Record<string, string> = {};
	for (const [i, key] of KEYS.entries()) {
		const lineNo = i + 2;
		const m = /^([A-Za-z_]+): (.*)$/.exec((block[i] ?? "").trimEnd());
		if (!m || m[1] !== key) {
			return fail(lineNo, `expected key \`${key}\` at line ${lineNo}`);
		}
		const raw = m[2] ?? "";
		if (key === "decision" || key === "status") {
			if (!QUOTED_RE.test(raw))
				return fail(
					lineNo,
					`\`${key}\` must be a one-line double-quoted string`,
				);
			const value: string = JSON.parse(raw);
			if (LINE_BREAK_RE.test(value)) {
				return fail(lineNo, `\`${key}\` must be one line`);
			}
			values[key] = value;
		} else {
			if (raw === "" || /^["'[{|>&*!%@`#]/.test(raw)) {
				return fail(lineNo, `\`${key}\` must be a plain unquoted value`);
			}
			values[key] = raw;
		}
	}
	const id = values.id ?? "";
	if (!ID_RE.test(id)) return fail(2, `id \`${id}\` is not canonical DL-NNN`);
	const fileId = FILE_RE.exec(pathPosix.basename(path))?.[1];
	if (fileId !== id)
		return fail(
			2,
			`id \`${id}\` does not match file name \`${pathPosix.basename(path)}\``,
		);
	const decision = values.decision ?? "";
	if (decision.trim() === "") return fail(3, "`decision` is empty");
	const status = values.status ?? "";
	if (!STATUS_RES.some((re) => re.test(status))) {
		return fail(
			4,
			`status \`${status}\` is not Active, Superseded by DL-NNN, or Retired with (<who>, YYYY-MM-DD)`,
		);
	}

	const recordRaw = values.record ?? "";
	const hash = recordRaw.indexOf("#");
	const target = hash === -1 ? recordRaw : recordRaw.slice(0, hash);
	const anchor = hash === -1 ? null : recordRaw.slice(hash + 1);
	if (!RECORD_RE.test(target)) {
		return fail(
			5,
			"`record` must be a relative `.md` path of letters, digits, `.`, `_`, `-` and `/`",
		);
	}
	if (anchor !== null && !ANCHOR_RE.test(anchor)) {
		return fail(5, `record anchor \`#${anchor}\` is not a heading slug`);
	}
	const recordPath = pathPosix.normalize(
		pathPosix.join(pathPosix.dirname(path), target),
	);
	if (recordPath.startsWith(".."))
		return fail(5, "`record` climbs above the repository root");
	// The four lines already passed; the whole-block parse catches what only YAML sees.
	try {
		Bun.YAML.parse(block.join("\n"));
	} catch (error) {
		return fail(2, `malformed YAML: ${String(error)}`);
	}

	return {
		ok: true,
		row: {
			path,
			line: 2,
			id,
			decision,
			status,
			recordRaw,
			recordPath,
			recordAnchor: anchor,
		},
	};
}

/**
 * Render one area's index table. It lives beside the area's decision files, so
 * the id links to the file name and Record reuses the stored relative path.
 */
export function renderDecisionIndex(rows: readonly DecisionRow[]): string {
	// Double only backslashes that would escape the pipe escape; code spans keep theirs.
	const cell = (s: string) =>
		s.replace(/\\+(?=\|)/g, (m) => m + m).replaceAll("|", "\\|");
	const sorted = [...rows].sort(
		(a, b) =>
			Number(a.id.slice(3)) - Number(b.id.slice(3)) ||
			a.path.localeCompare(b.path),
	);
	const out = ["| ID | Decision | Status | Record |", "|---|---|---|---|"];
	for (const r of sorted) {
		out.push(
			`| [${r.id}](${pathPosix.basename(r.path)}) | ${cell(r.decision)} | ${cell(r.status)} | [${cell(r.recordRaw)}](${r.recordRaw}) |`,
		);
	}
	return `${out.join("\n")}\n`;
}
