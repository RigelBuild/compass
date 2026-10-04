import type { Accessor, Context } from "solid-js";
import {
	createContext,
	createEffect,
	createMemo,
	createSignal,
	useContext,
} from "solid-js";
import {
	type AgentTab,
	CHAT_TAB_ID,
	chatTab,
	type Pane,
	prunePane,
	splitPaneIds,
	splitPaneOnce,
} from "./agent-tabs";
import { firstChannelId } from "./comms";
import type { Channel, Topic } from "./comms-stub";
import type { AgentSession } from "./session-events";
import type { AppStore } from "./store";
import type { Agent } from "./stub-data";
import type { RouteMatch } from "./view-route";
import { parseRoute } from "./view-route";

export interface ViewScope {
	id: string;
	path: Accessor<string>;
	route: Accessor<RouteMatch>;
	channel: Accessor<Channel | undefined>;
	topic: Accessor<Topic | undefined>;
	agent: Accessor<Agent | undefined>;
	navigate: (path: string) => void;
	/** The view agent's home DM: the agent workspace's chat pane. */
	workspaceChannel: Accessor<Channel | undefined>;
	/** The view agent's session trace, or undefined off an agent route. */
	agentSession: Accessor<AgentSession | undefined>;
	/** The workspace tabs, chat first; empty until the view's agent is set up. */
	agentTabs: Accessor<AgentTab[]>;
	activeAgentTabId: Accessor<string | null>;
	activeAgentTab: Accessor<AgentTab | undefined>;
	/** Show a tab; no-op for an unknown id. */
	setActiveAgentTab: (tabId: string) => void;
	/** Open `pane` as a full-screen tab and show it; an open pane id just refocuses. */
	openTab: (pane: Pane) => void;
	/** Close a tab, falling back to chat; the chat tab is permanent. */
	closeTab: (tabId: string) => void;
	/** Split the active tab's focused pane, placing and focusing `pane` beside it. */
	splitFocused: (pane: Pane, direction: "row" | "column") => void;
	/** Focus a pane of the active tab; no-op for a pane not in it. */
	setFocusedPane: (paneId: string) => void;
	/** Close a pane of the active tab; its last pane closes the tab. */
	closePane: (paneId: string) => void;
	/** The repo clone picked in this view's workspace, or null. */
	activeRepoId: Accessor<string | null>;
	setActiveRepoId: (repoId: string) => void;
}

export const ViewContext: Context<ViewScope | undefined> = createContext<
	ViewScope | undefined
>(undefined);

export function useView(): ViewScope {
	const view = useContext(ViewContext);
	if (!view)
		throw new Error("useView must be used within a ViewContext provider");
	return view;
}
/** The window-wide data a view resolves its route against. A subset of the
 *  store, so the store can build its own focused view while it is assembled. */
export type ViewScopeStore = Pick<
	AppStore,
	| "channels"
	| "topics"
	| "agentById"
	| "agentSessionById"
	| "firstSnapshotArrived"
>;

export function createViewScope(
	store: ViewScopeStore,
	id: string,
	initialPath: string,
): ViewScope {
	const [path, setPath] = createSignal(initialPath);
	const route = createMemo(() => parseRoute(path()));
	const channel = createMemo(() => {
		const match = route();
		if (match.view !== "channel" && match.view !== "topic") return undefined;
		return store.channels().find((item) => item.id === match.channelId);
	});
	const topic = createMemo(() => {
		const match = route();
		if (match.view !== "topic") return undefined;
		return store.topics().find((item) => item.id === match.topicId);
	});
	const agentId = createMemo(() => {
		const match = route();
		return match.view === "agent" ? match.agentId : null;
	});
	const agent = createMemo(() => {
		const current = agentId();
		return current ? store.agentById(current) : undefined;
	});
	// Keyed on the agent it was set up for: leaving and re-entering the same agent
	// keeps the tabs, a different agent starts fresh (the old applyAgentRoute guard).
	const [workspace, setWorkspace] = createSignal<AgentWorkspace>((prev) => {
		const current = agentId();
		if (prev && (current === null || prev.agentId === current)) return prev;
		return freshWorkspace(current);
	});
	const navigate = (nextPath: string): void => {
		setPath(nextPath);
	};

	const workspaceChannel = createMemo(() => {
		const home = agent()?.account.homeChannelId;
		return home ? store.channels().find((item) => item.id === home) : undefined;
	});
	const agentSession = createMemo<AgentSession | undefined>(() => {
		const current = agentId();
		return current ? store.agentSessionById(current) : undefined;
	});
	const agentTabs = createMemo<AgentTab[]>(() =>
		agentId() ? workspace().tabs : [],
	);
	const activeAgentTabId = createMemo<string | null>(() =>
		agentId() ? workspace().activeTabId : null,
	);
	const activeAgentTab = createMemo<AgentTab | undefined>(() =>
		agentTabs().find((tab) => tab.id === activeAgentTabId()),
	);
	const activeRepoId = (): string | null => workspace().repoId;

	const setActiveAgentTab = (tabId: string): void => {
		if (!agentId()) return;
		setWorkspace((ws) =>
			ws.tabs.some((tab) => tab.id === tabId)
				? { ...ws, activeTabId: tabId }
				: ws,
		);
	};
	const updateActiveTab = (fn: (tab: AgentTab) => AgentTab): void => {
		setWorkspace((ws) =>
			ws.activeTabId
				? {
						...ws,
						tabs: ws.tabs.map((tab) =>
							tab.id === ws.activeTabId ? fn(tab) : tab,
						),
					}
				: ws,
		);
	};
	const openTab = (pane: Pane): void => {
		if (!agentId()) return;
		setWorkspace((ws) => ({
			...ws,
			tabs: ws.tabs.some((tab) => tab.id === pane.id)
				? ws.tabs
				: [
						...ws.tabs,
						{
							id: pane.id,
							title: pane.title,
							layout: { kind: "leaf", pane },
							focusedPaneId: pane.id,
						},
					],
			activeTabId: pane.id,
		}));
	};
	const closeTab = (tabId: string): void => {
		setWorkspace((ws) => withoutTab(ws, tabId));
	};
	const splitFocused = (pane: Pane, direction: "row" | "column"): void => {
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
	const setFocusedPane = (paneId: string): void => {
		updateActiveTab((tab) =>
			splitPaneIds(tab.layout).includes(paneId)
				? { ...tab, focusedPaneId: paneId }
				: tab,
		);
	};
	const closePane = (paneId: string): void => {
		setWorkspace((ws) => {
			const tab = ws.tabs.find((item) => item.id === ws.activeTabId);
			// The chat pane is permanent, whether its tab is a lone leaf or split.
			if (!tab || (tab.id === CHAT_TAB_ID && paneId === CHAT_TAB_ID)) return ws;
			const pruned = prunePane(tab.layout, paneId);
			if (!pruned) return withoutTab(ws, tab.id);
			const remaining = splitPaneIds(pruned);
			return {
				...ws,
				tabs: ws.tabs.map((item) =>
					item.id === tab.id
						? {
								...item,
								layout: pruned,
								focusedPaneId: remaining.includes(item.focusedPaneId)
									? item.focusedPaneId
									: (remaining[0] ?? item.focusedPaneId),
							}
						: item,
				),
			};
		});
	};
	const setActiveRepoId = (repoId: string): void => {
		setWorkspace((ws) => ({ ...ws, repoId }));
	};

	// Pending-aware: an unknown id is held until the first snapshot, then this
	// view (only) falls back, mirroring the store's applyChannelRoute.
	createEffect(
		() => {
			const match = route();
			if (match.view !== "channel" && match.view !== "topic") return null;
			const channels = store.channels();
			return {
				match,
				channelKnown: channels.some((item) => item.id === match.channelId),
				topicKnown:
					match.view === "topic" &&
					store.topics().some((item) => item.id === match.topicId),
				firstSnapshotArrived: store.firstSnapshotArrived(),
				fallbackChannelId: firstChannelId(channels),
			};
		},
		(resolution) => {
			if (!resolution?.firstSnapshotArrived) return;
			if (!resolution.channelKnown) {
				navigate(
					resolution.fallbackChannelId
						? `/channel/${resolution.fallbackChannelId}`
						: "/",
				);
				return;
			}
			if (resolution.match.view === "topic" && !resolution.topicKnown) {
				navigate(`/channel/${resolution.match.channelId}`);
			}
		},
	);

	return {
		id,
		path,
		route,
		channel,
		topic,
		agent,
		navigate,
		workspaceChannel,
		agentSession,
		agentTabs,
		activeAgentTabId,
		activeAgentTab,
		setActiveAgentTab,
		openTab,
		closeTab,
		splitFocused,
		setFocusedPane,
		closePane,
		activeRepoId,
		setActiveRepoId,
	};
}

interface AgentWorkspace {
	agentId: string | null;
	tabs: AgentTab[];
	activeTabId: string | null;
	repoId: string | null;
}

function freshWorkspace(agentId: string | null): AgentWorkspace {
	return agentId
		? {
				agentId,
				tabs: [chatTab()],
				activeTabId: CHAT_TAB_ID,
				repoId: `${agentId}-repo`,
			}
		: { agentId: null, tabs: [], activeTabId: null, repoId: null };
}

// Focus falls back to the chat tab, which itself can never close.
function withoutTab(ws: AgentWorkspace, tabId: string): AgentWorkspace {
	if (tabId === CHAT_TAB_ID) return ws;
	return {
		...ws,
		tabs: ws.tabs.filter((tab) => tab.id !== tabId),
		activeTabId: ws.activeTabId === tabId ? CHAT_TAB_ID : ws.activeTabId,
	};
}
