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

import type {
	Account as WireAccount,
	Ask as WireAsk,
	AskQuestion as WireAskQuestion,
	Channel as WireChannel,
	ChannelGroup as WireChannelGroup,
	Message as WireMessage,
	Topic as WireTopic,
} from "@compass/client";
import { ChannelGroupVisibility, ChannelKind } from "@compass/client";
import type {
	Ask,
	AskQuestion,
	Channel,
	ChannelGroup,
	ConvBlock,
	ChannelKind as DomainChannelKind,
	ChannelGroupVisibility as DomainVisibility,
	Membership,
	Message,
	Topic,
} from "../comms-stub";
import type { Account } from "../stub-data";
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

/** Map a wire Account to the domain Account, flattening the `kind` oneof and
 *  lifting the agent arm's `homeChannelId`/`ownerUserId`/`parentAgentId` onto
 *  the flat domain shape (the domain models an agent's home DM + owner inline,
 *  not in a nested message). An unset `kind` oneof (`case: undefined`) is a
 *  malformed wire account; it maps to a `user` with no agent fields rather than
 *  throwing, so a single bad row never blanks the whole roster — the missing
 *  agent fields make it inert (no home DM, no ownership) rather than wrong. */
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
