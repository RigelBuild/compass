import { describe, expect, test } from "bun:test";
import { flush as flushSync } from "solid-js";
import { STUB_CHANNELS, STUB_MESSAGES, STUB_TOPICS } from "./comms-stub";
import type { CommandId } from "./keyboard/commands";
import { detectPlatform } from "./keyboard/dispatch";
import { shortcutFor } from "./keyboard/keymap";
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
// `<span class="cx-state-dot">` (StateDot.tsx:133-154); ChannelView root
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
				t.querySelector(".cx-state-dot") !== null &&
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
		flushSync();
		expect(leftPresent()).toBe(false);

		// Toggling back on restores it on the channel surface.
		store.toggleLeft();
		flushSync();
		expect(leftPresent()).toBe(true);
	});
});

// Coaching-tooltip adoption sweep (RIG-2530 T2). The topbar Bridge tab and the
// two glyph-only sidebar toggles convert from a native `title=` to a CoachTip;
// the toggles' dead chords are registered so they now dispatch. These assert
// the observable adoption contract: the tooltip reveals on focus, no `title`
// double-tooltips, `aria-keyshortcuts` survives, and the glyph toggles keep a
// non-glyph accessible name via the added `aria-label`.

// Kobalte portals its tooltip content on a macrotask, so a focus that opens it
// is observable only after one setTimeout(0).
async function settle(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	// biome-ignore lint/style/noRestrictedGlobals: deterministic macrotask yield (setTimeout(0)) to observe Kobalte's portalled tooltip; not a timed wait
	setTimeout(resolve, 0);
	await promise;
}

describe("coaching tooltips (RIG-2530 T2)", () => {
	test("the Bridge tab opens a coaching tooltip on focus showing the label + chord", async () => {
		const { container } = mountApp("/backlog");
		const tab = navViewTabs(container).find((t) =>
			t.textContent?.includes("Bridge"),
		);
		expect(tab).toBeDefined();

		tab?.focus();
		await settle();

		// Kobalte portals the tooltip content to document.body.
		const tooltip =
			document.body.querySelector<HTMLElement>('[role="tooltip"]');
		expect(tooltip).not.toBeNull();
		expect(tooltip?.textContent).toContain("Bridge");
		// Chord derived from the keymap, never hand-authored (D4).
		const chip = tooltip?.querySelector(".cx-palette-shortcut");
		const kbds = Array.from(chip?.querySelectorAll("kbd") ?? []).map(
			(k) => k.textContent,
		);
		expect(shortcutFor("view.bridge" as CommandId, detectPlatform())).toBe(
			"Ctrl+B",
		);
		expect(kbds).toEqual(["Ctrl", "B"]);
	});

	test("converted controls drop `title` but keep `aria-keyshortcuts`", () => {
		const { container } = mountApp();
		const bridgeTab = navViewTabs(container).find((t) =>
			t.textContent?.includes("Bridge"),
		);
		expect(bridgeTab?.hasAttribute("title")).toBe(false);
		expect(bridgeTab?.getAttribute("aria-keyshortcuts")).toBeTruthy();

		for (const label of ["Toggle left sidebar", "Toggle right sidebar"]) {
			const toggle = container.querySelector<HTMLElement>(
				`.pane-toggle[aria-label="${label}"]`,
			);
			expect(toggle).not.toBeNull();
			expect(toggle?.hasAttribute("title")).toBe(false);
			expect(toggle?.getAttribute("aria-keyshortcuts")).toBeTruthy();
		}
	});

	test("the glyph-only sidebar toggles are named by aria-label, not the bare glyph", () => {
		const { container } = mountApp();
		const left = container.querySelector<HTMLElement>(
			'.pane-toggle[aria-label="Toggle left sidebar"]',
		);
		const right = container.querySelector<HTMLElement>(
			'.pane-toggle[aria-label="Toggle right sidebar"]',
		);
		expect(left).not.toBeNull();
		expect(right).not.toBeNull();
		// The visible content is a decorative block glyph; the accessible name
		// must come from aria-label, never the glyph.
		expect(left?.getAttribute("aria-label")).toBe("Toggle left sidebar");
		expect(right?.getAttribute("aria-label")).toBe("Toggle right sidebar");
		expect(left?.textContent?.trim()).not.toBe("");
		expect(left?.getAttribute("aria-label")).not.toBe(
			left?.textContent?.trim(),
		);
	});

	test("both sidebar toggles are now live: their coached chords dispatch", async () => {
		const { store } = mountApp();
		expect(store.leftOpen()).toBe(true);
		expect(store.rightOpen()).toBe(true);

		// Both commands the sweep coaches resolve in the registry (dispatch path),
		// not only the keymap (display path) — the drift the A4 boundary guards.
		expect(
			store.keyboard.registry.get("sidebar.toggleLeft" as CommandId),
		).toBeDefined();
		expect(
			store.keyboard.registry.get("sidebar.toggleRight" as CommandId),
		).toBeDefined();

		// Mod+Shift+\ → toggleLeft; Mod+\ → toggleRight (keymap rows), now that
		// the commands are registered.
		window.dispatchEvent(
			new KeyboardEvent("keydown", {
				key: "\\",
				ctrlKey: true,
				shiftKey: true,
				bubbles: true,
			}),
		);
		await flush();
		expect(store.leftOpen()).toBe(false);

		window.dispatchEvent(
			new KeyboardEvent("keydown", {
				key: "\\",
				ctrlKey: true,
				bubbles: true,
			}),
		);
		await flush();
		expect(store.rightOpen()).toBe(false);
	});
});
