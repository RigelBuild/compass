import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import type { CommandId } from "./keyboard/commands";
import type { AppStore } from "./store";
import { flush, mountApp } from "./test-router";
import { focusedPane, MAX_TABS } from "./window-layout";

// An inactive tab stays mounted under `hidden`, so a half-typed
// draft survives a switch, and hiding a tab first moves focus to the shown one.

const AGENT_ID = "acc-compass-ui";
const TOPIC_PATH = "/channel/ch-svc-compass/topic/top-compass-acp";

const composer = (c: HTMLElement): HTMLTextAreaElement => {
	const input = c.querySelector<HTMLTextAreaElement>(
		".conv-composer textarea.cx-composer",
	);
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
const runCommand = (store: AppStore, commandId: string): void => {
	const command = store.keyboard.registry.get(commandId as CommandId);
	if (!command) throw new Error(`missing command ${commandId}`);
	command.run();
};
const panelForView = (container: HTMLElement, viewId: string): HTMLElement => {
	const panel = container.querySelector<HTMLElement>(`#view-panel-${viewId}`);
	if (!panel) throw new Error(`no panel for view ${viewId}`);
	return panel;
};

afterEach(() => cleanup());

describe("tab and pane commands on the real store", () => {
	test("New tab on the Bridge tab opens a second Bridge tab and focuses it", async () => {
		const { store } = mountApp("/");
		await flush();
		runCommand(store, "tab.new");
		await flush();
		expect(store.layout().tabs).toHaveLength(2);
		expect(store.layout().activeTabId).toBe(tabIdAt(store, 1));
		expect(store.view()).toBe("bridge");
	});

	test("New tab from a topic tab opens a second Bridge tab and focuses it", async () => {
		const { store } = mountApp(TOPIC_PATH);
		await flush();
		runCommand(store, "tab.new");
		await flush();
		expect(store.layout().tabs).toHaveLength(2);
		expect(store.layout().activeTabId).toBe(tabIdAt(store, 1));
		expect(store.view()).toBe("bridge");
	});

	test("New tab at the tab cap is refused with the layout notice", async () => {
		const { store } = mountApp(TOPIC_PATH);
		await flush();
		for (let index = 1; index < MAX_TABS; index++) {
			store.dispatchLayout({ kind: "open", path: `/agent/agent-${index}` });
		}
		await flush();
		expect(store.layout().tabs).toHaveLength(MAX_TABS);
		expect(store.layoutNotice()).toBeUndefined();
		runCommand(store, "tab.new");
		await flush();
		expect(store.layout().tabs).toHaveLength(MAX_TABS);
		expect(store.layoutNotice()?.text).toContain(`${MAX_TABS} tabs`);
	});

	test("Close tab on the last tab sends it to Bridge", async () => {
		const { store } = mountApp(TOPIC_PATH);
		await flush();
		runCommand(store, "tab.close");
		await flush();
		expect(store.layout().tabs).toHaveLength(1);
		expect(store.view()).toBe("bridge");
	});

	test("next and previous wrap; go to tab past the end changes nothing", async () => {
		const { store } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/settings/general" });
		await flush();
		const [first, second] = [tabIdAt(store, 0), tabIdAt(store, 1)];
		runCommand(store, "tab.next");
		await flush();
		expect(store.layout().activeTabId).toBe(first);
		runCommand(store, "tab.prev");
		await flush();
		expect(store.layout().activeTabId).toBe(second);
		const before = store.layout();
		runCommand(store, "tab.goto.3");
		await flush();
		expect(store.layout()).toBe(before);
		runCommand(store, "tab.goto.1");
		await flush();
		expect(store.layout().activeTabId).toBe(first);
	});

	test("move left and right reorder, and stop at the ends", async () => {
		const { store } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/settings/general" });
		await flush();
		const [first, second] = [tabIdAt(store, 0), tabIdAt(store, 1)];
		const atEnd = store.layout();
		runCommand(store, "tab.moveRight");
		await flush();
		expect(store.layout()).toBe(atEnd);
		runCommand(store, "tab.moveLeft");
		await flush();
		expect(store.layout().tabs.map((tab) => tab.id)).toEqual([second, first]);
		const atStart = store.layout();
		runCommand(store, "tab.moveLeft");
		await flush();
		expect(store.layout()).toBe(atStart);
	});

	test("pane commands split once and close only the other pane", async () => {
		const { store } = mountApp(TOPIC_PATH);
		await flush();
		runCommand(store, "pane.splitRight");
		await flush();
		const splitTab = store.layout().tabs[0];
		if (splitTab?.layout.kind !== "split") {
			throw new Error("Split right did not create a split tab");
		}
		expect(splitTab.layout.direction).toBe("row");
		expect(splitTab.layout.second.path).toBe(TOPIC_PATH);
		const split = store.layout();
		runCommand(store, "pane.splitDown");
		await flush();
		expect(store.layout()).toBe(split);
		runCommand(store, "pane.focusFirst");
		await flush();
		runCommand(store, "pane.closeOther");
		await flush();
		expect(store.layout().tabs[0]?.layout).toEqual({
			kind: "single",
			view: splitTab.layout.first,
		});
	});
});

describe("pane focus follows DOM focus", () => {
	const splitPanels = async (
		store: AppStore,
		container: HTMLElement,
	): Promise<[HTMLElement, HTMLElement]> => {
		store.dispatchLayout({ kind: "split", direction: "row" });
		await flush();
		const tab = store.layout().tabs[0];
		if (tab?.layout.kind !== "split") throw new Error("no split tab");
		return [
			panelForView(container, tab.layout.first.id),
			panelForView(container, tab.layout.second.id),
		];
	};
	const focusedPaneOf = (store: AppStore): string | undefined => {
		const tab = store.layout().tabs[0];
		return tab?.layout.kind === "split" ? tab.layout.focused : undefined;
	};

	test("a pane focus command moves DOM focus from one pane into the other", async () => {
		const { store, container } = mountApp(TOPIC_PATH);
		await flush();
		const [first, second] = await splitPanels(store, container);
		const input = first.querySelector<HTMLElement>(".conv-composer textarea");
		if (!input) throw new Error("no composer in the first pane");
		input.focus();
		await flush();
		expect(focusedPaneOf(store)).toBe("first");
		runCommand(store, "pane.focusSecond");
		await flush();
		expect(second.contains(document.activeElement)).toBe(true);
		runCommand(store, "pane.focusFirst");
		await flush();
		expect(document.activeElement).toBe(input);
	});

	test("a pane focus command from the tab strip moves DOM focus into that pane", async () => {
		const { store, container } = mountApp("/");
		await flush();
		const [first] = await splitPanels(store, container);
		const tabButton = container.querySelector<HTMLElement>(
			'.cx-tab-strip [role="tab"]',
		);
		if (!tabButton) throw new Error("no tab button");
		tabButton.focus();
		runCommand(store, "pane.focusFirst");
		await flush();
		expect(focusedPaneOf(store)).toBe("first");
		expect(first.contains(document.activeElement)).toBe(true);
	});

	test("focus moving into a pane makes it the focused pane", async () => {
		const { store, container } = mountApp("/");
		await flush();
		const [first, second] = await splitPanels(store, container);
		expect(focusedPaneOf(store)).toBe("second");
		first.focus();
		await flush();
		expect(focusedPaneOf(store)).toBe("first");
		expect(document.activeElement).toBe(first);
		second.focus();
		await flush();
		expect(focusedPaneOf(store)).toBe("second");
	});
});

describe("inactive tabs stay mounted", () => {
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

	test("a remembered target that became disabled is skipped on return", async () => {
		const { store, container } = mountApp(TOPIC_PATH);
		store.dispatchLayout({
			kind: "open",
			path: `/agent/${AGENT_ID}`,
			background: true,
		});
		await flush();
		const input = composer(container);
		input.focus();
		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 1) });
		await flush();
		input.disabled = true;
		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 0) });
		await flush();
		const focused = document.activeElement;
		if (!(focused instanceof HTMLElement)) throw new Error("focus was dropped");
		expect(focused).not.toBe(input);
		expect(panelOf(focused)).toBe(panelOf(input));
	});

	test("a switch leaves focus alone when it is outside the hidden view", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({
			kind: "open",
			path: "/settings/general",
			background: true,
		});
		await flush();
		const toggle = container.querySelector<HTMLElement>(".pane-toggle");
		if (!toggle) throw new Error("no pane toggle");
		toggle.focus();

		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 1) });
		await flush();
		expect(store.view()).toBe("settings");
		expect(document.activeElement).toBe(toggle);
	});

	test("a bad path in a background tab redirects that view, not the active one", async () => {
		const { store } = mountApp(TOPIC_PATH);
		store.dispatchLayout({ kind: "open", path: "/nope", background: true });
		await flush();
		await flush();
		expect(store.view()).toBe("topic");
		expect(store.focusedView().path()).toBe(TOPIC_PATH);
		const background = store.layout().tabs[1];
		expect(background && focusedPane(background.layout).path).toBe("/");
	});

	test("closing the tab that holds focus moves focus into the newly shown view", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: TOPIC_PATH });
		await flush();
		const input = composer(container);
		input.focus();
		store.dispatchLayout({ kind: "close", tabId: tabIdAt(store, 1) });
		await flush();
		const bridge = container.querySelector(".bridge");
		if (!bridge) throw new Error("no bridge");
		const focused = document.activeElement;
		expect(focused !== null && panelOf(bridge).contains(focused)).toBe(true);
	});

	test("a view with nothing focusable still takes focus on its panel", async () => {
		const { store, container } = mountApp(TOPIC_PATH);
		store.dispatchLayout({
			kind: "open",
			path: "/agent/acc-missing",
			background: true,
		});
		await flush();
		composer(container).focus();
		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 1) });
		await flush();
		const focused = document.activeElement;
		if (!(focused instanceof HTMLElement)) throw new Error("focus was dropped");
		const panel = panelOf(focused);
		expect(panel.hidden).toBe(false);
		// Nothing inside is focusable, so the panel itself takes focus.
		expect(focused).toBe(panel);
		expect(panel.getAttribute("tabindex")).toBe("-1");
	});

	test("every view of the active tab is shown, not only the focused pane", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "split", direction: "row" });
		await flush();
		const panels = [
			...container.querySelectorAll<HTMLElement>('[role="tabpanel"]'),
		];
		expect(panels.length).toBe(2);
		expect(panels.map((p) => p.hidden)).toEqual([false, false]);
	});
});

describe("two mounted Bridge views", () => {
	test("closing one Bridge keeps the board commands for the other", async () => {
		const { store } = mountApp("/");
		// The second tab is a Bridge too: opened elsewhere, navigated to `/` in place.
		store.dispatchLayout({ kind: "open", path: "/settings/general" });
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

	test("switching between two Bridges hands the commands over without a duplicate", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/settings/general" });
		await flush();
		store.showBridge();
		await flush();
		// A register over a live id means two Bridges held it at once.
		const registry = store.keyboard.registry;
		const register = registry.register;
		const overlaps: string[] = [];
		registry.register = (cmd) => {
			if (registry.get(cmd.id)) overlaps.push(cmd.id);
			register(cmd);
		};
		store.dispatchLayout({ kind: "focusTab", tabId: tabIdAt(store, 0) });
		await flush();
		expect(overlaps).toEqual([]);

		// The command drives the shown Bridge's cursor, not the hidden one's.
		const [first, second] = [
			...container.querySelectorAll<HTMLElement>('[role="tabpanel"]'),
		];
		if (!first || !second) throw new Error("expected two panels");
		expect(first.hidden).toBe(false);
		const cursor = (panel: HTMLElement) =>
			panel
				.querySelector('.bridge-grid [tabindex="0"]')
				?.getAttribute("aria-label");
		const hiddenBefore = cursor(second);
		const shownBefore = cursor(first);
		registry.get("list.moveNext" as CommandId)?.run();
		await flush();
		expect(cursor(first)).not.toBe(shownBefore);
		expect(cursor(second)).toBe(hiddenBefore);
	});
});
