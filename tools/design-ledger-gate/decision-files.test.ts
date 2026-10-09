import { describe, expect, test } from "bun:test";
import {
	type DecisionRow,
	parseDecisionFile,
	renderDecisionIndex,
} from "./decision-files.ts";

const PATH = "docs/designs/decisions/meta/DL-297.md";
const GOOD = [
	"---",
	"id: DL-297",
	'decision: "Exactly two docs repos | one public"',
	'status: "Active (Matt, 2026-10-07)"',
	"record: ../../meta/public-docs-repo.md#approach",
	"---",
	"",
	"Optional rationale body.",
	"",
].join("\n");

function parsed(text: string, path = PATH): DecisionRow {
	const r = parseDecisionFile(path, text);
	if (!r.ok) throw new Error(r.error.reason);
	return r.row;
}

function rejected(text: string, path = PATH) {
	const r = parseDecisionFile(path, text);
	if (r.ok) throw new Error("expected a parse failure");
	return r.error;
}

const swap = (from: string, to: string) => GOOD.replace(from, to);

describe("parseDecisionFile", () => {
	test("a well-formed file yields every field, with the record resolved repo-relative", () => {
		expect(parsed(GOOD)).toEqual({
			path: PATH,
			line: 2,
			id: "DL-297",
			decision: "Exactly two docs repos | one public",
			status: "Active (Matt, 2026-10-07)",
			recordRaw: "../../meta/public-docs-repo.md#approach",
			recordPath: "docs/designs/meta/public-docs-repo.md",
			recordAnchor: "approach",
		});
	});

	test("ids past DL-999 parse, and may be the successor in a supersession", () => {
		const path = "docs/designs/decisions/meta/DL-1000.md";
		const row = parsed(
			swap("id: DL-297", "id: DL-1000").replace(
				"Active (Matt, 2026-10-07)",
				"Superseded by DL-1001 (Matt, 2026-10-07)",
			),
			path,
		);
		expect(row.id).toBe("DL-1000");
		expect(row.status).toBe("Superseded by DL-1001 (Matt, 2026-10-07)");
	});

	test("a BOM, escaped quotes and a record without an anchor are accepted", () => {
		const row = parsed(
			`\uFEFF${swap('"Exactly two docs repos | one public"', '"say \\"two\\""').replace("#approach", "")}`,
		);
		expect(row.decision).toBe('say "two"');
		expect(row.recordAnchor).toBeNull();
	});

	test("a non-ASCII heading slug is a valid anchor", () => {
		expect(parsed(swap("#approach", "#décision-2026")).recordAnchor).toBe(
			"décision-2026",
		);
	});

	test("Superseded and Retired statuses are accepted", () => {
		expect(
			parsed(swap("Active (Matt", "Superseded by DL-300 (Matt")).status,
		).toStartWith("Superseded by DL-300");
		expect(parsed(swap("Active (Matt", "Retired (Matt")).status).toStartWith(
			"Retired",
		);
	});

	test.each([
		["content before the block", `x\n${GOOD}`, 1, "must start"],
		[
			"an unclosed block",
			GOOD.replace(/\n---\n\nOptional/, "\n\nOptional"),
			1,
			"not closed",
		],
		[
			"an unterminated quote",
			swap('"Exactly two docs repos | one public"', '"unterminated'),
			3,
			"double-quoted",
		],
		[
			"a YAML-only \\x escape",
			swap("Exactly two", "\\x41"),
			3,
			"double-quoted",
		],
		["a literal tab", swap("Exactly two", "a\tb"), 3, "double-quoted"],
		["a decoded newline", swap("Exactly two", "a\\nb"), 3, "one line"],
		[
			"a decoded line separator",
			swap("Exactly two", "a\\u2028b"),
			3,
			"one line",
		],
		["an anchored record", swap("record: ", "record: &a "), 5, "unquoted"],
		["a tagged id", swap("id: DL-297", "id: !!str DL-297"), 2, "unquoted"],
		[
			"a trailing comment on the id",
			swap("id: DL-297", "id: DL-297 # c"),
			2,
			"not canonical",
		],
		["a duplicate key", swap("record:", "id: DL-297\nrecord:"), 2, "exactly"],
		["an extra key", swap("record:", "owner: x\nrecord:"), 2, "exactly"],
		[
			"a missing key",
			swap('status: "Active (Matt, 2026-10-07)"\n', ""),
			2,
			"exactly",
		],
		[
			"keys out of order",
			GOOD.replace(/(decision: .*)\n(status: .*)/, "$2\n$1"),
			3,
			"expected key `decision`",
		],
		[
			"an unquoted decision",
			swap('"Exactly two docs repos | one public"', "Exactly two"),
			3,
			"double-quoted",
		],
		[
			"a single-quoted status",
			swap('"Active (Matt, 2026-10-07)"', "'Active (Matt, 2026-10-07)'"),
			4,
			"double-quoted",
		],
		["a quoted id", swap("id: DL-297", 'id: "DL-297"'), 2, "unquoted"],
		[
			"a width-drifted id",
			swap("id: DL-297", "id: DL-0297"),
			2,
			"not canonical",
		],
		[
			"an id that disagrees with the file name",
			swap("id: DL-297", "id: DL-298"),
			2,
			"does not match",
		],
		[
			"an empty decision",
			swap('"Exactly two docs repos | one public"', '"  "'),
			3,
			"empty",
		],
		[
			"a status without provenance",
			swap('"Active (Matt, 2026-10-07)"', '"Active"'),
			4,
			"status",
		],
		[
			"a superseded target of the wrong width",
			swap("Active (Matt", "Superseded by DL-30 (Matt"),
			4,
			"status",
		],
		[
			"a record that is not markdown",
			swap("public-docs-repo.md", "public-docs-repo.txt"),
			5,
			".md",
		],
		[
			"an absolute record",
			swap("../../meta/public-docs-repo.md", "/docs/x.md"),
			5,
			"relative",
		],
		[
			"a URL record",
			swap("../../meta/public-docs-repo.md", "https://x/y.md"),
			5,
			"relative",
		],
		["a non-slug anchor", swap("#approach", "#Approach Two"), 5, "slug"],
		["an uppercase anchor", swap("#approach", "#Approach"), 5, "slug"],
		["a record with a pipe", swap("public-docs-repo", "a|b"), 5, "relative"],
		["a record with a paren", swap("public-docs-repo", "a)b"), 5, "relative"],
		["a record with a space", swap("public-docs-repo", "a b"), 5, "relative"],
		[
			"a record with a query",
			swap("../../meta/public-docs-repo", "?a"),
			5,
			"relative",
		],
		[
			"a nameless record",
			swap("../../meta/public-docs-repo.md", ".md"),
			5,
			"relative",
		],
		[
			"a record above the repo root",
			swap("../../", "../../../../../../"),
			5,
			"climbs",
		],
	])("rejects %s", (_name, text, line, reason) => {
		const error = rejected(text);
		expect(error.path).toBe(PATH);
		expect(error.line).toBe(line);
		expect(error.reason).toContain(reason);
	});

	test("CRLF input parses to the same row as LF", () => {
		expect(parsed(GOOD.replaceAll("\n", "\r\n"))).toEqual(parsed(GOOD));
	});
});

describe("renderDecisionIndex", () => {
	const row = (id: string, decision = `d ${id}`): DecisionRow => ({
		...parsed(
			swap("id: DL-297", `id: ${id}`),
			`docs/designs/decisions/meta/${id}.md`,
		),
		decision,
	});
	const cells = (md: string) =>
		[...Bun.markdown.html(md).matchAll(/<tr>(.*?)<\/tr>/gs)].map((m) =>
			[...(m[1] ?? "").matchAll(/<t[dh]>(.*?)<\/t[dh]>/gs)].map((c) => c[1]),
		);

	test("a backslash before a pipe stays inside its cell", () => {
		const body = cells(renderDecisionIndex([row("DL-297", "a \\| b")]))[1];
		expect(body).toHaveLength(4);
		expect(body?.[1]).toBe("a \\| b");
	});

	test("a backslash inside a code span is not doubled", () => {
		const body = cells(
			renderDecisionIndex([row("DL-297", "use `C:\\tmp` path")]),
		)[1];
		expect(body?.[1]).toBe("use <code>C:\\tmp</code> path");
	});

	test("sorts numerically, links each id to its file, and escapes pipes", () => {
		expect(
			renderDecisionIndex([
				row("DL-297", "a | b"),
				row("DL-030"),
				row("DL-100"),
			]),
		).toBe(
			[
				"| ID | Decision | Status | Record |",
				"|---|---|---|---|",
				"| [DL-030](DL-030.md) | d DL-030 | Active (Matt, 2026-10-07) | [../../meta/public-docs-repo.md#approach](../../meta/public-docs-repo.md#approach) |",
				"| [DL-100](DL-100.md) | d DL-100 | Active (Matt, 2026-10-07) | [../../meta/public-docs-repo.md#approach](../../meta/public-docs-repo.md#approach) |",
				"| [DL-297](DL-297.md) | a \\| b | Active (Matt, 2026-10-07) | [../../meta/public-docs-repo.md#approach](../../meta/public-docs-repo.md#approach) |",
				"",
			].join("\n"),
		);
	});

	test("output is independent of input order", () => {
		const rows = [row("DL-005"), row("DL-150"), row("DL-042")];
		expect(renderDecisionIndex(rows)).toBe(
			renderDecisionIndex([...rows].reverse()),
		);
	});

	test("orders by id across areas, not by path", () => {
		const late = {
			...row("DL-001"),
			path: "docs/designs/decisions/zeta/DL-001.md",
		};
		const early = {
			...row("DL-002"),
			path: "docs/designs/decisions/alpha/DL-002.md",
		};
		const lines = renderDecisionIndex([early, late]).split("\n");
		expect(lines[2]).toStartWith("| [DL-001]");
		expect(lines[3]).toStartWith("| [DL-002]");
	});
});
