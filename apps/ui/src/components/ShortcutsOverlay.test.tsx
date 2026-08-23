import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { flush, mountApp } from "../test-router";

// The shortcuts overlay's rendered contract (RIG-2482). Mounts the full shell
// via mountApp and drives the overlay through the store's `toggleShortcuts` —
// the same seam the `?` chord uses in production — so these prove the surface
// wiring (dialog aria, auto-focus, Escape, live filtering, focus trap + restore)
// without self-registering `view.shortcuts`.

const dialog = (c: HTMLElement) =>
	c.querySelector<HTMLElement>(
		'[role="dialog"][aria-label="Keyboard shortcuts"]',
	);
const search = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".cx-shortcuts-search");

afterEach(cleanup);

describe("ShortcutsOverlay (RIG-2482)", () => {
	test("renders a labelled modal dialog when opened", async () => {
		const { store, container } = mountApp("/backlog");
		expect(dialog(container)).toBeNull();

		store.toggleShortcuts();
		await flush();

		const d = dialog(container);
		expect(d).not.toBeNull();
		expect(d?.getAttribute("aria-modal")).toBe("true");
	});

	test("auto-focuses the search input on open", async () => {
		const { store, container } = mountApp("/backlog");
		store.toggleShortcuts();
		await flush();

		expect(document.activeElement).toBe(search(container));
	});

	test("Escape closes the overlay via the store", async () => {
		const { store, container } = mountApp("/backlog");
		store.toggleShortcuts();
		await flush();
		const d = dialog(container);
		if (!d) throw new Error("no dialog");

		fireEvent.keyDown(d, { key: "Escape" });
		await flush();
		expect(store.shortcutsOpen()).toBe(false);
	});

	test("typing in the search box filters the rows", async () => {
		const { store, container } = mountApp("/backlog");
		store.toggleShortcuts();
		await flush();
		const input = search(container);
		if (!input) throw new Error("no search input");

		const before = container.querySelectorAll(".cx-shortcuts-row").length;
		expect(before).toBeGreaterThan(1);

		fireEvent.input(input, { target: { value: "bridge" } });
		await flush();
		const rows = container.querySelectorAll(".cx-shortcuts-row");
		expect(rows.length).toBe(1);
		expect(rows[0]?.textContent).toContain("Bridge");
	});

	test("a no-match query shows the dim empty row and no rows", async () => {
		const { store, container } = mountApp("/backlog");
		store.toggleShortcuts();
		await flush();
		const input = search(container);
		if (!input) throw new Error("no search input");

		fireEvent.input(input, { target: { value: "zzzznope" } });
		await flush();
		expect(container.querySelectorAll(".cx-shortcuts-row").length).toBe(0);
		expect(container.querySelector(".cx-shortcuts-empty")).not.toBeNull();
	});

	test("focus-restore: focus returns to the pre-open element on close", async () => {
		const { store, container } = mountApp("/backlog");
		const button = container.querySelector<HTMLButtonElement>(".view-tab");
		if (!button) throw new Error("no topbar button");
		button.focus();
		expect(document.activeElement).toBe(button);

		store.toggleShortcuts();
		await flush();
		expect(document.activeElement).toBe(search(container));

		store.hideShortcuts();
		await flush();
		expect(document.activeElement).toBe(button);
	});

	test("focus trap: Tab from the last focusable wraps to the first; Escape still closes", async () => {
		const { store, container } = mountApp("/backlog");
		store.toggleShortcuts();
		await flush();
		const d = dialog(container);
		const input = search(container);
		if (!d || !input) throw new Error("no dialog/input");

		// With only the search input focusable, it is both first and last: Tab
		// off it wraps back to itself (focus never leaves the dialog).
		input.focus();
		fireEvent.keyDown(d, { key: "Tab" });
		await flush();
		expect(d.contains(document.activeElement)).toBe(true);

		// The local Escape handler is still reachable after tabbing.
		fireEvent.keyDown(d, { key: "Escape" });
		await flush();
		expect(store.shortcutsOpen()).toBe(false);
	});
});
