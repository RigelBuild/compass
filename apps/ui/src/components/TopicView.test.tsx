import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	create,
	UserAccountSchema,
} from "@compass/client";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { StoreContext } from "../context";
import {
	createFakeComms,
	type FakeComms,
	wireChannel,
	wireTextMessage,
	wireTopic,
} from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { TopicView } from "./TopicView";

const OWNER = "acc-bob";
const AGENT = "acc-x";
const CHANNEL = "chan-mentions";
const TOPIC = "topic-mentions";

const wireUserAccount = (handle: string) =>
	create(AccountSchema, {
		id: OWNER,
		handle,
		displayName: handle,
		kind: { case: "user", value: create(UserAccountSchema, {}) },
	});

const wireAgentAccount = () =>
	create(AccountSchema, {
		id: AGENT,
		handle: "x",
		displayName: "X",
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, { ownerUserId: OWNER }),
		},
	});

async function mountTopic(text: string): Promise<{
	container: HTMLElement;
	fake: FakeComms;
	settled: () => Promise<void>;
}> {
	const fake = createFakeComms({
		accounts: [wireUserAccount("bob"), wireAgentAccount()],
		channels: [wireChannel(CHANNEL, OWNER)],
		topicsByChannel: {
			[CHANNEL]: [
				wireTopic({ id: TOPIC, channelId: CHANNEL, name: "mentions" }),
			],
		},
		messagesByChannel: {
			[CHANNEL]: [
				wireTextMessage({
					id: "msg-mention",
					topicId: TOPIC,
					authorAccountId: OWNER,
					atUnixMs: 1,
					text,
				}),
			],
		},
	});
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			comms: fake.client,
			callerId: OWNER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<TopicView />
			</StoreContext>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	await settled();
	store.openTopic(TOPIC);
	await settled();
	flush();
	return { container, fake, settled };
}

const mentionChip = (container: HTMLElement, token: string) =>
	[...container.querySelectorAll<HTMLElement>(".mention-chip")].find(
		(chip) => chip.textContent === token,
	);

const expectKnown = (chip: HTMLElement | undefined, token: string) => {
	expect(chip?.textContent).toBe(token);
	expect(chip?.classList.contains("reserved")).toBe(false);
	expect(chip?.classList.contains("unknown")).toBe(false);
};

describe("TopicView owner-qualified mentions", () => {
	test("an owner's qualified agent mention renders as known", async () => {
		const { container, fake } = await mountTopic("ping @bob/x");
		try {
			// The real store must resolve the owner's account handle to qualify the agent.
			expectKnown(mentionChip(container, "@bob/x"), "@bob/x");
		} finally {
			fake.close();
		}
	});

	test("renaming an owner updates known qualified mentions", async () => {
		const { container, fake, settled } = await mountTopic("@bob/x and @rob/x");
		try {
			expectKnown(mentionChip(container, "@bob/x"), "@bob/x");
			expect(
				mentionChip(container, "@rob/x")?.classList.contains("unknown"),
			).toBe(true);

			await fake.emit(
				{
					case: "accountChanged",
					value: { account: wireUserAccount("rob") },
				},
				1n,
			);
			await settled();
			flush();

			expect(
				mentionChip(container, "@bob/x")?.classList.contains("unknown"),
			).toBe(true);
			expectKnown(mentionChip(container, "@rob/x"), "@rob/x");
		} finally {
			fake.close();
		}
	});
});
