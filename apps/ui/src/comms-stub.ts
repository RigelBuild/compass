// Dev-only stub data for the Compass UI.
//
// Compass is a Discord/Slack-style multi-agent product: humans and agents
// talk in channels + DMs, and — once inside an agent workspace — that channel
// chat is the workspace's primary pane, with the agent's execution trace a
// secondary observation companion beside it (design: architecture-lineage). Humans
// and agents are first-class accounts; channels nest in channel groups
// (a user's space, e.g. group "matt" -> channel "coordination").
//
// This module mirrors the compass.v1 comms contract
// (proto/compass/v1/comms.proto): Account / ChannelGroup / Channel / Message /
// MessageBlock / Ask. It is the walking-skeleton fixture — the shell renders it
// through the comms store with no daemon, and when SubscribeComms lands this
// module is deleted and the components read the generated @compass/client
// instead (the shapes below intentionally mirror that eventual contract).
//
// Reframe deltas vs the raw proto, each annotated at its seam:
//  - the durable conversation block set narrows to `text` + `ask`. The rich ACP
//    blocks (thought/tool_call/plan/diff) do NOT appear in the channel — they
//    live in the session observation panel, rendered by OMP's own renderer over
//    opaque session frames.
//  - threading is TWO-LEVEL (Zulip model): a channel holds named `Topic`s, and
//    each `Message` belongs to exactly one topic (`Message.topicId`). There is
//    no per-message parent — a topic IS the thread. (comms.proto Message.topic_id
//    + Topic; field 2 reused as topic_id, parent_message_id removed.)
//  - per-channel membership (joined / subscribed) is a still-in-design contract
//    carrier — modeled here as a UI-side field. SEAM: expect the field name to
//    settle.

// ── Accounts (comms.proto Account) ───────────────────────────────────────────

// The account identity model is owned by stub-data.ts (the roster source of
// truth); the comms layer reads the same `Account` so a channel member id and an
// agent's account id are one id space. Re-exported so comms consumers keep
// importing `Account` from here (their comms seam) without reaching across.
export type { Account } from "./stub-data";

import type { Account } from "./stub-data";
import { STUB_AGENTS } from "./stub-data";

// ── Channel groups + channels (comms.proto ChannelGroup / Channel) ───────────

/** Group-level visibility (comms.proto ChannelGroupVisibility). Owner-scoped is
 *  the default (private to the owning user + its agents); shared is open to all. */
export type ChannelGroupVisibility = "owner" | "shared";

/** A channel group: a namespace node holding channels and nested groups, so a
 *  user's agents work in that user's space by default (comms.proto ChannelGroup). */
export interface ChannelGroup {
	/** Server-assigned stable id. */
	id: string;
	/** Leaf segment of the namespace, e.g. "matt". */
	name: string;
	/** Parent group; absent for a top-level group. */
	parentGroupId?: string;
	/** The user whose space this group is; absent for a shared/global group. */
	ownerUserId?: string;
	visibility: ChannelGroupVisibility;
}

/** A channel's kind (comms.proto ChannelKind). A plain channel is the default;
 *  DMs are direct conversations. An agent's ACP surface is NOT a channel — it is
 *  the agent workspace (the session observation panel), a separate surface. */
export type ChannelKind = "channel" | "dm" | "group_dm";

/** Per-channel membership state — the still-in-design join/subscribe model
 *  (brief: "join = can read; subscribe = new messages pushed at turn-end;
 *  always-subscribed to own channel"). SEAM: a UI-side field until the contract
 *  carrier settles.
 *  - `none`     — not joined (can't read; a discover/join affordance shows).
 *  - `joined`   — joined (reads history) but not pushed new messages live.
 *  - `subscribed` — joined and pushed new messages at turn-end. */
export type Membership = "none" | "joined" | "subscribed";

/** A channel's post policy (comms.proto ChannelPostPolicy). `open` — any member
 *  may post (the zero value, today's behavior). `owner_only` — only the
 *  channel's `ownerAccountId` may post; the coordination channel is provisioned
 *  this way, a one-way directive surface (design compass-manager-comms-substrate
 *  §A2). The composer renders disabled with an "owner-only channel" hint when
 *  the caller fails the policy. */
export type ChannelPostPolicy = "open" | "owner_only";

/** One entry on a channel's pinned board (comms.proto PinnedEntry): an EXISTING
 *  message promoted to the board at a position. Pinning mints no message — the
 *  `messageId` already lives in a topic of the channel, so the strip resolves it
 *  against the loaded message set. The board is authored by the channel owner
 *  (agent-managed, Matt's ruling) and rides the `ChannelChanged` event; the
 *  human client renders it, never edits it. Think of it as a permanent thread
 *  headline on a channel whose messages otherwise live only in topics. */
export interface PinnedEntry {
	/** The pinned message id; already exists in a topic of this channel. */
	messageId: string;
	/** Ordinal position on the board (0-based, ascending). */
	position: number;
}

/** A channel — a named conversation within a group (comms.proto Channel). */
export interface Channel {
	/** Server-assigned stable id. */
	id: string;
	/** Leaf name within the group, e.g. "coordination". */
	name: string;
	/** The channel group this channel belongs to; absent for an ungrouped
	 *  channel (owner-scoped to its creator). */
	groupId?: string;
	kind: ChannelKind;
	/** The accounts party to the channel (comms.proto member_account_ids). */
	memberAccountIds: string[];
	/** Optional one-line topic shown in the channel header. */
	topic?: string;
	/** SEAM (still-in-design): the caller's membership in this channel. A `none`
	 *  channel is NOT a rail row — it's the joinable/discover state (the caller
	 *  can see it exists but hasn't joined); the rail shows joined+subscribed
	 *  only. */
	membership: Membership;
	/** Whether the caller's subscription here is IMPLICIT and non-togglable
	 *  (design.md:416 — always-subscribed-to-own is "implicit, not a togglable
	 *  row"). When true the subscribe control renders fixed/disabled: the model
	 *  says the caller can't unsubscribe, so the UI must not offer to. Absent =
	 *  a normal togglable subscription. */
	alwaysSubscribed?: boolean;
	/** Unread message count for the rail badge (a UI projection; the real count
	 *  derives from the caller's last-read cursor vs the message stream). */
	unread?: number;
	/** Who may post here (comms.proto post_policy). `open` — any member; the
	 *  composer is gated only on membership. `owner_only` — only `ownerAccountId`
	 *  may post, and the composer renders disabled with an "owner-only channel"
	 *  hint for everyone else. Defaults to `open` (the wire zero value). */
	postPolicy: ChannelPostPolicy;
	/** The channel's owner/operator account for policy purposes (comms.proto
	 *  owner_account_id); absent when unowned. On an `owner_only` channel this is
	 *  the sole account permitted to post. */
	ownerAccountId?: string;
	/** When true, membership implies a subscription that cannot be toggled off
	 *  (comms.proto mandatory_subscription — the coordination-channel class). The
	 *  subscribe toggle is hidden entirely: the model says every member is
	 *  force-subscribed, so offering an unsubscribe control would be a lie. */
	mandatorySubscription?: boolean;
	/** The channel's pinned board, ordered by `position` (comms.proto
	 *  pinned_entries). A pure pointer set over existing messages; the strip in
	 *  the channel header resolves each `messageId` against the loaded messages.
	 *  Empty/absent = no board. Rides the `ChannelChanged` event, so a pin edit
	 *  re-renders the strip with no refetch. */
	pinnedEntries?: PinnedEntry[];
}

// ── Messages + content blocks (comms.proto Message / MessageBlock / Ask) ──────

/** One selectable answer to an AskQuestion (comms.proto AskOption). */
export interface AskOption {
	id: string;
	label: string;
	/** Optional explanatory text shown under the label. */
	description?: string;
}

/** One question within an Ask, carrying its own options and answer state
 *  (comms.proto AskQuestion). */
export interface AskQuestion {
	/** Unique, non-empty within the Ask; the key a RespondToAsk answer addresses
	 *  (comms.proto AskQuestion.question_id). */
	questionId: string;
	question: string;
	options: AskOption[];
	/** Whether more than one option may be chosen. */
	allowMultiple: boolean;
	/** The chosen option ids once answered; empty while pending (kept for audit). */
	chosenOptionIds: string[];
}

/** A structured question set an agent asks: one or more questions, each with its
 *  own selectable options (comms.proto Ask). NOT a permission prompt — permission
 *  gating is a separate, deferred concern, intentionally absent from the contract.
 *  Rendered inline in the channel as an async question (answerable via
 *  RespondToAsk), never a blocking modal. */
export interface Ask {
	/** Correlation id echoed by RespondToAsk; one per Ask, NOT per question —
	 *  questions are keyed inside by AskQuestion.questionId (comms.proto Ask.ask_id). */
	askId: string;
	/** The questions, in ask order. At least one (comms.proto Ask.questions). */
	questions: AskQuestion[];
	/** The server's own record that this ask is SPENT: it flips exactly once, on
	 *  the first RespondToAsk the server accepted, and every later respond is
	 *  refused. This is the ONLY reliable answered-signal — a fully-skipped ask
	 *  and a `custom_text`-only answer both leave every question's
	 *  `chosenOptionIds` empty, so scanning the questions cannot tell a CLOSED
	 *  ask from a pending one (comms.proto Ask.answered). */
	answered: boolean;
}

/** A durable content block inside a channel message. The comms model
 *  narrows the proto's MessageBlock oneof to the two durable conversation kinds:
 *  `text` (settled markdown, may carry @-mentions) and `ask` (an inline async
 *  question). The rich ACP blocks (thought/tool_call/plan/diff) are NOT part of
 *  the conversation — they render in the session observation panel. */
export type ConvBlock =
	| { kind: "text"; text: string }
	| { kind: "ask"; ask: Ask };

/** A named topic within a channel — the thread of the two-level Zulip model
 *  (comms.proto Topic). A channel holds many topics; each message belongs to
 *  exactly one. */
export interface Topic {
	/** Server-assigned stable id (survives rename). */
	id: string;
	/** The channel this topic lives in (comms.proto Topic.channel_id). */
	channelId: string;
	/** Display name, unique (case-insensitive) within the channel. */
	name: string;
	/** Creation time, ms since epoch (comms.proto Topic.created_at_unix_ms). */
	createdAtUnixMs: number;
	/** The account that first addressed the topic (created it). */
	createdByAccountId: string;
	/** Archived topics are hidden from the default index but keep their messages. */
	archived: boolean;
}

/** A message in a channel — the persisted unit of the comms layer (comms.proto
 *  Message). Scoped to exactly one topic; the message's channel is the topic's
 *  channel. */
export interface Message {
	/** Server-assigned stable id. */
	id: string;
	/** The topic this message belongs to (comms.proto Message.topic_id). */
	topicId: string;
	/** The posting account (a user or an agent). */
	authorAccountId: string;
	/** Post time, ms since epoch (comms.proto Message.at_unix_ms). */
	atUnixMs: number;
	/** Ordered durable content (text + ask only). */
	blocks: ConvBlock[];
}

// ── Mentions (brief: composer @-mentions + reserved pings) ────────────────────

/** Reserved broadcast mention targets (brief: @agents / @users / @everyone).
 *  SEAM: the exact reserved set + resolution is still-in-design; the composer
 *  mention UI is built generically over this list plus the account handles. */
export const RESERVED_MENTIONS = ["everyone", "agents", "users"] as const;
export type ReservedMention = (typeof RESERVED_MENTIONS)[number];

// ── Fixture data ─────────────────────────────────────────────────────────────
//
// A representative multi-agent wave, drawn from a real Compass session so it
// reads true: a human owner (matt) with owned agents, a shared announcements
// channel, the owner's coordination + service channels, and DMs.

const MATT = "acc-matt";

/** The human owner — the one user account, the caller. */
const MATT_ACCOUNT: Account = {
	id: MATT,
	handle: "matt",
	displayName: "Matt",
	kind: "user",
};

/** The reserved platform system sender `@compass` (SEA-1820). A third account
 *  kind alongside the caller and agents — never a roster/directory entry (it is
 *  not a STUB_AGENTS row, so it never enters the agent tree), but it IS a comms
 *  account so its posts render with the system-author style and an `@compass`
 *  mention resolves as a known account (not an unknown handle). The server keeps
 *  it visible only to the co-members of channels it posts into; the fixture
 *  models that by carrying it in the account set the message surfaces resolve.
 *
 *  DEFERRED (no autocomplete surface exists today — the composer is a plain
 *  input): when a composer @-mention autocomplete is built, it must filter
 *  `kind: "system"` accounts out of its suggestion source (`store.accounts()`)
 *  so `@compass` resolves once typed but is never OFFERED as a completion,
 *  mirroring the server's directory/roster exclusion. */
const COMPASS_ACCOUNT: Account = {
	id: "acc-sys-compass",
	handle: "compass",
	displayName: "Compass",
	kind: "system",
};

// The accounts are DERIVED from the roster (stub-data STUB_AGENTS) plus the
// caller: one account per agent, in roster order, the SAME object the roster
// owns (referential identity, not a parallel copy that can drift). The system
// sender is appended — a comms account, but not a roster agent.
export const STUB_ACCOUNTS: Account[] = [
	MATT_ACCOUNT,
	...STUB_AGENTS.map((a) => a.account),
	COMPASS_ACCOUNT,
];

export const STUB_CHANNEL_GROUPS: ChannelGroup[] = [
	{ id: "grp-shared", name: "shared", visibility: "shared" },
	{ id: "grp-matt", name: "matt", ownerUserId: MATT, visibility: "owner" },
];

// The surviving agent id-space — derived from the roster (stub-data) so a
// roster change flows through without a hand-edit here.
const AGENT_IDS = STUB_AGENTS.map((a) => a.account.id);
const EVERYONE = [MATT, ...AGENT_IDS];

// One home DM per board agent — the chat pane its workspace centers on
// (design: architecture-lineage). Its id is cached on `account.homeChannelId` (stub-data)
// so the chat pane resolves it O(1) with no per-render `agentDmChannel` search.
const AGENT_HOME_DMS: Channel[] = STUB_AGENTS.map((a) => ({
	id: a.account.homeChannelId ?? `dm-${a.account.handle}`,
	name: a.account.handle,
	kind: "dm",
	memberAccountIds: [MATT, a.account.id],
	membership: "subscribed",
	postPolicy: "open",
}));

export const STUB_CHANNELS: Channel[] = [
	{
		id: "ch-announcements",
		name: "announcements",
		groupId: "grp-shared",
		kind: "channel",
		memberAccountIds: EVERYONE,
		topic: "Fleet-wide posture, standing directives, CI gotchas.",
		membership: "subscribed",
		alwaysSubscribed: true,
		unread: 2,
		// A one-way directive surface: the supervisor owns it and is the only
		// poster, every member is force-subscribed, and it carries a pinned board
		// (the standing posture headlines) the human client renders read-only.
		postPolicy: "owner_only",
		ownerAccountId: "acc-supervisor",
		mandatorySubscription: true,
		pinnedEntries: [
			{ messageId: "msg-a1", position: 0 },
			{ messageId: "msg-a2", position: 1 },
		],
	},
	{
		id: "ch-coordination",
		name: "coordination",
		groupId: "grp-matt",
		kind: "channel",
		memberAccountIds: EVERYONE,
		topic: "Active hand-off + routing across the wave.",
		membership: "subscribed",
		// Owner-only + mandatory too, but owned by the caller (Matt) — so the
		// composer is ENABLED here, the contrast that proves the gate keys on the
		// owner id, not merely on the owner_only policy.
		postPolicy: "owner_only",
		ownerAccountId: MATT,
		mandatorySubscription: true,
	},
	{
		id: "ch-svc-compass",
		name: "svc.compass",
		groupId: "grp-matt",
		kind: "channel",
		memberAccountIds: [
			MATT,
			"acc-compass-server",
			"acc-compass-ui",
			"acc-compass-comms",
		],
		topic: "The compass service lane — seam reviews, contract rulings.",
		membership: "subscribed",
		unread: 5,
		postPolicy: "open",
	},
	{
		id: "ch-svc-ci-build",
		name: "svc.ci-build",
		groupId: "grp-matt",
		kind: "channel",
		memberAccountIds: EVERYONE,
		topic: "CI image + pipeline ownership.",
		membership: "joined",
		postPolicy: "open",
	},
	// A channel the caller can see but has not joined — exercises the discover /
	// join affordance (membership seam).
	{
		id: "ch-random",
		name: "random",
		groupId: "grp-shared",
		kind: "channel",
		memberAccountIds: [MATT, "acc-supervisor"],
		topic: "Off-topic.",
		membership: "none",
		postPolicy: "open",
	},
	{
		id: "dm-ui-server",
		name: "compass-ui, compass-server",
		kind: "group_dm",
		memberAccountIds: [MATT, "acc-compass-ui", "acc-compass-server"],
		membership: "subscribed",
		postPolicy: "open",
	},
	// The per-agent home DMs (one 1:1 DM per board agent) — the 1:1 surviving-
	// roster DMs that replace the pre-reshape hand-listed set.
	...AGENT_HOME_DMS,
];

// A fixed clock base so the fixture reads with plausible relative times without
// being wall-clock dependent (the formatter renders atUnixMs as UTC HH:MM).
const T0 = Date.UTC(2026, 6, 18, 17, 0, 0);
const min = (m: number): number => T0 + m * 60_000;

// The channel's named topics — the two-level Zulip model's thread layer. Each
// message below carries a `topicId` into one of these; the channel index renders
// them ordered by last activity. `ch-svc-compass` carries TWO topics so the
// index shows a real multi-topic channel; DMs carry a single home topic (a DM is
// a flat conversation, one implicit topic).
export const STUB_TOPICS: Topic[] = [
	{
		id: "top-ann-posture",
		channelId: "ch-announcements",
		name: "posture",
		createdAtUnixMs: min(1),
		createdByAccountId: "acc-supervisor",
		archived: false,
	},
	{
		id: "top-compass-acp",
		channelId: "ch-svc-compass",
		name: "ACP seam review",
		createdAtUnixMs: min(9),
		createdByAccountId: "acc-compass-ui",
		archived: false,
	},
	{
		id: "top-compass-integration",
		channelId: "ch-svc-compass",
		name: "integration CI",
		createdAtUnixMs: min(30),
		createdByAccountId: "acc-compass-server",
		archived: false,
	},
	{
		id: "top-dm-compass-server",
		channelId: "dm-compass-server",
		name: "general",
		createdAtUnixMs: min(14),
		createdByAccountId: MATT,
		archived: false,
	},
	{
		id: "top-dm-compass-ui",
		channelId: "dm-compass-ui",
		name: "general",
		createdAtUnixMs: min(49),
		createdByAccountId: "acc-compass-ui",
		archived: false,
	},
	{
		id: "top-dm-supervisor",
		channelId: "dm-supervisor",
		name: "general",
		createdAtUnixMs: min(59),
		createdByAccountId: "acc-supervisor",
		archived: false,
	},
];

export const STUB_MESSAGES: Message[] = [
	// ── #announcements / posture ──
	{
		id: "msg-a1",
		topicId: "top-ann-posture",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(2),
		blocks: [
			{
				kind: "text",
				text: "REVIEW POLICY (Matt-ruled): the three review subagents (correctness, contract, quality) are the sole review of record on every PR. No external reviewer gates a merge. @everyone",
			},
		],
	},
	{
		id: "msg-a2",
		topicId: "top-ann-posture",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(41),
		blocks: [
			{
				kind: "text",
				text: "CI COLD-PULL RE-FIRE: a 0-byte/empty-log ~340s bounce is the expected cold-pull dice, not your diff. Just re-fire your own pipeline. @agents",
			},
		],
	},

	// ── #svc.compass / ACP seam review (a multi-message topic exchange) ──
	{
		id: "msg-c1",
		topicId: "top-compass-acp",
		authorAccountId: "acc-compass-ui",
		atUnixMs: min(10),
		blocks: [
			{
				kind: "text",
				text: "@compass the ACP session pane fix-head is up for your seam pass at 527ec845a — reconnect cursor + streaming-frame coverage + the empty-trace guard.",
			},
		],
	},
	{
		id: "msg-c2",
		topicId: "top-compass-acp",
		authorAccountId: "acc-compass-server",
		atUnixMs: min(24),
		blocks: [
			{
				kind: "text",
				text: "Seam pass DONE, gate GREEN at 527ec845a. Verified with teeth: scope-clean, session-reload unit test, CI-exhaustiveness proven to bite both ways. Nothing owed back.",
			},
		],
	},
	{
		id: "msg-c3",
		topicId: "top-compass-acp",
		authorAccountId: "acc-compass-ui",
		atUnixMs: min(27),
		blocks: [
			{
				kind: "text",
				text: "Thanks — that's the last review surface on the pane. Holding the lane at Matt's merge gate.",
			},
		],
	},

	// ── #svc.compass / integration CI (an ask topic) ──
	{
		id: "msg-c4",
		topicId: "top-compass-integration",
		authorAccountId: "acc-compass-server",
		atUnixMs: min(33),
		blocks: [
			{
				kind: "ask",
				ask: {
					askId: "ask-s4-integration",
					questions: [
						{
							questionId: "q1",
							question:
								"Q2 (live-daemon integration CI) — new sub-issue, or fold into SEA-1023?",
							allowMultiple: false,
							chosenOptionIds: [],
							options: [
								{
									id: "opt-new",
									label: "New sub-issue",
									description:
										"Test-infra is orthogonal to the ACP lane; tracks the ci-build dep cleaner.",
								},
								{
									id: "opt-fold",
									label: "Fold into SEA-1023",
									description: "Keep it under the one ACP session lane.",
								},
							],
						},
					],
					answered: false,
				},
			},
		],
	},

	// ── DM: matt <-> compass-server ──
	{
		id: "msg-dm1",
		topicId: "top-dm-compass-server",
		authorAccountId: "acc-compass-server",
		atUnixMs: min(15),
		blocks: [
			{
				kind: "text",
				text: "The session-reload contract for #1023 is nearly firm — I'll ping compass-comms when the membership carrier settles.",
			},
		],
	},
	{
		id: "msg-dm2",
		topicId: "top-dm-compass-server",
		authorAccountId: MATT,
		atUnixMs: min(18),
		blocks: [
			{ kind: "text", text: "Good. Keep the join-vs-subscribe seam soft." },
		],
	},

	// ── DM: matt <-> compass-ui ──
	{
		id: "msg-dm-f1",
		topicId: "top-dm-compass-ui",
		authorAccountId: "acc-compass-ui",
		atUnixMs: min(50),
		blocks: [
			{
				kind: "text",
				text: "Comms-in-workspace shell is up — board primary, channels/DMs in the left rail, chat folded into the agent workspace. Want to walk the layout?",
			},
		],
	},
	{
		id: "msg-dm-f2",
		topicId: "top-dm-compass-ui",
		authorAccountId: "acc-compass-ui",
		atUnixMs: min(52),
		blocks: [
			{
				kind: "ask",
				ask: {
					askId: "ask-ui-layout",
					questions: [
						{
							questionId: "q1",
							question:
								"Board demotion — keep the top-bar Bridge tab, or move it into the left rail?",
							allowMultiple: false,
							chosenOptionIds: [],
							options: [
								{
									id: "opt-topbar",
									label: "Keep top-bar tab",
									description: "Board stays one click away from any surface.",
								},
								{
									id: "opt-rail",
									label: "Move to left rail",
									description: "Frees the top bar for the channel surface.",
								},
							],
						},
					],
					answered: false,
				},
			},
		],
	},

	// ── DM: matt <-> supervisor ──
	{
		id: "msg-dm-sup1",
		topicId: "top-dm-supervisor",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(60),
		blocks: [
			{
				kind: "text",
				text: "Fleet snapshot: compass-ui + compass-server in review, compass-native waiting on a merge gate. No collisions on the compass-ui zone right now.",
			},
		],
	},
	{
		id: "msg-dm-sup2",
		topicId: "top-dm-supervisor",
		authorAccountId: MATT,
		atUnixMs: min(63),
		blocks: [
			{
				kind: "text",
				text: "Good. Keep everyone in-lane until their PR merges.",
			},
		],
	},
	{
		id: "msg-dm-sup3",
		topicId: "top-dm-supervisor",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(65),
		blocks: [
			{
				kind: "text",
				text: "Understood — holding the board and re-driving any bounce.",
			},
		],
	},
	{
		id: "msg-dm-sup4",
		topicId: "top-dm-supervisor",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(67),
		blocks: [
			{
				kind: "ask",
				ask: {
					askId: "ask-sup-lane",
					questions: [
						{
							questionId: "q1",
							question:
								"compass-native's gate just cleared — put it on the flaky-CI lane next, or pull it onto the compass-ui review backlog?",
							allowMultiple: false,
							chosenOptionIds: [],
							options: [
								{
									id: "opt-flaky-ci",
									label: "Flaky-CI lane",
									description:
										"Stabilize the intermittent suite before it blocks more merges.",
								},
								{
									id: "opt-review-backlog",
									label: "Compass-ui review backlog",
									description:
										"Clear the stacked review queue while the zone is quiet.",
								},
							],
						},
					],
					answered: false,
				},
			},
		],
	},
];

/** The fixture as a `CommsState` — the shape `createAppStore` takes as
 *  `initialComms` when constructed OFFLINE (no comms client). The store no
 *  longer boots from this: it is the tests' explicit seed for the surfaces whose
 *  behavior is asserted over a populated conversation. Typed structurally (not
 *  by importing ./live/comms-state) so the fixture module stays free of the live
 *  layer; the store's parameter type checks the fit at every call site. */
export const STUB_COMMS_STATE = {
	accounts: STUB_ACCOUNTS,
	channelGroups: STUB_CHANNEL_GROUPS,
	channels: STUB_CHANNELS,
	topics: STUB_TOPICS,
	messages: STUB_MESSAGES,
	presence: new Map(),
};
