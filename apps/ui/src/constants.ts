// Shared display constants for the Compass ADE UI: the board lane order and the
// label/color lookups every surface reads. Static string-keyed tables → Record.

import type { GlyphName } from "./components/Glyph";
import type { IssueTab, PinnedAgent } from "./store";
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

/** PR-lifecycle columns for the Bridge PRs view (design D1; T6). A separate axis
 *  from the issue-lifecycle BOARD_LANES: a PR moves across these as it heads to
 *  merge. Colors reuse the issue-lifecycle namespace + the accent for "ready". */
export type PrLifecycle = "in_progress" | "in_review" | "ready" | "merged";

export const PR_LANES: { state: PrLifecycle; label: string; color: string }[] =
	[
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
		{ state: "ready", label: "Ready to merge", color: "var(--cx-accent)" },
		{ state: "merged", label: "Merged", color: "var(--cx-issue-done)" },
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

/** The shared fields of every activity-bar item (design D5/T6, dock-in-sidebar
 *  D2), mirroring Orca's `ActivityBarItem`. */
interface ActivityBarItemBase {
	/** Short label under the icon / for the tooltip. */
	title: string;
	/** Activity-bar group: fleet renders above the divider, issue below. */
	group: RightTabGroup;
}

/** A static tab: a fixed chrome symbol from the closed glyph set, drawn as a
 *  1-bit `<Glyph/>` (RIG-3603, compass-glyph-primitives). */
export interface GlyphTabItem extends ActivityBarItemBase {
	kind: "glyph";
	id: StaticRightTab;
	name: GlyphName;
}

/** A fleet agent tab: a person's initial, derived once from the handle via
 *  `avatarInitial`. Only fleet tabs carry `agentId`/`unreachable` — the pinned
 *  id whose `StateDot` badges the tab, and whether that pin resolves to a
 *  visible agent (RIG-1645). An unreachable pin's `agentId` resolves no agent,
 *  so it carries no live `StateDot`. */
export interface AvatarTabItem extends ActivityBarItemBase {
	kind: "avatar";
	id: `agent:${string}`;
	letter: string;
	group: "fleet";
	agentId: string;
	unreachable?: boolean;
}

/** An activity-bar item, split at the item per Matt's frozen ruling: a static
 *  glyph tab or a fleet avatar tab. */
export type ActivityBarItem = GlyphTabItem | AvatarTabItem;

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
	status: {
		kind: "glyph",
		id: "status",
		name: "status",
		title: "Fleet status",
		group: "fleet",
	},
	files: {
		kind: "glyph",
		id: "files",
		name: "files",
		title: "Files",
		group: "issue",
	},
	vcs: {
		kind: "glyph",
		id: "vcs",
		name: "vcs",
		title: "Version control",
		group: "issue",
	},
	pr: {
		kind: "glyph",
		id: "pr",
		name: "pr",
		title: "Pull request",
		group: "issue",
	},
};

/** The static issue-group items, in declaration order — the card-scoped tabs the
 *  activity bar renders below the divider (design dock-in-sidebar D2). */
export const RIGHT_SIDEBAR_ISSUE_ITEMS: readonly ActivityBarItem[] =
	Object.values(RIGHT_SIDEBAR_TAB_BY_ID).filter((t) => t.group === "issue");

/** Derive an agent's activity-bar avatar initial from its handle, per D1 of
 *  design compass-glyph-primitives. Handles are charset-unconstrained (proto
 *  `from_handle`, no schema validation), so both activity-bar constructors
 *  derive through here — and the ASCII clamp is what lets the Unifont pin
 *  retire. An uppercase that expands (`ß`→`SS`) keeps the first letter rather
 *  than `?`, since the initial exists to tell agents apart; the tab's title
 *  carries the full handle either way. */
export function avatarInitial(handle: string): string {
	const first = Array.from(handle.trim())[0];
	if (first === undefined) return "?";
	const folded = first.normalize("NFKD").replace(/\p{M}/gu, "").toUpperCase();
	const ascii = Array.from(folded)[0];
	return ascii !== undefined && /^[\x21-\x7e]$/.test(ascii) ? ascii : "?";
}

/** Build the fleet activity-bar item for a RESOLVABLE pinned agent (Record A
 *  §T2; RIG-1645 P1). The tab id is the `agent:`-prefixed account id; the
 *  letter is the agent handle's initial (a per-agent glyph, no hardcoded
 *  Supervisor ◆), and the title is the LIVE agent handle. The item is left
 *  unmarked (`unreachable` absent) so its `agentId` badges a real `StateDot`.
 *  An unresolvable pin is built by `unreachableFleetItem` instead — so a
 *  marked item can carry an `agentId` that resolves no agent. */
export function fleetItemForAgent(agent: Agent): AvatarTabItem {
	return {
		kind: "avatar",
		id: `agent:${agent.account.id}`,
		letter: avatarInitial(agent.account.handle),
		title: agent.account.handle,
		group: "fleet",
		agentId: agent.account.id,
	};
}

/** Build the fleet activity-bar item for an UNRESOLVABLE pin (RIG-1645 P1): its
 *  agent no longer resolves to a visible agent (dead / despawned / filtered
 *  out). The label is the handle cached at pin time (P0), so the item shows the
 *  human name the user pinned rather than an opaque id (a legacy `{ id, handle:
 *  id }` fallback pin degrades to the id). The item is marked `unreachable` so
 *  the activity bar and the pane render the unreachable state; its `agentId`
 *  intentionally resolves no agent (no live `StateDot`). */
export function unreachableFleetItem(pin: PinnedAgent): AvatarTabItem {
	return {
		kind: "avatar",
		id: `agent:${pin.id}`,
		letter: avatarInitial(pin.handle),
		title: pin.handle,
		group: "fleet",
		agentId: pin.id,
		unreachable: true,
	};
}
