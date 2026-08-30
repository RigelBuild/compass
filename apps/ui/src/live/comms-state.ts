// The live comms state reducer: the pure core that turns a SubscribeComms event
// stream into the four domain collections the store's comms accessors expose
// (accounts, channelGroups, channels, messages). Pure over its inputs — no
// Solid, no client, no network — so the snapshot-apply, the per-event
// transitions, and the dedup-by-id invariant are all unit-testable in isolation;
// the store (live/store wiring) drives it with the real stream and mirrors the
// reduced state into signals.
//
// Message mapping is INJECTED (`MapMessage`), not imported: the durable-message
// shape (text + the per-question Ask reshape landing in
// franklin-sea-1195-ask-in-channel-impl) is a moving contract, so this engine
// stays agnostic to it — it dedups and orders domain Messages by id/time and
// never names a block shape. Account/Channel/ChannelGroup mapping is stable and
// consumed directly from ./adapt.

import type {
	Account as WireAccount,
	Channel as WireChannel,
	ChannelGroup as WireChannelGroup,
	RosterEntry as WireRosterEntry,
	Topic as WireTopic,
} from "@compass/client";
import type {
	Account,
	Channel,
	ChannelGroup,
	Message,
	Topic,
} from "../comms-stub";
import {
	type AgentPresenceInfo,
	adaptAccount,
	adaptChannel,
	adaptChannelGroup,
	adaptRosterEntry,
	adaptTopic,
	agentHomeChannelIds,
} from "./adapt";

/** Maps a wire Message to the domain Message. Injected so this engine stays
 *  independent of the (moving) durable-message/Ask block shape — franklin's
 *  per-question reshape changes only the mapper, never the reducer. */
export type MapMessage = (wire: unknown) => Message;

/** The reduced comms state — exactly the four domain collections the store's
 *  comms accessors surface. Pure value: every transition returns a fresh object
 *  (structural sharing where a collection is untouched), so the store sets
 *  signals directly and Solid sees a new reference only for what changed.
 *
 *  The stream cursor (`since_seq`) and instance epoch are deliberately NOT here:
 *  they are transport bookkeeping the driver owns (see ./stream), not domain
 *  state the store consumes. Keeping them out is what keeps the read-RPC
 *  snapshot token and the stream tail cursor two SEPARATE counters — the client
 *  never conflates the point-in-time read boundary with the live tail position,
 *  so it stays gap-free whether the snapshot boundary resolves as bus-space or
 *  store-space (RIG-1333 amendment to the T2 contract). */
export interface CommsState {
	readonly accounts: readonly Account[];
	readonly channelGroups: readonly ChannelGroup[];
	readonly channels: readonly Channel[];
	readonly topics: readonly Topic[];
	readonly messages: readonly Message[];
	readonly presence: ReadonlyMap<string, AgentPresenceInfo>;
}

/** The empty state — the reducer's identity, before any snapshot or event. */
export const EMPTY_COMMS_STATE: CommsState = {
	accounts: [],
	channelGroups: [],
	channels: [],
	topics: [],
	messages: [],
	presence: new Map(),
};

/** A raw snapshot from the read RPCs (ListAccounts/ListChannelGroups/
 *  ListChannels/ListTopics/ListMessages) taken at one snapshot boundary — the
 *  since_seq=0 recovery path and the resync re-snapshot both produce this, then
 *  it is reduced into a fresh CommsState. Messages arrive already domain-mapped
 *  (the driver applies the injected MapMessage) so this module needs no message
 *  shape; topics arrive wire-typed (reduceSnapshot adapts them, like channels).
 *  The boundary token is NOT here: it is the opaque read-RPC cursor the driver
 *  passes verbatim to each list call, never reduced into domain state. */
export interface CommsSnapshot {
	readonly accounts: readonly WireAccount[];
	readonly channelGroups: readonly WireChannelGroup[];
	readonly channels: readonly WireChannel[];
	readonly topics: readonly WireTopic[];
	readonly messages: readonly Message[];
	readonly roster: readonly WireRosterEntry[];
}

/** Reduce a raw snapshot into a fresh CommsState. Accounts map first because
 *  channel adaptation derives `alwaysSubscribed` from the agent home-channel ids
 *  the accounts define; messages are pre-mapped by the driver. Ordering of the
 *  collections mirrors the read RPCs (server order); messages are sorted by the
 *  same (atUnixMs, id) key the pure comms core threads on so the tail's inserts
 *  land consistently. */
export function reduceSnapshot(
	callerId: string,
	snap: CommsSnapshot,
): CommsState {
	const accounts = snap.accounts.map(adaptAccount);
	const homeIds = agentHomeChannelIds(accounts);
	const channels = snap.channels.map((c) => adaptChannel(c, callerId, homeIds));
	const channelGroups = snap.channelGroups.map(adaptChannelGroup);
	const topics = snap.topics.map(adaptTopic);
	const messages = [...snap.messages].sort(byPostOrder);
	const presence = new Map(snap.roster.map(adaptRosterEntry));
	return { accounts, channelGroups, channels, topics, messages, presence };
}

/** Chronological by post time, then id — the stable tiebreak the pure comms
 *  core (`topicMessages`) uses, kept identical here so the live message list
 *  and the topic read agree on order. */
function byPostOrder(a: Message, b: Message): number {
	if (a.atUnixMs !== b.atUnixMs) return a.atUnixMs - b.atUnixMs;
	return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/** Upsert an item by `id` into a list, preserving order: an existing id is
 *  replaced in place (dedup — a MessageUpdated or a re-delivered MessagePosted
 *  never duplicates a row), a new id is appended. Returns the same array
 *  reference when the item is identical by reference (no-op churn avoided). */
function upsertById<T extends { id: string }>(
	list: readonly T[],
	item: T,
): readonly T[] {
	const i = list.findIndex((x) => x.id === item.id);
	if (i === -1) return [...list, item];
	if (list[i] === item) return list;
	const next = list.slice();
	next[i] = item;
	return next;
}

/** Remove an item by `id` from a list, preserving the order of the rest.
 *  Returns the same array reference when no row matches (no-op churn avoided). */
function removeById<T extends { id: string }>(
	list: readonly T[],
	id: string,
): readonly T[] {
	const i = list.findIndex((x) => x.id === id);
	if (i === -1) return list;
	const next = list.slice();
	next.splice(i, 1);
	return next;
}

/** Insert-or-replace a message by id, keeping the list in (atUnixMs, id) order.
 *  A MessagePosted for a known id (at-least-once redelivery) replaces rather
 *  than duplicates — the dedup-by-id contract (design T7 resync: "dedup by
 *  message id"). A MessageUpdated replaces the row's blocks in place. */
function upsertMessage(
	list: readonly Message[],
	msg: Message,
): readonly Message[] {
	const i = list.findIndex((m) => m.id === msg.id);
	if (i === -1) {
		// New message: splice into sorted position rather than append+resort.
		const next = list.slice();
		let lo = 0;
		while (lo < next.length && byPostOrder(next[lo], msg) <= 0) lo++;
		next.splice(lo, 0, msg);
		return next;
	}
	if (list[i] === msg) return list;
	const next = list.slice();
	if (next[i].atUnixMs === msg.atUnixMs) {
		// Same post time — the server never rewrites at_unix_ms on an update or a
		// redelivery (a block/text update touches blocks and text only), so an
		// in-place content replace preserves (atUnixMs, id) order.
		next[i] = msg;
		return next;
	}
	// Defensive: a changed at_unix_ms would move the row, so remove and re-splice
	// into sorted position — the ordering invariant holds regardless of contract.
	next.splice(i, 1);
	let lo = 0;
	while (lo < next.length && byPostOrder(next[lo], msg) <= 0) lo++;
	next.splice(lo, 0, msg);
	return next;
}

/** A single already-decoded stream event applied to the state. The driver
 *  decodes the wire `SubscribeCommsResponse` oneof into one of these tagged
 *  events (mapping message payloads through the injected MapMessage first) so
 *  this reducer is pure over domain types and testable without the generated
 *  stream type. The resync signal is not an event here — the driver handles it
 *  by re-snapshotting, which replaces the whole state via reduceSnapshot. */
export type CommsEvent =
	| { readonly kind: "messagePosted"; readonly message: Message }
	| { readonly kind: "messageUpdated"; readonly message: Message }
	| { readonly kind: "channelChanged"; readonly channel: Channel }
	| { readonly kind: "channelGroupChanged"; readonly group: ChannelGroup }
	| { readonly kind: "accountChanged"; readonly account: Account }
	| { readonly kind: "topicUpserted"; readonly topic: Topic }
	| { readonly kind: "channelRemoved"; readonly channelId: string }
	| {
			readonly kind: "presenceChanged";
			readonly accountId: string;
			readonly info: AgentPresenceInfo;
	  };

/** Apply one decoded event to the state, returning the next state. Pure: upserts
 *  the entity by id (dedup — a redelivered post or an update never duplicates a
 *  row) into its collection, or drops it (channelRemoved — the caller's own
 *  ejection), leaving the others untouched. The stream cursor is not advanced
 *  here — that is the driver's transport bookkeeping, kept out of the reduced
 *  domain state (see ./stream). */
export function applyEvent(state: CommsState, event: CommsEvent): CommsState {
	switch (event.kind) {
		case "messagePosted":
		case "messageUpdated":
			return {
				...state,
				messages: upsertMessage(state.messages, event.message),
			};
		case "channelChanged":
			return { ...state, channels: upsertById(state.channels, event.channel) };
		case "channelGroupChanged":
			return {
				...state,
				channelGroups: upsertById(state.channelGroups, event.group),
			};
		// accountChanged updates only `accounts`; it does not re-derive existing
		// channels' `alwaysSubscribed` (an adapt-time projection over the agent
		// home-channel set). This is sound because home_channel_id is server-set
		// and immutable (comms.proto AgentAccount, minted once at CreateAgent — no
		// update RPC), so an existing home channel's always-subscribed flag can
		// never go stale, and there is no account-removed event. The one residual
		// is a NEW agent whose channelChanged(home) decodes before its
		// accountChanged: that channel's flag stays unset until the next
		// channelChanged/resync — a self-healing cosmetic transient (the cold
		// subscribe via reduceSnapshot is always correct). Keeping this a pure
		// upsert avoids coupling the reducer to the adapt-layer projection.
		case "accountChanged":
			return { ...state, accounts: upsertById(state.accounts, event.account) };
		// A topic created/renamed/merged/archived: upsert by id, keeping the topic
		// index live without a refetch. Order within `topics` is server order; the
		// pure `topicsOf` re-sorts by last activity, so append-on-new is fine.
		case "topicUpserted":
			return { ...state, topics: upsertById(state.topics, event.topic) };
		case "channelRemoved":
			return {
				...state,
				channels: removeById(state.channels, event.channelId),
			};
		// A presence delta: upsert one agent's ephemeral presence into a FRESH
		// Map (clone-then-set), never mutating the existing map — the
		// "every transition returns a fresh object" contract, with the untouched
		// collections keeping identity (structural sharing).
		case "presenceChanged": {
			const presence = new Map(state.presence);
			presence.set(event.accountId, event.info);
			return { ...state, presence };
		}
	}
}
