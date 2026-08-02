// The board partition — the pure core of the issue state model (design D1). One
// source of truth for "what's active (a board column) vs pre-active (Backlog
// view) vs done", read by the Bridge board and the Backlog/Done views so the
// partition can never drift between surfaces.
//
// Pure over an injected issue list (no fixture import, no store), so the whole
// D1 contract is unit-testable and the same functions serve the fixture today
// and the @compass/client stream later.

import { openPrs } from "./board-render";
import { BOARD_LANES } from "./constants";
import type {
	Agent,
	AgentTreeNode,
	Issue,
	IssueState,
	PullRequest,
} from "./stub-data";
import { agentTree } from "./stub-data";

/** The active states, in board display order — derived from the one lane list
 *  so a lane edit can never desync the predicate. */
export const ACTIVE_STATES: readonly IssueState[] = BOARD_LANES.map(
	(l) => l.state,
);

/** Whether a state is an active board column (design D1). `backlog`/`todo` are
 *  the pre-active tier and are not on the board. */
export function isActiveState(state: IssueState): boolean {
	return ACTIVE_STATES.includes(state);
}

/** Whether a state is in the pre-active Backlog tier (design D1). */
export function isBacklogState(state: IssueState): boolean {
	return state === "backlog" || state === "todo";
}

/** Issues on the active board (any board column), fixture order preserved. */
export function activeIssues(all: readonly Issue[]): Issue[] {
	return all.filter((w) => isActiveState(w.state));
}

/** Issues in the pre-active tier (Backlog + Todo), fixture order preserved. */
export function backlogIssues(all: readonly Issue[]): Issue[] {
	return all.filter((w) => isBacklogState(w.state));
}

/** The agents that hold at least one active board issue — the swimlane rows.
 *  An agent is excluded when it has no active issue (its only work is
 *  pre-active — Backlog/Todo — or it has none at all); exclusion is by
 *  no-active-issue, never by role. `done` is an active column, so a done-only
 *  agent still gets a row (its card sits in the Done column). */
export function boardAgents(
	agents: readonly Agent[],
	all: readonly Issue[],
): Agent[] {
	return agents.filter((a) =>
		all.some((w) => w.assignee === a.account.id && isActiveState(w.state)),
	);
}

/** The cards for one swimlane cell: issues in `state`, optionally narrowed to
 *  one agent (agentId === null → the status-board column, all agents). Only
 *  active states yield cells; a non-board state always yields []. */
export function cellItems(
	all: readonly Issue[],
	agentId: string | null,
	state: IssueState,
): Issue[] {
	if (!isActiveState(state)) return [];
	return all.filter(
		(w) => w.state === state && (agentId === null || w.assignee === agentId),
	);
}

/** The count in a board column across all agents (the lane-head badge). */
export function laneTotal(all: readonly Issue[], state: IssueState): number {
	return all.filter((w) => w.state === state).length;
}

/** The swimlane ordering: every agent in depth-first tree order (parent before
 *  its children, a subtree contiguous), siblings and roots in the stable input
 *  order `agentTree` fixes (no sort). Orders the FULL agent set — filtering to
 *  agents with active issues stays `boardAgents`' separate concern, applied
 *  after ordering. Total: `agentTree` surfaces every agent exactly once (cycle
 *  members and dangling parents are promoted to roots), so each appears once. */
export function treeOrder(agents: readonly Agent[]): Agent[] {
	const ordered: Agent[] = [];
	const visit = (node: AgentTreeNode): void => {
		ordered.push(node.agent);
		for (const child of node.children) visit(child);
	};
	for (const root of agentTree(agents)) visit(root);
	return ordered;
}

/** The subtree membership set for `rootAgentId` — its own id plus every
 *  transitive descendant (a subtree includes its own root, so scoping the board
 *  to a subtree keeps the root's lane). The board's subtree-filter predicate.
 *  Precondition: `rootAgentId` is a real agent; a root not present in `agents`
 *  has no subtree to scope to, so this returns an empty set. */
export function subtreeAgentIds(
	agents: readonly Agent[],
	rootAgentId: string,
): ReadonlySet<string> {
	const ids = new Set<string>();
	const collect = (node: AgentTreeNode): void => {
		ids.add(node.agent.account.id);
		for (const child of node.children) collect(child);
	};
	const find = (nodes: readonly AgentTreeNode[]): AgentTreeNode | undefined => {
		for (const node of nodes) {
			if (node.agent.account.id === rootAgentId) return node;
			const hit = find(node.children);
			if (hit) return hit;
		}
		return undefined;
	};
	const root = find(agentTree(agents));
	if (root) collect(root);
	return ids;
}

/** One PRs-tab row: an OPEN PR paired with its owning issue. `Issue.prs` holds
 *  every PR opened for an issue, so an issue with two open PRs yields two rows —
 *  the PRs tab is the surface where `primaryPr`'s single-chip compression must
 *  NOT apply (a second open PR would otherwise be invisible). */
export type PrRow = { issue: Issue; pr: PullRequest };

/** Every open-PR row, across issues in ANY lifecycle state — the predicate is on
 *  the PR (`openPrs`), not the issue. Issue order then `prs` order preserved. */
export function prRows(all: readonly Issue[]): PrRow[] {
	return all.flatMap((issue) => openPrs(issue).map((pr) => ({ issue, pr })));
}

/** The PRs-tab groups: `prRows` grouped by `issue.assignee`, group sequence =
 *  `treeOrder(agents)` (C's contract) filtered to agents with ≥1 row, then one
 *  `agent: null` ("Unassigned") group LAST iff it has rows. */
export function prRowGroups(
	agents: readonly Agent[],
	all: readonly Issue[],
): { agent: Agent | null; rows: PrRow[] }[] {
	const rows = prRows(all);
	const groups: { agent: Agent | null; rows: PrRow[] }[] = [];
	for (const agent of treeOrder(agents)) {
		const owned = rows.filter((r) => r.issue.assignee === agent.account.id);
		if (owned.length > 0) groups.push({ agent, rows: owned });
	}
	const unassigned = rows.filter((r) => r.issue.assignee === null);
	if (unassigned.length > 0) groups.push({ agent: null, rows: unassigned });
	return groups;
}

/** The PRs-tab count badge — the number of rows the tab shows, so it equals the
 *  visible row count. Unscoped: `prRows(all).length`. With C's `subtreeAgentIds`
 *  scope, counts only rows whose `issue.assignee` is in scope (unassigned rows
 *  are excluded when scoped, matching the filtered tab). */
export function prCount(
	all: readonly Issue[],
	scope?: ReadonlySet<string>,
): number {
	const rows = prRows(all);
	if (!scope) return rows.length;
	return rows.filter(
		(r) => r.issue.assignee !== null && scope.has(r.issue.assignee),
	).length;
}
