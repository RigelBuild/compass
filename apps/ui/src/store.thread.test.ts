import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import { threadsOf } from "./comms";
import { type AppStore, CALLER_ID, createAppStore } from "./store";
import { STUB_AGENTS } from "./stub-data";

// The withStore harness, copied verbatim from store.test.ts:26-56. The store
// exposes SolidJS accessors that only compute inside a reactive root, so every
// assertion runs inside a fresh `createRoot` that is disposed afterward — no
// effects, no waiting, no cross-test leakage. Thread actions are synchronous and
// the `openThreadRootId` accessor recomputes on demand within the root.
function withStore(body: (store: AppStore) => void): void {
	createRoot((dispose) => {
		const store = createAppStore();
		try {
			body(store);
		} finally {
			dispose();
		}
	});
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

describe("postReply", () => {
	// Posting a reply appends one in-memory Message under the parent, carrying the
	// caller as author, a single text block, a minted local id, and a numeric
	// timestamp — the PostMessage seam's fixture-backed no-op. The reply is
	// observable through the public accessors: the message list grows by one and
	// threadsOf now groups the new message under the root's replies.
	test("appends a threaded message authored by the caller under the parent", () => {
		withStore((s) => {
			const { channelId, rootId } = threadInStore(s);
			const before = s.messages().length;

			s.postReply(channelId, rootId, "hello thread");

			// (a) exactly one message was appended.
			expect(s.messages().length).toBe(before + 1);

			// (b) the appended message carries the reply contract. Identify it by
			// its minted local id so the assertion targets the new message, not a
			// fixture one.
			const posted = s
				.messages()
				.find((m) => /^msg-local-/.test(m.id) && m.parentMessageId === rootId);
			expect(posted).toBeDefined();
			if (!posted) throw new Error("postReply did not append a message");
			expect(posted.parentMessageId).toBe(rootId);
			expect(posted.authorAccountId).toBe(CALLER_ID);
			expect(posted.channelId).toBe(channelId);
			expect(posted.blocks).toEqual([{ kind: "text", text: "hello thread" }]);
			expect(posted.id).toMatch(/^msg-local-/);
			expect(typeof posted.atUnixMs).toBe("number");

			// (c) the new message threads under the root: threadsOf now lists it
			// among that root's replies.
			const thread = threadsOf(s.messages(), channelId).find(
				(t) => t.root.id === rootId,
			);
			expect(thread).toBeDefined();
			expect(thread?.replies.map((r) => r.id)).toContain(posted.id);
		});
	});

	// The minted id is a monotonic counter (`msg-local-${++localReplyCount}`):
	// two replies under the same root must append two DISTINCT messages, both
	// carrying fresh local ids and both threading under the root — never a reused
	// id (which would collide in the message list) and never a new root. Pins the
	// ++localReplyCount contract that a reset-to-a-constant or a shared-id bug
	// would break.
	test("two replies under one root mint distinct threaded ids", () => {
		withStore((s) => {
			const { channelId, rootId } = threadInStore(s);
			const before = s.messages().length;

			s.postReply(channelId, rootId, "first reply");
			s.postReply(channelId, rootId, "second reply");

			// (a) exactly two messages were appended.
			expect(s.messages().length).toBe(before + 2);

			// (b) the two minted messages are the local ones threading under the
			// root; both ids match the local-mint shape and are DISTINCT.
			const minted = s
				.messages()
				.filter(
					(m) => /^msg-local-/.test(m.id) && m.parentMessageId === rootId,
				);
			expect(minted.length).toBe(2);
			expect(minted[0].id).toMatch(/^msg-local-/);
			expect(minted[1].id).toMatch(/^msg-local-/);
			expect(minted[0].id).not.toBe(minted[1].id);

			// (c) both thread under the root: threadsOf groups them as replies of
			// that root, not as new roots of their own.
			const threads = threadsOf(s.messages(), channelId);
			const thread = threads.find((t) => t.root.id === rootId);
			expect(thread).toBeDefined();
			const replyIds = thread?.replies.map((r) => r.id) ?? [];
			expect(replyIds).toContain(minted[0].id);
			expect(replyIds).toContain(minted[1].id);
			// neither minted id surfaced as a root of its own.
			const rootIds = threads.map((t) => t.root.id);
			expect(rootIds).not.toContain(minted[0].id);
			expect(rootIds).not.toContain(minted[1].id);
		});
	});
});
