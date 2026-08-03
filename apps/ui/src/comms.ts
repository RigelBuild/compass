// The comms partition — the pure core of the comms model (design compass-0.7).
// One source of truth for how the shell organizes channels, DMs,
// and threaded messages, read by the channel rail and the conversation view so
// the shape can never drift between surfaces.
//
// Pure over injected fixtures (no fixture import, no store), so the whole model
// is unit-testable and the same functions serve the stub today and the
// @compass/client stream later. Mirrors comms-stub.ts, which mirrors
// comms.proto.

import type {
	Account,
	Channel,
	ChannelGroup,
	ConvBlock,
	Message,
	Topic,
} from "./comms-stub";
import { RESERVED_MENTIONS } from "./comms-stub";

// ── Account lookup ───────────────────────────────────────────────────────────

/** The handle for an account id, or the id itself when unknown (so an
 *  unresolved author never renders blank). */
export function handleOf(
	byId: Map<string, Account>,
	accountId: string,
): string {
	return byId.get(accountId)?.handle ?? accountId;
}

// ── Channel organization (the left rail) ─────────────────────────────────────

/** Whether a channel is a direct/group DM (rendered in the DMs section) rather
 *  than a group channel (rendered under its group). */
export function isDm(channel: Channel): boolean {
	return channel.kind === "dm" || channel.kind === "group_dm";
}

/** The glyph before a channel name, by kind (Discord-style: # for a channel,
 *  @ for a DM, a cluster glyph for a group DM). One home so the rail row and
 *  the channel header never drift. */
export function channelGlyph(kind: Channel["kind"]): string {
	switch (kind) {
		case "dm":
			return "@";
		case "group_dm":
			return "⌗";
		default:
			return "#";
	}
}

/** The channels that belong in the rail: ones the caller has joined or
 *  subscribed to (design: a `none` channel the caller can't read is NOT a rail
 *  row — the rail shows member channels only). */
export function railChannels(channels: readonly Channel[]): Channel[] {
	return channels.filter((c) => c.membership !== "none");
}

/** The channels the caller can see but hasn't joined — the browse/discover list
 *  (membership `none` → a join affordance lives here, not in the rail). */
export function browsableChannels(channels: readonly Channel[]): Channel[] {
	return channels.filter((c) => c.membership === "none");
}

/** A group of channels under one channel group, for the rail's grouped view.
 *  `group` is undefined for ungrouped channels (owner-scoped, no namespace). */
export interface ChannelSection {
	group: ChannelGroup | undefined;
	channels: Channel[];
}

/** Partition channels into the rail's sections: one section per channel group
 *  (in `groups` order), then a trailing section for ungrouped channels. DMs are
 *  excluded — they render in their own section (see `dmChannels`). Group order
 *  is the caller's; channel order within a group is fixture order. A group with
 *  no visible channels is omitted so the rail shows no empty headers. */
export function channelSections(
	channels: readonly Channel[],
	groups: readonly ChannelGroup[],
): ChannelSection[] {
	const grouped = channels.filter((c) => !isDm(c));
	const sections: ChannelSection[] = [];
	for (const group of groups) {
		const inGroup = grouped.filter((c) => c.groupId === group.id);
		if (inGroup.length > 0) sections.push({ group, channels: inGroup });
	}
	const ungrouped = grouped.filter(
		(c) => c.groupId === undefined || !groups.some((g) => g.id === c.groupId),
	);
	if (ungrouped.length > 0)
		sections.push({ group: undefined, channels: ungrouped });
	return sections;
}

/** The DM + group-DM channels, fixture order preserved — the rail's DMs
 *  section. */
export function dmChannels(channels: readonly Channel[]): Channel[] {
	return channels.filter(isDm);
}

/** A DM's display label: the other participants' handles (excluding the caller),
 *  comma-joined. Falls back to the channel name when no other members resolve
 *  (so a malformed DM never renders blank). */
export function dmLabel(
	channel: Channel,
	callerId: string,
	byId: Map<string, Account>,
): string {
	const others = channel.memberAccountIds.filter((id) => id !== callerId);
	const handles = others.map((id) => handleOf(byId, id));
	return handles.length > 0 ? handles.join(", ") : channel.name;
}

/** The agent account observed by a channel, or undefined when the channel has no
 *  single agent to observe. A 1:1 DM whose other party is an agent resolves to
 *  that agent (the agent workspace: the agent's session
 *  trace shows beside the DM). A plain channel, a human↔human DM, or a group DM
 *  with more than one other party resolves to undefined — no single session to
 *  observe. */
export function agentDmAccountId(
	channel: Channel,
	callerId: string,
	byId: Map<string, Account>,
): string | undefined {
	if (channel.kind !== "dm") return undefined;
	const others = channel.memberAccountIds.filter((id) => id !== callerId);
	if (others.length !== 1) return undefined;
	const other = byId.get(others[0]);
	return other?.kind === "agent" ? other.id : undefined;
}

/** The DM channel whose single other party is `agentId` — the agent's own
 *  channel, the chat pane its workspace centers on (design compass-0.7
 *  T3). The inverse of `agentDmAccountId`: given an agent, find the channel to
 *  center its workspace on. Returns the first such DM (an agent has one home DM
 *  with the caller in the fixture), or undefined when the caller shares no DM
 *  with it. */
export function agentDmChannel(
	channels: readonly Channel[],
	agentId: string,
	callerId: string,
	byId: Map<string, Account>,
): Channel | undefined {
	return channels.find((c) => agentDmAccountId(c, callerId, byId) === agentId);
}

// ── Topics (the two-level Zulip conversation model) ──────────────────────────

/** A topic plus its messages, chronological — one thread of the channel's
 *  two-level model. The channel index renders one row per group; the topic view
 *  renders a group's `messages`. */
export interface TopicGroup {
	topic: Topic;
	messages: Message[];
}

/** The messages in one topic, chronological by post time then id (a stable
 *  tiebreak for equal timestamps). */
export function topicMessages(
	messages: readonly Message[],
	topicId: string,
): Message[] {
	return messages
		.filter((m) => m.topicId === topicId)
		.sort(
			(a, b) =>
				a.atUnixMs - b.atUnixMs || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0),
		);
}

/** The last-activity time of a topic group: its newest message's post time, or
 *  the topic's own creation time when it holds no messages (so a fresh, empty
 *  topic still sorts sensibly rather than sinking to the epoch). */
function lastActivityOf(group: TopicGroup): number {
	const last = group.messages[group.messages.length - 1];
	return last ? last.atUnixMs : group.topic.createdAtUnixMs;
}

/** Group a channel's topics into ordered TopicGroups: every ACTIVE topic in the
 *  channel (in `topics`, archived excluded), each carrying its own messages
 *  chronological, ordered by last activity DESCENDING (most-recently-active
 *  topic first). Archived topics are hidden from the index (matching the
 *  snapshot loader's listTopics{includeArchived:false}) but keep their messages,
 *  so a deep-link into an archived topic still renders in TopicView. The server
 *  guarantees every message has a topic, so there is NO client root-chasing and
 *  nothing is dropped: each message lands in exactly the group of its `topicId`.
 *  Ties break by topic id so the order is stable. Message order within
 *  `messages` need not be pre-sorted. */
export function topicsOf(
	topics: readonly Topic[],
	messages: readonly Message[],
	channelId: string,
): TopicGroup[] {
	const groups = topics
		.filter((t) => t.channelId === channelId && !t.archived)
		.map((topic) => ({ topic, messages: topicMessages(messages, topic.id) }));
	return groups.sort(
		(a, b) =>
			lastActivityOf(b) - lastActivityOf(a) ||
			(a.topic.id < b.topic.id ? -1 : a.topic.id > b.topic.id ? 1 : 0),
	);
}

/** A compact activity summary for a topic-index row: message count, the DISTINCT
 *  author ids in first-post order, and the last-activity time (0 when the topic
 *  is empty). One pass over the group's messages; pure — no store or component
 *  dependency. */
export interface TopicSummary {
	messageCount: number;
	participantIds: string[];
	lastActivityAtUnixMs: number;
}

export function topicSummary(group: TopicGroup): TopicSummary {
	const participantIds: string[] = [];
	const seen = new Set<string>();
	let lastActivityAtUnixMs = 0;
	for (const message of group.messages) {
		if (!seen.has(message.authorAccountId)) {
			seen.add(message.authorAccountId);
			participantIds.push(message.authorAccountId);
		}
		if (message.atUnixMs > lastActivityAtUnixMs) {
			lastActivityAtUnixMs = message.atUnixMs;
		}
	}
	return {
		messageCount: group.messages.length,
		participantIds,
		lastActivityAtUnixMs,
	};
}

// ── Mentions (composer + message rendering) ──────────────────────────────────

/** A parsed `@`-mention span within a message's text. `handle` is the token
 *  after the `@` (without it); `reserved` marks a reserved broadcast target
 *  (@everyone / @agents / @users). `start`/`end` are the span offsets in the
 *  source text so a renderer can chip exactly the matched run. */
export interface Mention {
	handle: string;
	reserved: boolean;
	start: number;
	end: number;
}

// An @-mention token: `@` then a run of handle characters. Handles are
// [a-z0-9._-] (matching account handles like "svc.compass" and "ci-build");
// the leading char must be a letter/digit so a bare "@" or "@." doesn't match.
const MENTION_RE = /@([a-z0-9][a-z0-9._-]*)/gi;

/** Parse all `@`-mentions out of a text block, in order of appearance. A
 *  reserved token (case-insensitive match against RESERVED_MENTIONS) is flagged
 *  `reserved`. Non-reserved tokens are returned regardless of whether they
 *  resolve to a known account — resolution is the caller's concern (an
 *  unresolved mention still chips, it just won't link). */
export function parseMentions(text: string): Mention[] {
	const out: Mention[] = [];
	for (const m of text.matchAll(MENTION_RE)) {
		const handle = m[1];
		const start = m.index ?? 0;
		out.push({
			handle,
			reserved: (RESERVED_MENTIONS as readonly string[]).includes(
				handle.toLowerCase(),
			),
			start,
			end: start + m[0].length,
		});
	}
	return out;
}

/** The plain text of a conversation block, or "" for a non-text block — the
 *  source a mention parse reads. */
export function blockText(block: ConvBlock): string {
	return block.kind === "text" ? block.text : "";
}
