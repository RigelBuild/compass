import { afterEach, describe, expect, test } from "bun:test";
import { cleanup } from "@solidjs/testing-library";
import type { CommandId } from "./keyboard/commands";
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

		expect(event.defaultPrevented).toBe(true);
	});

	test("after dispose App's onCleanup removes the listener: view.bridge never runs", async () => {
		setPlatform("other");
		const { store } = mountApp("/backlog");
		expect(store.view()).toBe("backlog");

		// Spy the shared `view.bridge` command on the app-lifetime registry (a
		// plain Map that outlives the reactive root). Observing the command
		// directly — not the frozen store view — is what discriminates here: a
		// leaked listener still resolves the chord and calls run() even though the
		// disposed store's view() can no longer update, so a store-view assertion
		// would pass on the very uninstall bug it purports to catch.
		let viewBridgeRuns = 0;
		store.keyboard.registry.register({
			id: "view.bridge" as CommandId,
			title: "Go to Bridge",
			keywords: [],
			scope: "global",
			run: () => viewBridgeRuns++,
		});

		// Unmount the shell — App's onCleanup runs the keymap uninstaller.
		cleanup();

		press({ key: "b", ctrlKey: true });
		await flush();
		expect(viewBridgeRuns).toBe(0);
	});
});

describe("shortcuts overlay (RIG-2482)", () => {
	const dialog = (c: HTMLElement) =>
		c.querySelector<HTMLElement>(
			'[role="dialog"][aria-label="Keyboard shortcuts"]',
		);
	const search = (c: HTMLElement) =>
		c.querySelector<HTMLInputElement>(".cx-shortcuts-search");
	const rowText = (c: HTMLElement): string[] =>
		Array.from(c.querySelectorAll(".cx-shortcuts-row")).map(
			(r) => r.textContent ?? "",
		);

	test("? opens the overlay; a second ? toggles it closed; focus restores", async () => {
		setPlatform("other");
		const { container } = mountApp("/backlog");
		expect(dialog(container)).toBeNull();

		// Focus a topbar button first, so restore has a target.
		const button = container.querySelector<HTMLButtonElement>(".view-tab");
		if (!button) throw new Error("no topbar button");
		button.focus();

		press({ key: "?", shiftKey: true });
		await flush();
		expect(dialog(container)).not.toBeNull();
		// The overlay took initial focus off the button.
		expect(document.activeElement).toBe(search(container));

		// A second ? with focus OUTSIDE the search input toggles it closed. Move
		// focus off the input first (the guard would suppress ? inside the input).
		button.focus();
		press({ key: "?", shiftKey: true });
		await flush();
		expect(dialog(container)).toBeNull();
		// Focus restored to the pre-open element.
		expect(document.activeElement).toBe(button);
	});

	test("? does NOT open while a text input is focused (editable-target guard)", async () => {
		setPlatform("other");
		const { container } = mountApp("/backlog");
		const input = document.createElement("input");
		input.type = "text";
		container.appendChild(input);
		input.focus();

		// Dispatch on the input so it bubbles to the window listener with
		// event.target = the input (the production shape the guard reads).
		input.dispatchEvent(
			new KeyboardEvent("keydown", {
				key: "?",
				shiftKey: true,
				bubbles: true,
				cancelable: true,
			}),
		);
		await flush();
		expect(dialog(container)).toBeNull();

		input.remove();
	});

	test("? inside the overlay's own search input does not re-toggle (stays open)", async () => {
		setPlatform("other");
		const { container } = mountApp("/backlog");
		press({ key: "?", shiftKey: true });
		await flush();
		const box = search(container);
		if (!box) throw new Error("no search input");
		box.focus();

		// The dispatcher's editable-target guard suppresses ? in the input (the
		// event bubbles from the input, so event.target is the input), so the
		// overlay stays open (no re-toggle).
		box.dispatchEvent(
			new KeyboardEvent("keydown", {
				key: "?",
				shiftKey: true,
				bubbles: true,
				cancelable: true,
			}),
		);
		await flush();
		expect(dialog(container)).not.toBeNull();
	});

	test("generated content on REAL registrations: Shift+Enter row shows, palette.open does not", async () => {
		setPlatform("other");
		const { container } = mountApp("/");
		expect(container.querySelector(".bridge")).not.toBeNull();

		press({ key: "?", shiftKey: true });
		await flush();

		const rows = rowText(container);
		expect(
			rows.some(
				(t) => t.includes("Open assigned agent") && t.includes("Shift+Enter"),
			),
		).toBe(true);
		// palette.open (Mod+K) is tabled but unregistered on main → omitted.
		expect(rows.some((t) => t.includes("Ctrl+K"))).toBe(false);
	});

	test("drift-immunity: registering palette.open makes the Mod+K row appear on next open", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/backlog");

		press({ key: "?", shiftKey: true });
		await flush();
		expect(rowText(container).some((t) => t.includes("Ctrl+K"))).toBe(false);
		// Close before mutating so the snapshot-at-open memo recomputes.
		store.hideShortcuts();
		await flush();

		store.keyboard.registry.register({
			id: "palette.open" as CommandId,
			title: "Open command palette",
			keywords: ["palette", "command"],
			scope: "global",
			run: () => {},
		});

		press({ key: "?", shiftKey: true });
		await flush();
		expect(
			rowText(container).some(
				(t) => t.includes("Open command palette") && t.includes("Ctrl+K"),
			),
		).toBe(true);
	});

	test("list.* rows follow the board lifecycle (RIG-2529 substrate)", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		expect(container.querySelector(".bridge")).not.toBeNull();

		press({ key: "?", shiftKey: true });
		await flush();
		expect(
			rowText(container).some(
				(t) => t.includes("Move up") && t.includes("ArrowUp"),
			),
		).toBe(true);
		store.hideShortcuts();
		await flush();

		// Navigate to a board-less route: the board unmounts, list.* retract.
		store.showBacklog();
		await flush();
		expect(container.querySelector(".bridge")).toBeNull();

		press({ key: "?", shiftKey: true });
		await flush();
		expect(
			rowText(container).some(
				(t) => t.includes("Move up") && t.includes("ArrowUp"),
			),
		).toBe(false);
	});

	test("tier-3 leak non-regression: Enter on a non-board button runs no list command, no preventDefault", async () => {
		setPlatform("other");
		const { container } = mountApp("/");
		expect(container.querySelector(".bridge")).not.toBeNull();

		const button = container.querySelector<HTMLButtonElement>(".view-tab");
		if (!button) throw new Error("no topbar button");
		button.focus();

		const event = press({ key: "Enter" });
		await flush();
		// The scope gate: list.openOrSelect is scope:"main", the active zone is
		// not main (a topbar button), so tier 3 declines — native activation
		// survives (no preventDefault).
		expect(event.defaultPrevented).toBe(false);
	});
});
