# Compass command palette (Cmd/Ctrl+K) + shortcut chips at point-of-use (RIG-2483)

Status: Draft

Parent: RIG-1661 (keyboard discoverability net). Builds on the App-root
keyboard spine (RIG-2456, `compass-keyboard-spine-app-root/design.md`, merged).
Sibling: RIG-2482 (`?` shortcuts overlay) is a separate record designed in
parallel — the palette is the *do-an-action* surface, the overlay the
*reference sheet*; the shared substrate is the App-root dispatcher and
registry both consume, plus the shared commands-as-inventory rule OWNED by
the RIG-2529 tier-3 scope-gate record
(`docs/designs/meta/compass-tier3-scope-gate/design.md`) — an external
frozen dependency both records cite, never redefine (see A3/D6). No other
cross-dependency is designed here, but at IMPL time the palette and overlay
PRs touch the same four App-root wiring lines (`spine.ts` deps + signature,
the `store.ts:1896` spine-creation site, `App.tsx` imports + `<Show>` hosts,
`spine.test.ts:37-43` deps fixture) — resolved by set-union; whoever merges
second rebases.

## Problem / Intent

The command palette is fully specified but does not exist. The spec is frozen
in `apps/ui/src/design/components.md:411-454` ("Command palette (Kobalte
combobox) … One surface, two modes, prefix-free") and its CSS is already
shipped (`apps/ui/src/design/components/palette.css:1-2` — "Command palette —
`.cx-palette` (D3/D5, Kobalte combobox). The full palette surface"; NOTE: the
issue's `apps/ui/src/components/palette.css` path is stale — the real file is
under `design/components/`). But there is no `Palette.tsx` in
`apps/ui/src/components/` (directory listing shows none), `.cx-palette-shortcut`
has zero TSX consumers (grep over `src/` matches only the CSS and the spec),
and the tabled chord `apps/ui/src/keyboard/keymap.ts:67` —
`{ chord: "Mod+K", commandId: cmd("palette.open") }` — is dead: nothing
registers `palette.open`, so the dispatcher's tier-3 lookup
(`dispatch.ts:124-128`) finds no command and the event falls through.

Cmd+K is the universal fallback ("forget every other shortcut, press Cmd+K") —
the safety net that makes an aggressive keyboard surface non-punishing. Ship
it, and surface each command's shortcut chip at point-of-use so the direct
chords are learnable from the surfaces that invoke them.

## Global Constraints

- **SolidJS ^1.9.13 (v1)** (`apps/ui/package.json:24` — `"solid-js":
  "^1.9.13"`). RIG-2187's v2 bump is on a separate merge-gated branch; design
  against v1 using the v1-legal forward idioms: `createMemo`, `<For>` (not
  `<Index>`), `createEffect(on(deps, apply))`. Vite + TS.
- **Biome 2.5.4 pinned**; relative imports sorted alphabetically.
- **Night Owl `--cx-*` semantic tier only** (`apps/ui/src/design/tokens.css`);
  never the dead legacy tier. Reduced-motion honored via token. The chase-light
  loader is consumed by reference (`.cx-loader[data-topology="bar"]`,
  foundation-T8 / `design/motion.md`) — never re-authored.
- **Command ids `noun.verbCamel`**; board-scoped `board.*`.
- **One keydown path**: any new global chord registers a `Command` into the
  shared `store.keyboard.registry` and rides the existing App-root dispatcher
  (`App.tsx:48-54`). NEVER a second window keydown listener. The palette's
  *inside* keys (list traversal, Enter, Escape) are Kobalte's own — they live
  on the Search primitive's focused input/listbox, not on window, so they do
  not violate this.
- **Kobalte scoped to a11y-hard behavior only** (DL-150,
  `docs/designs/product/DECISIONS.md:261` — "Kobalte scoped to a11y-hard
  behavior only (D3)"); all visuals via our `.cx-*` classes, no Kobalte default
  styles.
- **Tests**: `cd apps/ui && bun test --conditions browser <file>`; pure-module
  `MOON_TOOLCHAIN_FORCE_GLOBALS=true bun test --conditions browser
  src/<file>.test.ts`. Every new global-chord contract needs a
  **production-wiring** test: mount the real App via `mountApp`
  (`test-router.tsx:35-56`), press the chord — never a test that
  self-registers the command under test (the anti-pattern the RIG-2456 review
  caught; the good pattern is `keyboard-e2e.test.tsx:37-48`).
- **Registry contract is frozen-plus-additive** (DL-152/DL-225): consume
  `CommandRegistry` as-is (`commands.ts:108-116` — `register/get/all/
  unregister`); do not widen it.

## Approach

One sentence: a `Palette.tsx` component hosted once at the App root behind a
store-owned `paletteOpen` signal, opened by a `palette.open` command registered
in `createKeyboardSpine` beside `view.bridge` (registration lives with the
behavior), rendering a Kobalte Search primitive styled entirely by the shipped
`palette.css`, whose single prefix-free input matches **registered commands**
(action results, fuzzy over `registry.all()`, scoped-above-global per D5) and
**store-backed destinations** (navigation results via `DestinationProvider`s) —
each command row carrying its shortcut chip derived from the keymap table — plus
a small `shortcutFor`/`<ShortcutChip>` seam that makes the same chip renderable
at every future point-of-use.

### A1 — Open-state + `palette.open`: on the store, registered in the spine

The RIG-2456 pattern is explicit: "the registration lives with the behavior —
the spine is created in `createAppStore` where `showBridge` is in scope"
(`spine.ts:60-64`). Mirror it exactly:

- `createAppStore` gains `const [paletteOpen, setPaletteOpen] =
  createSignal(false)` plus `openPalette()`/`closePalette()`/`togglePalette()`
  helpers, and passes `togglePalette` into `createKeyboardSpine` alongside
  `showBridge` (`store.ts:1896` today: `const keyboard = createKeyboardSpine({
  showBridge });`). `openPalette()` also captures the D3 snapshot `{ zone,
  element }` (A3/D3) — **only on the false→true (opening) transition**; the
  close-toggle leg never re-captures (it would read a null zone with focus in
  the input and clobber the snapshot). `closePalette()` restores focus from it.
- `createKeyboardSpine` registers `palette.open` as its second seed command,
  next to `view.bridge` (`spine.ts:70-77`): `{ id: cmd("palette.open"),
  title: "Open command palette", keywords: ["palette", "command", "search",
  "k"], scope: "global", run: () => deps.togglePalette() }`. **No `shortcut`
  string** — the chip derives from the keymap via `shortcutFor` (A5/D4).
  The keymap row already exists (`keymap.ts:67`), unscoped, so it resolves at
  tier 3 (`dispatch.ts:122-128`) from anywhere — including editable targets,
  since the guard only suppresses modifier-less chords (`dispatch.ts:83-88`:
  "Mod/Ctrl/Alt chords are global and are NOT guarded").
- `AppStore` exposes `paletteOpen: Accessor<boolean>` and `closePalette()`;
  `App.tsx` renders `<Show when={store.paletteOpen()}><Palette /></Show>`
  inside the shell root (after the topbar; `--cx-z-palette` stacking is
  the CSS's concern: `tokens.css:233` — `--cx-z-palette: 300;`).

`Mod+K` while the palette is open **closes** it (toggle): `run()` is
`togglePalette()` — Linear parity, and the cheapest "get me out" alongside
Escape. Every close path (Escape, outside click, `Mod+K` toggle, running a
result) funnels through `closePalette()`, which restores focus (D3).
(Decisions D1/D5 below.)

Because the palette is app-lifetime at the root, `palette.open` is never
unregistered — the DL-225 `unregister` path is for surfaces that unmount
(`commands.ts:112-115`); the spine's seed commands are not that.

### A2 — Host: permanently-open Kobalte Search primitive in a fixed `.cx-palette` wrapper

The spec mandates the host: "**Host:** a Kobalte combobox; `Cmd/Ctrl+K` opens
it at `--cx-z-palette`. ARIA + keyboard traversal are Kobalte's; this owns only
the visual surface" (`components.md:447-448`). `@kobalte/core` is **not** a
dependency today (`package.json:10-26` — no `@kobalte/*` entry; grep for
`kobalte` over `src/` + `package.json` returns no code hits). **Adding it is
settled**: the spec names the host (`components.md:445-446`) and DL-150
pre-authorizes Kobalte for exactly this a11y-hard behavior. The spec says
"combobox" loosely; the host is Kobalte's **Search** primitive (a Combobox
variant — see the Anatomy below), which is what carries the published
command-menu recipe this surface needs. The host ANATOMY is likewise settled
— **the bare fixed wrapper below, ratified by Matt 2026-08-23 (D8)**; the
Kobalte `Dialog` host is Alternatives-rejected.

**Anatomy — Kobalte's published command-menu recipe.** The host is Kobalte's
**Search** primitive — a Combobox variant that inherits Combobox's ARIA +
listbox machinery but leaves result filtering to the caller (exactly the
palette's own fuzzy match). Kobalte publishes a command-menu recipe for Search
for precisely this shape: "add the `open` prop to permanently open [the]
dropdown. Replace `Search.Portal` and `Search.Content` with a div to directly
mount your content below the search input"
(<https://kobalte.dev/docs/core/components/search/>, `@kobalte/core`
v0.13.13). The default Search/Combobox popover anatomy otherwise floats
`Portal > Content > Listbox` as a separate popper-positioned element anchored
to `Control` — `.Input` lives in `Control` OUTSIDE the Portal, so that shape
would render the input at the App-root mount point while the list portals to
`document.body` at a popper-computed position, making the shipped single-panel
`.cx-palette` (the one 560px elev-3 float that contains BOTH input and list,
`palette.css:1-6`, `components.md:441-445`) unproducible. The recipe is the
fix: pin `open` and drop the portal. Concretely:

- A hand-positioned `position: fixed` wrapper div IS `.cx-palette`; the whole
  Search primitive mounts INSIDE it: `Search.Root` (options = the merged
  result list, controlled `open={true}` pinned while mounted, `onOpenChange`,
  caller-managed `onInputChange` filtering), `Search.Control` + `Search.Input`
  (`class="cx-palette-input"`), then — per the recipe — a plain `div`
  replacing `Search.Portal`/`Search.Content` that directly holds
  `Search.Listbox` (`class="cx-palette-list"`); the list renders inline below
  the input, exactly the shipped CSS's anatomy. Per-result `Search.Item`
  (`class="cx-palette-row"` — Kobalte emits `data-highlighted`/
  `data-disabled`, which `palette.css:150-151` already styles).
- Our own backdrop div (`class="cx-palette-backdrop"`) renders as a sibling
  behind the wrapper — it is not a Kobalte positioner.
- **Dismiss is ours, thin:** the backdrop's click handler and Escape both
  route to `store.closePalette()` (Kobalte's `onOpenChange(false)` — fired
  e.g. when it interprets Escape on the input — also routes there; with
  `open` pinned true, the visual teardown is always the `<Show>` unmount,
  never Kobalte's own dismiss).
- **Focus grant on open:** nothing focuses the input by itself — Kobalte
  Search does not autofocus the input (its open-time focus strategy targets a
  list item, not the input, `@kobalte/core` v0.13.13). `Palette.tsx` therefore
  holds a `ref` to the input and calls `input.focus()` in `onMount` (the
  `<Show>` mounts fresh per open).
- **Click-dismiss guard:** with `Search.Listbox` outside `Search.Content` the
  input's blur handler has no registered `contentRef` to exclude, so a bare
  pointer-down on a result row would blur the input. Two mechanics close this:
  `open` stays controlled-pinned true (a blur can never close the pinned list),
  and each result row carries `onMouseDown`-preventDefault so focus never
  leaves the input on click; selection fires on the row's own activate path.
  The sole teardown remains `closePalette()` → `<Show>` unmount.
- Because nothing portals and nothing is popper-positioned, **floating-ui is
  out of the test path entirely** — the happy-dom risk noted in A7 is
  retired by this shape.

All classes/geometry come from the shipped CSS (`palette.css:8-190`); no
Kobalte default styles ship. `App.tsx` imports nothing new for this — today
it imports only `badge-glyph.css`/`card.css` (`App.tsx:6-7`); `Palette.tsx`
imports `../design/components/palette.css` itself (component-adjacent import,
the pattern the other `design/components/*.css` files await).

### A3 — Action mode: fuzzy over `registry.all()`, scoped-above-global

Per the spec: "**Action mode** fuzzy-searches registered commands (`{ id,
title, keywords, scope, shortcut?, run() }`); scoped commands rank above global
when their scope is active, and each shows its shortcut chip"
(`components.md:420-424`). The ranking rule is D5's, ledgered as DL-152 and
already restated on the keymap (`keymap.ts:48-51` — "the scoped entry takes
precedence while its zone is active (D5's ranking rule: 'scoped commands rank
above global ones when their scope is active')").

- Source: `store.keyboard.registry.all()` (`commands.ts:111`). Match against
  `title` + `keywords` (`commands.ts:32-33` — "`keywords` broadens fuzzy
  matching in the palette").
- Fuzzy matcher: a small in-house case-insensitive subsequence scorer in a new
  pure module `apps/ui/src/keyboard/fuzzy.ts` (word-boundary and
  start-of-string bonuses, contiguity bonus). No new dependency for this —
  the ranking *weighting* is explicitly compass-ui's to choose
  (`components.md:427-428`: "the weighting is `compass-ui`'s and is not
  encoded here"; `commands.ts:79-82` pins only the inputs). Pure module →
  pure-module test cycle.
- **Snapshot capture at open time.** Opening the palette moves DOM focus into
  the Search input, so `store.keyboard.activeZone()` — derived from the
  focused roving group (`spine.ts:95-97`: `activeGroup()?.group.zone ??
  null`) — reads `null` once the palette is up, and the pre-open focused
  element is unknowable after the move. The palette therefore captures a
  snapshot `{ zone: activeZone(), element: document.activeElement }` **at
  the moment it opens** (in the `openPalette` path, stored beside
  `paletteOpen`). The `zone` half drives ranking — commands whose `scope`
  equals the captured zone rank above `scope: "global"` ones, title-fuzzy
  score deciding within each band; the `element` half drives focus restore
  on close (the escape-ladder's `return-to-anchor` contract, `zones.ts`
  `ESCAPE_STEP`). Decision D3.
- Running a result: `command.run()` then `closePalette()`. A command that
  navigates does so through its own closure (e.g. `view.bridge → showBridge()`,
  `spine.ts:70-76`).

**Seed registrations so action mode is non-empty.** Today only three commands
are registered app-wide: `view.bridge` (`spine.ts:70-77`) and the Bridge's
`board.openAssignedAgent`/`board.openCardCrossLink` (`Bridge.tsx:433-444`,
mounted only while the board is). The keymap tables `view.settings` (`Mod+,`,
`keymap.ts:66`) and `view.agentWorkspace` (`Mod+Shift+A`, `keymap.ts:65`) but
neither is registered — dead chords. This record registers **`view.settings`**
(→ `showSettings`, `store.ts:1892`), **`view.backlog`** (→ `showBacklog`,
`store.ts:1890`), and **`view.done`** (→ `showDone`, `store.ts:1891`) in
`createKeyboardSpine` beside `view.bridge` — same registration-beside-behavior
pattern, and `Mod+,` goes live for free. `view.agentWorkspace` is NOT
registered here: it needs a "current/last agent" notion the store does not
expose — OQ-4.

**The shared commands-as-inventory rule** (stated identically in the RIG-2482
overlay record; OWNED by the RIG-2529 tier-3 scope-gate record,
`docs/designs/meta/compass-tier3-scope-gate/design.md`): *register =
actionable-from-anywhere; a dispatching surface (this palette's action mode)
reads the registry, a display surface (the overlay) reads the keymap; a
command is registered beside its behavior, never purely for discoverability —
and tier 3 only runs a command whose scope is global or matches the active
zone, so registering a `scope: "main"` command is safe.* The seeds above are
`scope: "global"` view commands, so the palette does NOT depend on RIG-2529
for its own correctness — the rule is restated here only so the two records
never diverge; RIG-2529 is the external frozen substrate that makes the
registry safe as the app-wide inventory (e.g. the eight `list.*` commands it
registers, which then earn palette action rows for free).

### A4 — Navigation mode: store-backed `DestinationProvider`s

The contracts already exist, frozen and unimplemented: `Destination`
(`commands.ts:63-70` — `{ id, title, kind, navigate(), score? }`),
`DestinationKind` (`commands.ts:49-56` — `agent | channel | topic | issue |
pr | view`), `DestinationProvider` (`commands.ts:84-87` — `{ id,
query(input): Promise<Destination[]> }`). "Navigation mode — bare typing, no
`>` prefix — matches both commands and destinations" (`components.md:424-426`).

New pure module `apps/ui/src/keyboard/destinations.ts` exporting
`createStoreDestinationProviders(store): DestinationProvider[]` over the
store's reactive accessors:

- **agents** — `store.agents` (`store.ts:384`), `navigate: () =>
  store.openAgent(id)` (`store.ts:1248-1250`).
- **channels** — `store.channels` (`store.ts:394`), `navigate: () =>
  store.openChannel(id)` (`store.ts:1256`).
- **topics** — `store.topics` (`store.ts:398`), `navigate: () =>
  store.openTopic(id)` (`store.ts:1276-1277`).
- **views** — a static four-entry provider (Bridge/Backlog/Done/Settings) over
  `show*` (`store.ts:1889-1892`).
- **issues** — IN at ship (D9, Matt 2026-08-23; no longer conditional): the
  tracker's assigned-issue seam the board already reads
  (`store.assignedIssues`, `store.ts:552-554,783` — a reactive accessor over
  the tracker query); `navigate: () => store.selectIssue(id)`
  (`store.ts:1283-1287`), which selects the issue's card and syncs the
  roster.
- **prs** — IN at ship (D9, Matt 2026-08-23; Matt explicitly accepted the
  added scope). The store has NO PR collection today — `IssueTab = "files" |
  "vcs" | "pr"` (`store.ts:99`) is a detail-pane tab, not a collection — so
  the palette impl ADDS a store-level PR accessor. The contract fixed here:
  `AppStore` gains `prs: Accessor<PrRow[]>` — a `createMemo` over `issues()`
  via the existing pure `prRows()` (`board.ts:128-134`; `PrRow = { issue,
  pr }` pairs each open PR with its owning issue). The provider maps each
  row to a `Destination` with `kind: "pr"`, id `` `${pr.repo}#${pr.number}` ``,
  and `navigate` selecting the owning issue and revealing the PR pane
  (`store.selectIssue(issue.id)` + `store.setActiveRightTab("pr")`,
  `store.ts:322-323`). Exact store wiring is an impl detail; the accessor
  name/shape and the provider contract are fixed here. The fixture already
  carries PR shapes (`Issue.prs: PullRequest[]`, `stub-data.ts:238`), so no
  new data source is needed offline.

Providers are `Promise`-returning by contract; the store-backed ones resolve
synchronously-wrapped. Ranking: fuzzy score + recency where the store exposes
it — the weighting is intentionally unpinned (`commands.ts:79-82`), so the
implementation picks a simple linear blend and documents it in code, not here.
While any provider is in flight the list shows `.cx-palette-loading` hosting
`.cx-loader[data-topology="bar"]` (`components.md:438-440`, `palette.css:188`)
— by reference to T8's keyframe.

Results render in `.cx-palette-group` sections (`palette.css:74`): **Commands**
first (action results), then one group per destination kind with results, per
the spec's grouped-list anatomy. Prefix-free: one query string feeds both.

### A5 — Result-row anatomy + the shortcut chip seam

Each row renders the frozen four-part anatomy (`components.md:429-433`;
`palette.css:87-147`): `.cx-palette-glyph` (9px 1-bit type glyph — command icon
or destination `kind`; compass-ui emits the inline SVG, `palette.css:108`) ·
`.cx-palette-title` · `.cx-palette-context` (dim: the command's `scope` / the
destination's parent) · `.cx-palette-shortcut` (right-aligned chip).

**Chips derive from the keymap via `shortcutFor` — no hand-authored strings
anywhere.** The contract already says the split: "`shortcut` is the display
chip (the authoritative chord lives in the keymap table, see keymap.ts)"
(`commands.ts:33-34,41-42`). Hand-authoring the chip at a registration site
drifts from the table AND renders unresolved — `shortcutFor` is
`resolveChord`-resolved (Mod→Cmd/Ctrl), a raw `"Mod+K"` override string would
show literally — so no registration in this record sets `shortcut` (D4). A
new pure helper in `apps/ui/src/keyboard/keymap.ts`:

```ts
/** Display chord for a command: first DEFAULT_KEYMAP row bound to it, platform-resolved. */
export function shortcutFor(id: CommandId, platform: Platform): string | undefined;
```

(first matching row, `resolveChord`-resolved — `keymap.ts:37-38`). The palette
renders `shortcutFor(command.id, platform)` — every command this record gives
a chip has a keymap row. `Command.shortcut` stays in the frozen contract as an
escape hatch for a future keymap-less command, but any such override MUST be
piped through `resolveChord` before rendering, never shown raw. Presentation:
a tiny `<ShortcutChip chord={...} />` component
(`apps/ui/src/components/ShortcutChip.tsx`) that splits on `+` and renders the
`.cx-palette-shortcut` chip — one place to later prettify (`Cmd` → `⌘`).

### A6 — Shortcut at point-of-use beyond the palette (part 2, scoped)

Recon: grep over `src/` finds **zero** `.cx-menu` or `.cx-tooltip` TSX
consumers — both exist only as CSS (`design/components/menu.css:10`,
`tooltip.css:6`) and spec (`components.md:389-400,456-464`). There is no menu
or tooltip in the product today to put a chip in. The only existing
shortcut-at-point-of-use is the board cursor's `aria-keyshortcuts="Space"`
(`Bridge.tsx:469`).

So part 2 lands as **the seam plus the registrations, not a menu retrofit**:

1. `<ShortcutChip>` + `shortcutFor` (A5) are the shared primitives any future
   `.cx-menu-item` / `.cx-tooltip` renders its chip with — the contract is: a
   menu item or tooltip for a `PrimaryAction` (`commands.ts:97-99`) resolves
   its command and renders `<ShortcutChip>` right-aligned, exactly the palette
   row's treatment.
2. Today's registration sites — the spine's seeds and the Bridge's `board.*`
   registrations (`Bridge.tsx:433-444`) — set **no** `shortcut` string; their
   chips resolve via `shortcutFor` (D4). All the chipped commands have keymap
   rows.
3. The visible view-navigation buttons that exist TODAY invoke the exact
   `show*` paths the D6 seeds command — but they live in the **left sidebar**,
   not the topbar. The `LeftSidebar` `.bridge-link` buttons
   (`LeftSidebar.tsx:437-482`: Bridge/Backlog/Done/Settings) call
   `showBridge`/`showBacklog`/`showDone`/`showSettings` — i.e. `view.bridge` +
   the three D6 seeds. The topbar `.view-tabs` (`App.tsx:68-94`) holds only the
   Bridge tab (`view.bridge`, pre-existing) and a conditional agent tab (no
   registered command). **Ratified (D10, Matt 2026-08-23): T4 populates
   `aria-keyshortcuts` + `title` via `shortcutFor` on the LeftSidebar view
   buttons** — the surfaces that actually fire the seeded commands, so the D6
   seeds earn a visible, screen-reader-announced, hover-discoverable chip. The
   Bridge topbar tab gets the same treatment for parity (~12 lines total).
4. Actual `.cx-menu`/`.cx-tooltip` chip adoption is out of scope — DEFERRED
   to follow-up issue **RIG-2530** (D10); it belongs to the first surface
   that mounts one.

### A7 — Test strategy

- **Production-wiring chord tests (mandatory):** mirror
  `keyboard-e2e.test.tsx:37-48` — `mountApp("/")`, `setPlatform("other")`,
  dispatch `keydown {key:"k", ctrlKey:true}` on window, `await flush()`,
  assert `.cx-palette` (and the input, focused) present; `metaKey` variant
  for mac; press again → closed (toggle) and focus restored to the pre-open
  element; Escape → closed. Plus the chord D6 newly lights: `Mod+,` press →
  `store.view() === "settings"` (lands in T1's cycle). Registers NOTHING.
- **Pure-module tests:** `fuzzy.test.ts` (match/score/boundary bonuses,
  no-match), `keymap.test.ts` extension for `shortcutFor` (hit, miss,
  platform resolution), `destinations.test.ts` (each provider maps store
  fixtures to `Destination`s; `navigate()` routes — assert via the store's
  in-memory path).
- **Component tests** (`Palette.test.tsx`, mountApp-based): action results
  filtered + ranked (scoped-above-global with a captured zone), the toggle
  no-recapture case (open-from-board → `Mod+K` toggle-closed →
  reopen-from-board still ranks `main`-scoped above global), chip rendered
  from the keymap, navigation groups, latest-wins (a stale provider promise
  resolving late never clobbers newer results), empty state
  (`.cx-palette-empty`), run-closes-and-restores-focus, select-destination
  navigates (`await flush()` then assert `store.view()`).
- **Test mechanics:** query any portaled node (should one ever appear)
  against `document`, never `container.querySelector` — a portal lands under
  `document.body`, outside the mount container. Known risk this record's
  shape retires: Kobalte + floating-ui under happy-dom (no layout engine) is
  unproven in this repo (zero Kobalte usage today); A2's no-Portal/no-Content
  anatomy keeps floating-ui out of the test path entirely.

## Plan

Order: T1 → T2 → T3 → T4. T1 is the spine/store seam (no UI); T2 the surface +
action mode + the production-wiring test; T3 navigation mode; T4 the
point-of-use chip seam. One PR, stacked commits per task.

### T1 — `palette.open` command + store open-state + seed `view.*` registrations

Extend `createKeyboardSpine` deps and seeds; hang `paletteOpen` off the store.

- `spine.ts`: deps become `{ showBridge, showBacklog, showDone, showSettings,
  togglePalette }` (all `() => void`); register `palette.open`,
  `view.settings`, `view.backlog`, `view.done` beside `view.bridge` (ids as
  tabled in `keymap.ts:64-67`; `view.backlog`/`view.done` get no keymap row —
  palette-only until a chord is tabled). No seed sets `shortcut` (D4).
- `store.ts`: `paletteOpen` signal + `openPalette()`/`closePalette()`/
  `togglePalette()`; pass the new deps at the existing creation site
  (`store.ts:1893-1896`). `openPalette()` captures the D3 snapshot
  `{ zone: keyboard.activeZone(), element: document.activeElement }` —
  **only on the false→true transition**; the close-toggle leg never
  re-captures. `closePalette()` restores focus to the captured `element`
  (if still connected) and clears the snapshot.
- Interfaces:
  - `createKeyboardSpine(deps: { showBridge(): void; showBacklog(): void;
    showDone(): void; showSettings(): void; togglePalette(): void }):
    KeyboardSpine`
  - `AppStore` gains `paletteOpen: Accessor<boolean>`, `paletteZone:
    Accessor<FocusZone | null>` (the captured zone, read by A3's ranking),
    `openPalette(): void`, and `closePalette(): void` (restores focus). The
    captured `element` stays store-internal.
- Test cycle: extend `spine.test` coverage — the five seeds present in
  `registry.all()`; `palette.open.run()` toggles the store signal (drive via
  `createAppStore`); `view.settings.run()` navigates (in-memory path,
  `store.ts:724-741` pre-bind seam). **Production-wiring test for the chord
  D6 lights up:** `view.settings`'s keymap row (`keymap.ts:66`) was dead
  until this seed — a NEW live global chord, so per Global Constraints it
  gets a real-App test in `keyboard-e2e.test.tsx`: `mountApp`, press `Mod+,`
  → assert `store.view() === "settings"`. `bun test --conditions browser
  src/keyboard/spine.test.ts src/store.test.ts src/keyboard-e2e.test.tsx`
  (or the nearest existing suites).

### T2 — `Palette.tsx` (permanently-open Kobalte Search) + action mode + production-wiring test

New `apps/ui/src/components/Palette.tsx`; add `@kobalte/core` (settled;
anatomy per A2/D8); new `apps/ui/src/keyboard/fuzzy.ts`; `App.tsx`
renders `<Show when={store.paletteOpen()}><Palette /></Show>`.

- Fixed-wrapper anatomy per A2 (`open` pinned true, no Portal/Content, own
  backdrop, input `ref` + `focus()` on mount); classes/geometry entirely
  `palette.css`; snapshot captured at open, focus restored on every close
  path (A3/D3); rows per A5 with `<ShortcutChip>` (delivered here, consumed
  by T4); empty state `.cx-palette-empty`.
- Interfaces:
  - `Palette: Component` (no props — reads `useStore()`).
  - `fuzzy.ts`: `fuzzyScore(query: string, haystack: string): number | null`
    (null = no match; higher = better).
  - `keymap.ts`: `shortcutFor(id: CommandId, platform: Platform): string |
    undefined`.
  - `ShortcutChip: Component<{ chord: string }>` (renders
    `.cx-palette-shortcut`; T4 may generalize the class via a prop).
- Test cycle: `fuzzy.test.ts` + `shortcutFor` cases (pure,
  `MOON_TOOLCHAIN_FORCE_GLOBALS=true bun test --conditions browser
  src/keyboard/fuzzy.test.ts`); **`palette-e2e.test.tsx`** production-wiring
  per A7 (Ctrl+K opens and focuses the input / toggles closed with focus
  restored / Escape closes / run closes and navigates — `mountApp`, registers
  nothing); `Palette.test.tsx` ranking + toggle-reopen no-recapture case +
  chip render.

### T3 — Navigation mode: destination providers + grouped rendering + loading

New `apps/ui/src/keyboard/destinations.ts` (A4); Palette merges provider
results into kind groups under the Commands group; in-flight →
`.cx-palette-loading` hosting `.cx-loader[data-topology="bar"]` by reference.

- Interfaces:
  - `createStoreDestinationProviders(store: AppStore): DestinationProvider[]`
    (consumes `store.agents/channels/topics/assignedIssues/prs` +
    `show*`/`open*`/`selectIssue`/`setActiveRightTab`; produces `Destination`
    per `commands.ts:63-70` — ALL six kinds ship, D9).
  - Store PR accessor (added by this task): `AppStore.prs:
    Accessor<PrRow[]>` — `createMemo(() => prRows(issues()))` over the
    existing pure helper (`board.ts:128-134`); `PrRow = { issue: Issue; pr:
    PullRequest }`. The `prs` provider maps rows to `Destination{ kind:
    "pr" }` with `navigate: () => { store.selectIssue(row.issue.id);
    store.setActiveRightTab("pr"); }`.
  - Palette-internal: `queryDestinations(providers, input, generation):
    Promise<Map<DestinationKind, Destination[]> | null>` with per-provider
    isolation (one rejected provider drops its group, never the surface) and
    a **latest-wins guard**: the palette increments a query-generation
    counter per keystroke and passes it in; the token is captured at issue
    and compared at resolve — a resolution whose `generation` no longer
    equals the current counter is dropped (returns `null`, applies nothing),
    so a slow keystroke-N provider can never clobber keystroke-N+1's
    results. Store providers resolve sync-wrapped today, but the issue
    provider rides the async tracker seam and later kinds may be async —
    the contract is race-safe now. Debounce and result caps stay
    intentionally unpinned (tens of rows; the D2 in-house scorer is fine).
- Test cycle: `destinations.test.ts` (fixture store → ALL six mapped kinds
  incl. issue + pr; the `prs` accessor derives `PrRow`s from fixture issues;
  pr `navigate` selects the owning issue and sets the right tab; navigate
  routes; rejection isolation); `Palette.test.tsx` additions (groups render,
  selection navigates via `store.view()` after `flush()`, loading row while a
  hand-held pending provider is in flight, **latest-wins: a hand-held
  keystroke-N promise resolved after keystroke-N+1's results landed never
  clobbers them** — reuse the pending-provider fixture from the loading
  test, empty state when both modes miss).

### T4 — Shortcut chips at point-of-use (seam + existing sites)

Per A6: populate the existing registration sites and pin the contract for
future menus/tooltips.

- `Bridge.tsx:433-444`: the `board.*` registrations gain **no** `shortcut`
  strings — their chips resolve via `shortcutFor` like every other command
  (D4): `board.openAssignedAgent` → `Shift+Enter` (`keymap.ts:98`),
  `board.openCardCrossLink` → `Space` (the board maps the Lists-block `Space`
  to it, `keymap.ts:88-97`; the cursor already announces it via
  `aria-keyshortcuts`, `Bridge.tsx:469`). Both rows exist, so the chips
  surface with zero registration-site strings. The spine's seeds likewise
  resolve via `shortcutFor` (T1).
- View-button now-win (ratified, D10): populate `aria-keyshortcuts` + `title`
  via `shortcutFor` on the LeftSidebar `.bridge-link` view buttons
  (`LeftSidebar.tsx:437-482`: Bridge/Backlog/Done/Settings) — the surfaces that
  invoke the D6-seeded `show*` paths, so each seeded command earns a visible
  point-of-use chip — plus the Bridge topbar tab (`App.tsx:68-94`) for parity.
  `.cx-menu`/tooltip chip adoption is out of scope → RIG-2530.
- `ShortcutChip` gains a `class` pass-through so a `.cx-menu-item`/`.cx-tooltip`
  host can restyle the box; document the point-of-use contract as a comment on
  `PrimaryAction` (`commands.ts:97-99`): *a menu item or tooltip rendering a
  `PrimaryAction` resolves its command and renders `<ShortcutChip>`
  right-aligned*.
- Interfaces: `ShortcutChip: Component<{ chord: string; class?: string }>`.
- Test cycle: `Palette.test.tsx` asserts the board commands' chips surface in
  the palette while the board is mounted; an assertion that the LeftSidebar
  view buttons carry `aria-keyshortcuts` resolved via `shortcutFor`;
  existing `Bridge.test.tsx` aria-keyshortcuts coverage stays green. Full
  affected-area pass: `cd apps/ui
  && bun test --conditions browser` + `biome check` + stylelint (no CSS
  changes expected; palette.css is consumed as-is).

## Tasks

- [ ] T1 — spine seeds (`palette.open`, `view.settings/backlog/done`; no
      `shortcut` strings) + store `paletteOpen`/snapshot capture/
      `closePalette` focus restore + `Mod+,` production-wiring test + tests
- [ ] T2 — `@kobalte/core` dep + `Palette.tsx` (fixed-wrapper permanently-open
      Search primitive, action mode, snapshot capture, focus grant + restore,
      `ShortcutChip`, `fuzzy.ts`, `shortcutFor`) + production-wiring
      `palette-e2e.test.tsx` + component tests
- [ ] T3 — `destinations.ts` providers (ALL six kinds incl. issue + pr, D9) +
      store `prs: Accessor<PrRow[]>` accessor + grouped navigation results +
      latest-wins query generation + loading/empty states + tests
- [ ] T4 — point-of-use chips via `shortcutFor` (no hand-authored strings),
      `ShortcutChip` class seam, `PrimaryAction` point-of-use contract note,
      LeftSidebar view-button `aria-keyshortcuts` + `title` now-win (D10;
      menu/tooltip → RIG-2530) + tests

## Decisions

1. **D1 — Open-state on the store; `palette.open` registered in the spine.**
   Mirrors the ratified registration-beside-behavior pattern
   (`spine.ts:60-64`); no new context/provider; App renders the surface behind
   `store.paletteOpen()`.
2. **D2 — In-house fuzzy matcher, no dependency.** The weighting is explicitly
   compass-ui's (`components.md:427-428`, `commands.ts:79-82`); a ~40-line
   subsequence scorer beats a dep for a 26px-row list.
3. **D3 — Pre-open snapshot `{zone, element}` captured at palette-open time;
   focus restored on close.** Opening moves focus into the input, so
   `activeZone()` (focused-group-derived, `spine.ts:95-97`) reads null while
   open; the D5 scoped-above-global ranking (`keymap.ts:48-51`, DL-152) needs
   the pre-open `zone`, and the escape-ladder's `return-to-anchor` step
   (`zones.ts` `ESCAPE_STEP`; the palette is the ladder's first step) needs
   the pre-open `element` — Kobalte's non-modal Search does not restore
   focus itself, and after the `<Show>` unmount focus would fall to `body`,
   dropping the user's board cursor every palette peek. Both halves captured
   in `openPalette` (opening transition only — the close-toggle leg never
   re-captures); `closePalette` restores focus on every close path
   (Escape/outside-click/toggle/run).
4. **D4 — Chips derive from `DEFAULT_KEYMAP` via `shortcutFor`; no
   registration sets `shortcut`.** The contract already names keymap.ts
   authoritative (`commands.ts:33-34`); deriving kills registration-site
   drift, and a raw override renders unresolved (`shortcutFor` is
   `resolveChord`-resolved; a hand-authored `"Mod+K"` would show literally).
   Any future keymap-less override must pipe through `resolveChord`.
5. **D5 — `Mod+K` toggles (closes when open).** Linear parity; cheapest exit
   beside Escape.
6. **D6 — Seed `view.settings`/`view.backlog`/`view.done` registrations in the
   spine.** Action mode over `registry.all()` is otherwise three commands;
   `Mod+,` (`keymap.ts:66`) goes live for free. This is the ratified
   registration-beside-behavior pattern (`spine.ts:62-66`), purely additive —
   not scope-creep; an empty action mode would gut the palette. The newly
   live chord gets its production-wiring test in T1. `view.agentWorkspace`
   excluded (OQ-4). These seeds STAY under Matt's ratified
   commands-as-inventory rule (owned by RIG-2529,
   `compass-tier3-scope-gate/design.md`): registration-beside-behavior for
   real global commands is legitimate; see A3's shared-rule note.
7. **D7 — Point-of-use scope = seam + existing sites, not a menu retrofit.**
   Zero `.cx-menu`/`.cx-tooltip` consumers exist (grep evidence, A6); the chip
   primitive + the `PrimaryAction` contract note make future adoption
   mechanical.
8. **D8 — Host anatomy: the bare fixed wrapper (ratified, Matt 2026-08-23;
   was OQ-1; primitive re-pointed to Search per Matt after review, since
   Kobalte publishes this exact recipe for Search, not Combobox).** A
   permanently-open Kobalte **Search** primitive (`open` pinned true; Control +
   Input + Listbox, with `Search.Portal`/`Search.Content` replaced by a plain
   div per Kobalte's published command-menu recipe) inside a hand-positioned
   `position: fixed` div that IS `.cx-palette`, with our OWN
   `.cx-palette-backdrop`, Escape/outside-click dismiss, hand-wired focus grant
   (`ref` + `input.focus()` on mount), and D3's focus restore; NO Portal/Content
   — floating-ui stays out of the test path. Rationale (the decider): a
   DOM-only test path, with Kobalte only where it is load-bearing (the Search
   primitive's ARIA + keyboard traversal). Search over Combobox because Search
   is built for caller-managed filtering (our fuzzy match) and its docs publish
   the pinned-open/no-portal recipe verbatim
   (<https://kobalte.dev/docs/core/components/search/>, v0.13.13); the bare
   Combobox shape first specified relied on an unsupported extrapolation of
   that Search recipe. The Kobalte `Dialog` host moves to Alternatives
   considered. `@kobalte/core` itself stays settled-yes (DL-150 pre-authorizes).
9. **D9 — Navigation coverage: ALL six destination kinds ship, incl. issue
   AND pr (ratified, Matt 2026-08-23; was OQ-2).** The issue provider rides
   the tracker's assigned-issue seam (`store.assignedIssues`); the pr
   provider requires a NEW store-level PR accessor — `prs:
   Accessor<PrRow[]>` via `prRows(issues())` (`board.ts:128-134`) — which
   Matt explicitly accepted as added palette-impl scope (the store has no PR
   collection today; `IssueTab`'s `"pr"` is a detail-pane tab, not a
   collection). Contract pinned in A4/T3.
10. **D10 — Point-of-use part 2: seam + topbar now-win ship; menu/tooltip
    chips DEFERRED to RIG-2530 (ratified, Matt 2026-08-23; was OQ-3).** T4
    ships the seam (`ShortcutChip` + `shortcutFor` + the `PrimaryAction`
    contract note + populated `board.*` sites) AND populates
    `aria-keyshortcuts` + `title` on the existing topbar view-tab buttons
    (`App.tsx:63-77`) via `shortcutFor`. `.cx-menu`/hover-tooltip
    point-of-use is out of scope here — follow-up issue **RIG-2530**.

## Alternatives considered

- **Hand-rolled combobox instead of Kobalte** — rejected: combobox is
  a11y-hard (active-descendant, listbox semantics, dismiss layers), exactly
  DL-150's carve-out for Kobalte, and the spec names the host
  (`components.md:447`).
- **Palette as its own window keydown listener** — rejected outright: violates
  the one-keydown-path constraint (DL-222/223); the open chord rides tier 3.
- **`KeyboardContext` provider for palette state** — rejected for the same
  provider-churn reason DL-223 rejected it for the spine.
- **Default popover anatomy (`Portal > Content`, Search or Combobox)** —
  rejected: `.Input` lives in `Control` OUTSIDE the Portal, so the input would
  render at the App-root mount while the list portals to `document.body` at a
  popper-computed position — the shipped single-panel `.cx-palette`
  (`palette.css:1-6`, `components.md:441-445`) cannot be produced under it. A2
  uses Kobalte's published Search command-menu shape instead (`open` pinned,
  Portal/Content replaced by a div), which also keeps floating-ui out of the
  test path.
- **Search/Combobox hosted inside a Kobalte `Dialog`** — rejected (Matt
  2026-08-23, D8; formerly OQ-1's option b). Scrim, outside-click/Escape
  dismiss, focus-trap, and focus-restore would come free, but at the cost of
  two nested Kobalte primitives (modal dialog semantics around the search
  primitive), Dialog's own Portal/overlay layering to reconcile with the
  shipped `.cx-palette`/`.cx-palette-backdrop` CSS and `--cx-z-palette`, and a
  focus trap interacting with the dispatcher's window-level `Mod+K` toggle. The
  decider: the bare wrapper's hand-carried mechanics are small, fully specified
  (A2, D3), and keep the test path DOM-only.

## Open Questions

The three load-bearing forks this record carried (OQ-1 host anatomy, OQ-2
provider coverage, OQ-3 part-2 scope) are RESOLVED — ratified as Decisions
D8/D9/D10 (Matt, 2026-08-23). Only genuine residuals remain:

- **OQ-4 (non-load-bearing) — `view.agentWorkspace` registration.** Tabled at
  `keymap.ts:65` but needs a "current/last agent" the store doesn't expose;
  excluded from D6's seeds. Defer to whichever record introduces last-agent
  memory.
- **OQ-5 (non-load-bearing) — chord glyph prettification.** `ShortcutChip`
  renders resolved text (`Cmd+K`/`Ctrl+K`); mapping to `⌘K` on mac is a
  one-component change later. Assumption: plain text ships.
