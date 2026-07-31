// Shared display constants for the Compass ADE UI: the board lane order and the
// label/color lookups every surface reads. Static string-keyed tables → Record.

import type { RightSidebarTab } from "./store";
import type { AgentState, IssueState } from "./stub-data";

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
	{ state: "queued", label: "Queued", color: "var(--st-paused)" },
	{ state: "blocked", label: "Blocked", color: "var(--st-blocked)" },
	{ state: "in_progress", label: "In progress", color: "var(--st-working)" },
	{ state: "in_review", label: "In review", color: "var(--st-review)" },
	{ state: "done", label: "Done", color: "var(--st-merged)" },
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
	/** Fleet agent tabs: the agent whose `StateDot` badges the tab icon. */
	agentId?: string;
}

/** The right-sidebar tabs in activity-bar order (design dock-in-sidebar
 *  D2/D3): the fleet group first (Supervisor · Warden — always-on agent
 *  conversations — then Status, the fleet metrics pane), then
 *  the issue group (Files with a search box, VCS with commit history, PR
 *  with its checks). Keyed on the full `RightSidebarTab` union in a mapped
 *  object, so TypeScript rejects the module unless EVERY tab has an
 *  activity-bar entry (an array of `ActivityBarItem` only validates the ids
 *  that are present — it can't enforce that none is missing). Exported for the
 *  D5 chrome-hiding predicate. */
export const RIGHT_SIDEBAR_TAB_BY_ID: {
	[K in RightSidebarTab]: ActivityBarItem & { id: K };
} = {
	supervisor: {
		id: "supervisor",
		icon: "◆",
		title: "Supervisor",
		group: "fleet",
		agentId: "acc-supervisor",
	},
	warden: {
		id: "warden",
		icon: "🛡",
		title: "Warden",
		group: "fleet",
		agentId: "acc-warden",
	},
	status: { id: "status", icon: "▦", title: "Fleet status", group: "fleet" },
	files: { id: "files", icon: "🗀", title: "Files", group: "issue" },
	vcs: { id: "vcs", icon: "⎇", title: "Version control", group: "issue" },
	pr: { id: "pr", icon: "⇄", title: "Pull request", group: "issue" },
};

/** The activity bar as ordered groups (design dock-in-sidebar D2): fleet first,
 *  issue second, each carrying its items in declaration order (JS
 *  preserves string-key insertion order). One source of truth — adding a tab to
 *  the union forces an entry above, and it appears in its group here
 *  automatically. The activity-bar nav renders a divider between groups. */
export const RIGHT_SIDEBAR_TAB_GROUPS: readonly {
	group: RightTabGroup;
	items: readonly ActivityBarItem[];
}[] = (["fleet", "issue"] as const).map((group) => ({
	group,
	items: Object.values(RIGHT_SIDEBAR_TAB_BY_ID).filter(
		(t) => t.group === group,
	),
}));
