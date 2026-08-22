import { afterEach, describe, expect, test } from "bun:test";
import { cleanup } from "@solidjs/testing-library";
import { flush, mountApp } from "./test-router";

// Real-wiring global-chord tests (RIG-2456 T2/A6). These mount the FULL shell
// through `mountApp` — store + StoreContext + MemoryRouter root={App} +
// AppRoutes, the production shape — and register NOTHING: they prove App.tsx
// installs the keymap at the root over `store.keyboard` and that
// `createKeyboardSpine` registered `view.bridge → store.showBridge()`. If either
// were missing, `Mod+B` would be a dead chord and these red.

// Force the platform installKeymap detects (read once at install from navigator),
// mirroring dispatch.test.ts's technique.
function setPlatform(platform: "mac" | "other"): void {
	Object.defineProperty(navigator, "platform", {
		value: platform === "mac" ? "MacIntel" : "X11; Linux x64",
		configurable: true,
	});
}

const press = (init: KeyboardEventInit): KeyboardEvent => {
	const event = new KeyboardEvent("keydown", {
		bubbles: true,
		cancelable: true,
		...init,
	});
	window.dispatchEvent(event);
	return event;
};

afterEach(() => {
	cleanup();
	setPlatform("other");
});

describe("App-root keyboard spine (RIG-2456)", () => {
	test("Ctrl+B from a non-board surface flips to the Bridge", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/backlog");
		expect(store.view()).toBe("backlog");

		press({ key: "b", ctrlKey: true });
		await flush();

		expect(store.view()).toBe("bridge");
		expect(container.querySelector(".bridge")).not.toBeNull();
	});

	test("Meta+B on mac flips to the Bridge (platform stub)", async () => {
		setPlatform("mac");
		const { store, container } = mountApp("/backlog");
		expect(store.view()).toBe("backlog");

		press({ key: "b", metaKey: true });
		await flush();

		expect(store.view()).toBe("bridge");
		expect(container.querySelector(".bridge")).not.toBeNull();
	});

	test("Mod+B while a board stop is focused still flips (tier-1 fall-through)", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		expect(store.view()).toBe("bridge");
		// Focus the board cursor stop (the sole tabindex=0): the board declines the
		// global Mod+B at tier 1, so it must fall through to the global view.bridge.
		container
			.querySelector<HTMLElement>('.bridge-grid [tabindex="0"]')
			?.focus();

		const event = press({ key: "b", ctrlKey: true });
		await flush();

		expect(store.view()).toBe("bridge");
		expect(event.defaultPrevented).toBe(true);
	});

	test("after dispose the listener is removed: Ctrl+B is inert", async () => {
		setPlatform("other");
		const { store } = mountApp("/backlog");
		expect(store.view()).toBe("backlog");

		// Unmount the shell — App's onCleanup runs the keymap uninstaller.
		cleanup();

		// The disposed store's view is frozen; a post-dispose chord does nothing.
		press({ key: "b", ctrlKey: true });
		await flush();
		expect(store.view()).toBe("backlog");
	});
});
