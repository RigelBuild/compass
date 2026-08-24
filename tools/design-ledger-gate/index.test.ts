// Unit tests for the design-ledger gate's pure core + I/O wiring (index.ts).
//
// This gate is a CI oracle: its correctness defines whether the ledger (T1)
// and per-record `Status:` headers (T2) are accepted, so this suite defends the
// machine-readable contract exhaustively.
//
// Conventions (mirroring tools/spec-impact-gate/gate.test.ts and
// tools/no-bash-gate/index.test.ts):
// - Literal paths (`docs/designs/<bucket>/...`), NOT values derived from the
//   module constants (DESIGNS_ROOT / GOVERNED_ROOTS / DECISIONS_PATH /
//   HISTORICAL_CHAIN / LARGE_RECORD_BYTES): those constants ARE the thing under
//   test, so deriving inputs from them would let a drifted constant pass silently.
// - `row()` / `header()` yield valid baselines so each test perturbs one axis.
// - `.message` is human prose, asserted only by its identifying substring.

import { describe, expect, test } from "bun:test";
import {
	type Changed,
	type Deps,
	evaluate,
	HISTORICAL_CHAIN,
	type LedgerRow,
	parseLedger,
	parseRecordHeader,
	parseStatusValue,
	type RecordContent,
	type RecordHeader,
	recordContentFromText,
	resolveRecordRelative,
	runOnce,
	slugify,
	splitLink,
	touchesRecord,
} from "./index.ts";

const LEDGER = "docs/designs/DECISIONS.md";
const smallRecord = (): RecordContent => ({ headings: [], sizeBytes: 100 });
const noChange: Changed = { files: [], body: null, headBranch: "" };

/**
 * A PR-event `Changed` for touch-coupling tests. `headBranch` defaults to a
 * plain feature branch (non-exempt); pass an automation prefix to exercise the
 * exemption.
 */
const changed = (
	files: string[],
	body: string | null,
	headBranch = "flinders-feature",
): Changed => ({ files, body, headBranch });

// ---------------------------------------------------------------------------
// Pure helpers.
// ---------------------------------------------------------------------------

describe("slugify", () => {
	// GitHub slug: lowercase, strip punctuation, each whitespace CHAR → one
	// hyphen (no run-collapse), so " / " and " — " each yield "--".
	test("punctuation stripped, each space → a hyphen (no collapse)", () => {
		expect(slugify("Problem / Intent")).toBe("problem--intent");
	});
	test("plain heading", () => {
		expect(slugify("Approach")).toBe("approach");
	});
	test("em-dash stripped, surrounding spaces each become a hyphen", () => {
		expect(slugify("T3 — the gate")).toBe("t3--the-gate");
	});
});

describe("parseStatusValue", () => {
	test("Draft / Active / Historical map to their kinds", () => {
		expect(parseStatusValue("Status: Draft")).toEqual({ kind: "Draft" });
		expect(parseStatusValue("Status: Active")).toEqual({ kind: "Active" });
		expect(parseStatusValue("Status: Historical")).toEqual({
			kind: "Historical",
		});
	});
	test("Superseded captures the path", () => {
		expect(
			parseStatusValue("Status: Superseded by ../compass-0.8/design.md"),
		).toEqual({ kind: "Superseded", path: "../compass-0.8/design.md" });
	});
	test("trailing prose after the keyword → null", () => {
		expect(parseStatusValue("Status: Draft (freezes on merge).")).toBeNull();
	});
	test("lowercase keyword → null", () => {
		expect(parseStatusValue("Status: draft")).toBeNull();
	});
	test("a lone trailing space is trimEnd'd → Active passes", () => {
		expect(parseStatusValue("Status: Active ")).toEqual({ kind: "Active" });
	});
});

describe("splitLink", () => {
	test("link with no anchor", () => {
		expect(splitLink("[x](a/b.md)")).toEqual({ path: "a/b.md", anchor: null });
	});
	test("link with #anchor", () => {
		expect(splitLink("[x](a/b.md#foo-bar)")).toEqual({
			path: "a/b.md",
			anchor: "foo-bar",
		});
	});
	test("non-link cell → null", () => {
		expect(splitLink("no link")).toBeNull();
	});
});

describe("touchesRecord", () => {
	test("a <name>/design.md product record is a record", () => {
		expect(touchesRecord("docs/designs/product/compass-0.6/design.md")).toBe(
			true,
		);
	});
	test("a top-level <name>.md product record is a record", () => {
		expect(touchesRecord("docs/designs/product/compass-tauri-shell.md")).toBe(
			true,
		);
	});
	test("a record under a second governed root (agent) is a record", () => {
		expect(touchesRecord("docs/designs/agent/compass-x/design.md")).toBe(true);
	});
	test("a flat <name>.md at a second governed root is a record", () => {
		expect(touchesRecord("docs/designs/repo/compass-drop-proto.md")).toBe(true);
	});
	test("the ledger DECISIONS.md is NOT a record", () => {
		expect(touchesRecord("docs/designs/DECISIONS.md")).toBe(false);
	});
	test("a nested non-design.md file is not a record", () => {
		expect(touchesRecord("docs/designs/product/foo/bar.md")).toBe(false);
	});
	test("a flat .md inside a subgroup is NOT a record (governed at root only)", () => {
		expect(touchesRecord("docs/designs/infra/ci/foo.md")).toBe(false);
	});
	test("a file under an ungoverned bucket is not a record", () => {
		expect(touchesRecord("docs/designs/platform/x.md")).toBe(false);
	});
	test("a file under a non-bucket path is not a record", () => {
		expect(touchesRecord("docs/designs/notabucket/x.md")).toBe(false);
	});
	test("a non-markdown product file is not a record", () => {
		expect(touchesRecord("docs/designs/product/notes.txt")).toBe(false);
	});
});

describe("resolveRecordRelative", () => {
	test("a nested record's `../sibling` pointer → designs-root-relative sibling", () => {
		expect(
			resolveRecordRelative(
				"product/compass-0.6/design.md",
				"../compass-0.8/design.md",
			),
		).toBe("product/compass-0.8/design.md");
	});
	test("a cross-bucket pointer resolves inside DESIGNS_ROOT", () => {
		// A ui/ record superseded by an agent/ record: `../../agent/...` from
		// `ui/<name>/design.md` climbs to the designs root then into agent/.
		expect(
			resolveRecordRelative(
				"ui/compass-tauri-shell/design.md",
				"../../agent/compass-native-app/design.md",
			),
		).toBe("agent/compass-native-app/design.md");
	});
	test("a top-level record's bare pointer → that designs-root-relative path", () => {
		expect(resolveRecordRelative("a.md", "b.md")).toBe("b.md");
	});
	test("a pointer that climbs out of DESIGNS_ROOT → null", () => {
		expect(resolveRecordRelative("a.md", "../../escape.md")).toBeNull();
	});
});

describe("parseLedger", () => {
	const text = [
		"# Ledger", // 1
		"", // 2
		"| ID | Decision | Status | Record |", // 3 header — first cell not DL-\d
		"| --- | --- | --- | --- |", // 4 separator — first cell not DL-\d
		"| DL-001 | use X | Active (Matt, 2026-07-22) | [r](a.md) |", // 5
		"| DL-002 | use Y | Historical | [r](b.md) |", // 6
		"| DL-x | a | b |", // 7 — only 3 cells, skipped
		"just prose", // 8
	].join("\n");

	test("only DL-\\d+ rows with 4 cells are parsed, with 1-based lines", () => {
		const rows = parseLedger(text);
		expect(rows.length).toBe(2);
		expect(rows[0]).toEqual({
			id: "DL-001",
			decision: "use X",
			status: "Active (Matt, 2026-07-22)",
			recordCell: "[r](a.md)",
			line: 5,
		});
		expect(rows[1]).toEqual({
			id: "DL-002",
			decision: "use Y",
			status: "Historical",
			recordCell: "[r](b.md)",
			line: 6,
		});
	});

	test("a valid row with a GFM-escaped pipe (\\|) in a cell is parsed, not dropped", () => {
		// `.split("|")` would treat `\|` as a delimiter, inflating the cell count
		// so the 4-cell check silently discards the row — bypassing validation of
		// its id/status/supersession/record. The escaped pipe is one literal `|`.
		const rows = parseLedger(
			"| DL-003 | use A \\| B | Active (Matt, 2026-07-22) | [r](c.md) |",
		);
		expect(rows.length).toBe(1);
		expect(rows[0]).toEqual({
			id: "DL-003",
			decision: "use A | B",
			status: "Active (Matt, 2026-07-22)",
			recordCell: "[r](c.md)",
			line: 1,
		});
	});
});

describe("parseRecordHeader", () => {
	test("status slot is the first non-blank line after the H1", () => {
		const h = parseRecordHeader("r.md", "# Title\n\nStatus: Active\n");
		expect(h.statusLine).toBe("Status: Active");
		expect(h.line).toBe(3);
	});
	test("a prose preamble in the slot → statusLine null at that line", () => {
		const h = parseRecordHeader("r.md", "# Title\n> prose preamble\n");
		expect(h.statusLine).toBeNull();
		expect(h.line).toBe(2);
	});
	test("no H1 at all → statusLine null, line 1", () => {
		const h = parseRecordHeader("r.md", "just prose\nmore prose\n");
		expect(h.statusLine).toBeNull();
		expect(h.line).toBe(1);
	});
	test("a `#`-shaped line inside a pre-H1 fence is not mistaken for the H1", () => {
		const h = parseRecordHeader(
			"r.md",
			"```\n# fenced not-a-title\n```\n\n# Real Title\n\nStatus: Active\n",
		);
		expect(h.statusLine).toBe("Status: Active");
		expect(h.line).toBe(7);
	});
});

describe("recordContentFromText", () => {
	test("headings are slugified; sizeBytes is the utf8 byte length", () => {
		const text = "# Problem / Intent\n## Approach\nbody text\n";
		const rc = recordContentFromText(text);
		expect(rc.headings).toEqual(["problem--intent", "approach"]);
		expect(rc.sizeBytes).toBe(Buffer.byteLength(text, "utf8"));
	});
	test("a `#`-prefixed line inside a fenced code block is NOT a heading", () => {
		const text =
			"# Real Title\n\n```bash\n# not a heading\n```\n\n## Approach\n";
		const rc = recordContentFromText(text);
		expect(rc.headings).toEqual(["real-title", "approach"]);
		expect(rc.headings).not.toContain("not-a-heading");
	});
	test("a tilde fence also suppresses heading extraction", () => {
		const text = "# T\n\n~~~\n# fenced\n~~~\n";
		expect(recordContentFromText(text).headings).toEqual(["t"]);
	});
});

// ---------------------------------------------------------------------------
// The pure core: evaluate.
// ---------------------------------------------------------------------------

function row(overrides: Partial<LedgerRow> = {}): LedgerRow {
	return {
		id: "DL-001",
		decision: "use X",
		status: "Active (Matt, 2026-07-22)",
		recordCell: "[r](compass-0.6/design.md)",
		line: 5,
		...overrides,
	};
}

function header(overrides: Partial<RecordHeader> = {}): RecordHeader {
	return {
		// A top-level product record NOT in the version-narrative chain, so a
		// baseline `Status: Active` is valid.
		path: "docs/designs/product/compass-tauri-shell.md",
		statusLine: "Status: Active",
		line: 3,
		...overrides,
	};
}

describe("evaluate — happy path", () => {
	test("valid row + valid header + empty changed → no violations", () => {
		expect(evaluate([row()], [header()], noChange, smallRecord)).toEqual([]);
	});
});

describe("evaluate — duplicate DL id", () => {
	test("two rows same id → one 'duplicate' on the second row's line", () => {
		const vs = evaluate(
			[row({ line: 5 }), row({ line: 6 })],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("duplicate");
		expect(vs[0]?.file).toBe(LEDGER);
		expect(vs[0]?.line).toBe(6);
	});
});

describe("evaluate — row status-cell grammar", () => {
	test("both valid forms pass", () => {
		expect(
			evaluate(
				[row({ status: "Active (Matt, 2026-07-22)" })],
				[],
				noChange,
				smallRecord,
			),
		).toEqual([]);
		expect(
			evaluate(
				[
					row({
						id: "DL-001",
						status: "Superseded by DL-002 (Matt, 2026-07-22)",
						line: 5,
					}),
					row({ id: "DL-002", line: 6 }),
				],
				[],
				noChange,
				smallRecord,
			),
		).toEqual([]);
	});
	test("Retired form passes and needs no successor row", () => {
		// A decision scrapped with no replacement: valid, and — unlike
		// `Superseded by DL-<n>` — it must NOT require a target row to resolve.
		expect(
			evaluate(
				[row({ status: "Retired (Matt, 2026-08-23)" })],
				[],
				noChange,
				smallRecord,
			),
		).toEqual([]);
	});
	test("Retired does not enter a supersession chain (no dangling-target check)", () => {
		// Two independent Retired rows: neither points anywhere, so neither can
		// dangle or cycle.
		expect(
			evaluate(
				[
					row({ id: "DL-001", status: "Retired (Matt, 2026-08-23)", line: 5 }),
					row({ id: "DL-002", status: "Retired (Matt, 2026-08-23)", line: 6 }),
				],
				[],
				noChange,
				smallRecord,
			),
		).toEqual([]);
	});
	test("neither form → 'malformed'", () => {
		const vs = evaluate(
			[row({ status: "sometime later" })],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("malformed");
		expect(vs[0]?.line).toBe(5);
	});
	test("Retired-shaped but dateless → 'malformed' (pins ROW_RETIRED_RE strictness)", () => {
		// A cell that begins `Retired ` but omits the required date must be
		// rejected — otherwise a future loosening of ROW_RETIRED_RE to a bare
		// prefix would silently pass. Same strictness the Active/Superseded
		// forms enforce.
		const vs = evaluate(
			[row({ status: "Retired (Matt)" })],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("malformed");
		expect(vs[0]?.line).toBe(5);
	});
});

describe("evaluate — supersession integrity", () => {
	test("dangling target → 'not a ledger row'", () => {
		const vs = evaluate(
			[row({ status: "Superseded by DL-999 (Matt, 2026-07-22)" })],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("not a ledger row");
		expect(vs[0]?.line).toBe(5);
	});
	test("self-supersession → 'self'", () => {
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-001 (Matt, 2026-07-22)",
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("self");
		expect(vs[0]?.line).toBe(5);
	});
	test("a 2-cycle is reported exactly once (per unordered pair)", () => {
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 5,
				}),
				row({
					id: "DL-002",
					status: "Superseded by DL-001 (Matt, 2026-07-22)",
					line: 6,
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(1);
		expect(vs[0]?.message).toContain("DL-001");
		expect(vs[0]?.message).toContain("DL-002");
		expect(vs.length).toBe(1);
		expect(vs[0]?.file).toBe(LEDGER);
		expect(vs[0]?.line).toBe(5);
	});
	test("a ≥3-cycle is reported once naming all members (walk, not one-hop)", () => {
		// DL-001→DL-002→DL-003→DL-001. No row is self- or back-superseded, so a
		// one-hop 2-cycle back-check saw zero violations; only a walk-to-terminus
		// detector catches this loop. THE case OQ1b widened the gate to cover.
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 5,
				}),
				row({
					id: "DL-002",
					status: "Superseded by DL-003 (Matt, 2026-07-22)",
					line: 6,
				}),
				row({
					id: "DL-003",
					status: "Superseded by DL-001 (Matt, 2026-07-22)",
					line: 7,
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(1);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("DL-001");
		expect(vs[0]?.message).toContain("DL-002");
		expect(vs[0]?.message).toContain("DL-003");
		expect(vs[0]?.file).toBe(LEDGER);
		expect(vs[0]?.line).toBe(5);
	});
	test("a chain feeding into a cycle reports only the loop members, once", () => {
		// DL-001 (tail) → DL-002 ⇄ DL-003 (the cycle). The walk from DL-001 enters
		// the loop and closes it on DL-002, so the reported cycle is {DL-002,
		// DL-003} only; DL-001 is not a member and produces no cycle of its own.
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 5,
				}),
				row({
					id: "DL-002",
					status: "Superseded by DL-003 (Matt, 2026-07-22)",
					line: 6,
				}),
				row({
					id: "DL-003",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 7,
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(1);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("DL-002");
		expect(vs[0]?.message).toContain("DL-003");
		expect(vs[0]?.message).not.toContain("DL-001");
		expect(vs[0]?.line).toBe(6);
	});
	test("cycle report line is the lowest member line, not ledger array order", () => {
		// DL-002 (line 8) ⇄ DL-001 (line 3), with the higher-line row FIRST in the
		// array. The walk starts at DL-002 and closes the loop on it (line 8), but
		// the report anchors to min(line) = 3 — stable regardless of array order.
		const vs = evaluate(
			[
				row({
					id: "DL-002",
					status: "Superseded by DL-001 (Matt, 2026-07-22)",
					line: 8,
				}),
				row({
					id: "DL-001",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 3,
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(1);
		expect(vs.length).toBe(1);
		expect(vs[0]?.line).toBe(3);
	});
	test("a healthy chain terminating at Active is not a cycle", () => {
		// DL-001 → DL-002 → DL-003 (Active). The walk reaches a live terminus, so
		// no loop — guards against over-reporting a legitimate supersession chain.
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 5,
				}),
				row({
					id: "DL-002",
					status: "Superseded by DL-003 (Matt, 2026-07-22)",
					line: 6,
				}),
				row({ id: "DL-003", status: "Active (Matt, 2026-07-22)", line: 7 }),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(0);
		expect(vs).toEqual([]);
	});
	test("a self-loop yields only the per-edge message, never a cycle", () => {
		// DL-001 superseded by itself: the per-edge `superseded by itself` fires,
		// but the walk's `cycle.length > 1` guard skips the degenerate 1-cycle, so
		// there is exactly one violation and NO `supersession cycle`.
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-001 (Matt, 2026-07-22)",
					line: 5,
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("superseded by itself");
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(0);
	});
	test("two independent cycles are each reported once (not four)", () => {
		// {DL-001 ⇄ DL-002} and {DL-003 ⇄ DL-004} are disjoint loops. Each is keyed
		// by its own member set, so exactly two cycle violations — not one per row.
		const vs = evaluate(
			[
				row({
					id: "DL-001",
					status: "Superseded by DL-002 (Matt, 2026-07-22)",
					line: 5,
				}),
				row({
					id: "DL-002",
					status: "Superseded by DL-001 (Matt, 2026-07-22)",
					line: 6,
				}),
				row({
					id: "DL-003",
					status: "Superseded by DL-004 (Matt, 2026-07-22)",
					line: 7,
				}),
				row({
					id: "DL-004",
					status: "Superseded by DL-003 (Matt, 2026-07-22)",
					line: 8,
				}),
			],
			[],
			noChange,
			smallRecord,
		);
		expect(
			vs.filter((x) => x.message.includes("supersession cycle")).length,
		).toBe(2);
		expect(vs.length).toBe(2);
	});
});

describe("evaluate — Record link resolution", () => {
	test("path missing (readRecord null) → 'does not resolve'", () => {
		const vs = evaluate([row()], [], noChange, () => null);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("does not resolve");
		expect(vs[0]?.line).toBe(5);
	});
	test("dead #anchor → 'anchor not found'", () => {
		const vs = evaluate(
			[row({ recordCell: "[r](compass-0.6/design.md#missing)" })],
			[],
			noChange,
			() => ({ headings: ["present"], sizeBytes: 100 }),
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("anchor not found");
		expect(vs[0]?.line).toBe(5);
	});
	test("#anchor matching only a FENCED pseudo-heading → 'anchor not found'", () => {
		// The target's only `# not-a-heading` line is inside a code fence, so it
		// must not become a resolvable slug (else a dead pointer false-passes).
		const target = recordContentFromText(
			"# Real\n\n```bash\n# not-a-heading\n```\n",
		);
		const vs = evaluate(
			[row({ recordCell: "[r](compass-0.6/design.md#not-a-heading)" })],
			[],
			noChange,
			() => target,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("anchor not found");
	});
	test("live #anchor resolves even for a >50KB record → no violation", () => {
		const vs = evaluate(
			[row({ recordCell: "[r](compass-0.6/design.md#present)" })],
			[],
			noChange,
			() => ({ headings: ["present"], sizeBytes: 60 * 1024 }),
		);
		expect(vs).toEqual([]);
	});
	test("large record without an anchor → 'large record'", () => {
		const vs = evaluate(
			[row({ recordCell: "[r](compass-0.6/design.md)" })],
			[],
			noChange,
			() => ({ headings: [], sizeBytes: 50 * 1024 + 1 }),
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("large record");
		expect(vs[0]?.line).toBe(5);
	});
	test("small record without an anchor → no violation", () => {
		const vs = evaluate(
			[row({ recordCell: "[r](compass-0.6/design.md)" })],
			[],
			noChange,
			() => ({ headings: [], sizeBytes: 100 }),
		);
		expect(vs).toEqual([]);
	});
	test("Record cell is not a markdown link → 'not a markdown link'", () => {
		const vs = evaluate(
			[row({ recordCell: "plain text" })],
			[],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("not a markdown link");
		expect(vs[0]?.line).toBe(5);
	});
});

describe("evaluate — record Status: header presence & grammar", () => {
	test("statusLine null → 'missing'", () => {
		const vs = evaluate(
			[row()],
			[header({ statusLine: null, line: 3 })],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("missing");
		expect(vs[0]?.file).toBe("docs/designs/product/compass-tauri-shell.md");
		expect(vs[0]?.line).toBe(3);
	});
	test("malformed status header → 'malformed'", () => {
		const vs = evaluate(
			[row()],
			[header({ statusLine: "Status: Draft (freezes on merge)." })],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("malformed");
		expect(vs[0]?.line).toBe(3);
	});
});

describe("evaluate — Historical-set membership", () => {
	// The version-narrative chain is EMPTY (RIG-2453 retired the v0.3–v0.8
	// milestone records that made it up). So today EVERY record marked
	// `Status: Historical` is out-of-chain → a violation, and the
	// "legitimately in-chain" branch of the iff has no member to exercise with
	// a literal path. This drift guard pins the empty set; if a future
	// version-narrative record re-populates HISTORICAL_CHAIN, restore the
	// in-chain positive/negative cases (an in-chain record marked `Historical`
	// passes; marked `Active` must be `Historical`).
	test("HISTORICAL_CHAIN is empty (no version-narrative record today)", () => {
		expect(Object.keys(HISTORICAL_CHAIN)).toHaveLength(0);
	});
	test("any record marked Historical → 'version-narrative chain' (chain empty)", () => {
		const vs = evaluate(
			[],
			[
				header({
					path: "docs/designs/product/compass-tauri-shell.md",
					statusLine: "Status: Historical",
				}),
			],
			noChange,
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("not in the version-narrative chain");
	});
	test("an out-of-chain record marked Active → no violation", () => {
		expect(
			evaluate(
				[],
				[
					header({
						path: "docs/designs/product/compass-tauri-shell.md",
						statusLine: "Status: Active",
					}),
				],
				noChange,
				smallRecord,
			),
		).toEqual([]);
	});
});

describe("evaluate — record-level Superseded pointer", () => {
	// The Status pointer is RECORD-relative; readRecord receives a
	// designs-root-relative (bucket-qualified) path. This path-aware resolver
	// (unlike smallRecord, which ignores its arg) returns a record only for the
	// exact designs-root-relative path that exists, so it locks down the
	// resolution base.
	const onlyExists =
		(existing: string) =>
		(p: string): RecordContent | null =>
			p === existing ? { headings: [], sizeBytes: 100 } : null;

	test("pointer target missing → 'does not resolve'", () => {
		const vs = evaluate(
			[],
			[
				header({
					path: "docs/designs/product/compass-tauri-shell.md",
					statusLine: "Status: Superseded by ../other/design.md",
				}),
			],
			noChange,
			() => null,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("does not resolve");
		expect(vs[0]?.line).toBe(3);
	});
	test("record-relative pointer resolves to a sibling under a nested record", () => {
		// A nested non-chain record + `../sibling/design.md` → designs-root-relative
		// `product/sibling/design.md`. The resolver only knows that path, so a
		// correct base is the only way this passes.
		expect(
			evaluate(
				[],
				[
					header({
						path: "docs/designs/product/compass-ade-shell/design.md",
						statusLine:
							"Status: Superseded by ../compass-dock-in-sidebar/design.md",
					}),
				],
				noChange,
				onlyExists("product/compass-dock-in-sidebar/design.md"),
			),
		).toEqual([]);
	});
	test("a cross-bucket record-relative pointer resolves inside DESIGNS_ROOT", () => {
		// A ui/ record superseded by an agent/ record: `../../agent/...` climbs to
		// the designs root, then into agent/. Resolves as long as it stays inside
		// DESIGNS_ROOT.
		expect(
			evaluate(
				[],
				[
					header({
						path: "docs/designs/ui/compass-tauri-shell/design.md",
						statusLine:
							"Status: Superseded by ../../agent/compass-native-app/design.md",
					}),
				],
				noChange,
				onlyExists("agent/compass-native-app/design.md"),
			),
		).toEqual([]);
	});
	test("a designs-root-relative form does NOT resolve from a nested record (base is locked)", () => {
		// Writing the pointer bucket-qualified (`compass-dock-in-sidebar/design.md`)
		// from inside a nested record is wrong: it resolves record-relative to
		// `product/compass-ade-shell/compass-dock-in-sidebar/design.md`, which the
		// resolver rejects.
		const vs = evaluate(
			[],
			[
				header({
					path: "docs/designs/product/compass-ade-shell/design.md",
					statusLine: "Status: Superseded by compass-dock-in-sidebar/design.md",
				}),
			],
			noChange,
			onlyExists("product/compass-dock-in-sidebar/design.md"),
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("does not resolve");
	});
	test("a top-level record's record-relative pointer resolves to a sibling", () => {
		expect(
			evaluate(
				[],
				[
					header({
						path: "docs/designs/product/compass-tauri-shell.md",
						statusLine: "Status: Superseded by other-record.md",
					}),
				],
				noChange,
				onlyExists("product/other-record.md"),
			),
		).toEqual([]);
	});
});

describe("evaluate — touch-coupling (DL-Q1)", () => {
	const rec = "docs/designs/product/compass-0.6/design.md";

	test("touches a record, not the ledger, no declaration → one violation", () => {
		const vs = evaluate(
			[],
			[],
			changed([rec], "some unrelated body"),
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.file).toBe("(pull request)");
		expect(vs[0]?.line).toBe(0);
		expect(vs[0]?.message).toContain("Ledger-impact");
	});
	test("also touches DECISIONS.md → no violation", () => {
		expect(
			evaluate(
				[],
				[],
				changed([rec, "docs/designs/DECISIONS.md"], null),
				smallRecord,
			),
		).toEqual([]);
	});
	test("declares Ledger-impact: none → no violation", () => {
		expect(
			evaluate([], [], changed([rec], "Ledger-impact: none"), smallRecord),
		).toEqual([]);
	});
	test("empty changed set (non-PR event) → no violation", () => {
		expect(evaluate([], [], noChange, smallRecord)).toEqual([]);
	});
	test("a quoted declaration is tolerated → no violation", () => {
		expect(
			evaluate([], [], changed([rec], "> Ledger-impact: none"), smallRecord),
		).toEqual([]);
	});
	test("the declaration keyword is case-insensitive → no violation", () => {
		expect(
			evaluate([], [], changed([rec], "LEDGER-IMPACT: none"), smallRecord),
		).toEqual([]);
	});
	// Automation-exempt head branches (renovate/) skip touch-coupling. Mirrors
	// spec-impact-gate's branch exemption.
	test("renovate/ branch touching a record, no ledger, no decl → no violation", () => {
		expect(
			evaluate(
				[],
				[],
				changed([rec], null, "renovate/npm-lodash-4.x"),
				smallRecord,
			),
		).toEqual([]);
	});
	test("an exempt prefix mid-branch does NOT exempt (startsWith, not includes)", () => {
		const vs = evaluate(
			[],
			[],
			changed([rec], null, "feature/renovate_thing"),
			smallRecord,
		);
		expect(vs.length).toBe(1);
		expect(vs[0]?.message).toContain("Ledger-impact");
	});
});

// ---------------------------------------------------------------------------
// The I/O wiring: runOnce.
// ---------------------------------------------------------------------------

describe("runOnce", () => {
	const validLedger = [
		"| ID | Decision | Status | Record |",
		"| --- | --- | --- | --- |",
		"| DL-001 | use X | Active (Matt, 2026-07-22) | [r](compass-0.6/design.md) |",
	].join("\n");
	const oneRecord = "docs/designs/product/compass-tauri-shell.md";

	function deps(overrides: Partial<Deps>): {
		d: Deps;
		out: string[];
		errs: string[];
	} {
		const out: string[] = [];
		const errs: string[] = [];
		const d: Deps = {
			root: "/fake",
			readText: async (_root, rel) =>
				rel === "docs/designs/DECISIONS.md"
					? validLedger
					: "# Title\n\nStatus: Active\n",
			listRecordFiles: async () => [oneRecord],
			readRecord: () => ({ headings: [], sizeBytes: 100 }),
			changed: { files: [], body: null, headBranch: "" },
			log: (m) => out.push(m),
			err: (m) => errs.push(m),
			...overrides,
		};
		return { d, out, errs };
	}

	test("all-valid tree → exit 0 and logs OK", async () => {
		const { d, out } = deps({});
		expect(await runOnce(d)).toBe(0);
		expect(out.some((l) => l.includes("OK"))).toBe(true);
	});

	test("missing ledger → exit 1 and an error about not found", async () => {
		const { d, errs } = deps({
			readText: async () => null,
			listRecordFiles: async () => [],
		});
		expect(await runOnce(d)).toBe(1);
		expect(errs.some((l) => l.includes("not found"))).toBe(true);
	});

	test("a violation present → exit 1 and prints it", async () => {
		const { d, errs } = deps({
			readText: async (_root, rel) =>
				rel === "docs/designs/DECISIONS.md"
					? validLedger
					: "# Title\n\nStatus: bogus value\n",
		});
		expect(await runOnce(d)).toBe(1);
		expect(errs.some((l) => l.includes(oneRecord))).toBe(true);
		expect(errs.some((l) => l.includes("malformed"))).toBe(true);
	});

	test("a throwing dep → exit 2", async () => {
		const { d } = deps({
			listRecordFiles: async () => {
				throw new Error("boom");
			},
		});
		expect(await runOnce(d)).toBe(2);
	});
});
