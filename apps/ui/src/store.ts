// The Compass ADE UI's central state store.
//
// One store owns all cross-component state: which view is shown, what's
// selected, which panes are open, and the left-sidebar folder collapse state.
// Components read it through the AppStore context (see context.ts) and never hold
// their own copies, so selection stays coherent across the shell — clicking an
// agent in the tree, a card on the board, or a row in a swimlane all resolve to
// the same selection.
//
// The comms surface reads LIVE: `createAppStore` takes an optional CommsClient
// and runs `runCommsStream` over it, mirroring each reduced CommsState into the
// accessors below. The accessors are the seam the components were written
// against, so nothing above the store changed when the fixture went away. A
// store built WITHOUT a client is offline: it starts from `initialComms` (the
// fixture, in tests) and every write rejects — construction never needs a
// network client.

import type { CommsClient, CompassClient } from "@compass/client";
import {
	type Accessor,
	createMemo,
	createSignal,
	getOwner,
	onCleanup,
} from "solid-js";
import { agentDmAccountId, threadsOf } from "./comms";
import type {
	Account,
	Ask,
	Channel,
	ChannelGroup,
	Message,
} from "./comms-stub";
import { adaptMessage } from "./live/adapt";
import { probeServer } from "./live/client";
import { type CommsState, EMPTY_COMMS_STATE } from "./live/comms-state";
import { runCommsStream } from "./live/stream";
import type { AgentSession } from "./session-events";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import {
	type Agent,
	type DaemonInfo,
	STUB_AGENTS,
	STUB_DAEMON,
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

	// ── Daemon: server liveness/version banner ──
	/** The daemon liveness/version the top-bar banner shows (compass.v1
	 *  GetServerInfo). A store built with `options.compass` probes once at boot
	 *  and flips this to the live info; an offline store keeps STUB_DAEMON
	 *  (live:false). */
	daemon: Accessor<DaemonInfo>;

	// ── Comms: the channel surface (design compass-0.7) ──
	/** The calling account (the authenticated user; comms.proto caller model). */
	caller: Accessor<Account>;
	/** All accounts visible to the caller — the author/handle resolution source
	 *  for the channel surface (distinct from `agents`, the board's fleet). */
	accounts: Accessor<readonly Account[]>;
	/** All channel groups visible to the caller (the rail's group headers). */
	channelGroups: Accessor<readonly ChannelGroup[]>;
	/** All channels + DMs visible to the caller — the reactive rail source, so a
	 *  join/subscribe is visible everywhere at once. */
	channels: Accessor<readonly Channel[]>;
	/** All messages visible to the caller — the reactive conversation source. */
	messages: Accessor<readonly Message[]>;
	/** The selected channel id, or null (the empty state before a pick). */
	selectedChannelId: Accessor<string | null>;
	/** The resolved selected channel, or undefined. */
	selectedChannel: Accessor<Channel | undefined>;
	/** The agent workspace's chat channel — the selected agent's home DM,
	 *  derived off the account, independent of `selectedChannel` so the
	 *  standalone surface can't re-point the workspace pane. Undefined when no
	 *  agent is selected. */
	workspaceChannel: Accessor<Channel | undefined>;
	/** NOT WIRED YET — inert. The wire has no join RPC; the rail's join control
	 *  renders disabled. Kept as the seam the control binds to (and where the
	 *  RPC lands), but it fakes NO membership: a local-only join silently
	 *  reverted on the next SubscribeComms snapshot, which re-derives membership
	 *  from the server. */
	joinChannel: (channelId: string) => void;
	/** NOT WIRED YET — inert, for the same reason as `joinChannel`. The rail's
	 *  subscribe toggle renders disabled. */
	toggleSubscribe: (channelId: string) => void;
	/** Record an answer to a question within an ask, LOCALLY. The wire
	 *  `RespondToAsk` is gated on COMPLETENESS: the server accepts exactly one
	 *  respond per ask (go/internal/store/messages.go:400-403/:437), so answers
	 *  accumulate locally and exactly ONE atomic respond — every question's
	 *  answer in one call — is issued on the click that completes the ask. A
	 *  single-question ask completes on its only click. Single-select is
	 *  first-responder-wins (a later answer is a local no-op); multi-select
	 *  toggles. No-op for an unknown message/ask/question/option, and for an ask
	 *  already submitted. A REFUSED respond rolls the local answer back and
	 *  clears the submitted mark (the ask stays retryable); the error also
	 *  reaches `onCommsError`. */
	answerAsk: (
		messageId: string,
		askId: string,
		questionId: string,
		optionId: string,
	) => void;
	/** Submit an INCOMPLETE ask — the skip affordance. Issues the ask's one
	 *  `RespondToAsk` with the answers recorded so far and an empty
	 *  `chosenOptionIds` for every skipped question (the wire requires coverage
	 *  of each question, not an answer to each). No-op on an unknown ask, an ask
	 *  already submitted, and a wholly unanswered one. */
	submitAsk: (messageId: string, askId: string) => void;
	/** Whether this ask's one `RespondToAsk` has been issued — reactive, so the
	 *  render locks a submitted ask. Cleared again if the respond is refused. */
	isAskSubmitted: (askId: string) => boolean;
	/** The message from the last REFUSED `RespondToAsk` for this ask, or
	 *  undefined when its last respond was not refused — reactive, so the ask
	 *  block can say what went wrong instead of leaving the user's click to
	 *  vanish into a console line. Cleared when the user answers the ask again. */
	askError: (askId: string) => string | undefined;
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
	/** Post a root message to `channelId` through the wire `PostMessage`, with a
	 *  single text block and a fresh `clientRequestId` (the server dedups a
	 *  retry). Does NOT insert locally: the stored message arrives through the
	 *  SubscribeComms echo, which `upsertMessage` dedups by id — so the sent
	 *  message renders exactly once. Rejects when the post fails (or when the
	 *  store has no client) so the composer can keep the user's text. */
	postMessage: (channelId: string, text: string) => Promise<void>;
	/** Post a reply under `parentMessageId` in `channelId` — `postMessage` with
	 *  the wire's `parentMessageId` set. Same no-local-insert contract: the
	 *  stream echo renders it. */
	postReply: (
		channelId: string,
		parentMessageId: string,
		text: string,
	) => Promise<void>;

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
	 *  in the channel, not here — this is the one non-observational control.
	 *  Issues StopAgentSession for the OBSERVED session (`agentSession()`), a
	 *  no-op when nothing is selected. Resolves either way: the RPC is
	 *  Runner-backed and answers `Unavailable` when the server has no RunnerHub
	 *  attached (the socket-only path), so a refusal is routed to
	 *  `onCommsError` rather than rejected — there is no user text to preserve,
	 *  unlike a failed post. */
	stopAgent: () => Promise<void>;
	/** The message from the last REFUSED stop — a server refusal
	 *  (`Unavailable`), a fixture-sourced session, or a store with no compass
	 *  client — or undefined when the last attempt was not refused. Reactive, so
	 *  the log panel can SAY what went wrong instead of leaving the click to
	 *  vanish into a console line. Cleared at the start of the next attempt. */
	stopError: Accessor<string | undefined>;

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

/** What `createAppStore` is handed at boot. Everything is optional so a unit
 *  test constructs the store with NO network client at all: the comms surface
 *  then holds `initialComms` (the fixture, in tests) and every write rejects.
 *  index.tsx supplies the real `Connection`-derived client + caller. */
export interface AppStoreOptions {
	/** The live comms client. Present → the store runs `runCommsStream` over it
	 *  for its lifetime and every comms write is a real RPC. Absent → offline. */
	readonly comms?: CommsClient;
	/** The caller's account id, from the Connection (`VITE_COMPASS_CALLER_ID`).
	 *
	 *  SEAM (caller-identity): there is no `WhoAmI` RPC, so the operator supplies
	 *  the account the bearer authenticates as (live/connection.ts:28-35). The
	 *  fixture default keeps the offline store on the fixture's owner. */
	readonly callerId?: string;
	/** The comms state the store starts from before any stream push. Defaults to
	 *  EMPTY — tests that need populated comms pass the fixture explicitly. */
	readonly initialComms?: CommsState;
	/** The live compass client — the agent-lifecycle surface (StopAgentSession).
	 *  Absent → offline: `stopAgent` reports through `onCommsError` instead of
	 *  dialing. Separate from `comms`: the two services are separate clients over
	 *  the one Connection (live/client.ts:28-33). */
	readonly compass?: CompassClient;
	/** The observable agent sessions, keyed by agent account id. Defaults to the
	 *  hand-written fixture (STUB_SESSION_EVENTS), which is what the shipped app
	 *  still shows until SubscribeAgentSession is wired — every fixture entry is
	 *  marked `fixture: true`, so `stopAgent` refuses to put its id on the wire.
	 *  A test (or, later, the live stream) supplies server-sourced sessions here
	 *  to exercise the real Stop path. */
	readonly sessions?: Record<string, AgentSession>;
	/** Observes a comms failure — a stream error the driver retries past, a
	 *  rejected `RespondToAsk`, a refused `StopAgentSession`, or a failed boot
	 *  `GetServerInfo` probe (the banner stays on the stub, offline).
	 *  `postMessage`/`postReply` reject to their caller instead (the composer
	 *  must keep the user's text) and do NOT route here. */
	readonly onCommsError?: (error: unknown) => void;
}

/**
 * Build the app store. Called once at the app root; the instance is provided
 * through context. With `options.comms` set, the comms accessors are fed by the
 * live SubscribeComms stream, which runs until the store's reactive owner is
 * disposed (index.tsx's root lives for the app's lifetime).
 */
export function createAppStore(options: AppStoreOptions = {}): AppStore {
	const callerId = options.callerId ?? CALLER_ID;
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
	// ONE reduced CommsState drives all four comms accessors. It starts at
	// `initialComms` (EMPTY by default — the store no longer boots from the
	// fixture) and is replaced wholesale by each `runCommsStream` push; the local
	// membership mutations below rewrite it the same immutable way. The four
	// accessors are memos over it, so a message event leaves the channels array
	// reference untouched and the rail doesn't re-render.
	const [comms, setComms] = createSignal<CommsState>(
		options.initialComms ?? EMPTY_COMMS_STATE,
	);
	// CommsState's collections are `readonly` (a pure value the reducer rebuilds
	// on every transition) and the accessors keep that: nothing here or in the
	// components mutates in place — every write goes through setComms with a
	// fresh array — and a `readonly` signature is what keeps the compiler able
	// to hold that true, rather than a cast that silently permits the first
	// in-place mutation someone adds.
	const accounts = createMemo(() => comms().accounts);
	const channelGroups = createMemo(() => comms().channelGroups);
	const channels = createMemo(() => comms().channels);
	const messages = createMemo(() => comms().messages);
	// Open on the first subscribed channel so the shell boots into a live
	// conversation, not the empty state — no hardcoded id, and null before the
	// first snapshot arrives (the components render their empty state).
	const firstChannelId = (state: CommsState): string | null =>
		state.channels.find((c) => c.membership === "subscribed")?.id ??
		state.channels[0]?.id ??
		null;
	const [selectedChannelId, setSelectedChannelId] = createSignal<string | null>(
		firstChannelId(comms()),
	);
	// Adopt a state pushed by the stream and settle the selection onto it: the
	// user's explicit pick wins as long as the channel is still visible, so a
	// later snapshot/event can never yank the surface out from under them; an
	// absent or vanished selection falls back to the first subscribed channel.
	const adoptComms = (next: CommsState) => {
		setComms(next);
		const current = selectedChannelId();
		if (current && next.channels.some((c) => c.id === current)) return;
		setSelectedChannelId(firstChannelId(next));
	};
	// The root id of the open thread on the standalone channel surface, or null.
	// Channel-scoped: openChannel/openAgent clear it (T-T1).
	const [openThreadRootId, setOpenThreadRootId] = createSignal<string | null>(
		null,
	);
	// The live read path: run the SubscribeComms driver for the store's lifetime,
	// mirroring each reduced state into the signals above. Aborted on teardown
	// (index.tsx's root is never disposed, so in the app this runs forever; a
	// test root's dispose stops it). `runCommsStream` resolves only on abort and
	// retries internally, so nothing here awaits it — a rejection would be a
	// driver bug, and it is surfaced rather than swallowed.
	if (options.comms) {
		const client = options.comms;
		const abort = new AbortController();
		if (getOwner()) onCleanup(() => abort.abort());
		void runCommsStream({
			client,
			callerId,
			mapMessage: adaptMessage,
			onState: adoptComms,
			signal: abort.signal,
			onError: (error) => options.onCommsError?.(error),
		}).catch((error) => {
			if (!abort.signal.aborted) options.onCommsError?.(error);
		});
	}

	// The daemon banner reads LIVE: a one-shot GetServerInfo probe at boot flips
	// the banner to the server's liveness/version. An offline store (no
	// `options.compass`) keeps STUB_DAEMON — the stub banner shows exactly as
	// before the wire. api_version-mismatch policy is deliberately NOT handled
	// here (a parked design question): the banner surfaces what the probe
	// returns, nothing more. One-shot async (not a stream), so no
	// AbortController — a `disposed` flag guards a late-resolving probe from
	// writing into a torn-down root; a rejection routes through onCommsError.
	const [daemon, setDaemon] = createSignal<DaemonInfo>(STUB_DAEMON);
	if (options.compass) {
		const client = options.compass;
		let disposed = false;
		if (getOwner()) onCleanup(() => (disposed = true));
		void probeServer(client)
			.then((info) => {
				if (!disposed)
					setDaemon({
						version: info.version,
						apiVersion: info.apiVersion,
						live: true,
					});
			})
			.catch((error) => {
				if (!disposed) options.onCommsError?.(error);
			});
	}

	// The per-post idempotency key source: `clientRequestId` must be
	// caller-unique, so a per-store random prefix plus a monotonic counter gives
	// a fresh key per post. Not reactive — it only sources fresh ids on demand.
	const requestIdPrefix = `ui-${Date.now().toString(36)}-${Math.random()
		.toString(36)
		.slice(2, 10)}`;
	let requestCount = 0;

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
			accounts().find((a) => a.id === callerId) ?? {
				id: callerId,
				handle: callerId,
				displayName: callerId,
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
	// Sourced from the hand-written fixture unless the caller supplies sessions
	// — fixture entries carry `fixture: true`, which keeps their never-minted
	// ids off the wire (see `stopAgent`).
	const sessions = options.sessions ?? STUB_SESSION_EVENTS;
	const agentSession = createMemo<AgentSession | undefined>(() => {
		const id = selectedAgentId();
		return id ? sessions[id] : undefined;
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
		const agentId = agentDmAccountId(chan, callerId, byId);
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
	// Join / subscribe are NOT WIRED (Matt's ruling). The wire has no join or
	// subscribe RPC yet — that slice is unbuilt — and the local-only mutation
	// these used to perform was a lie against the live stream: `adoptComms`
	// replaces the state wholesale on every push and `deriveMembership`
	// (live/adapt.ts) re-derives membership from the server's member lists, so
	// the toggle silently reverted mid-use (join → the composer enables → you
	// type → the next snapshot flips the row back and the composer disables
	// under your draft). A control that plainly does not work yet beats one that
	// appears to work and undoes itself, so the rail renders these disabled
	// (LeftSidebar) and the store fakes NO membership state.
	//
	// They keep their shape rather than being deleted: they are the seam the
	// rail's controls are bound to, and the RPCs land here when the slice is
	// built. Until then they are deliberately inert — `channelId` is unused
	// because there is nothing yet to do with it.
	const joinChannel = (_channelId: string) => {};
	const toggleSubscribe = (_channelId: string) => {};
	// Apply one answer to a question, or return the SAME question object when the
	// answer is rejected (unknown option, or a settled single-select). Returning
	// the identical reference is what lets answerAsk below tell "recorded" from
	// "no-op" — and a no-op must send nothing on the wire.
	const answerQuestion = (
		q: Ask["questions"][number],
		optionId: string,
	): Ask["questions"][number] => {
		if (!q.options.some((o) => o.id === optionId)) return q;
		// First-responder-wins: a single-select question settles on its first
		// answer; a later answer is a no-op. Multi-select stays a toggle.
		if (!q.allowMultiple && q.chosenOptionIds.length > 0) return q;
		const chosen = q.allowMultiple
			? q.chosenOptionIds.includes(optionId)
				? q.chosenOptionIds.filter((id) => id !== optionId)
				: [...q.chosenOptionIds, optionId]
			: [optionId];
		return { ...q, chosenOptionIds: chosen };
	};
	// Whether an ask has had its ONE RespondToAsk issued. Reactive so the render
	// can lock a submitted ask, and the guard that keeps the store from ever
	// issuing a second respond for the same ask (the server accepts exactly one:
	// go/internal/store/messages.go:400-403 rejects a later one with ErrConflict,
	// :437 flips Answered on the first). An ask is marked ONLY when a respond is
	// actually issued, so an offline store (no `comms`) never marks anything.
	const [submittedAskIds, setSubmittedAskIds] = createSignal<
		ReadonlySet<string>
	>(new Set());
	const isAskSubmitted = (askId: string) => submittedAskIds().has(askId);
	const unmarkAskSubmitted = (askId: string) =>
		setSubmittedAskIds((prev) => {
			const next = new Set(prev);
			next.delete(askId);
			return next;
		});
	// The last refusal per ask, keyed by askId — what the ask block RENDERS so a
	// refused respond is not user-invisible (the rollback makes the state honest,
	// but the click would otherwise vanish with only a console line). Cleared
	// when the user answers that ask again, i.e. on the next respond.
	const [askErrors, setAskErrors] = createSignal<ReadonlyMap<string, string>>(
		new Map(),
	);
	const askError = (askId: string) => askErrors().get(askId);
	const clearAskError = (askId: string) =>
		setAskErrors((prev) => {
			if (!prev.has(askId)) return prev;
			const next = new Map(prev);
			next.delete(askId);
			return next;
		});
	// Locate an ask by its message + ask coordinates in the current state.
	const findAsk = (messageId: string, askId: string): Ask | undefined => {
		const msg = messages().find((m) => m.id === messageId);
		for (const b of msg?.blocks ?? []) {
			if (b.kind === "ask" && b.ask.askId === askId) return b.ask;
		}
		return undefined;
	};
	// An ask is COMPLETE once every question holds at least one chosen option —
	// the point at which one atomic RespondToAsk can carry the whole thing.
	const isAskComplete = (ask: Ask) =>
		ask.questions.every((q) => q.chosenOptionIds.length > 0);
	// Whether an ask still carries exactly the answers that were SHIPPED — the
	// test a rollback must pass, since restoring over an ask the stream moved
	// meanwhile would overwrite the server's value with stale local state. A
	// vanished ask (`current` undefined) counts as moved: there is nothing left
	// to roll back into.
	const sameAnswers = (current: Ask | undefined, shipped: Ask) =>
		current !== undefined &&
		current.questions.length === shipped.questions.length &&
		current.questions.every((q, i) => {
			const was = shipped.questions[i];
			return (
				was !== undefined &&
				q.questionId === was.questionId &&
				q.chosenOptionIds.length === was.chosenOptionIds.length &&
				q.chosenOptionIds.every((id, j) => id === was.chosenOptionIds[j])
			);
		});
	// Replace an ask in place — the one write used both to record an answer and
	// to roll a refused one back.
	const putAsk = (messageId: string, ask: Ask) => {
		setComms((prev) => ({
			...prev,
			messages: prev.messages.map((msg) =>
				msg.id === messageId
					? {
							...msg,
							blocks: msg.blocks.map((b) =>
								b.kind === "ask" && b.ask.askId === ask.askId
									? { kind: "ask", ask }
									: b,
							),
						}
					: msg,
			),
		}));
	};
	// Issue the ask's ONE RespondToAsk. The wire is ATOMIC: exactly one
	// AskQuestionAnswer per question (comms.proto RespondToAsk — the server
	// rejects a request that omits one, and accepts a request exactly once per
	// ask), so this ships every question's settled choice in a single call.
	// `chosenOptionIds` is empty for a question the user skipped, which the wire
	// permits: the contract is coverage of every question, not an answer to each.
	//
	// A REFUSED respond must not leave the UI showing an answer the server does
	// not have: `rollback` (the ask as it stood before the click that triggered
	// the send) is restored, and the submitted mark is cleared so the ask is
	// retryable — the server only burns an ask on a respond it ACCEPTED. The
	// refusal is also recorded against the ask so the block can SAY so: a
	// rollback alone makes the state honest but silently erases the click.
	//
	// The restore is CONDITIONAL on the ask not having moved while the respond
	// was in flight. An `adoptComms` stream push landing between the click and
	// the refusal carries the AUTHORITATIVE server value (another participant's
	// accepted answer, say); restoring over it would show an ask state the
	// server never had, with no further push to correct it before a resync.
	//
	// KNOWN-BROKEN END TO END (SEA-1310): the agent SDK's correlation key is
	// unwired, so the answer does not reach the asking agent. The client side
	// is correct and stays wired; nothing here assumes the round-trip lands.
	const sendAsk = (messageId: string, ask: Ask, rollback?: Ask) => {
		const comms = options.comms;
		if (!comms) return;
		setSubmittedAskIds((prev) => new Set(prev).add(ask.askId));
		clearAskError(ask.askId);
		void comms
			.respondToAsk({
				askId: ask.askId,
				answers: ask.questions.map((q) => ({
					questionId: q.questionId,
					chosenOptionIds: [...q.chosenOptionIds],
				})),
			})
			.catch((error) => {
				unmarkAskSubmitted(ask.askId);
				if (rollback && sameAnswers(findAsk(messageId, ask.askId), ask)) {
					putAsk(messageId, rollback);
				}
				setAskErrors((prev) => {
					const next = new Map(prev);
					next.set(
						ask.askId,
						error instanceof Error ? error.message : String(error),
					);
					return next;
				});
				options.onCommsError?.(error);
			});
	};
	const answerAsk = (
		messageId: string,
		askId: string,
		questionId: string,
		optionId: string,
	) => {
		// A submitted ask is settled on the wire: recording a further click would
		// put the UI back into the exact lying state the gate removes.
		if (isAskSubmitted(askId)) return;
		// The ask BEFORE and AFTER the local edit. `before` is the rollback target
		// if the send this click triggers is refused; both stay undefined when the
		// coordinates miss or the answer is rejected — then nothing is sent.
		let before: Ask | undefined;
		let answered: Ask | undefined;
		setComms((prev) => ({
			...prev,
			messages: prev.messages.map((msg) => {
				if (msg.id !== messageId) return msg;
				return {
					...msg,
					blocks: msg.blocks.map((b) => {
						if (b.kind !== "ask" || b.ask.askId !== askId) return b;
						const ask = b.ask;
						const questions = ask.questions.map((q) =>
							q.questionId === questionId ? answerQuestion(q, optionId) : q,
						);
						// Reference-identical questions ⇒ the answer was rejected (or the
						// questionId named no question): leave the block untouched.
						if (questions.every((q, i) => q === ask.questions[i])) return b;
						before = ask;
						answered = { ...ask, questions };
						return { kind: "ask", ask: answered };
					}),
				};
			}),
		}));
		// The user acted on this ask again: whatever the last refusal said is no
		// longer what the block should be showing.
		if (answered) clearAskError(askId);
		// THE GATE (Matt's ruling): the click stays LOCAL until the ask is
		// COMPLETE. A per-click respond would persist a partial answer and lock
		// the ask against the rest of it — the server takes exactly one respond
		// per ask, forever. A single-question ask completes on its only click, so
		// it still sends there.
		if (!answered || !isAskComplete(answered)) return;
		sendAsk(messageId, answered, before);
	};
	// The skip affordance. A question the user means to SKIP never gets an
	// answer, so the ask never completes and `answerAsk` never sends it: this is
	// the explicit "send what I have" — the answered questions plus an empty
	// `chosenOptionIds` for each skipped one. Inert on an ask that is already
	// submitted, unknown, or wholly unanswered (there is nothing to submit).
	const submitAsk = (messageId: string, askId: string) => {
		if (isAskSubmitted(askId)) return;
		const ask = findAsk(messageId, askId);
		if (!ask) return;
		if (ask.questions.every((q) => q.chosenOptionIds.length === 0)) return;
		// No rollback target: nothing was recorded by this call, so a refusal
		// leaves the local record exactly as the user staged it — still honest,
		// still unsent, still retryable.
		sendAsk(messageId, ask);
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
	// The one write path for both a root post and a threaded reply: PostMessage
	// with a single text block and a fresh clientRequestId (the server dedups a
	// retry of the same key and suppresses the duplicate fan-out).
	//
	// NOTHING is inserted locally. PostMessage returns the stored Message AND
	// SubscribeComms echoes it; comms-state's upsertMessage dedups by message id
	// and splices into (atUnixMs, id) order, so letting the echo render it is
	// what makes it appear exactly once — a local insert would render a
	// duplicate under a different (minted) id until the next resync.
	//
	// Rejects rather than swallowing: the composer must be able to keep the
	// user's typed text when a post fails.
	const post = async (
		channelId: string,
		text: string,
		parentMessageId: string,
	): Promise<void> => {
		const client = options.comms;
		if (!client) {
			throw new Error(
				"cannot post: this store has no comms client (offline construction)",
			);
		}
		await client.postMessage({
			container: { case: "channelId", value: channelId },
			blocks: [{ block: { case: "text", value: text } }],
			parentMessageId,
			clientRequestId: `${requestIdPrefix}-${++requestCount}`,
		});
	};
	const postMessage = (channelId: string, text: string): Promise<void> =>
		post(channelId, text, "");
	const postReply = (
		channelId: string,
		parentMessageId: string,
		text: string,
	): Promise<void> => post(channelId, text, parentMessageId);

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
	// The last refused Stop, or undefined when the last attempt was not refused
	// — the reactive hole the log panel RENDERS, the same shape `askError` gives
	// the ask block. Without it a refusal is a console line and a refused Stop is
	// observably identical to a successful one (nothing visibly happens either
	// way). Cleared at the start of the next attempt.
	const [stopError, setStopError] = createSignal<string | undefined>(undefined);
	// Record a refusal AND keep routing it to the shell funnel — additive.
	const refuseStop = (error: unknown) => {
		setStopError(error instanceof Error ? error.message : String(error));
		options.onCommsError?.(error);
	};
	// The observation pane's stop control. Steering happens in the channel; this
	// is the one non-observational control.
	//
	// StopAgentSession's whole request is the server-minted `session_id`
	// (compass_pb.ts:831-836), so this stops the OBSERVED session — no selection,
	// no session, nothing issued (an empty-string stop would be a wrong live
	// request). It is CompassClient-backed, NOT comms: the two services are
	// separate clients over the one Connection.
	//
	// Never rejects, and never swallows. The RPC is Runner-backed: a server with
	// no RunnerHub attached answers `Unavailable` (go/server/service.go:152-154),
	// which is a REAL condition on the socket-only path, not a bug. There is no
	// user text to preserve (unlike a failed post, which rejects so the composer
	// can keep it), so the honest shape is to resolve and route the failure —
	// including the offline no-client case — to `onCommsError`, where the shell
	// surfaces it. Stop is idempotent server-side, so a retry after a refusal is
	// safe.
	const stopAgent = async (): Promise<void> => {
		setStopError(undefined);
		const session = agentSession();
		if (!session) return;
		// A fixture-sourced session's id was never minted by a server. Issuing
		// StopAgentSession for it is worse than doing nothing: the server's
		// unknown-session path is idempotent-success (go/internal/runner/host.go:
		// 217-228), so the RPC would return OK, stop nothing, and never reach
		// onCommsError — a control that is inert in the one way indistinguishable
		// from working. Refuse locally and say why instead. (The control also
		// renders disabled for such a session — LogPanel.tsx.)
		if (session.fixture) {
			refuseStop(
				new Error(
					"cannot stop: this session is fixture data, not a server-minted session",
				),
			);
			return;
		}
		const client = options.compass;
		if (!client) {
			refuseStop(
				new Error(
					"cannot stop: this store has no compass client (offline construction)",
				),
			);
			return;
		}
		try {
			await client.stopAgentSession({ sessionId: session.sessionId });
		} catch (error) {
			refuseStop(error);
		}
	};
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
		daemon,
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
		submitAsk,
		isAskSubmitted,
		askError,
		openThreadRootId,
		openThread,
		closeThread,
		postMessage,
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
		stopError,
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
