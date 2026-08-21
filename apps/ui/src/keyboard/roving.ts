/**
 * Roving-group primitive (RIG-2130 T2). Owns the one-tab-stop invariant over a
 * reactive, ordered stop list: exactly one stop carries `tabindex="0"` (the
 * cursor), every other stop `tabindex="-1"`, re-applied whenever the stop list
 * or the cursor changes. On a cursor move it pulls DOM focus to the cursor stop
 * and scrolls it into view. It exposes the active-group handle the keymap
 * dispatcher (dispatch.ts) routes group-relative command ids to; the group's
 * 2-D / 1-D movement semantics live in the caller's `onCommand` (the board's in
 * T3/T4), so this primitive only delegates and reports the handled boolean.
 */

import { createEffect, on } from "solid-js";
import type { CommandId } from "./commands";
import type { RovingGroup } from "./zones";

/** A single roving tab stop: a stable id paired with its rendered element. */
export type Stop = { id: string; el: HTMLElement };

/**
 * The active-group handle the dispatcher holds. `handleCommand` routes a
 * group-relative command id into the group's movement semantics and returns
 * whether the group owned the action; `isFocused` reports whether DOM focus
 * currently lives on a stop of this group (the dispatcher gates its tier-1
 * claim on this, so the board only owns Enter/Space/Arrows/… while the board
 * is the focused surface — the focus-exclusivity contract, design §401-405);
 * `focus` pulls DOM focus onto the cursor stop (Tab-entry / zone-focus landing).
 */
export interface RovingGroupHandle {
	readonly group: RovingGroup;
	handleCommand(id: CommandId): boolean;
	isFocused(): boolean;
	focus(): void;
}

/** Constructor options — all state is caller-owned and reactive. */
export interface RovingGroupOptions {
	readonly group: RovingGroup;
	readonly stops: () => Stop[];
	readonly cursor: () => string | null;
	readonly setCursor: (id: string) => void;
	readonly onCommand: (id: CommandId) => boolean;
}

/**
 * Resolve the stop the cursor should currently rest on. Prefers the stop whose
 * id === `cursor()`; if that id is absent from the list (stale cursor — the stop
 * was removed), falls back to the first remaining stop so exactly one stop stays
 * tabbable and focusable without crashing. `undefined` only when the list is
 * empty. The nearest-stop DATA fallback (next-then-previous) is the board
 * cursor model's job (T3/T4); this keeps the DOM invariant intact meanwhile.
 */
function resolveCursorStop(
	stops: Stop[],
	cursor: string | null,
): Stop | undefined {
	if (cursor !== null) {
		const exact = stops.find((s) => s.id === cursor);
		if (exact) return exact;
	}
	return stops[0];
}

/**
 * Create a roving group over caller-owned reactive state. Must run inside a
 * reactive root (a component body or `createRoot`) so its sync effect is owned
 * and disposed with the caller.
 */
export function createRovingGroup(opts: RovingGroupOptions): RovingGroupHandle {
	// Sync tabindex + focus to (stops, cursor). Static dep set → explicit `on`
	// (matches the Solid-v2 migration convention). The effect drives tabindex on
	// every run (the one-tab-stop invariant), but pulls DOM focus onto the cursor
	// ONLY when focus already lives inside the group — a genuine in-group keyboard
	// move. It must NOT focus on the mount run, nor on a stops-rebuild that
	// recomputes the cursor while the user is elsewhere: either would steal focus
	// and scroll the board on load / on a background data push (WCAG 3.2.1). The
	// group is entered by native Tab (the cursor stop is the sole `tabindex=0`) or
	// by an explicit `handle.focus()` (zone landing); once focus is in the group,
	// a cursor move refocuses so focus never strands on a now-untabbable stop.
	let lastFocusedId: string | null = null;
	createEffect(
		on([opts.stops, opts.cursor], ([stops, cursor]) => {
			const active = resolveCursorStop(stops, cursor);
			for (const stop of stops) {
				stop.el.tabIndex = stop === active ? 0 : -1;
			}
			if (!active) {
				lastFocusedId = null;
				return;
			}
			const focusInGroup = stops.some((s) => s.el === document.activeElement);
			if (focusInGroup && active.id !== lastFocusedId) {
				active.el.focus();
				active.el.scrollIntoView({ block: "nearest" });
			}
			lastFocusedId = active.id;
		}),
	);

	return {
		group: opts.group,
		handleCommand(id: CommandId): boolean {
			return opts.onCommand(id);
		},
		isFocused(): boolean {
			return opts.stops().some((s) => s.el === document.activeElement);
		},
		focus(): void {
			const active = resolveCursorStop(opts.stops(), opts.cursor());
			active?.el.focus();
		},
	};
}
