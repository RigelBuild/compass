// The wire→domain adapter: generated @compass/client (compass.v1) messages →
// the UI's domain types (comms-stub / stub-data). The store's comms accessors
// stay the seam (store.ts:10-13); this is what their bodies read once the live
// SubscribeComms stream + read RPCs replace the in-memory fixture, so `comms.ts`
// (pure over the domain types) and every component render unchanged.
//
// Scope (T7, franklin-clear half): the STABLE entities — Account, ChannelGroup,
// Channel — plus the caller-relative `membership` derivation the domain type
// carries but the wire does not. Message/Ask mapping lands with the per-question
// Ask reshape (franklin-sea-1195-ask-in-channel-impl) so it is not built here
// against a shape about to change.
//
// Two structural gaps every mapper bridges:
//  - protobuf-es oneofs are `{case, value}` tagged unions; the domain uses a
//    discriminated `kind` string (Account) or a bare optional (Channel.groupId).
//  - protobuf-es enums are numeric (ChannelKind.CHANNEL = 0); the domain uses
//    string literals ("channel"). The maps below are total over the wire enum so
//    an unhandled value is a compile error, never a silent default.

import type {
	Account as WireAccount,
	Channel as WireChannel,
	ChannelGroup as WireChannelGroup,
} from "@compass/client";
import { ChannelGroupVisibility, ChannelKind } from "@compass/client";
import type {
	Channel,
	ChannelGroup,
	ChannelKind as DomainChannelKind,
	ChannelGroupVisibility as DomainVisibility,
	Membership,
} from "../comms-stub";
import type { Account } from "../stub-data";

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
 *  lifting the agent arm's `homeChannelId`/`ownerUserId` onto the flat domain
 *  shape (the domain models an agent's home DM + owner inline, not in a nested
 *  message). An unset `kind` oneof (`case: undefined`) is a malformed wire
 *  account; it maps to a `user` with no agent fields rather than throwing, so a
 *  single bad row never blanks the whole roster — the missing agent fields make
 *  it inert (no home DM, no ownership) rather than wrong. */
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
