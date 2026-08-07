/**
 * Focus-zone model — the contract compass-ui implements against.
 *
 * Frozen by design record D4 (Focus model: one ring, spatial focus zones,
 * roving tabindex). CONTRACTS ONLY: interfaces, type unions, and documented
 * ordering. No runtime behavior, no DOM, no Solid components — compass-ui owns
 * the implementation.
 */

/**
 * The four interactive focus zones of the shell (D4:393-398).
 *
 * `left` (left sidebar), `main` (main view), `right` (right sidebar), and
 * `topbar`. Cycled with `Ctrl+1..3` (left/main/right) and `F6`/`Shift+F6`
 * (topbar is F6-only).
 *
 * The usage bar (`UsageBar.tsx`) is a display-only landmark, NOT a zone: it
 * carries no interactive control, is reachable by screen-reader landmark
 * navigation but not in the F6 rotation. It rejoins as a fifth zone ONLY if it
 * gains an interactive control (T6 verifies this at flip time) — at which point
 * `'usagebar'` is appended to this union.
 */
export type FocusZone = "left" | "main" | "right" | "topbar";

/**
 * A roving-tabindex group within a zone (D4:399-402).
 *
 * One tab stop per zone: exactly one element in the group is tabbable
 * (`tabindex="0"`) at a time; the rest are `tabindex="-1"`, and arrow keys move
 * the selection (and the single tab stop) within the group — tree, board grid,
 * topic list, tab strip. This keeps global Tab order short.
 */
export interface RovingGroup {
	/** The zone this roving group belongs to. */
	readonly zone: FocusZone;
	/** Stable id of the group within its zone. */
	readonly id: string;
}

/**
 * Direction of intra-group roving movement.
 */
export type RovingDirection = "prev" | "next" | "first" | "last";

/**
 * The two fixed workspace panes (D4:403-405, D6).
 *
 * The workspace is two panes — the home channel and the session trace — not an
 * arbitrary split tree. The focused pane carries `data-focused` and the accent
 * inner rule; `Ctrl+Alt+Arrow` moves focus between the two.
 */
export type WorkspacePane = "channel" | "trace";

/**
 * The escape ladder (D4:407-409).
 *
 * `Esc` unwinds transient UI state in THIS fixed order — the ORDER is the
 * contract. Each `Esc` press advances one rung if that rung is active:
 *
 *   1. `palette`            — close the command palette
 *   2. `menu-dialog`        — close an open menu or (Kobalte-managed) dialog
 *   3. `clear-selection`    — clear transient selection within the active zone
 *   4. `return-to-anchor`   — return focus to the active zone's anchor element
 *
 * Nothing traps focus except Kobalte-managed modals.
 */
export type EscapeStep =
	| "palette"
	| "menu-dialog"
	| "clear-selection"
	| "return-to-anchor";

/**
 * The escape ladder in contract order (D4:407-409). The array order IS the
 * unwind order; consumers walk it front-to-back.
 */
export const ESCAPE_LADDER: readonly EscapeStep[] = [
	"palette",
	"menu-dialog",
	"clear-selection",
	"return-to-anchor",
];

/**
 * Zone controller contract — the signatures a zone needs (D4:399-405).
 *
 * Interface ONLY. compass-ui implements the roving-tabindex rule, the
 * one-tab-stop-per-zone invariant, zone cycling, and the workspace two-pane
 * focus move. No behavior is defined here.
 */
export interface FocusZoneController {
	/** The currently active focus zone. */
	readonly activeZone: FocusZone;

	/** Make `zone` the active zone (backs `Ctrl+1..3`, `F6`/`Shift+F6`). */
	focusZone(zone: FocusZone): void;

	/**
	 * Register a roving-tabindex group within a zone. Establishes the single
	 * tab stop for that group (one tab stop per zone).
	 */
	registerRovingGroup(group: RovingGroup): void;

	/**
	 * Move selection (and the single tab stop) within the active roving group.
	 * Backs arrow / Home / End within tree, board grid, topic list, tab strip.
	 */
	moveWithinGroup(direction: RovingDirection): void;

	/** The currently focused workspace pane, or `null` when not in a pane. */
	readonly focusedPane: WorkspacePane | null;

	/**
	 * Move focus between the two workspace panes (D4:403-405). Backs
	 * `Ctrl+Alt+Arrow`; the target pane gains `data-focused` and the accent
	 * inner rule.
	 */
	focusPane(pane: WorkspacePane): void;
}
