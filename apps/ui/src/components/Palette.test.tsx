import { afterEach, beforeEach, describe, expect, jest, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import { flush as flushSync } from "solid-js";
import { SEARCH_DEBOUNCE_MS } from "../keyboard/destination-surface";
import { flush, mountApp } from "../test-router";

// The command palette's rendered contract (RIG-2483). Mounts the full shell so
// the palette reads the REAL registry + store providers (registers nothing).
// Defends: action-mode fuzzy + scoped-above-global ranking against a captured
// zone, the toggle no-recapture case, chips derived from the keymap, navigation
// groups, loading + empty states, and select-navigates.

function setPlatform(platform: "mac" | "other"): void {
	Object.defineProperty(navigator, "platform", {
		value: platform === "mac" ? "MacIntel" : "Linux x86_64",
		configurable: true,
	});
}

const rows = (c: HTMLElement) =>
	Array.from(c.querySelectorAll<HTMLElement>(".cx-palette-row"));
const rowTitles = (c: HTMLElement): string[] =>
	rows(c).map((r) => r.querySelector(".cx-palette-title")?.textContent ?? "");
const groupLabels = (c: HTMLElement): string[] =>
	Array.from(c.querySelectorAll(".cx-palette-group")).map(
		(g) => g.textContent ?? "",
	);
const input = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".cx-palette-input");

/** Focus the board cursor stop so the captured open-time zone is "main". */
function focusBoardStop(container: HTMLElement): void {
	container.querySelector<HTMLElement>('.bridge-grid [tabindex="0"]')?.focus();
}
beforeEach(() => jest.useFakeTimers());
afterEach(() => {
	cleanup();
	jest.useRealTimers();
	setPlatform("other");
});

async function settle(): Promise<void> {
	jest.advanceTimersByTime(0);
	await flush();
	jest.advanceTimersByTime(SEARCH_DEBOUNCE_MS);
	await flush();
}
describe("Palette (RIG-2483)", () => {
	test("action mode: a query filters the registry, board main-scoped commands rank above global when opened from the board", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		focusBoardStop(container);
		store.openPalette();
		await flush();
		expect(store.paletteZone()).toBe("main");

		// "open" matches board.openAssignedAgent (scope main) + view commands via
		// keywords. With a captured main zone the board command ranks first.
		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "open" },
		});
		await settle();

		const titles = rowTitles(container);
		expect(titles.length).toBeGreaterThan(0);
		const boardIdx = titles.indexOf("Open assigned agent");
		const paletteIdx = titles.indexOf("Open command palette");
		expect(boardIdx).toBeGreaterThanOrEqual(0);
		expect(paletteIdx).toBeGreaterThanOrEqual(0);
		// The main-scoped board command sits above the global palette.open.
		expect(boardIdx).toBeLessThan(paletteIdx);
	});

	test("keyword-only match: a keyword hit surfaces a command whose title misses (A3)", async () => {
		jest.useFakeTimers();
		setPlatform("other");
		const { store, container } = mountApp("/");
		store.openPalette();
		await flush();

		// view.bridge's title is "Go to Bridge" (no "kanban"); "kanban" is one of
		// its seeded keywords. A title-miss/keyword-hit must still render the row,
		// proving bestCommandScore's keyword branch AND that Kobalte Search does
		// not drop keyword-only options through the render path.
		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "kanban" },
		});
		await settle();

		expect(rowTitles(container)).toContain("Go to Bridge");
	});

	test("toggle no-recapture: reopen-from-board still ranks main above global after a Mod+K toggle-close", async () => {
		jest.useFakeTimers();
		setPlatform("other");
		const { store, container } = mountApp("/");
		focusBoardStop(container);

		store.openPalette();
		await flush();
		expect(store.paletteZone()).toBe("main");

		// Toggle closed via the command run (focus now in the palette input). If
		// openPalette re-captured on the toggle leg it would read a null zone.
		store.keyboard.registry.get("palette.open" as never)?.run();
		await flush();
		expect(store.paletteOpen()).toBe(false);

		// Reopen from the board (focus restored to the board stop by closePalette).
		focusBoardStop(container);
		store.openPalette();
		await flush();
		expect(store.paletteZone()).toBe("main");

		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "open" },
		});
		await settle();
		const titles = rowTitles(container);
		expect(titles.indexOf("Open assigned agent")).toBeLessThan(
			titles.indexOf("Open command palette"),
		);
	});

	test("a command row renders its shortcut chip derived from the keymap", async () => {
		jest.useFakeTimers();
		setPlatform("other");
		const { store, container } = mountApp("/");
		store.openPalette();
		await flush();

		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "command palette" },
		});
		await settle();

		const row = rows(container).find(
			(r) =>
				r.querySelector(".cx-palette-title")?.textContent ===
				"Open command palette",
		);
		expect(row).toBeDefined();
		const chip = row?.querySelector(".cx-palette-shortcut");
		expect(chip).not.toBeNull();
		expect(chip?.textContent).toContain("Ctrl");
		expect(chip?.textContent).toContain("K");
	});

	test("navigation mode: destination groups render and selection navigates via the store", async () => {
		jest.useFakeTimers();
		setPlatform("other");
		const { store, container } = mountApp("/");
		store.openPalette();
		await flush();

		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "settings" },
		});
		await settle();

		// The Views group carries a "Settings" destination; the Commands group
		// carries "Go to Settings" (the seeded action).
		expect(groupLabels(container)).toContain("Views");
		expect(groupLabels(container)).toContain("Commands");

		const settingsRow = rows(container).find(
			(r) => r.querySelector(".cx-palette-title")?.textContent === "Settings",
		);
		expect(settingsRow).toBeDefined();
		fireEvent.click(settingsRow as HTMLElement);
		await flush();
		expect(store.view()).toBe("settings");
		expect(store.paletteOpen()).toBe(false);
	});

	test("empty state renders when both modes miss", async () => {
		jest.useFakeTimers();
		setPlatform("other");
		const { store, container } = mountApp("/");
		store.openPalette();
		await flush();

		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "zzznoresultsxyz" },
		});
		await settle();

		expect(rows(container).length).toBe(0);
		expect(container.querySelector(".cx-palette-empty")).not.toBeNull();
	});

	test("board commands' chips surface in the palette while the board is mounted", async () => {
		jest.useFakeTimers();
		setPlatform("other");
		const { store, container } = mountApp("/");
		expect(container.querySelector(".bridge")).not.toBeNull();
		store.openPalette();
		await flush();

		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "assigned agent" },
		});
		await settle();

		const row = rows(container).find(
			(r) =>
				r.querySelector(".cx-palette-title")?.textContent ===
				"Open assigned agent",
		);
		expect(row).toBeDefined();
		expect(row?.querySelector(".cx-palette-shortcut")?.textContent).toContain(
			"Shift",
		);
	});

	test("a .cx-palette-loading row (chase-light bar) shows while destination providers are in flight", async () => {
		setPlatform("other");
		jest.useFakeTimers();
		const { store, container } = mountApp("/");
		store.openPalette();
		await flush();
		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "set" },
		});
		flushSync();
		const loadingRow = container.querySelector(".cx-palette-loading");
		expect(loadingRow).not.toBeNull();
		expect(
			loadingRow?.querySelector('.cx-loader[data-topology="bar"]'),
		).not.toBeNull();
		jest.advanceTimersByTime(SEARCH_DEBOUNCE_MS);
		await flush();
		expect(container.querySelector(".cx-palette-loading")).toBeNull();
	});

	test("the LeftSidebar view buttons carry aria-keyshortcuts in WAI-ARIA tokens; the display chord moved to a CoachTip (RIG-2530), so no native title", () => {
		setPlatform("other");
		const { container } = mountApp("/");
		const links = Array.from(
			container.querySelectorAll<HTMLElement>(".left .bridge-link"),
		);
		const bridge = links.find((b) => b.textContent?.includes("Bridge"));
		const settings = links.find((b) => b.textContent?.includes("Settings"));
		const backlog = links.find((b) => b.textContent?.includes("Backlog"));
		const done = links.find((b) => b.textContent?.includes("Done"));
		// view.bridge → Mod+B, view.settings → Mod+, — aria uses the WAI-ARIA
		// Control token. The display chord no longer rides a native title (the
		// RIG-2530 sweep coaches it via CoachTip); a native title would
		// double-tooltip, so it must be absent.
		expect(bridge?.getAttribute("aria-keyshortcuts")).toBe("Control+B");
		expect(bridge?.getAttribute("title")).toBeNull();
		expect(settings?.getAttribute("aria-keyshortcuts")).toBe("Control+,");
		expect(settings?.getAttribute("title")).toBeNull();
		// view.backlog / view.done are sequence-only (G L / G D): shortcutForAria
		// skips the sequence so NO aria-keyshortcuts is emitted, and the RIG-2530
		// sweep moved coaching to a CoachTip, so there is no native title either.
		expect(backlog?.getAttribute("aria-keyshortcuts")).toBeNull();
		expect(backlog?.getAttribute("title")).toBeNull();
		expect(done?.getAttribute("aria-keyshortcuts")).toBeNull();
		expect(done?.getAttribute("title")).toBeNull();
	});
});
