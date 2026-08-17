# Compass component visual contracts (D3)

This document plus the CSS in `design/components/` is the **component visual
contract**: the class vocabulary, the `data-*` variant modifiers, the states
each component renders, and the `--cx-*` semantic tokens it consumes. It is the
human-readable contract `compass-ui` implements against.

This side owns **what renders**. SolidJS props, reactivity, and
`@compass/client` wiring are `compass-ui`'s side of the seam and are not
specified here.

Conventions across the set:

- Class + `data-*` variant modifiers, mirroring the existing codebase
  convention (`.state-dot[data-state=…]`, `data-status`, `data-priority`).
- Every interactive component renders six mandatory, uniform states: rest,
  hover, active, selected, disabled, focus. Focus is always
  `:focus-visible` → `--cx-focus-ring`; hover uses `--cx-bg-hover`; disabled is
  45% opacity + `cursor: default` (never a color swap that breaks status
  semantics).
- Three surface tiers: cards, rows, and list cells render on `--cx-bg-panel`;
  selection is carried by `--cx-bg-selected` + the accent left rule, never a
  raised background.
- Every value is a `--cx-*` semantic or scale token — no raw hex, no `--rigel-*`,
  no literal duration/easing/z-index (enforced by `cx-token-gate`).

## Button

- **Class:** `.cx-btn`
- **Variants:** `data-variant="primary | ghost | danger"`,
  `data-size="sm | md"`, `data-selected` (toggle/segmented).
- **States:** rest / hover (`--cx-bg-hover`) / active (`--cx-bg-active`) /
  selected (`--cx-bg-selected` + accent border) / disabled (45%) / focus
  (`--cx-focus-ring`).
- **Tokens:** `--cx-bg-panel`, `--cx-bg-hover`, `--cx-bg-active`,
  `--cx-bg-selected`, `--cx-accent`, `--cx-accent-hover`, `--cx-accent-muted`,
  `--cx-error`, `--cx-border`, `--cx-text`, `--cx-text-bright`, `--cx-bg`,
  `--cx-font-ui`, `--cx-text-xs/-sm`, `--cx-space-1/-2/-3`, `--cx-radius-md`,
  `--cx-motion-fast`, `--cx-ease-out`, `--cx-focus-ring`.

Primary is an accent fill; ghost is borderless with a hover wash; danger uses
`--cx-error`.

## Input / Search / Select / Composer

- **Classes:** `.cx-input`, `.cx-search`, `.cx-select`, `.cx-composer`.
- **States:** rest / hover / active (select) / selected (`data-selected`) /
  disabled (45%) / focus. Focus swaps the border to `--cx-border-focus` and
  renders `--cx-focus-ring`.
- **Tokens:** `--cx-bg-panel`, `--cx-bg-hover`, `--cx-bg-active`,
  `--cx-border`, `--cx-border-focus`, `--cx-accent`, `--cx-text`,
  `--cx-text-bright`, `--cx-text-faint`, `--cx-font-ui`, `--cx-text-sm`,
  `--cx-space-2/-3/-6/-8`, `--cx-radius-md`, `--cx-motion-fast`,
  `--cx-ease-out`, `--cx-focus-ring`.

The composer is multi-line and grows to a `40vh` cap; Enter / Shift-Enter is
`compass-ui` behaviour (this owns the visual box).

## Card

- **Class:** `.cx-card` · `data-selected`, `data-disabled`.
- **States:** rest / hover / active / selected (`--cx-bg-selected` + accent
  left rule) / disabled (45%) / focus.
- **Tokens:** `--cx-bg-panel`, `--cx-bg-hover`, `--cx-bg-active`,
  `--cx-bg-selected`, `--cx-accent`, `--cx-border`, `--cx-text`,
  `--cx-text-bright`, `--cx-font-ui`, `--cx-text-sm`, `--cx-space-3`,
  `--cx-radius-md`, `--cx-motion-fast`, `--cx-ease-out`, `--cx-focus-ring`.

Issue / PR / backlog rows on the panel tier; selection is the selection surface
plus the accent left rule, never a raised background.

## Badge / Pip / Chip

- **Classes:** `.cx-badge` · `data-status`, `.cx-pip`, `.cx-chip`.
- **Badge** (display): `data-status` maps CI (`ci-pass | ci-fail |
  ci-pending`) and review (`review-approved | review-changes |
  review-pending`) to `--cx-ci-*` / `--cx-review-*` color + border.
- **Pip** (display): unread-count / attention indicator on `--cx-accent`.
- **Chip** (interactive): tracker chip with all six states.
- **Tokens:** `--cx-bg-panel`, `--cx-bg-hover`, `--cx-bg-active`,
  `--cx-bg-selected`, `--cx-accent`, `--cx-border`, `--cx-text`,
  `--cx-text-dim`, `--cx-text-bright`, `--cx-ci-pass/-fail/-pending`,
  `--cx-review-approved/-changes/-pending`, `--cx-bg`, `--cx-font-ui`,
  `--cx-text-xs`, `--cx-space-1/-2/-4/-5`, `--cx-radius-sm`,
  `--cx-motion-fast`, `--cx-ease-out`, `--cx-focus-ring`.

### Axis badge — `.cx-axis-badge` (SEA-2117 / SEA-2121, shipped variant)

- **Classes:** `.cx-axis-badge` · `data-axis` (`ci | review`) · `data-status`
  · `data-compact`; contains `.cx-axis-code` (the `CI` / `RV` mono label) and a
  9×9 `.glyph` SVG.
- **What it is:** the frozen Option B — a fixed 2-char axis code in the mono UI
  face followed by a 9×9 1-bit pixel-art status glyph. The wrapper's
  `data-axis`+`data-status` is the single source of truth: it sets `color`,
  which paints both the code text and (via `currentColor`) the glyph fill. The
  inner glyph SVG carries NO own `data-status`/`data-verdict` — it selects its
  shape off the wrapper via CSS descendant selectors.
- **Statuses:** CI `success | pending | failure`; review `approved | changes |
  commented`.
- **Color routing:** `[data-axis="ci"][data-status="success"]` →
  `--cx-ci-pass`, `pending` → `--cx-ci-pending`, `failure` → `--cx-ci-fail`;
  `[data-axis="review"][data-status="approved"]` → `--cx-review-approved`,
  `changes` → `--cx-review-changes`, `commented` → `--cx-review-pending`.
- **Compact:** `.cx-axis-badge[data-compact] .cx-axis-code { display: none; }`
  — the glyph-only fallback for cramped surfaces (IssueCard); Bridge / Done
  rows show the code.
- **Geometry:** each glyph is a 9×9 1-bit grid, `shape-rendering="crispEdges"`,
  one `<rect width="1" height="1" fill="currentColor">` per lit cell.
- **Tokens:** `--cx-ci-pass/-pending/-fail`,
  `--cx-review-approved/-changes/-pending`, `--cx-font-ui`, `--cx-text-xs`,
  `--cx-space-1`.

#### The six axis-badge glyphs (canonical 9×9 grids)

`#` = lit cell, `.` = off; one CSS px per cell.

`ci-success` — check tick (reuses the state-dot `done` grid):

```text
.........
.........
.........
........#
.......#.
#.....#..
.#...#...
..#.#....
...#.....
```

`ci-pending` — ellipsis (three 2×2 dots, "running…"):

```text
.........
.........
.........
.........
##.##.##.
##.##.##.
.........
.........
.........
```

`ci-failure` — full X cross:

```text
#.......#
.#.....#.
..#...#..
...#.#...
....#....
...#.#...
..#...#..
.#.....#.
#.......#
```

`review-approved` — a thick/bold check (the "extra-affirmed" tick):

```text
.........
.........
........#
.......##
##....##.
.##..##..
..####...
...##....
.........
```

`review-changes` — delta (hollow triangle, the change mark):

```text
.........
....#....
....#....
...#.#...
...#.#...
..#...#..
..#...#..
.#######.
.........
```

`review-commented` — speech bubble with tail:

```text
.........
.#######.
.#.....#.
.#.....#.
.#######.
...#.....
..#......
.........
.........
```

#### Back-port to `rigel.build` (follow-up, Q5)

The public `rigel.build` DS docs still show the earlier flat-dot badge form.
Porting this axis-badge variant (the code + 9×9 glyph grids above) to that
surface is tracked as a follow-up, out of scope for the Compass adoption
(SEA-2117 Q5). Until then, the grids here are canonical.

## State dot

- **Class:** `.cx-state-dot` · `data-state`, `data-alive="1"` (working only).
- **States (agent process axis):** `working | idle | waiting | done | paused |
  stopped | error | disconnected` — the eight `AgentState` values, keyed 1:1
  against the frozen brand vocabulary.
- **Geometry:** a 9×9 1-bit bitmap grid, `shape-rendering="crispEdges"` (no
  anti-aliasing). Glyph geometry is fixed by the grids below; `compass-ui`
  emits identical inline SVG.
- **Color:** per-state via `--cx-st-*` (resolved through `currentColor`).
- **Motion:** only `working` animates — the working pulse at
  `--cx-pulse-period` in the working-state green `--cx-st-working`, **never**
  the purple phosphor `--cx-pulse-color`. The other seven are static,
  distinguished by glyph + color alone (CVD-safe, reduced-motion-safe).
- **Tokens:** `--cx-st-working/-idle/-waiting/-done/-paused/-stopped/-error/
  -disconnected`, `--cx-pulse-period`, `--cx-ease-out`.

### The eight frozen state glyphs (canonical 9×9 grids)

`#` = lit cell, `.` = off. The dot box is 9px (one CSS px per grid cell —
razor-crisp per the brand 1-bit whole-cell rule); it sits within the ~12px
agent-row height. Each grid reads correctly at that size (the reason `working`
is a double-chevron, not a ring — `brand state-icons.md`).

**Open question (for Matt):** Render size reconciliation: the frozen record T3
phrases this as "the 12px row-dot render size"; shipped as a 9px crisp dot in a
12px row. Confirm the 9px dot box is the intended shipped size (vs an 18px
integer-multiple).

`working` — double-chevron `»` (fast-forward); the ONLY animated state
(`--cx-st-working`):

```text
.........
#...#....
.#...#...
..#...#..
...#...#.
..#...#..
.#...#...
#...#....
.........
```

`idle` — 3×3 block (`--cx-st-idle`):

```text
.........
.........
.........
...###...
...###...
...###...
.........
.........
.........
```

`waiting` — `?` (`--cx-st-waiting`):

```text
..####...
.#....#..
......#..
.....#...
....#....
....#....
.........
....#....
.........
```

`done` — check-tick (`--cx-st-done`):

```text
.........
.........
.........
........#
.......#.
#.....#..
.#...#...
..#.#....
...#.....
```

`paused` — two bars (`--cx-st-paused`):

```text
.........
.........
..##.##..
..##.##..
..##.##..
..##.##..
..##.##..
.........
.........
```

`stopped` — hollow square outline (`--cx-st-stopped`):

```text
.........
.........
..#####..
..#...#..
..#...#..
..#...#..
..#####..
.........
.........
```

`error` — `!` (`--cx-st-error`):

```text
.........
....#....
....#....
....#....
....#....
....#....
.........
....#....
.........
```

`disconnected` — broken square outline (`--cx-st-disconnected`):

```text
.........
.........
..##.##..
..#...#..
.........
..#...#..
..##.##..
.........
.........
```

## Tabs

- **Class:** `.cx-tabs` · `data-orientation="h | v"`, with `.cx-tab` items
  (`data-selected`).
- **States (per tab):** rest / hover / active / selected (accent underline for
  `h`, accent left rule for `v`) / disabled (45%) / focus.
- **Tokens:** `--cx-bg-hover`, `--cx-bg-active`, `--cx-accent`, `--cx-border`,
  `--cx-text`, `--cx-text-dim`, `--cx-text-bright`, `--cx-font-ui`,
  `--cx-text-sm`, `--cx-space-1/-2/-3`, `--cx-motion-fast`, `--cx-ease-out`,
  `--cx-focus-ring`.

Topbar view-tabs, right-sidebar activity bar, workspace tab strip.

## Panel / Pane

- **Classes:** `.cx-panel`, `.cx-pane` · `data-focused`.
- **States:** a focused pane draws an accent 1px inner rule (the spatial-focus
  marker); interactive children own their own `:focus-visible` ring.
- **Tokens:** `--cx-bg-panel`, `--cx-accent`, `--cx-border`, `--cx-text`,
  `--cx-font-ui`, `--cx-text-sm`, `--cx-radius-md`, `--cx-motion-base`,
  `--cx-ease-out`.

The workspace's two fixed panes (home channel · session trace). Focus is
carried by the accent rule, not a raised background.

## Tree row

- **Class:** `.cx-tree-row` · `data-depth` (0–5), `data-selected`,
  `data-disabled`; children `.cx-tree-caret`, `.cx-tree-pin`.
- **States:** rest / hover (row wash + pin reveal) / active / selected
  (`--cx-bg-selected` + accent left rule) / disabled (45%) / focus. Height is
  26px.
- **Tokens:** `--cx-bg-hover`, `--cx-bg-active`, `--cx-bg-selected`,
  `--cx-accent`, `--cx-text`, `--cx-text-bright`, `--cx-text-faint`,
  `--cx-font-ui`, `--cx-text-sm`, `--cx-space-2/-3/-4/-5/-7/-8`,
  `--cx-motion-fast`, `--cx-ease-out`, `--cx-focus-ring`.

Agent tree + channel/topic rows; caret, state dot, pin affordance.

## Menu (Kobalte)

- **Class:** `.cx-menu`, with `.cx-menu-item` (`data-highlighted`,
  `data-selected`, `data-disabled`).
- **States (per item):** rest / hover (or Kobalte `data-highlighted`) / active
  / selected / disabled (45%) / focus. The float is elev-2 on the panel
  surface; keyboard is WAI-ARIA via Kobalte, styled entirely by our classes.
- **Tokens:** `--cx-bg-panel`, `--cx-bg-hover`, `--cx-bg-active`,
  `--cx-bg-selected`, `--cx-border`, `--cx-text`, `--cx-text-bright`,
  `--cx-font-ui`, `--cx-text-sm`, `--cx-space-1/-2/-8`, `--cx-radius-sm/-md`,
  `--cx-elev-2`, `--cx-z-overlay`, `--cx-motion-fast`, `--cx-ease-out`,
  `--cx-focus-ring`.

## Dialog (Kobalte)

- **Class:** `.cx-dialog`, with `.cx-dialog-backdrop` (scrim).
- **States:** elev-3 float above the scrim (`--cx-scrim`); focus-trap is
  Kobalte. Interactive children own their focus rings.
- **Tokens:** `--cx-bg-panel`, `--cx-scrim`, `--cx-border-strong`,
  `--cx-text`, `--cx-font-ui`, `--cx-text-sm`, `--cx-space-4/-5`,
  `--cx-radius-lg`, `--cx-elev-3`, `--cx-z-modal`.

## Command palette (Kobalte combobox)

- **Classes:** `.cx-palette` (the float container), with `.cx-palette-input`
  (query field), `.cx-palette-backdrop` (scrim + positioner),
  `.cx-palette-list` (results scroll region), `.cx-palette-group` (section
  header), `.cx-palette-row` (result row) · `data-selected`, `data-highlighted`,
  `data-disabled`, and the row sub-parts `.cx-palette-glyph`,
  `.cx-palette-title`, `.cx-palette-context`, `.cx-palette-shortcut`. Plus
  `.cx-palette-empty` (no-results row) and `.cx-palette-loading` (loader host).
- **One surface, two modes, prefix-free.** A single input over the command
  registry with no mode sigil. **Action mode** fuzzy-searches registered
  commands (`{ id, title, keywords, scope, shortcut?, run() }`); scoped
  commands rank above global when their scope is active, and each shows its
  shortcut chip. **Navigation mode** — bare typing, no `>` prefix — matches
  both commands and destinations (`{ id, title, kind, navigate(), score? }`,
  `kind ∈ agent | channel | topic | issue | pr | view`). Async destination
  providers rank by recency + fuzzy score; the weighting is `compass-ui`'s and
  is not encoded here.
- **Result-row anatomy** (left→right): `.cx-palette-glyph` (a 9px 1-bit type
  glyph — the command icon or destination `kind`) · `.cx-palette-title` (the
  label, takes free space and truncates) · `.cx-palette-context` (dim secondary
  text — scope / path / parent, `--cx-text-dim`) · `.cx-palette-shortcut` (the
  right-aligned key-hint chip).
- **States:** rest / hover (`--cx-bg-hover`, shared by Kobalte
  `data-highlighted`) / selected (`--cx-bg-selected` + accent left rule) /
  disabled (45%) / focus (`--cx-focus-ring`). `.cx-palette-group` is a dim
  uppercase label row (`--cx-text-faint`), never selectable. Empty state is a
  dim "no results" row (`.cx-palette-empty`); loading hosts the chase-light bar
  `.cx-loader[data-topology="bar"]` (D9) under the input via
  `.cx-palette-loading` — the keyframe is **T8**, referenced not authored here.
- **Geometry:** panel is elev-3 on `--cx-bg-panel`, **560px wide** (fluid to
  `100%` within the window), positioned in the **top third** of the viewport
  by the backdrop (horizontally centered, `20vh` top offset — a reach-for-it
  surface, not a dead-center modal). Rows mirror the `.cx-tree-row` density
  (26px height, same padding rhythm) so palette and tree read as one row
  system.
- **Host:** a Kobalte combobox; `Cmd/Ctrl+K` opens it at `--cx-z-palette`.
  ARIA + keyboard traversal are Kobalte's; this owns only the visual surface.
- **Tokens:** `--cx-bg-panel`, `--cx-bg-hover`, `--cx-bg-selected`, `--cx-scrim`,
  `--cx-accent`, `--cx-border`, `--cx-border-strong`, `--cx-border-focus`,
  `--cx-text`, `--cx-text-bright`, `--cx-text-dim`, `--cx-text-faint`,
  `--cx-font-ui`, `--cx-text-xs/-sm/-lg`, `--cx-space-1/-2/-3/-4`,
  `--cx-radius-sm/-lg`, `--cx-elev-3`, `--cx-z-palette`, `--cx-motion-fast`,
  `--cx-ease-out`, `--cx-focus-ring`.

## Tooltip (Kobalte)

- **Class:** `.cx-tooltip`.
- **States:** display surface (elev-1); open delay `--cx-tooltip-delay`
  (400ms) is Kobalte's timing prop. Never load-bearing — the same info is
  reachable elsewhere.
- **Tokens:** `--cx-bg-raised`, `--cx-border`, `--cx-text`, `--cx-font-ui`,
  `--cx-text-xs`, `--cx-space-1/-2`, `--cx-radius-sm`, `--cx-elev-1`,
  `--cx-z-overlay`.

## Toast

- **Class:** `.cx-toast` · `data-kind="info | ok | warn | error"`.
- **States:** display surface (elev-2), bottom-right stack at `--cx-z-toast`;
  `data-kind` sets a status accent left rule without swapping the surface. A
  dismiss control is a `.cx-btn`.
- **Tokens:** `--cx-bg-panel`, `--cx-border`, `--cx-border-strong`,
  `--cx-info`, `--cx-ok`, `--cx-warn`, `--cx-error`, `--cx-text`,
  `--cx-font-ui`, `--cx-text-sm`, `--cx-space-2/-3`, `--cx-radius-md`,
  `--cx-elev-2`, `--cx-z-toast`.

## Ask block

- **Class:** `.cx-ask` · `data-answered`, with `.cx-ask-options` and `.cx-btn`
  option buttons.
- **States:** open (accent left rule pulling the eye) → answered (rule drops,
  options dim to 45%, the chosen option's `.cx-btn[data-selected]` stays
  lit). Option buttons follow the `.cx-btn` state contract.
- **Tokens:** `--cx-bg-panel`, `--cx-accent`, `--cx-border`,
  `--cx-border-strong`, `--cx-text`, `--cx-text-dim`, `--cx-font-ui`,
  `--cx-text-sm`, `--cx-space-2/-3/-4`, `--cx-radius-md`.

## Markdown content

- **Class:** `.cx-md` — defined in `base.css` (T2); NOT redefined here. Shiki
  via the `--cx-ed-*` editor theme (T7), GFM tables, mention chips. Listed for
  completeness of the D3 set.

## Loader (spinner / bar)

- **Class:** `.cx-loader` · `data-topology="spinner | bar"`, with
  `.cx-loader-fill` (bar).
- **Spinner:** closed square loop, indeterminate work; the accent head travels
  the loop at the base cadence.
- **Bar:** open track, blue fill + fog head, determinate progress driven by
  `--cx-loader-value` (0–1) the `compass-ui` side sets; no indeterminate bar.
- **Non-purple loading palette.** The one sanctioned purple spinner (the
  brand-mark-in-motion moment) lives in the mark component (`mark*.css`), not
  here — purple is never aliased into `--cx-*`. The full keyframe /
  boot-sequence spec is **T8**; this owns the class contract and consumes the
  D9 motion tokens.
- **Tokens:** `--cx-accent`, `--cx-border`, `--cx-text`, `--cx-space-1/-4`,
  `--cx-radius-sm`, `--cx-motion-slow`, `--cx-motion-base`, `--cx-ease-out`.

## Scrollbar

- **Global** — defined in `base.css` (T2); NOT redefined here. Thin (8px),
  thumb `--cx-border-strong`, track transparent. Listed for completeness of the
  D3 set.
