import { describe, expect, test } from "bun:test";
import { RIGHT_SIDEBAR_TAB_GROUPS } from "../constants";
import type { FileNode, Issue, IssueState } from "../stub-data";
import { STUB_AGENTS } from "../stub-data";
import type { FleetMetrics } from "./RightSidebar";
import { filterFileTree, fleetMetrics } from "./RightSidebar";

// A small hand-built worktree: one dir with a mix of matching / non-matching
// children plus a nested dir, so the "keep only matching descendants" and
// "prune a dir whose subtree has no match" branches are both exercised.
const tree: FileNode[] = [
	{
		name: "src",
		kind: "dir",
		children: [
			{ name: "store.ts", kind: "file", status: "modified" },
			{ name: "App.tsx", kind: "file" },
			{
				name: "components",
				kind: "dir",
				children: [
					{ name: "RightSidebar.tsx", kind: "file" },
					{ name: "Board.tsx", kind: "file" },
				],
			},
		],
	},
	{ name: "package.json", kind: "file" },
];

describe("filterFileTree", () => {
	// An empty (or whitespace-only) query is the "no filter" state the search box
	// starts in: the tree must pass through untouched — same reference, nothing
	// pruned or copied.
	test("returns the tree unchanged for an empty query", () => {
		expect(filterFileTree(tree, "")).toBe(tree);
		expect(filterFileTree(tree, "   ")).toBe(tree);
	});

	// A leaf-name substring match is case-insensitive and keeps only the branch
	// leading to the matched file: the containing dir survives carrying just the
	// match, its non-matching siblings and the unrelated top-level file are gone.
	test("keeps only the path to a case-insensitive leaf match", () => {
		const result = filterFileTree(tree, "STORE");

		expect(result).toEqual([
			{
				name: "src",
				kind: "dir",
				children: [{ name: "store.ts", kind: "file", status: "modified" }],
			},
		]);
	});

	// A dir kept because a DESCENDANT matches keeps only the matching descendants
	// at every level: `src` and `components` survive because RightSidebar.tsx
	// matches, but Board.tsx, App.tsx, and store.ts are pruned.
	test("keeps a dir for a deep descendant match, pruning non-matching siblings", () => {
		const result = filterFileTree(tree, "rightsidebar");

		expect(result).toEqual([
			{
				name: "src",
				kind: "dir",
				children: [
					{
						name: "components",
						kind: "dir",
						children: [{ name: "RightSidebar.tsx", kind: "file" }],
					},
				],
			},
		]);
	});

	// A dir whose OWN name matches is kept whole — every descendant survives,
	// even ones that don't match the query (the dir itself is the match).
	test("keeps a whole subtree when the dir's own name matches", () => {
		const result = filterFileTree(tree, "components");

		expect(result).toEqual([
			{
				name: "src",
				kind: "dir",
				children: [
					{
						name: "components",
						kind: "dir",
						children: [
							{ name: "RightSidebar.tsx", kind: "file" },
							{ name: "Board.tsx", kind: "file" },
						],
					},
				],
			},
		]);
	});

	// A query matching nothing returns an empty list, so the files pane renders
	// nothing rather than the whole tree.
	test("returns [] when nothing matches", () => {
		expect(filterFileTree(tree, "no-such-file")).toEqual([]);
	});
});

describe("RIGHT_SIDEBAR_TAB_GROUPS", () => {
	// The activity bar renders groups top-to-bottom with a divider between them;
	// fleet (always-on agent conversations) must sit ABOVE issue (D2). A
	// swapped or renamed group order would render the bar upside-down, so pin the
	// exact sequence.
	test("orders the groups fleet-first, issue-second", () => {
		expect(RIGHT_SIDEBAR_TAB_GROUPS.map((g) => g.group)).toEqual([
			"fleet",
			"issue",
		]);
	});

	// The groups must PARTITION the full RightSidebarTab union: every tab appears
	// exactly once across all groups (fleet ids in declaration order, then
	// issue), with nothing missing, duplicated, or invented. A tab that
	// slipped out of both groups, or landed in two, would render either an
	// unreachable pane or a doubled icon — this catches both.
	test("partitions the union exactly, no id missing or repeated", () => {
		const ids = RIGHT_SIDEBAR_TAB_GROUPS.flatMap((g) =>
			g.items.map((item) => item.id),
		);
		expect(ids).toEqual([
			"supervisor",
			"warden",
			"status",
			"files",
			"vcs",
			"pr",
		]);
		// Ordered-equality above already forbids extras/gaps; this pins "no
		// duplicate" independently of order so a reordering refactor can't mask a
		// repeat.
		expect(new Set(ids).size).toBe(ids.length);
	});

	// The fleet/issue split decides which tabs badge an agent StateDot.
	// Issue panes never carry an agentId. Among fleet tabs, the
	// agent-conversation tabs (supervisor, warden) carry one; the Status tab is
	// a fleet PANE, not a conversation, so it carries none. Pin exactly which
	// fleet ids badge an agent so a stray or missing agentId can't slip through.
	test("fleet items carry an agentId; issue items do not", () => {
		const fleet = RIGHT_SIDEBAR_TAB_GROUPS.find((g) => g.group === "fleet");
		const issue = RIGHT_SIDEBAR_TAB_GROUPS.find((g) => g.group === "issue");
		expect(fleet).toBeDefined();
		expect(issue).toBeDefined();
		// Every issue pane is agent-less.
		for (const item of issue?.items ?? []) {
			expect(item.agentId).toBeUndefined();
		}
		// The fleet tabs that DO carry an agentId are exactly the conversation
		// tabs — Status is not among them.
		const withAgent = (fleet?.items ?? [])
			.filter((item) => item.agentId !== undefined)
			.map((item) => item.id);
		expect(withAgent).toEqual(["supervisor", "warden"]);
		// Status is present as a fleet tab, and carries no agentId.
		const status = (fleet?.items ?? []).find((item) => item.id === "status");
		expect(status).toBeDefined();
		expect(status?.agentId).toBeUndefined();
	});

	// A fleet agentId is only useful if it resolves a real stub agent (the D3
	// `agentFor` lookup badges the tab from STUB_AGENTS). Only conversation tabs
	// carry one — the Status tab, having none, is skipped, not a failure — so
	// iterate the fleet tabs that DO carry an agentId and assert each resolves.
	test("every fleet agentId resolves a real stub agent", () => {
		const fleet = RIGHT_SIDEBAR_TAB_GROUPS.find((g) => g.group === "fleet");
		const agentTabs = (fleet?.items ?? []).filter(
			(item) => item.agentId !== undefined,
		);
		// Guard against a vacuous pass if the filter ever empties the set.
		expect(agentTabs.length).toBeGreaterThan(0);
		for (const item of agentTabs) {
			expect(STUB_AGENTS.some((a) => a.account.id === item.agentId)).toBe(true);
		}
	});
});

// `fleetMetrics` is the pure count salvaged from the old BottomDock `countState`
// (dock-in-sidebar T2): it projects an issue list into the four numbers the
// Status pane renders. The routing of each state to a bucket — and which states
// are ignored — is the contract; these tests pin it against a miscount.
describe("fleetMetrics", () => {
	// fleetMetrics reads only `.state`; every other Issue field is
	// irrelevant to the count, so a state-keyed factory (cast past the full
	// interface) is the right fixture — building all fields would only couple
	// the test to shape.
	const ws = (state: IssueState): Issue => ({ state }) as Issue;

	// `active` is the sum of BOTH in_progress and in_review (salvaged
	// countState("in_progress","in_review")); queued/todo/blocked are single
	// states. A mixed list must add up per bucket — and prove active fuses the
	// two working states rather than counting only in_progress.
	test("counts each bucket across a mixed list", () => {
		const list = [
			ws("in_progress"),
			ws("in_progress"),
			ws("in_review"),
			ws("queued"),
			ws("todo"),
			ws("todo"),
			ws("todo"),
			ws("blocked"),
			ws("blocked"),
		];
		const expected: FleetMetrics = {
			active: 3,
			queued: 1,
			todo: 3,
			blocked: 2,
		};
		expect(fleetMetrics(list)).toEqual(expected);
	});

	// backlog and done fall outside the five counted states, so they land in no
	// bucket: a list of only those is all zeros, and dropping them into a
	// counted list must not shift a single number.
	test("ignores out-of-bucket states (backlog, done)", () => {
		const allZero: FleetMetrics = {
			active: 0,
			queued: 0,
			todo: 0,
			blocked: 0,
		};
		expect(fleetMetrics([ws("backlog"), ws("done"), ws("backlog")])).toEqual(
			allZero,
		);

		const counted = [ws("in_progress"), ws("queued"), ws("blocked")];
		const withNoise = [...counted, ws("backlog"), ws("done")];
		expect(fleetMetrics(withNoise)).toEqual(fleetMetrics(counted));
	});

	// The empty-list boundary: no issues → every bucket zero (an empty
	// reduce, not a throw or a NaN).
	test("returns all zeros for an empty list", () => {
		const expected: FleetMetrics = {
			active: 0,
			queued: 0,
			todo: 0,
			blocked: 0,
		};
		expect(fleetMetrics([])).toEqual(expected);
	});

	// Each counted state routes to exactly one bucket — a single-element list
	// puts a 1 in its own bucket and 0 everywhere else. Catches a swapped
	// mapping (e.g. todo counted as queued) and confirms in_review joins
	// `active` rather than getting a bucket of its own.
	const routing: { state: IssueState; bucket: keyof FleetMetrics }[] = [
		{ state: "in_progress", bucket: "active" },
		{ state: "in_review", bucket: "active" },
		{ state: "queued", bucket: "queued" },
		{ state: "todo", bucket: "todo" },
		{ state: "blocked", bucket: "blocked" },
	];
	for (const { state, bucket } of routing) {
		test(`routes a lone ${state} issue to the ${bucket} bucket`, () => {
			const expected: FleetMetrics = {
				active: 0,
				queued: 0,
				todo: 0,
				blocked: 0,
			};
			expected[bucket] += 1;
			expect(fleetMetrics([ws(state)])).toEqual(expected);
		});
	}
});
