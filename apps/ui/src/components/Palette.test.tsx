import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent } from "@solidjs/testing-library";
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

afterEach(() => {
	cleanup();
	setPlatform("other");
});

// Kobalte Search debounces onInputChange through a 0ms setTimeout, so the query
// signal updates on a macrotask — `flush()` (microtasks) alone won't see it.
// `settle` waits one macrotask then drains the microtask queue.
async function settle(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setTimeout(resolve, 0);
	await promise;
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
		const { store, container } = mountApp("/");
		store.openPalette();
		await flush();

		fireEvent.input(input(container) as HTMLInputElement, {
			target: { value: "set" },
		});
		// Kobalte's 0ms debounce fires the query on a macrotask, which starts the
		// async destination resource; the loading row is up until its microtask
		// resolution lands. Advance past the debounce timer WITHOUT draining
		// microtasks so the in-flight window is observable.
		await new Promise((resolve) => {
			setTimeout(resolve, 0);
		});
		const loadingRow = container.querySelector(".cx-palette-loading");
		expect(loadingRow).not.toBeNull();
		expect(
			loadingRow?.querySelector('.cx-loader[data-topology="bar"]'),
		).not.toBeNull();

		// It clears once the providers resolve.
		await flush();
		expect(container.querySelector(".cx-palette-loading")).toBeNull();
	});

	test("the LeftSidebar view buttons carry aria-keyshortcuts in WAI-ARIA tokens while title keeps the display chord", () => {
		setPlatform("other");
		const { container } = mountApp("/");
		const links = Array.from(
			container.querySelectorAll<HTMLElement>(".left .bridge-link"),
		);
		const bridge = links.find((b) => b.textContent?.includes("Bridge"));
		const settings = links.find((b) => b.textContent?.includes("Settings"));
		// view.bridge → Mod+B, view.settings → Mod+, — aria uses Control (WAI-ARIA
		// token), the display title keeps Ctrl.
		expect(bridge?.getAttribute("aria-keyshortcuts")).toBe("Control+B");
		expect(bridge?.getAttribute("title")).toContain("Ctrl+B");
		expect(settings?.getAttribute("aria-keyshortcuts")).toBe("Control+,");
	});
});
