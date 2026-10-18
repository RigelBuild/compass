import { afterEach, describe, expect, test } from "bun:test";
import { cleanup } from "@solidjs/testing-library";
import type { CommandId } from "./keyboard/commands";
import { flush, mountApp } from "./test-router";

// Mount the production shell to test global chords through the shared keyboard spine.

// Stub the platform detected by installKeymap.
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
		// Focus the board stop; tier 1 declines Mod+B so the global command handles it.
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

		// Count dispatches directly because disposed stores do not update.
		let viewBridgeRuns = 0;
		store.keyboard.registry.register({
			id: "view.bridge" as CommandId,
			title: "Go to Bridge",
			keywords: [],
			scope: "global",
			run: () => viewBridgeRuns++,
		});

		cleanup();
		press({ key: "b", ctrlKey: true });
		await flush();
		expect(viewBridgeRuns).toBe(0);
	});

	test("Mod+, now lands on Settings (D6 seed lit view.settings' dead chord)", async () => {
		setPlatform("other");
		const { store } = mountApp("/");
		expect(store.view()).toBe("bridge");

		// Mod+, now resolves to Settings through the registered command.
		press({ key: ",", ctrlKey: true });
		await flush();

		expect(store.view()).toBe("settings");
	});

	test("G then S lands on Settings (leader sequence, real App wiring)", async () => {
		setPlatform("other");
		const { store } = mountApp("/");
		expect(store.view()).toBe("bridge");

		// G then S reaches the same Settings command through the leader sequence.
		press({ key: "g" });
		press({ key: "s" });
		await flush();

		expect(store.view()).toBe("settings");
	});

	test("G then L lands on Backlog (sequence-only command, real App wiring)", async () => {
		setPlatform("other");
		const { store } = mountApp("/");
		expect(store.view()).toBe("bridge");

		// view.backlog's ONLY keyboard binding is the `G L` sequence; this proves
		// the leader runtime resolves it end to end with no App-specific setup.
		press({ key: "g" });
		press({ key: "l" });
		await flush();

		expect(store.view()).toBe("backlog");
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
		const button = container.querySelector<HTMLButtonElement>(
			'.cx-tab-strip [role="tab"]',
		);
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

	test("generated content on REAL registrations: Shift+Enter and the seeded Mod+K rows show", async () => {
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
		// palette.open (Mod+K) is now a live RIG-2483 seed, so its row renders.
		expect(
			rows.some(
				(t) => t.includes("Open command palette") && t.includes("Ctrl+K"),
			),
		).toBe(true);
	});
	test("Bridge title-ref commands and Escape appear in the overlay", async () => {
		setPlatform("other");
		const { container } = mountApp("/");
		press({ key: "?", shiftKey: true });
		await flush();
		const rows = rowText(container);
		expect(
			rows.some(
				(text) =>
					text.includes("Focus next link in card title") && text.includes("R"),
			),
		).toBe(true);
		expect(
			rows.some(
				(text) =>
					text.includes("Focus previous link in card title") &&
					text.includes("Shift+R"),
			),
		).toBe(true);
		expect(
			rows.some(
				(text) => text.includes("Return to card") && text.includes("Escape"),
			),
		).toBe(true);
	});

	test("drift-immunity: registering a tabled-but-dead command makes its row appear on next open", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/backlog");

		// view.agentWorkspace (Mod+Shift+A, keymap.ts:65) stays tabled-but-
		// unregistered (OQ-4), so its chord is dead and its row omitted.
		press({ key: "?", shiftKey: true });
		await flush();
		expect(rowText(container).some((t) => t.includes("Ctrl+Shift+A"))).toBe(
			false,
		);
		// Close before mutating so the snapshot-at-open memo recomputes.
		store.hideShortcuts();
		await flush();

		store.keyboard.registry.register({
			id: "view.agentWorkspace" as CommandId,
			title: "Go to agent workspace",
			keywords: ["agent", "workspace"],
			scope: "global",
			run: () => {},
		});

		press({ key: "?", shiftKey: true });
		await flush();
		expect(
			rowText(container).some(
				(t) =>
					t.includes("Go to agent workspace") && t.includes("Ctrl+Shift+A"),
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

		// Backlog is a Bridge segment: the Bridge stays, but the board group and
		// its list.* rows retract off the grid segments.
		store.showBacklog();
		await flush();
		expect(container.querySelector(".bridge")).not.toBeNull();

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

		const button = container.querySelector<HTMLButtonElement>(
			'.cx-tab-strip [role="tab"]',
		);
		if (!button) throw new Error("no topbar button");
		button.focus();

		const event = press({ key: "Enter" });
		await flush();
		// A non-main focus zone declines the main-scoped command.
		expect(event.defaultPrevented).toBe(false);
	});
});

// Coached commands must exist in the dispatch registry, not only the display keymap.
describe("coached-chord dispatch (RIG-2530 T2)", () => {
	// Every command id the sweep coaches; each must resolve in the registry.
	const COACHED_COMMANDS = [
		"view.bridge",
		"view.settings",
		"sidebar.toggleLeft",
		"sidebar.toggleRight",
	] as const;

	test("every coached command id resolves in the command registry", () => {
		const { store } = mountApp("/");
		for (const id of COACHED_COMMANDS) {
			expect(store.keyboard.registry.get(id as CommandId)).toBeDefined();
		}
	});

	test("the sidebar-toggle chords now dispatch (live after T2 registration)", async () => {
		setPlatform("other");
		const { store } = mountApp("/");
		expect(store.leftOpen()).toBe(true);
		expect(store.rightOpen()).toBe(true);

		// Mod+Shift+\ → sidebar.toggleLeft (keymap.ts).
		press({ key: "\\", ctrlKey: true, shiftKey: true });
		await flush();
		expect(store.leftOpen()).toBe(false);

		// Mod+\ → sidebar.toggleRight.
		press({ key: "\\", ctrlKey: true });
		await flush();
		expect(store.rightOpen()).toBe(false);
	});
});
