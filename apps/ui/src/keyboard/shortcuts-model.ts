/**
 * Generated shortcuts model (RIG-2482). Computes the overlay's grouped rows as a
 * render-time JOIN of the default keymap (chord truth) against the live command
 * registry (behavior + title truth) — so the sheet can never drift from the real
 * bindings. Pure: no DOM, no `navigator` read (the platform is passed in). The
 * `ShortcutsOverlay` component only renders what this returns.
 *
 * Join semantics (Approach §2):
 *   - Iterate DEFAULT_KEYMAP; resolve each `entry.commandId` via `registry.get()`.
 *   - OMIT a keymap row whose command is unregistered (the dispatcher treats it
 *     as dead, so listing it would advertise a chord that does nothing).
 *   - A registered command with no keymap row is implicitly omitted (never
 *     iterated — the keymap is the row source).
 *   - Group by the resolved COMMAND's `scope` (NOT the keymap `when` field),
 *     ordered `global, left, main, right, topbar`; keymap order within a group.
 *   - Render chords via `resolveChord(entry.chord, platform)`.
 *   - Substring filter (case-insensitive over the lowercased title, each
 *     keyword, and the resolved chord); an empty query passes every row.
 */

import type { CommandId, CommandRegistry, CommandScope } from "./commands";
import { type KeymapEntry, type Platform, resolveChord } from "./keymap";

export interface ShortcutRow {
	readonly chord: string; // platform-resolved via resolveChord
	readonly title: string; // command.title
	readonly commandId: CommandId;
}

export interface ShortcutGroup {
	readonly scope: CommandScope; // "global" | FocusZone (zones.ts:23)
	readonly rows: ShortcutRow[];
}

/** Group order: global first, then the zones in their fixed order (zones.ts:23). */
const SCOPE_ORDER: readonly CommandScope[] = [
	"global",
	"left",
	"main",
	"right",
	"topbar",
];

export function buildShortcutGroups(
	keymap: readonly KeymapEntry[],
	registry: CommandRegistry,
	platform: Platform,
	query: string,
): ShortcutGroup[] {
	const needle = query.trim().toLowerCase();
	const byScope = new Map<CommandScope, ShortcutRow[]>();

	for (const entry of keymap) {
		const command = registry.get(entry.commandId);
		if (!command) continue; // unregistered → dead chord, omit

		const chord = resolveChord(entry.chord, platform);
		if (needle && !matches(command.title, command.keywords, chord, needle)) {
			continue;
		}

		const row: ShortcutRow = {
			chord,
			title: command.title,
			commandId: entry.commandId,
		};
		const rows = byScope.get(command.scope);
		if (rows) rows.push(row);
		else byScope.set(command.scope, [row]);
	}

	const groups: ShortcutGroup[] = [];
	for (const scope of SCOPE_ORDER) {
		const rows = byScope.get(scope);
		if (rows && rows.length > 0) groups.push({ scope, rows });
	}
	return groups;
}

/** Case-insensitive substring over title, each keyword, and the resolved chord. */
function matches(
	title: string,
	keywords: readonly string[],
	chord: string,
	needle: string,
): boolean {
	if (title.toLowerCase().includes(needle)) return true;
	if (chord.toLowerCase().includes(needle)) return true;
	for (const keyword of keywords) {
		if (keyword.toLowerCase().includes(needle)) return true;
	}
	return false;
}
