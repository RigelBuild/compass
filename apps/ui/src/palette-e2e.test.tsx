import { afterEach, describe, expect, test } from "bun:test";
import { cleanup } from "@solidjs/testing-library";
import { flush, mountApp } from "./test-router";

// Real-wiring palette tests (RIG-2483, A7). These mount the FULL shell through
// `mountApp` — the production shape — and register NOTHING: they prove App.tsx
// renders <Palette> behind store.paletteOpen(), that createKeyboardSpine
// registered palette.open → store.togglePalette(), and that the Mod+K chord
// rides the one App-root dispatcher. If any were missing, Ctrl+K would be a dead
// chord and these red. Mirrors keyboard-e2e.test.tsx's technique.

function setPlatform(platform: "mac" | "other"): void {
	Object.defineProperty(navigator, "platform", {
		value: platform === "mac" ? "MacIntel" : "Linux x86_64",
		configurable: true,
	});
}

const press = (init: KeyboardEventInit): KeyboardEvent => {
	const event = new KeyboardEvent("keydown", { bubbles: true, ...init });
	window.dispatchEvent(event);
	return event;
};

const palette = (c: HTMLElement) => c.querySelector<HTMLElement>(".cx-palette");
const paletteInput = (c: HTMLElement) =>
	c.querySelector<HTMLInputElement>(".cx-palette-input");

afterEach(() => {
	cleanup();
	setPlatform("other");
});

describe("command palette (RIG-2483)", () => {
	test("Ctrl+K opens the palette and focuses its input", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		expect(palette(container)).toBeNull();

		press({ key: "k", ctrlKey: true });
		await flush();

		expect(store.paletteOpen()).toBe(true);
		expect(palette(container)).not.toBeNull();
		const input = paletteInput(container);
		expect(input).not.toBeNull();
		expect(document.activeElement).toBe(input);
	});

	test("Meta+K on mac opens the palette (platform stub)", async () => {
		setPlatform("mac");
		const { store, container } = mountApp("/");

		press({ key: "k", metaKey: true });
		await flush();

		expect(store.paletteOpen()).toBe(true);
		expect(palette(container)).not.toBeNull();
	});

	test("a second Ctrl+K toggles it closed and restores focus to the pre-open element", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/backlog");
		// Focus a real pre-open element so the D3 snapshot has something to restore.
		const anchor = container.querySelector<HTMLElement>(".bridge-link");
		anchor?.focus();
		expect(document.activeElement).toBe(anchor);

		press({ key: "k", ctrlKey: true });
		await flush();
		expect(store.paletteOpen()).toBe(true);

		press({ key: "k", ctrlKey: true });
		await flush();
		expect(store.paletteOpen()).toBe(false);
		expect(palette(container)).toBeNull();
		expect(document.activeElement).toBe(anchor);
	});

	test("Escape closes the palette", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		press({ key: "k", ctrlKey: true });
		await flush();
		expect(store.paletteOpen()).toBe(true);

		const input = paletteInput(container);
		input?.dispatchEvent(
			new KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
		);
		await flush();

		expect(store.paletteOpen()).toBe(false);
		expect(palette(container)).toBeNull();
	});

	test("backdrop click closes the palette", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		press({ key: "k", ctrlKey: true });
		await flush();
		expect(store.paletteOpen()).toBe(true);

		container
			.querySelector<HTMLElement>(".cx-palette-backdrop")
			?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
		await flush();

		expect(store.paletteOpen()).toBe(false);
	});

	test("the palette registers nothing new on the shared registry", async () => {
		setPlatform("other");
		const { store } = mountApp("/");
		const before = store.keyboard.registry.all().length;

		press({ key: "k", ctrlKey: true });
		await flush();

		expect(store.keyboard.registry.all().length).toBe(before);
	});
});
