import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import { type AppStore, createAppStore } from "../store";
import type { FileNode, Issue, IssueState } from "../stub-data";
import { STUB_AGENTS } from "../stub-data";
import { testQueryClient } from "../test-support";
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

describe("rightTabGroups() derivation (Record A §T2)", () => {
	// The activity-bar groups are a STORE derivation now (rightTabGroups()), not a
	// static table: the fleet group is the resolvable pins in pin order + the
	// static status item; the issue group is the static issue items. Build a store
	// inside a reactive root, pin agents, and assert the derived partition.
	const withGroups = (
		body: (
			groups: readonly {
				group: string;
				items: readonly {
					id: string;
					agentId?: string;
					unreachable?: boolean;
				}[];
			}[],
			store: AppStore,
		) => void,
		pins: readonly string[] = [],
	): void => {
		// A unique workspace key per call, cleared around the body, so the
		// process-wide localStorage can't leak pins between cases.
		const workspaceKey = `rtg-${Math.random().toString(36).slice(2)}`;
		globalThis.localStorage.removeItem(`compass.pinnedAgents.${workspaceKey}`);
		createRoot((dispose) => {
			const store = createAppStore({
				workspaceKey,
				queryClient: testQueryClient(),
			});
			for (const id of pins) store.pinAgent(id);
			try {
				body(store.rightTabGroups(), store);
			} finally {
				dispose();
			}
		});
		globalThis.localStorage.removeItem(`compass.pinnedAgents.${workspaceKey}`);
	};

	// The two visible fixture agents to pin — both resolve in STUB_AGENTS, so the
	// derivation must surface a fleet item for each, in pin order.
	const SUP = "acc-supervisor";
	const WARDEN = "acc-warden";

	// Fleet must sit ABOVE issue (D2). A swapped order renders the bar upside-down.
	test("orders the groups fleet-first, issue-second", () => {
		withGroups((groups) => {
			expect(groups.map((g) => g.group)).toEqual(["fleet", "issue"]);
		});
	});

	// Resolvable pins → fleet items in pin order, then the static status item;
	// issue items are the static Files/VCS/PR. Nothing missing, duplicated, or
	// invented across the partition.
	test("resolvable pins yield fleet items in pin order + status; issue items static", () => {
		withGroups(
			(groups) => {
				const ids = groups.flatMap((g) => g.items.map((item) => item.id));
				expect(ids).toEqual([
					`agent:${SUP}`,
					`agent:${WARDEN}`,
					"status",
					"files",
					"vcs",
					"pr",
				]);
				// No duplicate, independent of order.
				expect(new Set(ids).size).toBe(ids.length);
			},
			[SUP, WARDEN],
		);
	});

	// An unresolvable pin (no visible agent for the id) now surfaces a MARKED
	// activity-bar item (SEA-1645) — the derivation no longer filters. The
	// resolvable pin beside it is unmarked, so the marking is selective.
	test("an unresolvable pin surfaces a marked fleet item", () => {
		withGroups(
			(groups) => {
				const fleet = groups.find((g) => g.group === "fleet");
				const items = fleet?.items ?? [];
				const fleetIds = items.map((item) => item.id);
				// Both pins surface, in pin order, before status.
				expect(fleetIds).toEqual([`agent:${SUP}`, "agent:acc-ghost", "status"]);
				// The ghost item is marked; the resolvable pin is not.
				const ghost = items.find((item) => item.id === "agent:acc-ghost");
				expect(ghost?.unreachable).toBe(true);
				const sup = items.find((item) => item.id === `agent:${SUP}`);
				expect(sup?.unreachable).toBeUndefined();
			},
			[SUP, "acc-ghost"],
		);
	});

	// With no pins, the fleet group is just the static status item; the issue
	// group is unchanged. Each fleet PIN item carries an agentId that resolves a
	// real stub agent; status and every issue item carry none.
	test("fleet pin items carry a resolving agentId; status and issue items do not", () => {
		withGroups(
			(groups) => {
				const fleet = groups.find((g) => g.group === "fleet");
				const issue = groups.find((g) => g.group === "issue");
				expect(fleet).toBeDefined();
				expect(issue).toBeDefined();
				// Every issue pane is agent-less.
				for (const item of issue?.items ?? []) {
					expect(item.agentId).toBeUndefined();
				}
				// The UNMARKED fleet items with an agentId are exactly the resolvable
				// pins, each resolving a real stub agent. A marked (unreachable) item
				// may carry an agentId that resolves no stub (SEA-1645), so exclude it.
				const withAgent = (fleet?.items ?? []).filter(
					(item) => item.agentId !== undefined && item.unreachable !== true,
				);
				expect(withAgent.map((item) => item.id)).toEqual([
					`agent:${SUP}`,
					`agent:${WARDEN}`,
				]);
				for (const item of withAgent) {
					expect(STUB_AGENTS.some((a) => a.account.id === item.agentId)).toBe(
						true,
					);
				}
				// Status is present as a fleet item and carries no agentId.
				const status = (fleet?.items ?? []).find(
					(item) => item.id === "status",
				);
				expect(status).toBeDefined();
				expect(status?.agentId).toBeUndefined();
			},
			[SUP, WARDEN],
		);
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
