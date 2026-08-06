import { describe, expect, test } from "bun:test";
import { ChannelPostPolicy } from "@compass/client";
import { fireEvent, render } from "@solidjs/testing-library";
import { STUB_CHANNELS, STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import {
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireChannel,
	wireTextMessage,
	wireTopic,
} from "../live/comms-fake";
import { type AppStore, CALLER_ID, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { ChannelView } from "./ChannelView";
import { TopicView } from "./TopicView";

// Post-policy gating on the two channel write affordances (comms substrate
// §T8/§A2): an `owner_only` channel admits only its owner. The composer (in
// TopicView) and the "new topic" affordance (in ChannelView's topic index) both
// post through store.postMessage, so both must render DISABLED with an
// owner-only hint for a non-owner — the honest-disabled pattern (mirroring
// LeftSidebar's fixed subscribe toggle): visibly disabled with a reason, never
// hidden, never offered. The server would reject the post; the UI must not
// offer it. The membership gate stays; the policy gate sits beside it (disabled
// if EITHER fails).

const CALLER = "acc-me";
const OWNER = "acc-owner";
const CHANNEL = "chan-live";
const TOPIC = "top-live";

// A live snapshot whose one channel is owner_only, owned by `ownerId`. The
// caller is a member+subscriber (membership "subscribed"), so ONLY the post
// policy governs the composer's enablement.
const ownerOnlySnapshot = (ownerId: string) => {
	const chan = wireChannel(CHANNEL, CALLER);
	chan.postPolicy = ChannelPostPolicy.OWNER_ONLY;
	chan.ownerAccountId = ownerId;
	return {
		accounts: [wireAccount(CALLER), wireAccount(OWNER)],
		channels: [chan],
		topicsByChannel: {
			[CHANNEL]: [
				wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" }),
			],
		},
		messagesByChannel: {
			[CHANNEL]: [
				wireTextMessage({
					id: "m-existing",
					topicId: TOPIC,
					authorAccountId: OWNER,
					atUnixMs: 100,
					text: "already here",
				}),
			],
		},
	};
};

const composerInput = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".conv-main .conv-composer input.field");
const composerSend = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>(".conv-main .conv-composer .send");

// Mount TopicView over the live fake store (CALLER identity), drain the snapshot
// round-trip, then open the primary topic so the composer is bound.
async function mountComposer(fake: FakeComms): Promise<{
	store: AppStore;
	container: HTMLElement;
	settled: () => Promise<void>;
}> {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext.Provider value={store}>
				<TopicView />
			</StoreContext.Provider>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	await settled();
	store.openTopic(TOPIC);
	await settled();
	return { store, container, settled };
}

describe("composer post-policy gating (T8)", () => {
	// The named red: a non-owner on an owner_only channel gets a disabled
	// composer, the owner-only hint, and a send attempt posts nothing.
	test("non-owner composer disabled", async () => {
		const fake = createFakeComms(ownerOnlySnapshot(OWNER));
		const { container, settled } = await mountComposer(fake);
		try {
			const input = composerInput(container);
			const send = composerSend(container);
			if (!input || !send) throw new Error("composer did not render");

			expect(input.disabled).toBe(true);
			expect(send.disabled).toBe(true);
			// The honest-disabled reason is visible.
			const hint = container.querySelector(
				".conv-main .conv-composer-policy-hint",
			);
			expect(hint?.textContent).toContain("Owner-only channel");

			// A send attempt records NO post. (fireEvent still dispatches on a
			// disabled control in happy-dom, so this proves the send() guard, not
			// just the disabled attribute.)
			fireEvent.click(send);
			await settled();
			expect(fake.posts).toEqual([]);
		} finally {
			fake.close();
		}
	});

	// The contrast: the caller OWNS the owner_only channel → composer enabled and
	// a post goes through, proving the gate keys on the owner id, not merely on
	// the owner_only policy.
	test("owner composer enabled and posts through", async () => {
		const fake = createFakeComms(ownerOnlySnapshot(CALLER));
		const { container, settled } = await mountComposer(fake);
		try {
			const input = composerInput(container);
			const send = composerSend(container);
			if (!input || !send) throw new Error("composer did not render");

			expect(input.disabled).toBe(false);
			// No owner-only hint when the caller may post.
			expect(
				container.querySelector(".conv-main .conv-composer-policy-hint"),
			).toBeNull();

			fireEvent.input(input, { target: { value: "an owner directive" } });
			fireEvent.click(send);
			await settled();

			expect(fake.posts.length).toBe(1);
			expect(fake.posts[0].text).toBe("an owner directive");
			expect(fake.posts[0].topic).toEqual({ case: "topicId", value: TOPIC });
		} finally {
			fake.close();
		}
	});
});

// NewTopic gating over the offline fixture store (the same convention as
// ChannelView.test.tsx's membership tests). The fixture pins the contrast:
// `ch-announcements` is owner_only owned by acc-supervisor (the fixture caller
// acc-matt is NOT the owner → gated); `ch-coordination` is owner_only owned by
// MATT (the caller IS the owner → enabled).
const NON_OWNER_CHANNEL = (() => {
	const c = STUB_CHANNELS.find(
		(ch) =>
			ch.postPolicy === "owner_only" &&
			ch.ownerAccountId !== CALLER_ID &&
			ch.membership !== "none",
	);
	if (!c)
		throw new Error(
			"fixture has no owner_only channel the caller can't post to",
		);
	return c;
})();

const OWNER_CHANNEL = (() => {
	const c = STUB_CHANNELS.find(
		(ch) => ch.postPolicy === "owner_only" && ch.ownerAccountId === CALLER_ID,
	);
	if (!c) throw new Error("fixture has no owner_only channel the caller owns");
	return c;
})();

function mountChannelView(channelId: string): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const channel = STUB_CHANNELS.find((c) => c.id === channelId);
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext.Provider value={store}>
				<ChannelView channel={channel} />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

const newTopicName = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".new-topic .new-topic-name");
const newTopicMessage = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".new-topic .new-topic-message");
const newTopicStart = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>(".new-topic .new-topic-start");

describe("new-topic post-policy gating (T8)", () => {
	test("non-owner cannot start a topic on an owner_only channel", () => {
		const { container } = mountChannelView(NON_OWNER_CHANNEL.id);
		const name = newTopicName(container);
		const message = newTopicMessage(container);
		const start = newTopicStart(container);
		if (!name || !message || !start) {
			throw new Error("new-topic affordance did not render");
		}

		expect(name.disabled).toBe(true);
		expect(message.disabled).toBe(true);
		expect(start.disabled).toBe(true);
		expect(
			container.querySelector(".new-topic .new-topic-policy-hint")?.textContent,
		).toContain("Owner-only channel");
	});

	test("owner can start a topic on an owner_only channel they own", () => {
		const { container } = mountChannelView(OWNER_CHANNEL.id);
		const name = newTopicName(container);
		const message = newTopicMessage(container);
		const start = newTopicStart(container);
		if (!name || !message || !start) {
			throw new Error("new-topic affordance did not render");
		}

		// Fields enabled; no owner-only hint.
		expect(name.disabled).toBe(false);
		expect(message.disabled).toBe(false);
		expect(
			container.querySelector(".new-topic .new-topic-policy-hint"),
		).toBeNull();

		// With both fields filled the start button enables (the write is offered).
		fireEvent.input(name, { target: { value: "a new directive" } });
		fireEvent.input(message, { target: { value: "first line" } });
		expect(start.disabled).toBe(false);
	});
});
