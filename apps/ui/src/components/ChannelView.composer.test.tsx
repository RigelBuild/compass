import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import {
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireChannel,
	wireTextMessage,
	wireTopic,
} from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { TopicView } from "./TopicView";

// The topic composer's POSTING contract. In the two-level model a message is
// posted into a TOPIC (a topic IS the thread), so the composer lives in
// TopicView and posts through store.postMessage with the topic oneof
// {case:"topicId"}. The SubscribeComms echo renders the result. These tests
// mount TopicView over a store backed by the CommsClient double
// (live/comms-fake.ts) and assert the user-visible contract end to end: click
// and Enter both send, the draft clears, the message appears only on the echo,
// and a failure keeps the user's text. Switching between two topics scopes the
// draft to each.
//
// The membership-based enablement and the ask surface live in
// ChannelView.test.tsx (offline, fixture-backed); this suite is the live write
// path only.

const CALLER = "acc-me";
const CHANNEL = "chan-live";
const TOPIC = "top-live";
// A SECOND topic in the same channel, so a test can switch the surface between
// two topics — the exact transition an unkeyed composer memoizes away.
const TOPIC_B = "top-live-b";

const snapshot = () => ({
	accounts: [wireAccount(CALLER)],
	channels: [wireChannel(CHANNEL, CALLER)],
	topicsByChannel: {
		[CHANNEL]: [wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" })],
	},
	messagesByChannel: {
		[CHANNEL]: [
			wireTextMessage({
				id: "m-existing",
				topicId: TOPIC,
				authorAccountId: CALLER,
				atUnixMs: 100,
				text: "already here",
			}),
		],
	},
});

/** The same server plus a second topic in the channel. `TOPIC` stays first so
 *  the deep-link selection below is unambiguous. */
const twoTopicSnapshot = () => ({
	...snapshot(),
	topicsByChannel: {
		[CHANNEL]: [
			wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" }),
			wireTopic({ id: TOPIC_B, channelId: CHANNEL, name: "secondary" }),
		],
	},
});

// Re-queried after every topic switch: the composer is a FRESH instance per
// topic, so a reference captured before the switch is a detached node.
const composerInput = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".conv-main .conv-composer input.field");
const composerSend = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>(".conv-main .conv-composer .send");

/** Mount TopicView over a live store, wait out the driver's snapshot round-trip,
 *  then open the primary topic so the composer is bound before the body runs.
 *  Every hop is a resolved promise, so the bounded microtask drain is
 *  deterministic — no timers. */
async function mountComposer(fake: FakeComms): Promise<{
	store: AppStore;
	input: HTMLInputElement;
	send: HTMLButtonElement;
	container: HTMLElement;
	settled: () => Promise<void>;
}> {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({ comms: fake.client, callerId: CALLER });
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
	const input = composerInput(container);
	const send = composerSend(container);
	if (!input || !send) throw new Error("composer did not render");
	return { store, input, send, container, settled };
}

describe("topic composer (live PostMessage)", () => {
	// The core dogfood leg: typing a line and clicking send puts a PostMessage
	// on the wire for the open topic, and the draft clears.
	// Mutation-check: the pre-slice inert button (no onClick) records no post.
	test("send posts the typed text to the open topic and clears the draft", async () => {
		const fake = createFakeComms(snapshot());
		const { input, send, settled } = await mountComposer(fake);
		try {
			fireEvent.input(input, { target: { value: "hello from the composer" } });
			fireEvent.click(send);
			await settled();

			expect(fake.posts.length).toBe(1);
			expect(fake.posts[0].channelId).toBe(CHANNEL);
			expect(fake.posts[0].text).toBe("hello from the composer");
			// Posted into the open topic.
			expect(fake.posts[0].topic).toEqual({ case: "topicId", value: TOPIC });
			expect(fake.posts[0].clientRequestId.length).toBeGreaterThan(0);
			expect(input.value).toBe("");
		} finally {
			fake.close();
		}
	});

	// Enter sends too — the affordance a human actually uses. Shift+Enter does
	// NOT (it is the newline escape), so a multi-line draft is still possible.
	test("Enter sends; Shift+Enter does not", async () => {
		const fake = createFakeComms(snapshot());
		const { input, settled } = await mountComposer(fake);
		try {
			fireEvent.input(input, { target: { value: "sent with enter" } });
			fireEvent.keyDown(input, { key: "Enter" });
			await settled();

			expect(fake.posts.map((p) => p.text)).toEqual(["sent with enter"]);

			fireEvent.input(input, { target: { value: "not sent" } });
			fireEvent.keyDown(input, { key: "Enter", shiftKey: true });
			await settled();

			expect(fake.posts.map((p) => p.text)).toEqual(["sent with enter"]);
			expect(input.value).toBe("not sent");
		} finally {
			fake.close();
		}
	});

	// The no-optimistic-update ruling, at the DOM: the posted text does not
	// appear in the stream until the server echoes the stored message back —
	// and then exactly once, never twice. A composer that hand-inserted the
	// sent message would show it before the echo and duplicate it after.
	test("the sent message renders only on the stream echo, exactly once", async () => {
		const fake = createFakeComms(snapshot());
		const { container, input, send, settled } = await mountComposer(fake);
		try {
			fireEvent.input(input, { target: { value: "echo-me" } });
			fireEvent.click(send);
			await settled();

			const stream = () => container.querySelector(".conv-stream")?.textContent;
			expect(stream()).not.toContain("echo-me");

			const stored = wireTextMessage({
				id: "m-stored",
				topicId: TOPIC,
				authorAccountId: CALLER,
				atUnixMs: 200,
				text: "echo-me",
			});
			await fake.emit(
				{ case: "messagePosted", value: { message: stored } },
				1n,
			);
			await settled();
			expect(stream()).toContain("echo-me");

			// A redelivery of the same stored message must not render a second row.
			await fake.emit(
				{ case: "messagePosted", value: { message: stored } },
				2n,
			);
			await settled();

			const occurrences = (stream() ?? "").split("echo-me").length - 1;
			expect(occurrences).toBe(1);
		} finally {
			fake.close();
		}
	});

	// A failed post must not eat the user's message: the text comes back into
	// the (still empty) field and the error is surfaced beside it.
	test("a failed post restores the typed text and shows the error", async () => {
		const fake = createFakeComms(snapshot());
		const { container, input, send, settled } = await mountComposer(fake);
		try {
			fake.failNextPost(new Error("door is shut"));

			fireEvent.input(input, { target: { value: "precious words" } });
			fireEvent.click(send);
			await settled();

			expect(input.value).toBe("precious words");
			expect(
				container.querySelector(".conv-composer-error")?.textContent,
			).toContain("door is shut");
		} finally {
			fake.close();
		}
	});

	// …but a restore must never clobber what the user has since typed: if they
	// started the next message before the rejection landed, THEIR text stands.
	test("a failed post does not overwrite text typed since the send", async () => {
		const fake = createFakeComms(snapshot());
		const { input, send, settled } = await mountComposer(fake);
		try {
			fake.failNextPost(new Error("door is shut"));

			fireEvent.input(input, { target: { value: "first message" } });
			fireEvent.click(send);
			// The user keeps typing before the rejection resolves.
			fireEvent.input(input, { target: { value: "second message" } });
			await settled();

			expect(input.value).toBe("second message");
		} finally {
			fake.close();
		}
	});

	// A whitespace-only draft is not a message: send stays disabled and Enter
	// posts nothing, so a stray keystroke can't spam the topic with blanks.
	test("a blank draft sends nothing", async () => {
		const fake = createFakeComms(snapshot());
		const { input, send, settled } = await mountComposer(fake);
		try {
			fireEvent.input(input, { target: { value: "   " } });
			expect(send.disabled).toBe(true);

			fireEvent.keyDown(input, { key: "Enter" });
			await settled();

			expect(fake.posts).toEqual([]);
		} finally {
			fake.close();
		}
	});

	// A draft is scoped to the topic it was typed in. Switching the surface to
	// a DIFFERENT topic must present an empty composer — a keyed composer per
	// topic guarantees its `draft` signal does not survive and the next send
	// cannot post topic A's private text into topic B.
	test("an unsent draft does not survive a topic switch", async () => {
		const fake = createFakeComms(twoTopicSnapshot());
		const { store, container, settled } = await mountComposer(fake);
		try {
			const input = composerInput(container);
			if (!input) throw new Error("composer did not render");
			fireEvent.input(input, { target: { value: "meant for topic-a" } });

			store.openTopic(TOPIC_B);
			await settled();

			const switched = composerInput(container);
			if (!switched) throw new Error("composer did not render after switch");
			expect(switched.value).toBe("");

			// …and what is typed now posts to B, carrying ONLY the new text.
			fireEvent.input(switched, { target: { value: "meant for topic-b" } });
			const send = composerSend(container);
			if (!send) throw new Error("send did not render after switch");
			fireEvent.click(send);
			await settled();

			expect(fake.posts.length).toBe(1);
			expect(fake.posts[0].topic).toEqual({ case: "topicId", value: TOPIC_B });
			expect(fake.posts[0].text).toBe("meant for topic-b");
		} finally {
			fake.close();
		}
	});

	// The failure state is topic-scoped too: A's rejected post leaves an error
	// and a restored draft in A, and neither may follow the user into B.
	test("a failed post's error and restored text do not follow a topic switch", async () => {
		const fake = createFakeComms(twoTopicSnapshot());
		const { store, container, settled } = await mountComposer(fake);
		try {
			fake.failNextPost(new Error("door is shut"));

			const input = composerInput(container);
			if (!input) throw new Error("composer did not render");
			fireEvent.input(input, { target: { value: "secret for topic-a" } });
			fireEvent.click(composerSend(container) as HTMLButtonElement);
			await settled();

			// the failure landed in A…
			expect(input.value).toBe("secret for topic-a");
			expect(
				container.querySelector(".conv-main .conv-composer-error")?.textContent,
			).toContain("door is shut");

			store.openTopic(TOPIC_B);
			await settled();

			// …and stayed there.
			expect(
				container.querySelector(".conv-main .conv-composer-error"),
			).toBeNull();
			expect(composerInput(container)?.value).toBe("");
		} finally {
			fake.close();
		}
	});
});
