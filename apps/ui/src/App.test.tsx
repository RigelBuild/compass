import { describe, expect, test } from "bun:test";
import { STUB_CHANNELS, STUB_MESSAGES, STUB_TOPICS } from "./comms-stub";
import { STUB_AGENTS } from "./stub-data";
import { flush, mountApp } from "./test-router";

// RED acceptance spec for T7 (design.md §643-664): restore App.tsx from the
// current mid-reshape "Channels|Board swap" layout back to the board-primary
// origin/main shell, PLUS the T6 `channel` surface folded into the board switch.
// The end-state shell these reds assert (and which FAILS today):
//   - nav.view-tabs = a Bridge tab (today it says "Board") + a selected-agent
//     tab (StateDot + agent name) when store.selectedAgent() is set (today there
//     is no agent tab — the nav is a single Board button).
//   - LeftSidebar always available, gated only by leftOpen() OUTSIDE the view
//     switch → present on the channel/agent surfaces too (today it renders only
//     inside the `!onChannelSurface()` board branch, so it is absent on channel).
//   - center switch adds `channel`→<ChannelView/> with NO
//     ChannelSidebar anywhere (today the channel/agent branches mount
//     <ChannelSidebar/>, so `.channel-rail` is present).
// App consumes the store via useStore() and takes NO props, so every assertion
// below is a genuine runtime/structure red against a cleanly-mounting App
// (App.tsx imports fine today) — never a tsc/module-load error.
//
// Fixture ground truth (grepped from comms-stub.ts / stub-data.ts, quoted here;
// DERIVED below so a fixture reshuffle can't stale the test):
//   - The standalone (kind "channel") channel carrying an ask is `ch-svc-compass`
//     (name "svc.compass", kind "channel", membership "subscribed"; msg-c4's
//     `ask-s4-integration`). openChannel routes a kind:"channel" channel to the
//     channel surface (store.ts:605-615) — never the agent-workspace delegate,
//     which only fires for 1:1 agent DMs. Derived below via the same ask finder
//     ChannelView.test.tsx uses, so both suites pin to the same channel + real
//     threaded content renders (proving the surface really mounted, not empty).
//   - Agent `acc-compass-ui` (handle "compass-ui", home DM dm-compass-ui) — the agent openAgent
//     selects; its name (derived from STUB_AGENTS, not copied) is the label the
//     selected-agent view-tab must carry.
// Query anchors (grepped): Bridge root `.bridge` (Bridge.tsx:46); LeftSidebar
// root `<aside class="left">` (LeftSidebar.tsx:352); ChannelSidebar root
// `<aside class="channel-rail">` (ChannelSidebar.tsx:147); StateDot
// `<span class="state-dot">` (StateDot.tsx:19-25); ChannelView root
// `<section class="conversation">` (ChannelView.tsx:309); AgentView root
// `.agent-view` (AgentView.tsx:204); top nav `<nav class="view-tabs">` with
// `.view-tab` children (App.tsx:53-65, target §645-646).

// The standalone (kind "channel") channel carrying an ask — the channel these
// tests route to. Derived from the fixture (finds whatever standalone-channel
// ask exists) so a reshuffle can't stale it; lands on `ch-svc-compass`.
function standaloneChannelId(): string {
	const channelKind = new Map(STUB_CHANNELS.map((c) => [c.id, c.kind]));
	const topicChannel = new Map(STUB_TOPICS.map((t) => [t.id, t.channelId]));
	for (const m of STUB_MESSAGES) {
		const channelId = topicChannel.get(m.topicId);
		if (channelId === undefined || channelKind.get(channelId) !== "channel") {
			continue;
		}
		for (const b of m.blocks) {
			if (b.kind === "ask") return channelId;
		}
	}
	throw new Error(
		"fixture has no ask in a standalone (kind 'channel') channel — T7 channel-surface test needs one",
	);
}

const STANDALONE_CHANNEL_ID = standaloneChannelId(); // "ch-svc-compass"

// The agent openAgent selects (brief-specified id) with its name resolved from
// the fixture, so the view-tab label assertion tracks the real fixture name
// rather than a copied literal.
const AGENT_ID = "acc-compass-ui";
const AGENT_NAME = (() => {
	const agent = STUB_AGENTS.find((a) => a.account.id === AGENT_ID);
	if (!agent) {
		throw new Error(
			`fixture has no agent ${AGENT_ID} — T7 agent-tab test needs one`,
		);
	}
	return agent.account.displayName ?? agent.account.handle; // "compass-ui"
})();

// Mount the real App shell over a fixture-backed store on the shared
// MemoryRouter (test-router.tsx) — the same route table index.tsx renders in
// HashRouter, so these tests exercise the production routing. Navigation is
// async under the router: tests await `flush()` between an action and a routed
// read (record A2/A4).

// The top-nav surface view-tabs — the single tab strip the board-primary shell
// exposes (Bridge +, when an agent is selected, the agent tab). Scoped to
// `nav.view-tabs` so the agent-view's own StateDot in the center never leaks in.
const navViewTabs = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>("nav.view-tabs .view-tab"),
];

describe("App shell (T7)", () => {
	// Boot lands on the board-primary shell: view `bridge`, the Bridge surface
	// centered, an always-available LeftSidebar, no ChannelSidebar. The RED leg
	// is the nav tab — the board-primary strip names it "Bridge" (§645-646);
	// today the single tab says "Board". Mutation-check: renaming the tab back to
	// "Board" (or dropping the Bridge tab) reddens exactly this assertion.
	test("boot shows the board-primary shell (Bridge tab, LeftSidebar, no ChannelSidebar)", () => {
		const { store, container } = mountApp();

		// Precondition: the store boots on bridge and the Bridge surface renders
		// (proves App mounted cleanly — the reds below are structural, not empty).
		expect(store.view()).toBe("bridge");
		expect(container.querySelector(".bridge")).not.toBeNull();

		// RED today: the nav has a Bridge tab. Currently the only view-tab reads
		// "Board" → no tab text includes "Bridge".
		const tabs = navViewTabs(container);
		const bridgeTab = tabs.find((t) => t.textContent?.includes("Bridge"));
		expect(bridgeTab).toBeDefined();

		// The board-primary shell keeps the left sidebar and never mounts the
		// channel rail (both hold on the bridge view today; asserted as part of
		// the shell definition, kept green by the impl).
		expect(container.querySelector("aside.left")).not.toBeNull();
		expect(container.querySelectorAll(".channel-rail").length).toBe(0);
	});

	// Opening a standalone channel routes to the channel surface INSIDE the board
	// shell: ChannelView renders in `main.main`, the LeftSidebar stays (leftOpen),
	// and NO ChannelSidebar appears. Today the channel branch swaps in
	// <ChannelSidebar/> and drops the LeftSidebar → two RED legs. Mutation-check:
	// re-adding the ChannelSidebar reddens the rail leg; gating LeftSidebar behind
	// the board branch reddens the sidebar leg.
	test("opening a channel routes to the channel surface inside the board shell", async () => {
		const { store, container } = mountApp();
		store.openChannel(STANDALONE_CHANNEL_ID);
		await flush();

		// Precondition: the channel surface really mounted — ChannelView's root is
		// inside the center main.main and renders the channel's topic index.
		expect(store.view()).toBe("channel");
		const conv = container.querySelector("main.main .conversation");
		expect(conv).not.toBeNull();
		expect(
			container.querySelectorAll(".topic-index .topic-row").length,
		).toBeGreaterThan(0);

		// LeftSidebar is view-independent (leftOpen defaults true), so it stays
		// present on the channel surface.
		expect(container.querySelector("aside.left")).not.toBeNull();

		// No ChannelSidebar anywhere.
		expect(container.querySelectorAll(".channel-rail").length).toBe(0);
	});

	// Selecting an agent adds the selected-agent view-tab (agent name + StateDot)
	// and renders AgentView in the center; view() flips to `agent`. Today the nav
	// is a single Board tab with no agent tab → RED on the second tab / its
	// StateDot. Mutation-check: dropping the agent tab, its StateDot, or the name
	// each reddens the tab assertion.
	test("selecting an agent adds the agent view-tab with a StateDot", async () => {
		const { store, container } = mountApp();
		store.openAgent(AGENT_ID);
		await flush();

		// Precondition: routed to the agent workspace and AgentView mounted.
		expect(store.view()).toBe("agent");
		expect(container.querySelector(".agent-view")).not.toBeNull();

		// A second nav view-tab carries the agent name AND a StateDot.
		const tabs = navViewTabs(container);
		const agentTab = tabs.find(
			(t) =>
				t.querySelector(".state-dot") !== null &&
				t.textContent?.includes(AGENT_NAME),
		);
		expect(agentTab).toBeDefined();
	});

	// The always-present left sidebar toggles on the channel surface too, proving
	// it is view-independent (§647, gated only by leftOpen() outside the switch).
	// Today the LeftSidebar renders only inside the board branch, so it is absent
	// on the channel view regardless of leftOpen → the "present" legs redden.
	// Mutation-check: gating the sidebar behind the board branch reddens both
	// present legs; the toggled-off leg guards against always-rendering it.
	test("the always-present left sidebar toggles on the channel surface", async () => {
		const { store, container } = mountApp();
		store.openChannel(STANDALONE_CHANNEL_ID);
		await flush();
		expect(store.view()).toBe("channel");

		const leftPresent = () => container.querySelector("aside.left") !== null;

		// leftOpen defaults true → the sidebar shows on the channel surface.
		expect(leftPresent()).toBe(true);

		// Toggling off hides it (a synchronous pane action, not routed).
		store.toggleLeft();
		expect(leftPresent()).toBe(false);

		// Toggling back on restores it on the channel surface.
		store.toggleLeft();
		expect(leftPresent()).toBe(true);
	});
});
