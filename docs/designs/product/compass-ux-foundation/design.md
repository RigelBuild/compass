# Compass UX foundation — target design system + interaction model

Status: Draft
Linear: SEA-1663
Supersedes: the pre-freeze draft of this record (sealed PR #1075; its
DL-114..122 block never shipped)

That pre-freeze draft was authored against a *provisional* brand identity; the
Rigel brand spec has since FROZEN (`docs/specs/brand/`), and this rewrite
re-derives the record against the frozen spec. File references are relative to
`apps/ui/src/` in `RigelBuild/compass` unless otherwise pathed; `brand <file>`
cites the Rigel frozen brand spec.

## Problem / Intent

Compass's current UI is a prototype that was never designed. The stylesheet
says so itself — "Compass ADE — dev UI styling. Dark, dense, board-first"
(`app.css:1`) — and its palette is GitHub-dark ad-hoc greys
(`--bg: #0a0d12; --bg-raised: #10141b; --bg-panel: #131820`, `app.css:9-11`)
with a borrowed accent (`--accent: #58a6ff`, `app.css:26`) that has no
relationship to the Rigel brand. It has no command palette, almost no keyboard
navigation, and live accessibility defects
(`.r-files-search:focus { outline: none }`, `app.css:2385-2388`;
`.conv-composer .field:focus { outline: none }`, `app.css:3556-3559`).

This record designs the **target end-state UX**: the Compass semantic token
tier, the first-party component system, the keyboard-first interaction model
(command palette, global keymap, focus model), the editor-theme mapping, the
motion system, and how that system renders the frozen target IA — across both
render hosts (the Wails desktop app and the browser-hosted product at
`compass.rigel.build`). It does **not** design the visual language itself — the
Rigel brand spec froze that (the chase-light / dot-matrix spine, the Night Owl
palette, the three-face type system, the eight-state icon vocabulary, the motion
system). This record **consumes** the frozen brand spec as its primitive tier
and designs everything Compass owns on top of it. The bar is **genuine
excellence** on the surfaces a user lives in — the Bridge board, the Manager
tree, the agent channels + threads, and the agent session trace (D6's excellence
bar) — not merely brand-compliance. The current UI is the *functional*
reference — what users must be able to do — never the visual basis. Downstream
of the frozen brand spec, upstream of all `apps/ui` implementation work.

## Global Constraints

Frozen by Matt 2026-08-03 (stack/styling/seam/keyboard) and by the brand
freeze (spec-consumption). Every task in `## Plan` inherits these; do not
reopen any of them inside a task.

1. **Design record only.** No code ships with this record; it is the contract
   executing lanes read.
2. **Stack + render targets**: SolidJS ^1.9.13 + Vite + TypeScript, one UI
   codebase rendered in **two hosts**: the **Wails v3 (Go) desktop app**
   (DL-110; the primary surface) and a **browser** (the managed/hosted product
   at `compass.rigel.build`). Both consume the same transport-agnostic UI above
   the connection-provider seam (`connection.ts` — the shell supplies a
   `Connection` in the app, the env provider in the browser;
   `compass-native-app/design.md` §A1). So the layout is **fluid within a
   window** (it adapts to the window/viewport it is given — desktop windows and
   browser tabs both vary in size), NOT a fixed-pixel canvas; it is still a
   dense supervision surface, not a marketing page, so "fluid" means robust
   min/max and sensible reflow of the shell regions, not a mobile redesign. The
   Go shell, OS windows, and mode plumbing are compass-native's lane
   (SEA-1684); this record designs what renders inside a webview/tab and the
   render contract, including how the UI **decomposes into independently
   mountable window-scoped views** (D6 multi-window).
3. **Styling tech**: pure CSS custom properties + a small first-party SolidJS
   component set. A headless a11y primitive library (Kobalte preferred, Ark
   acceptable) may be used ONLY for accessibility-hard primitives — menu,
   dialog, combobox/command palette, focus-trap, tooltip — never for look.
   **No Tailwind. No full component library. No CSS-in-JS.** Adding any heavy
   styling dependency is a design fork requiring a new record. This scopes the
   motion *runtime*, not the motion *system*: the product UI expresses the
   frozen brand motion system (D9) in **pure CSS/SVG + Solid's fine-grained
   reactivity** — the marketing showcase's GSAP/Three.js/Lenis/Barba stack
   (`brand motion.md` §"The tech stack") is a *separate track* (brand scopes it
   to the marketing site, and `motion.md` marks it "the aspiration, not yet
   built"); this record consumes the same motion vocabulary that stack renders,
   built on the cheap substrate instead. The motion system itself is fully in
   scope (D9).
4. **Brand seam**: the frozen Rigel brand spec is the upstream source of
   truth and this record's **primitive tier**. A design record cites, never
   imports: Compass consumes the live token names verbatim
   (`brand tokens.css`) and never invents a parallel palette. Compass owns
   the **semantic tier** (`--cx-*`), the component system, the ADE look and
   density, the editor-theme mapping, and the keyboard interaction model.
   Mutual co-review: brand co-reviews the token/primitive layer; compass-ux
   co-reviews the editor-theme mapping.
5. **Keyboard-first is a first-class pillar.** A command palette
   (Linear/Zed/Raycast-class, `Cmd/Ctrl-K`) and comprehensive keyboard
   navigation are REQUIRED design surfaces. Every primary action must be
   keyboard-reachable; every interactive element renders a real focus state.
   Brand does not touch this pillar.
6. **Dark-primary.** The ADE ships dark first (Night Owl); a light pair
   ("polarity inversion, not a redesign", `brand color.md`) is kept
   architecturally open via semantic-tier indirection but does not ship in
   this record's scope.
7. **The target IA is frozen** (DL-095/096, DL-113, DL-098/099,
   DL-067/070/097, DL-129, DL-039). This record designs how it looks and how it
   is navigated; it does not redesign the IA. SEA-1622 (channels under the
   agent tree) is unfrozen — the design must accommodate that direction
   without depending on its mechanism.
8. **Naming/markup conventions** (record-adopted from the codebase, not
   Matt-frozen): component visual contracts are expressed as a class
   vocabulary plus `data-*` variant modifiers — the codebase's existing
   convention (`.state-dot[data-state=…]`, `app.css:490-514` consumed by
   `components/StateDot.tsx:20-24`; `<span class="ci-badge"
   data-status={…}>`, `components/Bridge.tsx:61`; `data-priority`/
   `data-state` rows, `components/BacklogView.tsx:17,25,28`).

## Approach

Ten numbered decisions (D1–D10), plus three folded rulings (render targets,
multi-window, workspace scope). Each is citable and owes a ledger row (see
`## Ledger-impact`).

### D1 — Visual language: the frozen Rigel spine, worn at ADE density

The old draft *derived* a visual language ("starlight instrumentation") from
Night Owl; that derivation is now superseded — the brand spec froze the
language. Compass looks like a precision instrument for supervising an agent
fleet at night, expressed in the one Rigel idiom: the chase-light / dot-matrix
spine — discrete ON/OFF cells, 1-bit bitmap-native, hard edges, a lit segment
that travels a fixed track (`brand spine.md`). What this record adds is how
that identity wears at ADE density. Five principles govern every component:

1. **Surface-color elevation, shadows secondary.** Depth is carried by the
   Night Owl surface ladder (navy `--rigel-night` → raised `--rigel-raised` →
   panel `--rigel-panel`, `brand color.md` §Surfaces); box-shadows are
   reserved for genuinely floating layers (menus, dialogs, palette, toasts).
2. **Contrast is meaningful and rationed.** Blue `--rigel-blue` is the
   interaction color ("functions · links · interaction", `brand color.md`
   §Syntax/UI): selection, primary actions, links, the focus ring, the
   focused pane. Purple is NOT an interaction color — it appears in exactly
   one place per surface, inside the mark (`brand color.md` §"The one-accent
   rule"); phosphor `--rigel-purple-lit` is a lit *state* (loading/active
   pulse), never a static fill. Spectrum colors appear only as status
   semantics — never decoration.
3. **Dense by default, calm by hierarchy.** An ADE is an information-dense
   supervision surface — a 4px spatial grid governing spacing tokens (not
   intrinsic row heights: the 26px tree/list row is 12px type + 2×7px
   padding, off-grid by design), compact row heights (24-28px) for
   tree/list rows, and a small type ramp (12px UI base; Space Mono is
   specified legible at 12px, `brand type.md`). Calm comes from hierarchy:
   `--rigel-haze` for secondary content, `--rigel-fog` for primary, blue only
   where attention is owed.
4. **Monospace is identity, not costume.** The whole UI is mono: Space Mono
   (`--rigel-mono`) is the body/UI workhorse for everything ≤16px and every
   off-grid size; Departure Mono (`--rigel-display`) appears only at large
   brand display moments on even 11px multiples (22/44); the bitmap R mark
   is SVG geometry, never a font (`brand type.md`; D2's type tokens carry
   the boundaries). Crossing these boundaries is a defect, not a style
   choice.
5. **Everything answers to the keyboard.** Every interactive element has a
   visible focus treatment (the focus-ring token, D4); every hover affordance
   has a keyboard equivalent; the command palette can reach anything the
   pointer can (D5). No `outline: none` without a replacement ring — the two
   current instances (`app.css:2385-2388`, `app.css:3556-3559`) are defects
   this system retires.

### D2 — Token model: three tiers, one consumption rule

**Tiering.** Three tiers, each with a distinct prefix and a strict
consumption direction (primitive → semantic → component; never skip upward):

- **Primitive tier — `--rigel-*`** (brand-owned, frozen). Compass mirrors the
  live brand token set VERBATIM into its primitives block — names and values
  from `brand tokens.css`, no additions, no renames:

  ```css
  /* Surfaces / text (brand tokens.css; contrast per brand color.md) */
  --rigel-night: #011627;   /* background — the night sky */
  --rigel-night-2: #0b2942;
  --rigel-panel: #0e2a45;   /* panel / border / inset */
  --rigel-raised: #0b2942;  /* raised surface (cards, dock, pills) */
  --rigel-fog: #d6deeb;     /* primary text, 13.54:1 AAA on night */
  --rigel-bright: #c5e4fd;  /* emphasis / active text, 13.87:1 AAA */
  --rigel-mute: #5f7e97;    /* low-emphasis meta, 4.29:1 — decorative/large only */
  --rigel-haze: #89a4bb;    /* readable secondary text, 7.06:1 AA */
  --rigel-blue: #82aaff;    /* interaction: links, focus, selection */
  --rigel-green: #addb67;   /* the WORKING-state green */
  --rigel-amber: #ecc48d;
  --rigel-red: #ef5350;
  --rigel-cyan: #7fdbca;
  /* The one color accent (mark-only; see the one-accent rule) */
  --rigel-purple: #a66ef5;
  --rigel-purple-hi: #d1aaff;
  --rigel-purple-lit: #b57eff; /* phosphor / lit / loading only */
  ```

  plus the brand motion/timing tokens (`--rigel-motion-slow`,
  `--rigel-ease-out`, `--rigel-ease-morph`, `--rigel-pulse-color`,
  `--rigel-pulse-period`, `--rigel-stream-char-ms`, `--rigel-cursor-blink` —
  D9) and the two font stacks (`--rigel-mono`, `--rigel-display` — verbatim
  from `brand type.md` §"The token stacks"). The old draft's
  `--rigel-slate/violet/coral/white/night-3/blue-bright/teal` names DO NOT
  EXIST in the frozen set and never appear; there is no violet token at all.
  Compass consumes this tier read-only; brand co-reviews any file that
  defines it. Six `brand color.md` values are documented in the spec but not
  yet tokenized in `brand tokens.css` (selection `#1d3b53`, faint `#637777`,
  the syntax-tier magenta `#c792ea` / success-green `#22da6e` / coral
  `#f78c6c`, and the loading-track empty `#0a2036`); the editor-theme mapping
  and the loaders need them — see Open Questions Q2 for how they enter the
  primitives block without inventing token names.

- **Semantic tier — `--cx-*`** (Compass-owned). Every UI rule consumes ONLY
  this tier. Named by intent, not hue:
  - Surfaces: `--cx-bg` → `--rigel-night`, `--cx-bg-raised` →
    `--rigel-raised`, `--cx-bg-panel` → `--rigel-panel`, `--cx-bg-hover` /
    `--cx-bg-active` (color-mix washes over panel), `--cx-bg-selected`
    (the brand selection surface — Q2 — with `--rigel-blue` low-alpha
    color-mix as the interim derivation).
  - Scrim: `--cx-scrim` (modal/palette backdrop — `--rigel-night` at ~60%).
  - Text: `--cx-text` → `--rigel-fog`, `--cx-text-bright` → `--rigel-bright`,
    `--cx-text-dim` → `--rigel-haze` (the readable secondary — NOT
    `--rigel-mute`, which is 4.29:1 and decorative/large-only per
    `brand tokens.css` comments), `--cx-text-faint` → `--rigel-mute`
    (meta/decorative only), `--cx-text-accent` → `--rigel-blue`.
  - Lines: `--cx-border`, `--cx-border-strong` (panel-derived),
    `--cx-border-focus` → `--rigel-blue`.
  - Accent: `--cx-accent` → `--rigel-blue`, `--cx-accent-hover`,
    `--cx-accent-muted` (`color-mix()` washes from `--rigel-blue`). Purple is
    NEVER aliased into the semantic tier — the one-accent rule means no
    `--cx-*` name can resolve to `--rigel-purple`; the mark is the only
    consumer and it consumes the primitive directly (D8), the sole sanctioned
    exception to the consumption rule (carved out narrowly in the D7 guard —
    mark component only).
  - Status: `--cx-ok` (success green — Q2's syntax-tier `#22da6e`, distinct
    from the working green per `brand color.md`: "the two greens are distinct
    roles and must not be swapped"), `--cx-warn` → `--rigel-amber`,
    `--cx-error` → `--rigel-red`, `--cx-info` → `--rigel-blue`.
  - Agent state (process axis) — `--cx-st-*`: the EIGHT `AgentState` values
    the code defines — `working|idle|waiting|done|paused|stopped|error|
    disconnected` (`stub-data.ts:47-55`; its comment fixes this as the
    *process* axis, distinct from an issue's `blocked` (the *task* axis),
    `stub-data.ts:42-46`) — consumed by `.state-dot[data-state=…]`
    (`app.css:490-514`) and typed `AgentState` in `components/StateDot.tsx`.
    Colors are the brand state-color mapping, verbatim (`brand color.md`
    §"State-color mapping"): working → `--rigel-green` (#addb67 — the
    working-state green, NOT the syntax-tier success green), done →
    `--rigel-cyan`, waiting → `--rigel-amber`, disconnected →
    `--rigel-amber`, error → `--rigel-red`, idle/paused/stopped →
    `--rigel-mute`. Non-color distinguishability comes from the frozen
    eight-glyph icon vocabulary (D3), so the set is CVD-safe and
    reduced-motion-safe by brand contract, not by per-record derivation.
  - Issue lifecycle (task axis) — `--cx-issue-*`: a SEPARATE namespace from
    agent state. `BOARD_LANES` (`constants.ts:17-23`) defines five rendered
    lanes — `queued|blocked|in_progress|in_review|done` — plus the pre-active
    `backlog|todo` tier that does not render on the grid (`constants.ts:28`).
    Colors re-mapped onto the frozen set (the old draft's violet does not
    exist): queued → `--rigel-mute`, blocked → `--rigel-red`, in_progress →
    `--rigel-green` (the lane means "an agent is working it" — deliberately
    the working green), in_review → `--rigel-amber` (awaiting human
    attention, the same intent family as agent `waiting`; ruled amber,
    Resolved decisions), done → `--rigel-cyan` (matching agent `done`, so
    "done = cyan" holds
    across both axes). These are issue-state intents, never agent-state
    tokens; D6's board tinting consumes `--cx-issue-*`.
  - CI/review: `--cx-ci-pass` → `--cx-ok`, `--cx-ci-fail` → `--cx-error`,
    `--cx-ci-pending` → `--cx-warn`; `--cx-review-approved` → `--cx-ok`,
    `--cx-review-changes` → `--cx-error`, `--cx-review-pending` →
    `--cx-text-faint` (consumed by the badge/pip contracts, D3).
  - Editor-theme: `--cx-ed-*` — the mapping from semantic tier to the
    embedded editor/Shiki theme (background, selection, cursor, syntax ramp),
    so editor panes and UI chrome share one palette. Night Owl "dresses the
    whole product UI and drives the Compass editor theme" (`brand color.md`);
    Compass owns this mapping (D8).
- **Scale tokens — `--cx-*` (non-color, Compass-owned).** Space: 4px-grid
  ramp `--cx-space-1..8` (4/8/12/16/20/24/32/40). Type: `--cx-font-ui` →
  `--rigel-mono` (Space Mono, IBM Plex Mono as fallback only — the stack is
  brand's, consumed verbatim), `--cx-font-display` → `--rigel-display`
  (Departure Mono; even 11px multiples ONLY — 22/44 in the ADE; odd
  multiples smear at fractional DPR, `brand type.md` §"The Departure-Mono
  11px-grid constraint"); size ramp `--cx-text-xs..xl` (11/12/13/14/16) with
  the 12px UI base — every ramp size is ≤16px, so by the brand type rule the
  entire ramp renders in `--cx-font-ui`; display sizes 22/44 exist outside
  the ramp as `--cx-display-sm|lg` and are the only sanctioned
  `--cx-font-display` sizes; weights 400/700 (Space Mono ships regular +
  bold; no synthetic weights — `font-synthesis: none` per `brand tokens.css`
  base). Radius: `--cx-radius-sm|md|lg` (3/6/10). Elevation: `--cx-elev-0..3`
  menu/dialog/palette). Motion: `--cx-motion-fast` (80ms) and `--cx-motion-base`
  (140ms) are already defined IN `brand tokens.css` as the inherited Compass
  tier; Compass mints the rest of the motion semantic tier as `--cx-*` aliases
  of the brand primitives so component CSS never names a `--rigel-*` motion
  token directly (satisfying the D7 guard): `--cx-motion-slow` →
  `--rigel-motion-slow`, `--cx-ease-out` → `--rigel-ease-out`, `--cx-ease-morph`
  → `--rigel-ease-morph`, `--cx-pulse-color` → `--rigel-pulse-color`,
  `--cx-pulse-period` → `--rigel-pulse-period`, `--cx-stream-char-ms` →
  `--rigel-stream-char-ms`, `--cx-cursor-blink` → `--rigel-cursor-blink`; plus
  `--cx-tooltip-delay` (400ms, Compass-owned — no brand primitive). D9 owns the
  motion rules. Z-index: `--cx-z-raised|overlay|modal|palette|toast`
  (10/100/200/300/400). Focus: `--cx-focus-ring` — already frozen upstream
  as `2px solid var(--rigel-blue)` (`brand tokens.css`; `brand color.md`:
  "the focus ring is blue… not purple"), consumed as-is.

**Consumption rule.** Component CSS consumes semantic + scale tokens only.
`--rigel-*` primitives are referenced exclusively inside the semantic-tier
definition block (one file, D7). Raw hex values are banned in component CSS;
this is lintable (a stylelint declaration-property-value check in CI, part of
delivery task T2). The same rule governs motion (D9): a literal `200ms` in a
component is a review failure, exactly like a literal hex
(`brand motion.md` §"Consumption rule").

**Theming.** The semantic tier is defined under a `[data-theme="night"]`
scope on the root. A light pair ships later by adding a `[data-theme="day"]`
scope that re-maps the same semantic names — brand fixes light mode as a
polarity inversion, not a redesign (`brand color.md`) — with zero component
CSS changes. This is the architectural openness Global Constraint 6 requires.

### D3 — Component system: a small first-party set with class + `data-*` visual contracts

Roughly twenty fundamental components, each specified as a **visual
contract**: a class vocabulary + `data-*` variant modifiers (the codebase
convention, Global Constraint 8), the states it renders, and the semantic
tokens it consumes. SolidJS mechanics (props, reactivity, `@compass/client`
wiring) are compass-ui's side of the seam; this record owns what renders.

**The set** (component · contract sketch · notes):

| Component | Contract sketch | Notes |
| --- | --- | --- |
| Button | `.cx-btn` · `data-variant="primary\|ghost\|danger"` · `data-size="sm\|md"` | Primary = accent fill; ghost = borderless, hover wash; danger = `--cx-error` |
| Input / Search / Select | `.cx-input`, `.cx-search`, `.cx-select` | Border `--cx-border`, focus swaps to `--cx-border-focus` + ring |
| Composer | `.cx-composer` | Multi-line, Enter/Shift-Enter, disabled state, grows to cap |
| Card | `.cx-card` · `data-selected` | Issue/PR/backlog rows; selected = `--cx-bg-selected` + accent left rule |
| Badge / Pip / Chip | `.cx-badge` · `data-status`, `.cx-pip`, `.cx-chip` | CI (`--cx-ci-*`), review (`--cx-review-*`), unread counts, tracker chips |
| State dot | `.cx-state-dot` · `data-state` (8 agent states) | The frozen brand vocabulary — see below |
| Tabs | `.cx-tabs` · `data-orientation="h\|v"` | Topbar view-tabs, right-sidebar activity bar, workspace tab strip |
| Panel / Pane | `.cx-panel`, `.cx-pane` · `data-focused` | The workspace's two fixed panes (home channel · session trace); focused pane = accent 1px inner rule |
| Tree row | `.cx-tree-row` · `data-depth`, `data-selected` | Agent tree + channel/topic rows; 26px height, caret, state dot, pin affordance |
| Menu (Kobalte) | `.cx-menu` | Elev-2 float on `--cx-bg-panel`; keyboard per WAI-ARIA via Kobalte |
| Dialog (Kobalte) | `.cx-dialog` | Elev-3, scrim `--cx-scrim` |
| Command palette (Kobalte combobox) | `.cx-palette` | See D5 — the flagship surface |
| Tooltip | `.cx-tooltip` | Elev-1, open delay `--cx-tooltip-delay` (400ms), never load-bearing (info also reachable elsewhere) |
| Toast | `.cx-toast` · `data-kind="info\|ok\|warn\|error"` | Bottom-right stack, `--cx-z-toast` |
| Ask block | `.cx-ask` · `data-answered` | The agent→human question card (option buttons, locked when answered) |
| Markdown content | `.cx-md` | Shiki via `--cx-ed-*` theme, GFM tables, mention chips |
| Loader (spinner / bar) | `.cx-loader` · `data-topology="spinner\|bar"` | The brand chase-light primitive, pure CSS/SVG — see D9 |
| Scrollbar | global | Thin (8px), thumb `--cx-border-strong`, track transparent |

**State dot: the frozen eight-glyph vocabulary.** The old draft spent four
review rounds deriving its own dot treatments (ring-arc working, plain-dot
idle, hourglass debates); that derivation is deleted, not carried — brand
froze the vocabulary and explicitly ruled the ring out ("the `working` form
was ruled this session over an earlier ring, which read as a bare 'C' at the
12px row-dot size", `brand state-icons.md`). `.cx-state-dot` consumes the
frozen set 1:1 against the `AgentState` enum (`stub-data.ts:47-55`), 9×9
bitmap grid, static by default, CVD-safe:

| state | glyph (`brand state-icons.md`) | color (`--cx-st-*`, D2) |
| --- | --- | --- |
| working | double-chevron `»` (fast-forward) — the ONLY animated state | `--rigel-green` |
| idle | 3×3 block | `--rigel-mute` |
| waiting | `?` | `--rigel-amber` |
| done | check-tick | `--rigel-cyan` |
| paused | two bars | `--rigel-mute` |
| stopped | hollow square outline | `--rigel-mute` |
| error | `!` | `--rigel-red` |
| disconnected | broken square outline | `--rigel-amber` |

Only `working` animates: the working pulse, the brand pulse cadence in the
working-state green — never the purple phosphor (`brand state-icons.md`
Requirement; D9). The other seven are distinguished by glyph + color alone,
so the set holds under `prefers-reduced-motion` by brand contract. Glyphs
render as inline SVG on the 9×9 grid with `shape-rendering="crispEdges"`
(`brand spine.md` §Rendering) — 1-bit cells, no anti-aliasing.

**States are mandatory and uniform.** Every interactive component specifies
rest / hover / active / selected / disabled / **focus**. Focus always renders
`--cx-focus-ring` (`:focus-visible`); hover states use `--cx-bg-hover`;
disabled = 45% opacity + `cursor: default`, never color-swaps that break
status semantics.

**Surface tiers are three.** Cards, rows, and list cells render on
`--cx-bg-panel`; there is no fourth card-surface token. Selection is carried
by `--cx-bg-selected` + the accent left rule, not a raised background.

**Kobalte scope.** Kobalte is used for menu, dialog, combobox (palette),
tooltip, and focus-trap only — the a11y-hard set Global Constraint 3 allows
(tooltip included because WCAG 1.4.13 hoverable/dismissable content-on-focus
has real timing and dismissal edge cases). It is styled entirely by our
classes; no Kobalte default styles ship.

### D4 — Focus model: one ring, spatial focus zones, roving tabindex

- **One focus treatment.** `--cx-focus-ring` on `:focus-visible` everywhere —
  the token is frozen upstream as `2px solid var(--rigel-blue)`
  (`brand tokens.css`), blue because interaction lives on the flow color and
  purple stays inside the mark (`brand color.md` §"The one-accent rule").
  Pointer interactions don't paint rings; keyboard always does. The two
  `outline: none` defects (`app.css:2385-2388`, `app.css:3556-3559`) are
  retired by rule: removing an outline without applying the ring token fails
  review.
- **Focus zones.** The shell is four interactive focus zones — left sidebar,
  main view, right sidebar, topbar — cycled with `Ctrl+1..3`
  (sidebar/main/right; topbar is F6-only) and `F6`/`Shift+F6`. The usage bar is
  a display-only landmark (`UsageBar.tsx` carries no interactive element),
  reachable by screen-reader landmark navigation but not in the F6 rotation; it
  rejoins as a fifth zone only if it gains an interactive control (T6 verifies
  this at flip time). Within a zone, arrow keys move a **roving tabindex** (one
  tab stop per zone; arrows move selection within tree, board grid, topic list,
  tab strips). This keeps global Tab order short and makes dense surfaces
  navigable.
- **Pane focus.** The workspace is two fixed panes (home channel, session
  trace — D6); the focused pane carries `data-focused` and the accent inner
  rule, and `Ctrl+Alt+Arrow` moves focus between the two. No arbitrary split
  tree.
- **Escape ladder.** `Esc` closes, in order: palette → menu/dialog → clears
  transient selection → returns focus to the zone's anchor. Nothing traps
  focus except Kobalte-managed modals.

### D5 — Command palette + global keymap: the keyboard is a first-class surface

Carried forward from the pre-freeze draft intact — brand does not touch this
pillar; only token references are refreshed. Greenfield (no palette exists;
keyboard today is Enter/Shift-Enter in the composer and a few explicit
`onKeyDown` handlers).

- **Command palette** (`Cmd/Ctrl+K`, Kobalte combobox, `.cx-palette`,
  `--cx-z-palette`): a single input over a **command registry**. Two modes in
  one surface, Linear-style:
  - **Action mode** (default): fuzzy search over registered commands, each
    `{ id, title, keywords, scope, shortcut?, run() }`. Scoped commands
    (e.g. "Split pane right") rank above global ones when their scope is
    active; every command shows its shortcut chip on the right.
  - **Navigation mode**: the palette is prefix-free — bare typing matches
    BOTH commands and destinations (no Raycast-style `>` action prefix).
    Destination providers: agents (tree), channels/topics, issues (`SEA-…`
    keys), PRs, views. Selecting a destination navigates; selecting a command
    runs it. Providers are async and ranked by recency + fuzzy score.
  - Rendering: elev-3 panel on `--cx-bg-panel`, 560px wide, top-third of the
    window; result rows are `.cx-tree-row`-class density with type-glyph +
    title + dim context + shortcut chip; selected row = `--cx-bg-selected`.
- **Command registry** is the spine: every primary action in the product
  (open agent, focus channel/trace pane, pin/unpin, post to topic, re-parent
  agent, toggle sidebars, switch view, open settings…) registers a command —
  the UI is read-only for issue lifecycle state (DL-129), so no
  lifecycle-mutation command (promote/archive) exists here. Menus
  and buttons invoke the same registry entries — one source of truth, so
  palette coverage can't drift from the UI. Enforced, not merely asserted:
  the primitive `Button`/`Menu` take a `command` id (not a raw handler) for
  any primary action, so an unregistered primary action can't be wired;
  compass-ui review against the T4 contract is the backstop — the
  keyboard-side equivalent of D2/T2's stylelint guard.
- **Global keymap** (defaults; single-file keymap registry so a user-remap
  surface stays open later, but remapping UI is out of scope):
  - Views: `Ctrl+B` Bridge, `Ctrl+Shift+A` agent workspace, `Ctrl+,`
    Settings; `Ctrl+K` palette.
  - Zones: `Ctrl+1/2/3` left/main/right; `F6` cycle; `Ctrl+\` toggle right
    sidebar, `Ctrl+Shift+\` left.
  - Lists/tree/board: arrows move, `Enter` open/select, `Space`
    expand/collapse or toggle pin, `Home/End` first/last.
  - Workspace: `Ctrl+Alt+Arrow` moves focus between the channel and trace
    panes.
  - Comms: `Enter` send / `Shift+Enter` newline (kept), `Ctrl+Enter`
    send-and-stay variant reserved.
  - All `Ctrl` bindings are `Cmd` on macOS; the keymap registry abstracts the
    modifier.
- **Discoverability**: shortcut chips in the palette and in tooltips; a
  "Keyboard shortcuts" palette command opens a cheat-sheet dialog.

### D6 — Rendering the frozen IA: surface-by-surface composition

The IA is frozen (Global Constraint 7); this decision states how the design
system renders each surface, in both render hosts (the Wails desktop app and
the browser, Global Constraint 2). Structure comes from the records; look and
navigation come from here. The native shell, OS windows, and mode plumbing are
compass-native's lane (`compass-native-app/design.md`).

**The excellence bar.** These are the product — the surfaces a user lives in
all day — and they must be genuinely excellent, not merely on-brand. Four carry
the bar and each has a concrete starting point already built, not a from-scratch
guess: the **Bridge board** and the **Manager tree** ship as production-quality
Rigel-site mockups in the Rigel brand source (`BridgeBoard.astro`,
`AgentTree.astro`, and `ThreadView.astro` below — same source as the brand spec,
not co-located in compass), each authored as "the real UI would ship it" citing
the real compass `Bridge.tsx`/`board.ts`/`LeftSidebar`, so Compass starts from
those, not a
redraw; **channels + threads** take **Zulip's topic-threading UX as the base**
(the model is already DL-098's) and add Rigel excellence on top; and the
**agent session trace** must both look excellent and carry the live
token-streaming treatment prototyped in the brand's UI-micro-excellence system
(`brand motion.md`, which folds in `ui-micro-excellence.md`: decoupled stream
cadence, the phosphor write-head, partial-markdown safety — D9). Everything
below the excellence surfaces still uses the shared component contracts (D3); it
is the four above that get bespoke design attention.

**Multi-window (D6.1).** The desktop app is **first-class multi-window**: a user
opens one window on the Bridge, another on a channel, another on a specific
agent's workspace, each an independent OS window (compass-native spawns and
manages the windows; this record owns the UI decomposition that makes it
possible). The render contract: every top-level surface (Bridge, a channel/its
topics, an agent workspace, Backlog/Done, Settings) is an **independently
mountable window-scoped view** — it mounts against the DL-127 hash route for
that surface, carries its own focus zones (D4) and command scope (D5), and needs
no sibling region to function (a Bridge window renders without the sidebars). A
single-window session composes these views into the shell grid below; a
multi-window session mounts one view per window. Deferred (**SEA-1808**, Beta
milestone), not built here: tabs *within* a window (Linear-style) and in-window
split views — the view decomposition is designed to admit both later without
rework (a tab strip or a splitter hosts the same window-scoped views), but
neither ships in the dogfood scope. **Cross-lane seam:** compass-native's frozen
record (`compass-native-app/design.md`, DL-110) is single-window today (one
window loading the built UI); DL-160 expands its scope, so compass-native's
shell record needs a multi-window amendment (SEA-1684's lane) before this
decomposition can be hosted in real OS windows — flagged here so the dependency
is explicit, not implicit.

- **Shell.** The grid shape survives (topbar / left / main / right / usage —
  the `.app` `grid-template-areas` rule, `app.css:112-115`; region markup
  `App.tsx:40-120`), re-clothed: topbar and usage bar on `--cx-bg`, sidebars
  on `--cx-bg-raised`, main on `--cx-bg`; 1px `--cx-border` separators; no
  shadows between docked regions (surface-elevation, D1.1). The topbar
  carries the wordmark treatment per the brand surface table
  (`brand identity.md` §"Which mark on which surface" — the sole in-app
  purple, D8), view-tabs (`.cx-tabs`), daemon status pip, pane toggles.
- **Left sidebar — the agent tree** (DL-095: `parent_agent_id` is the sole
  org mechanism, folders removed, no agent special-cased, re-parenting
  first-class). Rendered as `.cx-tree-row`s at 26px: caret (children), state
  dot (the frozen glyph set, D3), name, role pip (supervisor/warden get a
  pip, not a special row), unread badge. Selection = `--cx-bg-selected` +
  accent left rule. Keyboard: arrows/Enter/Space per D4; drag + a palette
  "Re-parent agent…" command for re-parenting. Below the tree, the channel
  rail: channel rows + their 3 most-recent topics as indented deep-nav
  sub-rows (DL-098). The two trees render with the SAME tree-row contract so
  SEA-1622's later unification is a data change, not a visual one. The
  state-dot column is the sanctioned one-pulse-per-region exception (D9): a
  scannable field of working pulses is brand-legal by the pulse budget rule.
  Starting point: the `AgentTree.astro` mockup (production-quality, Managers as
  nodes with a live worker count, a connecting spine with a chase-light pip
  travelling down it = delegation flowing parent→child) — Compass builds from
  it, not a redraw.
- **Right sidebar — pinned agents + status + issue detail** (DL-096: pins are
  client-local, empty default; DL-113: unreachable pin keeps its
  tab with an "unreachable" pane). Vertical `.cx-tabs[data-orientation="v"]`
  activity bar: pinned-agent tabs (avatar glyph + state dot), the
  always-present Status tab, then issue-detail tabs (Files/VCS/PR/Checks).
  Empty-pin default state gets a real empty-state pane (how to pin, palette
  hint) — not a blank void.
- **Bridge — Issues/PRs board** (DL-067/070/097, DL-129: canonical `Issue`,
  server lifecycle, Issues + PRs tabs, swimlanes by tree-ordered assignee).
  Starting point: the `BridgeBoard.astro` mockup, authored against the real
  `Bridge.tsx`/`IssueCard.tsx`/`board.ts` — Issues tab: sticky-left agent
  gutters (state dot + name, tree order), sticky-top status columns tinted by
  `--cx-issue-*` at low alpha in the lane head only (contrast rationing, D1.2),
  `.cx-card` cells; a card advancing a column carries the chase-light (D9). PRs
  tab: flat rows grouped by assignee — state badge, CI badge (`--cx-ci-*`),
  review badge, resolved/total thread count, issue cross-link chip. Board is a
  focus zone: arrow keys move a card cursor across the grid; `Enter` selects;
  `Shift+Enter` opens the assigned agent.
- **Comms — channel → topic** (DL-098/099: channel view is a topic index with
  NO composer; topic view is flat messages + composer; ThreadPanel and all
  `.thread-*` are removed). **Base: Zulip's topic-threading UX** (the model is
  DL-098's) — a channel is a list of named topics, each topic a focused flat
  stream — with Rigel excellence layered on (the `ThreadView.astro` mockup is
  the visual starting point). Topic index: `.cx-tree-row` topic list with
  unread badges + last-activity, "New topic" affordance. Topic view: message
  stream on `--cx-bg`, `.cx-md` content, `.cx-ask` blocks inline (a posted
  topic message is a complete post, never a live-streamed turn — DL-099;
  network streaming lives on the trace surface), author-stamped message headers
  (Manager vs human), composer (`.cx-composer`) pinned bottom with the topic
  name in its chrome (posting is topic-mandatory — the UI makes the topic
  visible at the point of send).
- **Agent workspace** (DL-039: typed session events on the trace surface
  only; DL-158: the workspace is the agent's home channel + its session
  trace, nothing more). Two fixed panes, no arbitrary split tree: the **home
  channel** conversation (the agent's own channel, rendered with the comms
  topic contract above) and the **session trace** — the excellence surface for
  watching an agent think. The LogPanel companion becomes the trace surface: a
  typed renderer, tool-status pips (`--cx-ci-*`-style), Shiki-highlighted
  code/diffs via `--cx-ed-*` so embedded content and chrome share the palette,
  collapsible, minimized-rail ↔ full-panel. This is where the brand's live
  token-streaming treatment runs (D9, `brand motion.md`): decoupled stream
  cadence (bursty network → steady screen at `--cx-stream-char-ms`), the
  phosphor write-head cooling to fog behind it, a blinking block cursor, and
  partial-markdown/code-fence safety so nothing flickers mid-stream. The tab
  strip (`.cx-tabs`) switches the main pane between them; `data-focused` + D4
  pane focus. No terminal pane and no
  file-viewer pane in dogfood: agents run in isolated containers (an
  operator-facing dev-server-launch affordance is deferred to its backlog
  issue, not dogfood), and PR review happens directly on the user's forge, so
  in-app file browsing is out of scope (a later file viewer is possible but
  low-priority). The terminal `PaneKind` arm + `newTerminalPane` in
  `store.ts` are retired with this simplification (a compass-ui deletion at
  the workspace flip step, D10).
- **Backlog / Done / Settings.** List surfaces reuse card/row/badge
  contracts; Settings uses input/select/button contracts with the
  status-mapping editor as a two-column `.cx-tree-row` table. No bespoke
  one-off styling on these views.

### D7 — Delivery: in-tree token + component layout under `apps/ui/src/`

- `apps/ui/src/design/tokens.css` — the three tiers in one file, three
  clearly-marked blocks: `:root` primitives (`--rigel-*` mirrored verbatim
  from `brand tokens.css` with a provenance comment pinning the brand spec
  path + date; brand co-reviewed), `[data-theme="night"]` semantic tier
  (`--cx-*`), scale tokens. The ONLY file where `--rigel-*` and raw hexes may
  appear. Includes the `prefers-reduced-motion` zeroing block and the
  `data-reduce="on"` manual mirror, carried from `brand tokens.css` (D9).
- `apps/ui/src/design/base.css` — reset, body type (Space Mono, 12px UI
  base, `font-synthesis: none` — pixel-face hygiene per `brand tokens.css`),
  scrollbar, `:focus-visible` ring application, `.cx-md` typography.
- `apps/ui/src/design/components/*.css` — one file per component contract
  (`button.css`, `palette.css`, `loader.css`, …), consuming semantic + scale
  tokens only.
- `apps/ui/src/components/primitives/` — the small first-party SolidJS
  primitive set (Button, Input, StateDot, Badge, Tabs, TreeRow, Palette,
  Loader…), Kobalte-wrapping where D3 says so. compass-ui owns the `.tsx`
  internals; this record owns their rendered contract.
- `apps/ui/src/keyboard/` — command registry (`commands.ts`), keymap
  (`keymap.ts`), focus-zone controller (`zones.ts`). Contracts (registry
  entry shape, keymap table shape) are frozen here; internals are
  compass-ui's.
- CI guard: stylelint bans raw hex + `--rigel-*` outside `tokens.css` (D2's
  consumption rule, enforced) and literal duration/easing values outside
  `tokens.css` (D9's motion consumption rule — same guard, second property
  set). One narrow carve-out: the mark component's CSS may name `--rigel-purple`
  directly (D2/D8 — purple is never aliased into `--cx-*`, so the mark is the
  sole sanctioned direct primitive consumer); the guard allowlists
  `--rigel-purple` in the mark file only.
- Rejected layouts: a workspace package (`packages/design-tokens`) and W3C
  design-token JSON + codegen — both recorded in
  `## Alternatives considered`.

### D8 — Brand seam + the mark + editor-theme mapping

- Brand owns the frozen spec; Compass **cites** it and mirrors `--rigel-*`
  values into the primitives block of `tokens.css` with provenance. When
  brand revises, the delta lands as a one-block PR that brand co-reviews. The
  old draft's "pending-freeze" variables are gone: purple is frozen
  (`#a66ef5`, mark-only), the type system is frozen (three faces — the old
  "IBM Plex Mono base" language is superseded; Plex survives only as the
  fallback inside the `--rigel-mono` stack, `brand type.md`).
- **The mark in the ADE.** The topbar brand slot renders per the brand
  surface table (`brand identity.md`): the wordmark (sigil-led — the bitmap R
  IS the mark; no icon-beside-wordmark lockup, the one-R rule) or, in
  icon-only contexts, `icon-navy`; never below the 16px icon floor / 24px
  wordmark floor — below the floor the mark is omitted, not shrunk. This is
  the ONE purple per surface; `icon-phosphor` (the `#b57eff` variant) appears
  only as the sanctioned loading/active brand moment. The Compass needle mark
  (`brand compass-mark.md`) is a placeholder, not locked — the ADE uses the
  R-family assets until brand locks it.
- **Editor-theme mapping** (`--cx-ed-*`) is Compass-owned: semantic tier →
  Shiki theme and editor-pane colors, from the same Night Owl set brand fixes
  as "driving the Compass editor theme" (`brand color.md`), so an embedded
  editor is indistinguishable in palette from the chrome around it. The
  syntax ramp consumes the `brand color.md` §Syntax/UI table (blue functions,
  cyan operators, magenta keywords, `#22da6e` diff-add, amber strings, coral
  numbers, red errors, `#addb67` attributes) — the five untokenized values
  route through Q2. compass-ux co-reviews brand's use of the editor theme;
  brand co-reviews the primitive block. This is the mutual co-review Global
  Constraint 4 encodes.

### D9 — Motion: consume the frozen brand motion system, pure CSS/SVG

New decision — the pre-freeze draft deferred motion to a follow-up record;
the brand motion system is now frozen (`brand motion.md`) and this record
consumes it. The product UI expresses it in **pure CSS/SVG** — no client-side
animation runtime (no GSAP/Three.js/Lenis/Barba; that stack is the marketing
showcase's, `brand motion.md` §"The tech stack" scopes the product-UI
framework out, and Global Constraint 3 bans heavy deps). The as-built
rigel.build site proves the restraint end of the curve works in pure CSS/SVG
(`brand surfaces.md`).

- **Tokens, consumed through the semantic tier** (D2 aliases the brand motion
  primitives so component CSS never names `--rigel-*` directly):
  `--cx-motion-fast` (80ms, user-caused feedback), `--cx-motion-base` (140ms,
  system-caused change), `--cx-motion-slow` (320ms, rare full-view morph — if a
  motion needs to be slower than 320ms to read, the motion is wrong),
  `--cx-ease-out` (settle), `--cx-ease-morph` (shared-element), `--cx-pulse-color`
  / `--cx-pulse-period` (1.6s), `--cx-stream-char-ms` (12ms streamed-text
  cadence), `--cx-cursor-blink` (1s). **Consumption rule:** components
  never hardcode a duration or easing; a literal `200ms` is a review failure
  like a literal hex (enforced by the D7 stylelint guard).
- **The working pulse.** The one recurring animation in the ADE: the
  `working` state dot pulses at the brand pulse cadence in the working-state
  green `#addb67` — a state color, never the purple phosphor
  (`brand state-icons.md`; `brand motion.md` §"The one motion accent"). At
  most one unbounded pulse per viewport region; the scannable state-dot
  column (tree, board gutter) is the sanctioned exception
  (`brand motion.md` pulse-budget Requirement).
- **Loaders.** The chase-light primitive (`.cx-loader`, D3): spinner (closed
  square loop) for indeterminate work, bar (open track, blue fill + fog
  head — the non-purple loading palette, `brand color.md` §"The loading
  palette") for determinate progress. No indeterminate bars (`brand
  surfaces.md` §"Per-layer mapping"). The purple spinner is the one
  sanctioned purple loader — the brand-mark-in-motion moment — used at most
  once per surface and never adjacent to a state-dot field where a
  purple-vs-green ring pair could read as state.
- **Streaming (the signature interaction).** Agent token-streaming on the trace
  surface (where turns actually stream — DL-039/DL-099) is the one place
  richness is always-on, because it *is* the product. The contract, consumed
  from the brand micro-excellence system (`brand motion.md`): the network layer
  buffers bursty model output and the visual layer drains it at a steady cadence
  (`--cx-stream-char-ms`, ~12ms/char, rate-adaptive so it never falls behind) —
  bursty network, smooth screen; a freshly-revealed character ignites at the
  phosphor `--cx-pulse-color` and cools to fog behind the write-head, the block
  cursor blinking at `--cx-cursor-blink`; partial-markdown/code-fence safety
  holds dangling inline markup and defers a code block to Shiki until its fence
  closes, so nothing flickers mid-stream; the region is an `aria-live` surface
  (`aria-busy` while streaming) so motion is never the sole "streaming" signal.
  Solid's fine-grained reactivity makes the per-character treatment cheap at
  many concurrent streams (the settled prefix is one text node; only a bounded
  trailing window are individual igniting spans).
- **Everyday motion.** Everything else is plain ease-out translate + fade at
  `--cx-motion-fast`/`--cx-motion-base`: hover/press feedback, panel
  open/close, row settle, toast arrival.
- **First-load boot-sequence.** The brand names Compass first-load the
  highest-leverage wow (`brand motion.md` §"The effort/wow curve"): a staged
  terminal-honest reveal — bitmap R pixel-assembly, phosphor pulse, wordmark,
  UI fades in behind. Consumed here as a design surface, CSS/SVG only
  (pixel-assembly is explicitly "cheap: SVG/CSS `steps()`" per
  `brand motion.md`), skippable, deferred behind idle time, reduced-motion
  honored. Specified in T8.
- **Reduced-motion: substitution, not removal.** Motion becomes an instant
  state change or a ≤80ms opacity crossfade; the information survives, only
  the travel is dropped. `tokens.css` zeroes all durations and
  `--rigel-pulse-period` under `prefers-reduced-motion` plus the
  `data-reduce="on"` manual mirror (both carried verbatim from
  `brand tokens.css`). Motion never sole-carries meaning: every state a
  motion conveys is also carried by glyph/color/text (the state-dot set is
  static-distinct by construction, D3).

### D10 — Adoption path: design-first, migration incremental

The design is from scratch; the code migration is not big-bang — the app
ships working throughout. Sequencing (detail in `## Adoption path`): tokens +
base land first behind `[data-theme="night"]`, components re-clothe
surface-by-surface, keyboard/palette lands as pure addition, old vocabulary
is deleted as each surface flips. **This record's merge is independent of the
in-flight impl lanes** (SEA-1645 unreachable-pin, SEA-1633 board remodel):
they continue on the current vocabulary. Adoption step 4's board/sidebar
flips sequence AFTER those lanes merge. Coordination call: if T1-T3 land
before SEA-1633 reaches its styling, SEA-1633 should build directly against
`.cx-*` and skip the build-on-legacy-then-re-skin double-work — a compass-ui
coordination note, not a blocker.

## Alternatives considered

Recorded for the record — the styling tech is frozen (Global Constraint 3),
so these are documentation of why, not open forks.

- **Tailwind CSS** — utility classes would speed prototyping, but the token
  system IS the deliverable here; Tailwind adds a config-DSL layer between
  brand tokens and CSS, a build dependency, and a class-soup idiom foreign to
  the existing `class + data-*` contract convention. Rejected (frozen).
- **A full component library** (Ark full builds, shadcn ports, Hope UI…) —
  ships someone else's look and density; the ADE look is the product's
  identity, and re-theming a library costs more than owning ~20 small
  primitives. Kobalte stays, scoped to a11y-hard behavior only. Rejected
  (frozen).
- **CSS-in-JS** (vanilla-extract, Panda) — runtime or codegen cost, a second
  styling idiom beside plain CSS, and no benefit over custom properties for a
  single fixed-shell app. Rejected (frozen).
- **A client-side animation runtime for product motion** (GSAP et al.) — the
  brand's award-site stack is scoped to the marketing showcase; the product
  UI's motion vocabulary (pulse, chase-light loaders, translate+fade,
  streaming cadence) is fully expressible in CSS/SVG, as the as-built
  rigel.build site demonstrates. Rejected (Global Constraint 3 + brand
  scoping; D9).
- **W3C design-token JSON source + codegen** — right when tokens feed
  multiple format targets. Compass has one consumer (this webview) plus an
  editor theme both expressible in CSS; the JSON layer is pure indirection
  today. Rejected; revisit only if brand needs multi-format export (that
  would be brand's record).
- **Workspace package for tokens** (`packages/design-tokens`) — a clean seam
  in theory, but there is exactly one consumer; a package adds versioning and
  build coupling with no second consumer in sight. In-tree
  `design/tokens.css` wins; extraction later is mechanical if a second
  consumer appears.
- **Preserve-and-extract (the closed prior record)** — systematize the
  current `app.css` values with zero visual change. Closed by Matt as the
  wrong altitude: it would freeze an undesigned prototype's look as the
  system. Superseded by this record's from-scratch target.
- **Command palette as a third-party widget** (kbar-style libs; none are
  Solid-native and mature) — the palette is a flagship surface and the
  registry must be our contract; Kobalte's combobox + our registry is the
  same effort with full ownership. Rejected.
- **Re-deriving the state-dot vocabulary in this record** (the pre-freeze
  draft's four-round ring-arc/glyph derivation) — superseded wholesale: brand
  froze the eight-glyph set and explicitly ruled out the ring-arc working
  form ("read as a bare 'C' at 12px", `brand state-icons.md`). Deleted, not
  carried; do not resurrect.

## Adoption path

Design-first: the system is specified in full by this record; the code moves
to it incrementally so `apps/ui` ships working at every step.

1. **Tokens + base land** (`design/tokens.css`, `design/base.css`,
   `[data-theme="night"]` on the root). The semantic tier initially coexists
   with the legacy `--bg/--st-*` variables; nothing visual flips yet except
   the scrollbar and the `:focus-visible` ring (deliberately in step 1 — an
   a11y defect, not a cosmetic; the type flip waits for step 2).
2. **Shell + chrome re-clothe** — the type system lands (Space Mono body,
   the frozen `--rigel-mono` stack; Departure display slots where D2 permits
   them); topbar, sidebars, usage bar, view-tabs move to `.cx-*` contracts
   and semantic tokens. First visible identity change.
3. **Keyboard spine lands as pure addition** — focus zones, keymap registry,
   command registry + palette. No surface rewiring required to start; each
   surface registers commands as it flips.
4. **Surface-by-surface flips**, each its own PR, legacy selectors deleted in
   the same diff (no shims): left sidebar/tree → Bridge board → comms
   (channel/topic) → right sidebar → workspace/trace → Backlog/Done/Settings.
   Ordering tracks the frozen-IA implementation lanes: the board flip lands
   after SEA-1633's remodel merges; the right-sidebar flip lands after SEA-1645
   (unreachable-pin, DL-113; the left sidebar/tree flip has no such dependency).
5. **Legacy vocabulary retired** — `--bg*`, `--st-*`, `--accent*` and orphan
   selectors deleted; the stylelint guard flips from warn to error; done
   means `app.css`'s `:root` block (`app.css:7-58`) is gone.

In-flight lanes SEA-1645 (unreachable-pin) and SEA-1633 (board remodel)
continue on the current vocabulary and re-skin at their surface's flip step —
they neither block nor are blocked by this record.

## Plan

Every task inherits `## Global Constraints`. Tasks are design/spec + initial
delivery slices sized for their own review cycle; compass-ui executes the
`.tsx` internals against the contracts each task freezes.

**T1 — Token tiers: author `design/tokens.css`** (D2, D8)
Author the three-tier token file: `--rigel-*` primitives block (mirrored
verbatim from `brand tokens.css` with provenance comment; the Q2 untokenized
values enter per that question's resolution), `[data-theme="night"]` semantic
tier (surfaces, text, lines, accent, `--cx-scrim`, status, the eight
`--cx-st-*` agent states, the five `--cx-issue-*` board-lane colors,
CI/review, and the `--cx-ed-*` editor block reserved here as names only — T7
authors its values), scale tokens (space/type/radius/elevation/z; motion and
focus consumed from the brand tier, D9/D4), and the reduced-motion zeroing
block plus its `data-reduce="on"` mirror. Verify WCAG contrast of every text token on each
surface tier against the `brand color.md` contrast table.
Interfaces: consumes `brand tokens.css` + `brand color.md` verbatim; produces
`apps/ui/src/design/tokens.css` defining the complete `--rigel-*`/`--cx-*`
vocabulary D2 names. Brand co-reviews the primitives block.

**T2 — Base layer + consumption guard** (D1, D2, D7, D9)
Author `design/base.css` (reset, Space Mono 12px UI type ramp,
`font-synthesis: none`, scrollbar, `:focus-visible` ring application,
`.cx-md` typography) and the stylelint CI check banning raw hex, `--rigel-*`,
and literal durations/easings outside `tokens.css` (warn until adoption
step 5, then error).
Interfaces: consumes T1's token vocabulary; produces
`apps/ui/src/design/base.css` + a stylelint config block in the UI package
lint setup (`apps/ui/package.json` lint script gains stylelint).

**T3 — Component visual contracts** (D3, D4)
Author the per-component contract specs + CSS for the D3 table: class name,
`data-*` variants, all six states (rest/hover/active/selected/disabled/
focus), consumed tokens, and per-component notes (card selection rule, pane
focus rule). The state dot consumes the frozen `brand state-icons.md`
glyph set as inline 9×9 `crispEdges` SVG — this task transcribes, it does not
re-derive; verify glyph legibility at the 12px row-dot render size against
the brand assertion. One CSS file per component under `design/components/`.
Interfaces: consumes T1 tokens + T2 base + `brand state-icons.md`/`brand
spine.md`; produces `apps/ui/src/design/components/*.css` + a contract table
(component · classes · data-attrs · states · tokens) appended to this
record's directory as `components.md`. compass-ui co-reviews.

**T4 — Command registry + keymap contract** (D5)
Freeze the command-registry entry shape (`{ id, title, keywords, scope,
shortcut?, run }`), the destination-provider interface (async, ranked), the
keymap table (D5 defaults, `Ctrl`↔`Cmd` abstraction), the focus-zone model
(zone ids, roving-tabindex rule, escape ladder), and the
primary-action-takes-a-`command`-id enforcement rule.
Interfaces: consumes D4/D5 of this record; produces
`apps/ui/src/keyboard/{commands,keymap,zones}.ts` contract stubs (typed
interfaces + default keymap table, no behavior) that compass-ui implements
against. compass-ui co-reviews.

**T5 — Command palette surface** (D3, D5)
Design + CSS for `.cx-palette`: Kobalte combobox composition, layout (560px,
top-third, elev-3), result-row anatomy (type glyph · title · dim context ·
shortcut chip), mode behavior (actions + destinations interleaved, scope
ranking), empty/loading states (loading uses the chase-light bar, D9).
Interfaces: consumes T3 row/badge contracts + T4 registry shape; produces
`design/components/palette.css` + the palette section of `components.md`.

**T6 — Surface composition specs** (D6)
Per-surface composition spec (shell, agent tree + channel rail, right sidebar +
pins, Bridge board, channel/topic comms, agent workspace = home channel +
session trace, Backlog/Done/Settings): which components compose it, zone +
keyboard behavior, empty states (pins-empty, tree-empty), mark placement per
the brand surface table, and the per-surface flip checklist the adoption path's
step 4 PRs execute. The four **excellence surfaces** get a fuller design pass
each, starting from their concrete reference (never a blank-page redraw): Bridge
board from the Rigel brand source (`BridgeBoard.astro`); Manager tree
from `AgentTree.astro`; channels + threads from **Zulip's topic-threading UX**
(+ `ThreadView.astro`); the session trace from the brand streaming treatment
(T8). Also specifies the **window-scoped-view contract** (D6.1): each top-level
surface mounts standalone against its DL-127 route with its own focus
zones/command scope and no sibling-region dependency, so compass-native can host
it in its own window.
Interfaces: consumes T3 contracts + frozen IA records
(DL-095/096/098/099/067/070/097/039/127/129) + `brand identity.md` + the named
Rigel-site mockups; produces `surfaces.md` in this record's directory — the
checklist each flip PR cites.

**T7 — Editor-theme mapping** (D8)
Derive the `--cx-ed-*` set: Shiki theme JSON and editor pane colors from the
semantic tier + the `brand color.md` §Syntax/UI ramp; verify against real
trace and markdown content in the workspace.
Interfaces: consumes T1 tokens + `brand color.md`; authors the values of the
`--cx-ed-*` block T1 reserved, plus a generated Shiki theme
(`design/editor-theme.json`). compass-ux co-reviews brand's downstream use;
brand co-reviews palette fidelity.

**T8 — Motion primitives spec** (D9)
Specify the CSS/SVG implementations: the working pulse (keyframes at
`--cx-pulse-period`, green, one-per-region budget with the state-dot-
column exception), the chase-light spinner + bar (`.cx-loader`, cell
keyframes per `brand spine.md`'s seed-reference mechanics, `crispEdges`),
streamed-text cadence + cursor blink, everyday translate+fade patterns, the
first-load boot-sequence (staged, skippable, idle-deferred), and the
reduced-motion substitution for each.
Interfaces: consumes T1 tokens + `brand motion.md`/`brand spine.md`; produces
`design/components/loader.css`, the pulse/stream keyframe blocks in
`base.css` (consuming the `--cx-*` motion aliases, never `--rigel-*`
directly, so the D7 guard stays clean), and a `motion.md` spec in this
record's directory. Brand co-reviews fidelity to the frozen motion system.

**T9 — Adoption step 1-2 execution: tokens/base/shell land** (D10)
Land T1+T2 output in-tree, flip the shell chrome (topbar, sidebar frames,
usage bar, view-tabs) to `.cx-*`, fix the two `outline: none` defects, keep
all other surfaces on legacy vocabulary.
Interfaces: consumes T1-T3 outputs; produces the first shipping PR of the
adoption path (app runs, shell re-clothed, everything else visually legacy
but functional).

## Tasks

- [ ] T1 — Token tiers: author `design/tokens.css` (primitive mirror/semantic/scale; brand co-review)
- [ ] T2 — Base layer + stylelint consumption guard (hex + `--rigel-*` + literal durations)
- [ ] T3 — Component visual contracts (`design/components/*.css` + `components.md`; frozen state-glyph transcription; compass-ui co-review)
- [ ] T4 — Command registry + keymap + focus-zone contracts (`keyboard/*.ts` stubs; compass-ui co-review)
- [ ] T5 — Command palette surface (`palette.css` + spec)
- [ ] T6 — Surface composition specs (`surfaces.md`: per-surface flip checklists, mark placement, the four excellence surfaces from their references, the window-scoped-view contract)
- [ ] T7 — Editor-theme mapping (`--cx-ed-*` + Shiki theme; brand co-review)
- [ ] T8 — Motion primitives spec (pulse, chase-light loaders, boot-sequence, reduced-motion; brand co-review)
- [ ] T9 — Adoption steps 1-2: tokens/base/shell land, focus defects fixed

## Open Questions

**Stated assumption (not an open question):** the semantic-token prefix is
`--cx-*` — already the convention the brand token file itself uses for the
inherited Compass tier (`--cx-motion-*`, `--cx-focus-ring` in
`brand tokens.css`), so it is now grounded upstream, not merely this record's
choice.

### Resolved decisions (Matt, 2026-08-05)

The four load-bearing forks the draft raised are ruled and folded into the
decisions above; recorded here for provenance.

1. **Q1 — `--cx-issue-in_review` = amber** (`--rigel-amber`). In-review reads
   as awaiting-human-attention, the same intent family as agent `waiting`;
   blue stays rationed to interaction. The cross-axis amber reuse is safe —
   board-lane tints are low-alpha lane-head washes (D6), never in the same
   visual position as an agent state dot, and the namespaces stay separate
   (D2). (Magenta and blue weighed and rejected.)
2. **Q2 — brand tokenizes the six values upstream; raw hex is the interim.**
   Compass files a one-block PR to brand to add the six `brand color.md`
   values (selection `#1d3b53`, faint `#637777`, magenta `#c792ea`,
   success-green `#22da6e`, coral `#f78c6c`, loading-empty `#0a2036`) to
   `brand tokens.css` under brand-chosen names, then mirrors them. Until
   brand lands them, Compass carries them as provenance-commented raw hexes in
   its primitives block (the one place raw hex is legal, D2) so T1 is not
   blocked; the later swap to brand's names is mechanical.
3. **Q3 — limited Departure Mono in the ADE.** Departure (`--rigel-display`)
   appears only at large brand-display moments on even 11px multiples: the
   boot-sequence wordmark and empty-state/settings headers at 22px. Nothing in
   dense chrome — the ≤16px workhorse rule holds (D1.4, D2).
4. **Q4 — the agent workspace simplifies to home channel + session trace;
   terminals removed.** No terminal pane and no file-viewer pane in dogfood.
   Agents run in isolated containers; an operator-facing "launch a dev server
   for the operator to view" affordance is deferred to its own backlog issue,
   not dogfood. PR review happens directly on the user's forge, so in-app file
   browsing is out of scope (a later file viewer is possible but low-priority).
   D6's workspace is rewritten to the two fixed panes accordingly; the terminal
   `PaneKind` arm + `newTerminalPane` in `store.ts` retire at the workspace
   flip step (D10). New decision DL-158.

Remaining deferrals, carried from the pre-freeze draft (none load-bearing):

1. **[NLB] User-remappable keybindings surface.** The keymap registry is a
   single table precisely so a remap UI is possible; the UI itself (settings
   surface, conflict detection) is deferred — the defaults are correct
   without it. Recommendation: defer; file a follow-up issue when T4 lands.
2. **[NLB] Light Owl (day theme) shipping scope.** Architecture is held open
   by `[data-theme]` indirection (D2) and brand fixes light mode as a
   polarity inversion (`brand color.md`), but no day semantic tier is
   authored in this record's tasks. Recommendation: defer until brand
   publishes light-mode tokens; authoring it then is a one-file addition.
3. **[NLB] Palette destination-provider ranking (recency + fuzzy weights).**
   D5 fixes the provider interface and the ranking inputs; exact weighting is
   a tuning matter with no contract impact. Recommendation: defer to T5
   implementation, tune against real fleet data.
4. **[NLB → Beta milestone] In-window tabs (Linear-style) and split views.**
   The multi-window decomposition (D6.1) is designed to admit both — a tab
   strip or a splitter hosts the same window-scoped views with no rework — but
   neither ships in dogfood scope. Filed as **SEA-1808** (Beta milestone; Matt,
   2026-08-05). Recommendation: defer to Beta; the decomposition is the only
   part that must be right now, and it is.

## Ledger-impact

One row per citable decision. Provisional block **DL-148..DL-160** (compass
ledger max on main is DL-147); compass appends the rows to `DECISIONS.md` at
ship after confirming this block — this record does not write the ledger.
IDs do not shift positionally.

| ID | Decision |
| --- | --- |
| DL-148 | Compass visual language is the frozen Rigel chase-light/dot-matrix spine worn at ADE density: surface-color elevation, blue-rationed interaction contrast (purple mark-only), 4px-grid density, mono-as-identity per the three-face type system, keyboard-answerable everything (D1) |
| DL-149 | Three-tier token model: `--rigel-*` primitives mirrored verbatim from the frozen brand token set (no invented names, no violet) → `--cx-*` semantic tier → scale tokens; one downward consumption rule (component CSS names only `--cx-*`, so the brand motion primitives are aliased into `--cx-motion-slow`/`--cx-ease-*`/`--cx-pulse-*`/`--cx-stream-char-ms`/`--cx-cursor-blink`); `[data-theme]` indirection keeping the light pair open; agent-state colors per brand (working `#addb67` green, done cyan, waiting/disconnected amber, idle/paused/stopped mute); issue lanes remapped off the dead violet (D2) |
| DL-150 | First-party component system of ~20 primitives specified as class + `data-*` visual contracts with six mandatory states; the state dot consumes the frozen brand eight-glyph vocabulary (working `»`, idle 3×3 block, waiting `?`, done tick, paused bars, stopped hollow square, error `!`, disconnected broken square) as 9×9 crispEdges SVG; Kobalte scoped to a11y-hard behavior only (D3) |
| DL-151 | Focus model: the single upstream-frozen `--cx-focus-ring` (2px solid `--rigel-blue`) on `:focus-visible`, four interactive shell focus zones (left/main/right/topbar; the usage bar is a display-only landmark until it gains a control) with roving tabindex, pane-focus rule, escape ladder (D4) |
| DL-152 | Command palette (Cmd/Ctrl-K, registry-backed, actions + destinations) and global keymap (direct chords) as first-class surfaces; every primary action registers a command; carried intact across the brand freeze (D5) |
| DL-153 | The frozen IA renders through the component system surface-by-surface (agent tree, pinned right sidebar, Issues/PRs board, channel→topic comms, agent workspace = home channel + session trace) in BOTH render hosts (Wails desktop app + browser); four primary surfaces (Bridge board, Manager tree, channels+threads, session trace) carry an explicit excellence bar with concrete starting points (the Rigel-site mockups, Zulip's threading UX, the brand streaming treatment) (D6) |
| DL-154 | Delivery layout: in-tree `apps/ui/src/design/` (tokens.css/base.css/components/) + `keyboard/` contracts; stylelint guard bans raw hex, `--rigel-*`, and literal durations outside tokens.css, with one narrow allowlist — the mark component's CSS may name `--rigel-purple` directly (purple is never aliased into `--cx-*`); no token package, no W3C token source (D7) |
| DL-155 | Brand seam: primitives mirrored with provenance from the frozen spec; the mark per the brand surface table is the one purple per surface with the 16px floor honored; Compass-owned `--cx-ed-*` editor-theme mapping from the Night Owl syntax ramp; mutual co-review (D8) |
| DL-156 | Motion: the product UI consumes the frozen brand motion system in pure CSS/SVG (no client animation runtime) — brand duration/easing/pulse/streaming tokens, the green working pulse with the one-pulse-per-region budget, chase-light spinner/bar loaders, the boot-sequence, reduced-motion as substitution not removal; literal durations are review failures (D9) |
| DL-157 | Adoption path: design-first, five-step incremental migration (tokens → shell → keyboard spine → surface flips → legacy retirement); in-flight lanes SEA-1645/SEA-1633 re-skin post-merge (D10) |
| DL-158 | The agent workspace simplifies to the agent's home channel + its session trace (two fixed panes, no arbitrary split tree); no terminal pane and no file-viewer pane in dogfood (isolated containers; an operator dev-server-view affordance is deferred to backlog, PR review lives on the user's forge); the terminal `PaneKind` arm + `newTerminalPane` retire at the workspace flip (D6/D10) |
| DL-159 | One UI codebase renders in two hosts — the Wails v3 desktop app (primary) and the browser (the managed/hosted product at `compass.rigel.build`) — over the same transport-agnostic UI above the `connection.ts` provider seam; the layout is fluid within its window/viewport (a dense supervision surface that reflows, not a fixed-pixel canvas and not a mobile redesign) (D6/§Global Constraints 2) |
| DL-160 | The desktop app is first-class multi-window: every top-level surface (Bridge, a channel, an agent workspace, Backlog/Done, Settings) is an independently mountable window-scoped view (own DL-127 route, own focus zones + command scope, no sibling region required); compass-native spawns/manages OS windows, this record owns the decomposition; in-window tabs (Linear-style) and split views are deferred to the Beta milestone (SEA-1808), admitted by the same decomposition without rework (D6.1) |
