// Shared display constants for the Compass ADE UI: the board lane order and the
// label/color lookups every surface reads. Static string-keyed tables → Record.

import type { IssueTab, PinnedAgent, RightSidebarTab } from "./store";
import type { Agent, AgentState, IssueState } from "./stub-data";

/** Board columns, left to right — the ACTIVE subset of the issue lifecycle
 *  (design D1). Backlog + Todo are the pre-active tier and live in the Backlog
 *  view (`BACKLOG_STATES`), not the board grid. */
export interface Lane {
	state: IssueState;
	label: string;
	/** The CSS state color variable name, for the column dot. */
	color: string;
}

export const BOARD_LANES: Lane[] = [
	{ state: "queued", label: "Queued", color: "var(--cx-issue-queued)" },
	{ state: "blocked", label: "Blocked", color: "var(--cx-issue-blocked)" },
	{
		state: "in_progress",
		label: "In progress",
		color: "var(--cx-issue-in_progress)",
	},
	{
		state: "in_review",
		label: "In review",
		color: "var(--cx-issue-in_review)",
	},
	{ state: "done", label: "Done", color: "var(--cx-issue-done)" },
];

/** The pre-active tier, in Backlog-view display order (Todo first, then
 *  Backlog). Todo is the global pool of promoted-but-unassigned tasks; Backlog
 *  is the un-promoted tier (design D1). Neither renders on the board grid. */
export const BACKLOG_STATES: readonly IssueState[] = ["todo", "backlog"];

/** Human labels for the agent dot (design D9/T10). Keyed on the full union so a
 *  new agent state can't ship without a label. */
export const AGENT_STATE_LABEL: Record<AgentState, string> = {
	working: "Working",
	idle: "Idle",
	waiting: "Waiting for input",
	done: "Done",
	paused: "Paused",
	stopped: "Stopped",
	error: "Error",
	disconnected: "Disconnected",
};

/** Activity-bar group: fleet tabs render above the divider, issue below
 *  (design dock-in-sidebar D2). */
export type RightTabGroup = "fleet" | "issue";

/** An icon-per-tab item in the right-sidebar activity bar (design D5/T6,
 *  dock-in-sidebar D2), mirroring Orca's `ActivityBarItem`. The icon is a glyph
 *  string, matching the UI's existing glyph-icon convention (file rows, the
 *  branch dropdown). */
export interface ActivityBarItem {
	id: RightSidebarTab;
	/** Single-glyph icon. */
	icon: string;
	/** Short label under the icon / for the tooltip. */
	title: string;
	/** Activity-bar group: fleet renders above the divider, issue below. */
	group: RightTabGroup;
	/** Fleet agent tabs: the agent whose `StateDot` badges the tab icon. On an
	 *  unreachable pin this is the pinned id that resolves to no visible agent, so
	 *  it carries no live `StateDot` (SEA-1645). */
	agentId?: string;
	/** Fleet agent tabs (SEA-1645): true when the pinned agent no longer resolves
	 *  to a visible agent (dead / despawned / filtered out). Absent/false = live.
	 *  The activity bar and the pane render the unreachable state for a marked
	 *  item. */
	unreachable?: boolean;
}

/** The STATIC right-sidebar tabs — the ones present regardless of the pin set:
 *  `status` (the fleet metrics pane) in the fleet group, and the card-scoped
 *  issue tabs (Files / VCS / PR). The agent conversation tabs are no longer
 *  hardcoded here — they are derived per pin from the store's pin set
 *  (`rightTabGroups()`), so the fleet group is a configurable pin layer, not a
 *  fixed Supervisor pin (Record A §T2). */
export type StaticRightTab = "status" | IssueTab;

/** The static tabs in activity-bar order, keyed on the static-tab union in a
 *  mapped object so TypeScript rejects the module unless EVERY static tab has an
 *  activity-bar entry. The dynamic pin items are built at the store from the pin
 *  set (`fleetItemForAgent`), so the mapped object keys the STATIC ids only —
 *  the open `agent:${string}` arm of `RightSidebarTab` can't be enumerated. */
export const RIGHT_SIDEBAR_TAB_BY_ID: {
	[K in StaticRightTab]: ActivityBarItem & { id: K };
} = {
	status: { id: "status", icon: "▦", title: "Fleet status", group: "fleet" },
	files: { id: "files", icon: "🗀", title: "Files", group: "issue" },
	vcs: { id: "vcs", icon: "⎇", title: "Version control", group: "issue" },
	pr: { id: "pr", icon: "⇄", title: "Pull request", group: "issue" },
};

/** The static issue-group items, in declaration order — the card-scoped tabs the
 *  activity bar renders below the divider (design dock-in-sidebar D2). */
export const RIGHT_SIDEBAR_ISSUE_ITEMS: readonly ActivityBarItem[] =
	Object.values(RIGHT_SIDEBAR_TAB_BY_ID).filter((t) => t.group === "issue");

/** Build the fleet activity-bar item for a RESOLVABLE pinned agent (Record A
 *  §T2; SEA-1645 P1). The tab id is the `agent:`-prefixed account id (the open
 *  arm of `RightSidebarTab`); the icon is the agent handle's initial (matching
 *  the UI's glyph-icon convention — a per-agent glyph, no hardcoded Supervisor
 *  ◆), and the title is the LIVE agent handle. The item is left
 *  unmarked (`unreachable` absent) so its `agentId` badges a real `StateDot`.
 *  An unresolvable pin is built by `unreachableFleetItem` instead — so a
 *  marked item can carry an `agentId` that resolves no agent. */
export function fleetItemForAgent(agent: Agent): ActivityBarItem {
	return {
		id: `agent:${agent.account.id}`,
		icon: (agent.account.handle.at(0) ?? "?").toUpperCase(),
		title: agent.account.handle,
		group: "fleet",
		agentId: agent.account.id,
	};
}

/** Build the fleet activity-bar item for an UNRESOLVABLE pin (SEA-1645 P1): its
 *  agent no longer resolves to a visible agent (dead / despawned / filtered
 *  out). The label is the handle cached at pin time (P0), so the item shows the
 *  human name the user pinned rather than an opaque id (a legacy `{ id, handle:
 *  id }` fallback pin degrades to the id). The item is marked `unreachable` so
 *  the activity bar and the pane render the unreachable state; its `agentId`
 *  intentionally resolves no agent (no live `StateDot`). */
export function unreachableFleetItem(pin: PinnedAgent): ActivityBarItem {
	return {
		id: `agent:${pin.id}`,
		icon: (pin.handle.at(0) ?? "?").toUpperCase(),
		title: pin.handle,
		group: "fleet",
		agentId: pin.id,
		unreachable: true,
	};
}
