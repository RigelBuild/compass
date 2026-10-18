import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentBoardSchema,
	AgentBoardSummarySchema,
	AgentPresence,
	ChannelGroupSchema,
	ChannelKind,
	ChannelSchema,
	type CommsClient,
	create,
	RosterEntrySchema,
	UserAccountSchema,
	type Account as WireAccount,
	type AgentBoard as WireAgentBoard,
	type Channel as WireChannel,
	type ChannelGroup as WireChannelGroup,
	type RosterEntry as WireRosterEntry,
} from "@compass/client";
import type { CommsState, MapMessage } from "./comms-state";
import { fetchSnapshot, runCommsStream } from "./stream";

// Exercise SubscribeComms snapshot, tail, resync, and reconnect behavior with a fake client.
// Event-gated termination keeps the suite deterministic without wall-clock waits.

const CALLER = "acc-me";

// The fake wire mapper requires an id and timestamp for every message.
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
	| {
			readonly case: "agentBoardChanged";
			readonly value: {
				readonly agentAccountId: string;
				readonly board?: WireAgentBoard;
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
	/** When set, every GetRoster after the first returns these entries instead. */
	readonly rosterAfter?: WireRosterEntry[];
	/** When set, every GetRoster after the first rejects with this error. */
	readonly rosterAfterError?: Error;
	/** Runs inside every GetRoster after the first, before it returns. */
	readonly onRosterAfter?: () => void;
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
			if (calls.getRosterScopes.length === 1)
				return { entries: config.roster ?? [] };
			config.onRosterAfter?.();
			if (config.rosterAfterError) throw config.rosterAfterError;
			return { entries: config.rosterAfter ?? config.roster ?? [] };
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

	test("a clean stream drop resubscribes gap-free from the STREAM seq — never snapshotSeq, never 0 (RIG-1333 two-counter regression)", async () => {
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
		// The following channel event must see the newly added agent account.
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
		// Ignore malformed bodies but advance the cursor so reconnect stays gap-free.
		const controller = new AbortController();
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

// Observe abort-aware backoff without waiting on a wall-clock timer.
// The abort listener is registered only while the delay is active.
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
// Observe backoff via its abort listener without waiting for a timer.
describe("runCommsStream — reconnect backoff", () => {
	test("an abort DURING the backoff wait resolves it early — the run terminates without hanging out the jittered delay", async () => {
		const controller = new AbortController();
		const boom = new Error("stream boom");
		// Abort after the wait listener registers but before the timer fires.
		// The run must finish through that listener, not the jittered delay.
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
		// A clean close without responses must back off instead of spinning.
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				// Zero progress: the stream closes without yielding a response.
				async function* () {},
			],
		});
		// Abort during backoff and assert no immediate retry occurs.
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

		// One backoff wait and no resubscribe prove zero-progress closes back off.
		expect(backoff.waits).toBe(1);
		expect(calls.subscribeSinceSeqs).toEqual([0n]);
	});

	test("a boundary-only clean close (snapshot replayed, no forward tail event) backs off before resubscribing — the boundary positions the tail but is not progress", async () => {
		const controller = new AbortController();
		// Boundary replay sets the tail but is not progress, so retry backs off.
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			channels: [wireChannel("chan-1")],
			messagesByChannel: { "chan-1": [] },
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 3n);
				},
			],
		});
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

		// One backoff wait and no resubscribe prove the boundary was not progress.
		expect(backoff.waits).toBe(1);
		expect(calls.subscribeSinceSeqs).toEqual([0n]);
	});

	test("a clean close after a genuine forward tail event resubscribes IMMEDIATELY — no backoff wait — resuming from the advanced tail", async () => {
		const controller = new AbortController();
		// A genuine tail event advances the cursor and allows immediate reconnect.
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
		// No backoff listener means forward progress reconnected immediately.
		const backoff = watchBackoff(controller.signal);
		await runCommsStream({
			client,
			callerId: CALLER,
			mapMessage,
			onState: () => {},
			signal: controller.signal,
		});

		// Real tail progress reconnects immediately from seq 6, not the boundary.
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

		// Tail deltas use the roster seed's presenceLifecycle path.
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

		// Roster read failure reaches onError before any state is published.
		expect(errors).toContain(boom);
		expect(states).toHaveLength(0);
	});
});

describe("runCommsStream — board summary re-read", () => {
	const entry = (waitingOnOperator: number) =>
		create(RosterEntrySchema, {
			agentAccountId: "acc-cook",
			presence: AgentPresence.WORKING,
			board: create(AgentBoardSummarySchema, { waitingOnOperator }),
		});
	const boardChanged = (seq: bigint): FakeResponse => ({
		seq,
		atUnixMs: 0n,
		instanceEpoch: 1n,
		snapshotSeq: 0n,
		payload: {
			case: "agentBoardChanged",
			value: {
				agentAccountId: "acc-cook",
				board: create(AgentBoardSchema, { agentAccountId: "acc-cook" }),
			},
		},
	});
	const run = async (
		config: Omit<FakeConfig, "subscribeScripts">,
		controller = new AbortController(),
	) => {
		const { client, calls } = createFakeClient(controller, {
			accounts: [wireUserAccount(CALLER)],
			...config,
			subscribeScripts: [
				async function* () {
					yield boundary(100n, 1n);
					yield boundary(0n, 2n);
					yield boardChanged(3n);
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
		const waiting = states.at(-1)?.presence.get("acc-cook")
			?.board?.waitingOnOperator;
		return { calls, waiting, errors };
	};

	test("a re-read that resolves after teardown applies nothing", async () => {
		const controller = new AbortController();
		const { calls, waiting } = await run(
			{
				roster: [entry(1)],
				rosterAfter: [entry(0)],
				onRosterAfter: () => controller.abort(),
			},
			controller,
		);
		expect(calls.getRosterScopes).toEqual([2, 2]);
		expect(waiting).toBe(1);
	});

	test("an agentBoardChanged re-reads GetRoster for the server's waiting count", async () => {
		const { calls, waiting } = await run({
			roster: [entry(1)],
			rosterAfter: [entry(0)],
		});
		expect(calls.getRosterScopes).toEqual([2, 2]);
		expect(waiting).toBe(0);
	});

	test("a failed re-read reports the error and keeps the event's summary", async () => {
		const boom = new Error("getRoster boom");
		const { waiting, errors } = await run({
			roster: [entry(1)],
			rosterAfterError: boom,
		});
		expect(errors).toEqual([boom]);
		expect(waiting).toBe(1);
	});

	test("an agent the re-read no longer lists waits on no one", async () => {
		const { calls, waiting } = await run({
			roster: [entry(2)],
			rosterAfter: [],
		});
		expect(calls.getRosterScopes).toEqual([2, 2]);
		expect(waiting).toBe(0);
	});
});
