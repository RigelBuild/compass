import { describe, expect, test } from "bun:test";
import { createMemoryHistory, MemoryRouter } from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import App from "./App";
import {
	STUB_CHANNELS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
	STUB_TOPICS,
} from "./comms-stub";
import { StoreContext } from "./context";
import {
	createFakeComms,
	wireAccount,
	wireChannel,
	wireTopic,
} from "./live/comms-fake";
import { AppRoutes } from "./routes";
import { type AppStore, createAppStore } from "./store";
import { flush, mountApp } from "./test-router";

// Route-behavior tests (record A4 / T3): the URL is the source of truth. These
// exercise the shared route table (routes.tsx) on a MemoryRouter — the same
// table index.tsx mounts under HashRouter — through the real App shell + store
// route-sync effect. Navigation is asynchronous (record A2): an action or a
// deep-link updates the location, and the effect writes the routed signals one
// reactive tick later, so every routed read follows a `flush()`.

// A standalone (kind "channel") channel that exists in the fixture — the target
// of an in-app openChannel. Derived so a fixture reshuffle can't stale it.
function standaloneChannelId(): string {
	const kindById = new Map(STUB_CHANNELS.map((c) => [c.id, c.kind]));
	const topicChannel = new Map(STUB_TOPICS.map((t) => [t.id, t.channelId]));
	for (const m of STUB_MESSAGES) {
		const channelId = topicChannel.get(m.topicId);
		if (channelId === undefined || kindById.get(channelId) !== "channel") {
			continue;
		}
		if (m.blocks.some((b) => b.kind === "ask")) return channelId;
	}
	throw new Error("fixture has no ask in a standalone channel");
}
const CHANNEL_ID = standaloneChannelId(); // "ch-svc-compass"

describe("routing (record A1/A4)", () => {
	// A deep-link initial path renders the matching surface. Boot straight onto
	// /backlog: the route drives view → "backlog" and BacklogView mounts, with no
	// in-app action taken. Mutation-check: a route-sync effect that ignored the
	// initial location would leave the boot-default bridge and redden.
	test("a deep-link initial path renders the right surface", async () => {
		const { store, container } = mountApp("/backlog");
		await flush();

		expect(store.view()).toBe("backlog");
		expect(container.querySelector(".backlog-view, .backlog")).not.toBeNull();
	});

	// A deep-link onto /channel/:channelId renders the channel surface with that
	// channel selected — the click path and deep-link resolve identically.
	test("a channel deep-link selects that channel", async () => {
		const { store } = mountApp(`/channel/${CHANNEL_ID}`);
		await flush();

		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe(CHANNEL_ID);
	});

	// A deep-link onto /agent/:agentId runs the full workspace anchoring lifted
	// from openAgent — the deep-link and the click share one home.
	test("an agent deep-link anchors the workspace", async () => {
		const { store, container } = mountApp("/agent/acc-cook");
		await flush();

		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-cook");
		expect(container.querySelector(".agent-view")).not.toBeNull();
	});

	// openChannel moves the memory history: an in-app action navigates, and the
	// route drives the store. Proven through the public routed reads (the history
	// is MemoryRouter-internal); a store action that still setView-d directly
	// would leave the URL — and any future route stacked on it — behind.
	test("openChannel moves the route and drives the surface", async () => {
		const { store } = mountApp("/");
		await flush();
		expect(store.view()).toBe("bridge");

		store.openChannel(CHANNEL_ID);
		await flush();

		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe(CHANNEL_ID);
	});

	// An unknown path redirects to "/" (the `*` catch-all → <Navigate href="/">),
	// landing on the board rather than a blank screen.
	test("an unknown path redirects to the board", async () => {
		const { store, container } = mountApp("/no-such-surface");
		await flush();

		expect(store.view()).toBe("bridge");
		expect(container.querySelector(".bridge")).not.toBeNull();
	});
});

// The pending-aware unknown-id fallback (record A3, lines 197-209) needs a LIVE
// store: it boots EMPTY and channels arrive asynchronously via the stream, so a
// deep-link to a valid channel must be HELD across the empty first run, not
// bounced. Gating is on first-snapshot arrival, never non-emptiness.
describe("pending-aware channel deep-link (record A3)", () => {
	const CALLER = "acc-me";
	const DEEP = "chan-deep";

	// Mount the full App shell over a LIVE store on a channel deep-link, with the
	// snapshot arrival under the test's control (the fake only emits its snapshot
	// boundary; the channels are served from the read RPCs after `flush`).
	function mountLive(initialPath: string, snapshotChannels: string[]) {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: snapshotChannels.map((id) => wireChannel(id, CALLER)),
			messagesByChannel: {},
		});
		let store!: AppStore;
		const history = createMemoryHistory();
		history.set({ value: initialPath });
		const { container } = render(() => {
			store = createAppStore({ comms: fake.client, callerId: CALLER });
			return (
				<StoreContext.Provider value={store}>
					<MemoryRouter history={history} root={App}>
						<AppRoutes />
					</MemoryRouter>
				</StoreContext.Provider>
			);
		});
		return { store, container, fake };
	}

	// A deep-link to a valid channel that the first snapshot DOES carry: it is
	// held through the empty first run, then resolves to that channel once the
	// snapshot lands — never bounced to "/". Mutation-check: gating the redirect
	// on non-emptiness (or omitting the first-snapshot guard) would bounce it on
	// the empty first effect run and leave view "bridge".
	test("a valid deep-link is held until the snapshot, then resolves", async () => {
		const { store, fake } = mountLive(`/channel/${DEEP}`, [DEEP]);

		// Before the snapshot the route is HELD on the channel surface with the id
		// carried through — not redirected.
		await flush();
		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe(DEEP);

		// After the snapshot arrives the id resolves against the loaded set.
		// (withLiveStore-style microtask drain settles the snapshot round-trip.)
		for (let i = 0; i < 20; i++) await Promise.resolve();
		await flush();
		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe(DEEP);

		fake.close();
	});

	// A deep-link to an id absent from the loaded set is redirected once the
	// first snapshot has arrived (genuinely unknown, not merely pending) — the
	// selection settles onto the snapshot's first channel and the route leaves
	// the dead id.
	test("an absent deep-link is redirected after the first snapshot", async () => {
		const { store, fake } = mountLive("/channel/chan-gone", [DEEP]);

		// Drain the snapshot round-trip: adoptComms fires, sees the current route
		// names a vanished channel, and re-points it.
		for (let i = 0; i < 30; i++) await Promise.resolve();
		await flush();

		expect(store.selectedChannelId()).toBe(DEEP);

		fake.close();
	});
});

// The topic deep-link (/channel/:channelId/topic/:topicId) carries the SAME
// pending-aware pattern as the channel route, plus a topic-specific second
// fallback: an absent-after-snapshot topic drops back to the channel index
// rather than stranding on a blank topic view (store.ts applyTopicRoute). A LIVE
// store is required for both — the topic set arrives asynchronously via the
// snapshot, so a valid topic deep-link must be HELD across the empty first run.
describe("pending-aware topic deep-link (record A3)", () => {
	const CALLER = "acc-me";
	const CHAN = "chan-deep";
	const TOPIC = "top-deep";

	// Mount the App shell over a LIVE store on a topic deep-link. The channel is
	// carried in the snapshot; `snapshotTopics` names the topic ids ListTopics
	// serves for CHAN, so a test controls whether the deep-linked topic resolves.
	function mountLive(initialPath: string, snapshotTopics: string[]) {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHAN, CALLER)],
			topicsByChannel: {
				[CHAN]: snapshotTopics.map((id) =>
					wireTopic({ id, channelId: CHAN, name: id }),
				),
			},
			messagesByChannel: {},
		});
		let store!: AppStore;
		const history = createMemoryHistory();
		history.set({ value: initialPath });
		const { container } = render(() => {
			store = createAppStore({ comms: fake.client, callerId: CALLER });
			return (
				<StoreContext.Provider value={store}>
					<MemoryRouter history={history} root={App}>
						<AppRoutes />
					</MemoryRouter>
				</StoreContext.Provider>
			);
		});
		return { store, container, fake };
	}

	// A deep-link to a valid topic the first snapshot carries: held on the topic
	// surface with the id through the empty first run, then resolves once the
	// snapshot lands. Mutation-check: omitting the first-snapshot guard would
	// bounce it to the channel index on the empty first effect run.
	test("a valid topic deep-link is held until the snapshot, then resolves", async () => {
		const { store, fake } = mountLive(`/channel/${CHAN}/topic/${TOPIC}`, [
			TOPIC,
		]);

		// Before the snapshot the topic dimension is HELD — view "topic", the id
		// carried through, not dropped to the channel index.
		await flush();
		expect(store.view()).toBe("topic");
		expect(store.selectedTopicId()).toBe(TOPIC);

		// After the snapshot the topic resolves against the loaded set.
		for (let i = 0; i < 20; i++) await Promise.resolve();
		await flush();
		expect(store.view()).toBe("topic");
		expect(store.selectedChannelId()).toBe(CHAN);
		expect(store.selectedTopicId()).toBe(TOPIC);

		fake.close();
	});

	// A deep-link to a topic absent from the loaded set (its channel valid) drops
	// back to the channel's index once the first snapshot has arrived — genuinely
	// unknown, not merely pending — rather than stranding on a blank topic view.
	test("an absent topic deep-link falls back to the channel index after the snapshot", async () => {
		const { store, fake } = mountLive(`/channel/${CHAN}/topic/top-gone`, [
			TOPIC,
		]);

		for (let i = 0; i < 30; i++) await Promise.resolve();
		await flush();

		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe(CHAN);
		expect(store.selectedTopicId()).toBeNull();

		fake.close();
	});
});

// Store-only route-sync unit checks (no <App> — the default in-memory seam
// applies routes synchronously, so createAppStore stays test-constructible with
// no router; record A3). These pin the seam's synchronous contract the fragment
// suites rely on.
describe("route-sync seam (no router)", () => {
	// Kept intentionally light — the transition matrix lives in store.test.ts;
	// this only asserts the default seam applies a route without a bound router.
	test("show* navigate the view synchronously through the default seam", () => {
		const store = createAppStore({ initialComms: STUB_COMMS_STATE });
		store.showBacklog();
		expect(store.view()).toBe("backlog");
		store.showSettings();
		expect(store.view()).toBe("settings");
		store.showBridge();
		expect(store.view()).toBe("bridge");
	});
});
