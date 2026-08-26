/**
 * Keymap dispatcher (RIG-2130 T1). Installs ONE window `keydown` listener that
 * normalizes each event to a chord string, then resolves it through the ratified
 * three-tier model (RD-2): active roving group → zone-scoped entry → window
 * global. A matched entry whose command is unregistered, or whose group handler
 * declines (`false`), falls through to the next tier — so a chord the active
 * group does not claim (e.g. `Mod+B` while the board is focused) still reaches
 * the global `view.bridge`. The caller registers commands and supplies the
 * active-group accessor; this module registers nothing.
 */

import type { CommandId, CommandRegistry } from "./commands";
import {
	DEFAULT_KEYMAP,
	type KeymapEntry,
	leaderPrefixes,
	type Platform,
	resolveChord,
} from "./keymap";
import type { RovingGroupHandle } from "./roving";
import type { FocusZone } from "./zones";

/**
 * Normalize a `KeyboardEvent` to a concrete-modifier chord string comparable
 * with `resolveChord(entry.chord, platform)`. Modifier order is fixed —
 * `Mod+Shift+Alt+Key` — with `Mod` rendered as the platform's concrete modifier
 * (`Cmd` on macOS via `metaKey`, `Ctrl` elsewhere via `ctrlKey`). Space's raw
 * `event.key` (`" "`) becomes `Space`; a single-character key is upper-cased so
 * `b` matches the keymap's `B` (and `,`/`\` pass through unchanged).
 *
 * Shift-drop (RIG-2482): when NO command modifier is held and the key is a
 * single printable non-ASCII-letter character (e.g. `?`, which IS shifted `/`),
 * the `Shift` part is omitted — the character already encodes the shift, so `?`
 * is authored as the bare `"?"` chord rather than the layout-fragile `"Shift+?"`.
 * Space is deliberately NOT dropped: the `" "` → `"Space"` rename runs BEFORE
 * this predicate, so `Space` is multi-char and fails it — otherwise `Shift+Space`
 * (today a dead chord) would silently become `Space` and fire
 * `list.expandOrToggle` (keymap.ts) with the board focused. `/[a-z]/i` is
 * ASCII-only, so a future non-Latin letter binding would be Shift-dropped
 * inconsistently with `Shift+B` — flagged here for the next binding author.
 */
export function eventToChord(event: KeyboardEvent, platform: Platform): string {
	const parts: string[] = [];
	const hasCommandModifier = event.metaKey || event.ctrlKey || event.altKey;
	if (platform === "mac" ? event.metaKey : event.ctrlKey) {
		parts.push(platform === "mac" ? "Cmd" : "Ctrl");
	}
	// Rename Space BEFORE the Shift-drop predicate so it is multi-char and never
	// Shift-dropped (see the doc comment's Space carve-out).
	let key = event.key;
	if (key === " ") key = "Space";
	else if (key.length === 1) key = key.toUpperCase();
	const shiftEncodedByKey =
		!hasCommandModifier && key.length === 1 && !/[a-z]/i.test(key);
	if (event.shiftKey && !shiftEncodedByKey) parts.push("Shift");
	if (event.altKey) parts.push("Alt");
	parts.push(key);
	return parts.join("+");
}

/**
 * A group-relative command id — the Lists block (`list.*`) whose action the
 * active roving group owns, plus the board's own chords (`board.*`, wired in
 * Wave 2). These are the ids the dispatcher routes to `handleCommand`; every
 * other id (`view.*`, `zone.*`, `comms.*`, …) is resolved against the registry.
 */
function isGroupRelative(id: CommandId): boolean {
	return id.startsWith("list.") || id.startsWith("board.");
}

/**
 * Whether an event target owns its own text keys (comms composer, native
 * `<select>` typeahead, or an ARIA combobox/listbox/menu widget). A bare letter
 * on any of these must reach the widget, never arm a leader or fire a chord —
 * arming unconditionally `preventDefault`s (§A3 step 4), which would otherwise
 * steal a `<select>`'s typeahead or the palette listbox's search keys (§A1).
 */
function isEditableTarget(target: EventTarget | null): boolean {
	return (
		target instanceof HTMLInputElement ||
		target instanceof HTMLTextAreaElement ||
		target instanceof HTMLSelectElement ||
		(target instanceof HTMLElement &&
			(target.isContentEditable ||
				target.closest('[role="combobox"],[role="listbox"],[role="menu"]') !==
					null))
	);
}

/**
 * Detect the running platform from `navigator` (RIG-2482). Hoisted from the
 * former inline predicate in `installKeymap` so `resolveChord` consumers (the
 * shortcuts overlay) share the ONE platform-detection convention. Exported here
 * rather than from keymap.ts to keep keymap.ts a pure data/string module with no
 * `navigator` read (Decision 10).
 */
export function detectPlatform(): Platform {
	return /mac/i.test(navigator.platform || navigator.userAgent)
		? "mac"
		: "other";
}

/**
 * A pending leader sequence: the resolved first-segment chord (`"G"`) plus the
 * live disarm timer handle. `null` when no sequence is armed. Per-install
 * closure state — no module-level global (matches App.tsx's install/onCleanup
 * discipline).
 */
type PendingLeader = { leader: string; timer: number } | null;

/**
 * How long an armed leader waits for its completion key before self-disarming
 * (§A3, OQ1 ratified at 1000 ms). Exported so the runtime and its tests share
 * the one value.
 */
export const LEADER_TIMEOUT_MS = 1000;

/**
 * Install the global keymap. Adds one `keydown` listener and returns the
 * uninstaller (removes exactly that listener). `active` yields the focused
 * roving group (or `null`); `activeZone` yields the focused zone for the scoped
 * tier and defaults to `() => null` (no live zone controller in this wave, so
 * tier 2 is dormant until one exists — kept correct and testable via the stub).
 *
 * D5 safety invariant (RIG-2529): the tier-3 scope gate is safe today because
 * `activeZone` is derived from `active` (spine.ts:95-97) — a non-null zone
 * implies a roving group holds focus, so tier 1 already saw any group-relative
 * chord and only a decline reaches the scope-gated tier 3 (harmless: a
 * group-relative command's `run` mirrors its handler). A future independent
 * zone controller that can make `activeZone()` non-null while NO roving group
 * is focused inherits the obligation to re-establish tier-3 safety for
 * zone-scoped commands, since that decline chain no longer holds.
 */
export function installKeymap(
	registry: CommandRegistry,
	active: () => RovingGroupHandle | null,
	activeZone: () => FocusZone | null = () => null,
): () => void {
	const platform: Platform = detectPlatform();
	// The leader-prefix set is table-derived and platform-fixed, so compute it
	// once at install rather than per keydown.
	const leaders = leaderPrefixes(DEFAULT_KEYMAP, platform);

	// Per-install pending-leader state (§A3). `null` unless a leader is armed.
	let pending: PendingLeader = null;
	const disarm = (): void => {
		if (pending) {
			clearTimeout(pending.timer);
			pending = null;
		}
	};

	// Resolve a set of matching rows through the ratified three tiers (active
	// group → scoped → global). Factored so a single chord and a completed
	// leader sequence run byte-identical resolution — the completion path passes
	// the sequence-matched rows, the single-chord path passes the single-chord
	// rows, and both flow through here.
	const resolve = (
		matching: readonly KeymapEntry[],
		event: KeyboardEvent,
	): void => {
		// Tier 1 — active group. Route a group-relative chord to the group; a
		// `true` return handles it (and suppresses native activation), a `false`
		// (declines) falls through to the next tier.
		const group = active();
		if (group) {
			const groupEntry = matching.find((entry) =>
				isGroupRelative(entry.commandId),
			);
			if (groupEntry && group.handleCommand(groupEntry.commandId)) {
				event.preventDefault();
				event.stopPropagation();
				return;
			}
		}

		// Tier 2 — scoped. A `when`-scoped entry whose zone is active wins over a
		// window-global one. An unregistered scoped command falls through.
		const zone = activeZone();
		if (zone) {
			const scopedEntry = matching.find((entry) => entry.when === zone);
			if (scopedEntry) {
				const command = registry.get(scopedEntry.commandId);
				if (command) {
					command.run();
					event.preventDefault();
					return;
				}
			}
		}

		// Tier 3 — global, scope-gated (RIG-2529). A window-global unscoped
		// entry fires anywhere its COMMAND's scope allows: a `scope:'global'`
		// command runs from any target; a zone-scoped command runs only while
		// its zone is active. An unregistered global command does not swallow
		// the event, and neither does a scoped command whose zone is inactive —
		// both fall out without preventDefault, so native activation survives.
		const globalEntry = matching.find((entry) => entry.when === undefined);
		if (globalEntry) {
			const command = registry.get(globalEntry.commandId);
			if (command && (command.scope === "global" || command.scope === zone)) {
				command.run();
				event.preventDefault();
			}
		}
	};

	const handler = (event: KeyboardEvent): void => {
		// Step 1 — normalize.
		const chord = eventToChord(event, platform);

		// Step 2 — editable-target guard FIRST, before any leader logic and
		// before the empty-matching return, but only for modifier-less keys. A
		// bare key in a text field / native <select> / ARIA widget must type,
		// never arm, complete, or fire. Mod/Ctrl/Alt chords are global and stay
		// unguarded (and, via the completion fall-through below, still disarm).
		const hasCommandModifier = event.metaKey || event.ctrlKey || event.altKey;
		if (!hasCommandModifier && isEditableTarget(event.target)) return;

		// Step 3 — completion. A leader is pending.
		if (pending) {
			// A pure-modifier keydown (holding Shift/Control/Alt/Meta mid-sequence
			// is human) neither completes nor disarms.
			if (
				event.key === "Shift" ||
				event.key === "Control" ||
				event.key === "Alt" ||
				event.key === "Meta"
			) {
				return;
			}
			// Escape disarms and is consumed.
			if (event.key === "Escape") {
				disarm();
				event.preventDefault();
				return;
			}
			// Otherwise resolve the two-segment chord "<leader> <completionChord>"
			// where the completion segment is the FULL normalized chord. Disarm
			// first (the sequence is spent either way).
			const { leader } = pending;
			disarm();
			const sequence = `${leader} ${chord}`;
			const seqMatching = DEFAULT_KEYMAP.filter(
				(entry) => resolveChord(entry.chord, platform) === sequence,
			);
			if (seqMatching.length > 0) {
				resolve(seqMatching, event);
				return;
			}
			// No row matches: fall through and process this key as a plain single
			// chord in the same keydown, RE-ENTERING the arming step first (so a
			// repeated leader re-arms and any other key resolves on its own).
		}

		// Step 4 — arming. No leader pending (or the sequence fell through): if
		// the normalized chord is a table-derived leader prefix (which, per the
		// A2 authoring rule, has no single-chord row of its own) and the key is
		// not an auto-repeat, arm and consume. Modifier chords never arm; the
		// editable guard already returned for interactive targets.
		if (!hasCommandModifier && !event.repeat && leaders.has(chord)) {
			pending = {
				leader: chord,
				timer: setTimeout(disarm, LEADER_TIMEOUT_MS) as unknown as number,
			};
			event.preventDefault();
			event.stopPropagation();
			return;
		}

		// Step 5 — single-chord path (unchanged): the empty-matching return and
		// the three tiers.
		const matching = DEFAULT_KEYMAP.filter(
			(entry) => resolveChord(entry.chord, platform) === chord,
		);
		if (matching.length === 0) return;
		resolve(matching, event);
	};

	window.addEventListener("keydown", handler);
	return () => {
		window.removeEventListener("keydown", handler);
		// Clear any live disarm timer so an uninstall (test teardown / HMR) never
		// leaks a timer that fires against a torn-down closure.
		disarm();
	};
}
