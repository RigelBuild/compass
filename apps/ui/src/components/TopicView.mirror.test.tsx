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
