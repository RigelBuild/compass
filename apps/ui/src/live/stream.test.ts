import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentPresence,
	ChannelGroupSchema,
	ChannelKind,
	ChannelSchema,
	type CommsClient,
	create,
	RosterEntrySchema,
	UserAccountSchema,
	type Account as WireAccount,
	type Channel as WireChannel,
	type ChannelGroup as WireChannelGroup,
	type RosterEntry as WireRosterEntry,
} from "@compass/client";
import type { CommsState, MapMessage } from "./comms-state";
import { fetchSnapshot, runCommsStream } from "./stream";

// stream.ts is the SubscribeComms driver: it applies the frozen
// snapshot+tail+resync protocol over a CommsClient, pushing each reduced state
// to onState. It owns the TWO transport cursors kept out of the reducer
// (SEA-1333): the read-RPC boundary `snapshotSeq` (an opaque token forwarded
// verbatim to every list call, NEVER a tail cursor) and the stream tail cursor,
// which advances from `SubscribeCommsResponse.seq` (the stream's own counter).
// These tests drive it against a hand-written fake client (no network, no
// timers) and defend the protocol contracts a refactor could silently break:
// since_seq=0 snapshot-then-tail with the boundary snapshotSeq threaded to every
// read RPC, boundary-overlap dedup, resyncRequired reset to a full re-snapshot,
// gap-free clean-drop resubscribe from the STREAM seq (never snapshotSeq),
// abort termination, onError-then-reconnect, and fetchSnapshot's backward paging
// with the opaque-token contract. Termination is always event-gated (a finite
// generator or an AbortController), never a wall-clock wait, so the suite is
// deterministic and hermetic.

const CALLER = "acc-me";

// ── The injected message mapper. Narrows the opaque wire object (never an inline
//    cast) — we author the fake wire below, so `id`/`atUnixMs` are always present.
interface FakeWireMessage {
	readonly id: string;
	readonly atUnixMs: number;
}
const mapMessage: MapMessage = (w) => {
	if (
		typeof w !== "object" ||
		w === null ||
		!("id" in w) ||
		typeof w.id !== "string"
	) {
		throw new Error("fake wire message must carry a string id");
	}
	const atUnixMs =
		"atUnixMs" in w && typeof w.atUnixMs === "number" ? w.atUnixMs : 0;
	return {
		id: w.id,
		topicId: "top-1",
		authorAccountId: CALLER,
		atUnixMs,
		blocks: [{ kind: "text", text: w.id }],
	};
};

function wireMessage(id: string, atUnixMs: number): FakeWireMessage {
	return { id, atUnixMs };
}

// ── Fake SubscribeCommsResponse — the fields the driver reads. Reconciled to the
//    generated type only at the single fake→CommsClient cast boundary below.
type FakePayload =
	| {
			readonly case: "messagePosted";
			readonly value: { readonly message: FakeWireMessage };
	  }
	| {
			readonly case: "messageUpdated";
			readonly value: { readonly message: FakeWireMessage };
	  }
	| {
			readonly case: "channelChanged";
			readonly value: {
				// Optional to mirror proto3 message presence — a body-less
				// channelChanged (value.channel undefined) is the malformed-event
				// case the driver must skip.
				readonly channel?: WireChannel;
				readonly removedAccountIds: string[];
			};
	  }
	| {
			readonly case: "channelGroupChanged";
			readonly value: { readonly group: WireChannelGroup };
	  }
	| {
			readonly case: "accountChanged";
			// Optional to mirror proto3 message presence — a body-less
			// accountChanged (value.account undefined) is skipped by the driver.
			readonly value: { readonly account?: WireAccount };
	  }
	| {
			readonly case: "agentPresenceChanged";
			readonly value: {
				readonly agentAccountId: string;
				readonly presence: AgentPresence;
				readonly activity: string;
			};
	  }
	| { readonly case: "resyncRequired"; readonly value: Record<string, never> }
	| { readonly case: undefined; readonly value?: undefined };
interface FakeResponse {
	readonly seq: bigint;
	readonly atUnixMs: bigint;
	readonly instanceEpoch: bigint;
	readonly snapshotSeq: bigint;
	readonly payload: FakePayload;
}

// A boundary-only first response: carries the snapshotSeq the driver reads at,
// no entity payload (decodeEvent yields null → snapshot only). `seq` is the
// stream tail cursor — kept distinct from snapshotSeq on purpose so the tests
// can tell which counter the driver tails from.
function boundary(
	snapshotSeq: bigint,
	seq = 0n,
	instanceEpoch = 1n,
): FakeResponse {
	return {
		seq,
		atUnixMs: 0n,
		instanceEpoch,
		snapshotSeq,
		payload: { case: undefined },
	};
}
function posted(
	seq: bigint,
	message: FakeWireMessage,
	instanceEpoch = 1n,
): FakeResponse {
	return {
		seq,
		atUnixMs: 0n,
		instanceEpoch,
		// A tail response carries no read boundary — snapshotSeq is a first-
		// response-only token. Fixed at 0 so a driver that (wrongly) tailed from
		// snapshotSeq would be caught resubscribing at 0, not the stream seq.
		snapshotSeq: 0n,
		payload: { case: "messagePosted", value: { message } },
	};
}
function resync(): FakeResponse {
	return {
		seq: 0n,
		atUnixMs: 0n,
		instanceEpoch: 0n,
		snapshotSeq: 0n,
		payload: { case: "resyncRequired", value: {} },
	};
}
// A tail channelChanged: an upsert of `channel`, plus the removed-account list
// (empty for a create/add; the caller's id present is the ghost-channel removal
// signal the driver decodes to channelRemoved). `channel` may be omitted to
// author the body-less (malformed) event the driver must skip.
function channelChanged(
	seq: bigint,
	channel?: WireChannel,
	removedAccountIds: string[] = [],
): FakeResponse {
	return {
		seq,
		atUnixMs: 0n,
		instanceEpoch: 1n,
		snapshotSeq: 0n,
		payload: { case: "channelChanged", value: { channel, removedAccountIds } },
	};
}
// A tail accountChanged: an upsert of `account`. `account` may be omitted to
// author the body-less (malformed) event the driver must skip.
function accountChanged(seq: bigint, account?: WireAccount): FakeResponse {
	return {
		seq,
		atUnixMs: 0n,
		instanceEpoch: 1n,
		snapshotSeq: 0n,
		payload: { case: "accountChanged", value: { account } },
	};
}

interface RecordedCalls {
	readonly listAccountsSeqs: bigint[];
	readonly listChannelsSeqs: bigint[];
	readonly listChannelGroupsSeqs: bigint[];
	readonly getRosterScopes: number[];
	readonly listMessages: {
		channelId: string;
		beforeMessageId: string;
		snapshotSeq: bigint;
	}[];
	readonly subscribeSinceSeqs: bigint[];
	readonly subscribeEpochs: bigint[];
}

interface FakeConfig {
	readonly accounts?: WireAccount[];
	readonly groups?: WireChannelGroup[];
	readonly channels?: WireChannel[];
	/** Per-channel topic list — what ListTopics serves for that channel. */
	readonly topicsByChannel?: Record<string, unknown[]>;
	/** Per-channel, newest-first full message list; paging is simulated over it. */
	readonly messagesByChannel?: Record<string, FakeWireMessage[]>;
	/** Simulate a server whose effective page size is SMALLER than the requested
	 *  limit — each listMessages returns at most this many rows regardless of the
	 *  driver's requested limit. Exercises the empty-page (not short-page)
	 *  terminator: a clamped page must not read as end-of-history. */
	readonly messagePageClamp?: number;
	/** One generator factory per subscribeComms call, in order. */
	readonly subscribeScripts?: Array<() => AsyncGenerator<FakeResponse>>;
	/** Roster entries GetRoster returns for the snapshot presence seed. */
	readonly roster?: WireRosterEntry[];
	/** When set, GetRoster rejects with this error instead of returning — the
	 *  read-failure path (the snapshot must abort, not seed an empty map). */
	readonly getRosterError?: Error;
}

const MESSAGE_PAGE = 200;

/** Build the fake comms client. The whole object is cast to CommsClient through
 *  `unknown` at this single boundary — it implements only the driven subset
 *  (list* + subscribeComms), so a structural assignability check would (rightly)
 *  reject the partial shape; the unknown-cast is the one sanctioned seam. */
function createFakeClient(
	controller: AbortController,
	config: FakeConfig,
): { client: CommsClient; calls: RecordedCalls } {
	const calls: RecordedCalls = {
		listAccountsSeqs: [],
		listChannelsSeqs: [],
		listChannelGroupsSeqs: [],
		getRosterScopes: [],
		listMessages: [],
		subscribeSinceSeqs: [],
		subscribeEpochs: [],
	};
	const scripts = config.subscribeScripts ?? [];
	let subscribeIdx = 0;

	// biome-ignore lint/correctness/useYield: intentionally yields nothing — an unscripted resubscribe aborts to end the driver loop rather than emitting a (spurious) response.
	async function* exhausted(): AsyncGenerator<FakeResponse> {
		// Safety net: an unscripted resubscribe would otherwise loop forever.
		// Abort so the driver's while-loop terminates instead of hanging.
		controller.abort();
	}

	const fake = {
		listAccounts: async ({ snapshotSeq }: { snapshotSeq: bigint }) => {
			calls.listAccountsSeqs.push(snapshotSeq);
			return { accounts: config.accounts ?? [] };
		},
		listChannelGroups: async ({ snapshotSeq }: { snapshotSeq: bigint }) => {
			calls.listChannelGroupsSeqs.push(snapshotSeq);
			return { groups: config.groups ?? [] };
		},
		listChannels: async ({ snapshotSeq }: { snapshotSeq: bigint }) => {
			calls.listChannelsSeqs.push(snapshotSeq);
			return { channels: config.channels ?? [] };
		},
		getRoster: async ({ scope }: { scope: number }) => {
			calls.getRosterScopes.push(scope);
			if (config.getRosterError) throw config.getRosterError;
			return { entries: config.roster ?? [] };
		},
		listTopics: async (req: { channelId: string }) => ({
			topics: config.topicsByChannel?.[req.channelId] ?? [],
		}),
		listMessages: async (req: {
			container: { case: "channelId"; value: string };
			limit: number;
			beforeMessageId: string;
			snapshotSeq: bigint;
		}) => {
			calls.listMessages.push({
				channelId: req.container.value,
				beforeMessageId: req.beforeMessageId,
				snapshotSeq: req.snapshotSeq,
			});
			const all = config.messagesByChannel?.[req.container.value] ?? [];
			let start = 0;
			if (req.beforeMessageId) {
				start = all.findIndex((m) => m.id === req.beforeMessageId) + 1;
			}
			// The effective page size is the smaller of the driver's requested
			// limit and the simulated server clamp — a clamp < limit yields
			// "full-from-the-old-code's-view" pages that only the empty-page
			// terminator can stop.
			const pageSize = Math.min(
				req.limit,
				config.messagePageClamp ?? req.limit,
			);
			return { messages: all.slice(start, start + pageSize) };
		},
		subscribeComms: (req: {
			sinceSeq: bigint;
			instanceEpoch: bigint;
		}): AsyncGenerator<FakeResponse> => {
			calls.subscribeSinceSeqs.push(req.sinceSeq);
			calls.subscribeEpochs.push(req.instanceEpoch);
			const factory = scripts[subscribeIdx++] ?? exhausted;
			return factory();
		},
	};

	return { client: fake as unknown as CommsClient, calls };
}

function wireUserAccount(id: string): WireAccount {
	return create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: { case: "user", value: create(UserAccountSchema, {}) },
	});
}
// An agent account whose home channel is `homeChannelId` — the input to the
// always-subscribed derivation (adaptChannel flags an agent's own home channel
// implicitly subscribed). Mirrors wireUserAccount; the agent kind arm carries
// the home channel id (comms_pb AgentAccount.homeChannelId).
function wireAgentAccount(id: string, homeChannelId: string): WireAccount {
	return create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, { ownerUserId: CALLER, homeChannelId }),
		},
	});
}
function wireChannel(id: string): WireChannel {
	return create(ChannelSchema, {
		id,
		name: id,
		kind: ChannelKind.CHANNEL,
		memberAccountIds: [CALLER],
		subscriberAccountIds: [CALLER],
	});
}
function wireGroup(id: string): WireChannelGroup {
	return create(ChannelGroupSchema, { id, name: id });
}
function wireRosterEntry(
	agentAccountId: string,
	presence: AgentPresence,
	activity = "",
): WireRosterEntry {
	return create(RosterEntrySchema, { agentAccountId, presence, activity });
}
// A tail agentPresenceChanged: an upsert of one agent's presence + activity.
function presenceChanged(
	seq: bigint,
	agentAccountId: string,
	presence: AgentPresence,
	activity = "",
): FakeResponse {
	return {
		seq,
		atUnixMs: 0n,
		instanceEpoch: 1n,
		snapshotSeq: 0n,
		payload: {
			case: "agentPresenceChanged",
			value: { agentAccountId, presence, activity },
		},
	};
}

describe("runCommsStream — snapshot then tail", () => {
	test("first subscribe boundary snapshots via the read RPCs (each at snapshotSeq), then tail deltas fire onState", async () => {
		const controller = new AbortController();
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER), wireUserAccount("acc-cook")],
			channels: [wireChannel("chan-1")],
			groups: [wireGroup("grp-1")],
			messagesByChannel: { "chan-1": [wireMessage("m-snap", 100)] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield posted(2n, wireMessage("m-tail", 200));
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		// Every read RPC received the boundary snapshotSeq (the point-in-time view).
		expect(calls.listAccountsSeqs).toEqual([100n]);
		expect(calls.listChannelsSeqs).toEqual([100n]);
		expect(calls.listChannelGroupsSeqs).toEqual([100n]);
		expect(calls.listMessages[0]?.snapshotSeq).toBe(100n);

		// onState fired once for the snapshot, then once for the tail delta.
		expect(states).toHaveLength(2);
		expect(states[0]?.accounts.map((a) => a.id)).toEqual([CALLER, "acc-cook"]);
		expect(states[0]?.channels.map((c) => c.id)).toEqual(["chan-1"]);
		expect(states[0]?.messages.map((m) => m.id)).toEqual(["m-snap"]);
		expect(states[1]?.messages.map((m) => m.id)).toEqual(["m-snap", "m-tail"]);
	});

	test("a tail messagePosted whose id is already in the snapshot does not duplicate (boundary dedup)", async () => {
		const controller = new AbortController();
		const { client } = createFakeClient(controller, {
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [wireMessage("m1", 100)] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					// Redelivery of m1 across the snapshot boundary (at-least-once).
					yield posted(2n, wireMessage("m1", 100));
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});
		const final = states.at(-1);
		expect(final?.messages.filter((m) => m.id === "m1")).toHaveLength(1);
		expect(final?.messages).toHaveLength(1);
	});
});

describe("runCommsStream — resync + reconnect", () => {
	test("resyncRequired resets to sinceSeq=0 and re-runs the snapshot", async () => {
		const controller = new AbortController();
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield posted(5n, wireMessage("m-a", 100));
					yield resync();
				},
				async function* () {
					yield boundary(200n, 1n);
					controller.abort();
				},
			],
		});
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
		});

		// Both subscribes went out at sinceSeq=0 — the reset dropped the cursor
		// the tail had advanced to 5, not resumed from it.
		expect(calls.subscribeSinceSeqs).toEqual([0n, 0n]);
		// Two full snapshot rounds ran, the second at the fresh boundary (200).
		expect(calls.listAccountsSeqs).toEqual([100n, 200n]);
	});

	test("a clean stream drop resubscribes gap-free from the STREAM seq — never snapshotSeq, never 0 (SEA-1333 two-counter regression)", async () => {
		const controller = new AbortController();
		// The regression fixture: the boundary snapshotSeq (500) and the tail seqs
		// (1,2,3) are DIFFERENT number spaces on purpose. A driver that conflated
		// them would resubscribe at 500 (snapshotSeq) or 0 (never advanced); the
		// gap-free contract is that it resumes from the last STREAM seq (3).
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(500n, 1n);
					yield posted(2n, wireMessage("m-a", 100));
					yield posted(3n, wireMessage("m-b", 150));
					// Clean return (not abort, not resync) — a dropped connection.
				},
				async function* () {
					yield posted(4n, wireMessage("m-c", 200));
					controller.abort();
				},
			],
		});
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
		});

		// THE regression assertion: the resubscribe resumes from the last tail
		// STREAM seq (3), NOT the boundary token (500) and NOT 0.
		expect(calls.subscribeSinceSeqs).toEqual([0n, 3n]);
		// The instance epoch is echoed back from the responses.
		expect(calls.subscribeEpochs).toEqual([0n, 1n]);
		// The snapshot ran exactly once — the clean-drop resubscribe did not
		// re-snapshot, and every read RPC read the boundary token verbatim (500).
		expect(calls.listAccountsSeqs).toEqual([500n]);
	});

	test("a stream error invokes onError and the driver reconnects from the stream seq", async () => {
		const controller = new AbortController();
		const boom = new Error("stream boom");
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield posted(3n, wireMessage("m-a", 150));
					throw boom;
				},
				async function* () {
					yield posted(4n, wireMessage("m-b", 200));
					controller.abort();
				},
			],
		});
		const errors: unknown[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
			onError: (e) => errors.push(e),
		});

		expect(errors).toEqual([boom]);
		// Reconnected after the error, resuming from the preserved stream seq (3).
		expect(calls.subscribeSinceSeqs).toEqual([0n, 3n]);
	});
});

describe("runCommsStream — abort", () => {
	test("aborting the signal terminates the run and stops firing onState", async () => {
		const controller = new AbortController();
		const { client } = createFakeClient(controller, {
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield posted(2n, wireMessage("m-a", 100));
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		// If abort were not honored (while-guard / catch-return removed), the
		// unscripted resubscribe safety-net still aborts — but the contract under
		// test is that this resolves at all and freezes onState after the abort.
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});
		// Snapshot + one tail delta, then terminated — no further onState.
		expect(states).toHaveLength(2);
	});
});

describe("runCommsStream — channel removal (ghost channel)", () => {
	test("a channelChanged carrying the caller in removedAccountIds drops the channel from state", async () => {
		const controller = new AbortController();
		// Snapshot has chan-1 (caller is member + subscriber). The server then
		// sends the caller's own removal as one final channelChanged with the
		// caller in removed_account_ids — the driver must DROP the channel, not
		// upsert it as a lingering membership='none' row.
		const { client } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield channelChanged(2n, wireChannel("chan-1"), [CALLER]);
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		// Snapshot had chan-1…
		expect(states[0]?.channels.map((c) => c.id)).toEqual(["chan-1"]);
		// …and the removal event dropped it entirely.
		expect(states.at(-1)?.channels.map((c) => c.id)).toEqual([]);
	});

	test("a channelChanged WITHOUT the caller in removedAccountIds upserts (no over-eager removal)", async () => {
		const controller = new AbortController();
		// A membership change that does NOT remove the caller (empty
		// removed_account_ids) is an ordinary upsert — the channel must stay.
		const { client } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield channelChanged(2n, wireChannel("chan-2"), []);
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		// Both the snapshot channel and the upserted one are present — the
		// no-removal path never drops.
		expect(states.at(-1)?.channels.map((c) => c.id)).toEqual([
			"chan-1",
			"chan-2",
		]);
	});
});

describe("runCommsStream — home-channel derivation ordering", () => {
	test("an accountChanged (new agent) before a channelChanged for its home channel makes that channel alwaysSubscribed — home set derived from CURRENT accounts", async () => {
		const controller = new AbortController();
		// The ordering invariant: decodeEvent derives the agent home-channel set
		// from state.accounts AT DECODE TIME. An accountChanged introducing an
		// agent whose home is chan-home, THEN a channelChanged for chan-home,
		// must flag it alwaysSubscribed — proving the home set reflects the prior
		// accountChanged, not a stale snapshot-time set.
		const { client } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [],
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield accountChanged(2n, wireAgentAccount("acc-agent", "chan-home"));
					yield channelChanged(3n, wireChannel("chan-home"), []);
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		const home = states.at(-1)?.channels.find((c) => c.id === "chan-home");
		expect(home?.membership).toBe("subscribed");
		expect(home?.alwaysSubscribed).toBe(true);
	});
});

describe("runCommsStream — body-less event guards", () => {
	test("channelChanged/accountChanged with no channel/account are skipped (no state change) but still advance the tail cursor", async () => {
		const controller = new AbortController();
		// A malformed (body-less) channelChanged/accountChanged decodes to null —
		// the driver skips it (no onState, no crash) but MUST still advance the
		// tail cursor, so a resubscribe after a clean drop resumes gap-free from
		// past the skipped seqs (mirrors the gap-free resubscribe contract).
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield channelChanged(5n, undefined, []);
					yield accountChanged(6n, undefined);
					// Clean drop — resubscribe should resume from the last seq (6).
				},
				// biome-ignore lint/correctness/useYield: intentionally yields nothing — an abort-only resubscribe ends the driver loop rather than emitting a (spurious) response.
				async function* () {
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		// Only the snapshot fired onState — both body-less events were skipped.
		expect(states).toHaveLength(1);
		// …yet the tail cursor advanced through them: the resubscribe resumed
		// from seq 6, not 5 or 0 — skipped events still position the cursor.
		expect(calls.subscribeSinceSeqs).toEqual([0n, 6n]);
	});
});

// Instrument the signal to observe the driver entering (or NOT entering) its
// abort-aware backoff wait WITHOUT elapsing wall-clock. abortableDelay (stream.ts)
// is the ONLY code on the reconnect path that registers an "abort" listener on
// the signal — the fake client and fetchSnapshot ignore their { signal } option
// — so a registered abort listener is a one-to-one proxy for "a backoff wait is
// in progress". `onBackoff` (if given) fires synchronously at registration, i.e.
// AFTER abortableDelay has armed its setTimeout + abort listener but BEFORE the
// timer macrotask can run; scheduling controller.abort() as a MICROTASK from
// there lands the abort DURING the wait (microtasks drain before the setTimeout),
// so the listener path — never the timer — resolves it. Mirrors test F's ordering
// but hooks the wait itself, since a clean close has no onError to piggyback on.
function watchBackoff(
	signal: AbortSignal,
	onBackoff?: () => void,
): { waits: number } {
	const tracker = { waits: 0 };
	const realAdd = signal.addEventListener.bind(signal);
	signal.addEventListener = ((
		type: string,
		listener: EventListenerOrEventListenerObject,
		options?: boolean | AddEventListenerOptions,
	) => {
		realAdd(type as "abort", listener, options);
		if (type === "abort") {
			tracker.waits++;
			onBackoff?.();
		}
	}) as typeof signal.addEventListener;
	return tracker;
}

describe("runCommsStream — reconnect backoff", () => {
	test("an abort DURING the backoff wait resolves it early — the run terminates without hanging out the jittered delay", async () => {
		const controller = new AbortController();
		const boom = new Error("stream boom");
		// One subscribe throws → the driver enters its backoff wait
		// (abortableDelay over Math.random()*ceiling, up to RECONNECT_CAP_MS=30s).
		// onError schedules the abort as a MICROTASK: it lands AFTER abortableDelay
		// has already registered its setTimeout + abort listener (so the signal is
		// NOT pre-aborted — the listener path, not the early-return, is what fires)
		// but BEFORE the timer macrotask can run (microtasks drain first). So the
		// abort listener is the ONLY thing that resolves the wait: if abortableDelay
		// didn't listen for abort, this test would hang out the full jittered delay
		// and time out. Fully hermetic — no wall-clock delay ever elapses.
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					throw boom;
				},
			],
		});
		const errors: unknown[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
			onError: (e) => {
				errors.push(e);
				queueMicrotask(() => controller.abort());
			},
		});

		// The run resolved (did not hang): onError fired once for the failure…
		expect(errors).toEqual([boom]);
		// …and the abort during the wait ended the loop before any resubscribe —
		// exactly one subscribeComms call, no reconnect after the failure.
		expect(calls.subscribeSinceSeqs).toEqual([0n]);
	});

	test("a zero-progress clean close (subscribe accepted then dropped with no response) backs off before resubscribing — an abort during that wait ends the run", async () => {
		const controller = new AbortController();
		// The first subscribe is accepted then closes cleanly having yielded
		// NOTHING — the accept-then-drop spin the fix guards against. The driver
		// must treat a zero-progress close as a failed attempt and enter the
		// abort-aware backoff, NOT resubscribe in a tight loop.
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				// A zero-progress clean close: yields nothing then returns — the
				// accept-then-drop the driver must back off on, not resubscribe.
				async function* () {},
			],
		});
		// Land the abort DURING the backoff wait: when the driver backs off,
		// abortableDelay registers its abort listener, we abort from a microtask,
		// and the wait resolves via that listener (no wall-clock elapses). If the
		// driver did NOT back off (the bug), no listener registers, the abort is
		// never armed, and the immediate resubscribe hits the exhausted() safety
		// net — a SECOND subscribe at sinceSeq 0 → subscribeSinceSeqs [0n, 0n].
		const backoff = watchBackoff(controller.signal, () =>
			queueMicrotask(() => controller.abort()),
		);
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
		});

		// The zero-progress close entered exactly one backoff wait…
		expect(backoff.waits).toBe(1);
		// …and the abort during that wait ended the loop BEFORE any resubscribe:
		// exactly one subscribeComms, no tight-loop retry. Removing the
		// `if (!sawResponse)` guard (or forcing sawResponse true) reverts this to
		// an immediate resubscribe — a second (unscripted) subscribe → [0n, 0n].
		expect(calls.subscribeSinceSeqs).toEqual([0n]);
	});

	test("a boundary-only clean close (snapshot replayed, no forward tail event) backs off before resubscribing — the boundary positions the tail but is not progress", async () => {
		const controller = new AbortController();
		// First subscribe is accepted, replays ONLY the snapshot boundary, then
		// closes cleanly. The boundary positions the tail (seq 3) but is NOT
		// forward progress: a server that accepts → replays only the boundary →
		// drops, repeatedly, is the cold-start tight loop the fix now guards
		// against. The driver must treat this as a failed attempt and enter the
		// abort-aware backoff, NOT resubscribe immediately.
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 3n);
					// Boundary-only clean close — must back off (no tail progress).
				},
			],
		});
		// Land the abort DURING the backoff wait: when the driver backs off,
		// abortableDelay registers its abort listener, we abort from a microtask,
		// and the wait resolves via that listener (no wall-clock elapses). If the
		// driver did NOT back off (the OLD bug — boundary counted as progress), no
		// listener registers, the immediate resubscribe hits the exhausted() safety
		// net, and a SECOND subscribe goes out at the boundary tail → [0n, 3n].
		const backoff = watchBackoff(controller.signal, () =>
			queueMicrotask(() => controller.abort()),
		);
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
		});

		// The boundary-only close entered exactly one backoff wait…
		expect(backoff.waits).toBe(1);
		// …and the abort during that wait ended the loop BEFORE any resubscribe:
		// exactly one subscribeComms, no tight-loop retry. Mutating the source
		// progress guard `if (!isBoundary)` to `if (true)` (boundary counts as
		// progress again) reverts this to an immediate resubscribe → [0n, 3n].
		expect(calls.subscribeSinceSeqs).toEqual([0n]);
	});

	test("a clean close after a genuine forward tail event resubscribes IMMEDIATELY — no backoff wait — resuming from the advanced tail", async () => {
		const controller = new AbortController();
		// First subscribe yields the boundary (snapshotSeq 100, tail seq 3) AND
		// THEN a real tail event that advances the cursor past it (channelChanged
		// at seq 6 > boundary seq 3, non-boundary), then closes cleanly. Genuine
		// forward tail progress: it must resubscribe with NO backoff, resuming
		// gap-free from the advanced tail (seq 6), NOT the boundary token (100).
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 3n);
					yield channelChanged(6n, wireChannel("chan-2"));
					// Clean close after a genuine tail advance — must NOT back off.
				},
				// biome-ignore lint/correctness/useYield: intentionally yields nothing — the immediate resubscribe aborts to end the loop; its existence (a second subscribe reached with no backoff) is the assertion.
				async function* () {
					controller.abort();
				},
			],
		});
		// No abort is armed: watchBackoff only COUNTS backoff waits. A genuine-
		// progress close must not enter one, so the run reaches the second
		// subscribe on its own — the immediate resubscribe — which aborts.
		// Immediate-resubscribe vs backoff is distinguished purely by the listener
		// count; no timer elapses on either branch, so the assertion is hermetic.
		const backoff = watchBackoff(controller.signal);
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
		});

		// Zero backoff waits — the genuine-progress close resubscribed immediately
		// from the advanced stream tail (seq 6), not the boundary seq (3) or the
		// boundary token (100). Real tail progress keeps the resubscribe immediate.
		expect(backoff.waits).toBe(0);
		expect(calls.subscribeSinceSeqs).toEqual([0n, 6n]);
	});
});

describe("fetchSnapshot — paging + opaque token", () => {
	test("pages backward by beforeMessageId until an empty page, collecting + mapping all, forwarding snapshotSeq verbatim", async () => {
		const controller = new AbortController();
		// One full page (200) + a partial page (3) + the empty page that
		// terminates → 203 total across THREE calls. Termination is the empty
		// page, not the short one, so a full channel costs one extra request.
		const full = Array.from({ length: MESSAGE_PAGE }, (_, i) =>
			wireMessage(`m${String(i).padStart(3, "0")}`, i),
		);
		const rest = [
			wireMessage("m200", 200),
			wireMessage("m201", 201),
			wireMessage("m202", 202),
		];
		const all = [...full, ...rest];
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			groups: [wireGroup("grp-1")],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": all },
		});

		const snap = await fetchSnapshot(client, mapMessage, 42n);

		// Three pages fetched: full (200) → partial (3) → empty ([]) terminator.
		expect(calls.listMessages).toHaveLength(3);
		expect(calls.listMessages[0]?.beforeMessageId).toBe("");
		expect(calls.listMessages[1]?.beforeMessageId).toBe(
			full[MESSAGE_PAGE - 1]?.id,
		);
		// The third call cursors past the SECOND page's oldest row (m202) — the
		// empty page it returns is what stops the loop.
		expect(calls.listMessages[2]?.beforeMessageId).toBe("m202");
		// Every message collected and run through the injected mapper.
		expect(snap.messages).toHaveLength(203);
		expect(snap.messages.map((m) => m.id)).toContain("m000");
		expect(snap.messages.map((m) => m.id)).toContain("m202");
		// Opaque-token contract: EVERY read RPC received the same snapshotSeq (42),
		// forwarded verbatim, never interpreted.
		expect(calls.listAccountsSeqs).toEqual([42n]);
		expect(calls.listChannelGroupsSeqs).toEqual([42n]);
		expect(calls.listChannelsSeqs).toEqual([42n]);
		expect(calls.listMessages.map((c) => c.snapshotSeq)).toEqual([
			42n,
			42n,
			42n,
		]);
	});

	test("terminates on the empty page, not a short one — a server page clamp below the requested limit does not truncate", async () => {
		const controller = new AbortController();
		// Five messages, but the fake server clamps every page to 2 — smaller than
		// the requested SNAPSHOT_MESSAGE_PAGE (200). The old short-page terminator
		// would stop after the first 2-row page (2 < 200) and silently drop 3
		// messages; the empty-page terminator collects all five.
		const all = [
			wireMessage("m0", 0),
			wireMessage("m1", 1),
			wireMessage("m2", 2),
			wireMessage("m3", 3),
			wireMessage("m4", 4),
		];
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": all },
			messagePageClamp: 2,
		});

		const snap = await fetchSnapshot(client, mapMessage, 7n);

		// Pages of 2, 2, 1, then the empty page that stops the loop → 4 calls.
		// A short-page terminator would have made only ONE call and truncated.
		expect(calls.listMessages).toHaveLength(4);
		expect(calls.listMessages.map((c) => c.beforeMessageId)).toEqual([
			"",
			"m1",
			"m3",
			"m4",
		]);
		// All five collected — no premature stop despite every page being < 200.
		expect(snap.messages.map((m) => m.id)).toEqual([
			"m0",
			"m1",
			"m2",
			"m3",
			"m4",
		]);
	});
});

describe("runCommsStream — presence seed + tail", () => {
	test("the snapshot seeds presence from GetRoster(OWNER), and a tail agentPresenceChanged upserts one key", async () => {
		const controller = new AbortController();
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			roster: [wireRosterEntry("acc-cook", AgentPresence.WORKING, "cooking")],
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield presenceChanged(2n, "acc-cook", AgentPresence.IDLE);
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		// GetRoster was called once with the OWNER scope.
		expect(calls.getRosterScopes).toEqual([2]);
		// The snapshot seeded presence from the roster read.
		expect(states[0]?.presence.get("acc-cook")).toEqual({
			lifecycle: "working",
			activity: "cooking",
		});
		// The tail delta upserted the same key to its new lifecycle.
		expect(states.at(-1)?.presence.get("acc-cook")).toEqual({
			lifecycle: "idle",
			activity: undefined,
		});
	});

	test("an UNSPECIFIED presence tail delta routes through presenceLifecycle → lifecycle undefined (seed↔tail one vocabulary)", async () => {
		const controller = new AbortController();
		const { client } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			roster: [wireRosterEntry("acc-x", AgentPresence.WORKING, "cooking")],
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield presenceChanged(2n, "acc-x", AgentPresence.UNSPECIFIED);
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			signal: controller.signal,
		});

		// The tail arm builds its AgentPresenceInfo through presenceLifecycle, the
		// same path the roster seed uses — so an UNSPECIFIED delta yields the
		// defensive `lifecycle: undefined`, never a bare enum leaking past the
		// vocabulary. A regression that bypassed presenceLifecycle would red here
		// while T1's isolated adapt tests stayed green.
		expect(states.at(-1)?.presence.get("acc-x")).toEqual({
			lifecycle: undefined,
			activity: undefined,
		});
	});

	test("a GetRoster rejection aborts the whole snapshot — onError fires, presence is never a silent empty map", async () => {
		const controller = new AbortController();
		const boom = new Error("getRoster boom");
		const { client } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			getRosterError: boom,
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					controller.abort();
				},
				// biome-ignore lint/correctness/useYield: the retried subscribe aborts to end the loop.
				async function* () {
					controller.abort();
				},
			],
		});
		const states: CommsState[] = [];
		const errors: unknown[] = [];
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: (s) => states.push(s),
			onError: (e) => errors.push(e),
			signal: controller.signal,
		});

		// The roster read is in the snapshot failure domain: its rejection threw,
		// surfaced to onError, and no state (empty presence or otherwise) was
		// pushed from the failed snapshot round.
		expect(errors).toContain(boom);
		expect(states).toHaveLength(0);
	});
});
