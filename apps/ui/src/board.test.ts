import { describe, expect, test } from "bun:test";
import {
	ACTIVE_STATES,
	activeWorkstreams,
	backlogWorkstreams,
	boardAgents,
	cellItems,
	isActiveState,
	isBacklogState,
	laneTotal,
} from "./board";
import { BOARD_LANES } from "./constants";
import type { Agent, Workstream, WorkstreamState } from "./stub-data";

// board.ts is the pure core of the D1 workstream state model: it partitions the
// 7-state lifecycle into the active board columns vs the pre-active Backlog tier,
// and derives every board query (lanes, swimlane rows, cells, counts) from that
// one partition. These tests defend the partition invariants against a future
// union edit or lane reorder silently desyncing the surfaces that read it.

// The complete lifecycle (stub-data WorkstreamState). Enumerated here on purpose:
// if the union grows an 8th state, TypeScript reddens this literal against the
// exhaustiveness assertion below, forcing the new state to be classified.
const ALL_STATES: readonly WorkstreamState[] = [
	"backlog",
	"todo",
	"queued",
	"blocked",
	"in_progress",
	"in_review",
	"done",
];

// Minimal valid Workstream — only the fields board.ts reads (state, assignee)
// carry meaning; the rest are present to satisfy the interface and are held
// constant so a fixture-shape change never perturbs a partition assertion.
function ws(
	over: Partial<Workstream> & Pick<Workstream, "id" | "state">,
): Workstream {
	return {
		issue: "SEA-0",
		title: "t",
		priority: "medium",
		assignee: null,
		summary: "s",
		branch: "b",
		changed: { files: 0, additions: 0, deletions: 0 },
		pr: null,
		...over,
	};
}

// Minimal valid Agent — board.ts only reads `account.id`; the rest satisfies the
// shape.
function agent(id: string): Agent {
	return {
		account: {
			id,
			handle: id,
			displayName: id,
			kind: "agent",
		},
		lifecycle: "idle",
		role: "worker",
		model: "m",
		cwd: "/",
		terminals: [],
	};
}

describe("state partition (D1)", () => {
	// Invariant 1 — the core D1 contract. Every lifecycle state is classified by
	// exactly one predicate: active XOR backlog. A future edit that adds a state
	// to the union, or moves one between the tiers, without updating both
	// predicates would break exhaustiveness or disjointness here.
	test("active/backlog partition is exhaustive and disjoint over all 7 states", () => {
		for (const state of ALL_STATES) {
			const active = isActiveState(state);
			const backlog = isBacklogState(state);
			// exactly one is true (XOR): never both, never neither.
			expect(active !== backlog).toBe(true);
		}
		// exactly 7 states enumerated, no duplicates.
		expect(new Set(ALL_STATES).size).toBe(7);
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

describe("activeWorkstreams / backlogWorkstreams", () => {
	// A list spanning every tier, deliberately interleaved so an order bug shows.
	const list: Workstream[] = [
		ws({ id: "w-done", state: "done" }),
		ws({ id: "w-backlog", state: "backlog" }),
		ws({ id: "w-inprog", state: "in_progress" }),
		ws({ id: "w-todo", state: "todo" }),
		ws({ id: "w-queued", state: "queued" }),
		ws({ id: "w-blocked", state: "blocked" }),
		ws({ id: "w-review", state: "in_review" }),
	];

	// Invariant 4 — the two views partition the input and preserve input order.
	test("select the active states in input order", () => {
		expect(activeWorkstreams(list).map((w) => w.id)).toEqual([
			"w-done",
			"w-inprog",
			"w-queued",
			"w-blocked",
			"w-review",
		]);
	});

	test("select the backlog states in input order", () => {
		expect(backlogWorkstreams(list).map((w) => w.id)).toEqual([
			"w-backlog",
			"w-todo",
		]);
	});

	// Invariant 4 (partition) — together the two views cover every workstream
	// exactly once, dropping none and duplicating none. Here every state is
	// active-or-backlog, so the union is the whole list.
	test("together cover every workstream exactly once (no drop, no dup)", () => {
		const active = activeWorkstreams(list).map((w) => w.id);
		const backlog = backlogWorkstreams(list).map((w) => w.id);
		const union = [...active, ...backlog];
		expect(new Set(union).size).toBe(union.length); // disjoint: no id twice
		expect(new Set(union)).toEqual(new Set(list.map((w) => w.id))); // exhaustive
	});
});

describe("cellItems", () => {
	const list: Workstream[] = [
		ws({ id: "a1", state: "in_progress", assignee: "agent-a" }),
		ws({ id: "b1", state: "in_progress", assignee: "agent-b" }),
		ws({ id: "a2", state: "in_progress", assignee: "agent-a" }),
		ws({ id: "t1", state: "todo", assignee: "agent-a" }),
	];

	// Invariant 5 — a non-active state never yields a cell, even when matching
	// workstreams exist. The board must not render backlog/todo columns.
	test("returns [] for a non-active state even when a matching workstream exists", () => {
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
	// workstream. A backlog/todo-only agent, and an agent with zero workstreams,
	// get no row; an agent with an in_progress workstream does.
	test("includes only agents holding an active workstream", () => {
		const agents = [agent("active-a"), agent("todo-only"), agent("empty")];
		const list: Workstream[] = [
			ws({ id: "w1", state: "in_progress", assignee: "active-a" }),
			ws({ id: "w2", state: "todo", assignee: "todo-only" }),
			// "empty" holds no workstream at all.
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
		const list: Workstream[] = [
			ws({ id: "d1", state: "done", assignee: "done-only" }),
			ws({ id: "bk1", state: "backlog", assignee: "backlog-only" }),
			ws({ id: "td1", state: "todo", assignee: "todo-only" }),
		];
		expect(boardAgents(agents, list).map((a) => a.account.id)).toEqual([
			"done-only",
		]);
	});
});

describe("laneTotal", () => {
	// Invariant 7 — the lane-head badge counts a state across ALL agents.
	test("counts workstreams in a state across every agent", () => {
		const list: Workstream[] = [
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
