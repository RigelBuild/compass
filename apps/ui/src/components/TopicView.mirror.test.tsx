import { expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { StoreContext } from "../context";
import {
	createFakeComms,
	wireAccount,
	wireChannel,
	wireTextMessage,
	wireTopic,
} from "../live/comms-fake";
import { createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { ViewContext } from "../view-scope";
import { TopicView } from "./TopicView";

const CALLER = "acc-me";
const CHANNEL = "chan-live";
const TOPIC = "top-live";
const TOPIC_B = "top-live-b";

test("topic header mirrors its channel and topic to the entered ref", async () => {
	const fake = createFakeComms({
		accounts: [wireAccount(CALLER)],
		channels: [wireChannel(CHANNEL, CALLER)],
		topicsByChannel: {
			[CHANNEL]: [
				wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" }),
			],
		},
		messagesByChannel: {
			[CHANNEL]: [
				wireTextMessage({
					id: "message-1",
					topicId: TOPIC,
					authorAccountId: CALLER,
					atUnixMs: 1,
					text: "A topic message",
				}),
			],
		},
	});
	let store!: ReturnType<typeof createAppStore>;
	const { container, unmount } = render(() => {
		store = createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<ViewContext value={store.focusedView()}>
					<TopicView />
				</ViewContext>
			</StoreContext>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	try {
		await settled();
		store.openTopic(TOPIC);
		await settled();
		const open = container.querySelector<HTMLButtonElement>(
			'button[aria-label="Mirror to issue"]',
		);
		if (!open) throw new Error("topic mirror control did not render");
		fireEvent.click(open);
		flush();
		const ref = container.querySelector<HTMLInputElement>(
			'input[aria-label="Issue or PR ref"]',
		);
		if (!ref) throw new Error("topic mirror ref input did not render");
		fireEvent.input(ref, { target: { value: "owner/repo#23" } });
		flush();
		const submit = container.querySelector<HTMLButtonElement>(".mirror-submit");
		if (!submit) throw new Error("topic mirror submit did not render");
		fireEvent.click(submit);
		await settled();

		expect(fake.mirrors).toEqual([
			{
				ref: "owner/repo#23",
				askId: "",
				topicId: TOPIC,
				messageIds: [],
				channelId: CHANNEL,
			},
		]);
		expect(
			container.querySelector<HTMLAnchorElement>(".mirror-result a")?.href,
		).toBe("https://github.com/owner/repo/issues/12#issuecomment-1");
	} finally {
		fake.close();
		unmount();
	}
});

test("switching topics resets the mirror form and submits the new topic", async () => {
	const fake = createFakeComms({
		accounts: [wireAccount(CALLER)],
		channels: [wireChannel(CHANNEL, CALLER)],
		topicsByChannel: {
			[CHANNEL]: [
				wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" }),
				wireTopic({ id: TOPIC_B, channelId: CHANNEL, name: "secondary" }),
			],
		},
		messagesByChannel: { [CHANNEL]: [] },
	});
	let store!: ReturnType<typeof createAppStore>;
	const { container, unmount } = render(() => {
		store = createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<ViewContext value={store.focusedView()}>
					<TopicView />
				</ViewContext>
			</StoreContext>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	try {
		await settled();
		store.openTopic(TOPIC);
		await settled();
		const firstButton = container.querySelector<HTMLButtonElement>(
			'button[aria-label="Mirror to issue"]',
		);
		if (!firstButton) throw new Error("topic mirror control did not render");
		fireEvent.click(firstButton);
		flush();
		const firstRef = container.querySelector<HTMLInputElement>(
			'input[aria-label="Issue or PR ref"]',
		);
		if (!firstRef) throw new Error("topic mirror ref input did not render");
		fireEvent.input(firstRef, { target: { value: "owner/repo#23" } });
		flush();

		store.openTopic(TOPIC_B);
		await settled();
		flush();
		expect(
			container.querySelector<HTMLInputElement>(
				'input[aria-label="Issue or PR ref"]',
			),
		).toBeNull();

		const secondButton = container.querySelector<HTMLButtonElement>(
			'button[aria-label="Mirror to issue"]',
		);
		if (!secondButton)
			throw new Error("new topic mirror control did not render");
		fireEvent.click(secondButton);
		flush();
		const secondRef = container.querySelector<HTMLInputElement>(
			'input[aria-label="Issue or PR ref"]',
		);
		if (!secondRef)
			throw new Error("new topic mirror ref input did not render");
		fireEvent.input(secondRef, { target: { value: "owner/repo#24" } });
		flush();
		const submit = container.querySelector<HTMLButtonElement>(".mirror-submit");
		if (!submit) throw new Error("topic mirror submit did not render");
		fireEvent.click(submit);
		await settled();

		expect(fake.mirrors).toEqual([
			{
				ref: "owner/repo#24",
				askId: "",
				topicId: TOPIC_B,
				messageIds: [],
				channelId: CHANNEL,
			},
		]);
	} finally {
		fake.close();
		unmount();
	}
});
