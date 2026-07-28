import {
	type Component,
	createSignal,
	For,
	Match,
	Show,
	Switch,
} from "solid-js";
import { activeWorkstreams, backlogWorkstreams } from "../board";
import {
	agentDmAccountId,
	browsableChannels,
	channelGlyph,
	channelSections,
	dmChannels,
	dmLabel,
	isDm,
	railChannels,
} from "../comms";
import type { Channel } from "../comms-stub";
import { useStore } from "../context";
import {
	type Agent,
	STUB_AGENTS,
	STUB_TREE,
	type TreeNode,
} from "../stub-data";
import { StateDot } from "./StateDot";

const agentById = (id: string): Agent | undefined =>
	STUB_AGENTS.find((a) => a.account.id === id);

/** An agent leaf row in the tree. */
const AgentLeaf: Component<{ agentId: string }> = (props) => {
	const store = useStore();
	const agent = () => agentById(props.agentId);
	return (
		<Show when={agent()}>
			{(a) => (
				<button
					type="button"
					class="tree-agent"
					classList={{
						selected:
							store.selectedAgentId() === a().account.id &&
							store.view() === "agent",
					}}
					onClick={() => store.openAgent(a().account.id)}
				>
					<StateDot state={a().lifecycle ?? "idle"} />
					<span class="name">{a().account.handle}</span>
					<Show when={a().role !== "worker"}>
						<span class="role-pip" data-role={a().role} title={a().role}>
							{a().role === "supervisor" ? "◆" : "🛡"}
						</span>
					</Show>
				</button>
			)}
		</Show>
	);
};

/** A folder row + its recursively-rendered children. */
const FolderRow: Component<{ node: Extract<TreeNode, { kind: "folder" }> }> = (
	props,
) => {
	const store = useStore();
	const folder = () => props.node.folder;
	const collapsed = () => store.isFolderCollapsed(folder().id);
	const agentCount = () => countAgents(props.node);
	return (
		<div class="folder">
			<button
				type="button"
				class="folder-row"
				aria-expanded={!collapsed()}
				onClick={() => store.toggleFolder(folder().id)}
			>
				<span class="folder-caret" classList={{ collapsed: collapsed() }}>
					▼
				</span>
				<span class="folder-icon" style={{ color: folder().color }}>
					{folder().icon}
				</span>
				<span class="folder-name">{folder().name}</span>
				<span class="folder-badge">{agentCount()}</span>
			</button>
			<Show when={!collapsed()}>
				<div class="folder-children">
					<For each={folder().children}>{(child) => <Node node={child} />}</For>
				</div>
			</Show>
		</div>
	);
};

/** Dispatch a tree node to the right row renderer. The `when` callbacks narrow
 *  the TreeNode union on its `kind` discriminant, so no cast is needed. */
const Node: Component<{ node: TreeNode }> = (props) => (
	<Switch>
		<Match when={props.node.kind === "folder" ? props.node : null}>
			{(folderNode) => <FolderRow node={folderNode()} />}
		</Match>
		<Match when={props.node.kind === "agent" ? props.node : null}>
			{(agentNode) => <AgentLeaf agentId={agentNode().agentId} />}
		</Match>
	</Switch>
);

/** Count agent leaves under a node (recursively), for the folder badge. */
function countAgents(node: TreeNode): number {
	if (node.kind === "agent") return 1;
	return node.folder.children.reduce((n, c) => n + countAgents(c), 0);
}

/** One rail row — a channel/DM the caller is a member of. The select button
 *  routes to the channel view via openChannel (a 1:1 agent DM delegates to the
 *  workspace); an unread badge and the subscribe toggle sit on the right. */
const ChannelRow: Component<{ channel: Channel }> = (props) => {
	const store = useStore();
	const channel = () => props.channel;
	const selected = () =>
		store.selectedChannelId() === channel().id && store.view() === "channel";
	const byId = () => new Map(store.accounts().map((a) => [a.id, a]));
	// A DM's label is its other participants; a channel's is its own name.
	const label = () =>
		isDm(channel())
			? dmLabel(channel(), store.caller().id, byId())
			: channel().name;
	const subscribed = () => channel().membership === "subscribed";
	// always-subscribed-to-own is implicit + non-togglable (design.md:416): render
	// the control fixed, never a toggle that claims you can unsubscribe.
	const fixed = () => channel().alwaysSubscribed === true;

	return (
		<div class="ch-row" classList={{ selected: selected() }}>
			<button
				type="button"
				class="ch-row-select"
				onClick={() => store.openChannel(channel().id)}
			>
				<span class="ch-glyph" aria-hidden="true">
					{channelGlyph(channel().kind)}
				</span>
				<span class="ch-name">{label()}</span>

				<Show when={(channel().unread ?? 0) > 0}>
					<span class="ch-unread">{channel().unread}</span>
				</Show>
			</button>

			{/* Subscribe toggle (only meaningful once joined, which every rail row
			    is). Fixed where the subscription is implicit; DISABLED everywhere
			    else until the subscribe RPC lands — the wire has none, and the
			    local-only toggle this used to drive silently reverted on the next
			    SubscribeComms snapshot. It still shows the real membership, it
			    just can't change it yet. */}
			<Show
				when={!fixed()}
				fallback={
					<span
						class="ch-sub fixed"
						role="img"
						title="Always subscribed — this subscription is implicit and can't be turned off."
						aria-label="Always subscribed"
					>
						◉
					</span>
				}
			>
				<button
					type="button"
					class="ch-sub"
					classList={{ on: subscribed() }}
					disabled
					title={
						subscribed()
							? "Subscribed — new messages are pushed to you. Unsubscribing is not wired up yet."
							: "Joined, not subscribed. Subscribing is not wired up yet."
					}
					aria-pressed={subscribed()}
				>
					{subscribed() ? "◉" : "○"}
				</button>
			</Show>
		</div>
	);
};

/** The browse/discover list: channels the caller can see but hasn't joined
 *  (membership `none`). Collapsed by default so the rail stays member-focused;
 *  expanding reveals a join affordance per channel. */
const BrowseChannels: Component<{ channels: Channel[] }> = (props) => {
	const [open, setOpen] = createSignal(false);
	return (
		<div class="rail-section rail-browse">
			<button
				type="button"
				class="rail-section-head browse-head"
				onClick={() => setOpen((o) => !o)}
				aria-expanded={open()}
			>
				<span class="browse-caret" classList={{ open: open() }}>
					▸
				</span>
				browse channels
				<span class="browse-count">{props.channels.length}</span>
			</button>
			<Show when={open()}>
				<For each={props.channels}>
					{(channel) => (
						<div class="ch-row browse-row">
							<span class="ch-glyph" aria-hidden="true">
								#
							</span>
							<span class="ch-name">{channel.name}</span>
							<button
								type="button"
								class="ch-join"
								disabled
								title="Joining is not wired up yet — the server has no join RPC, so this would only pretend."
							>
								join
							</button>
						</div>
					)}
				</For>
			</Show>
		</div>
	);
};

/** The collapsible Channels section (above Agent workspaces): grouped member
 *  channels, a group-DMs subsection (1:1 agent DMs are excluded — the agent
 *  workspace is their surface, §589), then a browse/join list. */
const ChannelsSection: Component = () => {
	const store = useStore();
	const collapsed = () => store.isSectionCollapsed("channels");
	const memberChannels = () => railChannels(store.channels());
	const sections = () =>
		channelSections(memberChannels(), store.channelGroups());
	// Group DMs only: drop any 1:1 agent DM (its surface is the workspace).
	const dms = () => {
		const byId = new Map(store.accounts().map((a) => [a.id, a]));
		return dmChannels(memberChannels()).filter(
			(c) => agentDmAccountId(c, store.caller().id, byId) === undefined,
		);
	};
	const browsable = () => browsableChannels(store.channels());

	return (
		<div class="ws-section">
			<button
				type="button"
				class="ws-section-head"
				onClick={() => store.toggleSection("channels")}
				aria-expanded={!collapsed()}
			>
				<span class="ws-caret" classList={{ open: !collapsed() }}>
					▸
				</span>
				Channels
			</button>
			<Show when={!collapsed()}>
				<div class="ws-section-body">
					<For each={sections()}>
						{(section) => (
							<div class="rail-section">
								<div class="rail-section-head">
									{section.group?.name ?? "channels"}
									<Show when={section.group?.visibility === "shared"}>
										<span
											class="rail-vis"
											title="Shared — visible to all accounts"
										>
											shared
										</span>
									</Show>
								</div>
								<For each={section.channels}>
									{(channel) => <ChannelRow channel={channel} />}
								</For>
							</div>
						)}
					</For>

					<Show when={dms().length > 0}>
						<div class="rail-section">
							<div class="rail-section-head">direct messages</div>
							<For each={dms()}>
								{(channel) => <ChannelRow channel={channel} />}
							</For>
						</div>
					</Show>

					<Show when={browsable().length > 0}>
						<BrowseChannels channels={browsable()} />
					</Show>
				</div>
			</Show>
		</div>
	);
};

/** The collapsible Agent workspaces section (below Channels): the existing
 *  user-organized folder tree of agents. */
const AgentsSection: Component = () => {
	const store = useStore();
	const collapsed = () => store.isSectionCollapsed("agents");
	return (
		<div class="ws-section">
			<button
				type="button"
				class="ws-section-head"
				onClick={() => store.toggleSection("agents")}
				aria-expanded={!collapsed()}
			>
				<span class="ws-caret" classList={{ open: !collapsed() }}>
					▸
				</span>
				Agent workspaces
			</button>
			<Show when={!collapsed()}>
				<div class="tree ws-section-body">
					<For each={STUB_TREE}>{(node) => <Node node={node} />}</For>
				</div>
			</Show>
		</div>
	);
};

/** The left sidebar: the Workspace header and Bridge/Backlog/Done/Settings nav
 *  links pinned at the top, then two collapsible sections — Channels above Agent
 *  workspaces (design compass-0.7 §578-590). */
export const LeftSidebar: Component = () => {
	const store = useStore();
	// The Bridge badge mirrors the board's in-flight count: active columns minus
	// done, via the same board.ts partition the Bridge reads — so the sidebar can
	// never show more than the board displays (D1, one source of truth).
	const inFlightCount = () =>
		activeWorkstreams(store.workstreams()).filter((w) => w.state !== "done")
			.length;
	// Backlog view badge: the pre-active tier (Todo + Backlog) the human triages.
	const backlogCount = () =>
		backlogWorkstreams(store.workstreams()).length +
		store.assignedIssues().length;
	return (
		<aside class="left" aria-label="Agents">
			<div class="left-head">
				<span class="label">Workspace</span>
				<button type="button" class="icon-btn" title="New folder">
					+
				</button>
			</div>
			<button
				type="button"
				class="bridge-link"
				classList={{ active: store.view() === "bridge" }}
				onClick={() => store.showBridge()}
			>
				<span class="glyph" aria-hidden="true">
					▦
				</span>
				<span>Bridge</span>
				<span class="count">{inFlightCount()}</span>
			</button>
			<button
				type="button"
				class="bridge-link"
				classList={{ active: store.view() === "backlog" }}
				onClick={() => store.showBacklog()}
			>
				<span class="glyph" aria-hidden="true">
					▤
				</span>
				<span>Backlog</span>
				<span class="count">{backlogCount()}</span>
			</button>
			<button
				type="button"
				class="bridge-link"
				classList={{ active: store.view() === "done" }}
				onClick={() => store.showDone()}
			>
				<span class="glyph" aria-hidden="true">
					✓
				</span>
				<span>Done</span>
			</button>
			<button
				type="button"
				class="bridge-link"
				classList={{ active: store.view() === "settings" }}
				onClick={() => store.showSettings()}
			>
				<span class="glyph" aria-hidden="true">
					⚙
				</span>
				<span>Settings</span>
			</button>
			<ChannelsSection />
			<AgentsSection />
		</aside>
	);
};
