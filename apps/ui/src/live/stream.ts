// The SubscribeComms stream driver: the orchestrator that turns the live comms
// subscription into a sequence of CommsState values, applying the
// snapshot+tail+resync protocol as TWO SEPARATE CURSORS:
//
//   1. subscribe(since_seq = last stream seq; 0 on a cold start) — the server
//      streams from that stream position. The FIRST response carries a
//      `snapshot_seq` boundary token + the `instance_epoch`.
//   2. snapshot state via the read RPCs, each passing that `snapshot_seq`
//      VERBATIM — an opaque read boundary token, never interpreted, never
//      compared to a stream seq.
//   3. tail the live stream, applying each response's payload; the tail cursor
//      advances from `SubscribeCommsResponse.seq` (the stream's own counter),
//      NOT from snapshot_seq.
//   4. message-id dedup (comms-state upsert) absorbs the overlap between the
//      snapshot and the buffered tail — subscribing before the snapshot read
//      means the live stream already covers everything the snapshot does.
//
//   A `resync_required` (or a cursor the server can't serve gap-free) resets
//   both cursors and re-snapshots from scratch; a fresh `instance_epoch` is a
//   server restart and self-heals the same way.
//
// The two cursors are kept apart on PURPOSE. `snapshot_seq` (the read-RPC
// boundary) and the stream `seq` (the tail cursor) are two counters doing two
// jobs, and the frozen design line "tail from snapshot_seq + 1" conflated them.
// Under the SEA-1333 amendment the snapshot boundary may resolve as store-space
// (durable BIGSERIAL) while the stream seq is bus-space (resets per boot) — the
// two are incomparable, so any arithmetic across them silently drops rows after
// a restart. This driver treats snapshot_seq as an opaque token and tails from
// the stream seq, so it is gap-free under either fork resolution with zero
// change here.
//
// This module owns the I/O and control flow; the pure reduction lives in
// ./comms-state (applyEvent/reduceSnapshot) and the wire→domain mapping in
// ./adapt. Message mapping is INJECTED (`MapMessage`) so the driver never names
// the durable-message/Ask block shape (franklin's per-question reshape) — the
// franklin-independent seam. The store wires this to its signals later
// (createAppStore injection, deferred behind franklin's landed store.ts).

import type { CommsClient, SubscribeCommsResponse } from "@compass/client";
import { RosterScope } from "@compass/client";
import {
	type AgentPresenceInfo,
	adaptAccount,
	adaptChannel,
	adaptChannelGroup,
	adaptTopic,
	agentHomeChannelIds,
	presenceLifecycle,
} from "./adapt";
import {
	applyEvent,
	type CommsEvent,
	type CommsSnapshot,
	type CommsState,
	EMPTY_COMMS_STATE,
	type MapMessage,
	reduceSnapshot,
} from "./comms-state";

/** Per-channel message page size for the snapshot read. The server clamps to its
 *  own maximum; this is the client's request. Pagination loops until a short
 *  page, so a channel with more than one page still loads fully. */
const SNAPSHOT_MESSAGE_PAGE = 200;

/** What the driver needs to run: the comms client, who the caller is (scopes
 *  membership derivation), the injected message mapper, and the sink for each
 *  new reduced state. `signal` cancels the whole run (component unmount / app
 *  teardown); `onError` observes a non-fatal stream error before the driver
 *  reconnects (fatal = aborted). */
export interface CommsStreamOptions {
	readonly client: CommsClient;
	readonly callerId: string;
	readonly mapMessage: MapMessage;
	/** Invoked with each new immutable CommsState: once after the snapshot, then
	 *  once per applied tail event. The store sets its signals from this. */
	readonly onState: (state: CommsState) => void;
	readonly signal?: AbortSignal;
	/** Observed on a stream error before reconnect; the run continues (retries
	 *  the subscribe). Not called on a clean abort. */
	readonly onError?: (error: unknown) => void;
}

/** Read a consistent point-in-time snapshot via the four read RPCs, mapping
 *  messages through the injected `mapMessage`. `snapshotSeq` is the OPAQUE
 *  boundary token from the subscribe response, passed verbatim to each read RPC
 *  so every page reads one point-in-time view — the driver never interprets it
 *  or compares it to a stream seq. Accounts/groups/channels stay wire-typed
 *  (reduceSnapshot adapts them); messages are mapped here because per-channel
 *  paging is where the wire messages surface. */
export async function fetchSnapshot(
	client: CommsClient,
	mapMessage: MapMessage,
	snapshotSeq: bigint,
	signal?: AbortSignal,
): Promise<CommsSnapshot> {
	const opts = { signal };
	// GetRoster joins the SAME failure domain as the other snapshot reads — NOT
	// best-effort. It carries no agent_account_id: the server defaults the
	// vantage to the caller and resolves a user caller to its own owned set (R6).
	// GetRosterRequest carries only scope + agent_account_id (no snapshotSeq), so
	// the presence seed is unversioned and converges via the seq'd tail replay.
	// A rejection propagates (throws) exactly like the sibling reads, is caught
	// in runCommsStream, and retries the whole snapshot with backoff — never a
	// swallow that would leave presence permanently empty.
	const [accountsResp, groupsResp, channelsResp, rosterResp] =
		await Promise.all([
			client.listAccounts({ snapshotSeq }, opts),
			client.listChannelGroups({ snapshotSeq }, opts),
			client.listChannels({ snapshotSeq }, opts),
			client.getRoster({ scope: RosterScope.OWNER }, opts),
		]);

	// Topics + messages page per channel. Topics list per channel (ListTopics is
	// channel-scoped); messages page the whole channel (ListMessages topic filter
	// empty), so the store's flat `messages()` accessor stays the whole visible
	// list. (Lazy per-topic load would change the accessor contract; out of
	// scope.)
	const [perChannelTopics, perChannelMessages] = await Promise.all([
		Promise.all(
			channelsResp.channels.map((channel) =>
				client
					.listTopics({ channelId: channel.id, includeArchived: false }, opts)
					.then((resp) => resp.topics),
			),
		),
		Promise.all(
			channelsResp.channels.map((channel) =>
				fetchChannelMessages(client, channel.id, snapshotSeq, signal),
			),
		),
	]);
	const topics = perChannelTopics.flat();
	const messages = perChannelMessages.flat().map(mapMessage);

	return {
		accounts: accountsResp.accounts,
		channelGroups: groupsResp.groups,
		channels: channelsResp.channels,
		topics,
		messages,
		roster: rosterResp.entries,
	};
}

/** Page one channel's messages fully at the snapshot boundary. ListMessages is
 *  newest-first, paged backward by `beforeMessageId` (exclusive); loop until a
 *  page is short. Returns wire messages (the caller maps them). */
async function fetchChannelMessages(
	client: CommsClient,
	channelId: string,
	snapshotSeq: bigint,
	signal?: AbortSignal,
) {
	// The driver couples only to the stable `id` (the page cursor); the whole
	// opaque wire message flows to the injected mapper, so it stays untyped here
	// — the franklin-independent seam over the moving message/Ask shape.
	const collected: { id: string }[] = [];
	let beforeMessageId = "";
	for (;;) {
		const resp = await client.listMessages(
			{
				container: { case: "channelId", value: channelId },
				limit: SNAPSHOT_MESSAGE_PAGE,
				beforeMessageId,
				snapshotSeq,
			},
			{ signal },
		);
		collected.push(...resp.messages);
		// An empty page is the unambiguous end of history. Terminating on a short
		// page instead would couple to the server's page clamp (maxPageLimit): if
		// that clamp ever dropped below SNAPSHOT_MESSAGE_PAGE, a clamped first page
		// would read as end-of-history and silently truncate the channel.
		if (resp.messages.length === 0) break;
		// Newest-first: the last row of the page is its oldest — page before it.
		beforeMessageId = resp.messages[resp.messages.length - 1].id;
	}
	return collected;
}

/** Decode one stream response's payload into a domain CommsEvent, or null when
 *  the payload is not a comms-state entity event (a boundary-only response, the
 *  observation-pane `agentWorkspaceChanged`, or `resyncRequired` — handled by
 *  the driver as control, not here). Channel adaptation derives membership +
 *  always-subscribed from the CURRENT state (accounts already applied), so the
 *  home-channel set reflects any prior accountChanged in this stream. */
function decodeEvent(
	state: CommsState,
	payload: SubscribeCommsPayload,
	callerId: string,
	mapMessage: MapMessage,
): CommsEvent | null {
	switch (payload.case) {
		case "messagePosted":
		case "messageUpdated": {
			// Singular message fields carry proto3 presence (X | undefined); a
			// body-less event is malformed — skip rather than apply a fabricated
			// row. The kind is preserved so an update vs. post stays distinct.
			if (!payload.value.message) return null;
			return { kind: payload.case, message: mapMessage(payload.value.message) };
		}
		case "channelChanged": {
			if (!payload.value.channel) return null;
			// The caller's own removal: the server sends one final ChannelChanged
			// carrying the caller in removed_account_ids (the last event before the
			// channel goes silent). Drop the channel rather than upserting it as a
			// lingering membership='none' row — the reducer's sole removal signal.
			if (payload.value.removedAccountIds.includes(callerId)) {
				return { kind: "channelRemoved", channelId: payload.value.channel.id };
			}
			const homeIds = agentHomeChannelIds(state.accounts);
			return {
				kind: "channelChanged",
				channel: adaptChannel(payload.value.channel, callerId, homeIds),
			};
		}
		case "channelGroupChanged":
			if (!payload.value.group) return null;
			return {
				kind: "channelGroupChanged",
				group: adaptChannelGroup(payload.value.group),
			};
		case "accountChanged":
			if (!payload.value.account) return null;
			return {
				kind: "accountChanged",
				account: adaptAccount(payload.value.account),
			};
		case "topicUpserted":
			// A topic created/renamed/merged/archived — keeps the topic index live
			// without a refetch. Singular field carries proto3 presence; a body-less
			// event is malformed, so skip rather than fabricate a topic.
			if (!payload.value.topic) return null;
			return {
				kind: "topicUpserted",
				topic: adaptTopic(payload.value.topic),
			};
		case "agentPresenceChanged": {
			// A presence delta: the agent's new lifecycle + activity note. Built
			// through the same presenceLifecycle path adaptRosterEntry uses so the
			// stream and the roster seed read one vocabulary (empty activity →
			// undefined). The reducer upserts it into a fresh presence Map.
			const info: AgentPresenceInfo = {
				lifecycle: presenceLifecycle(payload.value.presence),
				activity: payload.value.activity || undefined,
			};
			return {
				kind: "presenceChanged",
				accountId: payload.value.agentAccountId,
				info,
			};
		}
		default:
			// Boundary-only, agentWorkspaceChanged (observation pane, not comms
			// state), resyncRequired (control), or an unset payload.
			return null;
	}
}

/** The payload oneof of a SubscribeCommsResponse — an indexed access on the
 *  named wire type so the switch stays exhaustive against the wire cases
 *  without importing every inner event message. */
type SubscribeCommsPayload = SubscribeCommsResponse["payload"];

/** Reconnect backoff bounds: the first retry waits up to RECONNECT_BASE_MS, each
 *  subsequent one doubles the ceiling up to RECONNECT_CAP_MS. Full jitter (a
 *  uniform draw in [0, ceiling]) spreads a fleet's reconnects so a server
 *  restart doesn't trigger a synchronized thundering herd. */
const RECONNECT_BASE_MS = 500;
const RECONNECT_CAP_MS = 30_000;

/** Await `ms`, resolving early if `signal` aborts — a teardown during backoff
 *  returns promptly instead of blocking out the full delay. */
function abortableDelay(ms: number, signal?: AbortSignal): Promise<void> {
	if (signal?.aborted) return Promise.resolve();
	return new Promise<void>((resolve) => {
		const done = () => {
			clearTimeout(timer);
			signal?.removeEventListener("abort", done);
			resolve();
		};
		const timer = setTimeout(done, ms);
		signal?.addEventListener("abort", done, { once: true });
	});
}

/** Run the comms stream until `signal` aborts. Maintains the reduced state plus
 *  the two transport cursors across reconnects: a clean drop resubscribes
 *  gap-free from the stream tail cursor; a `resync_required` resets both cursors
 *  to a cold-start re-snapshot. Each new state is pushed to `onState`. Resolves
 *  only when aborted. */
export async function runCommsStream(opts: CommsStreamOptions): Promise<void> {
	const { client, callerId, mapMessage, onState, signal, onError } = opts;
	let state = EMPTY_COMMS_STATE;
	// Transport bookkeeping, driver-local and kept OUT of CommsState: the stream
	// tail cursor (echoed as since_seq) and the server's instance epoch. Both 0
	// means a cold start → take a fresh snapshot. Never derived from snapshot_seq.
	let tailSeq = 0n;
	let instanceEpoch = 0n;
	// Consecutive reconnect failures, driving the backoff ceiling. Reset to 0 the
	// moment a subscribe yields a response (the connection is live again).
	let failures = 0;

	// Back off before the next resubscribe so a down, restarting, or
	// accept-then-drop server isn't hammered by a tight reconnect loop. Full
	// jitter over a doubling, capped ceiling; abort during the wait returns
	// promptly. Cursors are preserved for a gap-free tail (or reset on resync).
	const backoffBeforeReconnect = (): Promise<void> => {
		const ceiling = Math.min(
			RECONNECT_CAP_MS,
			RECONNECT_BASE_MS * 2 ** failures,
		);
		failures++;
		return abortableDelay(Math.random() * ceiling, signal);
	};

	while (!signal?.aborted) {
		let madeProgress = false;
		try {
			// since_seq = 0 (cold/resync) requests a fresh snapshot boundary; a
			// positive cursor is a gap-free tail resubscribe from the stream seq.
			let pendingSnapshot = tailSeq === 0n;
			const stream = client.subscribeComms(
				{ sinceSeq: tailSeq, instanceEpoch },
				{ signal },
			);

			for await (const resp of stream) {
				if (resp.payload.case === "resyncRequired") {
					// Terminal: the server can't serve our cursor gap-free. Reset both
					// cursors + state to a cold start and reconnect immediately for a
					// fresh snapshot — a resync is a server directive, not a spin.
					tailSeq = 0n;
					instanceEpoch = 0n;
					state = EMPTY_COMMS_STATE;
					madeProgress = true;
					break;
				}

				// The first response on a cold start carries the opaque snapshot_seq
				// boundary; read the point-in-time snapshot through it. Subscribing
				// before this read means the live tail already covers the snapshot's
				// window — dedup-by-id absorbs the overlap.
				const isBoundary = pendingSnapshot;
				if (pendingSnapshot) {
					const snap = await fetchSnapshot(
						client,
						mapMessage,
						resp.snapshotSeq,
						signal,
					);
					state = reduceSnapshot(callerId, snap);
					onState(state);
					pendingSnapshot = false;
				}

				// Advance the tail cursor from the STREAM's own seq (never
				// snapshot_seq), guarding against an out-of-order redelivery lowering
				// it. Every positioned response advances it, even one whose payload
				// decodes to no domain event, so the resubscribe cursor stays
				// gap-free. Forward advancement from a real (non-boundary) tail
				// response is genuine progress: it keeps the next resubscribe
				// immediate and clears the backoff ceiling. The snapshot boundary
				// positions the tail but is NOT progress — a server that only replays
				// it then cleanly closes is the tight-loop the backoff guards against.
				if (resp.seq > tailSeq) {
					tailSeq = resp.seq;
					if (!isBoundary) {
						madeProgress = true;
						failures = 0;
					}
				}
				if (resp.instanceEpoch) instanceEpoch = resp.instanceEpoch;

				const event = decodeEvent(state, resp.payload, callerId, mapMessage);
				if (event) {
					state = applyEvent(state, event);
					onState(state);
				}
			}
			// A clean close that made no forward tail progress — zero responses,
			// only the snapshot boundary, or pure seq replays — is a spin a tight
			// loop would hammer; back off (escalating) before resubscribing. A close
			// after genuine tail events, or a resync directive, stays immediate.
			if (!madeProgress) await backoffBeforeReconnect();
		} catch (error) {
			if (signal?.aborted) return;
			onError?.(error);
			await backoffBeforeReconnect();
		}
	}
}
