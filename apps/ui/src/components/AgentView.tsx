import { type Component, For, Match, Show, Switch } from "solid-js";
import { useStore } from "../context";
import {
	type AgentTab,
	CHAT_TAB_ID,
	type Pane,
	type SplitNode,
	splitPanes,
} from "../store";
import type { Agent, Terminal } from "../stub-data";
import { ChannelView } from "./ChannelView";
import { LogPanel } from "./LogPanel";
import { StateDot } from "./StateDot";

/** A terminal pane: the fixture scrollback for the terminal a tab references. A
 *  minted placeholder terminal has no fixture match (its id is fresh), so it
 *  degrades to a muted "starting" state rather than an error — the daemon
 *  attaches the real terminal later. */
const TerminalBody: Component<{ agent: Agent; terminalId?: string }> = (
	props,
) => {
	const term = (): Terminal | undefined =>
		props.agent.terminals.find((t) => t.id === props.terminalId);
	return (
		<Show
			when={term()}
			fallback={<div class="av-leaf-empty muted">Terminal starting…</div>}
		>
			{(t) => <div class="term-body">{t().lines.join("\n")}</div>}
		</Show>
	);
};

/** A file pane: a read-only Markdown viewer placeholder (resolved decision 2 —
 *  Markdown first, read-only; other file types and editing are deferred). */
const FileViewer: Component<{ path?: string }> = (props) => (
	<div class="av-file">
		<div class="av-file-head">
			<span class="av-file-path">{props.path ?? "untitled"}</span>
			<span class="av-file-ro">read-only</span>
		</div>
		<p class="muted av-file-note">
			Read-only Markdown viewer — file contents aren't wired in the mockup yet.
		</p>
	</div>
);

/** One pane in a tab: renders its UI by kind, with a header carrying the
 *  split-right / split-down / close affordances. Clicking the pane focuses it
 *  (the pane the split buttons act on); the focused pane gets an accent ring. */
const PaneView: Component<{ pane: Pane; agent: Agent; focused: boolean }> = (
	props,
) => {
	const store = useStore();
	// The terminal pane the split buttons open: the agent's next unplaced FIXTURE
	// terminal (rich scrollback) while any remain, else a freshly-minted
	// placeholder from the store. Always returns a pane, so the split buttons are
	// always enabled — fixture terminals first, then placeholders.
	const nextOrMintedTerminalPane = (): Pane =>
		nextFreeTerminalPane(props.agent, store.agentTabs()) ??
		store.newTerminalPane(props.agent);
	// Split this tab's focused pane, opening that terminal beside it (row = split
	// right, column = split down); a later context menu will pick the pane kind.
	const splitWith = (direction: "row" | "column") => {
		store.setFocusedPane(props.pane.id);
		store.splitActivePane(nextOrMintedTerminalPane(), direction);
	};
	return (
		<div class="av-pane" classList={{ focused: props.focused }}>
			<div class="av-pane-head">
				<button
					type="button"
					class="av-pane-title"
					title={`Focus ${props.pane.title}`}
					aria-pressed={props.focused}
					onClick={() => store.setFocusedPane(props.pane.id)}
				>
					{props.pane.title}
				</button>
				<span class="av-pane-spacer" />
				<button
					type="button"
					class="av-pane-btn"
					title="Split right (open a terminal beside this pane)"
					aria-label="Split right"
					onClick={() => splitWith("row")}
				>
					⊞▏
				</button>
				<button
					type="button"
					class="av-pane-btn"
					title="Split down (open a terminal below this pane)"
					aria-label="Split down"
					onClick={() => splitWith("column")}
				>
					⊞▁
				</button>
				<Show when={props.pane.kind !== "chat"}>
					<button
						type="button"
						class="av-pane-btn av-pane-close"
						title="Close pane"
						aria-label="Close pane"
						onClick={() => store.closePane(props.pane.id)}
					>
						✕
					</button>
				</Show>
			</div>
			<div class="av-pane-body">
				<Switch>
					<Match when={props.pane.kind === "chat"}>
						<ChannelView channel={store.workspaceChannel()} />
					</Match>
					<Match when={props.pane.kind === "terminal"}>
						<TerminalBody
							agent={props.agent}
							terminalId={props.pane.terminalId}
						/>
					</Match>
					<Match when={props.pane.kind === "file"}>
						<FileViewer path={props.pane.filePath} />
					</Match>
				</Switch>
			</div>
		</div>
	);
};

/** The split tree within the active tab: a leaf renders one pane; a split lays
 *  its two children out in a row (side by side) or column (stacked), nesting
 *  recursively. The focused pane id highlights the leaf the split buttons hit. */
const SplitView: Component<{
	node: SplitNode;
	agent: Agent;
	focusedPaneId: string;
}> = (props) => (
	<Switch>
		<Match when={props.node.kind === "leaf" ? props.node : null}>
			{(leaf) => (
				<PaneView
					pane={leaf().pane}
					agent={props.agent}
					focused={leaf().pane.id === props.focusedPaneId}
				/>
			)}
		</Match>
		<Match when={props.node.kind === "split" ? props.node : null}>
			{(split) => (
				<div class="av-split" classList={{ [split().direction]: true }}>
					<SplitView
						node={split().left}
						agent={props.agent}
						focusedPaneId={props.focusedPaneId}
					/>
					<SplitView
						node={split().right}
						agent={props.agent}
						focusedPaneId={props.focusedPaneId}
					/>
				</div>
			)}
		</Match>
	</Switch>
);

/** The agent's next FIXTURE terminal not already shown as a tab or pane anywhere,
 *  built as a terminal pane ready to place — the rich-scrollback source "new tab"
 *  and "split" prefer. Returns undefined once every fixture terminal is placed;
 *  callers then fall back to a minted placeholder (`store.newTerminalPane`), so
 *  the buttons never disable. (A later context menu will pick terminal / markdown
 *  / file.) */
export const nextFreeTerminalPane = (
	agent: Agent,
	tabs: AgentTab[],
): Pane | undefined => {
	const shown = new Set(
		tabs.flatMap((t) => splitPanes(t.layout).map((p) => p.terminalId)),
	);
	const term = agent.terminals.find((t) => !shown.has(t.id));
	if (!term) return undefined;
	return {
		id: term.id,
		kind: "terminal",
		title: term.name,
		terminalId: term.id,
	};
};

/** The agent view (design D6): a strip of full-screen TABS across the top; the
 *  active tab shows its group of PANES (a split tree). The first tab is the chat
 *  tab and is always present. "+" always opens a new full-screen terminal tab
 *  (the agent's fixture terminals first, then minted placeholders; never
 *  disabled). Inside a tab, each pane's split buttons open terminals as split
 *  panes the same way. Reached by selecting an agent in the tree, a swimlane
 *  gutter, or a card. */
export const AgentView: Component = () => {
	const store = useStore();
	// Open a new full-screen terminal tab: reuse the next unplaced fixture
	// terminal (rich scrollback) while any remain, else mint a fresh placeholder.
	// Always opens a tab — the "+" button never disables.
	const openTerminalTab = (agent: Agent) => {
		store.openTab(
			nextFreeTerminalPane(agent, store.agentTabs()) ??
				store.newTerminalPane(agent),
		);
	};
	return (
		<Show
			when={store.selectedAgent()}
			fallback={<p class="muted">Select an agent.</p>}
		>
			{(agent) => (
				<div class="agent-view">
					<div class="av-header">
						<StateDot state={agent().lifecycle ?? "idle"} />
						<span class="av-name">{agent().account.handle}</span>
						<Show when={agent().model}>
							<span class="av-model">{agent().model}</span>
						</Show>
						<Show when={agent().cwd}>
							<span class="av-cwd">{agent().cwd}</span>
						</Show>
						<span class="av-spacer" />
						<button
							type="button"
							class="av-share"
							disabled
							title="Share this workspace with other users (wired with the daemon)"
						>
							Share
						</button>
					</div>
					<div class="av-body">
						<div class="av-tabs" role="tablist" aria-label="Agent tabs">
							<For each={store.agentTabs()}>
								{(tab) => (
									<div
										class="av-tab"
										classList={{ active: tab.id === store.activeAgentTabId() }}
									>
										<button
											type="button"
											role="tab"
											class="av-tab-label"
											aria-selected={tab.id === store.activeAgentTabId()}
											onClick={() => store.setActiveAgentTab(tab.id)}
										>
											{tab.title}
										</button>
										<Show when={tab.id !== CHAT_TAB_ID}>
											<button
												type="button"
												class="av-tab-close"
												aria-label={`Close ${tab.title}`}
												title={`Close ${tab.title}`}
												onClick={() => store.closeTab(tab.id)}
											>
												✕
											</button>
										</Show>
									</div>
								)}
							</For>
							<button
								type="button"
								class="av-tab-new"
								aria-label="New tab"
								title="New tab (terminal)"
								onClick={() => openTerminalTab(agent())}
							>
								+
							</button>
						</div>
						<div class="av-tree">
							<Show
								when={store.activeAgentTab()}
								fallback={<div class="av-leaf-empty muted">No tab open.</div>}
							>
								{(tab) => (
									<SplitView
										node={tab().layout}
										agent={agent()}
										focusedPaneId={tab().focusedPaneId}
									/>
								)}
							</Show>
						</div>
					</div>
					<LogPanel agent={agent()} />
				</div>
			)}
		</Show>
	);
};
