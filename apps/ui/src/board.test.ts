import { describe, expect, test } from "bun:test";
import {
	ACTIVE_STATES,
	activeIssues,
	backlogIssues,
	boardAgents,
	cellItems,
	isActiveState,
	isBacklogState,
	laneTotal,
	prCount,
	prRowGroups,
	prRows,
	subtreeAgentIds,
	treeOrder,
} from "./board";
import { BOARD_LANES } from "./constants";
import type { Agent, Issue, IssueState, PullRequest } from "./stub-data";

// board.ts is the pure core of the D1 issue state model: it partitions the
// lifecycle into the active board columns vs the pre-active Backlog tier vs the
// terminal `archived` sink (off-board), and derives every board query (lanes,
// swimlane rows, cells, counts) from that one partition. These tests defend the
// partition invariants against a future union edit or lane reorder silently
// desyncing the surfaces that read it.

// The complete lifecycle (stub-data IssueState). Enumerated here on purpose: if
// the union grows a state, TypeScript reddens this literal against the
// exhaustiveness assertion below, forcing the new state to be classified.
// `archived` (DL-071) is the terminal, off-board third tier — neither active
// nor backlog.
const ALL_STATES: readonly IssueState[] = [
	"backlog",
	"todo",
	"queued",
	"blocked",
	"in_progress",
	"in_review",
	"done",
	"archived",
];

// Minimal valid Issue — only the fields board.ts reads (state, assignee) carry
// meaning; the rest are present to satisfy the interface and are held constant
// so a fixture-shape change never perturbs a partition assertion.
function ws(over: Partial<Issue> & Pick<Issue, "id" | "state">): Issue {
	return {
		forge: { provider: "github", host: "github.com" },
		repo: "acme/repo",
		number: 0,
		title: "t",
		body: "",
		forgeState: "open",
		forgeAccount: "acct",
		url: "https://example.test/i",
		labels: [],
		priority: "medium",
		assignee: null,
		summary: "s",
		branch: "b",
		prs: [],
		...over,
	};
}

// Minimal valid Agent — board.ts only reads `account.id` and, for the tree
// helpers, `account.parentAgentId`; the rest satisfies the shape.
function agent(id: string, parentAgentId?: string): Agent {
	return {
		account: {
			id,
			handle: id,
			displayName: id,
			kind: "agent",
			parentAgentId,
		},
		lifecycle: "idle",
		role: "worker",
		model: "m",
		cwd: "/",
		terminals: [],
	};
}

describe("state partition (D1 + DL-071)", () => {
	// Invariant 1 — the core partition contract. Every working lifecycle state is
	// classified by exactly one predicate: active XOR backlog. The terminal
	// `archived` tier is off-board — neither active nor backlog — so it is
	// excluded from the XOR and checked separately below. A future edit that adds
	// a working state without updating both predicates would break exhaustiveness
	// or disjointness here.
	test("active/backlog partition is exhaustive and disjoint over the 7 working states", () => {
		for (const state of ALL_STATES) {
			if (state === "archived") continue;
			const active = isActiveState(state);
			const backlog = isBacklogState(state);
			// exactly one is true (XOR): never both, never neither.
			expect(active !== backlog).toBe(true);
		}
		// exactly 8 states enumerated, no duplicates.
		expect(new Set(ALL_STATES).size).toBe(8);
	});

	// Invariant 1b — the terminal tier. `archived` (DL-071) is off-board: neither
	// an active column nor a pre-active backlog state. It is the third tier.
	test("archived is off-board — neither active nor backlog", () => {
		expect(isActiveState("archived")).toBe(false);
		expect(isBacklogState("archived")).toBe(false);
	});

	// Invariant 2a — the red→green flip. `backlog` and `todo` were board columns
	// in the old 6-state model; under D1 they are the pre-active tier and must
	// NOT be active. This assertion fails against the old model.
	test("backlog and todo are NOT active (moved off the board)", () => {
		expect(isActiveState("backlog")).toBe(false);
		expect(isActiveState("todo")).toBe(false);
		expect(isBacklogState("backlog")).toBe(true);
		expect(isBacklogState("todo")).toBe(true);
	});

	// Invariant 2b — the renamed states. `queued`/`done` replace the old
	// `dispatched`/`merged` and are active board columns.
	test("queued and done ARE active (renamed from dispatched/merged)", () => {
		expect(isActiveState("queued")).toBe(true);
		expect(isActiveState("done")).toBe(true);
		expect(isBacklogState("queued")).toBe(false);
		expect(isBacklogState("done")).toBe(false);
	});
});

describe("ACTIVE_STATES", () => {
	// Invariant 3 — ACTIVE_STATES is derived from the lanes, so the predicate and
	// the rendered columns can never diverge. Order matters: it's the left→right
	// board column order, so assert the exact sequence, not just membership.
	test("equals the board lane states in display order", () => {
		expect([...ACTIVE_STATES]).toEqual(BOARD_LANES.map((l) => l.state));
		expect([...ACTIVE_STATES]).toEqual([
			"queued",
			"blocked",
			"in_progress",
			"in_review",
			"done",
		]);
	});
});

describe("activeIssues / backlogIssues", () => {
	// A list spanning every tier, deliberately interleaved so an order bug shows.
	const list: Issue[] = [
		ws({ id: "w-done", state: "done" }),
		ws({ id: "w-backlog", state: "backlog" }),
		ws({ id: "w-inprog", state: "in_progress" }),
		ws({ id: "w-todo", state: "todo" }),
		ws({ id: "w-queued", state: "queued" }),
		ws({ id: "w-blocked", state: "blocked" }),
		ws({ id: "w-review", state: "in_review" }),
		ws({ id: "w-archived", state: "archived" }),
	];

	// Invariant 4 — the two views select their tiers and preserve input order.
	test("select the active states in input order", () => {
		expect(activeIssues(list).map((w) => w.id)).toEqual([
			"w-done",
			"w-inprog",
			"w-queued",
			"w-blocked",
			"w-review",
		]);
	});

	test("select the backlog states in input order", () => {
		expect(backlogIssues(list).map((w) => w.id)).toEqual([
			"w-backlog",
			"w-todo",
		]);
	});

	// Invariant 4 (partition) — active ∪ backlog ∪ archived cover every issue
	// exactly once, dropping none and duplicating none. `archived` is the
	// off-board third tier (DL-071), so the two on-board views leave it out and
	// the three-way union is the whole list.
	test("active ∪ backlog ∪ archived cover every issue exactly once (no drop, no dup)", () => {
		const active = activeIssues(list).map((w) => w.id);
		const backlog = backlogIssues(list).map((w) => w.id);
		const archived = list
			.filter((w) => w.state === "archived")
			.map((w) => w.id);
		const union = [...active, ...backlog, ...archived];
		expect(new Set(union).size).toBe(union.length); // disjoint: no id twice
		expect(new Set(union)).toEqual(new Set(list.map((w) => w.id))); // exhaustive
	});
});

describe("cellItems", () => {
	const list: Issue[] = [
		ws({ id: "a1", state: "in_progress", assignee: "agent-a" }),
		ws({ id: "b1", state: "in_progress", assignee: "agent-b" }),
		ws({ id: "a2", state: "in_progress", assignee: "agent-a" }),
		ws({ id: "t1", state: "todo", assignee: "agent-a" }),
	];

	// Invariant 5 — a non-active state never yields a cell, even when matching
	// issues exist. The board must not render backlog/todo columns.
	test("returns [] for a non-active state even when a matching issue exists", () => {
		expect(cellItems(list, null, "todo")).toEqual([]);
		expect(cellItems(list, "agent-a", "todo")).toEqual([]);
		expect(cellItems(list, null, "backlog")).toEqual([]);
	});

	// Invariant 5 — agentId narrows to one swimlane row.
	test("narrows to a single agent's cards for an active state", () => {
		expect(cellItems(list, "agent-a", "in_progress").map((w) => w.id)).toEqual([
			"a1",
			"a2",
		]);
	});

	// Invariant 5 — agentId === null is the status-board column: all agents.
	test("returns every agent's cards in that state when agentId is null", () => {
		expect(cellItems(list, null, "in_progress").map((w) => w.id)).toEqual([
			"a1",
			"b1",
			"a2",
		]);
	});
});

describe("boardAgents", () => {
	// Invariant 6 — a swimlane row exists only for an agent holding ≥1 active
	// issue. A backlog/todo-only agent, and an agent with zero issues, get no
	// row; an agent with an in_progress issue does.
	test("includes only agents holding an active issue", () => {
		const agents = [agent("active-a"), agent("todo-only"), agent("empty")];
		const list: Issue[] = [
			ws({ id: "w1", state: "in_progress", assignee: "active-a" }),
			ws({ id: "w2", state: "todo", assignee: "todo-only" }),
			// "empty" holds no issue at all.
			// unassigned active work belongs to no agent row.
			ws({ id: "w3", state: "in_progress", assignee: null }),
		];
		expect(boardAgents(agents, list).map((a) => a.account.id)).toEqual([
			"active-a",
		]);
	});

	// Invariant 6 — `done` is a board column (D1), so a done-only agent DOES get
	// a row (its card sits in the Done column), while a todo/backlog-only agent
	// does not. This pins the corrected contract (the old doc-comment said
	// done-only agents were excluded; the behavior — and D1 — say otherwise).
	test("includes a done-only agent (done is a board column) but not a backlog/todo-only agent", () => {
		const agents = [
			agent("done-only"),
			agent("backlog-only"),
			agent("todo-only"),
		];
		const list: Issue[] = [
			ws({ id: "d1", state: "done", assignee: "done-only" }),
			ws({ id: "bk1", state: "backlog", assignee: "backlog-only" }),
			ws({ id: "td1", state: "todo", assignee: "todo-only" }),
		];
		expect(boardAgents(agents, list).map((a) => a.account.id)).toEqual([
			"done-only",
		]);
	});

	// Invariant 6 — an archived-only agent gets NO row: `archived` is off-board
	// (DL-071), so its card sits in no active column.
	test("excludes an archived-only agent (archived is off-board)", () => {
		const agents = [agent("archived-only")];
		const list: Issue[] = [
			ws({ id: "ar1", state: "archived", assignee: "archived-only" }),
		];
		expect(boardAgents(agents, list)).toEqual([]);
	});
});

describe("laneTotal", () => {
	// Invariant 7 — the lane-head badge counts a state across ALL agents.
	test("counts issues in a state across every agent", () => {
		const list: Issue[] = [
			ws({ id: "w1", state: "in_progress", assignee: "a" }),
			ws({ id: "w2", state: "in_progress", assignee: "b" }),
			ws({ id: "w3", state: "in_progress", assignee: null }),
			ws({ id: "w4", state: "blocked", assignee: "a" }),
			ws({ id: "w5", state: "todo", assignee: "a" }),
		];
		expect(laneTotal(list, "in_progress")).toBe(3);
		expect(laneTotal(list, "blocked")).toBe(1);
		expect(laneTotal(list, "queued")).toBe(0);
	});
});

// Helper: the account ids of an agent list, in order.
const orderIds = (list: readonly Agent[]): string[] =>
	list.map((a) => a.account.id);

describe("treeOrder", () => {
	// Depth-first flatten over agentTree: parent before its children, siblings
	// and roots in stable input order (no sort), every agent exactly once.
	test("a flat set (all roots) keeps input order", () => {
		const agents = [agent("c"), agent("a"), agent("b")];
		expect(orderIds(treeOrder(agents))).toEqual(["c", "a", "b"]);
	});

	test("a parent appears immediately before its children (subtree contiguous)", () => {
		const agents = [
			agent("root"),
			agent("child1", "root"),
			agent("child2", "root"),
		];
		expect(orderIds(treeOrder(agents))).toEqual(["root", "child1", "child2"]);
	});

	test("depth-first: a nested subtree is contiguous", () => {
		// root -> child -> grandchild, plus a sibling root after.
		const agents = [
			agent("root"),
			agent("child", "root"),
			agent("grandchild", "child"),
			agent("other"),
		];
		expect(orderIds(treeOrder(agents))).toEqual([
			"root",
			"child",
			"grandchild",
			"other",
		]);
	});

	test("multi-root: roots in input order, each subtree contiguous", () => {
		const agents = [
			agent("r1"),
			agent("r1a", "r1"),
			agent("r2"),
			agent("r2a", "r2"),
		];
		expect(orderIds(treeOrder(agents))).toEqual(["r1", "r1a", "r2", "r2a"]);
	});

	test("siblings follow input order, not alphabetical (no sort)", () => {
		const agents = [
			agent("root"),
			agent("zeta", "root"),
			agent("alpha", "root"),
		];
		expect(orderIds(treeOrder(agents))).toEqual(["root", "zeta", "alpha"]);
	});

	test("every agent appears exactly once", () => {
		const agents = [
			agent("root"),
			agent("a", "root"),
			agent("b", "a"),
			agent("c"),
			agent("d", "c"),
		];
		const out = treeOrder(agents);
		expect(out).toHaveLength(agents.length);
		expect(new Set(orderIds(out))).toEqual(
			new Set(agents.map((a) => a.account.id)),
		);
	});

	test("a cycle promotes both members to roots, each exactly once", () => {
		// x.parent=y, y.parent=x — agentTree severs the cycle by promoting both
		// to roots; treeOrder must surface each exactly once (the doc-comment's
		// totality claim, pinned at this layer).
		const agents = [agent("x", "y"), agent("y", "x")];
		const out = treeOrder(agents);
		expect(out).toHaveLength(2);
		expect(new Set(orderIds(out))).toEqual(new Set(["x", "y"]));
	});
});

describe("subtreeAgentIds", () => {
	// The subtree membership set: root id + all transitive descendants; the root
	// itself is included; a missing root yields an empty set.
	test("a root with children returns {root, all descendants}", () => {
		const agents = [
			agent("root"),
			agent("child1", "root"),
			agent("child2", "root"),
			agent("grandchild", "child1"),
			agent("other"),
		];
		expect(subtreeAgentIds(agents, "root")).toEqual(
			new Set(["root", "child1", "child2", "grandchild"]),
		);
	});

	test("a leaf root returns just itself", () => {
		const agents = [agent("root"), agent("leaf", "root")];
		expect(subtreeAgentIds(agents, "leaf")).toEqual(new Set(["leaf"]));
	});

	test("a deep chain includes every level", () => {
		const agents = [
			agent("a"),
			agent("b", "a"),
			agent("c", "b"),
			agent("d", "c"),
		];
		expect(subtreeAgentIds(agents, "a")).toEqual(new Set(["a", "b", "c", "d"]));
	});

	test("a mid-tree node returns its own subtree, excluding its ancestor", () => {
		// Querying an intermediate node must include its descendant and EXCLUDE
		// its ancestor — a collect() that wrongly started from the tree root
		// would pass every other test but fail this one.
		const agents = [
			agent("root"),
			agent("child", "root"),
			agent("grandchild", "child"),
		];
		expect(subtreeAgentIds(agents, "child")).toEqual(
			new Set(["child", "grandchild"]),
		);
	});

	test("a missing rootAgentId returns an empty set", () => {
		const agents = [agent("a"), agent("b", "a")];
		expect(subtreeAgentIds(agents, "nope")).toEqual(new Set<string>());
	});

	test("the set includes the root itself", () => {
		const agents = [agent("root"), agent("child", "root")];
		expect(subtreeAgentIds(agents, "root").has("root")).toBe(true);
	});
});

// A minimal PullRequest — only forgeState (the openPrs predicate) and number
// (row identity in assertions) carry meaning; the rest are inert defaults.
function prOf(over: Partial<PullRequest>): PullRequest {
	return {
		forge: { provider: "github", host: "github.com" },
		repo: "acme/repo",
		number: 0,
		title: "t",
		forgeState: "open",
		url: "https://example.test/pr",
		headRef: "h",
		baseRef: "b",
		forgeAccount: "acct",
		draft: false,
		reviews: [],
		threads: [],
		...over,
	};
}

// prRows / prRowGroups / prCount build the PRs-tab partition: one row per OPEN
// PR (any issue lifecycle state), grouped by assignee in treeOrder with an
// Unassigned group last, and a count that honors an optional subtree scope.
describe("prRows", () => {
	test("an issue with two open PRs yields two rows in prs order", () => {
		const a = prOf({ number: 1 });
		const b = prOf({ number: 2 });
		const rows = prRows([ws({ id: "i1", state: "in_review", prs: [a, b] })]);
		expect(rows).toHaveLength(2);
		expect(rows.map((r) => r.pr.number)).toEqual([1, 2]);
		expect(rows.every((r) => r.issue.id === "i1")).toBe(true);
	});

	test("merged and closed PRs are excluded", () => {
		const open = prOf({ number: 1, forgeState: "open" });
		const merged = prOf({ number: 2, forgeState: "merged" });
		const closed = prOf({ number: 3, forgeState: "closed" });
		const rows = prRows([
			ws({ id: "i1", state: "in_review", prs: [open, merged, closed] }),
		]);
		expect(rows.map((r) => r.pr.number)).toEqual([1]);
	});

	test("issue order then prs order is preserved across issues", () => {
		const rows = prRows([
			ws({ id: "i1", state: "in_progress", prs: [prOf({ number: 1 })] }),
			ws({
				id: "i2",
				state: "in_review",
				prs: [prOf({ number: 2 }), prOf({ number: 3 })],
			}),
		]);
		expect(rows.map((r) => `${r.issue.id}:${r.pr.number}`)).toEqual([
			"i1:1",
			"i2:2",
			"i2:3",
		]);
	});

	test("a pre-active issue with an open PR still contributes (predicate is on the PR)", () => {
		const rows = prRows([
			ws({ id: "i1", state: "backlog", prs: [prOf({ number: 9 })] }),
		]);
		expect(rows.map((r) => r.pr.number)).toEqual([9]);
	});
});

describe("prRowGroups", () => {
	test("groups follow treeOrder (parent before child), each with its rows", () => {
		const agents = [agent("root"), agent("child", "root")];
		const all = [
			ws({
				id: "c1",
				state: "in_review",
				assignee: "child",
				prs: [prOf({ number: 2 })],
			}),
			ws({
				id: "r1",
				state: "in_review",
				assignee: "root",
				prs: [prOf({ number: 1 })],
			}),
		];
		const groups = prRowGroups(agents, all);
		expect(groups.map((g) => g.agent?.account.id)).toEqual(["root", "child"]);
		expect(groups[0]?.rows.map((r) => r.pr.number)).toEqual([1]);
		expect(groups[1]?.rows.map((r) => r.pr.number)).toEqual([2]);
	});

	test("an agent with no open-PR rows is omitted from the sequence", () => {
		const agents = [agent("a"), agent("b")];
		const all = [
			ws({
				id: "i1",
				state: "in_review",
				assignee: "b",
				prs: [prOf({ number: 1 })],
			}),
		];
		const groups = prRowGroups(agents, all);
		expect(groups.map((g) => g.agent?.account.id)).toEqual(["b"]);
	});

	test("the Unassigned group is last when present", () => {
		const agents = [agent("a")];
		const all = [
			ws({
				id: "u1",
				state: "in_review",
				assignee: null,
				prs: [prOf({ number: 2 })],
			}),
			ws({
				id: "a1",
				state: "in_review",
				assignee: "a",
				prs: [prOf({ number: 1 })],
			}),
		];
		const groups = prRowGroups(agents, all);
		expect(groups.map((g) => g.agent?.account.id ?? "UNASSIGNED")).toEqual([
			"a",
			"UNASSIGNED",
		]);
		expect(groups[1]?.rows.map((r) => r.pr.number)).toEqual([2]);
	});

	test("no unassigned rows → no Unassigned group", () => {
		const agents = [agent("a")];
		const all = [
			ws({
				id: "a1",
				state: "in_review",
				assignee: "a",
				prs: [prOf({ number: 1 })],
			}),
		];
		const groups = prRowGroups(agents, all);
		expect(groups.every((g) => g.agent !== null)).toBe(true);
	});
});

describe("prCount", () => {
	test("unscoped: total open-PR rows across issues (unassigned included)", () => {
		const all = [
			ws({
				id: "a1",
				state: "in_review",
				assignee: "a",
				prs: [prOf({ number: 1 }), prOf({ number: 2 })],
			}),
			ws({
				id: "u1",
				state: "in_review",
				assignee: null,
				prs: [prOf({ number: 3 })],
			}),
			ws({
				id: "m1",
				state: "done",
				assignee: "a",
				prs: [prOf({ number: 4, forgeState: "merged" })],
			}),
		];
		expect(prCount(all)).toBe(3);
	});

	test("scoped: counts only rows whose assignee is in scope, unassigned excluded", () => {
		const all = [
			ws({
				id: "a1",
				state: "in_review",
				assignee: "a",
				prs: [prOf({ number: 1 })],
			}),
			ws({
				id: "b1",
				state: "in_review",
				assignee: "b",
				prs: [prOf({ number: 2 })],
			}),
			ws({
				id: "u1",
				state: "in_review",
				assignee: null,
				prs: [prOf({ number: 3 })],
			}),
		];
		expect(prCount(all, new Set(["a"]))).toBe(1);
		expect(prCount(all, new Set(["a", "b"]))).toBe(2);
	});

	test("an empty scope set counts nothing", () => {
		const all = [
			ws({
				id: "a1",
				state: "in_review",
				assignee: "a",
				prs: [prOf({ number: 1 })],
			}),
		];
		expect(prCount(all, new Set<string>())).toBe(0);
	});

	// Frozen-contract edge (Record B §T2): unscoped prCount is prRows(all).length —
	// it counts every open-PR row and is deliberately NOT agent-aware (the signature
	// takes no agent set). prRowGroups, by contrast, only emits rows whose assignee
	// is null or matches a known agent, so an issue with a non-null assignee that is
	// absent from the agent set is counted by the badge yet dropped from the render.
	// This is out of contract for real data — assignee is a trusted account id and a
	// miss is a store bug — but the asymmetry is intentional and pinned here so a
	// future change to either side is a conscious one.
	test("unscoped: an orphan (unknown) assignee is still counted (== prRows length)", () => {
		const all = [
			ws({
				id: "o1",
				state: "in_review",
				assignee: "ghost",
				prs: [prOf({ number: 1 })],
			}),
		];
		expect(prCount(all)).toBe(prRows(all).length);
		expect(prCount(all)).toBe(1);
		// The same orphan row is dropped from the grouped render (no matching agent).
		const groups = prRowGroups([agent("a")], all);
		expect(groups.flatMap((g) => g.rows)).toHaveLength(0);
	});
});
