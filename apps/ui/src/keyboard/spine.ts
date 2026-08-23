/**
 * Keyboard spine (RIG-2456) — the app-lifetime home of the keyboard-first
 * product's shared command registry plus the set of roving groups any surface
 * has published. It hangs off the store (`store.keyboard`, record RD-1/A1) so
 * every surface reaches the one registry through `useStore()` with no extra
 * provider, and `App.tsx` installs the single window keymap listener over the
 * spine's accessors (record A4).
 *
 * `activeGroup()` is the tier-1 focus gate, and it lives HERE — moved out of the
 * Bridge's former per-surface closure (`Bridge.tsx:463-467` on main) into this
 * one root accessor so the RIG-2130 focus-exclusivity contract is structural,
 * not per-surface convention: a surface publishes its group and CANNOT opt out
 * of the gate. The board claims a group-relative chord (Enter/Space/Arrows/
 * Home/End) in tier 1 ONLY while DOM focus is actually on one of its stops;
 * otherwise `activeGroup()` returns null and the chord falls through to the
 * scoped/global tiers — so a mounted-but-unfocused board never traps the
 * toolbar/sidebar buttons (design §401-405).
 *
 * `activeGroup()` scans the registered set for the group whose `isFocused()` is
 * true and returns the first match (record A2/RD-2). Two invariants make the
 * scan safe:
 *   (a) One group per stop element. `isFocused()` is
 *       `stops().some(s => s.el === document.activeElement)` — element IDENTITY,
 *       not containment (roving.ts:31) — so at most one group matches provided a
 *       stop element belongs to at most one registered group (specced surfaces
 *       have disjoint DOM: board cards, tree rows, sidebar tabs). Set iteration
 *       is insertion-order deterministic, so even a pathological shared element
 *       resolves deterministically.
 *   (b) A stale handle is dispatch-inert. A handle left in the set after a
 *       missed `onCleanup` holds only detached elements, and a detached element
 *       is never `document.activeElement`, so a leak degrades to a dead
 *       per-keydown scan — never a misfire.
 *
 * NOTE: `activeGroup()`/`activeZone()` are PLAIN functions called per keydown by
 * the dispatcher, not reactive signals — the group set needs no reactivity
 * (record T1 §343-345), so it is a plain `Set`, read fresh on each call.
 */

import type { Command, CommandId, CommandRegistry } from "./commands";
import { createCommandRegistry } from "./registry";
import type { RovingGroupHandle } from "./roving";
import type { FocusZone } from "./zones";

/**
 * The app's keyboard seam: the shared command registry plus the live set of
 * published roving groups, with the tier-1/tier-2 accessors the root
 * `installKeymap` reads.
 */
export interface KeyboardSpine {
	readonly registry: CommandRegistry;
	registerGroup(handle: RovingGroupHandle): void;
	unregisterGroup(handle: RovingGroupHandle): void;
	/** The focused registered group, per handle.isFocused(); null when none. */
	activeGroup(): RovingGroupHandle | null;
	/** activeGroup()?.group.zone ?? null — the tier-2 accessor. */
	activeZone(): FocusZone | null;
}

/**
 * Create the keyboard spine. Registers `view.bridge → deps.showBridge()`,
 * `view.shortcuts → deps.toggleShortcuts()` (RIG-2482), and the RIG-2483 seed
 * commands — `palette.open → deps.togglePalette()` plus the view seeds
 * `view.settings`/`view.backlog`/`view.done` (→ the matching `show*`) — as its
 * commands before returning (record A4 §182-186 / A3/D6: the registration lives
 * with the behavior — the spine is created in `createAppStore` where the
 * closures are in scope), so `Mod+B`, `?`, `Mod+K`, and `Mod+,` resolve through
 * the real wiring with no App-specific setup. No seed sets a `shortcut` string:
 * chips derive from the keymap via `shortcutFor` (D4).
 */
export function createKeyboardSpine(deps: {
	showBridge: () => void;
	toggleShortcuts: () => void;
	showBacklog: () => void;
	showDone: () => void;
	showSettings: () => void;
	togglePalette: () => void;
}): KeyboardSpine {
	const registry = createCommandRegistry();
	const viewBridge: Command = {
		id: "view.bridge" as CommandId,
		title: "Go to Bridge",
		keywords: ["board", "bridge", "kanban"],
		scope: "global",
		run: () => deps.showBridge(),
	};
	registry.register(viewBridge);
	const viewShortcuts: Command = {
		id: "view.shortcuts" as CommandId,
		title: "Keyboard shortcuts",
		keywords: ["help", "shortcuts", "keys", "keymap"],
		scope: "global",
		run: () => deps.toggleShortcuts(),
	};
	registry.register(viewShortcuts);
	const paletteOpen: Command = {
		id: "palette.open" as CommandId,
		title: "Open command palette",
		keywords: ["palette", "command", "search", "k"],
		scope: "global",
		run: () => deps.togglePalette(),
	};
	registry.register(paletteOpen);
	const viewSettings: Command = {
		id: "view.settings" as CommandId,
		title: "Go to Settings",
		keywords: ["settings", "preferences", "config", "tracker"],
		scope: "global",
		run: () => deps.showSettings(),
	};
	registry.register(viewSettings);
	const viewBacklog: Command = {
		id: "view.backlog" as CommandId,
		title: "Go to Backlog",
		keywords: ["backlog", "todo", "queue"],
		scope: "global",
		run: () => deps.showBacklog(),
	};
	registry.register(viewBacklog);
	const viewDone: Command = {
		id: "view.done" as CommandId,
		title: "Go to Done",
		keywords: ["done", "archive", "completed"],
		scope: "global",
		run: () => deps.showDone(),
	};
	registry.register(viewDone);

	const groups = new Set<RovingGroupHandle>();

	const activeGroup = (): RovingGroupHandle | null => {
		for (const g of groups) if (g.isFocused()) return g;
		return null;
	};

	return {
		registry,
		registerGroup(handle: RovingGroupHandle): void {
			groups.add(handle);
		},
		unregisterGroup(handle: RovingGroupHandle): void {
			groups.delete(handle);
		},
		activeGroup,
		activeZone(): FocusZone | null {
			return activeGroup()?.group.zone ?? null;
		},
	};
}
