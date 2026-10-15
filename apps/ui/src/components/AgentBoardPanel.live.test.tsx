import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentBoardSchema,
	BoardAskSchema,
	ChannelKind,
	ChannelSchema,
	CommsService,
	create,
	createRouterTransport,
	UserAccountSchema,
} from "@compass/client";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { StoreContext } from "../context";
import { createFakeComms, type FakeComms } from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { AgentBoardPanel } from "./AgentBoard";

const CALLER = "acc-me";
const AGENT = "acc-cook";
const ASK_CHANNEL = "chan-asks";

const wireUser = create(AccountSchema, {
	id: CALLER,
	handle: CALLER,
	displayName: CALLER,
	kind: { case: "user", value: create(UserAccountSchema, {}) },
});
const wireAgent = create(AccountSchema, {
	id: AGENT,
	handle: "cook",
	displayName: "cook",
	kind: {
		case: "agent",
		value: create(AgentAccountSchema, { ownerUserId: CALLER }),
	},
});
const wireChannel = create(ChannelSchema, {
	id: ASK_CHANNEL,
	name: "asks",
	kind: ChannelKind.CHANNEL,
	memberAccountIds: [CALLER, AGENT],
	subscriberAccountIds: [CALLER, AGENT],
});

const board = (writtenAtUnixMs: bigint) =>
	create(AgentBoardSchema, {
		agentAccountId: AGENT,
		handle: "cook",
		writtenAtUnixMs,
		asks: [
			create(BoardAskSchema, {
				askId: "ask-1",
				messageId: "msg-unloaded",
				channelId: ASK_CHANNEL,
				header: "Pick a plan",
			}),
		],
	});

/** Pump microtasks and Solid flushes until `ready` holds; fail after a bounded count. */
async function until(ready: () => boolean): Promise<void> {
	for (let i = 0; i < 5000 && !ready(); i++) {
		await Promise.resolve();
		flush();
	}
	expect(ready()).toBe(true);
}

async function mountPanel(): Promise<{
	store: AppStore;
	fake: FakeComms;
	container: HTMLElement;
	reads: () => number;
}> {
	let reads = 0;
	const transport = createRouterTransport(({ service }) =>
		service(CommsService, {
			listAgentBoards: (request) => {
				reads++;
				expect(request.agentHandles).toEqual(["cook"]);
				return { boards: [board(BigInt(reads))] };
			},
		}),
	);
	const fake = createFakeComms({
		accounts: [wireUser, wireAgent],
		channels: [wireChannel],
	});
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			comms: fake.client,
			transport,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<AgentBoardPanel agentId={AGENT} />
			</StoreContext>
		);
	});
	await until(() => reads > 0 && container.textContent !== "");
	return { store, fake, container, reads: () => reads };
}

describe("AgentBoardPanel", () => {
	test("an agentBoardChanged for the agent re-reads its board", async () => {
		const { store, fake, container, reads } = await mountPanel();
		expect(reads()).toBe(1);
		expect(container.textContent).toContain("Pick a plan");
		await fake.emit(
			{
				case: "agentBoardChanged",
				value: { agentAccountId: AGENT, board: board(99n) },
			},
			1n,
		);
		await until(() => reads() >= 2);
		// The roster re-read that follows the event must not read the board again.
		await until(() => store.agentView(AGENT)?.board?.writtenAtUnixMs === 99);
		for (let i = 0; i < 200; i++) await Promise.resolve();
		flush();
		expect(reads()).toBe(2);
	});

	test("an automatic patch that keeps the write time still re-reads", async () => {
		const { store, fake, reads } = await mountPanel();
		for (const seq of [1n, 2n]) {
			await fake.emit(
				{
					case: "agentBoardChanged",
					value: { agentAccountId: AGENT, board: board(50n) },
				},
				seq,
			);
			await until(() => reads() === Number(seq) + 1);
		}
		expect(store.agentView(AGENT)?.board?.writtenAtUnixMs).toBe(50);
	});

	test("an ask whose message is not loaded opens its channel", async () => {
		const { store, container } = await mountPanel();
		container.querySelector<HTMLButtonElement>(".agent-board-ask")?.click();
		await until(() => store.focusedView().route().view === "channel");
		expect(store.focusedView().route()).toMatchObject({
			view: "channel",
			channelId: ASK_CHANNEL,
		});
	});
});
