import { describe, expect, test } from "bun:test";
import {
	buildDecisionCorpus,
	type Changed,
	classifyDesignPath,
	conflictMarkerViolations,
	type DecisionCorpus,
	type Deps,
	evaluate,
	HISTORICAL_CHAIN,
	KEY_LINE,
	parseRecordHeader,
	parseStatusValue,
	prContextFrom,
	type RecordContent,
	type RecordHeader,
	recordContentFromText,
	resolveRecordRelative,
	runOnce,
	slugify,
	touchesRecord,
} from "./index.ts";

interface DecisionInput {
	id?: string;
	area?: string;
	record?: string;
	status?: string;
	decision?: string;
	path?: string;
}

const DECISION_DIR = "docs/designs/decisions";
const noChange: Changed = {
	files: [],
	body: null,
	headBranch: "",
	author: "",
	crossRepository: false,
};
const smallRecord = (): RecordContent => ({
	headings: ["present"],
	sizeBytes: 100,
});

function decisionText(options: DecisionInput = {}): string {
	const id = options.id ?? "DL-001";
	const area = options.area ?? "ui";
	const record = options.record ?? `../../${area}/record/design.md`;
	const status = options.status ?? "Active (Matt, 2026-07-22)";
	const decision = options.decision ?? "Use the stable design.";
	return [
		"---",
		`id: ${id}`,
		`decision: ${JSON.stringify(decision)}`,
		`status: ${JSON.stringify(status)}`,
		`record: ${record}`,
		"---",
	].join("\n");
}

function decisionFile(options: DecisionInput = {}) {
	const id = options.id ?? "DL-001";
	const area = options.area ?? "ui";
	return {
		path: options.path ?? `${DECISION_DIR}/${area}/${id}.md`,
		text: decisionText(options),
	};
}

function corpus(...files: DecisionInput[]): DecisionCorpus {
	const inputs = files.length === 0 ? [{}] : files;
	return buildDecisionCorpus(inputs.map((file) => decisionFile(file)));
}

function record(
	path = "docs/designs/ui/record/design.md",
	text = "# Title\n",
): RecordHeader {
	return parseRecordHeader(path, text);
}

function evaluateCorpus(
	decisions: DecisionCorpus,
	records: RecordHeader[] = [],
	changed: Changed = noChange,
	read: (path: string) => RecordContent | null = smallRecord,
) {
	return evaluate(decisions, records, changed, read);
}

test("slugify matches GitHub punctuation and whitespace rules", () => {
	expect(slugify("Hello, world!")).toBe("hello-world");
	expect(slugify("Problem / Intent")).toBe("problem--intent");
	expect(slugify("Approach — part 1")).toBe("approach--part-1");
});

test.each([
	["blockquoted Historical", "> Status: Historical", { kind: "Historical" }],
	["bold Historical", "**Status: Historical**", { kind: "Historical" }],
	[
		"quoted bold Historical",
		"> **Status: Historical**",
		{ kind: "Historical" },
	],
	[
		"bold supersession",
		"**Status: Superseded by ../next/design.md**",
		{ kind: "Superseded", path: "../next/design.md" },
	],
] as const)("parseStatusValue accepts %s", (_label, line, result) => {
	expect(parseStatusValue(line)).toEqual(result);
});

test.each(["Status: Draft", "Status: Active", "**Status: Active**"])(
	"parseStatusValue rejects %s",
	(line) => expect(parseStatusValue(line)).toBeNull(),
);
test("record-relative supersession resolves nested and cross-bucket paths only", () => {
	expect(
		resolveRecordRelative("ui/nested/record/design.md", "../sibling/design.md"),
	).toBe("ui/nested/sibling/design.md");
	expect(
		resolveRecordRelative("ui/record/design.md", "../../server/next/design.md"),
	).toBe("server/next/design.md");
	expect(resolveRecordRelative("ui/record.md", "other.md")).toBe("ui/other.md");
	expect(
		resolveRecordRelative("ui/record/design.md", "../../../escape.md"),
	).toBeNull();
});

describe("parseRecordHeader", () => {
	test("finds the Status slot and ignores preamble, missing H1, and fenced H1", () => {
		expect(
			parseRecordHeader(
				"record.md",
				"Preamble\n# Title\n\nStatus: Historical\n",
			),
		).toEqual({
			path: "record.md",
			statusLine: "Status: Historical",
			line: 4,
		});
		expect(parseRecordHeader("record.md", "Preamble only\n")).toEqual({
			path: "record.md",
			statusLine: null,
			line: 1,
		});
		expect(
			parseRecordHeader("record.md", "```md\n# Example\n```\nPreamble\n"),
		).toEqual({
			path: "record.md",
			statusLine: null,
			line: 1,
		});
		expect(parseRecordHeader("record.md", "# Title\n\nBody\n")).toEqual({
			path: "record.md",
			statusLine: null,
			line: 2,
		});
	});

	test.each([
		"> Status: Historical",
		"**Status: Historical**",
		"> **Status: Historical**",
	])("finds blockquoted or emphasized header %s", (line) => {
		expect(
			parseRecordHeader("record.md", `# Title\n\n${line}\n`).statusLine,
		).toBe(line);
	});
});

describe("design path discovery", () => {
	test("classifies decision files, the readme, legacy files, and misplaced files", () => {
		expect(classifyDesignPath(`${DECISION_DIR}/README.md`)).toBe(
			"decisions-readme",
		);
		expect(classifyDesignPath(`${DECISION_DIR}/ui/DL-001.md`)).toBe("decision");
		expect(classifyDesignPath(`${DECISION_DIR}/ui/nested/DL-001.md`)).toBe(
			"misplaced",
		);
		expect(classifyDesignPath(`${DECISION_DIR}/ui/notes.txt`)).toBe(
			"misplaced",
		);
		expect(classifyDesignPath(`${DECISION_DIR}/DL-001.md`)).toBe("misplaced");
		expect(classifyDesignPath(`${DECISION_DIR}/foo.md`)).toBe("misplaced");
		expect(classifyDesignPath(`${DECISION_DIR}/ui/DECISIONS.md`)).toBe(
			"legacy-ledger",
		);
		expect(classifyDesignPath("docs/designs/DECISIONS.md")).toBe(
			"legacy-ledger",
		);
		expect(classifyDesignPath("docs/designs/server/DECISIONS.md")).toBe(
			"legacy-ledger",
		);
		expect(classifyDesignPath("docs/designs/ui/DL-009.md")).toBe("misplaced");
		expect(classifyDesignPath("docs/designs/ui/record/design.md")).toBe(
			"other",
		);
	});

	test("decision tree is not a governed record", () => {
		expect(touchesRecord(`${DECISION_DIR}/ui/DL-001.md`)).toBe(false);
		expect(touchesRecord("docs/designs/DECISIONS.md")).toBe(false);
	});
});

describe("touchesRecord", () => {
	test.each([
		["docs/designs/ui/compass.md", true],
		["docs/designs/ui/compass/design.md", true],
		["docs/designs/ui/compass/other.md", true],
		["docs/designs/infra/runtime/compass-x/microvm-v3.md", true],
		["docs/designs/platform/x/design.md", true],
		["docs/designs/CONTRIBUTING.md", false],
		["docs/designs/nope/compass.md", false],
		["docs/designs/ui/subgroup/flat.md", true],
		["docs/designs/not-governed/record/design.md", false],
		["docs/designs/ui/", false],
		["docs/not-designs/ui/record/design.md", false],
		["docs/designs/ui/record/image.png", false],
	])("%s -> %s", (path, expected) => {
		expect(touchesRecord(path)).toBe(expected);
	});
});

describe("decision parsing and invariants", () => {
	test("Retired decisions pass and never require a successor", () => {
		expect(
			evaluateCorpus(corpus({ status: "Retired (Matt, 2026-08-23)" })),
		).toEqual([]);
		expect(
			evaluateCorpus(
				corpus(
					{ id: "DL-001", status: "Retired (Matt, 2026-08-23)" },
					{ id: "DL-002", status: "Retired (Matt, 2026-08-23)" },
				),
			),
		).toEqual([]);
	});

	test("decision status grammar is enforced by the parser at line 4", () => {
		const malformed = buildDecisionCorpus([
			{ ...decisionFile(), text: decisionText({ status: "Draft" }) },
		]);
		expect(malformed.rows).toHaveLength(0);
		expect(malformed.malformed[0]).toMatchObject({
			path: `${DECISION_DIR}/ui/DL-001.md`,
			line: KEY_LINE.status,
		});
	});

	test("duplicate ids across areas point to the second id key", () => {
		const got = evaluateCorpus(
			corpus(
				{ id: "DL-001", area: "ui" },
				{
					id: "DL-001",
					area: "server",
					record: "../../server/other/design.md",
				},
			),
		);
		expect(got).toContainEqual({
			file: `${DECISION_DIR}/server/DL-001.md`,
			line: KEY_LINE.id,
			message: expect.stringContaining("duplicate decision id"),
		});
	});

	test("valid supersession target passes; missing target and self-supersession fail", () => {
		expect(
			evaluateCorpus(
				corpus(
					{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
					{ id: "DL-002" },
				),
			),
		).toEqual([]);
		const missing = evaluateCorpus(
			corpus({
				id: "DL-001",
				status: "Superseded by DL-999 (Matt, 2026-07-22)",
			}),
		);
		expect(missing).toContainEqual({
			file: `${DECISION_DIR}/ui/DL-001.md`,
			line: KEY_LINE.status,
			message: expect.stringContaining("not a decision file"),
		});
		const self = evaluateCorpus(
			corpus({
				id: "DL-001",
				status: "Superseded by DL-001 (Matt, 2026-07-22)",
			}),
		);
		expect(self).toHaveLength(1);
		expect(self[0]?.message).toContain("superseded by itself");
	});

	test("reports cycles once at their stable lowest-id decision locus", () => {
		const twoCycle = evaluateCorpus(
			corpus(
				{ id: "DL-002", status: "Superseded by DL-001 (Matt, 2026-07-22)" },
				{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
			),
		);
		const cycleViolations = twoCycle.filter((item) =>
			item.message.includes("supersession cycle"),
		);
		expect(cycleViolations).toHaveLength(1);
		expect(cycleViolations[0]).toMatchObject({
			file: `${DECISION_DIR}/ui/DL-001.md`,
			line: KEY_LINE.status,
		});
		const threeCycle = evaluateCorpus(
			corpus(
				{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
				{ id: "DL-002", status: "Superseded by DL-003 (Matt, 2026-07-22)" },
				{ id: "DL-003", status: "Superseded by DL-001 (Matt, 2026-07-22)" },
			),
		);
		expect(
			threeCycle.filter((item) => item.message.includes("supersession cycle")),
		).toHaveLength(1);
		const feedIn = evaluateCorpus(
			corpus(
				{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
				{ id: "DL-002", status: "Superseded by DL-003 (Matt, 2026-07-22)" },
				{ id: "DL-003", status: "Superseded by DL-004 (Matt, 2026-07-22)" },
				{ id: "DL-004", status: "Superseded by DL-003 (Matt, 2026-07-22)" },
			),
		);
		const loop = feedIn.filter((item) =>
			item.message.includes("supersession cycle"),
		);
		expect(loop).toHaveLength(1);
		expect(loop[0]?.message).toContain("DL-003");
		expect(loop[0]?.message).toContain("DL-004");
		expect(loop[0]?.message).not.toContain("DL-001");
		const independent = evaluateCorpus(
			corpus(
				{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
				{ id: "DL-002", status: "Superseded by DL-001 (Matt, 2026-07-22)" },
				{ id: "DL-003", status: "Superseded by DL-004 (Matt, 2026-07-22)" },
				{ id: "DL-004", status: "Superseded by DL-003 (Matt, 2026-07-22)" },
			),
		);
		expect(
			independent.filter((item) => item.message.includes("supersession cycle")),
		).toHaveLength(2);
		const healthy = evaluateCorpus(
			corpus(
				{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
				{ id: "DL-002", status: "Superseded by DL-003 (Matt, 2026-07-22)" },
				{ id: "DL-003", status: "Active (Matt, 2026-07-22)" },
			),
		);
		expect(healthy).toEqual([]);
		const self = evaluateCorpus(
			corpus({
				id: "DL-001",
				status: "Superseded by DL-001 (Matt, 2026-07-22)",
			}),
		);
		expect(self).toHaveLength(1);
		expect(self[0]?.message).toContain("superseded by itself");
		expect(
			self.some((item) => item.message.includes("supersession cycle")),
		).toBe(false);
	});

	test("checks Record resolution, anchors, and large records", () => {
		const anchored = buildDecisionCorpus([
			{
				path: `${DECISION_DIR}/ui/DL-001.md`,
				text: decisionText({ record: "../../ui/record/design.md#present" }),
			},
		]);
		expect(
			evaluateCorpus(anchored, [], noChange, () => ({
				headings: ["present"],
				sizeBytes: 60_000,
			})),
		).toEqual([]);
		expect(
			evaluateCorpus(corpus(), [], noChange, () => ({
				headings: [],
				sizeBytes: 100,
			})),
		).toEqual([]);
		for (const fence of ["```", "~~~"]) {
			const fenced = recordContentFromText(`${fence}md\n# pseudo\n${fence}\n`);
			const deadAnchor = evaluateCorpus(
				corpus({ record: "../../ui/record/design.md#pseudo" }),
				[],
				noChange,
				() => fenced,
			);
			expect(
				deadAnchor.some((item) => item.message.includes("anchor not found")),
			).toBe(true);
		}
		const missingPath = evaluateCorpus(corpus(), [], noChange, () => null);
		expect(
			missingPath.some(
				(item) =>
					item.message.includes("does not resolve") &&
					item.line === KEY_LINE.record,
			),
		).toBe(true);
		const missingAnchor = evaluateCorpus(
			corpus({ record: "../../ui/record/design.md#missing" }),
			[],
			noChange,
			() => ({ headings: ["present"], sizeBytes: 100 }),
		);
		expect(
			missingAnchor.some((item) => item.message.includes("anchor not found")),
		).toBe(true);
		const largeWithoutAnchor = evaluateCorpus(corpus(), [], noChange, () => ({
			headings: [],
			sizeBytes: 50 * 1024 + 1,
		}));
		expect(
			largeWithoutAnchor.some((item) => item.message.includes("large record")),
		).toBe(true);
	});

	test("decision area must match a Record path under docs/designs", () => {
		const mismatch = evaluateCorpus(
			corpus({ record: "../../server/record/design.md" }),
			[],
			noChange,
			smallRecord,
		);
		expect(mismatch).toContainEqual({
			file: `${DECISION_DIR}/ui/DL-001.md`,
			line: KEY_LINE.record,
			message: expect.stringContaining("decision area must match"),
		});
		const outside = evaluateCorpus(
			corpus({ record: "../../../elsewhere/design.md" }),
			[],
			noChange,
			smallRecord,
		);
		expect(
			outside.some((item) => item.message.includes("decision area must match")),
		).toBe(true);
	});
});

describe("record Status headers", () => {
	test("rejects prohibited Status and Historical outside the historical chain", () => {
		expect(Object.keys(HISTORICAL_CHAIN)).toHaveLength(0);
		expect(
			evaluateCorpus(corpus(), [
				record(undefined, "# Title\n\nStatus: Draft\n"),
			]).some((item) => item.message.includes("malformed or prohibited")),
		).toBe(true);
		expect(
			evaluateCorpus(corpus(), [
				record(undefined, "# Title\n\nStatus: Historical\n"),
			]).some((item) => item.message.includes("version-narrative chain")),
		).toBe(true);
	});

	test("checks record-level Superseded pointer and decision status agreement", () => {
		const statusHeader = record(
			"docs/designs/ui/record/design.md",
			"# Title\n\nStatus: Superseded by ../next/design.md\n",
		);
		expect(
			evaluateCorpus(corpus(), [statusHeader]).some((item) =>
				item.message.includes("disagrees with its decision status"),
			),
		).toBe(true);
		const matching = evaluateCorpus(
			corpus(
				{ id: "DL-001", status: "Superseded by DL-002 (Matt, 2026-07-22)" },
				{ id: "DL-002", record: "../../ui/next/design.md" },
			),
			[statusHeader],
		);
		expect(matching.some((item) => item.message.includes("disagrees"))).toBe(
			false,
		);
		const missing = evaluateCorpus(
			corpus(),
			[statusHeader],
			noChange,
			() => null,
		);
		expect(
			missing.some((item) =>
				item.message.includes("Status supersession does not resolve"),
			),
		).toBe(true);
		const nestedPointer = record(
			"docs/designs/ui/nested/record/design.md",
			"# Title\n\nStatus: Superseded by ui/next/design.md\n",
		);
		const nestedWrongBase = evaluateCorpus(
			corpus(),
			[nestedPointer],
			noChange,
			(path) =>
				path === "docs/designs/ui/ui/next/design.md" ? smallRecord() : null,
		);
		expect(
			nestedWrongBase.some((item) =>
				item.message.includes("Status supersession does not resolve"),
			),
		).toBe(true);
	});
});

test("branch exemptions do not skip record Status validation", () => {
	const got = evaluateCorpus(
		corpus(),
		[record(undefined, "# Title\n\nStatus: Draft\n")],
		{
			files: [],
			body: null,
			headBranch: "renovate/update",
			author: "app/rigelbuild-renovate",
			crossRepository: false,
		},
	);
	expect(
		got.some((item) => item.message.includes("malformed or prohibited")),
	).toBe(true);
});

describe("decision completeness", () => {
	test("every governed root with records needs a valid decision in that area", () => {
		const got = evaluateCorpus(corpus(), [
			record("docs/designs/server/record/design.md"),
		]);
		expect(
			got.some(
				(item) =>
					item.message.includes("docs/designs/server/") &&
					item.message.includes("no valid decision file"),
			),
		).toBe(true);
	});
});

describe("touch coupling", () => {
	const changedRecord = "docs/designs/ui/record/design.md";
	test("record change requires a changed decision or Ledger-impact declaration", () => {
		const missing = evaluateCorpus(corpus(), [], {
			files: [changedRecord],
			body: "unrelated text",
			headBranch: "feature/change",
			author: "octocat",
			crossRepository: false,
		});
		expect(
			missing.some((item) =>
				item.message.includes("changed decision file or Ledger-impact"),
			),
		).toBe(true);
	});

	test("a nested supporting record or a platform record also couples", () => {
		for (const file of [
			"docs/designs/infra/runtime/compass-x/microvm-v3.md",
			"docs/designs/platform/x/design.md",
		]) {
			const missing = evaluateCorpus(corpus(), [], {
				files: [file],
				body: "no declaration",
				headBranch: "feature/change",
				author: "octocat",
				crossRepository: false,
			});
			expect(
				missing.some((item) =>
					item.message.includes("changed decision file or Ledger-impact"),
				),
			).toBe(true);
		}
	});

	test("any changed decision path or non-empty Ledger-impact satisfies coupling", () => {
		expect(
			evaluateCorpus(corpus(), [], {
				files: [changedRecord, `${DECISION_DIR}/server/DL-002.md`],
				body: null,
				headBranch: "feature/change",
				author: "octocat",
				crossRepository: false,
			}),
		).toEqual([]);
		expect(
			evaluateCorpus(corpus(), [], {
				files: [changedRecord],
				body: "Ledger-impact: none",
				headBranch: "feature/change",
				author: "octocat",
				crossRepository: false,
			}),
		).toEqual([]);
	});

	test.each(["> Ledger-impact: none", "LEDGER-IMPACT: x"])(
		"accepts declaration variant %s",
		(body) => {
			expect(
				evaluateCorpus(corpus(), [], {
					files: [changedRecord],
					body,
					headBranch: "feature/change",
					author: "octocat",
					crossRepository: false,
				}),
			).toEqual([]);
		},
	);
	test("middle-of-name exempt prefix does not exempt coupling", () => {
		const violations = evaluateCorpus(corpus(), [], {
			files: [changedRecord],
			body: null,
			headBranch: "feature/renovate/x",
			author: "octocat",
			crossRepository: false,
		});
		expect(
			violations.some((item) => item.message.includes("changed decision file")),
		).toBe(true);
	});
	test("empty changed set passes", () => {
		expect(evaluateCorpus(corpus(), [], noChange)).toEqual([]);
	});
	const bots = [
		["renovate/update", "app/rigelbuild-renovate"],
		["trunk-merge/pr-1/test", "app/trunk-io"],
	] as const;
	test.each(bots)(
		"branch %s from its bot %s skips coupling",
		(headBranch, author) => {
			expect(
				evaluateCorpus(corpus(), [], {
					files: [changedRecord],
					body: null,
					headBranch,
					author,
					crossRepository: false,
				}),
			).toEqual([]);
		},
	);
	test.each(bots)(
		"branch %s from another author still couples",
		(headBranch) => {
			const violations = evaluateCorpus(corpus(), [], {
				files: [changedRecord],
				body: null,
				headBranch,
				author: "octocat",
				crossRepository: false,
			});
			expect(
				violations.some((item) =>
					item.message.includes("changed decision file"),
				),
			).toBe(true);
		},
	);
	test.each(bots)(
		"branch %s from a fork still couples, even with the bot login",
		(headBranch, author) => {
			const violations = evaluateCorpus(corpus(), [], {
				files: [changedRecord],
				body: null,
				headBranch,
				author,
				crossRepository: true,
			});
			expect(
				violations.some((item) =>
					item.message.includes("changed decision file"),
				),
			).toBe(true);
		},
	);
	test("one bot cannot use the other bot's prefix", () => {
		const violations = evaluateCorpus(corpus(), [], {
			files: [changedRecord],
			body: null,
			headBranch: "trunk-merge/pr-1/test",
			author: "app/rigelbuild-renovate",
			crossRepository: false,
		});
		expect(
			violations.some((item) => item.message.includes("changed decision file")),
		).toBe(true);
	});
});

describe("runOnce", () => {
	const decisionPath = `${DECISION_DIR}/ui/DL-001.md`;
	const recordPath = "docs/designs/ui/record/design.md";

	function fixture(
		options: {
			paths?: string[];
			files?: Map<string, string>;
			changed?: Changed;
			list?: Deps["listDesignFiles"];
		} = {},
	) {
		const paths = options.paths ?? [decisionPath, recordPath];
		const files =
			options.files ??
			new Map([
				[decisionPath, decisionText()],
				[recordPath, "# Record\n\nBody\n"],
			]);
		const out: string[] = [];
		const errs: string[] = [];
		const deps: Deps = {
			root: "/fake",
			readText: async (_root, path) => files.get(path) ?? null,
			listDesignFiles: options.list ?? (async () => paths),
			readRecord: () => smallRecord(),
			changed: options.changed ?? noChange,
			log: (message) => out.push(message),
			err: (message) => errs.push(message),
		};
		return { deps, out, errs };
	}

	test("valid fixture lists once and logs decision and record counts", async () => {
		let listingCalls = 0;
		const { deps, out } = fixture({
			list: async () => {
				listingCalls++;
				return [decisionPath, recordPath, `${DECISION_DIR}/README.md`];
			},
		});
		expect(await runOnce(deps)).toBe(0);
		expect(listingCalls).toBe(1);
		expect(out).toEqual([
			"design-ledger-gate: OK — 1 decision file(s), 1 record(s) status-checked.",
		]);
	});

	test("rejects a reintroduced DECISIONS.md", async () => {
		const { deps, errs } = fixture({
			paths: [decisionPath, recordPath, "docs/designs/DECISIONS.md"],
		});
		expect(await runOnce(deps)).toBe(1);
		expect(errs.join("\n")).toContain(
			"DECISIONS.md is retired: record each decision as docs/designs/decisions/<area>/DL-NNN.md",
		);
	});

	test("reports misplaced decision-tree paths and DL files outside it", async () => {
		const { deps, errs } = fixture({
			paths: [
				decisionPath,
				recordPath,
				`${DECISION_DIR}/ui/extra.txt`,
				"docs/designs/server/DL-009.md",
			],
		});
		expect(await runOnce(deps)).toBe(1);
		expect(
			errs.filter((line) => line.includes("misplaced design file")),
		).toHaveLength(2);
	});

	test("zero valid decision files is a violation", async () => {
		const { deps, errs } = fixture({
			paths: [recordPath],
			files: new Map([[recordPath, "# Record\n"]]),
		});
		expect(await runOnce(deps)).toBe(1);
		expect(errs.join("\n")).toContain("no valid decision files were found");
	});

	test("malformed decision is reported at the parser line", async () => {
		const { deps, errs } = fixture({
			files: new Map([
				[decisionPath, decisionText({ status: "Draft" })],
				[recordPath, "# Record\n"],
			]),
		});
		expect(await runOnce(deps)).toBe(1);
		expect(errs.join("\n")).toContain(
			`${decisionPath}:4: malformed decision file`,
		);
	});

	test("checks decision conflict markers", async () => {
		const { deps, errs } = fixture({
			files: new Map([
				[
					decisionPath,
					`${decisionText()}\n<<<<<<< HEAD\n=======\n>>>>>>> theirs`,
				],
				[recordPath, "# Record\n"],
			]),
		});
		expect(await runOnce(deps)).toBe(1);
		expect(
			errs.filter((line) => line.includes("unresolved merge conflict marker")),
		).toHaveLength(3);
	});

	test("record Status checks still run and listing errors fail closed", async () => {
		const status = fixture({
			files: new Map([
				[decisionPath, decisionText()],
				[recordPath, "# Record\n\nStatus: Draft\n"],
			]),
		});
		expect(await runOnce(status.deps)).toBe(1);
		expect(status.errs.join("\n")).toContain("malformed or prohibited");
		const failed = fixture({
			list: async () => {
				throw new Error("boom");
			},
		});
		expect(await runOnce(failed.deps)).toBe(2);
		expect(failed.errs.join("\n")).toContain("boom");
	});
});

describe("conflictMarkerViolations", () => {
	test("flags git and jj markers, including lengthened runs", () => {
		const got = conflictMarkerViolations(
			"decision.md",
			[
				"<<<<<<< HEAD",
				"=======",
				">>>>>>> theirs",
				"%%%%%%%%%%% diff",
				"+++++++++++ side",
			].join("\n"),
		);
		expect(got.map((item) => item.line)).toEqual([1, 2, 3, 4, 5]);
	});

	test("ignores fenced examples and setext headings", () => {
		expect(
			conflictMarkerViolations(
				"record.md",
				["Heading", "==============", "```text", "<<<<<<< example", "```"].join(
					"\n",
				),
			),
		).toEqual([]);
	});
});

test("record content excludes fenced pseudo-headings from anchors", () => {
	const text = "# Real\n\n```text\n# Not real\n```\n";
	expect(recordContentFromText(text)).toEqual({
		headings: ["real"],
		sizeBytes: Buffer.byteLength(text, "utf8"),
	});
});

describe("prContextFrom", () => {
	test("valid PR coordinates enable the leg", () => {
		expect(
			prContextFrom({ REPO: "RigelBuild/compass", PR_NUMBER: "1315" }),
		).toEqual({
			kind: "pr",
			repo: "RigelBuild/compass",
			prNumber: "1315",
		});
	});

	test.each([
		[
			"missing coordinates on pull_request",
			{ GITHUB_EVENT_NAME: "pull_request" },
		],
		["missing repo", { PR_NUMBER: "1315" }],
		["invalid number", { REPO: "RigelBuild/compass", PR_NUMBER: "abc" }],
		["zero", { REPO: "RigelBuild/compass", PR_NUMBER: "0" }],
	])("%s fails closed", (_label, env) => {
		expect(prContextFrom(env).kind).toBe("error");
	});

	test.each([
		[
			"push with REPO and empty number",
			{ GITHUB_EVENT_NAME: "push", REPO: "RigelBuild/compass", PR_NUMBER: "" },
		],
		["schedule", { GITHUB_EVENT_NAME: "schedule" }],
		["workflow dispatch", { GITHUB_EVENT_NAME: "workflow_dispatch" }],
		["local", {}],
	])("%s without PR coordinates skips", (_label, env) => {
		expect(prContextFrom(env)).toEqual({ kind: "skip" });
	});

	test("non-PR dispatch with valid PR coordinates runs the leg", () => {
		expect(
			prContextFrom({
				GITHUB_EVENT_NAME: "workflow_dispatch",
				REPO: "RigelBuild/compass",
				PR_NUMBER: "1315",
			}),
		).toEqual({ kind: "pr", repo: "RigelBuild/compass", prNumber: "1315" });
	});
});
