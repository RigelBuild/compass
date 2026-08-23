import { afterEach, describe, expect, test } from "bun:test";
import type { Command, CommandId, CommandScope } from "./commands";
import { detectPlatform, eventToChord, installKeymap } from "./dispatch";
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

const makeCommand = (
	rawId: string,
	run: () => void,
	scope: CommandScope = "global",
): Command => ({
	id: id(rawId),
	title: rawId,
	keywords: [],
	scope,
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
	test("drops Shift for a modifier-less printable non-letter (RIG-2482): ? stays ?", () => {
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "?", shiftKey: true }),
				"other",
			),
		).toBe("?");
	});

	test("Space is carved out of Shift-drop: Shift+Space stays Shift+Space", () => {
		// The `" "` → `"Space"` rename runs before the Shift-drop predicate, so
		// Space is multi-char and keeps its Shift — otherwise it would rebind to
		// the bare `Space` chord (list.expandOrToggle).
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: " ", shiftKey: true }),
				"other",
			),
		).toBe("Shift+Space");
	});

	test("letters keep Shift+UPPER shape: Shift+B stays Shift+B", () => {
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "b", shiftKey: true }),
				"other",
			),
		).toBe("Shift+B");
	});

	test("modifier-carrying and multi-char chords are untouched by Shift-drop", () => {
		// Mod+Shift+\ carries a command modifier, so Shift is preserved.
		expect(
			eventToChord(
				new KeyboardEvent("keydown", {
					key: "\\",
					ctrlKey: true,
					shiftKey: true,
				}),
				"other",
			),
		).toBe("Ctrl+Shift+\\");
		// Shift+Enter: multi-char key, Shift preserved.
		expect(
			eventToChord(
				new KeyboardEvent("keydown", { key: "Enter", shiftKey: true }),
				"other",
			),
		).toBe("Shift+Enter");
	});
});

describe("detectPlatform", () => {
	test("reads navigator: mac stub → mac, otherwise other", () => {
		setPlatform("mac");
		expect(detectPlatform()).toBe("mac");
		setPlatform("other");
		expect(detectPlatform()).toBe("other");
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

	// Tier-3 scope gate (RIG-2529): a matched global entry runs only if its
	// COMMAND's scope is "global" or equals the active zone. `activeZone`
	// defaults to () => null, under which only global-scoped commands pass.
	test("scope gate: a global command fires from tier 3 with no group and no zone", () => {
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(makeCommand("view.bridge", () => ran++, "global"));
		uninstall = installKeymap(registry, () => null);

		const event = keydown({ key: "b", ctrlKey: true });

		expect(ran).toBe(1);
		expect(event.defaultPrevented).toBe(true);
	});

	test("scope gate: a scope:'main' command does NOT run at tier 3 when no zone is active", () => {
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(
			makeCommand("board.openAssignedAgent", () => ran++, "main"),
		);
		// Default zone stub (() => null): the leak scenario — board mounted but
		// unfocused, so no group and no zone.
		uninstall = installKeymap(registry, () => null);

		const event = keydown({ key: "Enter", shiftKey: true });

		expect(ran).toBe(0); // "main" === null → false, command does not run
		expect(event.defaultPrevented).toBe(false); // native activation survives
	});

	test("scope gate: a scope:'main' command RUNS at tier 3 when its zone is active", () => {
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(
			makeCommand("board.openAssignedAgent", () => ran++, "main"),
		);
		uninstall = installKeymap(
			registry,
			() => null,
			() => "main",
		);

		const event = keydown({ key: "Enter", shiftKey: true });

		expect(ran).toBe(1); // "main" === "main" → runs
		expect(event.defaultPrevented).toBe(true);
	});

	test("scope gate: a declining group falls through to a scope:'main' command when its zone is active", () => {
		// The one genuinely new reachable path the gate creates: a group-relative
		// command re-invoked at tier 3 after its group declined, with the zone
		// derived from that same group live (spine.ts:95-97). Matches production.
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(makeCommand("list.openOrSelect", () => ran++, "main"));
		const { handle, routed } = stubGroup(() => false); // declines
		uninstall = installKeymap(
			registry,
			() => handle,
			() => "main",
		);

		const event = keydown({ key: "Enter" });

		expect(routed).toEqual([id("list.openOrSelect")]);
		expect(ran).toBe(1); // fell through, zone active → runs
		expect(event.defaultPrevented).toBe(true);
	});

	test("scope gate: a scope:'main' command does NOT run at tier 3 when a DIFFERENT zone is active", () => {
		// Locks the `command.scope === zone` EQUALITY, not merely `zone != null`:
		// a main-scoped command must stay inert while another zone holds focus
		// (the cross-zone leak D1 chose this predicate over an isGroupRelative
		// skip to prevent — e.g. a scope:'right' sidebar command bound unscoped).
		// Not reachable in production today (single main-zone group ⇒ activeZone()
		// is only ever "main" or null), but the gate is the general contract.
		const registry = createCommandRegistry();
		let ran = 0;
		registry.register(
			makeCommand("board.openAssignedAgent", () => ran++, "main"),
		);
		uninstall = installKeymap(
			registry,
			() => null,
			() => "right",
		);

		const event = keydown({ key: "Enter", shiftKey: true });

		expect(ran).toBe(0); // "main" !== "right" → does not run
		expect(event.defaultPrevented).toBe(false); // native activation survives
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
