/**
 * Default global keymap table — the single source of default chord bindings.
 *
 * Frozen by design record D5 (Global keymap, D5:444-457). CONTRACT + DEFAULT
 * DATA: the typed table below captures every default chord D5 lists. No runtime
 * behavior — compass-ui reads this table to install bindings.
 *
 * Single-file registry so a user-remap surface stays possible later; the
 * remapping UI itself is an NLB deferral and is out of scope.
 */

import type { CommandId, CommandScope } from "./commands";

/**
 * Mint a `CommandId` for a default binding. The referenced commands are
 * registered by compass-ui against the D5 command registry (commands.ts); this
 * cast is the authoring boundary for the default keymap.
 */
const cmd = (id: string): CommandId => id as CommandId;

/**
 * The platform-neutral modifier token (D5:456).
 *
 * Chords are authored with `Mod`, which resolves to `Cmd` on macOS and `Ctrl`
 * everywhere else. Authoring with `Mod` (never a literal `Ctrl`/`Cmd`) is the
 * convention that keeps the keymap portable.
 */
export const MOD = "Mod" as const;

/** Supported platforms for chord resolution. */
export type Platform = "mac" | "other";

/**
 * Resolve a `Mod`-authored chord to a concrete-modifier chord for `platform`:
 * `Mod` → `Cmd` on macOS, `Ctrl` elsewhere (D5:456). Pure string mapping.
 */
export const resolveChord = (chord: string, platform: Platform): string =>
	chord.replaceAll(MOD, platform === "mac" ? "Cmd" : "Ctrl");

/**
 * Resolve a `Mod`-authored chord to WAI-ARIA modifier tokens for `platform`:
 * `Mod` → `Meta` on macOS, `Control` elsewhere. Unlike `resolveChord` (which
 * emits the display tokens `Cmd`/`Ctrl`), this emits the fixed tokens
 * `aria-keyshortcuts` requires; `Cmd` is never emitted. Other chord parts
 * (`Shift`/`Alt`/`Enter`/`?`/…) are already ARIA-valid and pass through.
 */
export const resolveChordAria = (chord: string, platform: Platform): string =>
	chord.replaceAll(MOD, platform === "mac" ? "Meta" : "Control");

/**
 * Split a chord string into its sequence segments on a single space. A plain
 * (single-press) chord yields a one-element array; a leader sequence like
 * `"G B"` yields `["G", "B"]`. Space is unambiguous as the separator because
 * the literal Space key normalizes to the multi-char token `"Space"`
 * (`dispatch.ts`), so a raw `" "` never appears as a key name inside a chord.
 */
export function chordSegments(chord: string): string[] {
	return chord.split(" ");
}

/**
 * The set of resolved leader keys for `platform`: the `resolveChord`-resolved
 * FIRST segment of every multi-segment (sequence) row in `keymap`. Derived from
 * the table so the dispatcher never hard-codes a leader key — adding a second
 * leader later is a data change, not a runtime change. A single-chord row
 * contributes nothing.
 */
export function leaderPrefixes(
	keymap: readonly KeymapEntry[],
	platform: Platform,
): ReadonlySet<string> {
	const prefixes = new Set<string>();
	for (const entry of keymap) {
		const segments = chordSegments(entry.chord);
		if (segments.length > 1) {
			prefixes.add(resolveChord(segments[0], platform));
		}
	}
	return prefixes;
}

/**
 * The display form of a chord for `platform`. A single chord resolves through
 * `resolveChord` (`"Mod+B"` → `"Cmd+B"`); a leader sequence resolves each
 * segment and joins them with `" then "` (`"G B"` → `"G then B"`), making
 * press order explicit where a bare `"G B"` would read as one simultaneous
 * chord. The single formatter behind every display surface (chips, titles,
 * the shortcuts overlay).
 */
export function formatChordForDisplay(
	chord: string,
	platform: Platform,
): string {
	const segments = chordSegments(chord);
	if (segments.length === 1) return resolveChord(chord, platform);
	return segments
		.map((segment) => resolveChord(segment, platform))
		.join(" then ");
}

/**
 * The display chord for a command: the FIRST `DEFAULT_KEYMAP` row bound to `id`,
 * `resolveChord`-resolved for `platform` (Mod→Cmd/Ctrl). `undefined` when no row
 * binds the id. This is the single derivation for every shortcut chip (D4) — a
 * registration never hand-authors a `shortcut` string, so a chip can't drift
 * from the table or render an unresolved raw `Mod+…`.
 */
export function shortcutFor(
	id: CommandId,
	platform: Platform,
): string | undefined {
	const entry = DEFAULT_KEYMAP.find((e) => e.commandId === id);
	return entry ? formatChordForDisplay(entry.chord, platform) : undefined;
}

/**
 * The aria-keyshortcuts value for a command: like shortcutFor but with WAI-ARIA
 * modifier tokens (Mod→Control on other / Meta on mac, Cmd never emitted), so
 * assistive tech can parse the chord. Display chips keep shortcutFor (Ctrl/Cmd).
 */
export function shortcutForAria(
	id: CommandId,
	platform: Platform,
): string | undefined {
	const entry = DEFAULT_KEYMAP.find(
		(e) => e.commandId === id && chordSegments(e.chord).length === 1,
	);
	return entry ? resolveChordAria(entry.chord, platform) : undefined;
}

/**
 * A single default keymap binding.
 *
 * `chord` is authored with the `Mod` token (see `MOD`/`resolveChord`).
 * `when` scopes the binding to a focus zone or `'global'` (the same
 * `CommandScope` a `Command` carries). An absent `when` means the binding is
 * not zone-scoped: it is either window-global (e.g. the view/zone chords) or
 * resolved against the active roving group at dispatch (the Lists block
 * below). When the same chord is bound both with and without a `when`, the
 * scoped entry takes precedence while its zone is active (D5's ranking rule:
 * "scoped commands rank above global ones when their scope is active"); the
 * consumer applies that precedence rather than double-firing.
 *
 * A `chord` may be a LEADER SEQUENCE: two segments separated by one space
 * (`"G B"` = press `G` then `B`), resolved for display by
 * `formatChordForDisplay` (`"G then B"`) and split by `chordSegments`. The
 * dispatcher's leader runtime resolves the completed sequence through the same
 * tiers as a single chord. Authoring rules (enforced by a `DEFAULT_KEYMAP`
 * invariant test once the first sequence rows land): a sequence is exactly two
 * segments; every segment is
 * modifier-less (so it inherits the editable-target guard — a modified segment
 * would fire while a text field is focused); and a sequence's first segment
 * (the leader) must not also be bound as a complete single chord (the leader
 * key is reserved, which keeps the runtime's fall-through simple).
 */
export interface KeymapEntry {
	readonly chord: string;
	readonly commandId: CommandId;
	readonly when?: CommandScope;
}

/**
 * The default global keymap (D5:446-455). Every chord D5 lists appears here.
 */
export const DEFAULT_KEYMAP: readonly KeymapEntry[] = [
	// Views (D5:446-447)
	{ chord: "Mod+B", commandId: cmd("view.bridge") },
	{ chord: "Mod+Shift+A", commandId: cmd("view.agentWorkspace") },
	{ chord: "Mod+,", commandId: cmd("view.settings") },
	{ chord: "Mod+K", commandId: cmd("palette.open") },
	// Shortcuts overlay (RIG-2482). Authored as the bare `?` chord; the
	// dispatcher's eventToChord drops Shift for modifier-less printable
	// non-ASCII-letter keys, so a US `Shift+/` normalizes to `?`.
	{ chord: "?", commandId: cmd("view.shortcuts") },

	// Go-to sequences (RIG-2484). Leader "G then <letter>" destinations, all
	// unscoped/global; each sits AFTER any existing modifier row for the same
	// command so shortcutFor/shortcutForAria resolve the modifier chord first.
	{ chord: "G B", commandId: cmd("view.bridge") },
	{ chord: "G L", commandId: cmd("view.backlog") },
	{ chord: "G D", commandId: cmd("view.done") },
	{ chord: "G S", commandId: cmd("view.settings") },

	// Zones (D5:448-449)
	{ chord: "Mod+1", commandId: cmd("zone.focusLeft") },
	{ chord: "Mod+2", commandId: cmd("zone.focusMain") },
	{ chord: "Mod+3", commandId: cmd("zone.focusRight") },
	{ chord: "F6", commandId: cmd("zone.cycle") },
	{ chord: "Mod+\\", commandId: cmd("sidebar.toggleRight") },
	{ chord: "Mod+Shift+\\", commandId: cmd("sidebar.toggleLeft") },

	// Lists / tree / board (D5:450-451). Arrows move within the roving group;
	// `when` is omitted (applies inside whichever zone owns the active group).
	{ chord: "ArrowUp", commandId: cmd("list.movePrev") },
	{ chord: "ArrowDown", commandId: cmd("list.moveNext") },
	{ chord: "ArrowLeft", commandId: cmd("list.moveLeft") },
	{ chord: "ArrowRight", commandId: cmd("list.moveRight") },
	{ chord: "Enter", commandId: cmd("list.openOrSelect") },
	{ chord: "Space", commandId: cmd("list.expandOrToggle") },
	{ chord: "Home", commandId: cmd("list.moveFirst") },
	{ chord: "End", commandId: cmd("list.moveLast") },

	// Board (RIG-2130 T4). Group-relative chords the active board roving group
	// claims in the dispatcher's tier 1, ahead of the `when:"main"` comms entries
	// below: `Shift+Enter` opens the cursor card's assigned agent (winning over
	// `comms.newline`). The board's `Space` cross-link rides the existing
	// `Space → list.expandOrToggle` row above: the dispatcher resolves a chord to
	// its FIRST group-relative match (dispatch.ts:95-97, DEFAULT_KEYMAP order), so
	// the earlier Lists-block `Space` wins and the board maps THAT id to the
	// cross-link (Bridge onCommand). The `board.openCardCrossLink` command still
	// exists (palette + a future remap when a real board `Space` affordance lands,
	// OQ-2) — it just needs no keymap row of its own, which would be dead here.
	{ chord: "Shift+Enter", commandId: cmd("board.openAssignedAgent") },

	// Workspace (D5:452-453): Ctrl+Alt+Arrow moves focus between the channel
	// and trace panes.
	{ chord: "Mod+Alt+ArrowLeft", commandId: cmd("workspace.focusPaneLeft") },
	{ chord: "Mod+Alt+ArrowRight", commandId: cmd("workspace.focusPaneRight") },

	// Comms (D5:454-455). Enter send / Shift+Enter newline (kept);
	// Ctrl+Enter send-and-stay variant reserved. NOTE: `Enter` (and the
	// group-relative list chords above) is also bound unscoped in the Lists
	// block; in the main zone the scoped `comms.send` wins over the global
	// `list.openOrSelect` per the precedence rule documented on KeymapEntry.
	{ chord: "Enter", commandId: cmd("comms.send"), when: "main" },
	{ chord: "Shift+Enter", commandId: cmd("comms.newline"), when: "main" },
	{ chord: "Mod+Enter", commandId: cmd("comms.sendAndStay"), when: "main" },
];
