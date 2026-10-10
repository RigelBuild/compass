// The Compass ADE UI's central state store: one store owns all cross-component
// state (view, selection, open panes, sidebar collapse). Components read it via the
// AppStore context and never hold copies, so selection stays coherent across shell.

// The comms surface reads LIVE: `createAppStore` takes an optional CommsClient and
// runs `runCommsStream`, mirroring each reduced CommsState into the accessors. A
// store built WITHOUT a client is offline: starts from `initialComms`, writes reject.

import {
	AgentSessionState,
	type CommsClient,
	type CompassClient,
	CompassService,
	type GetModelRegistryResponse,
	TourOutcome,
	type Transport,
} from "@compass/client";
import type { QueryClient } from "@tanstack/solid-query";
import { useQuery } from "@tanstack/solid-query";
import {
	type Accessor,
	createEffect,
	createMemo,
	createSignal,
	getOwner,
	mapArray,
	onCleanup,
	onSettled,
	untrack,
} from "solid-js";
import type { Pane } from "./agent-tabs";
import type { Analytics } from "./analytics/analytics";
import { type PrRow, prRows } from "./board";
import { agentDmAccountId, firstChannelId } from "./comms";
import {
	type Account,
	type Ask,
	type Channel,
	type ChannelGroup,
	type ConvBlock,
	isQuestionAnswered,
	type Message,
	type Topic,
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
import type { FocusZone } from "./keyboard/zones";
import { adaptMessage } from "./live/adapt";
import { probeServer } from "./live/client";
import { type CommsState, EMPTY_COMMS_STATE } from "./live/comms-state";
import { type AccountSession, runEventStream } from "./live/events";
import { createConnectQuery } from "./live/query";
import {
	appendSessionEvent,
	isTerminalSessionState,
	runSessionTail,
	type SessionFrameUpdate,
} from "./live/session-tail";
import { runCommsStream } from "./live/stream";
import {
	applyReduceMotion,
	loadReduceMotion,
	type ReduceMotion,
	saveReduceMotion,
} from "./preferences";
import { joinAgents } from "./roster";
import type { AgentSession, SessionEvent } from "./session-events";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import {
	type Agent,
	type DaemonInfo,
	type Issue,
	type RuntimeMarker,
	STUB_AGENTS,
	STUB_DAEMON,
	STUB_ISSUES,
	type TrackerConfig,
} from "./stub-data";
import { captureTourEvent } from "./tour/analytics";
import {
	DEMO_ACCOUNTS,
	DEMO_AGENTS,
	DEMO_CHANNELS,
	DEMO_ISSUES,
	DEMO_MESSAGES,
	DEMO_PRESENCE,
	DEMO_TOPICS,
	isDemoId,
} from "./tour/demo";
import { TOUR_STEPS, type TourStep } from "./tour/state";
import { createFixtureTrackerSeam, DEFAULT_TRACKER_CONFIG } from "./tracker";
import { focusViewPanel, viewPanelId, viewTabId } from "./view-panel";
import {
	parseRoute,
	SETTINGS_SECTIONS,
	type SettingsSection,
} from "./view-route";
import { createViewScope, type ViewScope } from "./view-scope";
import {
	focusedViewOf,
	focusView,
	type LayoutAction,
	layoutViews,
	loadLayout,
	MAX_TABS,
	reduceLayout,
	saveLayout,
	setViewPath,
	shownViewIds,
	singleTabLayout,
	type WindowLayout,
} from "./window-layout";

/** The caller — whose visibility scopes every listing and whose membership the
 *  rail reflects. The daemon derives this from the authenticated connection
 *  (comms.proto: "the caller is the account authenticated on the connection");
 *  the fixture pins it to the human owner. */
export const CALLER_ID = "acc-matt";

/** How long the tab-cap notice stays up before it dismisses itself. */
export const NOTICE_TIMEOUT_MS = 5000;

/** A transient layout notice; `count` makes a repeated refusal a new value. */
export interface LayoutNotice {
	text: string;
	count: number;
}

/** The top-level surface the shell routes between. `bridge`/`agents`/`backlog`/
 *  `done`/`settings` are the board-family surfaces (the default is `bridge`),
 *  still primary, reachable from the top bar; they swap the whole UI. `channel`
 *  is the channel's topic index; `topic` is one topic's messages + composer;
 *  `agent` is the per-agent workspace — the agent's channel plus its tab/split
 *  panes. */
export type View =
	| "channel"
	| "topic"
	| "agent"
	| "bridge"
	| "agents"
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
 *  (RIG-1645). The cached handle is the degraded label an unreachable pin renders
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

/** A candidate in a stable name's chain; order is its fallback order. */
export interface ModelRegistryCandidate {
	readonly provider: string;
	readonly modelId: string;
}

/** A display row for one fleet stable model name. */
export interface ModelRegistryRow {
	readonly stableName: string;
	readonly displayName: string;
	readonly candidates: readonly ModelRegistryCandidate[];
}

/** Loading and result states for the read-only fleet model registry. */
export type ModelRegistryState =
	| { readonly status: "offline" }
	| { readonly status: "pending" }
	| { readonly status: "error"; readonly message: string }
	| {
			readonly status: "ready";
			readonly version: bigint;
			readonly entries: readonly ModelRegistryRow[];
	  };

/** Convert the RPC map into stable-name-sorted rows, retaining candidate order. */
export function modelRegistryRows(
	entries: NonNullable<GetModelRegistryResponse["registry"]>["entries"],
): readonly ModelRegistryRow[] {
	return Object.entries(entries)
		.sort(([a], [b]) => a.localeCompare(b))
		.map(([stableName, entry]) => ({
			stableName,
			displayName: entry.displayName,
			candidates: entry.candidates.map(({ provider, modelId }) => ({
				provider,
				modelId,
			})),
		}));
}

/** The three per-account tour RPCs the store drives. The live `CompassClient`
 *  satisfies it; the fixture boot and tests pass an in-memory double. */
export interface TourClient {
	getTourState(
		req: Record<string, never>,
	): Promise<{ outcome: TourOutcome; stepId: string }>;
	claimTourStart(req: { stepId: string }): Promise<{ claimed: boolean }>;
	setTourState(req: { outcome: TourOutcome; stepId: string }): Promise<unknown>;
}

/**
 * The UI state contract every component reads. Accessors are reactive getters
 * (call them in JSX to subscribe); the remaining members are actions that mutate
 * state. Named here — the module that owns the store — so consumers import this
 * type rather than coupling to `ReturnType<typeof createAppStore>`.
 */
export interface AppStore {
	/** Per-agent most recent focused-view open time, persisted per workspace. */
	lastOpened: Accessor<ReadonlyMap<string, number>>;
	// ── View routing ──
	/** The view the window chrome reads routed selection through: the window
	 *  layout's focused view. The router mirrors its path (record A2). */
	focusedView: Accessor<ViewScope>;
	/** The window's tabs and splits; restored from `sessionStorage` at boot. */
	layout: Accessor<WindowLayout>;
	/** Apply a layout action; an eleventh tab is refused with a notice. */
	dispatchLayout: (action: LayoutAction) => void;
	/** Close a tab; focus on its tab button moves to the new active tab's. */
	closeTab: (tabId: string) => void;
	/** Focus the tab and pane holding `viewId`, as pointer focus in a pane does. */
	focusViewId: (viewId: string) => void;
	/** The tab-cap refusal notice; `count` grows on each repeat so a screen reader
	 *  re-announces it. Cleared by dismissal or after `NOTICE_TIMEOUT_MS`. */
	layoutNotice: Accessor<LayoutNotice | undefined>;
	dismissLayoutNotice: () => void;
	/** Pause the notice's timeout while the user is focused on or hovering it;
	 *  releasing restarts the full timeout. */
	holdLayoutNotice: (held: boolean) => void;
	/** One scope per view instance in the layout, in tab order; a view keeps its
	 *  scope object for its whole life, so a keyed render keeps it mounted. */
	viewScopes: Accessor<ViewScope[]>;
	/** The focused view's top-level surface. */
	view: Accessor<View>;
	/** Jump to the Bridge board. */
	showBridge: () => void;
	/** Show the agent tree view. */
	showAgents: () => void;
	/** Show the Backlog view (Todo + Backlog tiers, D3). */
	showBacklog: () => void;
	/** Show the Done/archive view (D4). */
	showDone: () => void;
	/** Show Settings in the last section used by this window. */
	showSettings: () => void;
	settingsSection: () => SettingsSection;
	setSettingsSection: (section: SettingsSection) => void;
	settingsPath: () => string;
	/** The per-device reduced-motion override. */
	reduceMotion: () => ReduceMotion;
	/** Apply and persist the per-device reduced-motion override. */
	setReduceMotion: (value: ReduceMotion) => void;
	/** Whether the keyboard-shortcuts overlay is open (RIG-2482). */
	shortcutsOpen: Accessor<boolean>;
	/** Close the keyboard-shortcuts overlay (Escape/backdrop/navigation). */
	hideShortcuts: () => void;
	/** Toggle the keyboard-shortcuts overlay — the `?` / `view.shortcuts` action. */
	toggleShortcuts: () => void;
	/** The first-run tour controller. The overlay reads it; `TOUR_STEPS` is the
	 *  step table `stepIndex` points into. */
	tour: {
		open: Accessor<boolean>;
		stepIndex: Accessor<number>;
		/** True while demo rows are merged into the read accessors. */
		demoActive: Accessor<boolean>;
		/** True only after ClaimTourStart returned claimed = true. */
		shouldAutoStart: Accessor<boolean>;
		start: (trigger: "first-run" | "replay" | "resume") => void;
		next: () => void;
		back: () => void;
		/** The overlay reports the current step on screen. Sends one
		 *  `tour_step_viewed` per step entry, so a step skipped unseen never counts. */
		stepShown: () => void;
		/** Escape: hides, clears demo rows and leaves a `demo:` route, with no
		 *  permanent write; resume stays available. */
		close: () => void;
		/** Skip tour: writes DISMISSED + current step id, closes, clears demo rows,
		 *  and leaves a `demo:` route. */
		dismiss: () => void;
		/** Writes COMPLETED, closes, clears demo rows, leaves a `demo:` route. */
		complete: () => void;
	};
	/** Whether the command palette is open (RIG-2483). */
	paletteOpen: Accessor<boolean>;
	/** The focus zone captured at palette-open time — read by the palette's
	 *  action-mode ranking (scoped-above-global, D3/D5). Null when nothing was
	 *  zone-focused at open, or while the palette is closed. */
	paletteZone: Accessor<FocusZone | null>;
	/** Open the command palette — captures the pre-open focus snapshot
	 *  (`{ zone, element }`) on the false→true transition only (D3). */
	openPalette: () => void;
	/** Close the command palette and restore focus to the captured element (if
	 *  still connected); clears the snapshot. Every close path (Escape, backdrop,
	 *  Mod+K toggle, running a result) funnels here (D3). */
	closePalette: () => void;
	/** Inject the router seam (record A3). Called once from App (inside the
	 *  router tree): supplies the real navigate + the reactive current location,
	 *  restores the window layout against it, and installs the hash sync both
	 *  ways. The store stays router-import-free; before this the actions route
	 *  through an in-memory default so createAppStore needs no router. */
	bindRouter: (r: {
		navigate: (
			path: string,
			options?: { replace?: boolean; state?: unknown },
		) => void;
		currentPath: () => string;
		currentState: () => unknown;
	}) => void;
	/** The app's keyboard spine (RIG-2456): the shared command registry and the
	 *  set of published roving groups. `App.tsx` installs the single window keymap
	 *  listener over its accessors; every surface registers commands / publishes
	 *  its roving group through it (keyboard/spine.ts). */
	readonly keyboard: KeyboardSpine;

	// ── Selection ──
	/** The roster's selected agent: the focused agent view's agent, moved by a
	 *  board pick (`selectIssue`) without leaving the board. */
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
	/** The path `openChannel` navigates to; undefined for an unknown channel. */
	channelPath: (channelId: string) => string | undefined;
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
	//    Record A §T2/T3; unreachable-pin amendment RIG-1645) ──
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
	 *  cached at pin time (RIG-1645) so an unreachable pin renders the name the
	 *  user pinned. Persisted per workspace in `localStorage`. */
	pinnedAgents: Accessor<readonly PinnedAgent[]>;
	/** Pin an agent's conversation to the fleet activity bar (append if new). */
	pinAgent: (accountId: string) => void;
	/** Unpin an agent; if its tab is active, fall back to `status`. */
	unpinAgent: (accountId: string) => void;
	/** Whether an agent id is in the pin set. */
	isPinned: (accountId: string) => boolean;
	/** Resolve an account id to its visible agent, or undefined — the single
	 *  agent-resolution seam (RIG-1645 P5). A REACTIVE read: consumers that call
	 *  it (`rightTabGroups`, and transitively `activeFleetItem`) re-run when the
	 *  agent set changes. Resolves through the reactive `agents` memo (offline
	 *  `STUB_AGENTS`, live `joinAgents`), so a
	 *  presence/account tick flips its answer. */
	agentById: (accountId: string) => Agent | undefined;
	/** The activity bar as ordered groups (unreachable-pin amendment RIG-1645):
	 *  the fleet group is EVERY pin (one item per pin, in pin order) plus the
	 *  static `status` item; a pin that resolves to no visible agent contributes
	 *  an item marked `unreachable`. The issue group is the static issue items. */
	rightTabGroups: Accessor<
		readonly { group: RightTabGroup; items: readonly ActivityBarItem[] }[]
	>;
	/** Repo clones present in the selected agent's container, for the repo/branch
	 *  dropdown. Empty when no agent is selected. */
	agentRepos: Accessor<RepoClone[]>;
	/** The repo clone picked in the focused view's workspace, or null. */
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
	/** Base URL of the connection shown on General settings. */
	serverUrl: () => string | undefined;

	// ── Comms: the channel surface (design: architecture-lineage) ──
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
	/** The focused view's channel, else the last one visited, else the first
	 *  subscribed (the boot default); null before any channel is known. */
	selectedChannelId: Accessor<string | null>;
	/** The resolved selected channel, or undefined. */
	selectedChannel: Accessor<Channel | undefined>;
	/** The focused topic view's topic id, or null off a topic route. */
	selectedTopicId: Accessor<string | null>;
	/** The resolved selected topic, or undefined. */
	selectedTopic: Accessor<Topic | undefined>;
	/** Drill into a topic's message view — navigate to
	 *  `/channel/<channelId>/topic/<topicId>`. Without `channelId`, resolves the
	 *  topic's channel off the topic set; a no-op on an unknown topic id. */
	openTopic: (topicId: string, channelId?: string) => void;
	/** The path `openTopic` navigates to; undefined for an unknown topic. */
	topicPath: (topicId: string) => string | undefined;
	/** NOT WIRED YET — inert. The wire has no join RPC; the rail's join control
	 *  renders disabled. Kept as the seam the control binds to (and where the
	 *  RPC lands), but it fakes NO membership: a local-only join silently
	 *  reverted on the next SubscribeComms snapshot, which re-derives membership
	 *  from the server. */
	joinChannel: (channelId: string) => void;
	/** NOT WIRED YET — inert, for the same reason as `joinChannel`. The rail's
	 *  subscribe toggle renders disabled. */
	toggleSubscribe: (channelId: string) => void;
	/** Record an answer to a question within an ask, LOCALLY — recording never
	 *  sends. The server accepts exactly ONE `RespondToAsk` per ask (the
	 *  answer-once guard in `applyAskAnswer`, `go/internal/store/messages.go`,
	 *  rejects a later one), so answers accumulate on the local ask copy and
	 *  only `submitAsk` ever ships them. Single-select is first-responder-wins
	 *  (a later answer is a local no-op); multi-select toggles. No-op for an
	 *  unknown message/ask/question/option, an ask already submitted, and a
	 *  CLOSED (`answered`) ask. */
	answerAsk: (
		messageId: string,
		askId: string,
		questionId: string,
		optionId: string,
	) => void;
	/** Record the free-text answer to a question, LOCALLY — like every
	 *  recorder, it never sends; re-typing replaces the draft until the
	 *  explicit submit. No-op on a single-select question already settled by a
	 *  chosen option (exclusivity), an unknown message/ask/question, a
	 *  submitted ask, and a CLOSED (`answered`) ask. */
	answerAskText: (
		messageId: string,
		askId: string,
		questionId: string,
		text: string,
	) => void;
	/** The ONE send path. Issues the ask's single `RespondToAsk` with the
	 *  answers recorded so far and an empty `chosenOptionIds` for every skipped
	 *  question (the wire requires coverage of each question, not an answer to
	 *  each). No-op on an unknown ask, an ask already submitted, a CLOSED
	 *  (`answered`) ask, and a wholly unanswered one. */
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

	// ── Agent sessions + terminal panes ──
	/** An agent's live session: the typed AgentSession (its ordered
	 *  SessionEvent stream + running flag), or undefined when it has none.
	 *  Window-wide data; each view looks up its own agent's session. */
	agentSessionById: (agentId: string) => AgentSession | undefined;
	/** Mint a fresh placeholder terminal pane for an agent — a brand-new pane
	 *  with a globally-unique id (monotonic counter), used to keep "new tab" and
	 *  "split" always available once the agent's fixture terminals are all placed.
	 *  Its `terminalId` intentionally matches no fixture (the pane starts empty
	 *  until the daemon attaches a real terminal). */
	newTerminalPane: (agent: Agent) => Pane;
	/** Stop the focused view's agent (the workspace's stop control). Steering
	 *  happens in the channel, not here — this is the one non-observational
	 *  control. Issues StopAgentSession for the focused view's session, a
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

	// ── Unified left-sidebar section ──
	/** Whether the sidebar's single section is collapsed. */
	isSectionCollapsed: (section: "channels") => boolean;
	toggleSection: (section: "channels") => void;

	// ── Issues (reactive board data) ──
	/** All issues — the reactive source every board surface reads, so a
	 *  streamed lifecycle update is visible everywhere at once (design "read
	 *  through the store accessors"). */
	issues: Accessor<Issue[]>;
	/** Every open-PR row across all issues, paired with its owning issue: a
	 *  `createMemo` over `prRows(issues())`. Search derives PR rows from its own
	 *  hits, so this is the board-wide PR collection, not a search source. */
	prs: Accessor<PrRow[]>;

	// ── Backlog view (D3) ──
	/** The current user's tracker-assigned issues (their personal queue), read
	 *  through the TrackerSeam for the Backlog view. */
	assignedIssues: Accessor<Issue[]>;

	// ── Tracker config (T11) ──
	/** The fixture queue's fixed tracker wiring. */
	trackerConfig: Accessor<TrackerConfig>;
	/** Read-only fleet model registry state. */
	modelRegistry: Accessor<ModelRegistryState>;
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
	/** The server base URL used by the live connection, when available. */
	readonly serverUrl?: string;
	/** The shared transport used to key and call the store's connect queries; absent means offline. */
	readonly transport?: Transport;
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
	 *  prefs in `localStorage` (pins, last-opened agent times). index.tsx derives
	 *  it from the live `Connection` so one deployment's ids never hydrate on
	 *  another. Absent (offline / tests) → falls back to `callerId`. */
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
	/** Overrides the sessions, keyed by account id. Absent: live when `compass` is set,
	 *  else the fixture, whose `fixture: true` entries `stopAgent` refuses to send. */
	readonly sessions?: Record<string, AgentSession>;
	/** Observes a comms failure — a stream error the driver retries past, a
	 *  rejected `RespondToAsk`, a refused `StopAgentSession`, or a failed boot
	 *  `GetServerInfo` probe (the banner stays on the stub, offline).
	 *  `postMessage` rejects to its caller instead (the composer must keep the
	 *  user's text) and does NOT route here. */
	readonly onCommsError?: (error: unknown) => void;
	/** Where the window layout persists (record A8): the boot passes
	 *  `sessionStorage`. Absent, the layout lives only as long as the store. */
	readonly layoutStorage?: Storage;
	/** The per-account tour state RPCs. Present → the store reads the stored
	 *  state at construction for resume; absent → the tour never auto-arms and
	 *  step writes are skipped. */
	readonly tour?: TourClient;
	/** Claim the first run at boot (needs `tour`). Only a boot whose app reacts
	 *  to `shouldAutoStart` may set it: the claim writes STARTED server-side. */
	readonly claimFirstRun?: boolean;
	/** The product-analytics embed the tour reports through. Absent → no events,
	 *  the same as the embed's own flag-off no-op. */
	readonly analytics?: Analytics;
}

/** One live session's tailed trace and the last lifecycle state the tail saw.
 *  `notFound` parks the tail until the next status for the session re-arms it. */
interface SessionTrace {
	readonly events: SessionEvent[];
	readonly state?: AgentSessionState;
	readonly notFound?: true;
}

/** A session runs until either stream reports a terminal state; terminal is
 *  absorbing, so whichever stream saw it first wins. No state at all is not running. */
function isRunning(
	status: AgentSessionState,
	tail: AgentSessionState | undefined,
): boolean {
	if (isTerminalSessionState(status)) return false;
	if (tail !== undefined && isTerminalSessionState(tail)) return false;
	return status !== AgentSessionState.UNSPECIFIED || tail !== undefined;
}

/** The live session source: each account's current session from the status
 *  stream, plus each session's tailed trace, kept for the store's lifetime. */
interface LiveSessions {
	accountSessions: Accessor<ReadonlyMap<string, AccountSession>>;
	setAccountSessions: (sessions: ReadonlyMap<string, AccountSession>) => void;
	sessionFor: (agentId: string) => AgentSession | undefined;
}

function createLiveSessions(
	client: CompassClient,
	deps: {
		shownAgentIds: Accessor<readonly string[]>;
		onError: (error: unknown) => void;
	},
): LiveSessions {
	const [accountSessions, setAccountSessions] = createSignal<
		ReadonlyMap<string, AccountSession>
	>(new Map());
	const [traces, setTraces] = createSignal<ReadonlyMap<string, SessionTrace>>(
		new Map(),
	);
	const updateTrace = (
		sessionId: string,
		update: (trace: SessionTrace) => SessionTrace,
	): void => {
		setTraces((prev) => {
			const next = new Map(prev);
			next.set(sessionId, update(prev.get(sessionId) ?? { events: [] }));
			return next;
		});
	};
	const appendFrame = (sessionId: string, frame: SessionFrameUpdate): void => {
		updateTrace(sessionId, (trace) => ({
			events: frame.event
				? appendSessionEvent(trace.events, frame.event)
				: trace.events,
			state: frame.state ?? trace.state,
		}));
	};
	// Bumped on every changed status per session; a NotFound only parks the tail
	// if no status arrived while it was in flight. Not reactive: only read on settle.
	const statusCounts = new Map<string, number>();
	// The last status per session id. Unlike the account map, a resync never
	// clears it, so a reuse straddling a resync still sees the terminal state.
	const lastStates = new Map<string, AgentSessionState>();
	// The last `sessionFor` result per session id and the inputs it was built from.
	const cached = new Map<
		string,
		{
			trace: SessionTrace | undefined;
			state: AgentSessionState;
			session: AgentSession;
		}
	>();
	// Drop each session whose account now maps to another id. A resync (an empty or
	// partial map) prunes nothing: a reused id can straddle one.
	const pruneMovedSessions = (
		next: ReadonlyMap<string, AccountSession>,
	): void => {
		const prev = untrack(accountSessions);
		const nextIds = new Set([...next.values()].map((s) => s.sessionId));
		const dropped = new Set<string>();
		for (const account of next.keys()) {
			const moved = prev.get(account)?.sessionId;
			if (moved !== undefined && !nextIds.has(moved)) dropped.add(moved);
		}
		for (const sessionId of dropped) {
			statusCounts.delete(sessionId);
			lastStates.delete(sessionId);
			cached.delete(sessionId);
		}
		if (![...dropped].some((id) => untrack(traces).has(id))) return;
		setTraces((prev) => {
			const kept = new Map(prev);
			for (const sessionId of dropped) kept.delete(sessionId);
			return kept;
		});
	};
	// A changed status lifts a NotFound park. Only a terminal-then-live status pair
	// re-arms an ended trace: that is a reused id (reload, wake, resume), not a late one.
	const adoptAccountSessions = (
		next: ReadonlyMap<string, AccountSession>,
	): void => {
		pruneMovedSessions(next);
		for (const status of next.values()) {
			const old = lastStates.get(status.sessionId);
			if (old === status.state) continue;
			lastStates.set(status.sessionId, status.state);
			statusCounts.set(
				status.sessionId,
				(statusCounts.get(status.sessionId) ?? 0) + 1,
			);
			if (!untrack(traces).has(status.sessionId)) continue;
			const reused =
				old !== undefined &&
				isTerminalSessionState(old) &&
				!isTerminalSessionState(status.state);
			updateTrace(status.sessionId, (trace) => ({
				events: trace.events,
				state: reused ? undefined : trace.state,
			}));
		}
		setAccountSessions(next);
	};
	// Tail until the session ends or aborts. A NotFound is retried only when a status
	// landed meanwhile (the start race); otherwise the session parks until the next one.
	const tailSession = async (
		sessionId: string,
		signal: AbortSignal,
	): Promise<void> => {
		for (;;) {
			const seen = statusCounts.get(sessionId) ?? 0;
			const end = await runSessionTail({
				client,
				sessionId,
				signal,
				onError: deps.onError,
				onFrame: (frame) => appendFrame(sessionId, frame),
			});
			if (end !== "notFound") return;
			if ((statusCounts.get(sessionId) ?? 0) === seen) {
				updateTrace(sessionId, (trace) => ({ ...trace, notFound: true }));
				return;
			}
		}
	};
	// Tail only the agents on screen: each tail holds a browser connection, and an
	// HTTP/1.1 door allows about six per host. A new session id replaces the old.
	const shownSessionIds = createMemo<readonly string[]>(() => {
		const ids = new Set<string>();
		for (const id of deps.shownAgentIds()) {
			const status = accountSessions().get(id);
			if (!status) continue;
			const trace = traces().get(status.sessionId);
			const ended =
				isTerminalSessionState(status.state) ||
				(trace?.state !== undefined && isTerminalSessionState(trace.state));
			if (!ended && !trace?.notFound) ids.add(status.sessionId);
		}
		return [...ids];
	});
	// One tail per shown session, diffed so a pane change leaves the others open.
	const tails = new Map<string, AbortController>();
	createEffect(shownSessionIds, (sessionIds) => {
		for (const [sessionId, abort] of tails) {
			if (sessionIds.includes(sessionId)) continue;
			abort.abort();
			tails.delete(sessionId);
		}
		for (const sessionId of sessionIds) {
			if (tails.has(sessionId)) continue;
			const abort = new AbortController();
			tails.set(sessionId, abort);
			void tailSession(sessionId, abort.signal).catch((error) => {
				if (!abort.signal.aborted) deps.onError(error);
			});
		}
	});
	if (getOwner())
		onCleanup(() => {
			for (const abort of tails.values()) abort.abort();
		});
	return {
		accountSessions,
		setAccountSessions: adoptAccountSessions,
		sessionFor: (agentId) => {
			const status = accountSessions().get(agentId);
			if (!status) return undefined;
			const trace = traces().get(status.sessionId);
			// Reuse the last object while its inputs hold, so a frame on another
			// session does not hand every view a new session object.
			const hit = cached.get(status.sessionId);
			if (
				hit !== undefined &&
				hit.trace === trace &&
				hit.state === status.state &&
				hit.session.agentAccountId === agentId
			)
				return hit.session;
			const session: AgentSession = {
				sessionId: status.sessionId,
				agentAccountId: agentId,
				running: isRunning(status.state, trace?.state),
				events: trace?.events ?? [],
			};
			cached.set(status.sessionId, { trace, state: status.state, session });
			return session;
		},
	};
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
 *  Self-healing per-element hydration (RIG-1645, no version flag): a bare
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

function loadLastOpened(workspace: string): ReadonlyMap<string, number> {
	const store = safeLocalStorage();
	if (!store) return new Map();
	try {
		const raw = store.getItem(`compass.lastOpened.${workspace}`);
		if (!raw) return new Map();
		const parsed: unknown = JSON.parse(raw);
		if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed))
			return new Map();
		const times = new Map<string, number>();
		for (const [accountId, timestamp] of Object.entries(parsed)) {
			if (typeof timestamp === "number" && Number.isFinite(timestamp))
				times.set(accountId, timestamp);
		}
		return times;
	} catch {
		return new Map();
	}
}

function saveLastOpened(
	workspace: string,
	lastOpened: ReadonlyMap<string, number>,
): void {
	const store = safeLocalStorage();
	if (!store) return;
	try {
		store.setItem(
			`compass.lastOpened.${workspace}`,
			JSON.stringify(Object.fromEntries(lastOpened)),
		);
	} catch {
		// Best-effort: a quota / privacy-locked write failure is non-fatal.
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
	const [reduceMotion, setReduceMotionValue] = createSignal<ReduceMotion>(
		loadReduceMotion(safeLocalStorage()),
	);
	const setReduceMotion = (value: ReduceMotion): void => {
		setReduceMotionValue(value);
		saveReduceMotion(safeLocalStorage(), value);
		if (typeof document !== "undefined") {
			applyReduceMotion(document.documentElement, value);
		}
	};
	const callerId = options.callerId ?? CALLER_ID;
	// The issue list is reactive so promote/archive (below) are visible on
	// every surface at once. Seeded from the fixture (or an explicit override);
	// the real @compass/client stream replaces the seed later. Reads go through
	// the merged `issues` memo below, never this raw signal.
	const [realIssues, setRealIssues] = createSignal<Issue[]>([
		...(options.initialIssues ?? STUB_ISSUES),
	]);
	// While the tour is open the base read accessors append the demo rows, so
	// every derived read sees them and no write path stores them.
	const [demoActive, setDemoActive] = createSignal(false);
	const issues = createMemo<Issue[]>(() =>
		demoActive() ? [...realIssues(), ...DEMO_ISSUES] : realIssues(),
	);

	// A board pick's agent (`selectIssue`). On an agent route the focused view's
	// agent wins, so the roster and chrome never disagree with the agent surface.
	const [pickedAgentId, setPickedAgentId] = createSignal<string | null>(null);
	// Default to the first issue so the seam survives swapping the fixture
	// for the real @compass/client (no hardcoded stub id); an empty board
	// starts with no selection.
	const [selectedIssueId, setSelectedIssueId] = createSignal<string | null>(
		(options.initialIssues ?? STUB_ISSUES)[0]?.id ?? null,
	);

	// ── Window layout and router seam (record A2) ─────────────────────────────
	// The layout owns every view's path; the URL hash mirrors the focused view.
	// ownedWrite: bindRouter restores the layout while App renders.
	const [layout, setLayout] = createSignal<WindowLayout>(singleTabLayout("/"), {
		ownedWrite: true,
	});
	// A refusal is shown to the user rather than silently dropped.
	const [layoutNotice, setLayoutNotice] = createSignal<
		LayoutNotice | undefined
	>(undefined, { ownedWrite: true });
	let cancelNoticeTimer = (): void => {};
	let noticeHeld = false;
	let noticeUp = false;
	const dismissLayoutNotice = (): void => {
		cancelNoticeTimer();
		noticeUp = false;
		noticeHeld = false;
		setLayoutNotice(undefined);
	};
	const startNoticeTimer = (): void => {
		cancelNoticeTimer();
		if (noticeHeld || !noticeUp) return;
		// biome-ignore lint/style/noRestrictedGlobals: a real UI dismiss delay, not a test wait.
		const timer = setTimeout(dismissLayoutNotice, NOTICE_TIMEOUT_MS);
		cancelNoticeTimer = () => clearTimeout(timer);
	};
	const holdLayoutNotice = (held: boolean): void => {
		noticeHeld = held;
		startNoticeTimer();
	};
	if (getOwner()) onCleanup(() => cancelNoticeTimer());
	// Closing removes the focused tab button, so focus follows to the new active one.
	const closeTab = (tabId: string): void => {
		const item = document.getElementById(viewTabId(tabId))?.parentElement;
		const focusWasInTab = item?.contains(document.activeElement) ?? false;
		dispatchLayout({ kind: "close", tabId });
		if (!focusWasInTab) return;
		onSettled(() => {
			document.getElementById(viewTabId(layout().activeTabId))?.focus();
		});
	};
	// Focus in the other pane follows through App's effect, which keeps each
	// pane's last target; focus outside both panes moves here.
	const focusPane = (pane: "first" | "second"): void => {
		const { tabs, activeTabId } = layout();
		const split = tabs.find((tab) => tab.id === activeTabId)?.layout;
		dispatchLayout({ kind: "focusPane", pane });
		if (split?.kind !== "split") return;
		const inPanel = (viewId: string): boolean =>
			document
				.getElementById(viewPanelId(viewId))
				?.contains(document.activeElement) ?? false;
		const target = pane === "first" ? split.first : split.second;
		const other = pane === "first" ? split.second : split.first;
		if (inPanel(target.id)) return;
		if (inPanel(other.id) && split.focused !== pane) return;
		focusViewPanel(target.id);
	};
	const dispatchLayout = (action: LayoutAction): void => {
		let refused = false;
		setLayout((prev) => {
			const next = reduceLayout(prev, action);
			refused = "refused" in next;
			return "refused" in next ? prev : next;
		});
		if (!refused) return;
		setLayoutNotice((prev) => ({
			text: `Tab limit reached: close a tab to open another (${MAX_TABS} tabs max).`,
			count: (prev?.count ?? 0) + 1,
		}));
		noticeUp = true;
		startNoticeTimer();
	};
	let routerBound = false;
	const navigateTo = (path: string): void => {
		// Before App binds, the layout moves but bindRouter then restores over it,
		// so a dev build warns about the lost navigation.
		if (!routerBound && import.meta.env?.DEV) {
			// biome-ignore lint/suspicious/noConsole: DEV-only pre-bindRouter navigation warning
			console.warn(
				`compass: navigate("${path}") before bindRouter — the URL will not ` +
					"update until App wires the router",
			);
		}
		anchorAgentEntry(path);
		dispatchLayout({ kind: "navigateFocused", path });
	};
	// Leaving a dead demo path or a non-canonical one replaces its history entry,
	// so Back cannot loop onto it.
	let replaceNextHashSync = false;
	const isDemoPath = (path: string): boolean => path.split("/").some(isDemoId);
	const leaveDemoPaths = (): void => {
		if (isDemoPath(focusedViewOf(untrack(layout)).path)) {
			replaceNextHashSync = true;
		}
		setLayout((prev) =>
			layoutViews(prev)
				.filter((item) => isDemoPath(item.path))
				.reduce((next, item) => setViewPath(next, item.id, "/"), prev),
		);
	};
	const bindRouter = (r: {
		navigate: (
			path: string,
			options?: { replace?: boolean; state?: unknown },
		) => void;
		currentPath: () => string;
		currentState: () => unknown;
	}): void => {
		routerBound = true;
		const storage = options.layoutStorage;
		setLayout(loadLayout(storage, untrack(r.currentPath)));
		createEffect(layout, (next) => {
			saveLayout(storage, next);
		});
		// Hash → layout: the entry's path goes to the view its state names, else
		// (no id, or a closed view) to the focused view.
		createEffect(
			() => {
				const state = r.currentState();
				const viewId =
					typeof state === "object" && state !== null && "viewId" in state
						? state.viewId
						: undefined;
				return {
					path: r.currentPath(),
					viewId: typeof viewId === "string" ? viewId : undefined,
				};
			},
			({ path, viewId }) => {
				setLayout((prev) => {
					const known =
						viewId !== undefined &&
						layoutViews(prev).some((view) => view.id === viewId);
					const target = known ? viewId : focusedViewOf(prev).id;
					return setViewPath(focusView(prev, target), target, path);
				});
			},
		);
		// Layout → hash: a move within one view pushes; a focus change (and the
		// boot entry, stamped with its view) replaces, so tab switches never stack.
		// Deferred: the router's first navigate flushes, which is a no-op in here.
		createEffect(
			() => focusedViewOf(layout()),
			(current, prev) => {
				queueMicrotask(() => {
					// A later layout change queued its own sync; this one is stale.
					if (untrack(() => focusedViewOf(layout())) !== current) return;
					const state = untrack(r.currentState);
					const stamped =
						typeof state === "object" &&
						state !== null &&
						"viewId" in state &&
						state.viewId === current.id;
					const samePath = untrack(r.currentPath) === current.path;
					const forceReplace = replaceNextHashSync;
					replaceNextHashSync = false;
					if (samePath && stamped) return;
					const push =
						!samePath &&
						prev !== undefined &&
						prev.id === current.id &&
						!forceReplace;
					r.navigate(current.path, {
						replace: !push,
						state: { viewId: current.id },
					});
				});
			},
		);
	};

	// The read-only fleet model registry. Cached data wins over a failed refetch so a
	// transient outage never blanks a loaded registry; memoized so rows map once per fetch.
	const modelRegistryQuery = options.transport
		? createConnectQuery(CompassService.method.getModelRegistry, () => ({}), {
				transport: options.transport,
				queryClient: options.queryClient,
			})
		: undefined;
	const modelRegistry: Accessor<ModelRegistryState> = modelRegistryQuery
		? createMemo((): ModelRegistryState => {
				const data = modelRegistryQuery.data;
				if (data)
					return {
						status: "ready",
						version: data.version,
						entries: modelRegistryRows(data.registry?.entries ?? {}),
					};
				if (modelRegistryQuery.isError)
					return { status: "error", message: modelRegistryQuery.error.message };
				return { status: "pending" };
			})
		: () => ({ status: "offline" });

	// The fixture queue uses the fixed tracker config through its seam.
	const seam = createFixtureTrackerSeam(DEFAULT_TRACKER_CONFIG);
	const issuesQuery = useQuery(
		() => ({
			queryKey: ["assignedIssues", DEFAULT_TRACKER_CONFIG.handle] as const,
			queryFn: (): Promise<Issue[]> =>
				seam.listAssignedIssues(DEFAULT_TRACKER_CONFIG.handle),
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
	// The unified sidebar section uses a namespaced key, separate from tree ids.
	const [sectionCollapsed, setSectionCollapsed] = createSignal<
		ReadonlySet<string>
	>(new Set());

	// The bottom log panel (D2): open by default; reset open on workspace entry.
	const [logOpen, setLogOpen] = createSignal(true);

	// ── Right sidebar (T6; dock-in-sidebar D1/D6; Record A §T2/T3/T5;
	//    unreachable-pin amendment RIG-1645): active tab + pin set + repo/branch ──

	// The pinned agent set: ordered, append-on-pin, persisted per workspace. Held as
	// `{ id, handle }` pairs (RIG-1645 P0); a pin resolving to no visible agent is
	// RETAINED and still emits a marked item. Falls back to `callerId` when no identity.
	const workspaceKey = options.workspaceKey ?? callerId;
	const [lastOpened, setLastOpened] = createSignal<ReadonlyMap<string, number>>(
		loadLastOpened(workspaceKey),
	);
	const [pinnedAgents, setPinnedAgents] = createSignal<readonly PinnedAgent[]>(
		loadPinnedAgents(workspaceKey),
	);
	const pinnedAgentIds = createMemo<readonly string[]>(() =>
		pinnedAgents().map((p) => p.id),
	);
	// The single agent-resolution seam (RIG-1645 P5): resolve an account id to its
	// visible agent. A REACTIVE closure over the `agents` memo, so every consumer
	// re-runs when the agent set changes. `agents` is the live join memo below, so a
	// presence/account tick flips resolution live→reachable through this one seam.
	const agentById = (accountId: string): Agent | undefined =>
		agents().find((a) => a.account.id === accountId);
	// ── Comms: the channel surface (design: architecture-lineage) ──
	// ONE reduced CommsState drives all four comms accessors, starting at `initialComms`
	// (EMPTY by default) and replaced wholesale by each push — bar the local ask picks
	// `preserveLocalAsks` carries across. Accessors are memos, so a message event is absorbed.
	const [comms, setComms] = createSignal<CommsState>(
		options.initialComms ?? EMPTY_COMMS_STATE,
	);
	// CommsState's collections are `readonly` (a pure value the reducer rebuilds each
	// transition) and the accessors keep that: every write goes through setComms with a
	// fresh array. The `readonly` signature is what lets the compiler hold that true.
	// Each merged accessor reads its own raw-slice memo, so a push that leaves a
	// slice's identity alone (structural sharing) leaves the merged view alone too.
	const withDemo = <T>(
		slice: Accessor<readonly T[]>,
		demo: readonly T[],
	): Accessor<readonly T[]> =>
		createMemo(() => (demoActive() ? [...slice(), ...demo] : slice()));
	const accounts = withDemo(
		createMemo(() => comms().accounts),
		DEMO_ACCOUNTS,
	);
	const channelGroups = createMemo(() => comms().channelGroups);
	const channels = withDemo(
		createMemo(() => comms().channels),
		DEMO_CHANNELS,
	);
	const messages = withDemo(
		createMemo(() => comms().messages),
		DEMO_MESSAGES,
	);
	const topics = withDemo(
		createMemo(() => comms().topics),
		DEMO_TOPICS,
	);
	// The board's fleet (§T3). Intermediate presence memo: re-notifies only when the
	// presence map's identity changes — each posted message replaces the whole
	// CommsState, and `===` equality plus structural sharing absorb that. The join
	// then gates on live/offline: offline the fixture, live re-joins on account/presence.
	const livePresence = createMemo(() => comms().presence);
	const presence = createMemo(() =>
		demoActive()
			? new Map([...livePresence(), ...DEMO_PRESENCE])
			: livePresence(),
	);
	// Runtime markers arrive on the session-status stream, not the comms one, so
	// they are their own signal joined in beside presence.
	const [runtimeMarkers, setRuntimeMarkers] = createSignal<
		ReadonlyMap<string, RuntimeMarker>
	>(new Map());
	// Live sessions unless the caller overrides them or the store is offline. The
	// shown agents read the layout, not the view scopes, which are built later.
	const live =
		options.compass && !options.sessions
			? createLiveSessions(options.compass, {
					shownAgentIds: () => {
						const shown = shownViewIds(layout());
						return layoutViews(layout()).flatMap((instance) => {
							if (!shown.includes(instance.id)) return [];
							const match = parseRoute(instance.path);
							return match.view === "agent" ? [match.agentId] : [];
						});
					},
					onError: (error) => options.onCommsError?.(error),
				})
			: undefined;
	// Live, the demo accounts and presence already ride the merged inputs; the
	// offline fixture roster has no join, so the demo agents append directly.
	const agents: Accessor<readonly Agent[]> = options.comms
		? createMemo(() =>
				joinAgents(
					accounts(),
					presence(),
					runtimeMarkers(),
					live?.accountSessions() ?? new Map(),
					lastOpened(),
				),
			)
		: withDemo(() => STUB_AGENTS, DEMO_AGENTS);
	// Boot default (Record A §T5): the first hydrated pin resolving to a visible
	// agent, else the static `status` pane (RIG-1645 P4, OQ-1 ruled kept). An
	// unresolvable leading pin is skipped but still shows its marked bar item.
	const firstResolvablePin = pinnedAgentIds().find(
		(id) => agentById(id) !== undefined,
	);
	const [activeRightTab, setActiveRightTabRaw] = createSignal<RightSidebarTab>(
		firstResolvablePin ? `agent:${firstResolvablePin}` : "status",
	);
	// The single public set seam (RIG-1645 P3): a plain pass-through. The old
	// resolvability guard is retired — an unresolvable `agent:` tab is now valid and
	// renders the unreachable pane. Unpin-active→status still routes here; a
	// visibility change never coerces the tab.
	const setActiveRightTab = (tab: RightSidebarTab) => {
		setActiveRightTabRaw(tab);
	};
	// True once the first comms snapshot has arrived from the stream. A view's
	// pending-aware route fallback reads it: before the first snapshot an absent
	// channel id is merely not-yet-loaded (held, not bounced); after it, an absent
	// id is genuinely unknown (redirected).
	const [firstSnapshotArrived, setFirstSnapshotArrived] = createSignal(false);
	// Adopt a stream-pushed state WHOLESALE except in-progress local ask picks
	// (never sent to server) — see `preserveLocalAsks`. A routed channel that the
	// push dropped is re-pointed by the view itself.
	const adoptComms = (next: CommsState) => {
		setComms((prev) => preserveLocalAsks(prev, next));
		setFirstSnapshotArrived(true);
	};
	// The live read path: run the SubscribeComms driver for the store's lifetime,
	// mirroring each reduced state into the signals above. Aborted on teardown.
	// `runCommsStream` resolves only on abort and retries internally, so nothing awaits
	// it — a rejection is a driver bug, surfaced rather than swallowed.
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

	// The daemon banner reads LIVE: a one-shot GetServerInfo probe at boot flips the
	// banner to the server's liveness/version. Offline (no `options.compass`) keeps
	// STUB_DAEMON. api_version-mismatch is deliberately NOT handled (parked). One-shot
	// async, so a `disposed` flag guards a late probe; a rejection routes through onCommsError.
	const [daemon, setDaemon] = createSignal<DaemonInfo>(STUB_DAEMON);
	if (options.compass) {
		const client = options.compass;
		let disposed = false;
		if (getOwner()) onCleanup(() => (disposed = true));
		// The live board read path: run the SubscribeEvents driver for the store's
		// lifetime, replacing STUB_ISSUES with the server's snapshot-as-events then live
		// upserts. Aborted on teardown. Reuses `options.compass`. `runEventStream`
		// resolves only on abort and retries internally; a rejection routes through onCommsError.
		const eventsAbort = new AbortController();
		if (getOwner()) onCleanup(() => eventsAbort.abort());
		void runEventStream({
			client,
			onIssues: setRealIssues,
			onRuntime: setRuntimeMarkers,
			onSessions: live?.setAccountSessions,
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
						rev: info.rev,
						live: true,
					});
			})
			.catch((error) => {
				if (!disposed) options.onCommsError?.(error);
			});
	}

	// The per-post idempotency key source: `clientRequestId` must be caller-unique, so
	// a per-store random prefix plus a monotonic counter gives a fresh key per post.
	// Not reactive — it only sources fresh ids on demand.
	const requestIdPrefix = `ui-${Date.now().toString(36)}-${Math.random()
		.toString(36)
		.slice(2, 10)}`;
	let requestCount = 0;

	// Monotonic counter for MINTED placeholder terminal panes (never reused, so ids
	// stay globally unique across opens/closes and across views). Not reactive: a
	// plain counter. The daemon will assign real terminal ids at this same seam later.
	let mintedTerminalCount = 0;

	// Each agent's session trace, or undefined when none. Offline it reads the override
	// or fixture (fixture ids never reach the wire; see `stopAgent`), live the tail.
	const fixtureSessions = options.sessions ?? STUB_SESSION_EVENTS;
	const agentSessionById = (agentId: string): AgentSession | undefined =>
		live ? live.sessionFor(agentId) : fixtureSessions[agentId];

	// ── View scopes (record A1): one per view instance in the layout, keyed by
	// view id, so a view keeps its workspace state while its path moves. ──
	const shownIds = createMemo(() => shownViewIds(layout()));
	const scopes = mapArray(
		() => layoutViews(layout()),
		(instance) => {
			const id = untrack(instance).id;
			return createViewScope(
				{ channels, topics, agentById, agentSessionById, firstSnapshotArrived },
				id,
				untrack(instance).path,
				{
					path: () => instance().path,
					navigate: (path, navOptions) => {
						if (navOptions?.replace && focusedViewOf(untrack(layout)).id === id)
							replaceNextHashSync = true;
						setLayout((prev) => setViewPath(prev, id, path));
					},
					shown: () => shownIds().includes(id),
				},
			);
		},
		{ keyed: (instance) => instance.id },
	);
	const focusedView = createMemo<ViewScope>(() => {
		const id = focusedViewOf(layout()).id;
		const scope = scopes().find((item) => item.id === id);
		if (!scope) throw new Error(`no view scope for ${id}`);
		return scope;
	});
	const [lastSettingsSection, setLastSettingsSection] =
		createSignal<SettingsSection>(SETTINGS_SECTIONS[0]);
	const settingsSection = createMemo(() => {
		const route = focusedView().route();
		return route.view === "settings" ? route.section : lastSettingsSection();
	});
	const setSettingsSection = (section: SettingsSection) =>
		setLastSettingsSection(section);
	const settingsPath = createMemo(() => `/settings/${settingsSection()}`);
	const view = createMemo<View>(() => focusedView().route().view);
	// Status events for other agents must not re-record the focused one.
	const focusedAgentTurn = createMemo(
		() => {
			const route = focusedView().route();
			if (route.view !== "agent") return undefined;
			const turnEndedAtUnixMs = live
				?.accountSessions()
				.get(route.agentId)?.turnEndedAtUnixMs;
			return { agentId: route.agentId, turnEndedAtUnixMs };
		},
		{
			equals: (a, b) =>
				a?.agentId === b?.agentId &&
				a?.turnEndedAtUnixMs === b?.turnEndedAtUnixMs,
		},
	);
	createEffect(focusedAgentTurn, (focused) => {
		if (!focused) return;
		setLastOpened((previous) => {
			const timestamp = Math.max(Date.now(), focused.turnEndedAtUnixMs ?? 0);
			if ((previous.get(focused.agentId) ?? 0) >= timestamp) return previous;
			const next = new Map(previous);
			next.set(focused.agentId, timestamp);
			saveLastOpened(workspaceKey, next);
			return next;
		});
	});
	// Last-visited: an agent or board route keeps the channel the user left, and a
	// channel the push dropped falls back to the first subscribed one.
	const selectedChannelId = createMemo<string | null>((prev) => {
		const match = focusedView().route();
		if (match.view === "channel" || match.view === "topic") {
			return match.channelId;
		}
		const known = channels();
		return prev && known.some((c) => c.id === prev)
			? prev
			: firstChannelId(known);
	});
	const selectedTopicId = createMemo<string | null>(() => {
		const match = focusedView().route();
		return match.view === "topic" ? match.topicId : null;
	});
	const selectedChannel = createMemo(() =>
		channels().find((c) => c.id === selectedChannelId()),
	);
	const selectedTopic = createMemo(() =>
		topics().find((t) => t.id === selectedTopicId()),
	);
	// The agent the log panel was last reset open for, so re-entering the same
	// agent keeps the user's minimize.
	let logResetForAgentId: string | null = null;
	// Entering an agent anchors the window-wide issue selection on every entry,
	// even onto the same path: keep the current issue when this agent owns it,
	// else its primary.
	const anchorAgentEntry = (path: string): void => {
		const match = parseRoute(path);
		if (match.view !== "agent") return;
		const owned = issues().filter((w) => w.assignee === match.agentId);
		setPickedAgentId(match.agentId);
		setSelectedIssueId(
			owned.find((w) => w.id === selectedIssueId())?.id ?? owned[0]?.id ?? null,
		);
		if (match.agentId !== logResetForAgentId) {
			setLogOpen(true);
			logResetForAgentId = match.agentId;
		}
	};
	// A focused view that becomes an agent view by any path (back, a deep link, a
	// tab switch) re-anchors too, as an explicit navigation does.
	createEffect(
		() => focusedView().path(),
		(path) => untrack(() => anchorAgentEntry(path)),
	);
	const selectedAgentId = createMemo<string | null>(() => {
		const match = focusedView().route();
		return match.view === "agent" ? match.agentId : pickedAgentId();
	});
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
	// The selected agent's repo clones (T6). The fixture models one clone per agent (the
	// monorepo) with branches from that agent's assigned issues. Returns an array so a
	// multi-clone daemon is a fixture change, not a shape change. `currentBranch` derives
	// from the selected issue, so dropdown, panes, and board selection can't drift apart.
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
		const picked = repos.find((r) => r.id === focusedView().activeRepoId());
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
	// Demo rows exist only while the tour runs; a reload, deep-link or Back onto a
	// demo path would strand an empty surface, so that view lands on the Bridge.
	createEffect(
		() =>
			!demoActive() &&
			layoutViews(layout()).some((item) => isDemoPath(item.path)),
		(stale) => {
			if (stale) leaveDemoPaths();
		},
	);

	// Open an agent's workspace by navigating, so the click and a `/agent/:agentId`
	// deep-link share one home (applyFocusedPath).
	const openAgent = (agentId: string) => {
		navigateTo(`/agent/${agentId}`);
	};

	// Open a channel: route to its topic index with it selected — unless it's a 1:1 agent
	// DM, whose surface is the agent workspace, so route to the agent (one entry point,
	// no dead-end DM view). Unknown id is a no-op.
	const channelPath = (channelId: string): string | undefined => {
		const chan = channels().find((c) => c.id === channelId);
		if (!chan) return undefined;
		const byId = new Map(accounts().map((a) => [a.id, a]));
		const agentId = agentDmAccountId(chan, callerId, byId);
		return agentId ? `/agent/${agentId}` : `/channel/${channelId}`;
	};
	const openChannel = (channelId: string) => {
		const path = channelPath(channelId);
		if (path) navigateTo(path);
	};

	// Drill into a topic's message view by navigating to `/channel/<id>/topic/<id>`,
	// so click and deep-link share one home. A caller holding the channel id (a search
	// hit) passes it; otherwise resolve off the topic set, a no-op on an unknown id.
	const topicPath = (topicId: string): string | undefined => {
		const topic = topics().find((t) => t.id === topicId);
		return topic ? `/channel/${topic.channelId}/topic/${topicId}` : undefined;
	};
	const openTopic = (topicId: string, channelId?: string) => {
		const path = channelId
			? `/channel/${channelId}/topic/${topicId}`
			: topicPath(topicId);
		if (path) navigateTo(path);
	};

	// Selecting an issue (a board card or a swimlane cell) syncs the roster
	// to its assignee but stays on the board — it does not jump into the agent
	// view, so the board stays the working surface while you scan cards.
	const selectIssue = (issueId: string) => {
		setSelectedIssueId(issueId);
		const ws = issues().find((w) => w.id === issueId);
		setPickedAgentId(ws?.assignee ?? null);
	};

	// ── Comms mutations (design: architecture-lineage) ──

	// Join / subscribe are NOT WIRED (Matt's ruling): no such RPC yet, and the old
	// local-only mutation lied against the live stream (`adoptComms` replaces state
	// wholesale, `deriveMembership` re-derives), so it reverted mid-use. Rendered disabled;
	// they keep their shape as the bind seam for when the slice lands. `channelId` unused.
	const joinChannel = (_channelId: string) => {};
	const toggleSubscribe = (_channelId: string) => {};
	// Apply one answer to a question, or return the SAME question object when rejected
	// (unknown option, or a settled single-select). The identical reference is what lets
	// answerAsk tell "recorded" from "no-op" — and a no-op must send nothing on the wire.
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
		// A single-select pick settles the question, so it clears any staged draft:
		// the server rejects an option plus custom text on a non-multi question.
		return q.allowMultiple
			? { ...q, chosenOptionIds: chosen }
			: { ...q, chosenOptionIds: chosen, customText: "" };
	};
	// Record free text, or return the SAME question when it changes nothing: a
	// single-select already settled by a pick (exclusivity), or an unchanged draft.
	const answerQuestionText = (
		q: Ask["questions"][number],
		text: string,
	): Ask["questions"][number] => {
		if (!q.allowMultiple && q.chosenOptionIds.length > 0) return q;
		if (text === q.customText) return q;
		return { ...q, customText: text };
	};
	// Whether an ask has had its ONE RespondToAsk issued. Reactive so the render can
	// lock a submitted ask, and the guard against issuing a second respond (server
	// accepts exactly one; a later one is ErrConflict). Marked only on a real respond,
	// so an offline store (no `comms`) never marks anything.
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
	// refused respond is not user-invisible. Cleared on the next respond for that ask.
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
	// Whether two asks pose the same questions, in order, with the same arity and
	// option ids — the axes whose staleness the server REJECTS. Labels are ignored
	// on purpose: guarding them would discard a half-typed answer on a benign
	// reword, at the cost of a relabelled-in-place option re-pointing a pick.
	const sameQuestions = (a: Ask, b: Ask) =>
		a.questions.length === b.questions.length &&
		a.questions.every((q, i) => {
			const other = b.questions[i];
			return (
				other !== undefined &&
				q.questionId === other.questionId &&
				q.allowMultiple === other.allowMultiple &&
				q.options.length === other.options.length &&
				q.options.every((o, j) => o.id === other.options[j]?.id)
			);
		});
	// An ask the server has said nothing about. The server's `answered` flag is the
	// authority — flips once, on the first ACCEPTED respond. A question scan for empty
	// chosenOptionIds cannot substitute: a CLOSED ask leaves them empty on a skip or a
	// custom_text-only answer, so a scan would restore stale picks over it → ErrConflict.
	const serverHasNoAnswer = (ask: Ask) => !ask.answered;
	// Carry in-progress LOCAL ask picks across a stream push. The wire is atomic (one
	// RespondToAsk on the completing click), so every pick but the last lives ONLY here.
	// Kept only where the server has no value (not submitted, pushed ask not `answered`,
	// questions line up, real unshipped pick), preserving "a server value is AUTHORITATIVE".
	function preserveLocalAsks(prev: CommsState, next: CommsState): CommsState {
		// Unsubmitted asks carrying a local pick, by message id then ask id. Empty on
		// nearly every push (then pushed state is adopted untouched). Scans the local
		// record's chosen ids on purpose — "is there an unshipped edit worth carrying";
		// the authority question (has the server closed it) is asked on the PUSHED ask.
		const local = new Map<string, Map<string, Ask>>();
		for (const msg of prev.messages) {
			for (const b of msg.blocks) {
				if (b.kind !== "ask") continue;
				if (isAskSubmitted(b.ask.askId)) continue;
				if (
					b.ask.questions.every(
						(q) => q.chosenOptionIds.length === 0 && q.customText === "",
					)
				)
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
	// Replace an ask in place — records a local answer, and restages the shipped
	// answers over a blank push after a refused respond.
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
	// RespondToAsk is atomic: one accepted respond per ask, every question covered
	// (empty chosenOptionIds = skipped). Only `submitAsk` calls this. On REFUSED,
	// clear the submitted mark so it retries and restage the shipped answers if a
	// blank push replaced them meanwhile. RIG-1310: SDK correlation unwired.
	const sendAsk = (messageId: string, ask: Ask) => {
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
					// A single-select holding an option ships no text: the server rejects
					// the pair. Trim here, at the audit seam — the staged draft stays raw.
					customText:
						!q.allowMultiple && q.chosenOptionIds.length > 0
							? ""
							: q.customText.trim(),
				})),
			})
			.catch((error) => {
				unmarkAskSubmitted(ask.askId);
				const current = findAsk(messageId, ask.askId);
				// A refusal must not cost staged work: a blank push adopted while the
				// respond was in flight took the local answers, and the shipped ask still
				// holds them. Declines when the ask CLOSED meanwhile (the
				// accepted-then-lost-reply race) or its shape moved.
				if (
					current &&
					current !== ask &&
					!current.answered &&
					sameQuestions(current, ask)
				) {
					putAsk(messageId, ask);
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
	// Apply a question reducer to one ask, LOCALLY. Both recorders share this: a
	// gate that lived in only one of them would be a hole in the other.
	const recordAnswer = (
		messageId: string,
		askId: string,
		questionId: string,
		reduce: (q: Ask["questions"][number]) => Ask["questions"][number],
	) => {
		if (isAskSubmitted(askId)) return;
		// The ask AFTER the local edit; stays undefined when the coordinates miss
		// or the answer is rejected.
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
						// The other way an ask is settled, invisible to the submitted mark:
						// the server burns an ask on the first ACCEPTED respond and refuses
						// later ones with ErrConflict, so recording here could only ship a
						// doomed RPC — whoever closed it.
						if (ask.answered) return b;
						const questions = ask.questions.map((q) =>
							q.questionId === questionId ? reduce(q) : q,
						);
						// Reference-identical questions ⇒ the answer was rejected (or the
						// questionId named no question): leave the block untouched.
						if (questions.every((q, i) => q === ask.questions[i])) return b;
						answered = { ...ask, questions };
						return { kind: "ask", ask: answered };
					}),
				};
			}),
		}));
		// The user acted on this ask again: whatever the last refusal said is no
		// longer what the block should be showing.
		if (answered) clearAskError(askId);
		// Recording is LOCAL, always: the server takes exactly one respond per
		// ask, forever, and only the explicit submit (`submitAsk`) ever sends one.
	};
	const answerAsk = (
		messageId: string,
		askId: string,
		questionId: string,
		optionId: string,
	) => {
		if (isDemoId(messageId) || isDemoId(askId)) return;
		recordAnswer(messageId, askId, questionId, (q) =>
			answerQuestion(q, optionId),
		);
	};
	const answerAskText = (
		messageId: string,
		askId: string,
		questionId: string,
		text: string,
	) => {
		if (isDemoId(messageId) || isDemoId(askId)) return;
		recordAnswer(messageId, askId, questionId, (q) =>
			answerQuestionText(q, text),
		);
	};
	// The ONE send path. Answers accumulate locally — clicks and typed text
	// alike — and this explicit gesture ships them atomically, with an empty
	// answer for each skipped question. Inert on a submitted, CLOSED, unknown,
	// or wholly unanswered ask.
	const submitAsk = (messageId: string, askId: string) => {
		if (isDemoId(messageId) || isDemoId(askId)) return;
		if (isAskSubmitted(askId)) return;
		const ask = findAsk(messageId, askId);
		if (!ask) return;
		// Closed server-side: the ask's one accepted respond has been taken, and a second
		// is refused with ErrConflict. The submitted mark does not cover this — the ask
		// can arrive closed on a push, or be closed by another participant.
		if (ask.answered) return;
		// "Nothing staged" is a question about the LOCAL record: a wholly blank respond
		// says nothing, so it stays inert. Typed text counts as an answer here too.
		if (!ask.questions.some(isQuestionAnswered)) return;
		// Nothing was recorded by this call, so a refusal leaves the local record
		// as the user staged it — still honest, unsent, retryable.
		sendAsk(messageId, ask);
	};
	// The one write path: PostMessage with the channel `container`, `topic` oneof, one
	// text block and a fresh clientRequestId (server dedups a retried key). NOTHING is
	// inserted locally — SubscribeComms echoes it and upsertMessage dedups by id, so it
	// renders once. Rejects rather than swallowing so the composer keeps typed text.
	const postMessage = async (
		channelId: string,
		topic:
			| { case: "topicId"; value: string }
			| { case: "topicName"; value: string },
		text: string,
	): Promise<void> => {
		// Rejects like the offline path, so the composer keeps the typed text and
		// says why instead of clearing it with nothing sent.
		if (
			isDemoId(channelId) ||
			(topic.case === "topicId" && isDemoId(topic.value))
		) {
			throw new Error("cannot post: this is a demo channel from the tour");
		}
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

	// Mint a fresh placeholder terminal pane. The counter only increments, so the id
	// (`term-<agentId>-<n>`) is unique across the session — no collision with a view's
	// openTab dedupe or splitPaneOnce's guard. `terminalId` mirrors the id and matches no fixture:
	// the pane renders an empty "starting" state until the daemon attaches one.
	const newTerminalPane = (agent: Agent): Pane => {
		const n = ++mintedTerminalCount;
		const id = `term-${agent.account.id}-${n}`;
		return { id, kind: "terminal", title: `Terminal ${n}`, terminalId: id };
	};
	// The last refused Stop, or undefined when the last attempt was not refused — the
	// reactive hole the log panel RENDERS, the shape `askError` gives the ask block.
	// Without it a refused Stop is observably identical to a successful one. Cleared at
	// the start of the next attempt.
	const [stopError, setStopError] = createSignal<string | undefined>(undefined);
	// Record a refusal AND keep routing it to the shell funnel — additive.
	const refuseStop = (error: unknown) => {
		setStopError(error instanceof Error ? error.message : String(error));
		options.onCommsError?.(error);
	};
	// The observation pane's stop control (the one non-observational control; steering
	// happens in the channel). StopAgentSession's whole request is the server-minted
	// `session_id`, so this stops the OBSERVED session. CompassClient-backed, NOT comms.

	// Never rejects, never swallows. The RPC is Runner-backed: a server with no
	// RunnerHub answers `Unavailable`, a REAL condition on the socket-only path. No
	// user text to preserve, so it resolves and routes the failure to `onCommsError`.
	// Stop is idempotent server-side, so a retry after a refusal is safe.
	const stopAgent = async (): Promise<void> => {
		setStopError(undefined);
		// A demo agent has no server session to stop.
		if (isDemoId(selectedAgentId())) return;
		const session = focusedView().agentSession();
		if (!session) return;
		// A fixture-sourced session's id was never minted by a server. Issuing Stop for it
		// is worse than nothing: the server's unknown-session path is idempotent-success,
		// so the RPC returns OK, stops nothing, and never reaches onCommsError — inert in
		// the one way indistinguishable from working. Refuse locally and say why.
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
	const showAgents = () => {
		hideShortcuts();
		navigateTo("/agents");
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
		navigateTo(settingsPath());
	};
	// ── First-run tour (A4/A5): open state, step cursor, and server writes ──
	const [tourOpen, setTourOpen] = createSignal(false);
	const [tourStepIndex, setTourStepIndex] = createSignal(0);
	const [shouldAutoStart, setShouldAutoStart] = createSignal(false);
	// The cursor the user last reached this session, and the one the boot read
	// fetched; the session's wins on resume.
	let resumeStepId = "";
	let savedStepId = "";
	// The stored outcome as last read or written; replay never resumes a
	// completed tour.
	let storedOutcome = TourOutcome.UNSPECIFIED;
	// A resume asked for before the boot read lands waits for it, so it opens
	// at the saved step instead of writing welcome over it.
	let bootReadPending = options.tour !== undefined;
	let pendingResume = false;
	let pendingReplay = false;
	// Set by any start, so a claim that lands after a manual start arms nothing.
	let tourStarted = false;
	// One chain: the server upserts in arrival order, so a slow STARTED must
	// never land after a later DISMISSED or COMPLETED. A failed write is
	// reported once and never retried.
	let tourWrites: Promise<unknown> = Promise.resolve();
	const writeTourState = (outcome: TourOutcome, stepId: string) => {
		storedOutcome = outcome;
		const client = options.tour;
		if (!client) return;
		tourWrites = tourWrites
			.then(() => client.setTourState({ outcome, stepId }))
			.catch((error: unknown) => options.onCommsError?.(error));
	};
	// The step index last reported viewed; showStep clears it so Back re-reports.
	let viewedIndex: number | undefined;
	const showStep = (index: number, persist: boolean) => {
		const step: TourStep | undefined = TOUR_STEPS[index];
		if (!step) return;
		setTourStepIndex(index);
		resumeStepId = step.id;
		viewedIndex = undefined;
		if (step.route === "/") showBridge();
		else if (step.route === "/backlog") showBacklog();
		else if (step.route === "/done") showDone();
		else if (step.route === "/settings") showSettings();
		if (persist) writeTourState(TourOutcome.STARTED, step.id);
	};
	// Every exit drops the demo rows. A route still naming a demo id is replaced,
	// not pushed, so Back cannot return to the dead demo URL.
	const endTour = () => {
		setTourOpen(false);
		setDemoActive(false);
		if (isDemoPath(focusedView().path())) hideShortcuts();
		leaveDemoPaths();
	};
	const tour: AppStore["tour"] = {
		open: tourOpen,
		stepIndex: tourStepIndex,
		demoActive,
		shouldAutoStart,
		start: (trigger) => {
			// Only a won claim arms the first run; the claim already wrote STARTED.
			if (trigger === "first-run" && !shouldAutoStart()) return;
			pendingResume = trigger === "resume" && bootReadPending;
			if (pendingResume) return;
			setShouldAutoStart(false);
			tourStarted = true;
			const resumed = TOUR_STEPS.findIndex(
				(s) => s.id === (resumeStepId || savedStepId),
			);
			setTourOpen(true);
			setDemoActive(true);
			captureTourEvent(options.analytics, { name: "tour_started", trigger });
			showStep(
				trigger === "resume" ? Math.max(resumed, 0) : 0,
				trigger !== "first-run",
			);
		},
		next: () => {
			if (!tourOpen()) return;
			if (tourStepIndex() >= TOUR_STEPS.length - 1) tour.complete();
			else showStep(tourStepIndex() + 1, true);
		},
		back: () => {
			if (tourOpen() && tourStepIndex() > 0) {
				showStep(tourStepIndex() - 1, true);
			}
		},
		stepShown: () => {
			const index = tourStepIndex();
			const step = TOUR_STEPS[index];
			if (!tourOpen() || !step || viewedIndex === index) return;
			viewedIndex = index;
			captureTourEvent(options.analytics, {
				name: "tour_step_viewed",
				step_id: step.id,
				index,
			});
		},
		close: () => {
			if (tourOpen()) endTour();
		},
		dismiss: () => {
			if (!tourOpen()) return;
			writeTourState(TourOutcome.DISMISSED, resumeStepId);
			captureTourEvent(options.analytics, {
				name: "tour_dismissed",
				step_id: resumeStepId,
			});
			endTour();
		},
		complete: () => {
			if (!tourOpen()) return;
			writeTourState(TourOutcome.COMPLETED, resumeStepId);
			captureTourEvent(options.analytics, { name: "tour_completed" });
			endTour();
		},
	};
	// The `tour.start` command: resume where a closed or skipped tour stopped,
	// else replay from the welcome. It decides only once the boot read lands.
	const replayTour = () => {
		if (tourOpen()) return;
		pendingReplay = bootReadPending;
		if (pendingReplay) return;
		const cursor = resumeStepId || savedStepId;
		const resumable = cursor !== "" && storedOutcome !== TourOutcome.COMPLETED;
		tour.start(resumable ? "resume" : "replay");
	};
	// Boot: the stored state feeds resume; a first run arms solely on a won
	// claim, so a failed read, a failed claim, or a lost race arms nothing.
	if (options.tour) {
		const client = options.tour;
		const claimFirstRun = options.claimFirstRun ?? false;
		let disposed = false;
		if (getOwner()) onCleanup(() => (disposed = true));
		const readSettled = () => {
			bootReadPending = false;
			if (pendingResume) tour.start("resume");
			if (pendingReplay) replayTour();
		};
		void client
			.getTourState({})
			.then(async (state) => {
				if (disposed) return;
				savedStepId = state.stepId;
				if (!tourStarted) storedOutcome = state.outcome;
				readSettled();
				if (!claimFirstRun || state.outcome !== TourOutcome.UNSPECIFIED) return;
				const { claimed } = await client.claimTourStart({
					stepId: TOUR_STEPS[0]?.id ?? "",
				});
				if (claimed && !disposed && !tourStarted) setShouldAutoStart(true);
			})
			.catch((error: unknown) => {
				if (disposed) return;
				// A failed read still releases a waiting resume, at step 0.
				if (bootReadPending) readSettled();
				options.onCommsError?.(error);
			});
	}
	// Command palette (RIG-2483): the open signal plus the D3 pre-open snapshot
	// `{ zone, element }`. `openPalette` captures the snapshot ONLY on the false→true
	// transition — re-entering with focus already in the palette input would clobber the
	// real pre-open zone. `closePalette` restores focus and clears; only the zone is exposed.
	const [paletteOpen, setPaletteOpen] = createSignal(false);
	const [paletteZone, setPaletteZone] = createSignal<FocusZone | null>(null);
	let paletteElement: HTMLElement | null = null;
	const openPalette = () => {
		if (paletteOpen()) return; // already open — never re-capture the snapshot
		setPaletteZone(keyboard.activeZone());
		const active = document.activeElement;
		paletteElement = active instanceof HTMLElement ? active : null;
		setPaletteOpen(true);
	};
	const closePalette = () => {
		const active = document.activeElement;
		const shouldRestore =
			active instanceof HTMLElement &&
			(active.closest(".cx-palette") !== null || active === document.body);
		setPaletteOpen(false);
		const el = paletteElement;
		paletteElement = null;
		setPaletteZone(null);
		if (shouldRestore && el?.isConnected) el.focus();
	};
	const togglePalette = () => {
		if (paletteOpen()) closePalette();
		else openPalette();
	};
	const toggleLeft = () => setLeftOpen((v) => !v);
	const toggleRight = () => setRightOpen((v) => !v);
	// The keyboard spine (RIG-2456): created here, after the `show*`/toggle closures
	// exist, so `view.bridge` + the RIG-2482/2483 seeds register next to their behavior.
	// App.tsx installs the one window keymap listener. `view.shortcuts` rides
	// `toggleShortcuts`; `palette.open` + the `view.*` seeds ride the closures below.
	const keyboard = createKeyboardSpine({
		showBridge,
		toggleShortcuts,
		showAgents,
		showBacklog,
		showDone,
		navigateSettings: (section) => {
			hideShortcuts();
			setSettingsSection(section);
			navigateTo(`/settings/${section}`);
		},
		showSettings,
		togglePalette,
		toggleLeft,
		toggleRight,
		layout,
		dispatchLayout,
		closeTab,
		focusPane,
		startTour: replayTour,
	});

	const isAgentCollapsed = (agentId: string) => collapsed().has(agentId);
	const toggleAgent = (agentId: string) =>
		setCollapsed((prev) => {
			const next = new Set(prev);
			next.has(agentId) ? next.delete(agentId) : next.add(agentId);
			return next;
		});

	// The bottom log panel (D2).
	const toggleLog = () => setLogOpen((v) => !v);

	// Sidebar section collapse uses a namespaced key, separate from tree nodes.
	const isSectionCollapsed = (section: "channels") =>
		sectionCollapsed().has(`section:${section}`);
	const toggleSection = (section: "channels") =>
		setSectionCollapsed((prev) => {
			const key = `section:${section}`;
			const next = new Set(prev);
			next.has(key) ? next.delete(key) : next.add(key);
			return next;
		});

	// ── Right sidebar actions (T6) ──
	const setActiveRepo = (repoId: string) => {
		if (agentRepos().some((r) => r.id === repoId)) {
			focusedView().setActiveRepoId(repoId);
		}
	};

	// ── Pins (Record A §T2/T3; unreachable-pin amendment RIG-1645) ──
	const isPinned = (accountId: string) =>
		pinnedAgents().some((p) => p.id === accountId);
	// Append-on-pin, order-preserving; a re-pin is a no-op (no reorder — OQ1). The handle
	// is cached at pin time (RIG-1645 P0) via the resolution seam, falling back to the id.
	// Persistence is synchronous (write-through) so a pin survives a reload with no
	// dependence on effect scheduling (§T3).
	const pinAgent = (accountId: string) =>
		setPinnedAgents((prev) => {
			if (isDemoId(accountId)) return prev;
			if (prev.some((p) => p.id === accountId)) return prev;
			const handle = agentById(accountId)?.account.handle ?? accountId;
			const next = [...prev, { id: accountId, handle }];
			savePinnedAgents(workspaceKey, next);
			return next;
		});
	// Unpinning drops the pin, persists, and falls the active tab back to the
	// static `status` pane if it was this agent's tab — a deliberate user gesture
	// (§T3; retained by RIG-1645, the only removal path).
	const unpinAgent = (accountId: string) => {
		if (isDemoId(accountId)) return;
		setPinnedAgents((prev) => {
			const next = prev.filter((p) => p.id !== accountId);
			savePinnedAgents(workspaceKey, next);
			return next;
		});
		if (activeRightTab() === `agent:${accountId}`) setActiveRightTab("status");
	};
	// The derivation (RIG-1645 P1): the fleet group is EVERY pin, in pin order — a pin
	// resolving to a visible agent (P5 seam) builds a live `fleetItemForAgent`, an
	// unresolvable one a marked `unreachableFleetItem`. Then the static `status` item
	// and the issue items. Nothing is filtered — an unreachable pin keeps its item.
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
	// Every open-PR row across all issues, from the same `prRows` helper the PRs
	// tab and search use, so the three cannot disagree on what counts as open.
	const prs = createMemo<PrRow[]>(() => prRows(issues()));

	return {
		view,
		bindRouter,
		keyboard,
		showBridge,
		showAgents,
		showBacklog,
		showDone,
		showSettings,
		settingsSection,
		setSettingsSection,
		settingsPath,
		reduceMotion,
		setReduceMotion,
		shortcutsOpen,
		tour,
		hideShortcuts,
		toggleShortcuts,
		paletteOpen,
		paletteZone,
		openPalette,
		closePalette,
		selectedAgentId,
		selectedIssueId,
		selectedAgent,
		agentView,
		selectedIssue,
		openAgent,
		openChannel,
		channelPath,
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
		activeRepoId: () => focusedView().activeRepoId(),
		serverUrl: () => options.serverUrl,
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
		topicPath,
		focusedView,
		layout,
		dispatchLayout,
		closeTab,
		focusViewId: (viewId) => {
			setLayout((prev) => focusView(prev, viewId));
		},
		layoutNotice,
		dismissLayoutNotice,
		holdLayoutNotice,
		viewScopes: scopes,
		joinChannel,
		lastOpened,
		toggleSubscribe,
		answerAsk,
		answerAskText,
		submitAsk,
		isAskSubmitted,
		askError,
		postMessage,
		agentSessionById,
		newTerminalPane,
		stopAgent,
		stopError,
		logOpen,
		toggleLog,
		isSectionCollapsed,
		toggleSection,
		issues,
		prs,
		assignedIssues,
		trackerConfig: () => DEFAULT_TRACKER_CONFIG,
		modelRegistry,
	};
}
