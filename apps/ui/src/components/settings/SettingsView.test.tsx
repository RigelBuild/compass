import { describe, expect, test } from "bun:test";
import { fireEvent } from "@solidjs/testing-library";
import { flush, mountApp } from "../../test-router";

describe("SettingsView", () => {
	test("bare settings canonicalizes to General and selects its nav link", async () => {
		const { store, container } = mountApp("/settings");
		await flush();

		expect(store.focusedView().path()).toBe("/settings/general");
		const general = container.querySelector<HTMLAnchorElement>(
			'a.cx-tab[href="#/settings/general"]',
		);
		expect(general?.dataset.selected).toBe("");
		expect(general?.getAttribute("aria-current")).toBe("page");
	});

	test("clicking Models shows the Models section instead of the tracker editor", async () => {
		const { store, container } = mountApp("/settings/tracker");
		await flush();
		const models = [
			...container.querySelectorAll<HTMLAnchorElement>("a.cx-tab"),
		].find((link) => link.textContent?.trim() === "Models");
		if (!models) throw new Error("missing Models section link");

		fireEvent.click(models);
		await flush();

		expect(store.focusedView().path()).toBe("/settings/models");
		expect(
			container.querySelector(".settings-body h2")?.textContent?.trim(),
		).toBe("Models");
		expect(container.querySelector('input[placeholder="you@org"]')).toBeNull();
	});

	test("middle-click and Mod+click on a section open it in a new tab", async () => {
		const { store, container } = mountApp("/settings/tracker");
		await flush();
		const models = container.querySelector<HTMLAnchorElement>(
			'a.cx-tab[href="#/settings/models"]',
		);
		if (!models) throw new Error("missing Models section link");

		fireEvent(models, new MouseEvent("auxclick", { bubbles: true, button: 1 }));
		await flush();
		expect(store.layout().tabs).toHaveLength(2);
		expect(store.focusedView().path()).toBe("/settings/models");

		fireEvent.click(models, { ctrlKey: true, metaKey: true });
		await flush();
		// Opening a path that already has a tab focuses it, so no third tab.
		expect(store.layout().tabs).toHaveLength(2);
	});

	test("a background Settings tab does not set the section openers return to", async () => {
		const { store } = mountApp("/");
		await flush();
		store.dispatchLayout({
			kind: "open",
			path: "/settings/models",
			background: true,
		});
		await flush();
		expect(store.settingsPath()).toBe("/settings/general");
	});

	test("refocusing a Settings tab makes its section the last one shown", async () => {
		const { store } = mountApp("/settings/tracker");
		await flush();
		const first = store.layout().activeTabId;
		store.dispatchLayout({ kind: "open", path: "/settings/models" });
		await flush();
		store.dispatchLayout({ kind: "focusTab", tabId: first });
		await flush();
		store.showBridge();
		await flush();

		store.showSettings();
		await flush();
		expect(store.focusedView().path()).toBe("/settings/tracker");
	});

	test("two mounted Settings views do not share element ids", async () => {
		const { store, container } = mountApp("/settings/tracker");
		await flush();
		store.dispatchLayout({
			kind: "open",
			path: "/settings/tracker",
			fresh: true,
		});
		await flush();
		expect(container.querySelectorAll(".settings-view")).toHaveLength(2);
		const ids = [...container.querySelectorAll(".settings-view [id]")].map(
			(node) => node.id,
		);
		expect(ids.length).toBeGreaterThan(0);
		expect(new Set(ids).size).toBe(ids.length);
	});

	test("showSettings returns to the section last shown, in the same tab", async () => {
		const { store, container } = mountApp("/settings/tracker");
		await flush();
		const models = container.querySelector<HTMLAnchorElement>(
			'a.cx-tab[href="#/settings/models"]',
		);
		if (!models) throw new Error("missing Models section link");
		fireEvent.click(models);
		await flush();
		store.showBridge();
		await flush();

		store.showSettings();
		await flush();
		expect(store.layout().tabs).toHaveLength(1);
		expect(store.focusedView().path()).toBe("/settings/models");
	});

	test("the selected section stays current after focus leaves and returns", async () => {
		// A real origin, so the router would claim same-origin links if it could.
		window.location.href = "http://localhost/";
		const { store, container } = mountApp("/settings/tracker");
		await flush();
		const settingsTab = store.layout().activeTabId;
		store.dispatchLayout({ kind: "open", path: "/" });
		await flush();
		store.dispatchLayout({ kind: "focusTab", tabId: settingsTab });
		await flush();

		const current = [
			...container.querySelectorAll<HTMLAnchorElement>("a.cx-tab"),
		].filter((link) => link.getAttribute("aria-current") === "page");
		expect(current.map((link) => link.textContent?.trim())).toEqual([
			"Tracker",
		]);
	});
});
