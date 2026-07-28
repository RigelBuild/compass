import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import {
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireChannel,
	wireTextMessage,
} from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { ChannelView } from "./ChannelView";

// The channel composer's POSTING contract. Before this slice the send button
// carried no onClick at all — a human could not post. It now issues the wire
// PostMessage through store.postMessage and lets the SubscribeComms echo render
// the result. These tests mount the standalone ChannelView over a store backed
// by the CommsClient double (live/comms-fake.ts) and assert the user-visible
// contract end to end: click and Enter both send, the draft clears, the message
// appears only on the echo, and a failure keeps the user's text.
//
// The membership-based enablement and the ask surface live in
// ChannelView.test.tsx (offline, fixture-backed); this suite is the live write
// path only.

const CALLER = "acc-me";
const CHANNEL = "chan-live";
// A SECOND subscribed channel, so a test can switch the surface between two
// truthy channels — the exact transition an unkeyed `<Show>` memoizes away.
const CHANNEL_B = "chan-live-b";

const snapshot = () => ({
	accounts: [wireAccount(CALLER)],
	channels: [wireChannel(CHANNEL, CALLER)],
	messagesByChannel: {
		[CHANNEL]: [
			wireTextMessage({
				id: "m-existing",
				channelId: CHANNEL,
				authorAccountId: CALLER,
				atUnixMs: 100,
				text: "already here",
			}),
		],
	},
});

/** The same server plus a second subscribed channel. `CHANNEL` stays first so
 *  the store's boot selection is unchanged from the single-channel snapshot. */
const twoChannelSnapshot = () => ({
	...snapshot(),
	channels: [wireChannel(CHANNEL, CALLER), wireChannel(CHANNEL_B, CALLER)],
});

// Re-queried after every channel switch: the composer is a FRESH instance per
// channel, so a reference captured before the switch is a detached node.
const composerInput = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".conv-main .conv-composer input.field");
const composerSend = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>(".conv-main .conv-composer .send");

/** Mount the standalone ChannelView over a live store and wait out the driver's
 *  snapshot round-trip, so the composer is bound to a real (subscribed) channel
 *  before the body runs. Every hop is a resolved promise, so the bounded
 *  microtask drain is deterministic — no timers. */
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
				<ChannelView />
			</StoreContext.Provider>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	await settled();
	const input = container.querySelector<HTMLInputElement>(
		".conv-composer input.field",
	);
	const send = container.querySelector<HTMLButtonElement>(
		".conv-composer .send",
	);
	if (!input || !send) throw new Error("composer did not render");
	return { store, input, send, container, settled };
}

describe("channel composer (live PostMessage)", () => {
	// The core dogfood leg: typing a line and clicking send puts a ROOT
	// PostMessage on the wire for the open channel, and the draft clears.
	// Mutation-check: the pre-slice inert button (no onClick) records no post.
	test("send posts the typed text to the open channel and clears the draft", async () => {
		const fake = createFakeComms(snapshot());
		const { input, send, settled } = await mountComposer(fake);
		try {
			fireEvent.input(input, { target: { value: "hello from the composer" } });
			fireEvent.click(send);
			await settled();

			expect(fake.posts.length).toBe(1);
			expect(fake.posts[0].channelId).toBe(CHANNEL);
			expect(fake.posts[0].text).toBe("hello from the composer");
			// A root post: no parent.
			expect(fake.posts[0].parentMessageId).toBe("");
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
				channelId: CHANNEL,
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
	// posts nothing, so a stray keystroke can't spam the channel with blanks.
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

	// A draft is scoped to the channel it was typed in. Switching the surface to
	// a DIFFERENT channel must present an empty composer — an unkeyed `<Show>`
	// reuses the one Composer instance across truthy channels, so its `draft`
	// signal survives and the next send posts channel A's private text into
	// channel B.
	test("an unsent draft does not survive a channel switch", async () => {
		const fake = createFakeComms(twoChannelSnapshot());
		const { store, container, settled } = await mountComposer(fake);
		try {
			const input = composerInput(container);
			if (!input) throw new Error("composer did not render");
			fireEvent.input(input, { target: { value: "meant for chan-a" } });

			store.openChannel(CHANNEL_B);
			await settled();

			const switched = composerInput(container);
			if (!switched) throw new Error("composer did not render after switch");
			expect(switched.value).toBe("");

			// …and what is typed now posts to B, carrying ONLY the new text.
			fireEvent.input(switched, { target: { value: "meant for chan-b" } });
			const send = composerSend(container);
			if (!send) throw new Error("send did not render after switch");
			fireEvent.click(send);
			await settled();

			expect(fake.posts.length).toBe(1);
			expect(fake.posts[0].channelId).toBe(CHANNEL_B);
			expect(fake.posts[0].text).toBe("meant for chan-b");
		} finally {
			fake.close();
		}
	});

	// The failure state is channel-scoped too: A's rejected post leaves an error
	// and a restored draft in A, and neither may follow the user into B.
	test("a failed post's error and restored text do not follow a channel switch", async () => {
		const fake = createFakeComms(twoChannelSnapshot());
		const { store, container, settled } = await mountComposer(fake);
		try {
			fake.failNextPost(new Error("door is shut"));

			const input = composerInput(container);
			if (!input) throw new Error("composer did not render");
			fireEvent.input(input, { target: { value: "secret for chan-a" } });
			fireEvent.click(composerSend(container) as HTMLButtonElement);
			await settled();

			// the failure landed in A…
			expect(input.value).toBe("secret for chan-a");
			expect(
				container.querySelector(".conv-main .conv-composer-error")?.textContent,
			).toContain("door is shut");

			store.openChannel(CHANNEL_B);
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
