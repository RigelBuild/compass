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
import type { QueryClient } from "@tanstack/solid-query";
import { useQuery } from "@tanstack/solid-query";
import {
	type Accessor,
	createEffect,
	createMemo,
	createSignal,
	getOwner,
	onCleanup,
} from "solid-js";
import { agentDmAccountId } from "./comms";
import type {
	Account,
	Ask,
	Channel,
	ChannelGroup,
	ConvBlock,
	Message,
	Topic,
} from "./comms-stub";
import {
	type ActivityBarItem,
	fleetItemForAgent,
	RIGHT_SIDEBAR_ISSUE_ITEMS,
	RIGHT_SIDEBAR_TAB_BY_ID,
	type RightTabGroup,
	unreachableFleetItem,
} from "./constants";
import { createKeyboardSpine, type KeyboardSpine } from "./keyboard/spine";
import { adaptMessage } from "./live/adapt";
import { probeServer } from "./live/client";
import { type CommsState, EMPTY_COMMS_STATE } from "./live/comms-state";
import { runEventStream } from "./live/events";
import { runCommsStream } from "./live/stream";
import { joinAgents } from "./roster";
import type { AgentSession } from "./session-events";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import {
	type Agent,
	type DaemonInfo,
	type Issue,
	STUB_AGENTS,
	STUB_DAEMON,
	STUB_ISSUES,
	type TrackerConfig,
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
 *  primary, reachable from the top bar; they swap the whole UI. `channel` is the
 *  channel's topic index; `topic` is one topic's messages + composer; `agent` is
 *  the per-agent workspace — the agent's channel plus its tab/split panes. */
export type View =
	| "channel"
	| "topic"
	| "agent"
	| "bridge"
	| "backlog"
	| "done"
	| "settings";

/** Right-sidebar tabs (design dock-in-sidebar D1/T1/T2; Record A §T2). The
 *  fleet group is a CONFIGURABLE PIN SET, not a hardcoded agent pair: a pinned
 *  agent's tab id is `agent:${accountId}` (the open arm), alongside the static
 *  `status` fleet pane and the card-scoped issue tabs (Files / VCS / PR). No
 *  agent is special-cased — pinning is a separate presentation layer (moat
 *  retired, Matt's ruling). Split so the grouped activity bar and the
 *  chrome-hiding rule (D5) key off shape, not string lists. */
type PinnedAgentTab = `agent:${string}`;
export type IssueTab = "files" | "vcs" | "pr";
export type RightSidebarTab = PinnedAgentTab | "status" | IssueTab;

/** A persisted pin: the agent's account id plus the handle cached at pin time
 *  (SEA-1645). The cached handle is the degraded label an unreachable pin renders
 *  when its agent no longer resolves — so a dropped/despawned pin still shows the
 *  human name the user pinned, not an opaque id. A resolvable pin always renders
 *  its LIVE handle (via `fleetItemForAgent`), so the cache only ever surfaces
 *  once a pin is already unreachable. */
export interface PinnedAgent {
	id: string;
	handle: string;
}

/** A repo clone present in the selected agent's container (T6). Multi-repo
 *  capable now; the fixture derives a single clone per agent until the daemon
 *  reports more (resolved decision 3). */
export interface RepoClone {
	id: string;
	/** "owner/name", e.g. "RigelBuild/compass". */
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
	/** Whether the keyboard-shortcuts overlay is open (RIG-2482). */
	shortcutsOpen: Accessor<boolean>;
	/** Close the keyboard-shortcuts overlay (Escape/backdrop/navigation). */
	hideShortcuts: () => void;
	/** Toggle the keyboard-shortcuts overlay — the `?` / `view.shortcuts` action. */
	toggleShortcuts: () => void;
	/** Inject the router seam (record A3). Called once from App (inside the
	 *  router tree): supplies the real navigate + a reactive currentPath and
	 *  installs the single-writer route-sync effect. The store stays
	 *  router-import-free; before this the actions route through an in-memory
	 *  default so createAppStore is constructible with no router. */
	bindRouter: (r: {
		navigate: (path: string) => void;
		currentPath: () => string;
	}) => void;
	/** The app's keyboard spine (RIG-2456): the shared command registry and the
	 *  set of published roving groups. `App.tsx` installs the single window keymap
	 *  listener over its accessors; every surface registers commands / publishes
	 *  its roving group through it (keyboard/spine.ts). */
	readonly keyboard: KeyboardSpine;

	// ── Selection ──
	/** The selected agent id, or null. Drives the agent view + roster highlight. */
	selectedAgentId: Accessor<string | null>;
	/** The selected issue id, or null. Drives the detail + right sidebar. */
	selectedIssueId: Accessor<string | null>;
	/** The resolved selected agent, or undefined. */
	selectedAgent: Accessor<Agent | undefined>;
	/** The composed roster view-model for an account id — account + optional
	 *  lifecycle by shared account id — or undefined when no agent owns the id.
	 *  The pure seam (`joinAgents` in the real era) the workspace WILL read once
	 *  the SubscribeComms/SubscribeEvents join lands; today every render surface
	 *  resolves the agent through `selectedAgent()`. */
	agentView: (id: string) => Agent | undefined;
	/** The resolved selected issue, or undefined. */
	selectedIssue: Accessor<Issue | undefined>;
	/** Select an agent and switch to its view; re-selecting is a no-op. */
	openAgent: (agentId: string) => void;
	/** Select a channel and route to its view — UNLESS it's a 1:1 agent DM, in
	 *  which case delegate to openAgent (the workspace is the DM's surface). */
	openChannel: (channelId: string) => void;
	/** Select an issue (card / swimlane cell) and sync the roster to it. */
	selectIssue: (issueId: string) => void;

	// ── Panes ──
	/** Whether the left sidebar (folder tree) is shown. */
	leftOpen: Accessor<boolean>;
	toggleLeft: () => void;
	/** Whether the right sidebar (files / VCS / PR) is shown. */
	rightOpen: Accessor<boolean>;
	toggleRight: () => void;

	// ── Left-sidebar agent tree ──
	/** Whether a parent agent's subtree is collapsed in the derived tree. */
	isAgentCollapsed: (agentId: string) => boolean;
	toggleAgent: (agentId: string) => void;

	// ── Right sidebar: activity-bar tabs + pins + repos (T6; dock-in-sidebar D1;
	//    Record A §T2/T3; unreachable-pin amendment SEA-1645) ──
	/** The active right-sidebar tab: a pinned agent conversation
	 *  (`agent:${accountId}`), the `status` fleet pane, or an issue tab (Files /
	 *  VCS / PR). */
	activeRightTab: Accessor<RightSidebarTab>;
	setActiveRightTab: (tab: RightSidebarTab) => void;
	/** The pinned agent account ids, in pin order (append-on-pin; reorder is
	 *  deferred, OQ1). Persisted per workspace in `localStorage`; a pin that
	 *  resolves to no visible agent is RETAINED here (visibility fluctuates) and
	 *  still emits a (marked-unreachable) item from `rightTabGroups()`. Derived
	 *  id-valued view of `pinnedAgents()` for its existing consumers. */
	pinnedAgentIds: Accessor<readonly string[]>;
	/** The pinned agents as `{ id, handle }` pairs, in pin order — the handle is
	 *  cached at pin time (SEA-1645) so an unreachable pin renders the name the
	 *  user pinned. Persisted per workspace in `localStorage`. */
	pinnedAgents: Accessor<readonly PinnedAgent[]>;
	/** Pin an agent's conversation to the fleet activity bar (append if new). */
	pinAgent: (accountId: string) => void;
	/** Unpin an agent; if its tab is active, fall back to `status`. */
	unpinAgent: (accountId: string) => void;
	/** Whether an agent id is in the pin set. */
	isPinned: (accountId: string) => boolean;
	/** Resolve an account id to its visible agent, or undefined — the single
	 *  agent-resolution seam (SEA-1645 P5). A REACTIVE read: consumers that call
	 *  it (`rightTabGroups`, and transitively `activeFleetItem`) re-run when the
	 *  agent set changes. Resolves through the reactive `agents` memo (offline
	 *  `STUB_AGENTS`, live `joinAgents(accounts(), presence())`), so a
	 *  presence/account tick flips its answer. */
	agentById: (accountId: string) => Agent | undefined;
	/** The activity bar as ordered groups (unreachable-pin amendment SEA-1645):
	 *  the fleet group is EVERY pin (one item per pin, in pin order) plus the
	 *  static `status` item; a pin that resolves to no visible agent contributes
	 *  an item marked `unreachable`. The issue group is the static issue items. */
	rightTabGroups: Accessor<
		readonly { group: RightTabGroup; items: readonly ActivityBarItem[] }[]
	>;
	/** Repo clones present in the selected agent's container, for the repo/branch
	 *  dropdown. Empty when no agent is selected. */
	agentRepos: Accessor<RepoClone[]>;
	/** The active repo id within the selected agent's clones, or null. */
	activeRepoId: Accessor<string | null>;
	/** The resolved active repo, or undefined. */
	activeRepo: Accessor<RepoClone | undefined>;
	setActiveRepo: (repoId: string) => void;
	/** Switch the current branch by selecting the issue that owns it, so the
	 *  dropdown and the Files/VCS/PR panes move together. No-op unless the branch
	 *  belongs to an issue of the selected agent. */
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
	/** The board's live fleet — the roster view-models `joinAgents` composes
	 *  from `accounts` (identity) + the comms presence map (lifecycle/activity).
	 *  Offline (no `options.comms`) this is the STUB_AGENTS fixture; live it is
	 *  the joined roster. The accessor the board components cut over to in T4. */
	agents: Accessor<readonly Agent[]>;
	/** Whether the first comms snapshot has been adopted. Offline this stays
	 *  false; live it flips true once `adoptComms` lands the initial state — the
	 *  gate that distinguishes a genuinely empty roster from a not-yet-loaded one
	 *  (T5 tree-empty seam). */
	firstSnapshotArrived: Accessor<boolean>;
	/** All channel groups visible to the caller (the rail's group headers). */
	channelGroups: Accessor<readonly ChannelGroup[]>;
	/** All channels + DMs visible to the caller — the reactive rail source, so a
	 *  join/subscribe is visible everywhere at once. */
	channels: Accessor<readonly Channel[]>;
	/** All messages visible to the caller — the reactive conversation source. */
	messages: Accessor<readonly Message[]>;
	/** All topics visible to the caller — the reactive topic-index source. */
	topics: Accessor<readonly Topic[]>;
	/** The selected channel id, or null (the empty state before a pick). */
	selectedChannelId: Accessor<string | null>;
	/** The resolved selected channel, or undefined. */
	selectedChannel: Accessor<Channel | undefined>;
	/** The selected topic id, or null (no topic drilled into). Set by the
	 *  `/channel/:channelId/topic/:topicId` route via applyTopicRoute. */
	selectedTopicId: Accessor<string | null>;
	/** The resolved selected topic, or undefined. */
	selectedTopic: Accessor<Topic | undefined>;
	/** Drill into a topic's message view — navigate to
	 *  `/channel/<channelId>/topic/<topicId>`; the route-sync effect
	 *  (applyTopicRoute) writes view + selection. Resolves the topic's channel
	 *  off the topic set; a no-op on an unknown topic id. */
	openTopic: (topicId: string) => void;
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
	 *  respond per ask (go/internal/store/messages.go:404-405 rejects a later one,
	 *  :438 sets the flag that gate reads), so answers
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
	/** Post a message through the wire `PostMessage`: `container` = the channel,
	 *  `topic` = the topic oneof (post into an existing topic by id, or
	 *  get-or-create a topic by name — the "new topic" affordance), a single text
	 *  block, and a fresh `clientRequestId` (the server dedups a retry). Does NOT
	 *  insert locally: the stored message arrives through the SubscribeComms echo,
	 *  which `upsertMessage` dedups by id — so the sent message renders exactly
	 *  once. Rejects when the post fails (or when the store has no client) so the
	 *  composer can keep the user's text. */
	postMessage: (
		channelId: string,
		topic:
			| { case: "topicId"; value: string }
			| { case: "topicName"; value: string },
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

	// ── Issues (reactive board data) ──
	/** All issues — the reactive source every board surface reads, so a
	 *  streamed lifecycle update is visible everywhere at once (design "read
	 *  through the store accessors"). */
	issues: Accessor<Issue[]>;

	// ── Backlog view (D3) ──
	/** The current user's tracker-assigned issues (their personal queue), read
	 *  through the TrackerSeam for the Backlog view. */
	assignedIssues: Accessor<Issue[]>;

	// ── Tracker config (T11) ──
	/** The user's tracker wiring (kind + handle + Compass↔tracker mapping). */
	trackerConfig: Accessor<TrackerConfig>;
	setTrackerConfig: (cfg: TrackerConfig) => void;
}

/** What `createAppStore` is handed at boot. The network clients and seeds are
 *  optional so a unit test constructs the store with NO network client at all:
 *  the comms surface then holds `initialComms` (the fixture, in tests) and every
 *  write rejects. `queryClient` is the one REQUIRED field — the store holds it
 *  explicitly to run its query-backed reads (§A3), since its createRoot owner
 *  never sits under a `QueryClientProvider` (index.tsx builds the store before
 *  render() mounts the provider). index.tsx supplies the real
 *  `Connection`-derived client + caller alongside it. */
export interface AppStoreOptions {
	/** The server-state cache the store's query-backed reads key against — the
	 *  SAME instance components read through `QueryClientProvider`, so both paths
	 *  are one cache (§A1). REQUIRED and passed EXPLICITLY (never context): the
	 *  store's owner has no provider ancestor, so a context read would throw
	 *  `No QueryClient set` at boot (§A3). */
	readonly queryClient: QueryClient;
	/** The live comms client. Present → the store runs `runCommsStream` over it
	 *  for its lifetime and every comms write is a real RPC. Absent → offline. */
	readonly comms?: CommsClient;
	/** The caller's account id — whose visibility scopes every listing and whose
	 *  membership the rail reflects.
	 *
	 *  index.tsx learns it from the server via the compass.v1 WhoAmI RPC right
	 *  after the transport is up (live/client.ts resolveCaller) and feeds it here.
	 *  The fixture default keeps the offline store on the fixture's owner. */
	readonly callerId?: string;
	/** The workspace/connection identity used to namespace per-deployment UI
	 *  prefs in `localStorage` — the pinned-agent set (Record A §T3). index.tsx
	 *  derives it from the live `Connection` (baseUrl + caller) so one
	 *  deployment's account ids never hydrate as pins on another. Absent (offline
	 *  / tests) → the pin key falls back to `callerId`, so two stores built with
	 *  distinct caller/workspace identities keep separate pin sets. */
	readonly workspaceKey?: string;
	/** The comms state the store starts from before any stream push. Defaults to
	 *  EMPTY — tests that need populated comms pass the fixture explicitly. */
	readonly initialComms?: CommsState;
	/** The board issue list the store starts from before any event-stream push.
	 *  Defaults to STUB_ISSUES (the fixture); a test or an empty-board harness
	 *  route passes [] to construct a board with no issues. The live event stream
	 *  still replaces it (the accessor stays the seam). */
	readonly initialIssues?: readonly Issue[];
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
	 *  `postMessage` rejects to its caller instead (the composer must keep the
	 *  user's text) and does NOT route here. */
	readonly onCommsError?: (error: unknown) => void;
}

/** The `localStorage` handle, or undefined where it is absent or throwing (SSR,
 *  a privacy-locked context). Persistence is best-effort: a missing store means
 *  pins live only for the session, never a crash. */
function safeLocalStorage(): Storage | undefined {
	try {
		return globalThis.localStorage;
	} catch {
		return undefined;
	}
}

/** Hydrate the persisted pin set for a workspace. The key is namespaced by the
 *  workspace/connection identity (Record A §T3) so one deployment's account ids
 *  never hydrate as pins on another.
 *
 *  Self-healing per-element hydration (SEA-1645, no version flag): a bare
 *  `string` element is a LEGACY (pre-`{id,handle}`) pin and hydrates as
 *  `{ id, handle: id }`; an object carrying string `id`/`handle` hydrates as-is;
 *  anything else is dropped. A missing key, bad JSON, or a non-array payload
 *  yields the empty set. */
function loadPinnedAgents(workspace: string): readonly PinnedAgent[] {
	const store = safeLocalStorage();
	if (!store) return [];
	try {
		const raw = store.getItem(`compass.pinnedAgents.${workspace}`);
		if (!raw) return [];
		const parsed: unknown = JSON.parse(raw);
		if (!Array.isArray(parsed)) return [];
		const pins: PinnedAgent[] = [];
		for (const entry of parsed) {
			if (typeof entry === "string") {
				pins.push({ id: entry, handle: entry });
			} else if (
				typeof entry === "object" &&
				entry !== null &&
				"id" in entry &&
				"handle" in entry &&
				typeof entry.id === "string" &&
				typeof entry.handle === "string"
			) {
				pins.push({ id: entry.id, handle: entry.handle });
			}
		}
		return pins;
	} catch {
		return [];
	}
}

/** Write the pin set through to the workspace-namespaced key (best-effort). */
function savePinnedAgents(
	workspace: string,
	pins: readonly PinnedAgent[],
): void {
	const store = safeLocalStorage();
	if (!store) return;
	try {
		store.setItem(`compass.pinnedAgents.${workspace}`, JSON.stringify(pins));
	} catch {
		// Best-effort: a quota / privacy-locked write failure is non-fatal.
	}
}

/**
 * Build the app store. Called once at the app root; the instance is provided
 * through context. With `options.comms` set, the comms accessors are fed by the
 * live SubscribeComms stream, which runs until the store's reactive owner is
 * disposed (index.tsx's root lives for the app's lifetime).
 */
export function createAppStore(options: AppStoreOptions): AppStore {
	const callerId = options.callerId ?? CALLER_ID;
	// The issue list is reactive so promote/archive (below) are visible on
	// every surface at once. Seeded from the fixture (or an explicit override);
	// the real @compass/client stream replaces the seed later (the accessor
	// stays the seam).
	const [issues, setIssues] = createSignal<Issue[]>([
		...(options.initialIssues ?? STUB_ISSUES),
	]);

	const [view, setView] = createSignal<View>("bridge");
	const [selectedAgentId, setSelectedAgentId] = createSignal<string | null>(
		null,
	);
	// Default to the first issue so the seam survives swapping the fixture
	// for the real @compass/client (no hardcoded stub id); an empty board
	// starts with no selection.
	const [selectedIssueId, setSelectedIssueId] = createSignal<string | null>(
		(options.initialIssues ?? STUB_ISSUES)[0]?.id ?? null,
	);

	// ── Router seam (record A3): routes are the source of truth ──────────────
	// The routed dimension — view + the surface's identifying param — is driven
	// by the URL, not set imperatively. The store lives outside the router tree
	// (index.tsx's app-lifetime createRoot), so the router is INJECTED as a seam
	// rather than imported: createAppStore stays router-free and constructible
	// with no router. Until App binds the real router, a default in-memory seam
	// applies routes SYNCHRONOUSLY, so an offline store (unit/fragment tests, no
	// <App>) drives openChannel/openAgent/show* and reads the routed state in the
	// same tick — "test-constructible exactly as today". App swaps in the real
	// @solidjs/router navigate + a reactive currentPath via bindRouter, after
	// which navigation is asynchronous (navigate → location → route-sync effect
	// → applyRoute), the ratified routes-as-truth contract.
	const [inMemoryPath, setInMemoryPath] = createSignal("/");
	let routerNavigate = (path: string): void => {
		// Pre-bind navigation: nothing in production navigates before App binds
		// (the only pre-bind action sources — the async comms stream and the
		// assigned-issues query — never navigate), so reaching here in a dev build
		// signals a future violation where the URL would silently diverge from
		// the in-memory path. Warn loudly rather than fail silent. Offline tests
		// (run outside vite, import.meta.env.DEV undefined) drive this path by
		// design and stay quiet.
		if (import.meta.env?.DEV) {
			console.warn(
				`compass: navigate("${path}") before bindRouter — the URL will not ` +
					"update until App wires the router",
			);
		}
		setInMemoryPath(path);
		applyRoute(path);
	};
	let routerCurrentPath = (): string => inMemoryPath();
	const navigateTo = (path: string): void => routerNavigate(path);
	const currentPath = (): string => routerCurrentPath();
	// Wire the real router (called once from App, inside the router tree + a
	// reactive root). The route-sync effect is the SINGLE writer of the routed
	// dimension under the real router: the location drives applyRoute. Created
	// here so App's owner disposes it on unmount.
	const bindRouter = (r: {
		navigate: (path: string) => void;
		currentPath: () => string;
	}): void => {
		routerNavigate = r.navigate;
		routerCurrentPath = r.currentPath;
		createEffect(() => applyRoute(r.currentPath()));
	};

	// The tracker wiring (T11) + the seam it drives. assignedIssues (D3) is the
	// user's personal queue, read through a query keyed on the tracker handle: a
	// handle change re-keys and refetches with no manual reload (§A3). The tracker
	// seam is NOT yet a Connect RPC (tracker.ts is the fixture contract), so this
	// is a plain solid-query key + queryFn over the seam — the store-internal
	// query pattern (explicit `queryClient`, no provider ancestor), swappable to a
	// connect-query-core descriptor when the daemon RPC lands.
	const [trackerConfig, setTrackerConfigSignal] = createSignal<TrackerConfig>(
		DEFAULT_TRACKER_CONFIG,
	);
	let seam: TrackerSeam = createFixtureTrackerSeam(DEFAULT_TRACKER_CONFIG);
	const issuesQuery = useQuery(
		() => ({
			// The handle is part of the key, so a `setTrackerConfig` that changes it
			// re-keys and refetches — the old manual re-load dance is gone.
			queryKey: ["assignedIssues", trackerConfig().handle] as const,
			queryFn: (): Promise<Issue[]> =>
				seam.listAssignedIssues(trackerConfig().handle),
		}),
		// Explicit client — the store's owner has no QueryClientProvider ancestor
		// (§A3), so this must never resolve from context.
		() => options.queryClient,
	);
	// Today's fallback preserved: no data yet (pre-fetch) or a failed read both
	// read as the empty queue, exactly as the catch-to-`[]` loader did.
	const assignedIssues: Accessor<Issue[]> = () => issuesQuery.data ?? [];

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

	// ── Right sidebar (T6; dock-in-sidebar D1/D6; Record A §T2/T3/T5;
	//    unreachable-pin amendment SEA-1645): active tab + pin set + repo/branch ──
	// The pinned agent set: ordered, append-on-pin, persisted per workspace so one
	// deployment's account ids never hydrate on another. Held as `{ id, handle }`
	// pairs (SEA-1645 P0) — the handle cached at pin time is the degraded label an
	// unreachable pin renders. A pin that resolves to no visible agent is RETAINED
	// here (visibility fluctuates — the pin survives the agent returning) and still
	// emits a marked item from the derivation below. Falls back to `callerId` when
	// no workspace identity is supplied.
	const workspaceKey = options.workspaceKey ?? callerId;
	const [pinnedAgents, setPinnedAgents] = createSignal<readonly PinnedAgent[]>(
		loadPinnedAgents(workspaceKey),
	);
	const pinnedAgentIds = createMemo<readonly string[]>(() =>
		pinnedAgents().map((p) => p.id),
	);
	// The single agent-resolution seam (SEA-1645 P5): resolve an account id to
	// its visible agent. A REACTIVE read — a closure over the `agents` memo, so
	// every consumer (`rightTabGroups`, transitively `activeFleetItem`) re-runs
	// when the agent set changes. The live-agents migration this seam owed is
	// discharged: `agents` is now the reactive join memo below (offline fixture,
	// live `joinAgents(accounts(), presence())`), not a static const, so a
	// presence/account tick flips resolution live→reachable through this one seam.
	const agentById = (accountId: string): Agent | undefined =>
		agents().find((a) => a.account.id === accountId);
	// The active repo id (T6). The current branch is derived from the selected
	// issue (see agentRepos), so there's no separate branch-pick signal to
	// drift from the panes.
	const [activeRepoId, setActiveRepoId] = createSignal<string | null>(null);

	// ── Comms: the channel surface (design compass-0.7) ──
	// ONE reduced CommsState drives all four comms accessors. It starts at
	// `initialComms` (EMPTY by default — the store no longer boots from the
	// fixture) and is replaced wholesale by each `runCommsStream` push — bar the
	// in-progress local ask answers `preserveLocalAsks` carries across, the one
	// state the server cannot send back because it was never told; the local
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
	const topics = createMemo(() => comms().topics);
	// The board's fleet (§T3). An intermediate presence memo beside the
	// per-collection memos: it re-notifies only when the presence map's identity
	// actually changes — each posted message replaces the whole CommsState via
	// `adoptComms`, and this memo's `===` equality plus the reducer's structural
	// sharing absorb that, so a chat message does not re-join the roster. Then
	// the join itself, gated on the live/offline switch: offline it is the
	// fixture; live it re-joins only when accounts or presence change.
	const presence = createMemo(() => comms().presence);
	const agents = createMemo<readonly Agent[]>(() =>
		options.comms ? joinAgents(accounts(), presence()) : STUB_AGENTS,
	);
	// Boot default (Record A §T5): the first hydrated pin that resolves to a
	// visible agent, else the static `status` pane. Boot has no mid-view state to
	// preserve, so it lands on a live pane rather than an unreachable one (SEA-1645
	// P4, OQ-1 ruled kept). An unresolvable leading pin is skipped here but still
	// shows its (marked) bar item. The D6 no-auto-switch rule is unchanged.
	const firstResolvablePin = pinnedAgentIds().find(
		(id) => agentById(id) !== undefined,
	);
	const [activeRightTab, setActiveRightTabRaw] = createSignal<RightSidebarTab>(
		firstResolvablePin ? `agent:${firstResolvablePin}` : "status",
	);
	// The single public set seam (SEA-1645 P3): a plain pass-through. The old
	// resolvability guard (coerce an unresolvable `agent:` tab to `status`) is
	// retired — selecting or keeping an unresolvable agent tab is now valid and
	// renders the unreachable pane, so an `agent:` tab no longer requires a visible
	// agent. The unpin-active→status fallback (a user gesture) still routes through
	// here. No fluctuation-watcher: a visibility change never coerces the tab.
	const setActiveRightTab = (tab: RightSidebarTab) => {
		setActiveRightTabRaw(tab);
	};
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
	// True once the first comms snapshot has arrived from the stream. The
	// pending-aware route fallback (applyChannelRoute) reads it: before the first
	// snapshot an absent channel id is merely not-yet-loaded (held, not bounced);
	// after it, an absent id is genuinely unknown (redirected).
	const [firstSnapshotArrived, setFirstSnapshotArrived] = createSignal(false);
	// Adopt a state pushed by the stream and settle the selection onto it: the
	// user's explicit pick wins as long as the channel is still visible, so a
	// later snapshot/event can never yank the surface out from under them; an
	// absent or vanished selection falls back to the first subscribed channel.
	//
	// The push is adopted WHOLESALE except for in-progress local ask answers,
	// which the server has never been told about and so cannot send back — see
	// `preserveLocalAsks`.
	const adoptComms = (next: CommsState) => {
		setComms((prev) => preserveLocalAsks(prev, next));
		setFirstSnapshotArrived(true);
		const current = selectedChannelId();
		if (current && next.channels.some((c) => c.id === current)) return;
		// The selection is absent from the pushed snapshot (vanished, or the boot
		// null). Under routes-as-truth the channel surface is a route: if the
		// current route names a channel, re-point it through navigate so the URL
		// and selection stay one authority; then re-seed the signal as the
		// "last visited channel" fallback for any other surface.
		const fallback = firstChannelId(next);
		if (currentPath().startsWith("/channel/")) {
			navigateTo(fallback ? `/channel/${fallback}` : "/");
		}
		setSelectedChannelId(fallback);
	};
	// The selected topic id on the topic view, or null. Written solely by the
	// route-sync (applyTopicRoute), the single writer of the routed dimension —
	// openTopic navigates, it does not set this directly.
	const [selectedTopicId, setSelectedTopicId] = createSignal<string | null>(
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
		// The live board read path: run the SubscribeEvents driver for the store's
		// lifetime, replacing the STUB_ISSUES seed with the server's snapshot-as-
		// events then live upserts. Aborted on teardown (a test root's dispose
		// stops it; the app root never disposes). Reuses `options.compass` — the
		// same client the daemon probe dials. `runEventStream` resolves only on
		// abort and retries internally, so nothing awaits it; a rejection routes
		// through onCommsError rather than being swallowed.
		const eventsAbort = new AbortController();
		if (getOwner()) onCleanup(() => eventsAbort.abort());
		void runEventStream({
			client,
			onIssues: setIssues,
			signal: eventsAbort.signal,
			onError: (error) => options.onCommsError?.(error),
		}).catch((error) => {
			if (!eventsAbort.signal.aborted) options.onCommsError?.(error);
		});
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
	// for — distinct from `selectedAgentId`, which the board's `selectIssue`
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
		agents().find((a) => a.account.id === selectedAgentId()),
	);
	// The pure seam that composes the durable `account` with the optional
	// ephemeral `lifecycle` by shared account id (`joinAgents` in the real era) —
	// lifecycle is already carried on the view-model, so this is a lookup.
	const agentView = (id: string): Agent | undefined =>
		agents().find((a) => a.account.id === id);
	const selectedIssue = createMemo(() =>
		issues().find((w) => w.id === selectedIssueId()),
	);
	// The selected agent's repo clones (T6). The fixture models one clone per
	// agent — the monorepo — with the branches drawn from that agent's assigned
	// issues (design "single clone until the daemon reports more"). The
	// accessor returns an array so a multi-clone daemon is a fixture change, not
	// a shape change. `currentBranch` is derived from the selected issue
	// (each issue owns one branch) — so the dropdown, the detail panes, and
	// the board selection are one source of truth and can't drift apart.
	const agentRepos = createMemo<RepoClone[]>(() => {
		const id = selectedAgentId();
		if (!id) return [];
		const owned = issues().filter((w) => w.assignee === id);
		if (owned.length === 0) return [];
		const branches = owned.map((w) => w.branch);
		// The current branch is the selected issue's branch when it belongs
		// to this agent, else the primary (first) — never a stale independent pick.
		const selected = owned.find((w) => w.id === selectedIssueId());
		return [
			{
				id: `${id}-repo`,
				name: "RigelBuild/compass",
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
	const selectedTopic = createMemo(() =>
		topics().find((t) => t.id === selectedTopicId()),
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

	// ── Route application (record A3): the single writer of the routed
	// dimension (view + selectedChannelId/selectedAgentId). Invoked
	// synchronously by the default seam (offline) or by the bound route-sync
	// effect (real router). It parses currentPath itself — the store is outside
	// the router tree and cannot call useParams.
	function applyRoute(path: string): void {
		const segs = path.split("/").filter((s) => s.length > 0);
		const [head, param, sub, subParam] = segs;
		switch (head) {
			case undefined:
				setView("bridge");
				return;
			case "channel":
				// `/channel/:channelId/topic/:topicId` drills into a topic; the plain
				// `/channel/:channelId` shows the topic index.
				if (param && sub === "topic" && subParam) {
					applyTopicRoute(param, subParam);
				} else if (param) {
					applyChannelRoute(param);
				} else {
					setView("bridge");
				}
				return;
			case "agent":
				if (param) applyAgentRoute(param);
				else setView("bridge");
				return;
			case "backlog":
				setView("backlog");
				return;
			case "done":
				setView("done");
				return;
			case "settings":
				setView("settings");
				return;
			default:
				// Unknown path — the router's `*` route redirects to "/"; leave the
				// routed state untouched until that navigation lands (no blank
				// surface, no wrong-view write).
				return;
		}
	}
	// Apply a `/channel/:channelId` route. Pending-aware: an id merely not-yet-
	// loaded is HELD (ChannelView renders its empty state), NOT bounced — a valid
	// deep-link into an async-loaded channel must survive boot. Only once the
	// first snapshot has arrived is an absent id treated as genuinely unknown and
	// redirected off (gating on first-snapshot arrival, never non-emptiness: a
	// genuinely empty workspace is a valid resolved state). Deliberately does NOT
	// clear selectedTopicId — the topic dimension has exactly one writer
	// (applyTopicRoute); a stale id left here is inert because every read of
	// selectedTopicId()/selectedTopic() is view()-guarded (TopicView unmounts and
	// the sidebar's selected class gates on view() === "topic").
	function applyChannelRoute(channelId: string): void {
		setView("channel");
		if (channels().some((c) => c.id === channelId)) {
			setSelectedChannelId(channelId);
			return;
		}
		if (firstSnapshotArrived()) {
			const fallback = firstChannelId(comms());
			navigateTo(fallback ? `/channel/${fallback}` : "/");
			return;
		}
		setSelectedChannelId(channelId);
	}
	// Apply a `/channel/:channelId/topic/:topicId` route — the topic message view.
	// The SOLE writer of the topic dimension, following applyChannelRoute's
	// pending-aware pattern: the channel selection is set the same way (held while
	// not-yet-loaded, bounced to the fallback channel once the snapshot has
	// arrived and it is genuinely absent), and the topic id is held on the signal
	// so a deep-link into an async-loaded topic survives boot. An absent topic
	// after the snapshot has arrived falls back to the channel's index rather than
	// a blank topic view.
	function applyTopicRoute(channelId: string, topicId: string): void {
		setView("topic");
		const channelKnown = channels().some((c) => c.id === channelId);
		if (!channelKnown && firstSnapshotArrived()) {
			const fallback = firstChannelId(comms());
			navigateTo(fallback ? `/channel/${fallback}` : "/");
			return;
		}
		setSelectedChannelId(channelId);
		if (topics().some((t) => t.id === topicId)) {
			setSelectedTopicId(topicId);
			return;
		}
		if (firstSnapshotArrived()) {
			// The topic is genuinely unknown — drop back to the channel's index.
			setView("channel");
			setSelectedTopicId(null);
			navigateTo(`/channel/${channelId}`);
			return;
		}
		setSelectedTopicId(topicId);
	}
	// Apply an `/agent/:agentId` route — the workspace anchoring lifted verbatim
	// from the old openAgent so the click path and a direct deep-link run the
	// SAME code once. Anchor the issue selection to this agent: keep the current
	// selection when this agent owns it (a card double-click selects the card's
	// issue just before opening — often a non-primary one), else the agent's
	// primary (first-owned). The reset guard keys on `agentViewAgentId` (the id
	// the workspace was initialized for) — NOT `selectedAgentId`, which
	// `selectIssue` moves from the board without initializing the workspace — so
	// a roster move followed by opening that agent still initializes the view,
	// and re-opening the already-initialized agent only re-asserts the selection
	// and preserves the tabs the user has since opened.
	function applyAgentRoute(agentId: string): void {
		setView("agent");
		const owned = issues().filter((w) => w.assignee === agentId);
		const anchored =
			owned.find((w) => w.id === selectedIssueId())?.id ?? owned[0]?.id ?? null;
		if (agentId === agentViewAgentId()) {
			setSelectedAgentId(agentId);
			setSelectedIssueId(anchored);
			return;
		}
		setSelectedAgentId(agentId);
		setSelectedIssueId(anchored);
		setActiveRepoId(`${agentId}-repo`);
		setTabs([chatTab()]);
		setActiveAgentTabId(CHAT_TAB_ID);
		setLogOpen(true);
		setAgentViewAgentId(agentId);
	}

	// Open an agent's workspace: navigate — the route-sync effect (applyAgentRoute)
	// runs the anchoring, so the click path and a `/agent/:agentId` deep-link share
	// one home.
	const openAgent = (agentId: string) => {
		navigateTo(`/agent/${agentId}`);
	};

	// Open a channel: route to the channel's topic index with it selected — unless
	// it's a 1:1 agent DM, in which case its surface is the agent workspace, so
	// delegate to openAgent (one entry point, no dead-end DM view). Unknown id is a
	// no-op.
	const openChannel = (channelId: string) => {
		const chan = channels().find((c) => c.id === channelId);
		if (!chan) return;
		const byId = new Map(accounts().map((a) => [a.id, a]));
		const agentId = agentDmAccountId(chan, callerId, byId);
		if (agentId) {
			openAgent(agentId);
			return;
		}
		navigateTo(`/channel/${channelId}`);
	};

	// Drill into a topic's message view: navigate to
	// `/channel/<channelId>/topic/<topicId>` — the route-sync effect
	// (applyTopicRoute) is the single writer that sets view + selection, so the
	// click path and a topic deep-link share one home. Resolves the topic's
	// channel off the topic set; a no-op on an unknown topic id (nothing to route
	// to). NEVER setView — navigation is the sole entry.
	const openTopic = (topicId: string) => {
		const topic = topics().find((t) => t.id === topicId);
		if (!topic) return;
		navigateTo(`/channel/${topic.channelId}/topic/${topicId}`);
	};

	// Selecting an issue (a board card or a swimlane cell) syncs the roster
	// to its assignee but stays on the board — it does not jump into the agent
	// view, so the board stays the working surface while you scan cards.
	const selectIssue = (issueId: string) => {
		setSelectedIssueId(issueId);
		const ws = issues().find((w) => w.id === issueId);
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
	// go/internal/store/messages.go:404-405 rejects a later one with ErrConflict,
	// :438 flips Answered on the first). An ask is marked ONLY when a respond is
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
	// Whether two asks pose the SAME questions, in the same order, offering the
	// same options — the shape test every ask-to-ask comparison starts from. An
	// ask whose shape moved is a different ask as far as local state is
	// concerned: there is nothing left to line the answers up against.
	//
	// The OPTION IDS are part of that shape, not decoration on it. The server's
	// block-update path rewrites a message's entire block set and requires only
	// that `ask_id` survive (go/internal/store/messages.go:151, :163-167), so an
	// option's id can appear, vanish, or be replaced under a stable question id.
	// Comparing question ids alone would miss that, leaving the UI rendering a
	// WITHDRAWN option and shipping an option id the server no longer offers —
	// which `validateQuestionAnswer` rejects as ErrInvalidArgument, a refusal the
	// user cannot act on, because the option they need is not on screen. So the
	// offered option ids, in order, are part of the shape the compare checks.
	//
	// What it deliberately does NOT distinguish: a revision to question text,
	// option LABELS, or allowMultiple under stable question and option ids. When
	// the ids all still line up, `preserveLocalAsks` carries the local ask copy
	// forward whole, so such a revision would momentarily render with the local
	// wording. That is tolerable only because the path is unwired (see below) and
	// a text/label edit is cosmetic and self-corrects on the next resync. Wiring
	// the block-update path live must instead overlay the local picks onto the
	// PUSHED question objects, so a server revision to any non-id field is
	// adopted while the in-progress pick survives.
	//
	// Scope honesty: this is designed-for, not yet wired. Nothing calls the
	// block-update RPC today, so the widened compare defends a documented wire
	// capability (comms.proto MessageUpdated carries the full CURRENT block set)
	// rather than a bug in flight.
	const sameQuestions = (a: Ask, b: Ask) =>
		a.questions.length === b.questions.length &&
		a.questions.every((q, i) => {
			const other = b.questions[i];
			return (
				other !== undefined &&
				q.questionId === other.questionId &&
				q.options.length === other.options.length &&
				q.options.every((o, j) => o.id === other.options[j]?.id)
			);
		});
	// Whether an ask still carries exactly the answers that were SHIPPED — the
	// test a rollback must pass, since restoring over an ask the stream moved
	// meanwhile would overwrite the server's value with stale local state. A
	// vanished ask (`current` undefined) counts as moved: there is nothing left
	// to roll back into.
	const sameAnswers = (current: Ask | undefined, shipped: Ask) =>
		current !== undefined &&
		sameQuestions(current, shipped) &&
		current.questions.every((q, i) => {
			const was = shipped.questions[i];
			return (
				was !== undefined &&
				q.chosenOptionIds.length === was.chosenOptionIds.length &&
				q.chosenOptionIds.every((id, j) => id === was.chosenOptionIds[j])
			);
		});
	// An ask the server has said nothing about. The server's own `answered` flag
	// is the authority: it flips exactly once, on the first RespondToAsk the
	// server ACCEPTED, so `!answered` is precisely "no authoritative value yet".
	//
	// Scanning the questions for an empty `chosenOptionIds` CANNOT stand in for
	// it, because two answer shapes the server accepts and records leave every
	// question's chosen ids empty on a CLOSED ask: (a) a deliberate skip — an
	// answer entry with no chosen ids and empty custom_text is an ACCEPTED skip
	// that satisfies the wire's coverage-of-every-question contract (see
	// `submitAsk`); (b) a custom_text-only answer to a free-text question, which
	// carries no options to choose. Against either, a question scan reports "the
	// server has no value" for an ask the server has already closed, so
	// `preserveLocalAsks` restores stale local picks over it and the completing
	// respond comes back ErrConflict (comms.proto Ask.answered).
	const serverHasNoAnswer = (ask: Ask) => !ask.answered;
	// Carry in-progress LOCAL ask answers across a stream push.
	//
	// The wire is atomic — one RespondToAsk per ask, issued only on the click
	// that COMPLETES it (see `sendAsk`) — so on a multi-question ask every choice
	// but the last lives ONLY in this state. `adoptComms` otherwise replaces it
	// wholesale, so a push landing mid-ask would silently discard the user's
	// clicks, and the server could not send them back: it was never told.
	//
	// A local answer is kept ONLY where the server demonstrably has no value of
	// its own, which keeps "a server value for an ask is AUTHORITATIVE" — the
	// property `sendAsk`'s conditional rollback rests on — exactly true:
	//
	//   - the ask must not be SUBMITTED: once our respond is issued the local
	//     record is a claim about what the server was told, not an edit in
	//     progress, and the rollback decides by comparing it against whatever
	//     the stream has since put in its place;
	//   - the pushed ask must not be ANSWERED: once the server's `answered` flag
	//     is set the ask is closed and its record — ours accepted, or another
	//     participant's — wins;
	//   - the questions must line up, or the ask's shape moved and it is new;
	//   - the LOCAL ask must carry an unshipped pick at all: a wholly untouched
	//     ask has nothing to carry, so it is skipped and the pushed state is
	//     adopted by reference — the fast path nearly every push takes.
	//
	// A hoisted declaration so it can sit beside the ask machinery it reuses
	// while `adoptComms`, defined above with the rest of the stream wiring,
	// still calls it.
	function preserveLocalAsks(prev: CommsState, next: CommsState): CommsState {
		// The unsubmitted asks carrying a local pick, by message id then ask id.
		// Empty whenever no ask is mid-answer — which is nearly every push — and
		// then the pushed state is adopted untouched, references and all.
		//
		// This leg scans the LOCAL record's chosen ids on purpose, and does not
		// consult `answered` on a `prev` entry at all: the question here is only
		// "is there an unshipped edit worth carrying", which the chosen ids
		// answer by themselves. A `prev` entry is the last SERVER state we
		// adopted with our clicks layered over it — `answerAsk` spreads the ask
		// it edits (`{ ...ask, questions }`), so a server `answered: true` rides
		// straight through onto a locally-edited ask — which makes the flag on a
		// `prev` entry a statement about the ask we ADOPTED, not about our edit.
		// The authority question — has the server closed this ask — is asked
		// where its answer lives: `serverHasNoAnswer` on the PUSHED ask below.
		const local = new Map<string, Map<string, Ask>>();
		for (const msg of prev.messages) {
			for (const b of msg.blocks) {
				if (b.kind !== "ask") continue;
				if (isAskSubmitted(b.ask.askId)) continue;
				if (b.ask.questions.every((q) => q.chosenOptionIds.length === 0))
					continue;
				const byAsk = local.get(msg.id) ?? new Map<string, Ask>();
				byAsk.set(b.ask.askId, b.ask);
				local.set(msg.id, byAsk);
			}
		}
		if (local.size === 0) return next;
		let touched = false;
		const messages = next.messages.map((msg) => {
			const byAsk = local.get(msg.id);
			if (!byAsk) return msg;
			let replaced = false;
			const blocks = msg.blocks.map((b): ConvBlock => {
				if (b.kind !== "ask") return b;
				const mine = byAsk.get(b.ask.askId);
				if (!mine || !serverHasNoAnswer(b.ask) || !sameQuestions(b.ask, mine)) {
					return b;
				}
				replaced = true;
				return { kind: "ask", ask: mine };
			});
			if (!replaced) return msg;
			touched = true;
			return { ...msg, blocks };
		});
		return touched ? { ...next, messages } : next;
	}
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
	// A CLOSED ask is the case `sameAnswers` cannot see, because it compares
	// answers and `answered` is not one. The server flips the flag in the very
	// write that records the chosen ids (go/internal/store/messages.go:438,
	// beside the :435 that records them) and refuses every later respond with
	// ErrConflict (:404-406), so a pushed ask carrying `answered` is CLOSED —
	// and on the accepted-then-lost-reply path (the server COMMITTED our respond
	// and published the update, but our RPC's own reply never landed) that push
	// carries OUR chosen ids, which is precisely what makes `sameAnswers` pass.
	// Restoring there would overwrite the authoritative CLOSED state with the
	// stale OPEN one, re-enable every option, and re-offer a click that can only
	// produce ErrConflict — or ship a DIFFERENT answer than the one durably
	// recorded, showing the user a state contradicting the audit record. So the
	// guard sits at the SITE, not in `sameAnswers`: a rollback into a closed ask
	// is never right whether or not the answers line up.
	//
	// The submitted mark is still cleared on that path, so the ask is not left
	// falsely "in flight"; it is left CLOSED, which is the truth — and the write
	// gates (`answerAsk`, `submitAsk`) read the flag, so nothing further ships.
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
				const current = findAsk(messageId, ask.askId);
				if (
					rollback &&
					current &&
					!current.answered &&
					sameAnswers(current, ask)
				) {
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
						// The other way an ask is settled, and the one the submitted
						// mark cannot see: the server burns an ask on the first
						// RespondToAsk it ACCEPTS and refuses every later one with
						// ErrConflict (go/internal/store/messages.go:404-406). An ask
						// carrying `answered` is therefore closed no matter who closed
						// it — us on a previous run, another participant, or a push we
						// adopted already-closed — so recording a click here could only
						// ever complete the ask into a respond the server is guaranteed
						// to refuse. Refusing the click is the honest surface; shipping
						// the doomed RPC and rendering its error is not.
						if (ask.answered) return b;
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
	// submitted, CLOSED by the server, unknown, or wholly unanswered (there is
	// nothing to submit).
	const submitAsk = (messageId: string, askId: string) => {
		if (isAskSubmitted(askId)) return;
		const ask = findAsk(messageId, askId);
		if (!ask) return;
		// Closed server-side: the ask's one accepted respond has already been
		// taken, and messages.go:404-406 refuses a second with ErrConflict. The
		// submitted mark does not cover this — the ask can arrive closed on a
		// push, or be closed by another participant, without this client ever
		// having issued a respond.
		if (ask.answered) return;
		// "Nothing staged" is a question about the LOCAL record, so it scans the
		// chosen ids rather than the server's `answered` flag: this ask has never
		// been shipped, so the server has no view of it to consult.
		if (ask.questions.every((q) => q.chosenOptionIds.length === 0)) return;
		// No rollback target: nothing was recorded by this call, so a refusal
		// leaves the local record exactly as the user staged it — still honest,
		// still unsent, still retryable.
		sendAsk(messageId, ask);
	};
	// The one write path: PostMessage with the channel `container`, the `topic`
	// oneof (post into an existing topic by id, or get-or-create by name), a
	// single text block and a fresh clientRequestId (the server dedups a retry of
	// the same key and suppresses the duplicate fan-out).
	//
	// NOTHING is inserted locally. PostMessage returns the stored Message AND
	// SubscribeComms echoes it; comms-state's upsertMessage dedups by message id
	// and splices into (atUnixMs, id) order, so letting the echo render it is
	// what makes it appear exactly once — a local insert would render a
	// duplicate under a different (minted) id until the next resync.
	//
	// Rejects rather than swallowing: the composer must be able to keep the
	// user's typed text when a post fails.
	const postMessage = async (
		channelId: string,
		topic:
			| { case: "topicId"; value: string }
			| { case: "topicName"; value: string },
		text: string,
	): Promise<void> => {
		const client = options.comms;
		if (!client) {
			throw new Error(
				"cannot post: this store has no comms client (offline construction)",
			);
		}
		await client.postMessage({
			container: { case: "channelId", value: channelId },
			topic,
			blocks: [{ block: { case: "text", value: text } }],
			clientRequestId: `${requestIdPrefix}-${++requestCount}`,
		});
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
	// Keyboard-shortcuts overlay (RIG-2482): the open signal + its show/hide/
	// toggle closures live here, so the spine's `view.shortcuts` command (created
	// below) closes over `toggleShortcuts` next to its behavior, and App.tsx
	// renders the overlay from `shortcutsOpen()`.
	const [shortcutsOpen, setShortcutsOpen] = createSignal(false);
	const hideShortcuts = () => setShortcutsOpen(false);
	const toggleShortcuts = () => setShortcutsOpen((v) => !v);
	// Close-on-navigation (Decision 9): a route change retracts the snapshot-at-
	// open sheet so no modal floats over a new route advertising stale commands.
	const showBridge = () => {
		hideShortcuts();
		navigateTo("/");
	};
	const showBacklog = () => {
		hideShortcuts();
		navigateTo("/backlog");
	};
	const showDone = () => {
		hideShortcuts();
		navigateTo("/done");
	};
	const showSettings = () => {
		hideShortcuts();
		navigateTo("/settings");
	};
	// The keyboard spine (RIG-2456): created here, after `showBridge` exists, so
	// `view.bridge` is registered next to its behavior. App.tsx installs the one
	// window keymap listener over its accessors. `view.shortcuts` (RIG-2482)
	// rides the same seam via `toggleShortcuts`.
	const keyboard = createKeyboardSpine({ showBridge, toggleShortcuts });

	const setTrackerConfig = (cfg: TrackerConfig) => {
		setTrackerConfigSignal(cfg);
		// Rebuild the seam against the new config (the queryFn reads it at fetch
		// time). No manual reload: the handle is part of the query key, so a
		// changed handle re-keys and refetches automatically (§A3).
		seam = createFixtureTrackerSeam(cfg);
	};

	const toggleLeft = () => setLeftOpen((v) => !v);
	const toggleRight = () => setRightOpen((v) => !v);

	const isAgentCollapsed = (agentId: string) => collapsed().has(agentId);
	const toggleAgent = (agentId: string) =>
		setCollapsed((prev) => {
			const next = new Set(prev);
			next.has(agentId) ? next.delete(agentId) : next.add(agentId);
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

	// ── Pins (Record A §T2/T3; unreachable-pin amendment SEA-1645) ──
	const isPinned = (accountId: string) =>
		pinnedAgents().some((p) => p.id === accountId);
	// Append-on-pin, order-preserving; a re-pin is a no-op (no reorder — OQ1).
	// The handle is cached at pin time (SEA-1645 P0) via the resolution seam,
	// falling back to the id if somehow unresolvable at pin time. Persistence is
	// synchronous (write-through) so a pin survives a page reload with no
	// dependence on effect scheduling (§T3).
	const pinAgent = (accountId: string) =>
		setPinnedAgents((prev) => {
			if (prev.some((p) => p.id === accountId)) return prev;
			const handle = agentById(accountId)?.account.handle ?? accountId;
			const next = [...prev, { id: accountId, handle }];
			savePinnedAgents(workspaceKey, next);
			return next;
		});
	// Unpinning drops the pin, persists, and falls the active tab back to the
	// static `status` pane if it was this agent's tab — a deliberate user gesture
	// (§T3; retained by SEA-1645, the only removal path).
	const unpinAgent = (accountId: string) => {
		setPinnedAgents((prev) => {
			const next = prev.filter((p) => p.id !== accountId);
			savePinnedAgents(workspaceKey, next);
			return next;
		});
		if (activeRightTab() === `agent:${accountId}`) setActiveRightTab("status");
	};
	// The derivation (SEA-1645 P1): the fleet group is EVERY pin, in pin order —
	// a pin that resolves to a visible agent via the P5 seam builds a live
	// `fleetItemForAgent`, an unresolvable one builds a marked `unreachableFleetItem`
	// (cached-handle label). Then the static `status` item; the issue group is the
	// static issue items. Nothing is filtered — an unreachable pin keeps its item.
	const rightTabGroups = createMemo<
		readonly { group: RightTabGroup; items: readonly ActivityBarItem[] }[]
	>(() => {
		const fleetItems: ActivityBarItem[] = [];
		for (const pin of pinnedAgents()) {
			const agent = agentById(pin.id);
			fleetItems.push(
				agent ? fleetItemForAgent(agent) : unreachableFleetItem(pin),
			);
		}
		fleetItems.push(RIGHT_SIDEBAR_TAB_BY_ID.status);
		return [
			{ group: "fleet", items: fleetItems },
			{ group: "issue", items: RIGHT_SIDEBAR_ISSUE_ITEMS },
		];
	});
	// Switch the current branch within the active repo by selecting the issue
	// that owns it — so the dropdown, the detail panes (Files/VCS/PR), and the
	// board selection all move together (each branch is one issue's branch).
	// A no-op unless the branch belongs to an issue of the selected agent.
	const setActiveBranch = (branch: string) => {
		const id = selectedAgentId();
		if (!id) return;
		const ws = issues().find((w) => w.assignee === id && w.branch === branch);
		if (ws) setSelectedIssueId(ws.id);
	};

	return {
		view,
		bindRouter,
		keyboard,
		showBridge,
		showBacklog,
		showDone,
		showSettings,
		shortcutsOpen,
		hideShortcuts,
		toggleShortcuts,
		selectedAgentId,
		selectedIssueId,
		selectedAgent,
		agentView,
		selectedIssue,
		openAgent,
		openChannel,
		selectIssue,
		leftOpen,
		toggleLeft,
		rightOpen,
		toggleRight,
		isAgentCollapsed,
		toggleAgent,
		activeRightTab,
		setActiveRightTab,
		pinnedAgentIds,
		pinnedAgents,
		pinAgent,
		unpinAgent,
		isPinned,
		agentById,
		rightTabGroups,
		agentRepos,
		activeRepoId,
		activeRepo,
		setActiveRepo,
		setActiveBranch,
		caller,
		daemon,
		accounts,
		agents,
		firstSnapshotArrived,
		channelGroups,
		channels,
		messages,
		topics,
		selectedChannelId,
		selectedChannel,
		selectedTopicId,
		selectedTopic,
		openTopic,
		workspaceChannel,
		joinChannel,
		toggleSubscribe,
		answerAsk,
		submitAsk,
		isAskSubmitted,
		askError,
		postMessage,
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
		issues,
		assignedIssues,
		trackerConfig,
		setTrackerConfig,
	};
}
