import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import type { CommandId } from "./keyboard/commands";
import type { AppStore } from "./store";
import { flush, mountApp } from "./test-router";

// Record A4: an inactive tab stays mounted under `hidden`, so a half-typed
// draft survives a switch, and hiding a tab first moves focus to the shown one.

const AGENT_ID = "acc-compass-ui";
const TOPIC_PATH = "/channel/ch-svc-compass/topic/top-compass-acp";

const composer = (c: HTMLElement): HTMLInputElement => {
	const input = c.querySelector<HTMLInputElement>(".conv-composer input.field");
	if (!input) throw new Error("no topic composer");
	return input;
};
const panelOf = (el: Element): HTMLElement => {
	const panel = el.closest<HTMLElement>('[role="tabpanel"]');
	if (!panel) throw new Error("element is not inside a view panel");
	return panel;
};
const tabIdAt = (store: AppStore, index: number): string => {
	const tab = store.layout().tabs[index];
	if (!tab) throw new Error(`no tab at ${index}`);
	return tab.id;
};

afterEach(() => cleanup());

describe("inactive tabs stay mounted (record A4)", () => {
	test("a hidden tab keeps its view mounted and its composer draft", async () => {
		const { store, container } = mountApp(TOPIC_PATH);
		await flush();
		const input = composer(container);
		fireEvent.input(input, { target: { value: "half-typed reply" } });

		store.dispatchLayout({ kind: "open", path: `/agent/${AGENT_ID}` });
		await flush();
		expect(store.view()).toBe("agent");
		// Still mounted, only hidden.
		expect(input.isConnected).toBe(true);
		expect(panelOf(input).hidden).toBe(true);
		const agentView = container.querySelector(".agent-view");
		if (!agentView) throw new Error("agent view did not mount");
		expect(panelOf(agentView).hidden).toBe(false);

		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 0) });
		await flush();
		expect(composer(container)).toBe(input);
		expect(input.value).toBe("half-typed reply");
		expect(panelOf(input).hidden).toBe(false);
		expect(panelOf(agentView).hidden).toBe(true);
	});

	test("a background-opened tab mounts hidden", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({
			kind: "open",
			path: `/agent/${AGENT_ID}`,
			background: true,
		});
		await flush();
		const agentView = container.querySelector(".agent-view");
		if (!agentView) throw new Error("background tab did not mount");
		expect(panelOf(agentView).hidden).toBe(true);
		const bridge = container.querySelector(".bridge");
		if (!bridge) throw new Error("no bridge");
		expect(panelOf(bridge).hidden).toBe(false);
	});

	test("switching away from a focused view moves focus into the shown view", async () => {
		const { store, container } = mountApp(TOPIC_PATH);
		store.dispatchLayout({
			kind: "open",
			path: `/agent/${AGENT_ID}`,
			background: true,
		});
		await flush();
		const input = composer(container);
		input.focus();
		expect(document.activeElement).toBe(input);

		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 1) });
		await flush();
		const agentView = container.querySelector(".agent-view");
		if (!agentView) throw new Error("no agent view");
		const focused = document.activeElement;
		if (!focused) throw new Error("focus was dropped");
		expect(panelOf(agentView).contains(focused)).toBe(true);
		expect(panelOf(input).hidden).toBe(true);

		// Back again: focus returns to where it was in that view.
		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 0) });
		await flush();
		expect(document.activeElement).toBe(input);
	});

	test("a switch leaves focus alone when it is outside the hidden view", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/settings", background: true });
		await flush();
		const toggle = container.querySelector<HTMLElement>(".pane-toggle");
		if (!toggle) throw new Error("no pane toggle");
		toggle.focus();

		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 1) });
		await flush();
		expect(store.view()).toBe("settings");
		expect(document.activeElement).toBe(toggle);
	});
});

describe("two mounted Bridge views", () => {
	test("closing one Bridge keeps the board commands for the other", async () => {
		const { store } = mountApp("/");
		// The second tab is a Bridge too: opened elsewhere, navigated to `/` in place.
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		store.showBridge();
		await flush();
		expect(store.layout().tabs.length).toBe(2);

		store.dispatchLayout({ kind: "close", tabId: tabIdAt(store, 0) });
		await flush();
		expect(store.view()).toBe("bridge");
		for (const id of ["board.openAssignedAgent", "list.moveNext"]) {
			expect(store.keyboard.registry.get(id as CommandId)).toBeDefined();
		}
	});
});
