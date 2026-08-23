import { describe, expect, test } from "bun:test";
import type { CommandId } from "./commands";
import { shortcutFor, shortcutForAria } from "./keymap";

// shortcutFor (RIG-2483, A5/D4) — the single derivation for every shortcut chip:
// the first DEFAULT_KEYMAP row bound to an id, resolveChord-resolved. Pure
// module. These defend the hit/miss/platform contract the chips rely on.

const id = (s: string): CommandId => s as CommandId;

describe("shortcutFor", () => {
	test("resolves a bound command's chord, platform-specific (Mod→Cmd/Ctrl)", () => {
		expect(shortcutFor(id("palette.open"), "other")).toBe("Ctrl+K");
		expect(shortcutFor(id("palette.open"), "mac")).toBe("Cmd+K");
	});

	test("resolves view.settings' Mod+, (the D6-lit chord)", () => {
		expect(shortcutFor(id("view.settings"), "other")).toBe("Ctrl+,");
		expect(shortcutFor(id("view.settings"), "mac")).toBe("Cmd+,");
	});

	test("a non-Mod chord passes through unchanged on both platforms", () => {
		expect(shortcutFor(id("board.openAssignedAgent"), "other")).toBe(
			"Shift+Enter",
		);
		expect(shortcutFor(id("board.openAssignedAgent"), "mac")).toBe(
			"Shift+Enter",
		);
	});

	test("undefined for a command with no keymap row (miss)", () => {
		expect(shortcutFor(id("board.openCardCrossLink"), "other")).toBeUndefined();
		expect(shortcutFor(id("view.backlog"), "other")).toBeUndefined();
		expect(shortcutFor(id("nonexistent.command"), "other")).toBeUndefined();
	});

	test("returns the FIRST matching row for an id bound more than once", () => {
		// Enter is bound to list.openOrSelect (unscoped) AND comms.send (when:main);
		// shortcutFor takes the first DEFAULT_KEYMAP row — list.openOrSelect's.
		expect(shortcutFor(id("list.openOrSelect"), "other")).toBe("Enter");
	});
});

describe("shortcutForAria", () => {
	test("emits WAI-ARIA modifier tokens (Mod→Control/Meta), never Ctrl/Cmd", () => {
		expect(shortcutForAria(id("view.bridge"), "other")).toBe("Control+B");
		expect(shortcutForAria(id("view.bridge"), "mac")).toBe("Meta+B");
	});

	test("undefined for a command with no keymap row (miss)", () => {
		expect(shortcutForAria(id("view.backlog"), "other")).toBeUndefined();
		expect(shortcutForAria(id("nonexistent.command"), "other")).toBeUndefined();
	});
});
