import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import type { CommandId } from "../keyboard/commands";
import type { AppStore } from "../store";
import { flush, mountApp } from "../test-router";
import {
	loadLayout,
	type TabLayout,
	type WindowLayout,
} from "../window-layout";

// A split tab renders both views in one row or column, sized by the ratio,
// with a keyboard- and pointer-driven splitter whose ratio persists.

const TOPIC_PATH = "/channel/ch-svc-compass/topic/top-compass-acp";

type Split = Extract<TabLayout, { kind: "split" }>;

function memoryStorage(): Storage {
	const items = new Map<string, string>();
	return {
		get length() {
			return items.size;
		},
		clear: () => items.clear(),
		getItem: (key) => items.get(key) ?? null,
		key: (index) => [...items.keys()][index] ?? null,
		removeItem: (key) => {
			items.delete(key);
		},
		setItem: (key, value) => {
			items.set(key, value);
		},
	};
}

const activeSplit = (layout: WindowLayout): Split => {
	const tab = layout.tabs.find((item) => item.id === layout.activeTabId);
	if (tab?.layout.kind !== "split") throw new Error("active tab is not split");
	return tab.layout;
};
const splitter = (c: HTMLElement): HTMLElement => {
	const el = c.querySelector<HTMLElement>('.cx-split-pane [role="separator"]');
	if (!el) throw new Error("no splitter");
	return el;
};
const panel = (c: HTMLElement, viewId: string): HTMLElement => {
	const el = c.querySelector<HTMLElement>(`#view-panel-${viewId}`);
	if (!el) throw new Error(`no panel for ${viewId}`);
	return el;
};
const runCommand = (store: AppStore, id: string): void => {
	const command = store.keyboard.registry.get(id as CommandId);
	if (!command) throw new Error(`missing command ${id}`);
	command.run();
};

async function mountSplit(direction: "row" | "column") {
	const storage = memoryStorage();
	const mounted = mountApp(TOPIC_PATH, storage);
	await flush();
	mounted.store.dispatchLayout({ kind: "split", direction });
	await flush();
	return { ...mounted, storage };
}

afterEach(() => cleanup());

describe("SplitPane", () => {
	test("a row split shows both views side by side, sized by the ratio", async () => {
		const { store, container } = await mountSplit("row");
		const split = activeSplit(store.layout());
		const first = panel(container, split.first.id);
		const second = panel(container, split.second.id);
		const root = first.parentElement;
		expect(root?.classList.contains("cx-split-pane")).toBe(true);
		expect(root?.dataset.direction).toBe("row");
		expect(second.parentElement).toBe(root);
		expect([first.hidden, second.hidden]).toEqual([false, false]);
		expect([first.style.flexGrow, second.style.flexGrow]).toEqual([
			"0.5",
			"0.5",
		]);
		const sep = splitter(container);
		expect(sep.getAttribute("aria-orientation")).toBe("vertical");
		expect(sep.getAttribute("aria-controls")).toBe(first.id);
		// The splitter sits between the two panes.
		expect(first.nextElementSibling).toBe(sep);
		expect(sep.nextElementSibling).toBe(second);
	});

	test("a column split stacks the views with a horizontal splitter", async () => {
		const { store, container } = await mountSplit("column");
		const split = activeSplit(store.layout());
		expect(
			panel(container, split.first.id).parentElement?.dataset.direction,
		).toBe("column");
		expect(splitter(container).getAttribute("aria-orientation")).toBe(
			"horizontal",
		);
	});

	test("a single-view tab has no splitter", async () => {
		const { container } = mountApp(TOPIC_PATH);
		await flush();
		expect(container.querySelector('[role="separator"]')).toBeNull();
	});

	test("arrow keys step the ratio along the split axis, clamped and persisted", async () => {
		const { store, container, storage } = await mountSplit("row");
		const sep = splitter(container);
		expect(sep.tabIndex).toBe(0);
		expect(
			["aria-valuemin", "aria-valuemax", "aria-valuenow"].map((name) =>
				sep.getAttribute(name),
			),
		).toEqual(["20", "80", "50"]);

		fireEvent.keyDown(sep, { key: "ArrowRight" });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.55);
		expect(sep.getAttribute("aria-valuenow")).toBe("55");

		// Each real keydown is its own task, so the layout settles between them.
		for (let i = 0; i < 10; i++) {
			fireEvent.keyDown(sep, { key: "ArrowRight" });
			await flush();
		}
		expect(activeSplit(store.layout()).ratio).toBe(0.8);
		expect(sep.getAttribute("aria-valuenow")).toBe("80");

		fireEvent.keyDown(sep, { key: "ArrowLeft" });
		await flush();
		// The cross axis does nothing on a row split.
		fireEvent.keyDown(sep, { key: "ArrowUp" });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.75);
		expect(activeSplit(loadLayout(storage, TOPIC_PATH)).ratio).toBe(0.75);
	});

	test("up and down step a column split", async () => {
		const { store, container } = await mountSplit("column");
		const sep = splitter(container);
		fireEvent.keyDown(sep, { key: "ArrowUp" });
		fireEvent.keyDown(sep, { key: "ArrowRight" });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.45);
	});

	test("dragging the splitter resizes, clamped to the bounds, and persists", async () => {
		const { store, container, storage } = await mountSplit("row");
		const sep = splitter(container);
		const root = sep.parentElement;
		if (!root) throw new Error("splitter has no root");
		root.getBoundingClientRect = () => new DOMRect(100, 0, 1000, 600);

		fireEvent.pointerDown(sep, { pointerId: 1, button: 0, clientX: 600 });
		fireEvent.pointerMove(sep, { pointerId: 1, buttons: 1, clientX: 400 });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.3);
		const split = activeSplit(store.layout());
		expect(panel(container, split.first.id).style.flexGrow).toBe("0.3");
		expect(panel(container, split.second.id).style.flexGrow).toBe("0.7");

		fireEvent.pointerMove(sep, { pointerId: 1, buttons: 1, clientX: 120 });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.2);

		fireEvent.pointerUp(sep, { pointerId: 1, clientX: 120 });
		fireEvent.pointerMove(sep, { pointerId: 1, buttons: 1, clientX: 900 });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.2);
		expect(activeSplit(loadLayout(storage, TOPIC_PATH)).ratio).toBe(0.2);
	});

	test("a drag ends when capture is lost or the button is no longer held", async () => {
		const { store, container } = await mountSplit("row");
		const sep = splitter(container);
		const root = sep.parentElement;
		if (!root) throw new Error("splitter has no root");
		root.getBoundingClientRect = () => new DOMRect(100, 0, 1000, 600);

		fireEvent.pointerDown(sep, { pointerId: 1, button: 0, clientX: 600 });
		fireEvent.pointerMove(sep, { pointerId: 1, buttons: 0, clientX: 400 });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.5);

		// Capture lost with no pointerup reaching the splitter.
		fireEvent.lostPointerCapture(sep, { pointerId: 1 });
		fireEvent.pointerMove(sep, { pointerId: 1, buttons: 1, clientX: 400 });
		await flush();
		expect(activeSplit(store.layout()).ratio).toBe(0.5);
	});

	test("removing a focused splitter keeps focus in a pane", async () => {
		for (const command of ["pane.closeOther", "tab.close", "tab.next"]) {
			const { store, container } = await mountSplit("row");
			if (command !== "pane.closeOther") {
				store.dispatchLayout({
					kind: "open",
					path: "/settings",
					background: true,
				});
				await flush();
			}
			splitter(container).focus();
			runCommand(store, command);
			await flush();
			const active = document.activeElement;
			expect(active === document.body || active === null).toBe(false);
			expect(active?.closest(".view-panel:not([hidden])")).not.toBeNull();
			cleanup();
		}
	});

	test("the focused pane carries the focus marker, and only in a split", async () => {
		const { store, container } = await mountSplit("row");
		const split = activeSplit(store.layout());
		const first = panel(container, split.first.id);
		const second = panel(container, split.second.id);
		expect([first.dataset.focused, second.dataset.focused]).toEqual([
			undefined,
			"",
		]);
		runCommand(store, "pane.focusFirst");
		await flush();
		expect([first.dataset.focused, second.dataset.focused]).toEqual([
			"",
			undefined,
		]);
		runCommand(store, "pane.closeOther");
		await flush();
		expect(first.dataset.focused).toBeUndefined();
	});

	test("splitting and closing the other pane keep the first view mounted", async () => {
		const { store, container } = mountApp(TOPIC_PATH);
		await flush();
		const input = container.querySelector<HTMLInputElement>(
			".conv-composer input.field",
		);
		if (!input) throw new Error("no composer");
		fireEvent.input(input, { target: { value: "draft" } });
		store.dispatchLayout({ kind: "split", direction: "row" });
		await flush();
		runCommand(store, "pane.focusFirst");
		runCommand(store, "pane.closeOther");
		await flush();
		expect(input.isConnected).toBe(true);
		expect(input.value).toBe("draft");
	});

	test("an inactive split tab hides its splitter with its panes", async () => {
		const { store, container } = await mountSplit("row");
		const sep = splitter(container);
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		expect(sep.closest("[hidden]")).not.toBeNull();
	});
});
