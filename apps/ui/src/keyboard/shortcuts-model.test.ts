import { describe, expect, test } from "bun:test";
import type { Command, CommandId, CommandScope } from "./commands";
import type { KeymapEntry, Platform } from "./keymap";
import { createCommandRegistry } from "./registry";
import { buildShortcutGroups, type ShortcutGroup } from "./shortcuts-model";

// buildShortcutGroups is the render-time JOIN of DEFAULT_KEYMAP × CommandRegistry.
// Hand-built keymap + registry fixtures are legitimate HERE (per the record): the
// unit under test IS the join, so it must be fed both sides directly.

const id = (s: string): CommandId => s as CommandId;

const entry = (chord: string, commandId: string): KeymapEntry => ({
	chord,
	commandId: id(commandId),
});

const makeCommand = (
	rawId: string,
	title: string,
	scope: CommandScope,
	keywords: string[] = [],
): Command => ({
	id: id(rawId),
	title,
	keywords,
	scope,
	run: () => {},
});

function registryOf(...commands: Command[]) {
	const registry = createCommandRegistry();
	for (const c of commands) registry.register(c);
	return registry;
}

describe("buildShortcutGroups", () => {
	test("omits keymap rows whose command is unregistered (dead chord)", () => {
		const keymap = [
			entry("Mod+B", "view.bridge"),
			entry("Mod+K", "palette.open"), // unregistered
		];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global"),
		);

		const groups = buildShortcutGroups(keymap, registry, "other", "");
		const ids = groups.flatMap((g) => g.rows.map((r) => r.commandId));
		expect(ids).toEqual([id("view.bridge")]);
	});

	test("omits registered commands that have no keymap row", () => {
		const keymap = [entry("Mod+B", "view.bridge")];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global"),
			// registered but no keymap row → never surfaced
			makeCommand("board.openCardCrossLink", "Open card cross-link", "main"),
		);

		const groups = buildShortcutGroups(keymap, registry, "other", "");
		const ids = groups.flatMap((g) => g.rows.map((r) => r.commandId));
		expect(ids).toEqual([id("view.bridge")]);
	});

	test("groups by command.scope in the fixed order global,left,main,right,topbar", () => {
		const keymap = [
			entry("Enter", "list.openOrSelect"), // main
			entry("Mod+B", "view.bridge"), // global
			entry("T", "topbar.act"), // topbar
			entry("L", "left.act"), // left
			entry("R", "right.act"), // right
		];
		const registry = registryOf(
			makeCommand("list.openOrSelect", "Open or select", "main"),
			makeCommand("view.bridge", "Go to Bridge", "global"),
			makeCommand("topbar.act", "Topbar act", "topbar"),
			makeCommand("left.act", "Left act", "left"),
			makeCommand("right.act", "Right act", "right"),
		);

		const groups = buildShortcutGroups(keymap, registry, "other", "");
		expect(groups.map((g) => g.scope)).toEqual([
			"global",
			"left",
			"main",
			"right",
			"topbar",
		]);
	});

	test("preserves keymap order within a group", () => {
		const keymap = [
			entry("ArrowUp", "list.movePrev"),
			entry("ArrowDown", "list.moveNext"),
			entry("Enter", "list.openOrSelect"),
		];
		const registry = registryOf(
			makeCommand("list.moveNext", "Move down", "main"),
			makeCommand("list.movePrev", "Move up", "main"),
			makeCommand("list.openOrSelect", "Open or select", "main"),
		);

		const groups = buildShortcutGroups(keymap, registry, "other", "");
		const main = groups.find((g) => g.scope === "main");
		expect(main?.rows.map((r) => r.chord)).toEqual([
			"ArrowUp",
			"ArrowDown",
			"Enter",
		]);
	});

	test("resolves Mod to the platform modifier in the rendered chord", () => {
		const keymap = [entry("Mod+B", "view.bridge")];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global"),
		);

		const mac = buildShortcutGroups(keymap, registry, "mac", "");
		const other = buildShortcutGroups(keymap, registry, "other", "");
		expect(mac[0]?.rows[0]?.chord).toBe("Cmd+B");
		expect(other[0]?.rows[0]?.chord).toBe("Ctrl+B");
	});

	test("substring filter matches title, keyword, and resolved chord", () => {
		const keymap = [
			entry("Mod+B", "view.bridge"),
			entry("Enter", "list.openOrSelect"),
		];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global", ["board", "kanban"]),
			makeCommand("list.openOrSelect", "Open or select", "main", ["select"]),
		);
		const platform: Platform = "other";

		// title
		expect(
			ids(buildShortcutGroups(keymap, registry, platform, "bridge")),
		).toEqual([id("view.bridge")]);
		// keyword
		expect(
			ids(buildShortcutGroups(keymap, registry, platform, "kanban")),
		).toEqual([id("view.bridge")]);
		// resolved chord ("Ctrl+B") — proves the filter sees the RESOLVED chord
		expect(
			ids(buildShortcutGroups(keymap, registry, platform, "ctrl")),
		).toEqual([id("view.bridge")]);
	});

	test("filter is case-insensitive", () => {
		const keymap = [entry("Mod+B", "view.bridge")];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global"),
		);
		expect(
			ids(buildShortcutGroups(keymap, registry, "other", "BRIDGE")),
		).toEqual([id("view.bridge")]);
	});

	test("empty query passes every registered row", () => {
		const keymap = [
			entry("Mod+B", "view.bridge"),
			entry("Enter", "list.openOrSelect"),
		];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global"),
			makeCommand("list.openOrSelect", "Open or select", "main"),
		);
		expect(ids(buildShortcutGroups(keymap, registry, "other", "")).length).toBe(
			2,
		);
	});

	test("a no-match query yields no groups", () => {
		const keymap = [entry("Mod+B", "view.bridge")];
		const registry = registryOf(
			makeCommand("view.bridge", "Go to Bridge", "global"),
		);
		expect(buildShortcutGroups(keymap, registry, "other", "zzz")).toEqual([]);
	});
});

function ids(groups: ShortcutGroup[]): CommandId[] {
	return groups.flatMap((g) => g.rows.map((r) => r.commandId));
}
