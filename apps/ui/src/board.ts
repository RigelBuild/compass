// The board partition — the pure core of the workstream state model (design
// D1). One source of truth for "what's active (a board column) vs pre-active
// (Backlog view) vs done", read by the Bridge board and the Backlog/Done views
// so the partition can never drift between surfaces.
//
// Pure over an injected workstream list (no fixture import, no store), so the
// whole D1 contract is unit-testable and the same functions serve the fixture
// today and the @compass/client stream later.

import { BOARD_LANES } from "./constants";
import type { Agent, Workstream, WorkstreamState } from "./stub-data";

/** The active states, in board display order — derived from the one lane list
 *  so a lane edit can never desync the predicate. */
export const ACTIVE_STATES: readonly WorkstreamState[] = BOARD_LANES.map(
	(l) => l.state,
);

/** Whether a state is an active board column (design D1). `backlog`/`todo` are
 *  the pre-active tier and are not on the board. */
export function isActiveState(state: WorkstreamState): boolean {
	return ACTIVE_STATES.includes(state);
}

/** Whether a state is in the pre-active Backlog tier (design D1). */
export function isBacklogState(state: WorkstreamState): boolean {
	return state === "backlog" || state === "todo";
}

/** Workstreams on the active board (any board column), fixture order preserved. */
export function activeWorkstreams(all: readonly Workstream[]): Workstream[] {
	return all.filter((w) => isActiveState(w.state));
}

/** Workstreams in the pre-active tier (Backlog + Todo), fixture order preserved. */
export function backlogWorkstreams(all: readonly Workstream[]): Workstream[] {
	return all.filter((w) => isBacklogState(w.state));
}

/** The agents that hold at least one active board workstream — the swimlane
 *  rows. Moat agents (supervisor/warden) own no board lanes, so an agent with
 *  no active workstream is excluded, as is one whose only work is pre-active
 *  (Backlog/Todo). `done` is an active column, so a done-only agent still gets
 *  a row (its card sits in the Done column). */
export function boardAgents(
	agents: readonly Agent[],
	all: readonly Workstream[],
): Agent[] {
	return agents.filter((a) =>
		all.some((w) => w.assignee === a.account.id && isActiveState(w.state)),
	);
}

/** The cards for one swimlane cell: workstreams in `state`, optionally narrowed
 *  to one agent (agentId === null → the status-board column, all agents). Only
 *  active states yield cells; a non-board state always yields []. */
export function cellItems(
	all: readonly Workstream[],
	agentId: string | null,
	state: WorkstreamState,
): Workstream[] {
	if (!isActiveState(state)) return [];
	return all.filter(
		(w) => w.state === state && (agentId === null || w.assignee === agentId),
	);
}

/** The count in a board column across all agents (the lane-head badge). */
export function laneTotal(
	all: readonly Workstream[],
	state: WorkstreamState,
): number {
	return all.filter((w) => w.state === state).length;
}
