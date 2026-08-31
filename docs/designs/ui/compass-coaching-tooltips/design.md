# Compass coaching keyboard-discoverability tooltips (RIG-2530)

Status: Draft

Parent: RIG-1661 (keyboard discoverability net). Refines DL-234 (point-of-use
chips, RIG-2483), which explicitly deferred the `.cx-menu`/hover-tooltip
adoption to this record. Forward-compatible with RIG-2484 (leader chords,
`compass-leader-chords`, PR #544 — submitted, unmerged), never blocked on it.

## Problem / Intent

Compass has a full keyboard surface (dispatcher, registry, palette, `?`
overlay) but the chrome teaches none of it: command-backed buttons carry only a
raw native `title=` (slow ~1s browser delay, unstyled, no chip), so a mouse
user never passively learns the keyboard path. Ship a real coaching Tooltip —
label + resolved chord chip on hover **and** focus, in the already-shipped
`.cx-tooltip` box — and adopt it across command-backed chrome: Linear's
signature discoverability move. Scope is the tooltip layer only; the wider
onboarding set (empty-state nudges, key-hint footer, first-run coach) is a
Matt-ratified deferral (see Deferred).

## Global Constraints

- **SolidJS v2** (`apps/ui/package.json:26` — `"solid-js": "^2.0.0-rc.1"`;
  `:19` — `"@solidjs/web": "^2.0.0-rc.1"`). v2 is a foundational rework: no v1
  idioms (`produce`, `createResource`, `<Suspense>`, `batch`,
  `solid-js/store`, `mergeProps`/`splitProps`). Component props are NEVER
  destructured (severs reactivity — `skill://solid-skills` TTSR rule
  `solid-no-destructured-props`). `ShortcutChip.tsx:14-17` is the in-tree v2
  shape to match (non-destructured `props`, thunked derivation).
- **Kobalte `2.0.0-alpha.0`** (`apps/ui/package.json:15` —
  `"@kobalte/core": "2.0.0-alpha.0"`). Kobalte is scoped to a11y-hard behavior
  only (DL-150, `docs/designs/product/DECISIONS.md:271`); all visuals via our
  `.cx-*` classes. The v2-alpha Tooltip API used here is grounded from the
  installed types (see Approach A1), never the 0.13 docs.
- **Chord source of truth is the keymap.** Every displayed chord resolves via
  `shortcutFor(id, platform)` and every AT-parseable chord via
  `shortcutForAria(id, platform)` (`apps/ui/src/keyboard/keymap.ts:57-76`) —
  "a registration never hand-authors a `shortcut` string" (`keymap.ts:53-55`,
  DL-234's single-derivation rule). This record NEVER hand-authors a chord.
- **Reuse `.cx-tooltip`** (`apps/ui/src/design/components/tooltip.css:6-19`)
  and the `--cx-tooltip-delay: 400ms` token
  (`apps/ui/src/design/tokens.css:227`). No new visual tokens.
- **Command ids `noun.verbCamel`**; the registry contract stays as-is
  (`commands.ts:113-121`). This record registers exactly TWO commands —
  `sidebar.toggleLeft`/`sidebar.toggleRight` — beside their existing store
  behavior (`store.ts:328/331`), because their chords are declared in the
  keymap (`keymap.ts:116-117`) yet registered nowhere, so they are dead today
  and coaching them would teach a lie (see A4/T2). This is DL-229-compliant:
  the behavior already exists (`store.toggleLeft/toggleRight`), so the command
  is registered beside a behavior, not purely for discoverability.
- **Tests**: `cd apps/ui && bun test --conditions browser <files>`; component
  tests via `@solidjs/testing-library@1.0.0-beta.2`
  (`apps/ui/package.json:36`). Red → green per `rule://red-green-testing`.
- **Ledger**: the driver folds `DECISIONS.md` in the same PR; new rows are
  DL-245..247 (PR #544's leader-chord block was renumbered to DL-248..252
  after a concurrent merge, so it no longer overlaps). This record refines
  DL-234; no row is superseded.
- **Docs gate**: markdownlint (`.markdownlint.json`) — blank lines around
  every fence, space-padded table delimiter rows, dash bullets.

## Approach

One reusable coaching Tooltip built on the Kobalte v2-alpha `Tooltip`
primitive, styled by the existing `.cx-tooltip` box, whose content is the
control's label plus its keymap-resolved chord; then a mechanical adoption
sweep that converts the native `title=` on command-backed controls (removing
the `title` so no double tooltip) and leaves every other `title=` native.

### A1 — Kobalte v2-alpha Tooltip, not hand-rolled

The `.cx-tooltip` CSS was authored FOR Kobalte from day one:

> `tooltip.css:1-2`: "Tooltip — .cx-tooltip (D3, Kobalte). Elev-1 float, open
> delay --cx-tooltip-delay (the delay is Kobalte's timing prop — this owns the
> visual box)."

and the component spec agrees (`apps/ui/src/design/components.md:458-460` —
"**Class:** `.cx-tooltip` … open delay `--cx-tooltip-delay` (400ms) is
Kobalte's timing prop"). A tooltip is squarely DL-150's "a11y-hard behavior"
(hover+focus open, delay/skip-delay timing, Escape dismiss, safe-area
pointer-travel, `aria-describedby` wiring) — the class of thing we adopt
Kobalte for, unlike the ShortcutsOverlay's modal chrome which D5 ratified
hand-rolled (`ShortcutsOverlay.tsx:6-7` — "Hand-rolled modal on the
`.cx-dialog` convention … no @kobalte/core"). The palette already ships the
same alpha (`Palette.tsx:19` — `import { Search } from
"@kobalte/core/search";`), so the version risk is already on the books.

The installed v2-alpha API (grounded from
`apps/ui/node_modules/@kobalte/core/dist/tooltip/index.d.ts` re-exporting
`dist/index/CDsbCLm32.d.ts`) is the familiar four-part anatomy, and it gives
us every behavior the coaching layer needs out of the box:

- `Tooltip` (Root) — `TooltipRootOptions` (`CDsbCLm32.d.ts:65-101`) carries
  `openDelay?: number` (`:82-83`), `closeDelay`, `skipDelayDuration`, and
  `triggerOnFocusOnly?: boolean` with the default we want: "By default, opens
  for both focus and hover" (`:77-81`). Root doc: "A popup that displays
  information related to an element when the element receives keyboard focus
  or the mouse hovers over it" (`:103-106`).
- `Tooltip.Trigger` — polymorphic, `declare function TooltipTrigger<T extends
  ValidComponent = "button">` (`:127`), and its render props include
  `"aria-describedby": string | undefined` (`:120-122`) — the ARIA wiring is
  automatic.
- `Tooltip.Content` — render props carry `role: "tooltip"`
  (`:48-50`); we put `class="cx-tooltip"` on it.
- `Tooltip.Portal` — portals content to `body` (`:58-62`).

So the WAI-ARIA bar (tooltip role + `aria-describedby` + focus reveal +
Escape dismiss) is met by the primitive, and our component only owns content
and styling. `openDelay` is passed as `400` — the number mirrored by the
`--cx-tooltip-delay: 400ms` token (`tokens.css:227`), per the tooltip.css
comment that the delay "is Kobalte's timing prop — this owns the visual box"
(the token documents the value; Kobalte's number prop enforces it — see D5).

### A2 — Component shape: composable trigger, encapsulated content

Solid v2's reactive prop helpers are `omit`/`merge` (v1's `splitProps`/
`mergeProps` are gone), so a monolithic `<CoachTip {...allButtonProps}>`
wrapper that forwards a subset of props to the trigger is buildable
v2-cleanly via `omit` (Kobalte's own Tooltip does exactly this internally).
It is nonetheless the wrong shape. The three-part API instead mirrors
Kobalte's own anatomy, keeps call sites authoring their own trigger element
(same class/onClick/aria attributes they have today), and avoids a bespoke
prop-forwarding contract — the component encapsulates only what is new:

```tsx
// apps/ui/src/components/CoachTip.tsx
import { Tooltip } from "@kobalte/core/tooltip";

/** Root with the house 400ms open delay; hover+focus reveal is Kobalte's
 *  default (triggerOnFocusOnly stays unset). */
export const CoachTip: Component<ParentProps<{ openDelay?: number }>>;

/** Re-export: the call site's existing <button> becomes
 *  <CoachTipTrigger as="button" …same attributes…>. */
export const CoachTipTrigger = Tooltip.Trigger;

/** The coaching content: label + resolved chord. Chord resolution:
 *  props.chord if given, else shortcutFor(props.command, detectPlatform()).
 *  undefined chord → label-only tooltip (e.g. view.backlog has no keymap
 *  row yet — LeftSidebar.tsx:429-430). */
export const CoachTipContent: Component<{
  label: string;
  command?: CommandId;
  /** Explicit chord override; NEVER hand-authored at call sites — reserved
   *  for tests and future non-registry chords. */
  chord?: string;
  class?: string;
}>;
```

`CoachTipContent` renders `<Tooltip.Portal><Tooltip.Content
class="cx-tooltip">` containing the label text and, right of it, the chord —
via `<ShortcutChip chord={…}>` (`ShortcutChip.tsx:14-23`) exactly as the
`PrimaryAction` contract prescribes: "a menu item or tooltip rendering a
`PrimaryAction` resolves its command and renders `<ShortcutChip>`
right-aligned … The chord comes from `shortcutFor(command, platform)`
(keymap.ts), never a hand-authored string" (`commands.ts:97-100`). This is the
first consumer of that frozen contract. Platform comes from `detectPlatform()`
(`dispatch.ts:81-84`), matching every existing chip site (`App.tsx:64`,
`LeftSidebar.tsx:431`, `Palette.tsx:86`).

`ShortcutChip`'s own header anticipates exactly this host: "The `class` prop
lets a future `.cx-menu-item`/`.cx-tooltip` host (RIG-2530) restyle the box
while reusing the same split rendering" (`ShortcutChip.tsx:5-6`).

### A3 — RIG-2484 forward-compat: sequence-aware chord rendering

`ShortcutChip` splits on `"+"` (`ShortcutChip.tsx:17` — `const keys = () =>
props.chord.split("+");`), so once PR #544's impl lands and `shortcutFor`
returns a formatted sequence like `"G then B"` (no `"+"`), the chip would
render it as one giant `<kbd>G then B</kbd>` — wrong. `CoachTipContent`
therefore branches on the resolved string: a chord containing no `" then "`
renders through `<ShortcutChip>`; a sequence renders as plain text in the
chip's typographic style (see D3). Because the component only ever reads
`shortcutFor`'s return value, it shows `Ctrl+B`/`Cmd+B` today and `G then B`
automatically after #544 merges — no dependency on the merge order.

The `" then "` separator is not a guess: it is #544's ratified display
contract — its exported `formatChordForDisplay` emits a sequence as `"G then B"`
(leader-chords record §A5; DL-251, formerly DL-243). #544 exports the
**function**, not a separator token, so the coaching impl reuses that function
as the source of truth: it renders the chord string `formatChordForDisplay`
already produced, and its `CoachTipContent` sequence branch keys off that
rendered string (a value containing `" then "`, per the DL-251 format
contract) rather than re-deriving the chord. Because both records drive off the
one exported formatter, the branch here and #544's output cannot silently
diverge; the T1 test (below) asserts the observable rendering of a
`formatChordForDisplay` sequence output, so a format change in #544 surfaces as
a failing test, never a giant `<kbd>`.

### A4 — Adoption boundary: the `title=` classification

A native `title` and a custom tooltip on the same element double-tooltip, so
every converted control DROPS its `title=` (the `aria-keyshortcuts` attribute
from DL-234 stays — it is the AT-parseable chord, orthogonal to the visual
tooltip). The full `title=` census of `apps/ui/src` (grep against main tip `fb234f33`),
classified:

**Convert — command-backed (the coaching sweep):**

| Site | Command | Evidence |
| --- | --- | --- |
| `LeftSidebar.tsx:447-449` | `view.bridge` | `title={ chord("view.bridge") ? \`Bridge (${chord("view.bridge")})\` : undefined }` |
| `LeftSidebar.tsx:462-466` | `view.backlog` | same pattern; no keymap row yet → label-only tooltip (`LeftSidebar.tsx:429-430` — "view.backlog/view.done have no keymap row yet, so shortcutFor is undefined") |
| `LeftSidebar.tsx:479` | `view.done` | `title={chord("view.done") ? \`Done (${chord("view.done")})\` : undefined}`; label-only until a row exists |
| `LeftSidebar.tsx:491-495` | `view.settings` | same pattern; `Mod+,` (`keymap.ts:104`) |
| `App.tsx:88` | `view.bridge` | `title={bridgeChord ? \`Bridge (${bridgeChord})\` : undefined}` (topbar Bridge tab) |
| `App.tsx:125` | `sidebar.toggleLeft` | `title="Toggle left sidebar"`; chord `Mod+Shift+\` (`keymap.ts:117`). **Dead today** — declared in the keymap, registered nowhere; the button calls `store.toggleLeft()` directly (`App.tsx:126`). T2 registers the command so the coached chord fires. **Glyph-only** (content is `▐`, no text label) → T2 adds `aria-label="Toggle left sidebar"` so the trigger keeps an accessible name once `title=` is dropped (A5). |
| `App.tsx:133` | `sidebar.toggleRight` | `title="Toggle right sidebar"`; chord `Mod+\` (`keymap.ts:116`). **Dead today** — same as above; T2 registers `sidebar.toggleRight` beside `store.toggleRight()` (`App.tsx:134`). **Glyph-only** (content is `▌`) → T2 adds `aria-label="Toggle right sidebar"` (A5). |

**Keep native — actionable but no registered command** (a command is
"registered beside its behavior, never purely for discoverability" — DL-229,
`DECISIONS.md:300` — so these gain nothing to coach until a real command
exists): `LeftSidebar.tsx:67` (pin/unpin), `:225` + `:292` (disabled
subscribe/join), `:438` (new folder), `AgentView.tsx:74,84,93,103,230,261,274`
(pane focus/split/close, share, tabs), `LogPanel.tsx:78,104` (stop,
minimize/expand), `RightSidebar.tsx:298,336,379,443` (open agent, repo/branch
dropdowns, open workspace), `ChannelView.tsx:132` (ask submit). See D4 and Deferred for
the label-only-tooltip deferral.

**Keep native — plain text / truncation / status** (not controls, or the
`title` restates truncated/decorative content): `LeftSidebar.tsx:50,58,214,343`
(role pip, activity, always-subscribed glyph, shared badge),
`RightSidebar.tsx:289,324,673` (truncated issue title, repo label, tab title),
`LogPanel.tsx:66` (running/idle status), `ChannelView.tsx:376` (avatar
handle), `StateDot.tsx:23` (`title={label()}` beside `aria-label` on a
`role="img"` span). `BacklogView.tsx:100,105,110` are component props named
`title`, not HTML attributes — out of the census.

### A5 — Reveal semantics

Hover AND focus, Kobalte's default (`CDsbCLm32.d.ts:77-81` —
`triggerOnFocusOnly` "By default, opens for both focus and hover"). A
coaching layer that only shows on mouse-hover is self-defeating: the keyboard
user tabbing the chrome is exactly who benefits from seeing the direct chord.
`triggerOnFocusOnly` stays unset; no per-site override.

A button with a visible **text** label already carries that label as its
accessible name, so a screen reader announces the label twice (name +
description) plus the chord — standard tooltip-pattern redundancy, no extra
wiring owed. This holds for five of the seven converted sites (the four
`LeftSidebar` view buttons and the `App.tsx` Bridge tab all wrap visible
text). It does **not** hold for the two glyph-only sidebar toggles
(`App.tsx:125/133`): their only content is a decorative block glyph (`▐`/`▌`),
so today `title=` is serving as the accessible **name**, not a description.
Dropping `title=` there without a replacement would leave the button named by
the meaningless glyph. So those two triggers get an explicit
`aria-label="Toggle left sidebar"` / `"Toggle right sidebar"` (the
human-readable string moves into the accessible name), independent of the
tooltip description. The AT-authoritative chord stays `aria-keyshortcuts`
(DL-234), not the tooltip text.

## Alternatives considered

### Native `title=` enrichment only

Keep the DL-234 `title` strings and just append chords everywhere. Rejected:
the native tooltip is unstyled, has a fixed ~1s UA delay (vs the house 400ms
token), cannot render a `<kbd>` chip or any `.cx-*` styling, and is
suppressed entirely on touch and on focus in most browsers — it coaches
nobody. It is what we have today, and it is not "like Linear".

### Hand-rolled tooltip on `.cx-tooltip`

The ShortcutsOverlay precedent (`ShortcutsOverlay.tsx:6-10`) hand-rolls its
modal, so a hand-rolled tooltip (CSS `:hover`/`:focus-visible` reveal or a
small positioned component) is plausible. Rejected: the overlay is a modal —
its hard parts (focus trap/restore) are small and self-contained — while a
tooltip's hard parts are exactly the a11y-hard behaviors DL-150 scopes Kobalte
to (delay + skip-delay timing across adjacent triggers, safe-area pointer
travel, Escape dismiss, portal stacking, `aria-describedby` id plumbing,
hover+focus unification). The `.cx-tooltip` CSS itself was authored naming
Kobalte as the behavior owner (`tooltip.css:1-2`), and the alpha is already a
shipped dependency via the palette's Search (`Palette.tsx:19`). Re-deriving
that behavior by hand is a second convention beside a ratified one.

### A parallel chord formatter for tooltips

A tooltip-local "pretty" formatter (e.g. mapping to `⌘B` glyphs). Rejected:
DL-234's single-derivation rule — every chip renders `shortcutFor`'s output,
"no hand-authored shortcut strings" (`DECISIONS.md:305`), and the mac-glyph
question is already parked as a one-component change inside `ShortcutChip`
(`ShortcutChip.tsx:8-9`). A second formatter would fork the derivation this
net exists to keep single.

### Monolithic wrapper via `omit`

A single `<CoachTip>` that wraps the trigger and forwards passthrough props
with Solid v2's `omit` (the reactive successor to v1's `splitProps`). Buildable
v2-cleanly — but rejected on composition: it must own a prop-forwarding
contract for every trigger variant (button, link, div), re-implements the
split Kobalte already exposes as `Tooltip.Trigger`, and hides the call site's
own element behind a passthrough. The three-part API keeps each call site
authoring its real trigger and matches Kobalte's own anatomy (A2).

## Plan

### T1 — `CoachTip` component (Kobalte Tooltip + `.cx-tooltip` + chord content)

New file `apps/ui/src/components/CoachTip.tsx` (plus importing
`../design/components/tooltip.css`), per A1/A2/A3.

Interfaces:

```tsx
import { Tooltip } from "@kobalte/core/tooltip";
import type { Component, ParentProps } from "solid-js";
import type { CommandId } from "../keyboard/commands";

/** House open delay, mirrors --cx-tooltip-delay (tokens.css:227). */
export const COACH_TIP_DELAY_MS = 400;

/** Kobalte Tooltip root with openDelay defaulted to COACH_TIP_DELAY_MS;
 *  hover+focus reveal (Kobalte default, triggerOnFocusOnly unset). */
export const CoachTip: Component<ParentProps<{ openDelay?: number }>>;

/** The trigger — Kobalte's polymorphic Trigger, re-exported so call sites
 *  author <CoachTipTrigger as="button" type="button" class=… onClick=…>
 *  with their existing attributes. */
export const CoachTipTrigger: typeof Tooltip.Trigger;

/** Portal + Content(class="cx-tooltip") rendering `label`, then the chord:
 *  chord = props.chord ?? shortcutFor(props.command, detectPlatform());
 *  undefined → label only; contains " then " → plain-text sequence;
 *  otherwise <ShortcutChip chord={chord}>. Never destructures props. */
export const CoachTipContent: Component<{
  label: string;
  command?: CommandId;
  chord?: string;
  class?: string;
}>;
```

Red → green (component tests, `apps/ui/src/components/CoachTip.test.tsx`,
`@solidjs/testing-library`, `cd apps/ui && bun test --conditions browser
src/components/CoachTip.test.tsx`):

- label + chord render: `command="view.bridge"` on platform `other` shows
  "Bridge" and a chip whose `<kbd>` split is `["Ctrl","B"]` (via
  `DEFAULT_KEYMAP` `keymap.ts:102`, never a hand-authored expectation string).
- aria wiring: focus the trigger (Kobalte opens on focus immediately,
  independent of `openDelay`), assert content has `role="tooltip"` and the
  trigger's `aria-describedby` points at it.
- focus reveal: tooltip opens on trigger focus without any pointer event.
- label-only: `command="view.backlog"` (no keymap row) renders label, no chip.
- sequence handling: `chord="G then B"` renders plain text, no `<kbd>` split;
  `chord="Ctrl+B"` renders the two-`<kbd>` chip. The `"G then B"` fixture is
  #544's `formatChordForDisplay` sequence output (the `" then "` format
  contract, DL-251) — the test asserts the observable rendering of that output,
  not a hand-picked literal, so a format change in #544 surfaces here as a
  failing test rather than silently mis-rendering.

### T2 — Adoption sweep: convert the command-backed `title=` sites

First, register the two dead commands so their coached chords fire (Matt-ruled,
2026-08-23): in `createKeyboardSpine`'s deps or beside the existing view
registrations (`spine.ts:79-126`), `registry.register` `sidebar.toggleLeft` →
`store.toggleLeft()` and `sidebar.toggleRight` → `store.toggleRight()`
(`store.ts:328/331`), so `Mod+Shift+\`/`Mod+\` (`keymap.ts:116-117`) dispatch
instead of falling through. The `App.tsx` buttons keep their direct
`onClick={() => store.toggleLeft()}` (`App.tsx:126/134`) — the registration
adds the keyboard path, it does not change the click path.

Then convert the seven A4 "convert" rows: each `<button title=…>` becomes
`<CoachTip><CoachTipTrigger as="button" …existing attributes, title
removed…>…</CoachTipTrigger><CoachTipContent label=… command=… /></CoachTip>`.
`aria-keyshortcuts` stays. `App.tsx:125/133` gain their (now-live) chord
coaching for the first time (labels "Toggle left sidebar"/"Toggle right
sidebar", commands `sidebar.toggleLeft`/`sidebar.toggleRight`). Those two are
glyph-only, so their `CoachTipTrigger` also gets an explicit
`aria-label="Toggle left sidebar"` / `"Toggle right sidebar"` — the
human-readable name must survive dropping `title=` (A5). No other `title=` in
the tree is touched.

Interfaces: consumes T1's exports; files touched: `apps/ui/src/App.tsx`,
`apps/ui/src/components/LeftSidebar.tsx`,
`apps/ui/src/keyboard/spine.ts` (the two registrations).

Red → green (`cd apps/ui && bun test --conditions browser
src/components/LeftSidebar.test.tsx src/App.test.tsx` — extend the existing
suites where present, else colocated new files):

- the Bridge view button opens a coaching tooltip on focus showing "Bridge" +
  the `shortcutFor("view.bridge", platform)` chord.
- converted controls carry NO `title` attribute (double-tooltip regression
  guard) and still carry `aria-keyshortcuts`.
- a keep-native control (e.g. `LeftSidebar.tsx:438` new-folder) still has its
  native `title` — the sweep boundary held.
- accessible name: every converted trigger exposes a non-glyph accessible
  name — the five text-labelled buttons via their visible text, and the two
  glyph-only sidebar toggles (`App.tsx:125/133`) via the added `aria-label`
  (asserts the toggle is not left named by the bare `▐`/`▌` glyph once `title=`
  is dropped).
- coached-chord dispatch: every command id the sweep coaches resolves in the
  command registry AND its chord fires when pressed (table-driven, mirroring
  `keyboard-e2e.test.tsx`) — guards display-path (keymap) vs dispatch-path
  (registry) drift, the failure the A4 boundary must avoid. Both sidebar
  toggles are LIVE here (T2 registers them), so their coached chord actually
  fires.

### T3 — Docs + ledger follow-through

- Update `apps/ui/src/design/components.md` §Tooltip (`:458-462`) to note the
  shipped consumer (`CoachTip`) and the label+chord content contract.
- Ledger delta rides the SAME PR (driver-folded): DL-245 (Kobalte-v2-alpha
  tooltip adoption), DL-246 (sequence-aware chord rendering via `shortcutFor`),
  DL-247 (the convert/keep `title=` boundary + the two sidebar-toggle
  registrations) — all Active, refining DL-234, no row superseded (the
  DL-240..244 block first drafted for PR #544 was renumbered to DL-248..252
  when a concurrent merge took DL-240, so these three sit past it).
- Sweep code comments citing RIG-2530 as future (`ShortcutChip.tsx:5-6`,
  `keymap.ts` if any) to cite the shipped component instead.

Interfaces: none new; docs only. Gate: markdownlint on touched docs;
`cd apps/ui && bun test --conditions browser` for the affected suites plus
Biome format/lint per repo convention.

## Tasks

- [ ] T1: `CoachTip.tsx` (`CoachTip`/`CoachTipTrigger`/`CoachTipContent`,
  Kobalte v2-alpha Tooltip, `.cx-tooltip`, `shortcutFor` resolution,
  sequence-aware rendering) + red→green component tests.
- [ ] T2: Register `sidebar.toggleLeft`/`sidebar.toggleRight` (`spine.ts`)
  so their chords fire, then the adoption sweep of the seven command-backed
  `title=` sites (`LeftSidebar.tsx` ×4, `App.tsx` ×3), `title` removed,
  `aria-keyshortcuts` kept + red→green adoption + coached-chord-dispatch tests.
- [ ] T3: `components.md` tooltip-section update; ledger delta noted for the
  driver (DL-245+, refines DL-234); comment sweep.

## Resolved decisions

Matt ruled the load-bearing fork (2026-08-23); the remaining recommendations
are adopted as written.

- **D1 — dead sidebar chords (fork, Matt-ruled).** The two topbar
  sidebar-toggle buttons (`App.tsx:125/133`) have keymap rows
  (`keymap.ts:116-117`) but no registration, so `Mod+\`/`Mod+Shift+\` are dead
  today and coaching them would teach a lie. **Decision: register the two
  commands beside their existing store behavior** (`store.toggleLeft/Right`)
  and coach all seven sites (A4/T2). DL-229-compliant (behavior exists, not
  registered purely for discoverability); amends the "registers nothing new"
  constraint to "registers exactly these two". Rejected alternative: shrink the
  sweep to five live-chord sites and leave the toggles uncoached — defers a
  real drift this net owns.
- **D2 — Kobalte v2-alpha Tooltip, not hand-rolled (A1).** The `.cx-tooltip`
  CSS names Kobalte as the behavior owner (`tooltip.css:1-2`), the behavior set
  is DL-150's "a11y-hard" scope, and the alpha already ships via the palette's
  Search (`Palette.tsx:19`). Residual risk: `2.0.0-alpha.0` API churn before a
  stable cut — bounded because the dependency is already pinned and shipped; a
  breakage would hit the palette first and equally.
- **D3 — sequence chords render as plain text, plus-chords through
  `ShortcutChip` (A3).** When `shortcutFor` returns a RIG-2484 sequence
  (`"G then B"`, no `"+"`), `ShortcutChip.split("+")` would render one giant
  `<kbd>` (`ShortcutChip.tsx:17`). `CoachTipContent` branches: no `" then "` →
  `ShortcutChip`; sequence → plain text. The `" then "` separator is #544's
  ratified format contract (DL-251, §A5), not a local guess; #544 exports the
  `formatChordForDisplay` function (not a separator token), so the coaching
  impl reuses that function's output as the source of truth and branches on the
  rendered value rather than re-deriving the separator. Teaching `ShortcutChip`
  itself to split on `" then "` is deferred to the RIG-2484 impl (it re-opens a
  shipped primitive ahead of #544's merge).
- **D4 — `title=` boundary: convert only the command-backed sites (A4).**
  Actionable command-less controls and truncation/status `title`s stay native;
  a control with no command has no chord to coach, and DL-229 forbids
  registering commands purely for discoverability.
- **D5 — `COACH_TIP_DELAY_MS = 400` constant, not a runtime token read.**
  Kobalte's `openDelay` is a number prop; the token `--cx-tooltip-delay: 400ms`
  (`tokens.css:227`) is CSS. A constant with a comment binding it to the token
  is enough (the tooltip.css header already ratifies "the delay is Kobalte's
  timing prop"); a `getComputedStyle` read is over-engineering.

## Deferred (not open work)

- **Label-only `CoachTip` on command-less actionable chrome** (`AgentView`
  pane buttons, `LogPanel` stop, `RightSidebar` dropdowns, …): visually
  consistent styled tooltips, no chord. Adopt opportunistically as those
  controls gain registered commands; the component already supports label-only.

The wider onboarding affordances — empty-state keyboard nudges, a persistent
key-hint footer, a first-run coach — are explicitly deferred by Matt:
"coaching to start, we can follow up with the full [set] after we get a feel
for the best ways to do that, and if we end up modifying some keys." A future
record picks these up; nothing here designs them.
