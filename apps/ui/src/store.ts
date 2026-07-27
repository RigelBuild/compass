// The Compass ADE UI's central state store.
//
// One store owns all cross-component state: which view is shown, what's
// selected, which panes are open, and the left-sidebar folder collapse state.
// Components read it through the AppStore context (see context.ts) and never hold
// their own copies, so selection stays coherent across the shell — clicking an
// agent in the tree, a card on the board, or a row in a swimlane all resolve to
// the same selection.
//
// This is a dev mockup: the store reads the in-memory fixture (stub-data.ts).
// When the daemon grows the real streams, the accessors below stay and their
// bodies swap the fixture for the generated @compass/client — the AppStore
// contract is the seam the components are written against.

import { type Accessor, createMemo, createSignal } from "solid-js";
import { agentDmAccountId, threadsOf } from "./comms";
import type {
	Account,
	Channel,
	ChannelGroup,
	Membership,
	Message,
} from "./comms-stub";
import {
	STUB_ACCOUNTS,
	STUB_CHANNEL_GROUPS,
	STUB_CHANNELS,
	STUB_MESSAGES,
} from "./comms-stub";
import type { AgentSession } from "./session-events";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import {
	type Agent,
	STUB_AGENTS,
	STUB_WORKSTREAMS,
	type TrackerConfig,
	type Workstream,
} from "./stub-data";
import {
	createFixtureTrackerSeam,
	DEFAULT_TRACKER_CONFIG,
	type TrackerSeam,
} from "./tracker";

/** The caller — whose visibility scopes every listing and whose membership the
 *  rail reflects. The daemon derives this from the authenticated connection
 *  (comms.proto: "the caller is the account authenticated on the connection");
 *  the fixture pins it to the human owner. */
export const CALLER_ID = "acc-matt";

/** The top-level surface the shell routes between. `bridge`/`backlog`/`done`/
 *  `settings` are the board-family surfaces (the default is `bridge`), still
 *  primary, reachable from the top bar; they swap the whole UI. `channel` is
 *  the standalone channel conversation; `agent` is the per-agent workspace —
 *  the agent's channel plus its tab/split panes. */
export type View =
	| "channel"
	| "agent"
	| "bridge"
	| "backlog"
	| "done"
	| "settings";

/** Right-sidebar tabs (design dock-in-sidebar D1/T1/T2). Fleet tabs are
 *  always-on agent conversations (Supervisor, Warden) plus the Status pane
 *  (fleet metrics), grouped above the
 *  card-scoped workstream tabs (Files with a search box, VCS with commit
 *  history, PR with its checks). Split into named subsets so the grouped
 *  activity bar and the chrome-hiding rule (D5) key off types, not string
 *  lists. */
export type FleetTab = "supervisor" | "warden" | "status";
export type WorkstreamTab = "files" | "vcs" | "pr";
export type RightSidebarTab = FleetTab | WorkstreamTab;

/** A repo clone present in the selected agent's container (T6). Multi-repo
 *  capable now; the fixture derives a single clone per agent until the daemon
 *  reports more (resolved decision 3). */
export interface RepoClone {
	id: string;
	/** "owner/name", e.g. "sealedsecurity/sealed". */
	name: string;
	branches: string[];
	currentBranch: string;
}

/** What a single pane in the agent view shows: the chat conversation, a
 *  terminal, or a file. A pane is the actual UI leaf — the thing rendered on
 *  screen. */
export type PaneKind = "chat" | "terminal" | "file";

/** One pane in the agent view (design D6/T7): a leaf UI. `terminalId`/`filePath`
 *  are set for the matching kind; the chat pane carries neither. */
export interface Pane {
	id: string;
	kind: PaneKind;
	title: string;
	terminalId?: string;
	filePath?: string;
}

/** A binary split tree of panes within one tab (T7): a leaf shows one pane; a
 *  split places two children row (side by side) or column (stacked), nesting
 *  recursively. "Split right" adds a row split, "split down" a column split. */
export type SplitNode =
	| { kind: "leaf"; pane: Pane }
	| {
			kind: "split";
			direction: "row" | "column";
			left: SplitNode;
			right: SplitNode;
	  };

/** A tab in the agent view: a group of panes shown together on one screen.
 *  Tabs are the top-level switcher (clicking a tab shows its panes full-screen);
 *  a tab owns its own split tree and remembers which pane is focused (the pane
 *  the split buttons act on). The first tab is always the chat (design D6). */
export interface AgentTab {
	id: string;
	title: string;
	/** The split tree of panes in this tab. */
	layout: SplitNode;
	/** The focused pane id — where "split right"/"split down" insert. */
	focusedPaneId: string;
}

/** The always-present chat tab/pane id — the home-DM conversation, which every
 *  agent view opens with and can never close (design D6). The tab and its sole
 *  starting pane share this id. */
export const CHAT_TAB_ID = "chat";

/** The chat pane — the home-DM conversation leaf every agent view opens on. */
const chatPane = (): Pane => ({
	id: CHAT_TAB_ID,
	kind: "chat",
	title: "Chat",
});

/** The default tab set: one chat tab holding the chat pane full-screen. */
const chatTab = (): AgentTab => ({
	id: CHAT_TAB_ID,
	title: "Chat",
	layout: { kind: "leaf", pane: chatPane() },
	focusedPaneId: CHAT_TAB_ID,
});

/** Every pane id in a tab's split tree, left-to-right (the pane layout order). */
export function splitPaneIds(node: SplitNode): string[] {
	return node.kind === "leaf"
		? [node.pane.id]
		: [...splitPaneIds(node.left), ...splitPaneIds(node.right)];
}

/** Every pane in a tab's split tree, left-to-right. */
export function splitPanes(node: SplitNode): Pane[] {
	return node.kind === "leaf"
		? [node.pane]
		: [...splitPanes(node.left), ...splitPanes(node.right)];
}

/** Remove a pane from a tab's split tree, collapsing any split that loses a
 *  child to its surviving sibling. Returns null if the tree would be empty. */
function prunePane(node: SplitNode, paneId: string): SplitNode | null {
	if (node.kind === "leaf") return node.pane.id === paneId ? null : node;
	const left = prunePane(node.left, paneId);
	const right = prunePane(node.right, paneId);
	if (left && right) return { ...node, left, right };
	return left ?? right;
}

/** Split the FIRST leaf matching `targetPaneId`, placing `newPane` beside it in
 *  `direction` (`row` = split right, `column` = split down). Recurses left-first
 *  and stops at the first match, so splitting grows the tree by exactly one pane
 *  (no pane explosion when a pane id somehow repeats). Returns the rewritten
 *  tree and whether a leaf matched. */
export function splitPaneOnce(
	node: SplitNode,
	targetPaneId: string,
	newPane: Pane,
	direction: "row" | "column",
): [SplitNode, boolean] {
	if (node.kind === "leaf") {
		return node.pane.id === targetPaneId
			? [
					{
						kind: "split",
						direction,
						left: node,
						right: { kind: "leaf", pane: newPane },
					},
					true,
				]
			: [node, false];
	}
	const [left, insertedLeft] = splitPaneOnce(
		node.left,
		targetPaneId,
		newPane,
		direction,
	);
	if (insertedLeft) return [{ ...node, left }, true];
	const [right, insertedRight] = splitPaneOnce(
		node.right,
		targetPaneId,
		newPane,
		direction,
	);
	return [{ ...node, right }, insertedRight];
}

/**
 * The UI state contract every component reads. Accessors are reactive getters
 * (call them in JSX to subscribe); the remaining members are actions that mutate
 * state. Named here — the module that owns the store — so consumers import this
 * type rather than coupling to `ReturnType<typeof createAppStore>`.
 */
export interface AppStore {
	// ── View routing ──
	/** The active top-level view. */
	view: Accessor<View>;
	/** Jump to the Bridge board. */
	showBridge: () => void;
	/** Show the Backlog view (Todo + Backlog tiers, D3). */
	showBacklog: () => void;
	/** Show the Done/archive view (D4). */
	showDone: () => void;
	/** Show the Settings view (tracker mapping + handle, T11). */
	showSettings: () => void;

	// ── Selection ──
	/** The selected agent id, or null. Drives the agent view + roster highlight. */
	selectedAgentId: Accessor<string | null>;
	/** The selected workstream id, or null. Drives the detail + right sidebar. */
	selectedWorkstreamId: Accessor<string | null>;
	/** The resolved selected agent, or undefined. */
	selectedAgent: Accessor<Agent | undefined>;
	/** The composed roster view-model for an account id — account + optional
	 *  lifecycle by shared account id — or undefined when no agent owns the id.
	 *  The pure seam (`joinAgents` in the real era) the workspace WILL read once
	 *  the SubscribeComms/SubscribeEvents join lands; today every render surface
	 *  resolves the agent through `selectedAgent()`. */
	agentView: (id: string) => Agent | undefined;
	/** The resolved selected workstream, or undefined. */
	selectedWorkstream: Accessor<Workstream | undefined>;
	/** Select an agent and switch to its view; re-selecting is a no-op. */
	openAgent: (agentId: string) => void;
	/** Select a channel and route to its view — UNLESS it's a 1:1 agent DM, in
	 *  which case delegate to openAgent (the workspace is the DM's surface). */
	openChannel: (channelId: string) => void;
	/** Select a workstream (card / swimlane cell) and sync the roster to it. */
	selectWorkstream: (workstreamId: string) => void;

	// ── Panes ──
	/** Whether the left sidebar (folder tree) is shown. */
	leftOpen: Accessor<boolean>;
	toggleLeft: () => void;
	/** Whether the right sidebar (files / VCS / PR) is shown. */
	rightOpen: Accessor<boolean>;
	toggleRight: () => void;

	// ── Left-sidebar folders ──
	/** Whether a folder id is collapsed in the tree. */
	isFolderCollapsed: (folderId: string) => boolean;
	toggleFolder: (folderId: string) => void;

	// ── Right sidebar: activity-bar tabs + repos (T6; dock-in-sidebar D1) ──
	/** The active right-sidebar tab: a fleet conversation (Supervisor / Warden)
	 *  or a workstream tab (Files / VCS / PR). */
	activeRightTab: Accessor<RightSidebarTab>;
	setActiveRightTab: (tab: RightSidebarTab) => void;
	/** Repo clones present in the selected agent's container, for the repo/branch
	 *  dropdown. Empty when no agent is selected. */
	agentRepos: Accessor<RepoClone[]>;
	/** The active repo id within the selected agent's clones, or null. */
	activeRepoId: Accessor<string | null>;
	/** The resolved active repo, or undefined. */
	activeRepo: Accessor<RepoClone | undefined>;
	setActiveRepo: (repoId: string) => void;
	/** Switch the current branch by selecting the workstream that owns it, so the
	 *  dropdown and the Files/VCS/PR panes move together. No-op unless the branch
	 *  belongs to a workstream of the selected agent. */
	setActiveBranch: (branch: string) => void;

	// ── Comms: the channel surface (design compass-0.7) ──
	/** The calling account (the authenticated user; comms.proto caller model). */
	caller: Accessor<Account>;
	/** All accounts visible to the caller — the author/handle resolution source
	 *  for the channel surface (distinct from `agents`, the board's fleet). */
	accounts: Accessor<Account[]>;
	/** All channel groups visible to the caller (the rail's group headers). */
	channelGroups: Accessor<ChannelGroup[]>;
	/** All channels + DMs visible to the caller — the reactive rail source, so a
	 *  join/subscribe is visible everywhere at once. */
	channels: Accessor<Channel[]>;
	/** All messages visible to the caller — the reactive conversation source. */
	messages: Accessor<Message[]>;
	/** The selected channel id, or null (the empty state before a pick). */
	selectedChannelId: Accessor<string | null>;
	/** The resolved selected channel, or undefined. */
	selectedChannel: Accessor<Channel | undefined>;
	/** The agent workspace's chat channel — the selected agent's home DM,
	 *  derived off the account, independent of `selectedChannel` so the
	 *  standalone surface can't re-point the workspace pane. Undefined when no
	 *  agent is selected. */
	workspaceChannel: Accessor<Channel | undefined>;
	/** Join a channel the caller can see but hasn't joined (`none` → `joined`).
	 *  No-op if already joined/subscribed. */
	joinChannel: (channelId: string) => void;
	/** Toggle the caller's subscription on a joined channel (`joined` ⇆
	 *  `subscribed`). No-op on an unjoined channel or an always-subscribed one. */
	toggleSubscribe: (channelId: string) => void;
	/** Answer a question within an ask in a message: records the chosen
	 *  option(s) on the named question. Single-select is first-responder-wins —
	 *  once a question is answered a later single-select answer is a no-op;
	 *  multi-select toggles. No-op for an unknown message/ask/question/option.
	 *  Becomes a RespondToAsk call when the stream lands. */
	answerAsk: (
		messageId: string,
		askId: string,
		questionId: string,
		optionId: string,
	) => void;
	/** The root id of the currently open thread on the standalone channel
	 *  surface, or null when no thread is open. Channel-scoped: a selection
	 *  change (openChannel/openAgent) clears it. */
	openThreadRootId: Accessor<string | null>;
	/** Open the thread rooted at `rootMessageId`. Callers pass a ROOT id; the
	 *  store guards by resolving through `threadsOf(messages(), channelId)` and
	 *  no-ops on a reply id or an unknown id (record §321-324). */
	openThread: (rootMessageId: string) => void;
	/** Close the open thread, resetting `openThreadRootId` to null. */
	closeThread: () => void;
	/** Post a reply under `parentMessageId` in `channelId`: appends one in-memory
	 *  Message authored by the caller with a single text block and a minted local
	 *  id. Becomes a PostMessage call when the stream lands. */
	postReply: (channelId: string, parentMessageId: string, text: string) => void;

	// ── Agent view: tabs (pane groups) + per-tab split trees (T7) ──
	/** The selected agent's live session: the typed AgentSession (its ordered
	 *  SessionEvent stream + running flag), or undefined when no agent is selected
	 *  / it has no session. Compass folds and renders these typed events (design
	 *  compass-0.8); `running` drives the Stop control's enablement. */
	agentSession: Accessor<AgentSession | undefined>;
	/** The open tabs (chat first, never closable; design D6). Each tab is a
	 *  group of panes with its own split tree. Empty when no agent is selected.
	 *  Terminals are not auto-opened — a fresh agent shows only the chat. */
	agentTabs: Accessor<AgentTab[]>;
	/** The active tab id (the tab shown full-screen), or null when no agent is
	 *  selected. */
	activeAgentTabId: Accessor<string | null>;
	/** The resolved active tab, or undefined. */
	activeAgentTab: Accessor<AgentTab | undefined>;
	/** Switch which tab is shown. No-op for an unknown id. */
	setActiveAgentTab: (tabId: string) => void;
	/** Open a new full-screen tab and focus it. The MVP opens a terminal tab
	 *  (later: a context menu picks terminal / markdown / file, design D6). A tab
	 *  starts with its one pane full-screen. Re-opening a pane already shown as a
	 *  tab just focuses it (id-deduped). */
	openTab: (pane: Pane) => void;
	/** Mint a fresh placeholder terminal pane for an agent — a brand-new pane
	 *  with a globally-unique id (monotonic counter), used to keep "new tab" and
	 *  "split" always available once the agent's fixture terminals are all placed.
	 *  Its `terminalId` intentionally matches no fixture (the pane starts empty
	 *  until the daemon attaches a real terminal). */
	newTerminalPane: (agent: Agent) => Pane;
	/** Close a tab and drop it; the chat tab can't be closed. Focus falls back
	 *  to the chat tab. */
	closeTab: (tabId: string) => void;
	/** Split the active tab's focused pane, adding `pane` beside it — `row` =
	 *  split right, `column` = split down (design D6). The new pane becomes the
	 *  tab's focused pane. No-op when no agent/tab is active. */
	splitActivePane: (pane: Pane, direction: "row" | "column") => void;
	/** Focus a pane within the active tab (the pane the split buttons act on).
	 *  No-op for a pane not in the active tab. */
	setFocusedPane: (paneId: string) => void;
	/** Close a pane within the active tab, collapsing its split. Closing the last
	 *  pane in a non-chat tab closes the tab; the chat pane in the chat tab is
	 *  permanent. */
	closePane: (paneId: string) => void;
	/** Stop the selected agent (the workspace's stop control). Steering happens
	 *  in the channel, not here — this is the one non-observational control. A
	 *  no-op stub until the daemon's StopAgentSession lands. */
	stopAgent: () => void;

	// ── Log panel (D2) ──
	/** Whether the bottom log panel is open. Defaults open; resets open on
	 *  workspace entry (openAgent). */
	logOpen: Accessor<boolean>;
	toggleLog: () => void;

	// ── Left-sidebar sections (channels / agents) ──
	/** Whether a sidebar section is collapsed. The two sections collapse
	 *  independently (design compass-0.7 §410-411). */
	isSectionCollapsed: (section: "channels" | "agents") => boolean;
	toggleSection: (section: "channels" | "agents") => void;

	// ── Workstreams (reactive board data) ──
	/** All workstreams — the reactive source every board surface reads, so a
	 *  promote/archive is visible everywhere at once (design "read through the
	 *  store accessors"). */
	workstreams: Accessor<Workstream[]>;
	/** Promote a Backlog workstream to Todo (D1/D3) and mirror to the tracker
	 *  through the mapping. No-op if it isn't currently `backlog`. */
	promoteToTodo: (workstreamId: string) => void;
	/** Archive a Done workstream (D4): stamps `archivedAt`, dropping it from the
	 *  active surfaces. A marker, not a delete — the Done view still lists it.
	 *  No-op if it isn't currently `done`. */
	archiveWorkstream: (workstreamId: string) => void;

	// ── Backlog view (D3) ──
	/** The current user's tracker-assigned issues (their personal queue), read
	 *  through the TrackerSeam for the Backlog view. */
	assignedIssues: Accessor<Workstream[]>;

	// ── Tracker config (T11) ──
	/** The user's tracker wiring (kind + handle + Compass↔tracker mapping). */
	trackerConfig: Accessor<TrackerConfig>;
	setTrackerConfig: (cfg: TrackerConfig) => void;
}

/**
 * Build the app store over the in-memory fixture. Called once at the app root;
 * the instance is provided through context.
 */
export function createAppStore(): AppStore {
	const agents = STUB_AGENTS;
	// The workstream list is reactive so promote/archive (below) are visible on
	// every surface at once. Seeded from the fixture; the real @compass/client
	// stream replaces the seed later (the accessor stays the seam).
	const [workstreams, setWorkstreams] =
		createSignal<Workstream[]>(STUB_WORKSTREAMS);

	const [view, setView] = createSignal<View>("bridge");
	const [selectedAgentId, setSelectedAgentId] = createSignal<string | null>(
		null,
	);
	// Default to the first workstream so the seam survives swapping the fixture
	// for the real @compass/client (no hardcoded stub id).
	const [selectedWorkstreamId, setSelectedWorkstreamId] = createSignal<
		string | null
	>(STUB_WORKSTREAMS[0]?.id ?? null);

	// The tracker wiring (T11) + the seam it drives. assignedIssues (D3) is the
	// user's personal queue, loaded once from the seam; re-loads when the handle
	// changes so the Backlog view tracks a reconfigured tracker.
	const [trackerConfig, setTrackerConfigSignal] = createSignal<TrackerConfig>(
		DEFAULT_TRACKER_CONFIG,
	);
	let seam: TrackerSeam = createFixtureTrackerSeam(DEFAULT_TRACKER_CONFIG);
	const [assignedIssues, setAssignedIssues] = createSignal<Workstream[]>([]);
	const loadAssignedIssues = () => {
		seam
			.listAssignedIssues(trackerConfig().handle)
			.then(setAssignedIssues)
			.catch(() => setAssignedIssues([]));
	};
	loadAssignedIssues();

	const [leftOpen, setLeftOpen] = createSignal(true);
	const [rightOpen, setRightOpen] = createSignal(true);

	const [collapsed, setCollapsed] = createSignal<ReadonlySet<string>>(
		new Set(),
	);
	// Left-sidebar section collapse (channels / agents) — a separate namespaced
	// key space on the same set mechanism as folder collapse (compass-0.7
	// §410-411). Keyed `section:${name}` so no tree folder id can collide.
	const [sectionCollapsed, setSectionCollapsed] = createSignal<
		ReadonlySet<string>
	>(new Set());

	// The bottom log panel (D2): open by default; reset open on workspace entry.
	const [logOpen, setLogOpen] = createSignal(true);

	// ── Right sidebar (T6; dock-in-sidebar D1/D6): active tab + repo/branch ──
	// Boots onto the Supervisor conversation (D6) so the shell opens with the
	// Bridge board + a full-height Supervisor chat side by side.
	const [activeRightTab, setActiveRightTab] =
		createSignal<RightSidebarTab>("supervisor");
	// The active repo id (T6). The current branch is derived from the selected
	// workstream (see agentRepos), so there's no separate branch-pick signal to
	// drift from the panes.
	const [activeRepoId, setActiveRepoId] = createSignal<string | null>(null);

	// ── Comms: the channel surface (design compass-0.7) ──
	// Accounts/groups are static seeds; channels + messages are reactive so a
	// membership toggle or (later) a post shows on every surface at once. The
	// accessors stay the seam when the real SubscribeComms stream replaces them.
	const [accounts] = createSignal<Account[]>(STUB_ACCOUNTS);
	const [channelGroups] = createSignal<ChannelGroup[]>(STUB_CHANNEL_GROUPS);
	const [channels, setChannels] = createSignal<Channel[]>(STUB_CHANNELS);
	const [messages, setMessages] = createSignal<Message[]>(STUB_MESSAGES);
	// Open on the first subscribed channel so the shell boots into a live
	// conversation, not the empty state — no hardcoded id (survives the swap to
	// the real stream).
	const [selectedChannelId, setSelectedChannelId] = createSignal<string | null>(
		STUB_CHANNELS.find((c) => c.membership === "subscribed")?.id ??
			STUB_CHANNELS[0]?.id ??
			null,
	);
	// The root id of the open thread on the standalone channel surface, or null.
	// Channel-scoped: openChannel/openAgent clear it (T-T1).
	const [openThreadRootId, setOpenThreadRootId] = createSignal<string | null>(
		null,
	);
	// Monotonic counter for MINTED local reply ids (the PostMessage seam). Not
	// reactive — like mintedTerminalCount, it only sources fresh ids on demand.
	let localReplyCount = 0;

	// ── Agent view (T7): tabs (pane groups) + active tab ──
	// Each tab owns its own split tree of panes and its focused pane. A fresh
	// agent shows only the chat tab (terminals hidden by default, D6). The
	// list is empty until an agent is opened; `openAgent` seeds it.
	const [tabs, setTabs] = createSignal<AgentTab[]>([]);
	const [activeAgentTabId, setActiveAgentTabId] = createSignal<string | null>(
		null,
	);
	// The agent id the agent-view state (tabs/split/branch) was last initialized
	// for — distinct from `selectedAgentId`, which the board's `selectWorkstream`
	// moves without initializing the view. `openAgent` keys its reset on THIS, so
	// a roster move followed by opening that agent still initializes the view.
	const [agentViewAgentId, setAgentViewAgentId] = createSignal<string | null>(
		null,
	);
	// Monotonic counter for MINTED placeholder terminal panes (never reused, so
	// ids stay globally unique across opens/closes — openTab's id-dedupe and
	// splitPaneOnce's repeat-guard never collide). Not reactive: it only sources
	// fresh ids on demand, so a plain counter, not a signal. The daemon will
	// assign real terminal ids at this same seam later.
	let mintedTerminalCount = 0;

	const selectedAgent = createMemo(() =>
		agents.find((a) => a.account.id === selectedAgentId()),
	);
	// The pure seam that composes the durable `account` with the optional
	// ephemeral `lifecycle` by shared account id (`joinAgents` in the real era) —
	// lifecycle is already carried on the view-model, so this is a lookup.
	const agentView = (id: string): Agent | undefined =>
		agents.find((a) => a.account.id === id);
	const selectedWorkstream = createMemo(() =>
		workstreams().find((w) => w.id === selectedWorkstreamId()),
	);
	// The selected agent's repo clones (T6). The fixture models one clone per
	// agent — the monorepo — with the branches drawn from that agent's assigned
	// workstreams (design "single clone until the daemon reports more"). The
	// accessor returns an array so a multi-clone daemon is a fixture change, not
	// a shape change. `currentBranch` is derived from the selected workstream
	// (each workstream owns one branch) — so the dropdown, the detail panes, and
	// the board selection are one source of truth and can't drift apart.
	const agentRepos = createMemo<RepoClone[]>(() => {
		const id = selectedAgentId();
		if (!id) return [];
		const owned = workstreams().filter((w) => w.assignee === id);
		if (owned.length === 0) return [];
		const branches = owned.map((w) => w.branch);
		// The current branch is the selected workstream's branch when it belongs
		// to this agent, else the primary (first) — never a stale independent pick.
		const selected = owned.find((w) => w.id === selectedWorkstreamId());
		return [
			{
				id: `${id}-repo`,
				name: "sealedsecurity/sealed",
				branches,
				currentBranch: selected?.branch ?? branches[0],
			},
		];
	});
	// The active repo: the explicit pick if it's still among the agent's clones,
	// else the first clone (so a stale pick from a previous agent can't dangle).
	const activeRepo = createMemo<RepoClone | undefined>(() => {
		const repos = agentRepos();
		const picked = repos.find((r) => r.id === activeRepoId());
		return picked ?? repos[0];
	});

	// ── Comms memos ──
	const caller = createMemo<Account>(
		() =>
			accounts().find((a) => a.id === CALLER_ID) ?? {
				id: CALLER_ID,
				handle: CALLER_ID,
				displayName: CALLER_ID,
				kind: "user",
			},
	);
	const selectedChannel = createMemo(() =>
		channels().find((c) => c.id === selectedChannelId()),
	);
	// The agent workspace's chat channel: the selected agent's home DM, resolved
	// O(1) off the account — NOT `selectedChannel`. Deriving it from the agent
	// (not the shared selection signal) is what keeps the standalone channel
	// surface and the workspace chat pane independent: a standalone channel the
	// user opens moves `selectedChannel`, never this, so it can't bleed into the
	// interactive workspace pane (D3). Undefined when no agent is selected or its
	// home DM isn't in the channel set (the real-daemon partial-join case).
	const workspaceChannel = createMemo(() => {
		const home = selectedAgent()?.account.homeChannelId;
		return home ? channels().find((c) => c.id === home) : undefined;
	});
	// The selected agent's live session trace (the workspace's trace source), or
	// undefined when no agent is selected or it has no trace. One id space after
	// T1 makes the observed agent ≡ the selected agent (record §478-481).
	const agentSession = createMemo<AgentSession | undefined>(() => {
		const id = selectedAgentId();
		return id ? STUB_SESSION_EVENTS[id] : undefined;
	});

	// The agent view's tabs (T7): the chat tab first (always present), then the
	// tabs the user has opened. Empty when no agent is selected; terminals are
	// NOT auto-opened (design D6 "terminals hidden by default").
	const agentTabs = createMemo<AgentTab[]>(() =>
		selectedAgentId() ? tabs() : [],
	);
	const activeAgentTab = createMemo<AgentTab | undefined>(() =>
		agentTabs().find((t) => t.id === activeAgentTabId()),
	);

	// Opening an agent switches to its per-agent workspace and, for an agent whose
	// workspace isn't yet initialized, syncs the workstream selection to the
	// agent's primary (first-assigned) workstream and resets the workspace state:
	// tabs reset to the lone chat tab, the chat pane centers on the agent's home
	// DM channel, the repo pick resets to the agent's clone, and the log panel
	// re-opens. The guard keys on `agentViewAgentId` — the id the workspace was
	// initialized for — NOT `selectedAgentId`, which `selectWorkstream` moves from
	// the board without initializing the workspace; keying on it would let a
	// roster move suppress the reset. Re-opening the already-initialized agent
	// only re-asserts the selection (the contract: re-selecting is a no-op) and
	// preserves the tabs the user has since opened.
	const openAgent = (agentId: string) => {
		// Entering an agent workspace drops the channel-scoped open-thread state.
		setOpenThreadRootId(null);
		setView("agent");
		// Anchor the workstream selection to this agent: keep the currently
		// selected workstream when this agent owns it (a card double-click selects
		// the card's workstream just before opening — often a non-primary one),
		// else fall back to the agent's primary (first-owned). This holds on BOTH
		// paths so a roster move that pointed the selection at another agent's
		// workstream can't leak into this view.
		const owned = workstreams().filter((w) => w.assignee === agentId);
		const anchored =
			owned.find((w) => w.id === selectedWorkstreamId())?.id ??
			owned[0]?.id ??
			null;
		if (agentId === agentViewAgentId()) {
			setSelectedAgentId(agentId);
			setSelectedWorkstreamId(anchored);
			return;
		}
		setSelectedAgentId(agentId);
		setSelectedWorkstreamId(anchored);
		setActiveRepoId(`${agentId}-repo`);
		// Reset the tab group to the lone permanent chat tab. The chat pane's
		// channel is derived from the selected agent (`workspaceChannel`), not
		// written here — so `selectedChannelId` stays the standalone surface's
		// own state and can never re-point the workspace pane.
		setTabs([chatTab()]);
		setActiveAgentTabId(CHAT_TAB_ID);
		setLogOpen(true);
		setAgentViewAgentId(agentId);
	};

	// Open a channel: route to the channel view with it selected — unless it's a
	// 1:1 agent DM, in which case its surface is the agent workspace, so delegate
	// to openAgent (one entry point, no dead-end DM view). Unknown id is a no-op.
	const openChannel = (channelId: string) => {
		// Switching the standalone channel surface drops the open-thread state.
		setOpenThreadRootId(null);
		const chan = channels().find((c) => c.id === channelId);
		if (!chan) return;
		const byId = new Map(accounts().map((a) => [a.id, a]));
		const agentId = agentDmAccountId(chan, CALLER_ID, byId);
		if (agentId) {
			openAgent(agentId);
			return;
		}
		setSelectedChannelId(channelId);
		setView("channel");
	};

	// Selecting a workstream (a board card or a swimlane cell) syncs the roster
	// to its assignee but stays on the board — it does not jump into the agent
	// view, so the board stays the working surface while you scan cards.
	const selectWorkstream = (workstreamId: string) => {
		setSelectedWorkstreamId(workstreamId);
		const ws = workstreams().find((w) => w.id === workstreamId);
		setSelectedAgentId(ws?.assignee ?? null);
	};

	// ── Comms mutations (design compass-0.7) ──
	// A membership transition on one channel, applied immutably so the accessor
	// emits a fresh list. `next` maps the current membership to its new value.
	const setMembership = (
		channelId: string,
		next: (current: Membership) => Membership,
	) => {
		setChannels((cs) =>
			cs.map((c) =>
				c.id === channelId ? { ...c, membership: next(c.membership) } : c,
			),
		);
	};
	const joinChannel = (channelId: string) => {
		setMembership(channelId, (m) => (m === "none" ? "joined" : m));
	};
	const toggleSubscribe = (channelId: string) => {
		// always-subscribed-to-own is implicit + non-togglable (design.md:416):
		// refuse to toggle it at the store seam, so the invariant holds even if a
		// caller bypasses the UI's fixed control.
		const channel = channels().find((c) => c.id === channelId);
		if (channel?.alwaysSubscribed) return;
		setMembership(channelId, (m) =>
			m === "subscribed" ? "joined" : m === "joined" ? "subscribed" : m,
		);
	};
	const answerAsk = (
		messageId: string,
		askId: string,
		questionId: string,
		optionId: string,
	) => {
		setMessages((ms) =>
			ms.map((msg) => {
				if (msg.id !== messageId) return msg;
				return {
					...msg,
					blocks: msg.blocks.map((b) => {
						if (b.kind !== "ask" || b.ask.askId !== askId) return b;
						const ask = b.ask;
						return {
							kind: "ask",
							ask: {
								...ask,
								questions: ask.questions.map((q) => {
									if (q.questionId !== questionId) return q;
									if (!q.options.some((o) => o.id === optionId)) return q;
									// First-responder-wins: a single-select question settles on its
									// first answer; a later answer is a no-op. Multi-select stays a toggle.
									if (!q.allowMultiple && q.chosenOptionIds.length > 0)
										return q;
									const chosen = q.allowMultiple
										? q.chosenOptionIds.includes(optionId)
											? q.chosenOptionIds.filter((id) => id !== optionId)
											: [...q.chosenOptionIds, optionId]
										: [optionId];
									return { ...q, chosenOptionIds: chosen };
								}),
							},
						};
					}),
				};
			}),
		);
	};
	// ── Thread actions (T-T1) ──
	// Guarded open: callers pass a ROOT id; resolve the message's channel and
	// verify it is a thread root before setting. A reply id or an unknown id
	// no-ops (record §321-324).
	const openThread = (rootMessageId: string) => {
		const msg = messages().find((m) => m.id === rootMessageId);
		if (!msg) return;
		const isRoot = threadsOf(messages(), msg.channelId).some(
			(t) => t.root.id === rootMessageId,
		);
		if (isRoot) setOpenThreadRootId(rootMessageId);
	};
	const closeThread = () => setOpenThreadRootId(null);
	// The PostMessage seam: append one in-memory reply authored by the caller,
	// mirroring answerAsk's immutable setMessages and the minted-id counter. The
	// daemon will issue a real PostMessage at this seam later.
	const postReply = (
		channelId: string,
		parentMessageId: string,
		text: string,
	) => {
		const id = `msg-local-${++localReplyCount}`;
		const reply: Message = {
			id,
			channelId,
			authorAccountId: CALLER_ID,
			atUnixMs: Date.now(),
			parentMessageId,
			blocks: [{ kind: "text", text }],
		};
		setMessages((ms) => [...ms, reply]);
	};

	// ── Agent view actions (T7) ──
	const setActiveAgentTab = (tabId: string) => {
		if (agentTabs().some((t) => t.id === tabId)) setActiveAgentTabId(tabId);
	};
	// Mutate the active tab in place (its split tree / focused pane), leaving the
	// other tabs untouched. A no-op when no tab is active.
	const updateActiveTab = (fn: (tab: AgentTab) => AgentTab) => {
		const id = activeAgentTabId();
		if (!id) return;
		setTabs((prev) => prev.map((t) => (t.id === id ? fn(t) : t)));
	};
	// Open a new full-screen tab holding `pane` and focus it. Re-opening a pane
	// already shown as a tab just focuses that tab (no duplicate). The MVP opens
	// a terminal; later a context menu picks the pane kind (design D6).
	const openTab = (pane: Pane) => {
		if (!selectedAgentId()) return;
		const existing = tabs().find((t) => t.id === pane.id);
		if (!existing) {
			const tab: AgentTab = {
				id: pane.id,
				title: pane.title,
				layout: { kind: "leaf", pane },
				focusedPaneId: pane.id,
			};
			setTabs((prev) => [...prev, tab]);
		}
		setActiveAgentTabId(pane.id);
	};
	// Mint a fresh placeholder terminal pane. The counter only ever increments,
	// so the id (`term-<agentId>-<n>`) is unique across the session even as panes
	// open and close — no collision with openTab's id-dedupe or splitPaneOnce's
	// repeat-guard. `terminalId` mirrors the id and matches no fixture on purpose:
	// the pane renders an empty "starting" state until the daemon attaches one.
	const newTerminalPane = (agent: Agent): Pane => {
		const n = ++mintedTerminalCount;
		const id = `term-${agent.account.id}-${n}`;
		return { id, kind: "terminal", title: `Terminal ${n}`, terminalId: id };
	};
	// Close a tab: drop it and fall focus back to the chat tab. The chat tab is
	// permanent (D6) — closing it is a no-op.
	const closeTab = (tabId: string) => {
		if (tabId === CHAT_TAB_ID) return;
		setTabs((prev) => prev.filter((t) => t.id !== tabId));
		if (activeAgentTabId() === tabId) setActiveAgentTabId(CHAT_TAB_ID);
	};
	// Split the active tab's focused pane, placing `pane` beside it (`row` =
	// split right, `column` = split down). The new pane becomes focused so a
	// follow-up split chains off it.
	const splitActivePane = (pane: Pane, direction: "row" | "column") => {
		updateActiveTab((tab) => {
			const [layout, inserted] = splitPaneOnce(
				tab.layout,
				tab.focusedPaneId,
				pane,
				direction,
			);
			return inserted ? { ...tab, layout, focusedPaneId: pane.id } : tab;
		});
	};
	// Focus a pane within the active tab (where the split buttons act). No-op
	// unless the pane is in that tab.
	const setFocusedPane = (paneId: string) => {
		updateActiveTab((tab) =>
			splitPaneIds(tab.layout).includes(paneId)
				? { ...tab, focusedPaneId: paneId }
				: tab,
		);
	};
	// Close a pane within the active tab, collapsing its split. Closing the last
	// pane of a non-chat tab closes the whole tab; the chat pane is permanent
	// (D6) — closing it is a no-op even when the chat tab is split.
	const closePane = (paneId: string) => {
		const tab = activeAgentTab();
		if (!tab) return;
		// The chat pane is permanent: it can't be pruned from the chat tab,
		// whether that tab is a lone leaf or a split. (The UI hides its close
		// button; this guards the public action too.)
		if (tab.id === CHAT_TAB_ID && paneId === CHAT_TAB_ID) return;
		const pruned = prunePane(tab.layout, paneId);
		if (!pruned) {
			// The tab has no panes left — close it (or no-op for the chat tab).
			closeTab(tab.id);
			return;
		}
		const focusedGone = !splitPaneIds(pruned).includes(tab.focusedPaneId);
		setTabs((prev) =>
			prev.map((t) =>
				t.id === tab.id
					? {
							...t,
							layout: pruned,
							focusedPaneId: focusedGone
								? splitPaneIds(pruned)[0]
								: t.focusedPaneId,
						}
					: t,
			),
		);
	};
	// The observation pane's stop control. Steering happens in the channel; this
	// is the one non-observational control. A no-op stub until StopAgentSession
	// lands (the daemon owns the actual stop).
	const stopAgent = () => {};
	const showBridge = () => setView("bridge");
	const showBacklog = () => setView("backlog");
	const showDone = () => setView("done");
	const showSettings = () => setView("settings");

	// Promote a Backlog workstream to Todo (D1/D3): the human moves it into the
	// global unassigned pool the Dispatcher assigns from, and the change mirrors
	// to the tracker. A no-op unless it's currently `backlog`, so the action is
	// idempotent against a double-click.
	const promoteToTodo = (workstreamId: string) => {
		const ws = workstreams().find((w) => w.id === workstreamId);
		if (ws?.state !== "backlog") return;
		setWorkstreams((prev) =>
			prev.map((w) =>
				w.id === workstreamId ? { ...w, state: "todo" as const } : w,
			),
		);
		// Mirror to the tracker only when the transition actually happened, so a
		// rejected promote never writes Todo to the tracker (the local guard and
		// the seam write stay in lockstep). The seam addresses the tracker's
		// native issue id (`issue`), not the Compass workstream id.
		void seam.updateIssueStatus(ws.issue, "todo");
	};

	// Archive a Done workstream (D4): stamp `archivedAt` so it drops off the
	// active surfaces but the Done view still lists it. A no-op unless it's
	// currently `done`, and idempotent (an already-archived one keeps its stamp).
	const archiveWorkstream = (workstreamId: string) => {
		setWorkstreams((prev) =>
			prev.map((w) =>
				w.id === workstreamId && w.state === "done" && !w.archivedAt
					? { ...w, archivedAt: new Date().toISOString() }
					: w,
			),
		);
	};

	const setTrackerConfig = (cfg: TrackerConfig) => {
		setTrackerConfigSignal(cfg);
		seam = createFixtureTrackerSeam(cfg);
		loadAssignedIssues();
	};

	const toggleLeft = () => setLeftOpen((v) => !v);
	const toggleRight = () => setRightOpen((v) => !v);

	const isFolderCollapsed = (folderId: string) => collapsed().has(folderId);
	const toggleFolder = (folderId: string) =>
		setCollapsed((prev) => {
			const next = new Set(prev);
			next.has(folderId) ? next.delete(folderId) : next.add(folderId);
			return next;
		});

	// The bottom log panel (D2).
	const toggleLog = () => setLogOpen((v) => !v);

	// Left-sidebar section collapse (channels / agents) — namespaced keys on the
	// same set mechanism, so the two sections toggle independently and can't
	// collide with tree folder ids (compass-0.7 §410-411).
	const isSectionCollapsed = (section: "channels" | "agents") =>
		sectionCollapsed().has(`section:${section}`);
	const toggleSection = (section: "channels" | "agents") =>
		setSectionCollapsed((prev) => {
			const key = `section:${section}`;
			const next = new Set(prev);
			next.has(key) ? next.delete(key) : next.add(key);
			return next;
		});

	// ── Right sidebar actions (T6) ──
	const setActiveRepo = (repoId: string) => {
		if (agentRepos().some((r) => r.id === repoId)) setActiveRepoId(repoId);
	};
	// Switch the current branch within the active repo by selecting the workstream
	// that owns it — so the dropdown, the detail panes (Files/VCS/PR), and the
	// board selection all move together (each branch is one workstream's branch).
	// A no-op unless the branch belongs to a workstream of the selected agent.
	const setActiveBranch = (branch: string) => {
		const id = selectedAgentId();
		if (!id) return;
		const ws = workstreams().find(
			(w) => w.assignee === id && w.branch === branch,
		);
		if (ws) setSelectedWorkstreamId(ws.id);
	};

	return {
		view,
		showBridge,
		showBacklog,
		showDone,
		showSettings,
		selectedAgentId,
		selectedWorkstreamId,
		selectedAgent,
		agentView,
		selectedWorkstream,
		openAgent,
		openChannel,
		selectWorkstream,
		leftOpen,
		toggleLeft,
		rightOpen,
		toggleRight,
		isFolderCollapsed,
		toggleFolder,
		activeRightTab,
		setActiveRightTab,
		agentRepos,
		activeRepoId,
		activeRepo,
		setActiveRepo,
		setActiveBranch,
		caller,
		accounts,
		channelGroups,
		channels,
		messages,
		selectedChannelId,
		selectedChannel,
		workspaceChannel,
		joinChannel,
		toggleSubscribe,
		answerAsk,
		openThreadRootId,
		openThread,
		closeThread,
		postReply,
		agentSession,
		agentTabs,
		activeAgentTabId,
		activeAgentTab,
		setActiveAgentTab,
		openTab,
		newTerminalPane,
		closeTab,
		splitActivePane,
		setFocusedPane,
		closePane,
		stopAgent,
		logOpen,
		toggleLog,
		isSectionCollapsed,
		toggleSection,
		workstreams,
		promoteToTodo,
		archiveWorkstream,
		assignedIssues,
		trackerConfig,
		setTrackerConfig,
	};
}
