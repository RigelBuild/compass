import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { STUB_AGENTS } from "../stub-data";
import { flush, mountApp } from "../test-router";
import { MAX_TABS } from "../window-layout";

// The topbar view-tab strip: one tab per layout tab, labelled from
// the tab's route, dispatching focus / close / move into the window layout.

const AGENT_ID = "acc-compass-ui";
const AGENT_NAME =
	STUB_AGENTS.find((a) => a.account.id === AGENT_ID)?.account.displayName ??
	AGENT_ID;
const TOPIC_PATH = "/channel/ch-svc-compass/topic/top-compass-acp";

const tabs = (c: HTMLElement): HTMLElement[] => [
	...c.querySelectorAll<HTMLElement>('.cx-tab-strip [role="tab"]'),
];
const labels = (c: HTMLElement): string[] =>
	tabs(c).map((t) => t.querySelector(".cx-tab-strip-label")?.textContent ?? "");
const selected = (c: HTMLElement): HTMLElement | undefined =>
	tabs(c).find((t) => t.getAttribute("aria-selected") === "true");

const press = (init: KeyboardEventInit): KeyboardEvent => {
	const event = new KeyboardEvent("keydown", {
		bubbles: true,
		cancelable: true,
		...init,
	});
	window.dispatchEvent(event);
	return event;
};

afterEach(() => cleanup());

describe("TabStrip", () => {
	test("renders one tablist tab per layout tab, labelled from its route", async () => {
		const { store, container } = mountApp("/");
		const strip = container.querySelector(".cx-tab-strip");
		expect(strip?.getAttribute("role")).toBe("tablist");
		expect(labels(container)).toEqual(["Bridge"]);

		store.dispatchLayout({ kind: "open", path: `/agent/${AGENT_ID}` });
		store.dispatchLayout({ kind: "open", path: TOPIC_PATH });
		await flush();

		expect(labels(container)).toEqual([
			"Bridge",
			AGENT_NAME,
			"ACP seam review",
		]);
		// The agent tab keeps the old agent tab's state dot.
		expect(tabs(container)[1]?.querySelector(".cx-state-dot")).not.toBeNull();
		expect(selected(container)?.textContent).toContain("ACP seam review");
	});

	test("clicking a tab focuses it: the layout and the routed view follow", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/backlog" });
		await flush();
		expect(store.view()).toBe("backlog");

		const bridge = tabs(container).find((t) =>
			t.textContent?.includes("Bridge"),
		);
		if (!bridge) throw new Error("no Bridge tab");
		fireEvent.click(bridge);
		await flush();

		expect(store.view()).toBe("bridge");
		expect(store.layout().activeTabId).toBe(store.layout().tabs[0]?.id ?? "");
		expect(selected(container)).toBe(bridge);
	});

	test("the close button closes its tab; the last tab goes home instead", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		expect(tabs(container).length).toBe(2);

		const close = container.querySelector<HTMLElement>(
			'.cx-tab-strip button[aria-label="Close Settings"]',
		);
		if (!close) throw new Error("no close button for Settings");
		fireEvent.click(close);
		await flush();
		expect(labels(container)).toEqual(["Bridge"]);
		expect(store.view()).toBe("bridge");

		store.dispatchLayout({ kind: "navigateFocused", path: "/done" });
		await flush();
		const last = container.querySelector<HTMLElement>(
			'.cx-tab-strip button[aria-label="Close Done"]',
		);
		if (!last) throw new Error("no close button for Done");
		fireEvent.click(last);
		await flush();
		expect(labels(container)).toEqual(["Bridge"]);
		expect(store.view()).toBe("bridge");
	});

	test("arrow keys rove focus across the tabs without switching them", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/backlog" });
		store.dispatchLayout({ kind: "open", path: "/done" });
		await flush();
		const [first, second, third] = tabs(container);
		if (!first || !second || !third) throw new Error("expected three tabs");
		// One tab stop: the selected tab.
		expect(tabs(container).map((t) => t.tabIndex)).toEqual([-1, -1, 0]);

		third.focus();
		press({ key: "ArrowLeft" });
		await flush();
		expect(document.activeElement).toBe(second);
		press({ key: "Home" });
		await flush();
		expect(document.activeElement).toBe(first);
		// Roving moves focus only; Enter (native click) activates.
		expect(store.view()).toBe("done");
	});

	test("dropping a dragged tab on another moves it there", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/backlog" });
		store.dispatchLayout({ kind: "open", path: "/done" });
		await flush();
		const items = [
			...container.querySelectorAll<HTMLElement>(".cx-tab-strip-item"),
		];
		const [bridge, , done] = items;
		if (!bridge || !done) throw new Error("expected three tab items");

		fireEvent.dragStart(done);
		fireEvent.dragOver(bridge);
		fireEvent.drop(bridge);
		await flush();
		expect(labels(container)).toEqual(["Done", "Bridge", "Backlog"]);
		expect(store.view()).toBe("done");
	});

	test("at the tab cap a further open is refused and the strip is unchanged", async () => {
		const { store, container } = mountApp("/");
		for (let i = 1; i < MAX_TABS; i++) {
			store.dispatchLayout({ kind: "open", path: `/agent/agent-${i}` });
		}
		await flush();
		expect(tabs(container).length).toBe(MAX_TABS);
		const before = store.layout();

		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		expect(store.layout()).toBe(before);
		expect(tabs(container).length).toBe(MAX_TABS);
		expect(selected(container)?.textContent).toContain("agent-9");
		const notice = container.querySelector('[role="status"].cx-toast');
		expect(notice?.textContent).toContain(`${MAX_TABS} tabs`);
	});

	test("closing the active tab by its button leaves focus on the new active tab", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/done" });
		await flush();
		const close = container.querySelector<HTMLElement>(
			'.cx-tab-strip button[aria-label="Close Done"]',
		);
		if (!close) throw new Error("no close button for Done");
		close.focus();
		fireEvent.click(close);
		await flush();
		expect(store.view()).toBe("bridge");
		expect(document.activeElement).toBe(selected(container) ?? null);
	});

	test("Delete on the active tab closes it and focuses the new active tab", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/done" });
		await flush();
		const done = selected(container);
		if (!done) throw new Error("no selected tab");
		done.focus();
		fireEvent.keyDown(done, { key: "Delete" });
		await flush();
		expect(labels(container)).toEqual(["Bridge"]);
		expect(document.activeElement).toBe(selected(container) ?? null);
	});

	test("closing an unselected tab returns the tab stop to the selected tab", async () => {
		const { store, container } = mountApp("/");
		store.dispatchLayout({ kind: "open", path: "/backlog" });
		store.dispatchLayout({ kind: "open", path: "/done" });
		await flush();
		const [, backlog, done] = tabs(container);
		if (!backlog || !done) throw new Error("expected three tabs");
		done.focus();
		press({ key: "ArrowLeft" });
		await flush();
		expect(document.activeElement).toBe(backlog);
		fireEvent.keyDown(backlog, { key: "Delete" });
		await flush();
		expect(labels(container)).toEqual(["Bridge", "Done"]);
		expect(tabs(container).map((t) => t.tabIndex)).toEqual([-1, 0]);
	});

	test("on mac, Backspace closes the focused tab", async () => {
		const platform = navigator.platform;
		Object.defineProperty(navigator, "platform", {
			value: "MacIntel",
			configurable: true,
		});
		try {
			const { store, container } = mountApp("/");
			store.dispatchLayout({ kind: "open", path: "/done" });
			await flush();
			const done = selected(container);
			if (!done) throw new Error("no selected tab");
			fireEvent.keyDown(done, { key: "Backspace" });
			await flush();
			expect(labels(container)).toEqual(["Bridge"]);
		} finally {
			Object.defineProperty(navigator, "platform", {
				value: platform,
				configurable: true,
			});
		}
	});
});
