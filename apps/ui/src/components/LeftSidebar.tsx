import { type Component, createMemo, createSignal, For, Show } from "solid-js";
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
import { openLink } from "../open-link";
import { type Agent, type AgentTreeNode, agentTree } from "../stub-data";
import { DEMO_AGENTS, isDemoId } from "../tour/demo";
import { CoachTip, CoachTipContent, CoachTipTrigger } from "./CoachTip";
import { Glyph } from "./Glyph";
import { RuntimeMarker } from "./RuntimeMarker";
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
				data-tour={
					a().account.id === DEMO_AGENTS[0]?.account.id
						? "demo-agent"
						: undefined
				}
				type="button"
				class={[
					"tree-agent",
					{
						selected:
							store.selectedAgentId() === a().account.id &&
							store.view() === "agent",
					},
				]}
				{...openLink(
					store,
					() => `/agent/${a().account.id}`,
					() => store.openAgent(a().account.id),
				)}
			>
				<StateDot state={a().lifecycle ?? "idle"} scale={2} />
				<Show when={a().runtime}>{(m) => <RuntimeMarker marker={m()} />}</Show>
				<Show when={isDemoId(a().account.id)}>
					<span class="cx-tour-demo-badge">Demo</span>
				</Show>
				<span class="name">{a().account.handle}</span>
				<Show when={a().role !== undefined && a().role !== "worker"}>
					<span class="role-pip" data-role={a().role} title={a().role}>
						<Glyph name="role" />
					</span>
				</Show>
				<Show when={props.badge !== undefined}>
					<span class="tree-badge">{props.badge}</span>
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
				{pinned() ? <Glyph name="pin" /> : <Glyph name="pin-outline" />}
			</button>
		</div>
	);
};

/** A parent agent row with collapsible child agents and attached channels. */
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
					aria-label={`${collapsed() ? "Expand" : "Collapse"} ${props.node.agent.account.handle}'s tree`}
					onClick={() => store.toggleAgent(agentId())}
				>
					<span class={["tree-caret", { collapsed: collapsed() }]}>
						<Glyph name="disclosure-open" />
					</span>
				</button>
				<AgentLeaf
					agent={props.node.agent}
					badge={
						props.node.children.length > 0
							? countDescendants(props.node)
							: undefined
					}
				/>
			</div>
			<Show when={!collapsed()}>
				<div class="tree-children">
					<For each={props.node.channels}>
						{(channel) => <ChannelRow channel={channel} />}
					</For>
					<For each={props.node.children}>
						{(child) => <Node node={child} />}
					</For>
				</div>
			</Show>
		</div>
	);
};

/** Child agents or channels render the parent form; otherwise render the plain leaf. */
const Node: Component<{ node: AgentTreeNode }> = (props) => (
	<Show
		when={props.node.children.length > 0 || props.node.channels.length > 0}
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
					{...openLink(
						store,
						() => store.channelPath(channel().id),
						() => store.openChannel(channel().id),
					)}
				>
					<span class="ch-glyph" aria-hidden="true">
						{channelGlyph(channel().kind)}
					</span>
					<Show when={isDemoId(channel().id)}>
						<span class="cx-tour-demo-badge">Demo</span>
					</Show>
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
								<Glyph name="subscribed" />
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
							{subscribed() ? (
								<Glyph name="subscribed" />
							) : (
								<Glyph name="unsubscribed" />
							)}
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
						{...openLink(
							store,
							() => store.topicPath(group.topic.id),
							() => store.openTopic(group.topic.id),
						)}
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
				<span class={["browse-caret", { open: open() }]}>
					<Glyph name="disclosure" />
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

/** One collapsible sidebar section for the agent tree and channel bands. */
const ChannelsSection: Component = () => {
	const store = useStore();
	const collapsed = () => store.isSectionCollapsed("channels");
	const memberChannels = () => railChannels(store.channels());
	// Only member channels may be claimed; a non-member one stays in browse.
	const tree = createMemo(() => agentTree(store.agents(), memberChannels()));
	const claimedIds = createMemo(() => {
		const ids = new Set<string>();
		const nodes = [...tree()];
		while (nodes.length > 0) {
			const node = nodes.pop();
			if (node) {
				for (const channel of node.channels) ids.add(channel.id);
				nodes.push(...node.children);
			}
		}
		return ids;
	});
	const unclaimed = () =>
		memberChannels().filter((channel) => !claimedIds().has(channel.id));
	const sharedGroups = () =>
		store.channelGroups().filter((group) => group.visibility === "shared");
	const sections = () => channelSections(unclaimed(), sharedGroups());
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
				<span class={["ws-caret", { open: !collapsed() }]}>
					<Glyph name="disclosure" />
				</span>
				Channels
			</button>
			<Show when={!collapsed()}>
				<div class="ws-section-body">
					<div class="tree" data-tour="agent-tree">
						<Show
							when={store.firstSnapshotArrived() && tree().length === 0}
							fallback={
								<For each={tree()}>{(node) => <Node node={node} />}</For>
							}
						>
							<div class="tree-empty">
								No agents in the fleet yet — the supervisor builds the tree.
							</div>
						</Show>
					</div>
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

/** The left sidebar links and its unified channel-and-agent tree. */
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
	// shortcutFor inside CoachTipContent (never hand-authored — D4). view.agents/
	// view.backlog/view.done have no keymap row yet, so the tooltip is label-only there.
	const platform = detectPlatform();
	const ariaChord = (id: string) => shortcutForAria(id as CommandId, platform);
	return (
		<aside class="left" aria-label="Agents">
			<div class="left-head">
				<span class="label">Workspace</span>
			</div>
			<CoachTip>
				<CoachTipTrigger
					as="button"
					type="button"
					class={["bridge-link", { active: store.view() === "agents" }]}
					{...openLink(
						store,
						() => "/agents",
						() => store.showAgents(),
					)}
					aria-keyshortcuts={ariaChord("view.agents")}
				>
					<span class="glyph" aria-hidden="true">
						<Glyph name="role" />
					</span>
					<span>Agents</span>
				</CoachTipTrigger>
				<CoachTipContent label="Agents" command={"view.agents" as CommandId} />
			</CoachTip>
			<CoachTip>
				<CoachTipTrigger
					as="button"
					type="button"
					class={["bridge-link", { active: store.view() === "bridge" }]}
					{...openLink(
						store,
						() => "/",
						() => store.showBridge(),
					)}
					aria-keyshortcuts={ariaChord("view.bridge")}
				>
					<span class="glyph" aria-hidden="true">
						<Glyph name="status" />
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
					{...openLink(
						store,
						() => "/backlog",
						() => store.showBacklog(),
					)}
					aria-keyshortcuts={ariaChord("view.backlog")}
				>
					<span class="glyph" aria-hidden="true">
						<Glyph name="list" />
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
					{...openLink(
						store,
						() => "/done",
						() => store.showDone(),
					)}
					aria-keyshortcuts={ariaChord("view.done")}
				>
					<span class="glyph" aria-hidden="true">
						<Glyph name="check" />
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
					{...openLink(
						store,
						() => "/settings",
						() => store.showSettings(),
					)}
					aria-keyshortcuts={ariaChord("view.settings")}
				>
					<span class="glyph" aria-hidden="true">
						<Glyph name="gear" />
					</span>
					<span>Settings</span>
				</CoachTipTrigger>
				<CoachTipContent
					label="Settings"
					command={"view.settings" as CommandId}
				/>
			</CoachTip>
			<ChannelsSection />
		</aside>
	);
};
