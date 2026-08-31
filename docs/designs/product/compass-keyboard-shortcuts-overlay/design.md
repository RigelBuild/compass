# Design: Searchable `?` Keyboard-Shortcuts Overlay (RIG-2482)

Status: Draft

## Problem / Intent

Compass ships a Linear-shaped keyboard ENGINE — a typed `DEFAULT_KEYMAP` table
(`apps/ui/src/keyboard/keymap.ts:62-113`) resolved by one App-root dispatcher
(`apps/ui/src/keyboard/dispatch.ts:65-134`, installed in
`apps/ui/src/App.tsx:48-54`) — but NO discoverability layer: a repo-wide grep
for a shortcuts overlay / `?` help surface returns nothing, so every chord is
invisible to anyone who doesn't already know it. Linear's fix (changelog
2021-03-25) was a searchable shortcuts help screen opened by `?` from anywhere.
This record designs that surface for Compass: a `?`-opened, searchable overlay
**generated from `DEFAULT_KEYMAP` joined against the live `CommandRegistry`**,
so it can never drift from the real bindings. Sibling non-goal: the Cmd+K
command palette is RIG-2483 (do-an-action surface); this overlay is the
exhaustive reference sheet.
The sheet's `list.*` completeness rests on the tier-3 scope gate and
`list.*` registration owned by **RIG-2529**
(`docs/designs/meta/compass-tier3-scope-gate/design.md`), an external
frozen dependency of this record — cited the same way this record cites the
merged RIG-2456 spine (see D8, T2b).

## Global Constraints

- **SolidJS ^1.9.13 (v1)** — `apps/ui/package.json:24` `"solid-js": "^1.9.13"`.
  Design against v1 with the v1-legal forward idioms (RIG-2187 "no second
  convention"): `createMemo`, `<For>` not `<Index>`, `createEffect(on(deps,
  apply))`. Vite + TS.
- **Biome 2.5.4 pinned** — relative imports sorted alphabetically.
- **Night Owl `--cx-*` semantic token tier only**
  (`apps/ui/src/design/tokens.css`); never the dead legacy tier.
  Reduced-motion is honored via token — `tokens.css:241-248` zeroes
  `--cx-motion-fast`/`--cx-motion-base` under
  `@media (prefers-reduced-motion: reduce)`, so any transition that consumes
  the motion tokens is automatically compliant.
- **Command ids** `noun.verbCamel`; board-scoped `board.*`.
- **One keydown path** — the `?` chord registers a `Command` into the shared
  `store.keyboard.registry` and rides the existing App-root dispatcher
  (`App.tsx:48-54`). NEVER a second window keydown listener.
- **Tests** run `cd apps/ui && bun test --conditions browser <file>`;
  pure-module `MOON_TOOLCHAIN_FORCE_GLOBALS=true bun test --conditions browser
  src/<file>.test.ts`. Every new global-chord contract needs a
  production-wiring test (mount the real App via `mountApp`
  (`apps/ui/src/test-router.tsx:35-56`), press the chord) — never a test that
  self-registers the command under test.
- **Generated, never hand-maintained** — the overlay's content is a render-time
  join of `DEFAULT_KEYMAP` × `CommandRegistry`. No hand-written shortcut list
  anywhere in the overlay.

## Approach

### 1. The `?` global command: `view.shortcuts`

**Registration site — the spine, next to `view.bridge`.** The spine's own
contract is that a view-level command's registration lives with its behavior:

> Create the keyboard spine. Registers `view.bridge → deps.showBridge()` as
> its first command before returning (record A4 §182-186: the registration
> lives with the behavior — the spine is created in `createAppStore` where
> `showBridge` is in scope) — `apps/ui/src/keyboard/spine.ts:59-64`

The store already builds the spine after its navigation closures exist:

> `const keyboard = createKeyboardSpine({ showBridge });` —
> `apps/ui/src/store.ts:1896`

We extend the same seam: the store owns a `shortcutsOpen` signal plus
`showShortcuts`/`hideShortcuts`/`toggleShortcuts` closures, and
`createKeyboardSpine` grows a second dep so it registers `view.shortcuts →
deps.toggleShortcuts()` alongside `view.bridge` (`spine.ts:70-77` is the
pattern to mirror: `id`, `title: "Keyboard shortcuts"`, `keywords: ["help",
"shortcuts", "keys", "keymap"]`, `scope: "global"`). App.tsx renders the
overlay from `store.shortcutsOpen()` — command and surface both hang off the
one store, no new provider.

**Keymap row + chord normalization.** `?` is `Shift+/` on US-like layouts, so
the dispatcher's normalizer today would produce `"Shift+?"`:

> `if (event.shiftKey) parts.push("Shift"); ... else if (key.length === 1) key
> = key.toUpperCase();` — `apps/ui/src/keyboard/dispatch.ts:30-34`

Rather than authoring the layout-fragile `"Shift+?"`, `eventToChord` gains one
narrow normalization: **when no command modifier is held (`!metaKey &&
!ctrlKey && !altKey`) and the key is a single printable non-ASCII-letter
character, EXCLUDING space, drop the `Shift` part** — the character already
encodes the shift (`?` IS shifted `/`). Space must be carved out — either
exclude `" "` explicitly, or apply the existing `key === " "` → `"Space"`
rename (`dispatch.ts:25-37`, which today runs AFTER the parts array is built)
BEFORE the predicate so Space is multi-char: `" "` is a single char failing
`/[a-z]/i`, so without the carve-out `Shift+Space` — today a dead chord —
would silently become `Space` and fire `list.expandOrToggle`
(`keymap.ts:84`) with the board focused. Note `/[a-z]/i` is ASCII-only: a
future non-Latin letter binding would be Shift-dropped inconsistently with
`Shift+B` (called out in T1a for the next binding author). The keymap row is
then the layout-portable `{ chord: "?", commandId: cmd("view.shortcuts") }`
appended to the Views block (`keymap.ts:63-67`). The normalization is
deliberately scoped to modifier-less chords so existing bindings like
`Mod+Shift+\` (`keymap.ts:75`) keep their current behavior untouched.

**Editable-target guard — already correct, by construction.** `?` carries no
command modifier, so the dispatcher's existing guard suppresses it while focus
is in the comms composer or any text input:

> `const hasCommandModifier = event.metaKey || event.ctrlKey || event.altKey;
> if (!hasCommandModifier && isEditableTarget(event.target)) return;` —
> `apps/ui/src/keyboard/dispatch.ts:87-88`

This also means typing `?` inside the overlay's own search `<input>` cannot
re-trigger the command — the input is an `HTMLInputElement`
(`dispatch.ts:50-56`). No new guard code is needed.

**Toggle + Escape.** `view.shortcuts` toggles (a second `?` outside the input
closes). Escape closes via a local `onKeyDown` on the dialog root, mirroring
the existing hand-rolled dialog (`apps/ui/src/components/StartAgentDialog.tsx:34-36`);
this is consistent with the escape ladder's rung 2 `menu-dialog`
(`apps/ui/src/keyboard/zones.ts:60-71`) — no dispatcher `Escape` row exists,
so there is no conflict.

### 2. The generated content model

A pure module `apps/ui/src/keyboard/shortcuts-model.ts` computes the overlay's
rows; the component only renders. Signature (exact, for the executor):

```ts
export interface ShortcutRow {
  readonly chord: string;      // platform-resolved via resolveChord
  readonly title: string;      // command.title
  readonly commandId: CommandId;
}
export interface ShortcutGroup {
  readonly scope: CommandScope;  // "global" | FocusZone (zones.ts:23)
  readonly rows: ShortcutRow[];
}
export function buildShortcutGroups(
  keymap: readonly KeymapEntry[],
  registry: CommandRegistry,
  platform: Platform,
  query: string,
): ShortcutGroup[];
```

Join semantics:

- **Iterate `DEFAULT_KEYMAP`, resolve each `entry.commandId` through
  `registry.get()`** (`apps/ui/src/keyboard/commands.ts:110` `get(id:
  CommandId): Command | undefined`). The keymap is the chord truth; the
  registry is the behavior + title truth — exactly the split the `Command`
  contract documents: *"`shortcut` is the display chip (the authoritative
  chord lives in the keymap table, see keymap.ts)"* (`commands.ts:33-34`).
- **A keymap row with no registered command is OMITTED.** The dispatcher
  treats an unregistered command as dead (falls through, `dispatch.ts:122-129`),
  so listing it would advertise a chord that does nothing. Today that
  correctly hides e.g. `Mod+K → palette.open` (`keymap.ts:67`) until RIG-2483
  registers it — and the row appears automatically the moment it does.
- **A registered command with no keymap row is OMITTED** (e.g.
  `board.openCardCrossLink`, registered at
  `apps/ui/src/components/Bridge.tsx:440-446` with deliberately no keymap row,
  `keymap.ts:95-97`). It has no shortcut to teach;
  it belongs to the palette's inventory, not this sheet.
- **Group by `command.scope`** (`commands.ts:40`, `CommandScope = FocusZone |
  "global"`, `commands.ts:18`): `global` first, then zones in the fixed order
  `left, main, right, topbar` (`zones.ts:23`). Note the grouping axis is
  `command.scope`, NOT the keymap `when` field — a `when`-absent keymap row is
  not "global"; its group is its resolved command's declared scope. So a chord
  that appears on two keymap rows whose commands share a scope lands twice in
  the SAME group: `Enter` resolves to `list.openOrSelect` (`scope:"main"` under
  RIG-2529, `keymap.ts:83`, `when` absent) and to `comms.send` (`scope:"main"`,
  `keymap.ts:110`, `when:"main"`) — both `main`, so both rows sit side by side
  in the `main` group. Showing both is truthful (the dispatcher's precedence
  note `keymap.ts:48-51` is scope-local), and the adjacency is exactly what
  OQ1's deferred "wins while composing" annotation would later disambiguate.
- **Chord rendering via `resolveChord(entry.chord, platform)`**
  (`keymap.ts:37-38`: `Mod` → `Cmd` on mac, `Ctrl` elsewhere). The platform is
  detected ONCE by the same predicate the dispatcher uses
  (`dispatch.ts:70-74`); to avoid a second convention that predicate is
  hoisted as `export function detectPlatform(): Platform` in `dispatch.ts` —
  the module where it already lives — and `installKeymap` flips to consuming
  it (pure refactor, behavior identical). Exporting from `dispatch.ts` rather
  than `keymap.ts` keeps `keymap.ts` a pure data/string module with no
  `navigator` read (Decision 10).
- **Registry snapshot at open.** The registry is a plain Map, not reactive
  (`apps/ui/src/keyboard/registry.ts:18-40`); the join runs in a `createMemo`
  keyed on the open signal + query, so every open recomputes against the live
  registry (a command registered while the overlay is already open shows on
  next open — acceptable for a reference sheet, noted in Decisions).
- **Launch inventory: strict join over the RIG-2529-widened registry.**
  Production on main registers only `view.bridge` (`spine.ts:70-77`) and,
  while the Bridge is mounted, `board.openAssignedAgent`/
  `board.openCardCrossLink` (`Bridge.tsx:433-446`). The `list.*` chords are
  handled WITHOUT registry entries — the board's roving group maps them in
  its tier-1 `onCommand` (`Bridge.tsx:360-410`), and the dispatcher routes
  group-relative ids to the group before ever consulting the registry
  (`dispatch.ts:93-103`) — so a strict join over main alone would hide the
  whole arrows/Enter/Space/Home/End block. Matt ruled (2026-08-23) the
  principled end state: the eight `list.*` commands ARE registered
  (`scope: "main"`) and tier 3 becomes scope-aware, closing the RIG-2130
  RD-4/OQ-6 focus hole. That is a shared-dispatcher change and is OWNED BY
  ITS OWN RECORD, **RIG-2529**
  (`docs/designs/meta/compass-tier3-scope-gate/design.md`), which this
  record depends on as external frozen substrate — the same way it depends
  on the merged RIG-2456 spine. With RIG-2529 merged, the strict join (D3,
  unchanged) surfaces `?`, `Mod+B`, `Shift+Enter`, AND the `list.*` block.
  See T2b and D8.

### 3. Search semantics

**Case-insensitive substring** over `command.title`, `command.keywords`
(`commands.ts:39`, *"`keywords` broadens fuzzy matching in the palette"*,
`commands.ts:32-33`), and the resolved chord string. Grep confirms **no fuzzy
helper exists in the repo** — the only "fuzzy" hits are prose in
`components.md:421/427`, `commands.ts:33/79` (palette contract, explicitly
deferred: *"the exact weighting ... is an NLB deferral"*, `commands.ts:80-81`)
and a stub-data issue title. Fuzzy matching is the palette's concern
(RIG-2483); for an exhaustive sheet of ~20 rows, substring is Linear-parity
and avoids inventing a ranking convention this record would then impose on the
sibling. An empty query shows everything; a query with no matches shows a dim
empty row.

### 4. The surface

A modal dialog on the `.cx-dialog` convention:

> **Class:** `.cx-dialog`, with `.cx-dialog-backdrop` (scrim) — `apps/ui/src/design/components.md:404`

with the styles already shipped in `apps/ui/src/design/components/menu.css:69-90`
(elev-3 float, `--cx-scrim` backdrop, `--cx-z-modal`). Structure:

- **Host component** `apps/ui/src/components/ShortcutsOverlay.tsx`, mounted in
  `App.tsx` under `<Show when={store.shortcutsOpen()}>` — shell-level chrome,
  like the sidebars. `App.tsx` adds `import "./design/components/menu.css"` (it
  currently imports only `badge-glyph.css`/`card.css`, `App.tsx:6-7`).
- **Hand-rolled dialog, not Kobalte — with unconditional trap + restore.**
  `components.md:405-406` says the focus-trap is Kobalte, but `@kobalte/core`
  is NOT in `apps/ui/package.json:10-26` and the one shipped dialog is
  hand-rolled (`StartAgentDialog.tsx:26-36`: `role="dialog"`,
  `aria-modal="true"`, `tabindex={-1}`, self-focus `onMount`, local Escape).
  That precedent is INITIAL-FOCUS-ONLY — no Tab trap, no focus-restore
  (`StartAgentDialog.tsx:17-37`) — so this record follows its shape but adds
  both as unconditional correctness in T4: a modal Tab can walk out of loses
  its local Escape handler (the sheet becomes keyboard-uncloseable) and lies
  about `aria-modal="true"`. Kobalte sequencing is RATIFIED (Matt,
  2026-08-23): hand-roll the `.cx-dialog` NOW — no `@kobalte/core` in this
  record — and migrate to a Kobalte Dialog later if ever (D5). Initial
  focus goes to the search input (it is the overlay's one interaction).
- **Anatomy:** `.cx-dialog` panel (centered, ~480-560px, max-height with a
  scrolling row region) containing a `.cx-search` query input
  (`apps/ui/src/design/components/input.css:5-33`), then `<For>` over groups:
  a dim uppercase scope header (`--cx-text-faint`, mirroring the palette's
  group-header treatment, `components.md:436-437`), then rows of
  `title (--cx-text)` left + right-aligned chord chip (`--cx-text-dim`; the UI
  face is already mono — `--cx-font-ui: var(--rigel-mono)`,
  `tokens.css:195`). New CSS lives in
  `apps/ui/src/design/components/shortcuts.css` consuming only `--cx-*`
  tokens; any transition uses `--cx-motion-fast`/`--cx-ease-out` so
  reduced-motion zeroes it (`tokens.css:241-248`).
- The overlay lists itself (`? — Keyboard shortcuts` in the Global group) —
  free, since it is generated.
- **Close on navigation.** The store's navigation closures also call
  `hideShortcuts()` (T2, Decision 9): the snapshot-at-open sheet can never
  keep advertising commands a route change retracted, and no modal floats
  over a new route.

### 5. Test strategy

Layered: pure-module tests on `buildShortcutGroups` (custom keymap + registry
fixtures — the ONLY place hand-built data is legitimate, because the unit IS
the join), then **production-wiring tests** that mount the real App via
`mountApp` (`test-router.tsx:35-56`) and press real keys, mirroring
`apps/ui/src/keyboard-e2e.test.tsx:37-76`. The production tests register
NOTHING for the open path (`view.shortcuts` must come from the spine) and
prove the generated-content contract on REAL registrations: the Bridge
registers `board.openAssignedAgent` on mount (`Bridge.tsx:433-439`) and its
keymap row exists (`keymap.ts:98`), so the overlay must show that row with no
test-side registration — and must NOT show `palette.open` (tabled at
`keymap.ts:67`, registered nowhere on main). A registry-mutation test proves
drift-immunity end to end: register a command for an existing tabled id
through `store.keyboard.registry` (the public seam, as
`keyboard-e2e.test.tsx:90` does), reopen, assert the row appeared.

## Plan

### T1a — `eventToChord` Shift-drop normalization (hot-path dispatcher change)

Make `?` matchable with zero behavior change to any existing chord —
including `Shift+Space`, which the naive predicate would silently rebind.

- In `apps/ui/src/keyboard/dispatch.ts` `eventToChord` (`dispatch.ts:25-37`):
  when no command modifier is held (`!event.metaKey && !event.ctrlKey &&
  !event.altKey`) and `event.key` is a single printable non-ASCII-letter
  character EXCLUDING space, do not push `Shift` — the character already
  encodes it. Implementation: apply the existing `key === " "` → `"Space"`
  mapping (`dispatch.ts:25-37` — today it runs AFTER the parts array is
  built) BEFORE the predicate so `Space` is multi-char and never
  Shift-dropped, or carve `" "` out explicitly. Without the carve-out,
  `Shift+Space` — today a dead chord that matches no row — would become
  `Space` and fire `list.expandOrToggle` (`keymap.ts:84`) with the board
  focused. Letters keep today's `Shift+`+uppercase shape; note `/[a-z]/i` is
  ASCII-only, so a future non-Latin letter binding would be Shift-dropped
  inconsistently with `Shift+B` — accepted for now, flagged for the next
  binding author. All modifier-carrying chords (`Mod+Shift+\`,
  `Mod+Shift+A`) are untouched; `Shift+Enter` (multi-char key) is untouched.

Interfaces:

- consumes: `KeyboardEvent`, `Platform` (`keymap.ts:31`).
- produces: `eventToChord(event: KeyboardEvent, platform: Platform): string`
  (unchanged signature, refined semantics).

Test cycle: extend `apps/ui/src/keyboard/dispatch.test.ts` — red first:
`eventToChord(new KeyboardEvent("keydown", { key: "?", shiftKey: true }),
"other")` must equal `"?"`; red-first regression row `Shift+Space` →
`"Shift+Space"` (key `" "`, `shiftKey: true` — the space carve-out case);
regression rows for `Shift+B` → `"Shift+B"`, `Mod+Shift+\` and `Shift+Enter`
unchanged. Run `cd apps/ui && bun test --conditions browser
src/keyboard/dispatch.test.ts`.

### T1b — `detectPlatform` export + `?` keymap row (zero-risk refactors)

Split from T1a so the hot-path normalizer diff gets undiluted review.

- In `apps/ui/src/keyboard/dispatch.ts`: hoist the platform predicate
  (`dispatch.ts:70-74`, `/mac/i.test(navigator.platform ||
  navigator.userAgent) ? "mac" : "other"`) into
  `export function detectPlatform(): Platform`; flip `installKeymap` to call
  it. Exported from `dispatch.ts`, NOT `keymap.ts`, so `keymap.ts` stays a
  pure data/string module with no `navigator` read (Decision 10).
- In `apps/ui/src/keyboard/keymap.ts`: add the keymap row
  `{ chord: "?", commandId: cmd("view.shortcuts") }` to the Views block after
  `keymap.ts:67`, with a comment naming this record.

Interfaces:

- consumes: `Platform` (`keymap.ts:31`), `navigator`.
- produces: `detectPlatform(): Platform` (new export in dispatch.ts); one new
  `KeymapEntry` row.

Test cycle: extend `dispatch.test.ts` — `detectPlatform()` under both
navigator stubs (`dispatch.test.ts:49-52` technique). Run the same dispatch
suite (the keymap row is data an e2e case covers in T5; no `keymap.test.ts`
exists today).

### T2 — Store signal + spine registration of `view.shortcuts`

- `apps/ui/src/store.ts`: a `[shortcutsOpen, setShortcutsOpen]` boolean signal;
  closures `showShortcuts`/`hideShortcuts`/`toggleShortcuts`; expose
  `shortcutsOpen`, `hideShortcuts`, `toggleShortcuts` on the store value.
  The navigation closures (`showBridge` today; siblings as they land) each
  additionally call `hideShortcuts()` — close-on-navigation, one line each
  (Decision 9). Extend the spine construction (`store.ts:1896`) to
  `createKeyboardSpine({ showBridge, toggleShortcuts })`.
- `apps/ui/src/keyboard/spine.ts`: widen `createKeyboardSpine`'s deps to
  `{ showBridge: () => void; toggleShortcuts: () => void }` and register the
  second command next to `view.bridge` (`spine.ts:70-77` pattern):
  `{ id: "view.shortcuts", title: "Keyboard shortcuts", keywords: ["help",
  "shortcuts", "keys", "keymap"], scope: "global", run: () =>
  deps.toggleShortcuts() }`.

Interfaces:

- consumes: `createCommandRegistry` (`registry.ts:18`), store signal
  primitives.
- produces: `createKeyboardSpine(deps: { showBridge: () => void;
  toggleShortcuts: () => void }): KeyboardSpine`; store members
  `shortcutsOpen: Accessor<boolean>`, `toggleShortcuts(): void`,
  `hideShortcuts(): void`.

Test cycle: extend `apps/ui/src/keyboard/spine.test.ts` (mirror its
`view.bridge` assertions, `spine.test.ts:37-43`): `view.shortcuts` is
registered, `scope === "global"`, `run()` invokes the dep. Store-level: a
toggle flips `shortcutsOpen()`; `toggleShortcuts()` then `showBridge()` →
`shortcutsOpen()` is false (close-on-navigation, Decision 9). Run
`bun test --conditions browser src/keyboard/spine.test.ts`.

### T2b — `list.*` rows via the RIG-2529 scope gate (stacks on RIG-2529's merge)

**Ratified (Matt, 2026-08-23): the long-term end state.** The eight
`list.*` commands get registered and tier 3 becomes scope-aware — but that
change is a shared-dispatcher concern OWNED BY **RIG-2529**
(`docs/designs/meta/compass-tier3-scope-gate/design.md`), an external
frozen dependency of this record, exactly as the merged RIG-2456 spine is.
RIG-2529 specifies: tier 3 (`dispatch.ts:122-129`) runs a matched global
entry's command only if `command.scope === "global"` or it matches the
active zone (`Command.scope`, `commands.ts:18,40`, already exists); the
eight `list.*` ids register alongside the existing `board.*` registrations
(`Bridge.tsx:433-450` pattern), each `scope: "main"`, `run: () =>
onCommand(id)`, retracted `onCleanup` — safe under the gate. RIG-2529 also
flips the regression guard that today asserts the tier-3 hole stays open
(`Bridge.test.tsx:537-572`, assertion at `:568-571`) and thereby closes
RIG-2130 RD-4/OQ-6.

This record's task is therefore pure consumption: the strict keymap ×
registry join (T3, D3 — unchanged) surfaces the eight `list.*` rows the
moment RIG-2529's registrations exist. No overlay-side dispatch or
registration code. **The overlay impl PR STACKS ON RIG-2529's MERGE.**

Interfaces:

- consumes: the RIG-2529-merged registry state (eight `Command`
  registrations `list.movePrev/moveNext/moveLeft/moveRight/moveFirst/
  moveLast/openOrSelect/expandOrToggle`, `scope: "main"`,
  Bridge-lifecycle-bound), read via `registry.get()` inside
  `buildShortcutGroups` (T3) — no new code surface in this record.
- produces: the `main` group's `list.*` rows in the rendered overlay,
  verified by T5 cases 5-6.

Test cycle: the registration-inventory and tier-3 leak non-regression
proofs (Bridge mounted, focus a non-board non-editable element, press
Enter → no list command runs, no `preventDefault`) are primarily
RIG-2529's to carry red-first; this record keeps T5 case 6 as a
belt-and-braces consumer check and adds T5 case 5 (list rows follow board
lifecycle). Run `bun test --conditions browser src/keyboard-e2e.test.tsx`
on a branch based on RIG-2529's merge.

### T3 — `buildShortcutGroups` pure model

New pure module `apps/ui/src/keyboard/shortcuts-model.ts` implementing the join
of Approach §2 and the substring filter of §3 (match over lowercased
`title`, each `keywords` entry, and the resolved chord; empty query passes
all). Groups ordered `global, left, main, right, topbar`; within a group,
keymap order. Rows whose commandId has no registration, and commands with no
keymap row, are omitted.

Interfaces:

- consumes: `KeymapEntry`/`resolveChord`/`Platform` (`keymap.ts:37-57`),
  `CommandRegistry`/`CommandScope` (`commands.ts:18,108-116`).
- produces: `buildShortcutGroups(keymap: readonly KeymapEntry[], registry:
  CommandRegistry, platform: Platform, query: string): ShortcutGroup[]` with
  `ShortcutRow { chord; title; commandId }`, `ShortcutGroup { scope; rows }`
  (exact shapes in Approach §2).

Test cycle: new `apps/ui/src/keyboard/shortcuts-model.test.ts` (fixture keymap +
`createCommandRegistry()` — hand-built data is correct HERE, the unit is the
join): unregistered-id omission, keymap-less-command omission, scope grouping
and order, `Mod` resolution per platform, substring on title/keyword/chord,
case-insensitivity, empty query. Run
`MOON_TOOLCHAIN_FORCE_GLOBALS=true bun test --conditions browser
src/keyboard/shortcuts-model.test.ts`.

### T4 — `ShortcutsOverlay` component + `shortcuts.css` + App mount

- `apps/ui/src/components/ShortcutsOverlay.tsx`: hand-rolled modal per
  `StartAgentDialog.tsx:26-36` precedent — backdrop div
  `.cx-dialog-backdrop`, panel `.cx-dialog.cx-shortcuts` with `role="dialog"`,
  `aria-modal="true"`, `aria-label="Keyboard shortcuts"`; `onMount` focuses
  the `.cx-search` input; local `onKeyDown` Escape → `store.hideShortcuts()`;
  backdrop click closes. Body: `createMemo(() =>
  buildShortcutGroups(DEFAULT_KEYMAP, store.keyboard.registry,
  detectPlatform(), query()))`, rendered with `<For>` over groups then rows;
  dim uppercase scope headers; right-aligned mono chord chips; dim empty row
  when no matches.
- **Unconditional dialog correctness** (beyond the `StartAgentDialog`
  precedent, which is initial-focus-only — no trap, no restore,
  `StartAgentDialog.tsx:17-37`): (1) **focus-restore** — capture
  `document.activeElement` in `onMount` (before focusing the search input)
  and restore it in `onCleanup`, so it holds for every close path (`?`
  toggle, Escape, backdrop, close-on-navigation); (2) **a minimal focus
  trap** — a local `Tab`/`Shift+Tab` handler wrapping between the dialog's
  first and last focusable elements (~15 lines), so focus cannot walk out
  behind `aria-modal="true"` and the local Escape handler stays reachable.
  Both are unconditional correctness, not a Kobalte question (the dialog
  host is ratified as hand-rolled, D5); if a later migration lands
  Kobalte's Dialog, both collapse into its primitive.
- `apps/ui/src/design/components/shortcuts.css`: the overlay-specific layout
  (header row, scroll region `max-height`, chord chip) consuming only `--cx-*`
  tokens (`--cx-text-faint` headers, `--cx-text-dim` chips on `--cx-font-ui`,
  transitions on `--cx-motion-fast`/`--cx-ease-out` only).
- `apps/ui/src/App.tsx`: import `./design/components/menu.css` (dialog styles,
  currently unimported — `App.tsx:4-8`) + `./design/components/shortcuts.css`
  (Biome-sorted); render `<Show when={store.shortcutsOpen()}>
  <ShortcutsOverlay /></Show>` inside the shell root.

Interfaces:

- consumes: `buildShortcutGroups` (T3), `DEFAULT_KEYMAP`, `detectPlatform`
  (T1b, imported from `dispatch.ts`), store members (T2),
  `.cx-dialog`/`.cx-dialog-backdrop` (`menu.css:69-90`), `.cx-search`
  (`input.css:5-33`).
- produces: `ShortcutsOverlay: Component` (no props — reads the store via
  `useStore()`), class contract `.cx-shortcuts`, `.cx-shortcuts-group`,
  `.cx-shortcuts-row`, `.cx-shortcuts-chord`, `.cx-shortcuts-empty`.

Test cycle: new `apps/ui/src/components/ShortcutsOverlay.test.tsx` (render via
`mountApp` + `store.toggleShortcuts()`): dialog role/aria present, search
input auto-focused, Escape closes, typing filters rows, no-match shows the
empty row; focus-restore — focus a button, open, close → focus returns to
that button; trap integrity — Tab from the last focusable wraps to the
first, and Escape STILL closes after tabbing (the trap keeps the local
handler reachable). Run `bun test --conditions browser
src/components/ShortcutsOverlay.test.tsx`.

### T5 — Production-wiring e2e

Extend `apps/ui/src/keyboard-e2e.test.tsx` (its `press`/`setPlatform` helpers,
`keyboard-e2e.test.tsx:15-30`) with a `?` suite that registers NOTHING for the
open path:

1. `mountApp("/backlog")`, `press({ key: "?", shiftKey: true })` → overlay
   visible (`[role="dialog"]` with the shortcuts aria-label); press again with
   focus outside the input → closed (toggle); focus a button first, open,
   close → focus is restored to that button.
2. Focus a text input (insert one, or the comms composer), press `?` → overlay
   does NOT open (editable-target guard, `dispatch.ts:87-88`); the same while
   the overlay's own search input is focused → stays open, no re-toggle.
3. Generated-content proof on REAL registrations: `mountApp("/")` (Bridge
   mounts, registering `board.openAssignedAgent`, `Bridge.tsx:433-439`), open
   overlay → a row `Shift+Enter → Open assigned agent` exists in the `main`
   group; `palette.open` (`keymap.ts:67`, unregistered on main) does NOT
   appear.
4. Drift-immunity: through `store.keyboard.registry.register(...)` (the
   public seam, `keyboard-e2e.test.tsx:90`) register `palette.open`, reopen →
   the `Mod+K` row now appears.
5. (Rides with T2b; requires RIG-2529 merged) list rows follow board
   lifecycle: on `/` the `main` group contains the `ArrowUp → Move up` row;
   on `/backlog` (board unmounted, `list.*` retracted) it does not.
6. Tier-3 leak non-regression (primarily RIG-2529's proof — see T2b; kept
   here as a consumer check): `mountApp("/")` (Bridge mounted), focus a
   non-board non-editable element (a topbar button), press Enter → no list
   command runs and the event is NOT `preventDefault`ed (native button
   activation preserved).

Interfaces:

- consumes: `mountApp`/`flush` (`test-router.tsx:28-56`), the full
  T1a/T1b-T4 stack.
- produces: the RIG-2482 acceptance suite.

Test cycle: red first (write against main, watch every case fail), then green
on the stack. Run `cd apps/ui && bun test --conditions browser
src/keyboard-e2e.test.tsx`, then the full affected set: `bun test
--conditions browser src/keyboard src/components/ShortcutsOverlay.test.tsx
src/keyboard-e2e.test.tsx` + Biome + stylelint per repo pre-finish
convention.

### Task dependency shape

T1a, T1b, T2, T3 are independent (parallelizable); T4 needs T1a+T1b+T2+T3;
T5 needs T4. T2b needs RIG-2529 MERGED (T5 cases 5-6 ride with it) — the
overlay impl PR stacks on that merge; the rest of the record executes
independently of RIG-2529's schedule.

### Merge coordination with RIG-2483 (command palette)

This record and the palette record
(`docs/designs/product/compass-command-palette/design.md`, RIG-2483) rewrite
the SAME four regions: the `createKeyboardSpine` deps object + signature
(`spine.ts:66-68`), the one-line spine creation call (`store.ts:1896`), the
`App.tsx:4-8` import run plus a shell-root `<Show>`, and the
`spine.test.ts:37-43` assertions. The changes are semantically additive
(disjoint dep names, command ids, signals) but textually hard-conflicting:
whichever branch merges second gets a guaranteed conflict on all four files.
Resolution rule: the deps object is a set-union (`showBridge` +
`toggleShortcuts` + the palette's deps), registrations concatenate, imports
Biome-sort; the second merger rebases and re-runs `spine.test.ts`. The
failure mode to watch for is a naive resolution silently dropping a dep. No
design change either way.

**Cross-record substrate note:** RIG-2529
(`docs/designs/meta/compass-tier3-scope-gate/design.md`) is the shared
scope-gate substrate BOTH this record and RIG-2483 cite as an external
frozen dependency; the overlay and palette impl PRs stack on RIG-2529's
merge. The four shared App-root regions above remain an impl-time
set-union coordination between the two sibling impl PRs only.

**Merge ORDER (design-PR / ledger level):** RIG-2529 (#508) must merge
*before* this record (#509). This record's ledger rows DL-230/231 reference
DL-227/229 (RIG-2529's scope-gate + commands-as-inventory rows), which exist
only in #508's tree — not on main (real max DL-226) nor in this PR's diff. The
design-ledger-gate enforces id-disjointness and above-max but does NOT verify a
cited DL resolves, so an out-of-order merge would land DL-230/231 with dangling
references and nothing mechanical would catch it. The driver sequences the
three design-PR merges #508 → #509/#510. (This is the ledger dependency; the
four-region App-root conflict above is the separate impl-PR concern.)

## Tasks

- [ ] T1a — `eventToChord` Shift-drop for modifier-less printable
      non-ASCII-letters, EXCLUDING space (+ red-first `?` and `Shift+Space`
      dispatch tests)
- [ ] T1b — `detectPlatform` export from dispatch.ts; `?` keymap row
      (+ dispatch tests)
- [ ] T2 — store `shortcutsOpen` signal + close-on-navigation + spine
      `view.shortcuts` registration (+ spine/store tests)
- [ ] T3 — `shortcuts-model.ts` `buildShortcutGroups` join + substring filter
      (+ pure-module tests)
- [ ] T2b — `list.*` rows via the RIG-2529 scope gate (external frozen
      dependency; the overlay impl PR stacks on RIG-2529's merge) — pure
      registry consumption, verified by T5 cases 5-6
- [ ] T4 — `ShortcutsOverlay.tsx` (focus-restore + Tab trap unconditional) +
      `shortcuts.css` + App.tsx mount & css imports (+ component tests)
- [ ] T5 — production-wiring e2e suite in `keyboard-e2e.test.tsx`
      (+ full-set run, Biome, stylelint)

## Decisions

1. **`view.shortcuts` registers in the spine, not App.tsx** — mirrors the
   ratified `view.bridge` precedent (`spine.ts:59-64`: "the registration lives
   with the behavior"); the store owns the open signal so the command's
   behavior closure exists where the spine is created (`store.ts:1893-1896`).
2. **`?` is authored as the bare chord `"?"`; `eventToChord` drops `Shift`
   for modifier-less single printable non-ASCII-letter keys, EXCLUDING
   space** — layout-portable (the event's `key` already encodes the shifted
   character), scoped so no existing binding changes shape. Space is carved
   out (the `" "` → `"Space"` rename applies before the predicate) because
   `" "` is a single non-letter char that would otherwise silently rebind
   `Shift+Space` to `list.expandOrToggle` (`keymap.ts:84`). `/[a-z]/i` is
   ASCII-only, so a future non-Latin letter binding would be Shift-dropped
   inconsistently with `Shift+B` — noted, accepted. The alternative
   (authoring `"Shift+?"`) breaks on layouts where `?` is not shifted.
   **RATIFIED (Matt, 2026-08-23): the GENERAL rule** (all modifier-less
   single printable non-ASCII-letters, excluding Space) — not a `?`-only
   special case, which would be a trap for the next bare punctuation
   chord. The Space carve-out and the red-first `Shift+Space` regression
   row stay; the ASCII-only `/[a-z]/i` caveat stays flagged for the next
   binding author.
3. **Join omits unregistered keymap rows AND keymap-less commands** — the
   sheet lists only chords that actually do something (dispatcher treats
   unregistered as dead, `dispatch.ts:122-129`) and only commands reachable by
   a chord; the palette (RIG-2483) owns the full command inventory.
4. **Substring search, no fuzzy** — no fuzzy helper exists in the repo (grep:
   only prose mentions in `components.md:421,427` and `commands.ts:33,79`);
   fuzzy ranking is the palette's deferred concern (`commands.ts:80-81`), and
   a ~20-row reference sheet doesn't need ranking.
5. **Hand-rolled dialog per `StartAgentDialog` precedent, styled
   `.cx-dialog` — WITH unconditional focus-restore and a minimal Tab trap**
   — `components.md:402-409` names Kobalte for focus-trap, but
   `@kobalte/core` is not a dependency (`package.json:10-26`) and the shipped
   dialog is hand-rolled; that precedent is initial-focus-only
   (`StartAgentDialog.tsx:17-37` — no trap, no restore), which this record
   treats as a gap, not a contract: trap + restore ship in T4
   unconditionally. **RATIFIED (Matt, 2026-08-23): hand-roll now** — no
   `@kobalte/core` for this dialog; migrate to Kobalte's Dialog later if
   ever. Rationale: one DOM-only testable modal-chrome pattern across the
   discoverability surfaces; Kobalte only where load-bearing (the sibling
   palette's combobox ARIA/traversal).
6. **Registry read is snapshot-at-open (memo keyed on open + query), not
   reactive** — the registry is a plain Map by design (`spine.ts:34-36`
   documents the non-reactivity choice for the group set; `registry.ts:19`);
   making it reactive for a reference sheet would be a second convention.
7. **The overlay toggles on `?` and closes on Escape locally** — consistent
   with the escape-ladder rung `menu-dialog` (`zones.ts:60-71`); no global
   `Escape` keymap row exists, so a local handler (the `StartAgentDialog.tsx:34-36`
   pattern) is the current convention.
8. **The `list.*` overlay-rows rule is RATIFIED (Matt, 2026-08-23): the
   long-term end state — register the group-relative commands AND make
   tier 3 scope-aware — owned by RIG-2529
   (`docs/designs/meta/compass-tier3-scope-gate/design.md`), an
   external frozen dependency of this record.** The naive drafted shape
   (register against a scope-blind tier 3) stays withdrawn as unsafe;
   RIG-2529's scope gate (run a tier-3 match only when `command.scope` is
   `"global"` or matches the active zone) makes `scope: "main"`
   registration safe and closes RIG-2130 RD-4/OQ-6. The unifying
   commands-as-inventory rule, shared verbatim with RIG-2483: **register =
   actionable-from-anywhere; a dispatching surface (the palette's action
   mode) reads the registry, a display surface (this overlay) reads the
   keymap; a command is registered beside its behavior, never purely for
   discoverability** — the tier-3 scope gate is what makes that rule safe
   to follow. The surviving invariant: NO hand-maintained sidecar list
   enters the overlay.
9. **Close-on-navigation** — the store's navigation closures also call
   `hideShortcuts()`, so the snapshot-at-open sheet (D6, which stands) can
   never keep advertising commands a route change retracted, and no modal
   floats over a new route. One line per closure; the alternative (overlay
   persists across navigation) was rejected as a silently-stale surface.
10. **`detectPlatform` is exported from `dispatch.ts`, not `keymap.ts`** —
    the predicate already lives there (`dispatch.ts:70-74`), and `keymap.ts`
    stays a pure data/string module with no `navigator` read (keeps the
    pure-module test lane DOM-free). Tradeoff noted: chord RENDERING helpers
    (`resolveChord`) stay in keymap.ts while platform DETECTION lives with
    the dispatcher that consumes it — acceptable, since the overlay already
    imports from both modules.

## Resolved forks (ratified by Matt, 2026-08-23)

The three load-bearing forks this record carried to review are settled;
the full rulings live in the Decisions above. In brief:

1. **Dialog host (was OQ1).** Hand-roll the `.cx-dialog` overlay NOW — no
   `@kobalte/core` — with focus-trap + focus-restore as unconditional T4
   correctness; migrate to Kobalte later if ever (D5). Rationale: one
   DOM-only testable modal-chrome pattern; Kobalte only where load-bearing
   (the palette's combobox).
2. **The `list.*` block (was OQ2, the blocker).** The principled end state
   — register the eight `list.*` commands AND make tier 3 scope-aware,
   closing RIG-2130 RD-4/OQ-6 — extracted to its own record **RIG-2529**
   (`docs/designs/meta/compass-tier3-scope-gate/design.md`), which this
   record consumes as external frozen substrate (D8, T2b). Neither the
   group-relative-rendering workaround nor deferral to RIG-2483 was taken.
3. **Shift-drop scope (was OQ3).** The GENERAL rule — drop `Shift` for a
   modifier-less single printable non-ASCII-letter, EXCLUDING Space — not
   a `?`-only special case (D2). The Space carve-out and its red-first
   `Shift+Space` regression row stay; the ASCII-only caveat stays noted.

## Open Questions

1. **[non-load-bearing] Overlay listing scoped duplicates.** `Enter`
   appears twice, both resolving to `main`-scoped commands — `list.openOrSelect`
   (`keymap.ts:83`) and `comms.send` (`keymap.ts:110`) — so the two rows sit
   adjacently within the single `main` group (grouping is by `command.scope`).
   Fine to ship; a per-row "wins while composing" annotation that disambiguates
   the two same-group `Enter` rows is deferrable polish.
2. **[non-load-bearing] Where the `?` hint is advertised.** Linear shows
   the help affordance in its UI chrome; a topbar hint (`? for shortcuts`)
   is out of scope here and can ride a later chrome pass.
