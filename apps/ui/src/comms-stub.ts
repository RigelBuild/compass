// Dev-only stub data for the Compass UI.
//
// Compass is a Discord/Slack-style multi-agent product: humans and agents
// talk in channels + DMs, and — once inside an agent workspace — that channel
// chat is the workspace's primary pane, with the agent's execution trace a
// secondary observation companion beside it (design compass-0.7). Humans
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
//  - threading is carried by `parentMessageId` (the channel-model amendment
//    compass is folding into the contract — not yet in the frozen proto). SEAM:
//    expect the field to settle in the proto.
//  - per-channel membership (joined / subscribed) is a still-in-design contract
//    carrier — modeled here as a UI-side field. SEAM: expect the field name to
//    settle.
//  - `Message.container` is channel-only here; the proto's `workspace_id`
//    container arm is being dropped (channel-only containment).

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
}

/** A durable content block inside a channel message. The comms model
 *  narrows the proto's MessageBlock oneof to the two durable conversation kinds:
 *  `text` (settled markdown, may carry @-mentions) and `ask` (an inline async
 *  question). The rich ACP blocks (thought/tool_call/plan/diff) are NOT part of
 *  the conversation — they render in the session observation panel. */
export type ConvBlock =
	| { kind: "text"; text: string }
	| { kind: "ask"; ask: Ask };

/** A message in a channel — the persisted unit of the comms layer (comms.proto
 *  Message, container narrowed to channel-only). */
export interface Message {
	/** Server-assigned stable id. */
	id: string;
	/** The channel this message belongs to (comms.proto Message.channel_id). */
	channelId: string;
	/** The posting account (a user or an agent). */
	authorAccountId: string;
	/** Post time, ms since epoch (comms.proto Message.at_unix_ms). */
	atUnixMs: number;
	/** SEAM (channel-model amendment): the message this one replies to, forming a
	 *  thread; absent for a top-level message. */
	parentMessageId?: string;
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

// The accounts are DERIVED from the roster (stub-data STUB_AGENTS) plus the
// caller: one account per agent, in roster order, the SAME object the roster
// owns (referential identity, not a parallel copy that can drift).
export const STUB_ACCOUNTS: Account[] = [
	MATT_ACCOUNT,
	...STUB_AGENTS.map((a) => a.account),
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
// (design compass-0.7). Its id is cached on `account.homeChannelId` (stub-data)
// so the chat pane resolves it O(1) with no per-render `agentDmChannel` search.
const AGENT_HOME_DMS: Channel[] = STUB_AGENTS.map((a) => ({
	id: a.account.homeChannelId ?? `dm-${a.account.handle}`,
	name: a.account.handle,
	kind: "dm",
	memberAccountIds: [MATT, a.account.id],
	membership: "subscribed",
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
	},
	{
		id: "ch-coordination",
		name: "coordination",
		groupId: "grp-matt",
		kind: "channel",
		memberAccountIds: EVERYONE,
		topic: "Active hand-off + routing across the wave.",
		membership: "subscribed",
	},
	{
		id: "ch-svc-compass",
		name: "svc.compass",
		groupId: "grp-matt",
		kind: "channel",
		memberAccountIds: [MATT, "acc-livingstone", "acc-cook", "acc-ross"],
		topic: "The compass service lane — seam reviews, contract rulings.",
		membership: "subscribed",
		unread: 5,
	},
	{
		id: "ch-svc-ci-build",
		name: "svc.ci-build",
		groupId: "grp-matt",
		kind: "channel",
		memberAccountIds: EVERYONE,
		topic: "CI image + pipeline ownership.",
		membership: "joined",
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
	},
	{
		id: "dm-cook-ross",
		name: "cook, ross",
		kind: "group_dm",
		memberAccountIds: [MATT, "acc-cook", "acc-ross"],
		membership: "subscribed",
	},
	// The per-agent home DMs (one 1:1 DM per board agent) — the 1:1 surviving-
	// roster DMs that replace the pre-reshape hand-listed set.
	...AGENT_HOME_DMS,
];

// A fixed clock base so the fixture reads with plausible relative times without
// being wall-clock dependent (the formatter renders atUnixMs as UTC HH:MM).
const T0 = Date.UTC(2026, 6, 18, 17, 0, 0);
const min = (m: number): number => T0 + m * 60_000;

export const STUB_MESSAGES: Message[] = [
	// ── #announcements ──
	{
		id: "msg-a1",
		channelId: "ch-announcements",
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
		channelId: "ch-announcements",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(41),
		blocks: [
			{
				kind: "text",
				text: "CI COLD-PULL RE-FIRE: a 0-byte/empty-log ~340s bounce is the expected cold-pull dice, not your diff. Just re-fire your own pipeline. @agents",
			},
		],
	},

	// ── #svc.compass (a threaded exchange) ──
	{
		id: "msg-c1",
		channelId: "ch-svc-compass",
		authorAccountId: "acc-cook",
		atUnixMs: min(10),
		blocks: [
			{
				kind: "text",
				text: "@compass T3a #727 fix-head is up for your seam pass at 527ec845a — sumtype fix + streaming door coverage + the classification omission guard.",
			},
		],
	},
	{
		id: "msg-c2",
		channelId: "ch-svc-compass",
		authorAccountId: "acc-livingstone",
		atUnixMs: min(24),
		parentMessageId: "msg-c1",
		blocks: [
			{
				kind: "text",
				text: "Seam pass DONE, gate GREEN at 527ec845a. Verified with teeth: scope-clean, admin-gate interceptor unit test, CI-exhaustiveness proven to bite both ways. Nothing owed back.",
			},
		],
	},
	{
		id: "msg-c3",
		channelId: "ch-svc-compass",
		authorAccountId: "acc-cook",
		atUnixMs: min(27),
		parentMessageId: "msg-c1",
		blocks: [
			{
				kind: "text",
				text: "Thanks — that's the last review surface on T3a. Holding the lane at Matt's merge gate.",
			},
		],
	},
	{
		id: "msg-c4",
		channelId: "ch-svc-compass",
		authorAccountId: "acc-livingstone",
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
								"Q2 (live-NATS integration CI) — new sub-issue, or fold into SEA-1243?",
							allowMultiple: false,
							chosenOptionIds: [],
							options: [
								{
									id: "opt-new",
									label: "New sub-issue",
									description:
										"Test-infra is orthogonal to the port lane; tracks the ci-build dep cleaner.",
								},
								{
									id: "opt-fold",
									label: "Fold into SEA-1243",
									description: "Keep it under the one comms/runner port lane.",
								},
							],
						},
					],
				},
			},
		],
	},

	// ── DM: matt <-> livingstone ──
	{
		id: "msg-dm1",
		channelId: "dm-livingstone",
		authorAccountId: "acc-livingstone",
		atUnixMs: min(15),
		blocks: [
			{
				kind: "text",
				text: "The channel-model amendment for #767 is nearly firm — I'll ping franklin when the membership contract carrier settles.",
			},
		],
	},
	{
		id: "msg-dm2",
		channelId: "dm-livingstone",
		authorAccountId: MATT,
		atUnixMs: min(18),
		blocks: [
			{ kind: "text", text: "Good. Keep the join-vs-subscribe seam soft." },
		],
	},

	// ── DM: matt <-> cook ──
	{
		id: "msg-dm-f1",
		channelId: "dm-cook",
		authorAccountId: "acc-cook",
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
		channelId: "dm-cook",
		authorAccountId: "acc-cook",
		atUnixMs: min(52),
		blocks: [
			{
				kind: "ask",
				ask: {
					askId: "ask-cook-layout",
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
				},
			},
		],
	},

	// ── DM: matt <-> supervisor ──
	{
		id: "msg-dm-sup1",
		channelId: "dm-supervisor",
		authorAccountId: "acc-supervisor",
		atUnixMs: min(60),
		blocks: [
			{
				kind: "text",
				text: "Fleet snapshot: cook + livingstone in review, cousteau waiting on a merge gate, warden holding the sandbox lane. No collisions on the compass-ui zone right now.",
			},
		],
	},
	{
		id: "msg-dm-sup2",
		channelId: "dm-supervisor",
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
		channelId: "dm-supervisor",
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
		channelId: "dm-supervisor",
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
								"cousteau's gate just cleared — put him on the flaky-CI lane next, or pull him onto the compass-ui review backlog?",
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
				},
			},
		],
	},

	// ── DM: matt <-> warden ──
	{
		id: "msg-dm-war1",
		channelId: "dm-warden",
		authorAccountId: "acc-warden",
		atUnixMs: min(70),
		blocks: [
			{
				kind: "text",
				text: "Sandbox policy check: every worker is confined to its own clone under ~/agents/workspaces. No writes to main, allowlisted owners only.",
			},
		],
	},
	{
		id: "msg-dm-war2",
		channelId: "dm-warden",
		authorAccountId: MATT,
		atUnixMs: min(73),
		blocks: [
			{ kind: "text", text: "Flag anything that reaches outside its lane." },
		],
	},
	{
		id: "msg-dm-war3",
		channelId: "dm-warden",
		authorAccountId: "acc-warden",
		atUnixMs: min(75),
		blocks: [
			{
				kind: "text",
				text: "Will do — no boundary crossings observed this cycle.",
			},
		],
	},
];
