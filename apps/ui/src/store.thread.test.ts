import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import { threadsOf } from "./comms";
import { STUB_COMMS_STATE } from "./comms-stub";
import {
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireChannel,
	wireTextMessage,
} from "./live/comms-fake";
import { type AppStore, createAppStore } from "./store";
import { STUB_AGENTS } from "./stub-data";

// The withStore harness, copied verbatim from store.test.ts:26-56. The store
// exposes SolidJS accessors that only compute inside a reactive root, so every
// assertion runs inside a fresh `createRoot` that is disposed afterward — no
// effects, no waiting, no cross-test leakage. Thread actions are synchronous and
// the `openThreadRootId` accessor recomputes on demand within the root.
//
// The open/close tests are OFFLINE (no client), reading the comms fixture seeded
// through `initialComms`; the postReply tests below are LIVE (`withLiveStore`)
// because posting is now a real wire call.
function withStore(body: (store: AppStore) => void): void {
	createRoot((dispose) => {
		const store = createAppStore({ initialComms: STUB_COMMS_STATE });
		try {
			body(store);
		} finally {
			dispose();
		}
	});
}

// The live cousin, mirroring store.live.test.ts: a store over the CommsClient
// double, its snapshot settled before the body runs, torn down after.
const CALLER = "acc-me";
const AUTHOR = "acc-cook";
const CHANNEL = "chan-1";

async function withLiveStore(
	fake: FakeComms,
	body: (store: AppStore, settled: () => Promise<void>) => Promise<void>,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({ comms: fake.client, callerId: CALLER });
	});
	// Every hop of the driver's snapshot round-trip is a resolved promise, so a
	// bounded microtask drain is deterministic — no timers, no wall-clock wait.
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	try {
		await settled();
		await body(store, settled);
	} finally {
		fake.close();
		dispose();
	}
}

// Find a channel in the fixture whose messages group into a thread that has at
// least one reply, and return the channel id plus the thread's root and first
// reply. Derived (not hardcoded) so the tests survive a fixture reshuffle: they
// assert the open-thread contract relative to whatever real thread they find. A
// reply id is, by construction, NOT a root id — the guard test needs exactly
// that distinction.
function threadInStore(store: AppStore): {
	channelId: string;
	rootId: string;
	replyId: string;
} {
	for (const chan of store.channels()) {
		for (const thread of threadsOf(store.messages(), chan.id)) {
			if (thread.replies.length > 0) {
				return {
					channelId: chan.id,
					rootId: thread.root.id,
					replyId: thread.replies[0].id,
				};
			}
		}
	}
	throw new Error(
		"fixture has no channel with a threaded reply — open-thread tests need one",
	);
}

// A standalone channel (kind "channel", not a 1:1 agent DM) other than the one
// carrying the thread. `openChannel` on a 1:1 agent DM delegates to `openAgent`
// (a different transition), so restricting to a plain channel keeps the test on
// the channel-switch path. Derived so it isn't pinned to a fixture id.
function otherChannelId(store: AppStore, exclude: string): string {
	const chan = store
		.channels()
		.find((c) => c.kind === "channel" && c.id !== exclude);
	if (!chan) throw new Error("fixture has no second standalone channel");
	return chan.id;
}

describe("open-thread state", () => {
	// The root id starts empty — the anchor every open/close transition builds on.
	test("openThreadRootId is null on a fresh store", () => {
		withStore((s) => {
			expect(s.openThreadRootId()).toBeNull();
		});
	});

	// Opening a valid root id surfaces exactly that id through the accessor.
	test("openThread(rootId) sets openThreadRootId to that root", () => {
		withStore((s) => {
			const { rootId } = threadInStore(s);
			s.openThread(rootId);
			expect(s.openThreadRootId()).toBe(rootId);
		});
	});

	// Closing returns the state to null after an open — proving close is a real
	// reset, not a no-op that leaves the last-opened id in place.
	test("closeThread resets openThreadRootId to null", () => {
		withStore((s) => {
			const { rootId } = threadInStore(s);
			s.openThread(rootId);
			expect(s.openThreadRootId()).toBe(rootId);

			s.closeThread();
			expect(s.openThreadRootId()).toBeNull();
		});
	});
});

describe("open-thread root-only guard", () => {
	// A reply id is not a root: `openThread` must resolve through
	// threadsOf(messages, channelId) and reject a non-root, so the state stays
	// null. Passing a reply id here would open a "thread" that isn't one.
	test("openThread with a reply id is a no-op", () => {
		withStore((s) => {
			const { replyId } = threadInStore(s);
			s.openThread(replyId);
			expect(s.openThreadRootId()).toBeNull();
		});
	});

	// An id that matches no message at all must also no-op — the guard rejects
	// unknown ids, not just non-root ones.
	test("openThread with an unknown id is a no-op", () => {
		withStore((s) => {
			s.openThread("msg-does-not-exist");
			expect(s.openThreadRootId()).toBeNull();
		});
	});
});

describe("selection closes an open thread", () => {
	// The open-thread id is channel-scoped state: routing the standalone surface
	// to a different channel must drop it. A close hook missing from openChannel
	// would leave a stale root id pointing into the previous channel.
	test("openChannel to a different channel closes the open thread", () => {
		withStore((s) => {
			const { channelId, rootId } = threadInStore(s);
			s.openThread(rootId);
			expect(s.openThreadRootId()).toBe(rootId);

			s.openChannel(otherChannelId(s, channelId));
			expect(s.openThreadRootId()).toBeNull();
		});
	});

	// Likewise, entering an agent workspace drops the channel-scoped thread state.
	test("openAgent closes the open thread", () => {
		withStore((s) => {
			const { rootId } = threadInStore(s);
			s.openThread(rootId);
			expect(s.openThreadRootId()).toBe(rootId);

			s.openAgent(STUB_AGENTS[0].account.id);
			expect(s.openThreadRootId()).toBeNull();
		});
	});
});

// postReply used to append an in-memory Message with a minted `msg-local-N` id
// — the fixture-backed no-op standing in for PostMessage. It is now the real
// wire call, so the two tests that pinned the minted id and the local append are
// REPLACED (not deleted) by the same contract restated against the live
// behavior: the reply goes out as a PostMessage carrying parentMessageId, and
// the store inserts NOTHING locally — the SubscribeComms echo renders it, which
// upsertMessage dedups by id so it appears exactly once.
describe("postReply", () => {
	// The replacement for "appends a threaded message under the parent": the
	// reply reaches the wire with the parent, the channel, the text and an
	// idempotency key — and the message list is untouched until the echo, at
	// which point threadsOf groups the echoed reply under the root.
	test("issues a PostMessage under the parent and renders only the echo", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER), wireAccount(AUTHOR)],
			channels: [wireChannel(CHANNEL, CALLER)],
			messagesByChannel: {
				[CHANNEL]: [
					wireTextMessage({
						id: "m-root",
						channelId: CHANNEL,
						authorAccountId: AUTHOR,
						atUnixMs: 100,
						text: "the root",
					}),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			expect(store.messages().map((m) => m.id)).toEqual(["m-root"]);

			await store.postReply(CHANNEL, "m-root", "hello thread");

			// (a) the wire carries the reply contract.
			expect(fake.posts.length).toBe(1);
			const [post] = fake.posts;
			expect(post.channelId).toBe(CHANNEL);
			expect(post.parentMessageId).toBe("m-root");
			expect(post.text).toBe("hello thread");
			expect(post.clientRequestId.length).toBeGreaterThan(0);

			// (b) nothing was inserted locally.
			expect(store.messages().map((m) => m.id)).toEqual(["m-root"]);

			// (c) the echo renders it, threaded under the root.
			await fake.emit(
				{
					case: "messagePosted",
					value: {
						message: wireTextMessage({
							id: "m-reply",
							channelId: CHANNEL,
							authorAccountId: CALLER,
							atUnixMs: 200,
							text: "hello thread",
							parentMessageId: "m-root",
						}),
					},
				},
				1n,
			);
			await settled();

			const thread = threadsOf(store.messages(), CHANNEL).find(
				(t) => t.root.id === "m-root",
			);
			expect(thread?.replies.map((r) => r.id)).toEqual(["m-reply"]);
		});
	});

	// The replacement for "two replies mint distinct ids": ids are now the
	// SERVER's, so what must stay distinct is the per-post idempotency key — a
	// reused key would have the server dedup the second reply away as a retry of
	// the first, silently losing it.
	test("two replies under one root carry distinct idempotency keys", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL, CALLER)],
			messagesByChannel: {
				[CHANNEL]: [
					wireTextMessage({
						id: "m-root",
						channelId: CHANNEL,
						authorAccountId: CALLER,
						atUnixMs: 100,
						text: "the root",
					}),
				],
			},
		});

		await withLiveStore(fake, async (store) => {
			await store.postReply(CHANNEL, "m-root", "first reply");
			await store.postReply(CHANNEL, "m-root", "second reply");

			expect(fake.posts.map((p) => p.text)).toEqual([
				"first reply",
				"second reply",
			]);
			expect(fake.posts.every((p) => p.parentMessageId === "m-root")).toBe(
				true,
			);
			expect(fake.posts[0].clientRequestId).not.toBe(
				fake.posts[1].clientRequestId,
			);
		});
	});
});
