import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import type { FakeComms } from "./live/comms-fake";
import {
	createFakeComms,
	wireAccount,
	wireChannel,
	wireTopic,
} from "./live/comms-fake";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";
import type { ViewScope } from "./view-scope";
import { createViewScope, useView } from "./view-scope";

const CALLER = "acc-me";
const CHANNEL = "chan-deep";
const TOPIC = "top-deep";

async function drainSnapshot(): Promise<void> {
	for (let i = 0; i < 30; i++) await Promise.resolve();
}

function mountScope(
	initialPath: string,
	channelIds: readonly string[],
	topicIds: readonly string[] = [],
): {
	store: AppStore;
	scope: ViewScope;
	otherScope: ViewScope;
	fake: FakeComms;
	dispose: () => void;
} {
	const fake = createFakeComms({
		accounts: [wireAccount(CALLER)],
		channels: channelIds.map((id) => wireChannel(id, CALLER)),
		topicsByChannel: {
			[CHANNEL]: topicIds.map((id) =>
				wireTopic({ id, channelId: CHANNEL, name: id }),
			),
		},
		messagesByChannel: {},
	});
	let dispose = () => {};
	const mounted = createRoot((stop) => {
		dispose = stop;
		const store = createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return {
			store,
			scope: createViewScope(store, "test-view", initialPath),
			otherScope: createViewScope(store, "other-view", "/done"),
		};
	});
	return { ...mounted, fake, dispose };
}

describe("ViewScope pending route resolution", () => {
	test("holds a channel deep link until the snapshot resolves it", async () => {
		const { scope, store, fake, dispose } = mountScope(`/channel/${CHANNEL}`, [
			CHANNEL,
		]);
		try {
			expect(store.firstSnapshotArrived()).toBe(false);
			expect(scope.path()).toBe(`/channel/${CHANNEL}`);
			expect(scope.route()).toEqual({ view: "channel", channelId: CHANNEL });
			expect(scope.channel()).toBeUndefined();

			await drainSnapshot();

			expect(store.firstSnapshotArrived()).toBe(true);
			expect(scope.path()).toBe(`/channel/${CHANNEL}`);
			expect(scope.channel()?.id).toBe(CHANNEL);
		} finally {
			fake.close();
			dispose();
		}
	});

	test("falls back from an unknown channel after the snapshot", async () => {
		const { scope, otherScope, store, fake, dispose } = mountScope(
			"/channel/chan-gone",
			[CHANNEL],
		);
		try {
			expect(scope.path()).toBe("/channel/chan-gone");
			expect(store.firstSnapshotArrived()).toBe(false);

			await drainSnapshot();

			expect(scope.path()).toBe(`/channel/${CHANNEL}`);
			expect(scope.channel()?.id).toBe(CHANNEL);
			expect(otherScope.path()).toBe("/done");
		} finally {
			fake.close();
			dispose();
		}
	});

	test("holds a topic deep link until the snapshot resolves it", async () => {
		const { scope, store, fake, dispose } = mountScope(
			`/channel/${CHANNEL}/topic/${TOPIC}`,
			[CHANNEL],
			[TOPIC],
		);
		try {
			expect(store.firstSnapshotArrived()).toBe(false);
			expect(scope.path()).toBe(`/channel/${CHANNEL}/topic/${TOPIC}`);
			expect(scope.route()).toEqual({
				view: "topic",
				channelId: CHANNEL,
				topicId: TOPIC,
			});
			expect(scope.topic()).toBeUndefined();

			await drainSnapshot();

			expect(scope.path()).toBe(`/channel/${CHANNEL}/topic/${TOPIC}`);
			expect(scope.channel()?.id).toBe(CHANNEL);
			expect(scope.topic()?.id).toBe(TOPIC);
		} finally {
			fake.close();
			dispose();
		}
	});

	test("falls back from an unknown topic to its channel after the snapshot", async () => {
		const { scope, otherScope, store, fake, dispose } = mountScope(
			`/channel/${CHANNEL}/topic/top-gone`,
			[CHANNEL],
			[TOPIC],
		);
		try {
			expect(scope.path()).toBe(`/channel/${CHANNEL}/topic/top-gone`);
			expect(store.firstSnapshotArrived()).toBe(false);

			await drainSnapshot();

			expect(scope.path()).toBe(`/channel/${CHANNEL}`);
			expect(scope.route()).toEqual({ view: "channel", channelId: CHANNEL });
			expect(scope.channel()?.id).toBe(CHANNEL);
			expect(scope.topic()).toBeUndefined();
			expect(otherScope.path()).toBe("/done");
		} finally {
			fake.close();
			dispose();
		}
	});
});

test("useView throws outside a ViewContext provider", () => {
	expect(() => createRoot(() => useView())).toThrow();
});
