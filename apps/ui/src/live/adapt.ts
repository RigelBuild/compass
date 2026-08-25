// The wire→domain adapter: generated @compass/client (compass.v1) messages →
// the UI's domain types (comms-stub / stub-data). The store's comms accessors
// stay the seam (store.ts:10-13); this is what their bodies read once the live
// SubscribeComms stream + read RPCs replace the in-memory fixture, so `comms.ts`
// (pure over the domain types) and every component render unchanged.
//
// Scope (T7, franklin-clear half): the STABLE entities — Account, ChannelGroup,
// Channel — plus the caller-relative `membership` derivation the domain type
// carries but the wire does not, and the durable Message/Ask mapping the live
// read path consumes through `MapMessage` (./comms-state).
//
// Two structural gaps every mapper bridges:
//  - protobuf-es oneofs are `{case, value}` tagged unions; the domain uses a
//    discriminated `kind` string (Account) or a bare optional (Channel.groupId).
//  - protobuf-es enums are numeric (ChannelKind.CHANNEL = 0); the domain uses
//    string literals ("channel"). The maps below are total over the wire enum so
//    an unhandled value is a compile error, never a silent default.

import {
	AgentPresence,
	ChannelGroupVisibility,
	ChannelKind,
	ChannelPostPolicy,
	ForgeProvider,
	IssueState,
	type Account as WireAccount,
	type AgentAttribution as WireAgentAttribution,
	type Ask as WireAsk,
	type AskQuestion as WireAskQuestion,
	type ChangedStats as WireChangedStats,
	type Channel as WireChannel,
	type ChannelGroup as WireChannelGroup,
	type Check as WireCheck,
	type ChecksSummary as WireChecksSummary,
	type ForgeRef as WireForgeRef,
	type Issue as WireIssue,
	type Message as WireMessage,
	type PinnedEntry as WirePinnedEntry,
	type PullRequest as WirePullRequest,
	type Review as WireReview,
	type ReviewThread as WireReviewThread,
	type RosterEntry as WireRosterEntry,
	type Topic as WireTopic,
	type TrackerRef as WireTrackerRef,
} from "@compass/client";
import type {
	Ask,
	AskQuestion,
	Channel,
	ChannelGroup,
	ConvBlock,
	ChannelKind as DomainChannelKind,
	ChannelPostPolicy as DomainPostPolicy,
	ChannelGroupVisibility as DomainVisibility,
	Membership,
	Message,
	PinnedEntry,
	Topic,
} from "../comms-stub";
import type {
	Account,
	AgentState,
	AgentAttribution as DomainAgentAttribution,
	ChangedStats as DomainChangedStats,
	Check as DomainCheck,
	ChecksSummary as DomainChecksSummary,
	ForgeProvider as DomainForgeProvider,
	ForgeRef as DomainForgeRef,
	Issue as DomainIssue,
	IssueState as DomainIssueState,
	Priority as DomainPriority,
	PullRequest as DomainPullRequest,
	Review as DomainReview,
	ReviewThread as DomainReviewThread,
	TrackerRef as DomainTrackerRef,
} from "../stub-data";
import type { MapMessage } from "./comms-state";

/** The wire `ChannelKind` enum → the domain's string-literal kind. Total over
 *  the enum: a `satisfies Record<ChannelKind, …>` makes a new wire variant a
 *  compile error here rather than an unmapped channel that renders wrong. */
const CHANNEL_KIND: Record<ChannelKind, DomainChannelKind> = {
	[ChannelKind.CHANNEL]: "channel",
	[ChannelKind.DM]: "dm",
	[ChannelKind.GROUP_DM]: "group_dm",
} satisfies Record<ChannelKind, DomainChannelKind>;

/** The wire `ChannelGroupVisibility` enum → the domain's string literal. Total,
 *  same rationale as CHANNEL_KIND. */
const GROUP_VISIBILITY: Record<ChannelGroupVisibility, DomainVisibility> = {
	[ChannelGroupVisibility.OWNER]: "owner",
	[ChannelGroupVisibility.SHARED]: "shared",
} satisfies Record<ChannelGroupVisibility, DomainVisibility>;

/** The wire `ChannelPostPolicy` enum → the domain's string literal. Total, same
 *  rationale as CHANNEL_KIND — a new wire policy is a compile error here, never
 *  a silently mis-gated composer. */
const POST_POLICY: Record<ChannelPostPolicy, DomainPostPolicy> = {
	[ChannelPostPolicy.OPEN]: "open",
	[ChannelPostPolicy.OWNER_ONLY]: "owner_only",
} satisfies Record<ChannelPostPolicy, DomainPostPolicy>;

/** Map a wire PinnedEntry to the domain one: the board pointer (message id +
 *  position) the strip renders. The wire's audit extras (`pinnedAtUnixMs`,
 *  `pinnedByAccountId`) are not in the domain contract — the strip renders the
 *  pinned MESSAGE, not who pinned it when — so they are dropped here rather than
 *  carried unrendered. */
function adaptPinnedEntry(w: WirePinnedEntry): PinnedEntry {
	return { messageId: w.messageId, position: w.position };
}

/** Map a wire Account to the domain Account, flattening the `kind` oneof. The
 *  agent arm lifts `homeChannelId`/`ownerUserId`/`parentAgentId` onto the flat
 *  domain shape (the domain models an agent's home DM + owner inline, not in a
 *  nested message); the system arm (the reserved `@compass` platform sender)
 *  carries only the base identity. An unset `kind` oneof (`case: undefined`) is
 *  a malformed wire account; it maps to a `user` with no agent fields rather
 *  than throwing, so a single bad row never blanks the whole roster — the
 *  missing agent fields make it inert (no home DM, no ownership) rather than
 *  wrong. */
export function adaptAccount(w: WireAccount): Account {
	const base = {
		id: w.id,
		handle: w.handle,
		displayName: w.displayName,
	};
	if (w.kind.case === "agent") {
		return {
			...base,
			kind: "agent",
			ownerUserId: w.kind.value.ownerUserId,
			// The wire always sets home_channel_id on an agent (server-minted at
			// CreateAgent); an empty string is nonetheless normalized to undefined
			// so the domain's "absent = no home DM" contract holds.
			homeChannelId: w.kind.value.homeChannelId || undefined,
			// The wire encodes "root" (no parent agent) as an empty string;
			// normalize it to undefined so the domain's "absent = a root" contract
			// holds and agentTree derives it as top-level.
			parentAgentId: w.kind.value.parentAgentId || undefined,
		};
	}
	if (w.kind.case === "system") {
		return { ...base, kind: "system" };
	}
	return { ...base, kind: "user" };
}

/** Map a wire ChannelGroup to the domain ChannelGroup: normalize the wire's
 *  empty-string "unset" convention for the optional parent/owner ids to the
 *  domain's absent (undefined), and map the visibility enum. */
export function adaptChannelGroup(w: WireChannelGroup): ChannelGroup {
	return {
		id: w.id,
		name: w.name,
		parentGroupId: w.parentGroupId || undefined,
		ownerUserId: w.ownerUserId || undefined,
		visibility: GROUP_VISIBILITY[w.visibility],
	};
}

/** The caller's membership in a channel, DERIVED from the wire's account lists
 *  (the wire carries no per-caller membership enum — the domain's join/subscribe
 *  model is a UI projection over member_account_ids + subscriber_account_ids,
 *  comms.proto Channel):
 *   - in subscriber_account_ids  → "subscribed" (joined + pushed at turn-end)
 *   - in member_account_ids only → "joined" (read access, no push)
 *   - in neither                 → "none" (visible but not joined; the rail
 *                                   excludes it, a join affordance shows)
 *  A subscriber is by definition a member (the server enforces "subscribe only a
 *  current/added member"), so the subscriber check is sufficient for the top
 *  tier without also re-checking membership. */
export function deriveMembership(w: WireChannel, callerId: string): Membership {
	if (w.subscriberAccountIds.includes(callerId)) return "subscribed";
	if (w.memberAccountIds.includes(callerId)) return "joined";
	return "none";
}

/** Whether the caller's subscription to a channel is IMPLICIT and non-togglable
 *  — true only for an agent's own home channel (comms.proto AgentAccount: "the
 *  agent is always subscribed to it, implicit, not a togglable row"). The domain
 *  renders the subscribe control fixed/disabled when true. Derived by matching
 *  the channel id against the set of agent home-channel ids (built once by the
 *  caller from the account set), so the always-subscribed flag can't drift from
 *  the accounts that define it. */
export function adaptChannel(
	w: WireChannel,
	callerId: string,
	agentHomeChannelIds: ReadonlySet<string>,
): Channel {
	const membership = deriveMembership(w, callerId);
	return {
		id: w.id,
		name: w.name,
		groupId: w.groupId || undefined,
		kind: CHANNEL_KIND[w.kind],
		memberAccountIds: w.memberAccountIds,
		membership,
		// A home channel the caller owns is implicitly, non-togglably subscribed.
		// Only meaningful when the caller is actually subscribed; a `none`/joined
		// channel is never flagged always-subscribed.
		alwaysSubscribed:
			membership === "subscribed" && agentHomeChannelIds.has(w.id)
				? true
				: undefined,
		postPolicy: POST_POLICY[w.postPolicy],
		// Empty-string "unset" convention (matching groupId): an unowned channel
		// carries "" on the wire, absent in the domain.
		ownerAccountId: w.ownerAccountId || undefined,
		// Absent when false so the domain's optional flag reads "no mandatory
		// subscription" as absence, matching the fixture shape.
		mandatorySubscription: w.mandatorySubscription || undefined,
		// Ordered by position at the render seam (pinnedMessages); mapped verbatim
		// here. Absent when empty so an unpinned channel carries no board.
		pinnedEntries:
			w.pinnedEntries.length > 0
				? w.pinnedEntries.map(adaptPinnedEntry)
				: undefined,
	};
}

/** The set of agent home-channel ids from a domain account list — the input to
 *  `adaptChannel`'s always-subscribed derivation. Built once per snapshot so the
 *  per-channel map stays O(1). */
export function agentHomeChannelIds(
	accounts: readonly Account[],
): ReadonlySet<string> {
	const ids = new Set<string>();
	for (const a of accounts) {
		if (a.kind === "agent" && a.homeChannelId) ids.add(a.homeChannelId);
	}
	return ids;
}

/** Map a wire AskQuestion to the domain one, preserving option order. The wire's
 *  presentation/audit extras (header, recommended, customText, timedOut) are not
 *  in the domain contract and are deliberately dropped here rather than carried
 *  half-rendered. The option `description` follows the file's empty-string→absent
 *  convention. */
function adaptAskQuestion(w: WireAskQuestion): AskQuestion {
	return {
		questionId: w.questionId,
		question: w.question,
		options: w.options.map((o) => ({
			id: o.id,
			label: o.label,
			description: o.description || undefined,
		})),
		allowMultiple: w.allowMultiple,
		chosenOptionIds: w.chosenOptionIds,
	};
}

/** Map a wire Ask to the domain Ask: the correlation id plus every question in
 *  ask order (comms.proto Ask — one ask_id per Ask, questions keyed inside by
 *  question_id, so order is the only positional contract and is preserved),
 *  plus the server's `answered` flag. That flag is SERVER-OWNED and read-only to
 *  the client: the server flips it on the first RespondToAsk it accepted and
 *  DROPS it on anything inbound, so it is only ever read here, never asserted
 *  (comms.proto Ask.answered). */
export function adaptAsk(w: WireAsk): Ask {
	return {
		askId: w.askId,
		questions: w.questions.map(adaptAskQuestion),
		answered: w.answered,
	};
}

/** Map a wire MessageBlock's oneof to a durable domain block, or `undefined` for
 *  any other/unset case. The domain narrows the proto oneof to the two DURABLE
 *  conversation kinds (comms-stub.ts:143-147): the rich ACP blocks
 *  (thought/tool_call/plan/diff) are execution trace that renders in the session
 *  observation panel, not the conversation, so a non-durable case is DROPPED —
 *  not mapped to a placeholder (which would render as a phantom message body)
 *  and not thrown on (which would blank the whole channel over one block). */
function adaptBlock(w: WireMessage["blocks"][number]): ConvBlock | undefined {
	switch (w.block.case) {
		case "text":
			return { kind: "text", text: w.block.value };
		case "ask":
			return { kind: "ask", ask: adaptAsk(w.block.value) };
		default:
			return undefined;
	}
}

/** Map a wire Topic to the domain Topic: the verbatim scalars plus the int64
 *  `created_at_unix_ms` bigint → a JS `number` (explicit — implicit coercion on
 *  a bigint throws). All fields are required on the wire, so there is no
 *  empty-string→absent seam here. */
export function adaptTopic(w: WireTopic): Topic {
	return {
		id: w.id,
		channelId: w.channelId,
		name: w.name,
		createdAtUnixMs: Number(w.createdAtUnixMs),
		createdByAccountId: w.createdByAccountId,
		archived: w.archived,
	};
}

/** Map a wire Message to the domain Message. Two bridges beyond the verbatim
 *  scalars:
 *   - `topic_id` is the message's sole containment (the two-level Zulip model —
 *     a topic is scoped to one channel, and the message's channel is the topic's
 *     channel); copied verbatim, no oneof flattening.
 *   - `at_unix_ms` is an int64 → a JS `bigint` on the wire; the domain wants a
 *     `number`, so convert EXPLICITLY (implicit coercion on a bigint throws). */
function adaptWireMessage(w: WireMessage): Message {
	return {
		id: w.id,
		topicId: w.topicId,
		authorAccountId: w.authorAccountId,
		atUnixMs: Number(w.atUnixMs),
		blocks: w.blocks
			.map(adaptBlock)
			.filter((b): b is ConvBlock => b !== undefined),
	};
}

/** The injected wire→domain message mapper the live reducer consumes
 *  (./comms-state MapMessage). The reducer's parameter is `unknown` on purpose —
 *  it never names the block shape — so the narrowing happens ONCE, here at the
 *  boundary, and the body stays typed against the generated wire message. */
export const adaptMessage: MapMessage = (wire) =>
	adaptWireMessage(wire as WireMessage);

/** The wire `IssueState` numeric enum → the domain's string-literal lifecycle.
 *  Total over the enum: a `satisfies Record<IssueState, DomainIssueState>` makes
 *  a new wire variant a compile error here rather than a silently mis-mapped
 *  card. `UNSPECIFIED` is the proto zero — a malformed/absent state on the wire;
 *  it degrades to `"backlog"` (the earliest lifecycle stage) so one bad row
 *  lands on the board inertly rather than blanking it. */
const ISSUE_STATE: Record<IssueState, DomainIssueState> = {
	[IssueState.UNSPECIFIED]: "backlog",
	[IssueState.BACKLOG]: "backlog",
	[IssueState.TODO]: "todo",
	[IssueState.QUEUED]: "queued",
	[IssueState.BLOCKED]: "blocked",
	[IssueState.IN_PROGRESS]: "in_progress",
	[IssueState.IN_REVIEW]: "in_review",
	[IssueState.DONE]: "done",
	[IssueState.ARCHIVED]: "archived",
} satisfies Record<IssueState, DomainIssueState>;

/** The wire `ForgeProvider` numeric enum → the domain's string-literal provider.
 *  Total, same rationale as ISSUE_STATE. `UNSPECIFIED` degrades to `"github"`
 *  (the default forge) so a malformed forge ref renders inertly. */
const FORGE_PROVIDER: Record<ForgeProvider, DomainForgeProvider> = {
	[ForgeProvider.UNSPECIFIED]: "github",
	[ForgeProvider.GITHUB]: "github",
	[ForgeProvider.GITLAB]: "gitlab",
	[ForgeProvider.FORGEJO]: "forgejo",
	[ForgeProvider.LINEAR]: "linear",
} satisfies Record<ForgeProvider, DomainForgeProvider>;

/** Map a wire ForgeRef to the domain one, mapping the provider enum. An unset
 *  wire `forge` (optional on Issue/PullRequest) has no domain home — both domain
 *  types require `forge` — so a missing ref degrades to a github/empty-host ref
 *  rather than throwing, keeping a malformed row inert on the board. */
function adaptForgeRef(w: WireForgeRef | undefined): DomainForgeRef {
	return {
		provider: w ? FORGE_PROVIDER[w.provider] : "github",
		host: w?.host ?? "",
	};
}

/** Map a wire AgentAttribution to the domain one. The current wire shape carries
 *  only `agentHandle` (DL-094 burned owner_handle/verified as reserved); the
 *  domain's `ownerHandle`/`verified` have no wire source, so they take honest
 *  hedged defaults (`""` / `false` — an unverified claim with no owner) rather
 *  than a fabricated value. */
function adaptAgentAttribution(
	w: WireAgentAttribution,
): DomainAgentAttribution {
	return { agentHandle: w.agentHandle, ownerHandle: "", verified: false };
}

/** Map a wire ChangedStats to the domain diffstat — verbatim scalars. */
function adaptChangedStats(w: WireChangedStats): DomainChangedStats {
	return { files: w.files, additions: w.additions, deletions: w.deletions };
}

/** Map a wire Check to the domain one. The wire `state` is a free string; the
 *  domain narrows it to the 6-valued forge vocabulary — cast at the boundary
 *  (the server emits exactly that vocabulary; comms_pb.ts:1703). */
function adaptCheck(w: WireCheck): DomainCheck {
	return {
		name: w.name,
		state: w.state as DomainCheck["state"],
		url: w.url,
		required: w.required,
	};
}

/** Map a wire ChecksSummary to the domain roll-up, narrowing the roll-up state
 *  string to the 3-valued domain union and mapping each check. */
function adaptChecksSummary(w: WireChecksSummary): DomainChecksSummary {
	return {
		headSha: w.headSha,
		state: w.state as DomainChecksSummary["state"],
		checks: w.checks.map(adaptCheck),
	};
}

/** Map a wire Review to the domain one, narrowing the forge `verdict` string to
 *  the domain union. Structurally identical otherwise. */
function adaptReview(w: WireReview): DomainReview {
	return {
		author: w.author,
		isBot: w.isBot,
		verdict: w.verdict as DomainReview["verdict"],
		body: w.body,
	};
}

/** Map a wire ReviewThread to the domain one. The nested `comments` are
 *  structurally identical (author/isBot/body), copied verbatim. */
function adaptReviewThread(w: WireReviewThread): DomainReviewThread {
	return {
		path: w.path,
		resolved: w.resolved,
		comments: w.comments.map((c) => ({
			author: c.author,
			isBot: c.isBot,
			body: c.body,
		})),
	};
}

/** Map a wire TrackerRef to the domain one, narrowing `kind` to the tracker
 *  union. All fields verbatim otherwise. */
function adaptTrackerRef(w: WireTrackerRef): DomainTrackerRef {
	return {
		kind: w.kind as DomainTrackerRef["kind"],
		id: w.id,
		status: w.status,
		url: w.url,
	};
}

/** Map a wire PullRequest to the domain PullRequest: the verbatim scalars, the
 *  provider/forge ref, the optional agent attribution, the optional diffstat and
 *  checks roll-up (absent wire message → absent domain field), and the review /
 *  thread lists. `forgeState` is a forge-truth string narrowed to the domain
 *  union at the boundary (comms_pb.ts:1587). */
export function adaptPullRequest(w: WirePullRequest): DomainPullRequest {
	return {
		forge: adaptForgeRef(w.forge),
		repo: w.repo,
		number: w.number,
		title: w.title,
		forgeState: w.forgeState as DomainPullRequest["forgeState"],
		url: w.url,
		headRef: w.headRef,
		baseRef: w.baseRef,
		agent: w.agent ? adaptAgentAttribution(w.agent) : undefined,
		forgeAccount: w.forgeAccount,
		draft: w.draft,
		changed: w.changed ? adaptChangedStats(w.changed) : undefined,
		checks: w.checks ? adaptChecksSummary(w.checks) : undefined,
		reviews: w.reviews.map(adaptReview),
		threads: w.threads.map(adaptReviewThread),
	};
}

/** Map a wire Issue to the domain Issue: the verbatim scalars, the provider /
 *  forge ref, the `state` lifecycle enum (total ISSUE_STATE map), the `priority`
 *  and `forgeState` forge-truth strings narrowed to their domain unions, the
 *  optional agent attribution, the empty-string→null `assignee` seam (the wire
 *  encodes "unassigned" as an empty string; the domain uses null), the nested
 *  PRs, and the optional tracker ref. Pure and total. */
export function adaptIssue(w: WireIssue): DomainIssue {
	return {
		id: w.id,
		forge: adaptForgeRef(w.forge),
		repo: w.repo,
		number: w.number,
		title: w.title,
		body: w.body,
		forgeState: w.forgeState as DomainIssue["forgeState"],
		url: w.url,
		agent: w.agent ? adaptAgentAttribution(w.agent) : undefined,
		forgeAccount: w.forgeAccount,
		labels: w.labels,
		state: ISSUE_STATE[w.state],
		priority: w.priority as DomainPriority,
		assignee: w.assignee || null,
		summary: w.summary,
		branch: w.branch,
		prs: w.prs.map(adaptPullRequest),
		tracker: w.tracker ? adaptTrackerRef(w.tracker) : undefined,
	};
}

/** The wire `AgentPresence` enum → the domain's `AgentState` dot state. Total
 *  over the 4-state MVP enum (DL-194): `WORKING → "working"`, `IDLE → "idle"`,
 *  `WAITING → "waiting"`, `OFFLINE → "stopped"`. `OFFLINE → "stopped"` is a
 *  ruled decision (R2): the server defaults every agent absent from its
 *  in-memory presence source to `OFFLINE`, so `OFFLINE` covers both
 *  "deliberately stopped" and "never started" — the enum cannot split them
 *  client-side, and the "stopped" hollow-ring dot is the honest render.
 *  `UNSPECIFIED → undefined` is a defensive arm only: unreachable on the
 *  `GetRoster` path (which never emits UNSPECIFIED), it lets a caller fall back
 *  to its own default rather than paint a wrong dot. The default arm throws on
 *  an unmodeled numeric (the `agent-state.ts:71-78` exhaustiveness convention):
 *  the enum is proto3-open, so a version-skewed server could send a variant the
 *  `never` check can't catch at compile time — throw rather than return a raw
 *  enum that would break a downstream `Record<AgentState>` lookup. */
export function presenceLifecycle(p: AgentPresence): AgentState | undefined {
	switch (p) {
		case AgentPresence.WORKING:
			return "working";
		case AgentPresence.IDLE:
			return "idle";
		case AgentPresence.WAITING:
			return "waiting";
		case AgentPresence.OFFLINE:
			return "stopped";
		case AgentPresence.UNSPECIFIED:
			return undefined;
		default: {
			const _exhaustive: never = p;
			throw new Error(`Unhandled AgentPresence: ${_exhaustive}`);
		}
	}
}

/** The domain presence value — the ephemeral join the store layers onto an
 *  agent's durable account identity (DL-193). Both fields optional: `lifecycle`
 *  is absent when presence is `UNSPECIFIED` (the caller falls back to its own
 *  default dot), and `activity` is absent when the wire carries no note. NEVER a
 *  wire shape — the adapt seam maps `AgentPresence`/`RosterEntry` into this. */
export interface AgentPresenceInfo {
	readonly lifecycle?: AgentState;
	readonly activity?: string;
}

/** Map one wire `RosterEntry` to its presence-map entry: `[agentAccountId,
 *  info]`. The join carries ONLY the ephemeral presence — `lifecycle` (via
 *  `presenceLifecycle`) and the human-readable `activity` note (empty-string
 *  normalized to undefined, matching the `Agent.activity` contract
 *  `stub-data.ts:350-351`). The identity fields the wire also carries (`handle`,
 *  `displayName`, `parentAgentId`) are deliberately DROPPED: accounts own
 *  identity (Approach fork 1 / R1), and a second identity source here could
 *  drift from the live `accountChanged` stream. */
export function adaptRosterEntry(
	w: WireRosterEntry,
): [string, AgentPresenceInfo] {
	return [
		w.agentAccountId,
		{
			lifecycle: presenceLifecycle(w.presence),
			activity: w.activity || undefined,
		},
	];
}
