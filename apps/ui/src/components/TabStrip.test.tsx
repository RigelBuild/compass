import { afterEach, describe, expect, jest, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { type AppStore, NOTICE_TIMEOUT_MS } from "../store";
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

	const fillToCap = (store: AppStore): void => {
		for (let i = 1; i < MAX_TABS; i++) {
			store.dispatchLayout({ kind: "open", path: `/agent/agent-${i}` });
		}
	};
	const region = (c: HTMLElement): HTMLElement | null =>
		c.querySelector('[role="status"].layout-notice-region');

	test("at the tab cap a further open is refused and the strip is unchanged", async () => {
		const { store, container } = mountApp("/");
		fillToCap(store);
		await flush();
		expect(tabs(container).length).toBe(MAX_TABS);
		const before = store.layout();

		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		expect(store.layout()).toBe(before);
		expect(tabs(container).length).toBe(MAX_TABS);
		expect(selected(container)?.textContent).toContain("agent-9");
		expect(region(container)?.textContent).toContain(`${MAX_TABS} tabs`);
	});

	test("the notice region is mounted and empty before any refusal", () => {
		const { container } = mountApp("/");
		expect(region(container)).not.toBeNull();
		expect(region(container)?.textContent).toBe("");
	});

	test("a repeated refusal changes the announced text again", async () => {
		const { store, container } = mountApp("/");
		fillToCap(store);
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		const first = region(container)?.textContent;
		store.dispatchLayout({ kind: "open", path: "/done" });
		await flush();
		const second = region(container)?.textContent;
		expect(first).toContain(`${MAX_TABS} tabs`);
		expect(second).toContain(`${MAX_TABS} tabs`);
		expect(second).not.toBe(first);
	});

	test("an unrelated layout action keeps the notice", async () => {
		const { store, container } = mountApp("/");
		fillToCap(store);
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		store.dispatchLayout({
			kind: "focusTab",
			tabId: store.layout().tabs[0]?.id ?? "",
		});
		await flush();
		expect(region(container)?.textContent).toContain(`${MAX_TABS} tabs`);
	});

	test("the dismiss button clears the notice", async () => {
		const { store, container } = mountApp("/");
		fillToCap(store);
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		const dismiss = region(container)?.querySelector<HTMLElement>(
			'button[aria-label="Dismiss"]',
		);
		if (!dismiss) throw new Error("no dismiss button");
		fireEvent.click(dismiss);
		await flush();
		expect(region(container)?.textContent).toBe("");
	});

	test("the notice clears itself after its timeout", async () => {
		jest.useFakeTimers();
		try {
			const { store, container } = mountApp("/");
			fillToCap(store);
			store.dispatchLayout({ kind: "open", path: "/settings" });
			await flush();
			expect(region(container)?.textContent).toContain(`${MAX_TABS} tabs`);
			jest.advanceTimersByTime(NOTICE_TIMEOUT_MS + 1);
			await flush();
			expect(region(container)?.textContent).toBe("");
		} finally {
			jest.useRealTimers();
		}
	});

	const raiseNotice = async (): Promise<{
		store: AppStore;
		container: HTMLElement;
		dismiss: HTMLElement;
	}> => {
		const { store, container } = mountApp("/");
		fillToCap(store);
		store.dispatchLayout({ kind: "open", path: "/settings" });
		await flush();
		const dismiss = region(container)?.querySelector<HTMLElement>(
			'button[aria-label="Dismiss"]',
		);
		if (!dismiss) throw new Error("no dismiss button");
		return { store, container, dismiss };
	};

	test("the window losing focus keeps the hold of a focused notice", async () => {
		jest.useFakeTimers();
		const hasFocus = jest.spyOn(document, "hasFocus");
		try {
			const { container, dismiss } = await raiseNotice();
			dismiss.focus();
			hasFocus.mockReturnValue(false);
			fireEvent.focusOut(dismiss, { relatedTarget: null });
			jest.advanceTimersByTime(NOTICE_TIMEOUT_MS + 1);
			await flush();
			expect(region(container)?.textContent).toContain(`${MAX_TABS} tabs`);
		} finally {
			hasFocus.mockRestore();
			jest.useRealTimers();
		}
	});

	test("a notice that expires with focus inside hands focus to the active tab", async () => {
		jest.useFakeTimers();
		try {
			const { store, container, dismiss } = await raiseNotice();
			dismiss.focus();
			// Release the hold without moving focus, so expiry finds focus inside.
			store.holdLayoutNotice(false);
			jest.advanceTimersByTime(NOTICE_TIMEOUT_MS + 1);
			await flush();
			expect(region(container)?.textContent).toBe("");
			expect(document.activeElement).toBe(selected(container) ?? null);
		} finally {
			jest.useRealTimers();
		}
	});

	test("the notice holds while focus is inside it, and dismissing returns focus to the active tab", async () => {
		jest.useFakeTimers();
		try {
			const { store, container } = mountApp("/");
			fillToCap(store);
			store.dispatchLayout({ kind: "open", path: "/settings" });
			await flush();
			const dismiss = region(container)?.querySelector<HTMLElement>(
				'button[aria-label="Dismiss"]',
			);
			if (!dismiss) throw new Error("no dismiss button");
			dismiss.focus();
			jest.advanceTimersByTime(NOTICE_TIMEOUT_MS + 1);
			await flush();
			expect(region(container)?.textContent).toContain(`${MAX_TABS} tabs`);

			fireEvent.click(dismiss);
			await flush();
			expect(region(container)?.textContent).toBe("");
			expect(document.activeElement).toBe(selected(container) ?? null);
		} finally {
			jest.useRealTimers();
		}
	});

	test("the notice resumes its timeout once hover leaves it", async () => {
		jest.useFakeTimers();
		try {
			const { store, container } = mountApp("/");
			fillToCap(store);
			store.dispatchLayout({ kind: "open", path: "/settings" });
			await flush();
			const toast = region(container)?.querySelector<HTMLElement>(".cx-toast");
			if (!toast) throw new Error("no toast");
			fireEvent.mouseEnter(toast);
			jest.advanceTimersByTime(NOTICE_TIMEOUT_MS + 1);
			await flush();
			expect(region(container)?.textContent).toContain(`${MAX_TABS} tabs`);
			fireEvent.mouseLeave(toast);
			jest.advanceTimersByTime(NOTICE_TIMEOUT_MS + 1);
			await flush();
			expect(region(container)?.textContent).toBe("");
		} finally {
			jest.useRealTimers();
		}
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
