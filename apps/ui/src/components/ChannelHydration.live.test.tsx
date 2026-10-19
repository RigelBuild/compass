import { describe, expect, test } from "bun:test";
import { createRouter, memoryHistory } from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import App from "../App";
import { StoreContext } from "../context";
import {
	createFakeComms,
	wireAccount,
	wireChannel,
	wireTextMessage,
	wireTopic,
} from "../live/comms-fake";
import { appRoutes } from "../routes";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";

const CALLER = "acc-me";
const CHANNEL = "chan-arrived-live";
const TOPIC = "top-arrived-live";
const MESSAGE = "msg-arrived-live";
const MESSAGE_TEXT = "history from before the channel appeared";

async function settle(): Promise<void> {
	for (let i = 0; i < 30; i++) await Promise.resolve();
}

describe("live channel hydration", () => {
	test("a channel first seen live renders its earlier history, read once", async () => {
		// The channel is absent from the snapshot's channel list, but the server
		// already holds its topic and message: posted before the caller could see it.
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			topicsByChannel: {
				[CHANNEL]: [
					wireTopic({ id: TOPIC, channelId: CHANNEL, name: "prior work" }),
				],
			},
			messagesByChannel: {
				[CHANNEL]: [
					wireTextMessage({
						id: MESSAGE,
						topicId: TOPIC,
						authorAccountId: CALLER,
						atUnixMs: 123,
						text: MESSAGE_TEXT,
					}),
				],
			},
		});
		let topicReads = 0;
		const listTopics = fake.client.listTopics;
		fake.client.listTopics = (req) => {
			if (req.channelId === CHANNEL) topicReads++;
			return listTopics(req);
		};
		let store!: AppStore;
		const errors: unknown[] = [];
		const Router = createRouter({
			routes: appRoutes,
			history: memoryHistory("/"),
		});
		const { container } = render(() => {
			store = createAppStore({
				comms: fake.client,
				callerId: CALLER,
				queryClient: testQueryClient(),
				onCommsError: (error) => errors.push(error),
			});
			return (
				<StoreContext value={store}>
					<Router>{(props) => <App {...props} />}</Router>
				</StoreContext>
			);
		});

		try {
			await settle();
			expect(store.channels().map((channel) => channel.id)).toEqual([]);

			const channel = wireChannel(CHANNEL, CALLER);
			await fake.emit(
				{ case: "channelChanged", value: { channel, removedAccountIds: [] } },
				1n,
			);
			await settle();
			store.openChannel(CHANNEL);
			await settle();
			const topicRow = container.querySelector<HTMLButtonElement>(".topic-row");
			expect(topicRow?.textContent).toContain("prior work");
			topicRow?.click();
			await settle();
			expect(
				container.querySelector(".topic-view .msg")?.textContent,
			).toContain(MESSAGE_TEXT);

			// A redelivered channelChanged for a known channel does not re-read.
			await fake.emit(
				{ case: "channelChanged", value: { channel, removedAccountIds: [] } },
				2n,
			);
			await settle();
			expect(topicReads).toBe(1);
			expect(errors).toEqual([]);
		} finally {
			fake.close();
		}
	});
});
