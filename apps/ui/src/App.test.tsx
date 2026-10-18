import { describe, expect, test } from "bun:test";
import { flush as flushSync } from "solid-js";
import needle from "./assets/compass-needle.svg";
import { STUB_CHANNELS, STUB_MESSAGES, STUB_TOPICS } from "./comms-stub";
import type { CommandId } from "./keyboard/commands";
import { STUB_AGENTS } from "./stub-data";
import { flush, mountApp } from "./test-router";

// Pin shell layout across board, channel, and agent views.

// Find a fixture channel containing an ask so routing tests follow fixture changes.
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

// Resolve the selected agent's label from fixture data.
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

// Router navigation is asynchronous; flush before reading routed state.

// Scope tab queries to the topbar, excluding AgentView's StateDot.
const navViewTabs = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>('.cx-tab-strip [role="tab"]'),
];

describe("App shell (T7)", () => {
	// Keep the Bridge tab, left sidebar, and absence of a channel rail on boot.
	test("boot shows the board-primary shell (Bridge tab, LeftSidebar, no ChannelSidebar)", () => {
		const { store, container } = mountApp();

		// Confirm the Bridge surface mounted before checking shell structure.
		expect(store.view()).toBe("bridge");
		expect(container.querySelector(".bridge")).not.toBeNull();

		// The boot tab is the Bridge tab.
		const tabs = navViewTabs(container);
		const bridgeTab = tabs.find((t) => t.textContent?.includes("Bridge"));
		expect(bridgeTab).toBeDefined();

		// The shell keeps its left sidebar and omits the channel rail.
		expect(container.querySelector("aside.left")).not.toBeNull();
		expect(container.querySelectorAll(".channel-rail").length).toBe(0);
	});

	// Channel navigation retains the left sidebar and never mounts the channel rail.
	test("opening a channel routes to the channel surface inside the board shell", async () => {
		const { store, container } = mountApp();
		store.openChannel(STANDALONE_CHANNEL_ID);
		await flush();

		// Confirm ChannelView mounted inside the center pane with its topic index.
		expect(store.view()).toBe("channel");
		const conv = container.querySelector("main.main .conversation");
		expect(conv).not.toBeNull();
		expect(
			container.querySelectorAll(".topic-index .topic-row").length,
		).toBeGreaterThan(0);

		// The left sidebar remains visible on the channel surface.
		expect(container.querySelector("aside.left")).not.toBeNull();

		// No ChannelSidebar anywhere.
		expect(container.querySelectorAll(".channel-rail").length).toBe(0);
	});

	// Opening an agent focuses its named tab and mounts AgentView.
	test("selecting an agent adds the agent view-tab with a StateDot", async () => {
		const { store, container } = mountApp();
		store.openAgent(AGENT_ID);
		await flush();

		// Confirm the agent workspace mounted before checking its tab.
		expect(store.view()).toBe("agent");
		expect(container.querySelector(".agent-view")).not.toBeNull();

		// The focused view tab carries the agent name AND a StateDot.
		const tabs = navViewTabs(container);
		const agentTab = tabs.find(
			(t) =>
				t.querySelector(".cx-state-dot") !== null &&
				t.textContent?.includes(AGENT_NAME),
		);
		expect(agentTab).toBeDefined();
	});

	// The left sidebar remains toggleable on channel surfaces.
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

	// The top bar shows the needle mark alone and the shell has no bottom usage bar.
	test("the shell carries the needle mark alone and no usage bar", () => {
		const { container } = mountApp();
		const brand = container.querySelector(".topbar .brand");
		const imgs = brand?.querySelectorAll("img") ?? [];
		expect(imgs).toHaveLength(1);
		expect(imgs[0]?.getAttribute("src")).toBe(needle);
		expect(imgs[0]?.getAttribute("alt")).toBe("Compass");
		expect(brand?.textContent).toBe("");
		expect(container.querySelector("footer")).toBeNull();
	});
});

// Glyph-only controls use CoachTip instead of native title tooltips.

describe("coaching tooltips (RIG-2530 T2)", () => {
	test("the sidebar toggles drop `title` but keep `aria-keyshortcuts`", () => {
		const { container } = mountApp();

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
		// The SVG is decorative; the accessible name must come from aria-label.
		expect(left?.getAttribute("aria-label")).toBe("Toggle left sidebar");
		expect(right?.getAttribute("aria-label")).toBe("Toggle right sidebar");
		const glyph = left?.querySelector("svg");
		expect(glyph).not.toBeNull();
		expect(glyph?.getAttribute("aria-hidden")).toBe("true");
		expect(left?.textContent?.trim()).toBe("");
	});

	test("both sidebar toggles are now live: their coached chords dispatch", async () => {
		const { store } = mountApp();
		expect(store.leftOpen()).toBe(true);
		expect(store.rightOpen()).toBe(true);

		// Coached chords must exist in the dispatch registry, not only the keymap.
		expect(
			store.keyboard.registry.get("sidebar.toggleLeft" as CommandId),
		).toBeDefined();
		expect(
			store.keyboard.registry.get("sidebar.toggleRight" as CommandId),
		).toBeDefined();

		// The registered commands use these keymap chords.
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
