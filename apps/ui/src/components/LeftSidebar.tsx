import { type Component, createSignal, For, Show } from "solid-js";
import { activeIssues, backlogIssues } from "../board";
import {
	agentDmAccountId,
	browsableChannels,
	channelGlyph,
	channelSections,
	dmChannels,
	dmLabel,
	isDm,
	railChannels,
	topicsOf,
} from "../comms";
import type { Channel } from "../comms-stub";
import { useStore } from "../context";
import type { CommandId } from "../keyboard/commands";
import { detectPlatform } from "../keyboard/dispatch";
import { shortcutForAria } from "../keyboard/keymap";
import { type Agent, type AgentTreeNode, agentTree } from "../stub-data";
import { CoachTip, CoachTipContent, CoachTipTrigger } from "./CoachTip";
import { StateDot } from "./StateDot";

/** An agent leaf row in the tree — the per-agent select button, plus a hover
 *  pin/unpin affordance on the right (Record A §T4). Renders the same StateDot /
 *  handle / non-worker role-pip (RIG-1623: pip left alone) for both a childless
 *  leaf and a parent agent's own row. An optional descendant badge trails the
 *  row when the agent has children. The pin toggle sits as a sibling BUTTON
 *  outside the select button (a button can't nest a button), calling
 *  pinAgent/unpinAgent with state from isPinned. */
const AgentLeaf: Component<{ agent: Agent; badge?: number }> = (props) => {
	const store = useStore();
	const a = () => props.agent;
	const pinned = () => store.isPinned(a().account.id);
	return (
		<div class="tree-agent-row">
			<button
				type="button"
				class={[
					"tree-agent",
					{
						selected:
							store.selectedAgentId() === a().account.id &&
							store.view() === "agent",
					},
				]}
				onClick={() => store.openAgent(a().account.id)}
			>
				<StateDot state={a().lifecycle ?? "idle"} />
				<span class="name">{a().account.handle}</span>
				<Show when={a().role !== undefined && a().role !== "worker"}>
					<span class="role-pip" data-role={a().role} title={a().role}>
						◆
					</span>
				</Show>
				<Show when={props.badge !== undefined}>
					<span class="folder-badge">{props.badge}</span>
				</Show>
				<Show when={a().activity}>
					<span class="agent-activity" title={a().activity}>
						{a().activity}
					</span>
				</Show>
			</button>
			<button
				type="button"
				class={["tree-agent-pin", { pinned: pinned() }]}
				aria-pressed={pinned() ? "true" : "false"}
				title={
					pinned()
						? `Unpin ${a().account.handle} from the fleet sidebar`
						: `Pin ${a().account.handle} to the fleet sidebar`
				}
				aria-label={
					pinned()
						? `Unpin ${a().account.handle} from the fleet sidebar`
						: `Pin ${a().account.handle} to the fleet sidebar`
				}
				onClick={() =>
					pinned()
						? store.unpinAgent(a().account.id)
						: store.pinAgent(a().account.id)
				}
			>
				{pinned() ? "★" : "☆"}
			</button>
		</div>
	);
};

/** A parent agent's row + its recursively-rendered children. The agent's own
 *  row selects (openAgent); a dedicated caret sub-button toggles collapse — the
 *  expand/collapse + descendant count a folder row carried today, re-keyed to
 *  the parent agent id. */
const Branch: Component<{ node: AgentTreeNode }> = (props) => {
	const store = useStore();
	const agentId = () => props.node.agent.account.id;
	const collapsed = () => store.isAgentCollapsed(agentId());
	return (
		<div class="folder">
			<div class="tree-branch">
				<button
					type="button"
					class="tree-branch-caret"
					aria-expanded={!collapsed() ? "true" : "false"}
					aria-label={`${collapsed() ? "Expand" : "Collapse"} ${props.node.agent.account.handle}'s agents`}
					onClick={() => store.toggleAgent(agentId())}
				>
					<span class={["folder-caret", { collapsed: collapsed() }]}>▼</span>
				</button>
				<AgentLeaf
					agent={props.node.agent}
					badge={countDescendants(props.node)}
				/>
			</div>
			<Show when={!collapsed()}>
				<div class="folder-children">
					<For each={props.node.children}>
						{(child) => <Node node={child} />}
					</For>
				</div>
			</Show>
		</div>
	);
};

/** Dispatch a derived-tree node: a node with children renders the parent form
 *  (caret + badge + collapsible children); a childless node renders the plain
 *  agent leaf. */
const Node: Component<{ node: AgentTreeNode }> = (props) => (
	<Show
		when={props.node.children.length > 0}
		fallback={<AgentLeaf agent={props.node.agent} />}
	>
		<Branch node={props.node} />
	</Show>
);

/** Count descendant agents under a node — the parent-agent badge: every agent
 *  in the subtree below it (all descendants, recursive), not counting itself. */
function countDescendants(node: AgentTreeNode): number {
	return node.children.reduce((n, c) => n + 1 + countDescendants(c), 0);
}

/** The number of most-recent topics a channel row surfaces as deep-nav
 *  sub-rows — a UI constant (the sidebar hint, not the full index). */
const RECENT_TOPIC_COUNT = 3;

/** One rail row — a channel/DM the caller is a member of. The select button
 *  routes to the channel view via openChannel (a 1:1 agent DM delegates to the
 *  workspace); an unread badge and the subscribe toggle sit on the right. A
 *  non-DM channel also surfaces its ≤3 most-recent topics as deep-nav sub-rows
 *  routing straight to a topic view (openTopic). */
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
	// mandatory_subscription channels force-subscribe every member, so ANY
	// subscribe affordance — even a disabled toggle or a fixed marker — would be a
	// lie: the control is HIDDEN entirely (§T8).
	const mandatory = () => channel().mandatorySubscription === true;
	// The channel's most-recent topics (last-activity-desc), capped — a DM has no
	// topic index (it is a flat conversation), so it surfaces none.
	const recentTopics = () =>
		isDm(channel())
			? []
			: topicsOf(store.topics(), store.messages(), channel().id).slice(
					0,
					RECENT_TOPIC_COUNT,
				);

	return (
		<div class="ch-row-group">
			<div class={["ch-row", { selected: selected() }]}>
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
				    just can't change it yet. On mandatory_subscription channels the
				    control is HIDDEN entirely: every member is force-subscribed, so
				    any affordance (toggle or fixed marker) would be a lie (§T8). */}
				<Show when={!mandatory()}>
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
							class={["ch-sub", { on: subscribed() }]}
							disabled
							title={
								subscribed()
									? "Subscribed — new messages are pushed to you. Unsubscribing is not wired up yet."
									: "Joined, not subscribed. Subscribing is not wired up yet."
							}
							aria-pressed={subscribed() ? "true" : "false"}
						>
							{subscribed() ? "◉" : "○"}
						</button>
					</Show>
				</Show>
			</div>

			{/* The channel's ≤3 most-recent topics as deep-nav sub-rows — a straight
			    jump into that topic's message view (openTopic). Not `.ch-row`, so the
			    rail's channel-row count is unaffected. */}
			<For each={recentTopics()}>
				{(group) => (
					<button
						type="button"
						class={[
							"ch-topic-row",
							{
								selected:
									store.selectedTopicId() === group.topic.id &&
									store.view() === "topic",
							},
						]}
						onClick={() => store.openTopic(group.topic.id)}
					>
						<span class="ch-topic-name">{group.topic.name}</span>
					</button>
				)}
			</For>
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
				aria-expanded={open() ? "true" : "false"}
			>
				<span class={["browse-caret", { open: open() }]}>▸</span>
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
				aria-expanded={!collapsed() ? "true" : "false"}
			>
				<span class={["ws-caret", { open: !collapsed() }]}>▸</span>
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
				aria-expanded={!collapsed() ? "true" : "false"}
			>
				<span class={["ws-caret", { open: !collapsed() }]}>▸</span>
				Agent workspaces
			</button>
			<Show when={!collapsed()}>
				<div class="tree ws-section-body">
					<Show
						when={
							store.firstSnapshotArrived() &&
							agentTree(store.agents()).length === 0
						}
						fallback={
							<For each={agentTree(store.agents())}>
								{(node) => <Node node={node} />}
							</For>
						}
					>
						<div class="tree-empty">
							No agents in the fleet yet — the supervisor builds the tree.
						</div>
					</Show>
				</div>
			</Show>
		</div>
	);
};

/** The left sidebar: the Workspace header and Bridge/Backlog/Done/Settings nav
 *  links pinned at the top, then two collapsible sections — Channels above Agent
 *  workspaces (design: architecture-lineage). */
export const LeftSidebar: Component = () => {
	const store = useStore();
	// The Bridge badge mirrors the board's in-flight count: active columns minus
	// done, via the same board.ts partition the Bridge reads — so the sidebar can
	// never show more than the board displays (D1, one source of truth).
	const inFlightCount = () =>
		activeIssues(store.issues()).filter((w) => w.state !== "done").length;
	// Backlog view badge: the pre-active tier (Todo + Backlog) the human triages.
	const backlogCount = () =>
		backlogIssues(store.issues()).length + store.assignedIssues().length;
	// Point-of-use coaching (RIG-2530): the view buttons announce their chord via
	// aria-keyshortcuts + a CoachTip tooltip, resolved from the keymap through
	// shortcutFor inside CoachTipContent (never hand-authored — D4). view.backlog/
	// view.done have no keymap row yet, so the tooltip is label-only there.
	const platform = detectPlatform();
	const ariaChord = (id: string) => shortcutForAria(id as CommandId, platform);
	return (
		<aside class="left" aria-label="Agents">
			<div class="left-head">
				<span class="label">Workspace</span>
				<button type="button" class="icon-btn" title="New folder">
					+
				</button>
			</div>
			<CoachTip>
				<CoachTipTrigger
					as="button"
					type="button"
					class={["bridge-link", { active: store.view() === "bridge" }]}
					onClick={() => store.showBridge()}
					aria-keyshortcuts={ariaChord("view.bridge")}
				>
					<span class="glyph" aria-hidden="true">
						▦
					</span>
					<span>Bridge</span>
					<span class="count">{inFlightCount()}</span>
				</CoachTipTrigger>
				<CoachTipContent label="Bridge" command={"view.bridge" as CommandId} />
			</CoachTip>
			<CoachTip>
				<CoachTipTrigger
					as="button"
					type="button"
					class={["bridge-link", { active: store.view() === "backlog" }]}
					onClick={() => store.showBacklog()}
					aria-keyshortcuts={ariaChord("view.backlog")}
				>
					<span class="glyph" aria-hidden="true">
						▤
					</span>
					<span>Backlog</span>
					<span class="count">{backlogCount()}</span>
				</CoachTipTrigger>
				<CoachTipContent
					label="Backlog"
					command={"view.backlog" as CommandId}
				/>
			</CoachTip>
			<CoachTip>
				<CoachTipTrigger
					as="button"
					type="button"
					class={["bridge-link", { active: store.view() === "done" }]}
					onClick={() => store.showDone()}
					aria-keyshortcuts={ariaChord("view.done")}
				>
					<span class="glyph" aria-hidden="true">
						✓
					</span>
					<span>Done</span>
				</CoachTipTrigger>
				<CoachTipContent label="Done" command={"view.done" as CommandId} />
			</CoachTip>
			<CoachTip>
				<CoachTipTrigger
					as="button"
					type="button"
					class={["bridge-link", { active: store.view() === "settings" }]}
					onClick={() => store.showSettings()}
					aria-keyshortcuts={ariaChord("view.settings")}
				>
					<span class="glyph" aria-hidden="true">
						⚙
					</span>
					<span>Settings</span>
				</CoachTipTrigger>
				<CoachTipContent
					label="Settings"
					command={"view.settings" as CommandId}
				/>
			</CoachTip>
			<ChannelsSection />
			<AgentsSection />
		</aside>
	);
};
