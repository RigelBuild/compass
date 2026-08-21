import { afterEach, describe, expect, test } from "bun:test";
import type { Command, CommandId } from "./commands";
import { eventToChord, installKeymap } from "./dispatch";
import type { Platform } from "./keymap";
import { createCommandRegistry } from "./registry";
import type { RovingGroupHandle } from "./roving";
import type { RovingGroup } from "./zones";

// The keymap dispatcher: chord normalization + the RD-2 three-tier resolution
// (active group → scoped → global) with fall-through, the editable-target guard,
// and native-activation suppression on handled group chords. Real KeyboardEvents
// are dispatched on the window (or a focused element, so event.target is set).

const id = (s: string): CommandId => s as CommandId;
const GROUP: RovingGroup = { zone: "main", id: "board" };

const makeCommand = (rawId: string, run: () => void): Command => ({
	id: id(rawId),
	title: rawId,
	keywords: [],
	scope: "global",
	run,
});

// A group handle stub: records routed ids, returns a caller-set verdict.
function stubGroup(handled: (cmd: CommandId) => boolean): {
	handle: RovingGroupHandle;
	routed: CommandId[];
} {
	const routed: CommandId[] = [];
	return {
		routed,
		handle: {
			group: GROUP,
			handleCommand(cmd) {
				routed.push(cmd);
				return handled(cmd);
			},
			// The dispatcher gates nothing on isFocused (the Bridge install does);
			// this stub is fed directly as the active group, so it reports focused.
			isFocused: () => true,
			focus() {},
		},
	};
}

// Force the platform installKeymap detects (read once at install from navigator).
function setPlatform(platform: Platform): void {
	Object.defineProperty(navigator, "platform", {
		value: platform === "mac" ? "MacIntel" : "X11; Linux x64",
		configurable: true,
	});
}

function keydown(init: KeyboardEventInit, target?: HTMLElement): KeyboardEvent {
	const event = new KeyboardEvent("keydown", {
		bubbles: true,
		cancelable: true,
		...init,
	});
	(target ?? window).dispatchEvent(event);
	return event;
}

describe("eventToChord", () => {
	test("normalizes Space, upper-cases letters, orders Mod+Shift+Alt+Key", () => {
		expect(
			eventToChord(new KeyboardEvent("keydown", { key: " " }), "other"),
		).toBe("Space");
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "b", ctrlKey: true }),
				"other",
			),
		).toBe("Ctrl+B");
		expect(
			eventToChord(
				new KeyboardEvent("keydown", {
					key: "ArrowLeft",
					ctrlKey: true,
					altKey: true,
				}),
				"other",
			),
		).toBe("Ctrl+Alt+ArrowLeft");
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "Enter", shiftKey: true }),
				"other",
			),
		).toBe("Shift+Enter");
	});

	test("resolves Mod to the platform modifier (Cmd on mac, Ctrl elsewhere)", () => {
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "b", metaKey: true }),
				"mac",
			),
		).toBe("Cmd+B");
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "b", ctrlKey: true }),
				"other",
			),
		).toBe("Ctrl+B");
	});
});

describe("installKeymap", () => {
	let uninstall: (() => void) | null = null;

	afterEach(() => {
		uninstall?.();
		uninstall = null;
		setPlatform("other");
	});

	test("Mod+B fires view.bridge in the global tier (both platforms)", () => {
		for (const platform of ["other", "mac"] as const) {
			setPlatform(platform);
			const registry = createCommandRegistry();
			let ran = 0;
			registry.register(makeCommand("view.bridge", () => ran++));
			const stop = installKeymap(registry, () => null);

			const event = keydown(
				platform === "mac"
					? { key: "b", metaKey: true }
					: { key: "b", ctrlKey: true },
			);

			expect(ran).toBe(1);
			expect(event.defaultPrevented).toBe(true);
			stop();
		}
	});

	test("active group claims a list chord: routes and suppresses native activation", () => {
		const registry = createCommandRegistry();
		const { handle, routed } = stubGroup(() => true);
		uninstall = installKeymap(registry, () => handle);

		const event = keydown({ key: "Enter" });

		expect(routed).toEqual([id("list.openOrSelect")]);
		expect(event.defaultPrevented).toBe(true);
	});

	test("active-group beats scoped and global for a claimed chord", () => {
		const registry = createCommandRegistry();
		let commsRan = 0;
		registry.register(makeCommand("comms.send", () => commsRan++));
		const { handle, routed } = stubGroup(() => true);
		uninstall = installKeymap(
			registry,
			() => handle,
			() => "main",
		);

		keydown({ key: "Enter" });

		// The board group owns Enter — comms.send (when:"main") never runs.
		expect(routed).toEqual([id("list.openOrSelect")]);
		expect(commsRan).toBe(0);
	});

	test("scoped tier beats global when its zone is active", () => {
		// A hypothetical scoped chord routing: Enter has both a global
		// (list.openOrSelect) and a scoped (comms.send, when:"main") entry. With
		// no active group and the main zone active on a NON-editable target, the
		// scoped entry wins.
		const registry = createCommandRegistry();
		let commsRan = 0;
		let listRan = 0;
		registry.register(makeCommand("comms.send", () => commsRan++));
		registry.register(makeCommand("list.openOrSelect", () => listRan++));
		uninstall = installKeymap(
			registry,
			() => null,
			() => "main",
		);

		keydown({ key: "Enter" });

		expect(commsRan).toBe(1);
		expect(listRan).toBe(0);
	});

	test("fall-through: unregistered scoped command drops to the global tier", () => {
		const registry = createCommandRegistry();
		let listRan = 0;
		// comms.send NOT registered; list.openOrSelect is.
		registry.register(makeCommand("list.openOrSelect", () => listRan++));
		uninstall = installKeymap(
			registry,
			() => null,
			() => "main",
		);

		keydown({ key: "Enter" });

		expect(listRan).toBe(1);
	});

	test("fall-through: a declining group drops the chord to the global tier", () => {
		const registry = createCommandRegistry();
		let listRan = 0;
		registry.register(makeCommand("list.openOrSelect", () => listRan++));
		const { handle, routed } = stubGroup(() => false); // declines
		uninstall = installKeymap(registry, () => handle);

		keydown({ key: "Enter" });

		expect(routed).toEqual([id("list.openOrSelect")]);
		expect(listRan).toBe(1); // fell through to global
	});

	test("fall-through: Mod+B reaches view.bridge while the board group is active", () => {
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(makeCommand("view.bridge", () => ran++));
		const { handle, routed } = stubGroup(() => true);
		uninstall = installKeymap(registry, () => handle);

		// The group does not claim Mod+B (not group-relative), so it never routes.
		keydown({ key: "b", ctrlKey: true });

		expect(routed).toEqual([]);
		expect(ran).toBe(1);
	});

	test("unregistered global command does not swallow the event", () => {
		const registry = createCommandRegistry(); // view.bridge NOT registered
		uninstall = installKeymap(registry, () => null);

		const event = keydown({ key: "b", ctrlKey: true });

		expect(event.defaultPrevented).toBe(false);
	});

	test("editable-target guard: a modifier-less chord is ignored in an input", () => {
		const registry = createCommandRegistry();
		const { handle, routed } = stubGroup(() => true);
		uninstall = installKeymap(registry, () => handle);

		const input = document.createElement("input");
		document.body.appendChild(input);
		const event = keydown({ key: "Enter" }, input);

		expect(routed).toEqual([]); // never routed
		expect(event.defaultPrevented).toBe(false);
		input.remove();
	});

	test("editable-target guard does NOT block modifier chords", () => {
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(makeCommand("view.bridge", () => ran++));
		uninstall = installKeymap(registry, () => null);

		const input = document.createElement("input");
		document.body.appendChild(input);
		keydown({ key: "b", ctrlKey: true }, input);

		expect(ran).toBe(1); // Mod+B fires even from a text field
		input.remove();
	});

	test("an unmapped chord is left alone", () => {
		const registry = createCommandRegistry();
		uninstall = installKeymap(registry, () => null);

		const event = keydown({ key: "q" });

		expect(event.defaultPrevented).toBe(false);
	});

	test("the uninstaller removes the listener", () => {
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(makeCommand("view.bridge", () => ran++));
		const stop = installKeymap(registry, () => null);
		stop();

		keydown({ key: "b", ctrlKey: true });

		expect(ran).toBe(0);
	});
});
