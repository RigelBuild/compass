# Design: Compass Bridge re-clothe (SEA-2111)

Status: Draft
Owner lane: compass-ux (design) → compass-ui (execution)
Refs: SEA-2111 (live board doesn't match the rigel.build reference); sequenced
after the SEA-2034 DS-token cutover (merged, main `18e988b5`). Two adjacent
concerns are split into their own lanes/PRs, not folded here: the state-dot
pixel-art glyph adoption → SEA-2118 (mechanical frozen-spec adoption, global
across surfaces); review/CI badge semantic clarity → SEA-2117 (its own design
pass — see T4). This record is the board-structure re-clothe and ships on the
current state-dots.
Governing spec: `apps/ui/src/design/surfaces.md` §"Bridge — the Issues and PRs
board" (L196–268, frozen T6/SEA-1816)
Reference render: `apps/rigel.build/src/components/BridgeBoard.astro` (brand
repo, read-only)

## Problem / Intent

The live Bridge board (`apps/ui/src/components/Bridge.tsx`) renders on the
merged `--cx-*` tier but still wears its pre-DS clothing: GitHub-board-style
bordered cells, rounded priority-striped cards, a plain 13px toolbar heading,
and a PRs tab that is a grouped list rather than the board the frozen spec
describes. The rigel.build reference render (`BridgeBoard.astro`) is the
excellence bar the surfaces spec names as "the starting point … not a redraw"
(`surfaces.md:202-203`). This record lifts the reference's visual structure
onto the live component — real data, real interaction, only the clothing
changes.

## Global Constraints

Every task below inherits these; none restates them.

- **Stack:** SolidJS ^1.9.13 + Vite + TypeScript; pure CSS custom properties.
  No Tailwind, no component library, no CSS-in-JS.
- **Tokens:** consume the `--cx-*` semantic tier ONLY. No raw hex, no
  `--rigel-*` refs, no literal durations/easings outside
  `design/tokens.css` — the D7 stylelint guard bans all four at error
  (`apps/ui/.stylelintrc.cjs:26-61`: `color-no-hex`, hex-anywhere regex,
  `/var\(\s*--rigel-/`, legacy-var vocabulary, raw motion literals on
  `transition|animation`). Reference `--rigel-*` values translate through their
  existing `--cx-*` aliases (`tokens.css:111` `--cx-accent: var(--rigel-blue)`,
  `tokens.css:139-143` `--cx-issue-*`, `tokens.css:146-151`
  `--cx-ci-*`/`--cx-review-*`).
- **`tokens.css` is read-only for this lane.** No new token is coined here; a
  need the tier can't answer is an Open Question, never a unilateral mint.
- **Motion axis is D9/foundation-T8's** (`apps/ui/src/design/motion.md`). This
  record consumes existing motion tokens (`--cx-pulse-period`,
  `--cx-ease-out`, `--cx-motion-fast/base`) and never coins a duration or
  easing. The advancing-card affordance the reference shows is not in
  motion.md's primitive list — it ships as a dormant hook (T5, Decision D2),
  not a coined token.
- **State-dots and badge-clarity are OUT of this lane.** The state-dot
  pixel-art glyph adoption is SEA-2118 (its own global PR — Matt ruled the
  glyphs ship at 9px; grounded blast radius is 5 files + the `.r-tab` overlay,
  too wide to fold into a board-scoped PR). The review/CI badge semantic-clarity
  redesign is SEA-2117 (its own design pass); T4 here recolors the existing
  badges onto the semantic tier and consumes whatever badge FORM that design
  freezes. The board keeps today's `StateDot` unchanged; no `.state-dot` rule
  is touched here.
- **Not a redraw.** `surfaces.md:202-203`: "The starting point is
  `BridgeBoard.astro` (authored against the real `Bridge.tsx` /
  `IssueCard.tsx` / `board.ts`), not a redraw." The live component's data
  wiring (`board.ts` / `board-render.ts` accessors), selection model
  (`store.selectIssue` / `store.openAgent`), click/dblclick/chip a11y
  compromises (`IssueCard.tsx:56-59`, `Bridge.tsx:69-71`), and the
  Swimlanes/Status grouping toggle (`Bridge.tsx:98`, `160-175`) all survive
  unchanged.
- **No fabricated data.** Where the Astro mock shows a fact the live store
  cannot source (the `advancing` flag, `BridgeBoard.astro:39,70`), the gap is
  handled as a dormant hook — never a stubbed fake field presented as real.
- **Sticky behavior is load-bearing.** The live board scrolls
  (`app.css:510-512` `.swimlane { flex: 1; overflow: auto; … }`) with sticky
  column heads (`app.css:536-539`) and sticky lane gutters (`app.css:566-569`).
  The reference does not scroll (it caps at `max-width: 1040px`,
  `BridgeBoard.astro:434-437`, with a mobile `overflow-x: auto` fallback only,
  `:612-618`). Every lifted visual must keep the sticky/scroll model working.
- **Ledger coupling:** this is a product design record; its freeze requires a
  same-PR `docs/designs/product/DECISIONS.md` delta or a `Ledger-impact:` PR
  line. The driver handles this at PR time. Candidate ledger rows are flagged
  in §Ledger below.
- **Verification vehicle:** the Playwright visual-smoke harness from the
  cutover (`apps/ui/e2e/visual-smoke.spec.ts`, `apps/ui/playwright.config.ts`)
  is extended, not reinvented. Every visual slice re-runs it and the driver
  attaches before/after PNGs for Matt.
- Format + lint + tests (`biome`, `moon run compass-ui:stylelint`, vitest,
  `test:visual`) run per slice before it is called done.

## Decisions (Matt-ruled 2026-08-16, via ask)

These closed the load-bearing forks the draft surfaced; the plan below is
written against them.

- **D1 — PRs view is board-ified** (was OQ-1). The PRs tab becomes the same
  swimlane grid on PR-lifecycle columns, per `surfaces.md:206-210` and the
  reference. Two sub-rulings: **(a) unresolved review threads gate a PR to "In
  review"** — an approved + CI-green PR with open threads is *not* "Ready to
  merge"; **(b) the "Merged" column shows PRs merged within a 1-day window,
  then they drop** — and that windowing is **handled server-side / in the
  store lane**, not in this UI. The UI renders whatever merged rows the store
  hands it; T6 does not implement retention.
- **D2 — advancing card ships as a dormant CSS hook** (was OQ-2). Existing
  motion tokens only; wired to no data until a real source lands. The
  motion-spec addition routes to D9/foundation-T8, the data field to the store
  lane.
- **D3 — CI/review badges: recolor onto the semantic tier; FORM per SEA-2117.**
  T4 re-points the badges from the raw `--cx-ok/error/warn` they use today onto
  the dedicated `--cx-ci-*`/`--cx-review-*` tier, and adopts whatever badge
  form the SEA-2117 badge-clarity design freezes (pixel-art 1-bit glyphs
  leading). The `.cx-badge` naming in `surfaces.md` is the token/vocabulary
  contract, not a labelled-DOM-box mandate. The live `commented` verdict maps
  to `--cx-review-pending`. If SEA-2117 has not frozen by the time T4 runs, T4
  ships the interim compact recolored pip and the glyph form lands as a fast
  follow — T4 does not block on it.
- **D4 — roving-tabindex 2-D keyboard grid is OUT of scope** (was OQ-5): filed
  as a follow-up interaction issue at dispatch; this record changes only the
  clothing, not the keyboard model.
- **D5 — state-dot glyph adoption is a separate lane** (SEA-2118, see Global
  Constraints); the board ships on the current `StateDot`.

## Approach

One PR train of six right-sized slices over `Bridge.tsx`, `IssueCard.tsx`, and
the `.bridge*`/`.swim*`/`.card*`/`.pr-*` blocks of `app.css`, in the order:
harness extension → grid shell → cards → badges → advancing hook → PRs board.
Each slice deletes the legacy selectors it replaces in the same diff
(`surfaces.md:267` flip item 8).

The reference is lifted at three levels:

1. **Structure** — the hairline grid. The reference draws its grid as a 1px
   `gap` over a `--rigel-night-2` container background with cells on
   `--rigel-night` (`BridgeBoard.astro:434-450`), which reads as hairlines.
   That exact technique is **not** sticky-safe: the live board scrolls under
   sticky heads, and grid gaps are windows to the scrolled content behind
   them. The re-clothe therefore keeps the live border technique
   (`app.css:609-611`) but tunes it to the hairline look — uniform 1px
   `--cx-border` cell borders, cells on `--cx-bg`, lane gutters on
   `--cx-bg-panel` (the reference's `.bridge-lane { background:
   var(--rigel-panel) }`, `BridgeBoard.astro:459-465`), and the board frame on
   a single outer `--cx-border` border. Same optics, sticky-safe mechanics.
2. **Composition** — the frozen `surfaces.md` §Composition contract
   (`surfaces.md:224-234`): cells are `.cx-card` on the panel tier; selection
   is `--cx-bg-selected` plus the accent left rule, never a raised background;
   the column-head tint consumes `--cx-issue-*` at low alpha in the lane head
   only; badges live on `--cx-ci-*`/`--cx-review-*`; the issue cross-link is a
   `.cx-chip`. The DS component CSS exists but is imported nowhere
   (`App.tsx:4-6` imports only `tokens.css`, `base.css`, `app.css`) — the
   card/badge component files get wired in as part of their adoption slices.
3. **Type hierarchy** — the reference quiets the issue key
   (`.card-issue { color: var(--rigel-mute); letter-spacing: 0.5px }`,
   `BridgeBoard.astro:515-520`) and brightens the title
   (`.card-title { color: var(--rigel-fog) }`, `:556-561`); the live card
   inverts that (key on `--cx-accent` 600-weight, `app.css:678-683`). The
   toolbar heading goes display-face (`--cx-font-display` at
   `--cx-display-sm`, `tokens.css:196,202` — the reference's
   `font-family: var(--rigel-display); font-size: 22px`,
   `BridgeBoard.astro:379-384`) from the live 13px/600 (`app.css:477-480`).
   Column heads go uppercase letterspaced mono per the reference
   (`BridgeBoard.astro:451-458`) — the live heads are already close
   (`app.css:536-552`) and keep their live-data count badge.

Where the DS and the reference disagree on primitive shape (the DS `.cx-card`
carries `--cx-radius-md` and motion-token transitions,
`design/components/card.css:11,17-19`; the reference card is square,
`BridgeBoard.astro:501-508`), **the DS wins on shape** — the reference guides
structure, spacing, and hierarchy; frozen DS components govern primitive shape.
Where **no DS component governs the element** (the CI/review pips, the seg
control), the reference's shape wins (OQ-1, generalizing the draft's pip-only
carve-out).

The PRs view is the one structural change (D1): `surfaces.md:206-210` freezes
"both are the same swimlane grid, only the columns differ — … PRs use the
PR-lifecycle columns (In progress, In review, Ready to merge, Merged)", and the
reference renders exactly that (`BridgeBoard.astro:120-129,309-359`). The live
PRs tab is a grouped flat list (`Bridge.tsx:274-320`, `.pr-tab`/`.pr-group`/
`.pr-row`, `app.css:783-861`). The board-ification is the last slice, driven by
a new pure derivation over the real `PullRequest` shape — no fabricated column
field.

### Alternatives considered

- **Port the reference's gap-grid hairlines verbatim.** Rejected: not
  sticky-safe (above). Border-tuning reproduces the optics without breaking
  the scroll model.
- **Restyle the PRs list in place instead of board-ifying.** Rejected by D1:
  it satisfies "PRs view rows carry the PR's own facts" (`surfaces.md:219-220`)
  but contradicts the same section's "both are the same swimlane grid"
  (`surfaces.md:206-207`) and the reference render. Matt ruled board-ify.

## Gap analysis

Reference (`BridgeBoard.astro`, cited `astro:`) vs live, each row mapped to the
live file+line it changes and the token it lands on. Live line numbers at main
`18e988b5`. (The former G9 state-dot row is removed — it is its own lane,
SEA-2118.)

| # | Reference has | Live has | Live change site | Token(s) |
| --- | --- | --- | --- | --- |
| G1 | Hairline 1px grid: container bg `--rigel-night-2`, `gap: 1px`, cells `--rigel-night` (`astro:434-450`) | Per-cell 1px borders bottom/right on `--cx-border` (`app.css:609-611`), corner/head borders (`app.css:524-525,541-542`) | `.swim-cell`, `.swim-corner`, `.swim-colhead`, `.swim-gutter` border/bg rules; add outer board frame | `--cx-border`, `--cx-bg`, `--cx-bg-panel` |
| G2 | Lane gutter on the panel tier, `padding: 14px 12px; gap: 9px` (`astro:459-465`) | Gutter on `--cx-bg-raised`, `padding: 10px 12px; gap: 8px` (`app.css:566-578`) | `.swim-gutter` (rename `.bridge-lane` per `surfaces.md:211`) | `--cx-bg-panel` |
| G3 | Col heads: 11px mono, `letter-spacing: 1px`, uppercase, `padding: 12px 14px` (`astro:451-458`) | 10px/600, `letter-spacing: 0.05em`, `padding: 8px 12px`, colored `lane-dot` + count (`app.css:536-563`; dot fed `lane.color` from `constants.ts:17-31`) | `.swim-colhead` (rename `.bridge-col-head`); drop the `lane-dot`, keep the live count | column tint: `color-mix(in srgb, var(--lane-tint) 8%, var(--cx-bg-raised))` where `--lane-tint` = the lane's `--cx-issue-*` (OQ-2 alpha) |
| G4 | Column-head tint per `surfaces.md:213-215` ("tinted by the issue-state color at low alpha in the lane head only — contrast rationing") — NOT actually present in the reference CSS (`astro:451-458` is untinted `--rigel-haze`) | No tint | `.swim-colhead` background; `Bridge.tsx:190-194` passes `--lane-tint` inline style | `--cx-issue-queued/blocked/in_progress/in_review/done` (`tokens.css:139-143`) |
| G5 | Flat square card: `bg --rigel-raised`, 1px border, `padding: 9px 10px`, no radius, no left stripe (`astro:501-508`) | `.card`: `--cx-bg-panel`, `--cx-radius-sm`, 3px priority left border (`app.css:626-643,659-670`), selected = accent ring (`app.css:654-657`) | `IssueCard.tsx:46` container class → `.cx-card` + `data-selected`; delete `app.css` `.card` base/selected/priority rules; add the board-scoped internal-layout block (F3) | `.cx-card` contract: `--cx-bg-panel`, `--cx-bg-selected` + accent left rule (`design/components/card.css:5-37`) |
| G6 | Quiet key / bright title: `.card-issue` on mute + 0.5px tracking (`astro:515-520`), `.card-title` on fog (`astro:556-561`) | Key on `--cx-accent` 11px/600 (`app.css:678-683`); title default text (`app.css:695-698`) | `.card-issue`, `.card-title` rules in `app.css` (propagates to DoneView's shared sub-parts, F6) | `--cx-text-faint` (key), `--cx-text` (title) |
| G7 | Display-face board heading, 22px (`astro:379-384`) | 13px/600 UI face (`app.css:477-480`) | `.bridge-toolbar .heading` | `--cx-font-display`, `--cx-display-sm` (`tokens.css:196,202`) |
| G8 | Square seg control, mono labels, bordered (`astro:385-403`) | `.seg` on `--cx-bg-panel` + active `--cx-bg-active`, `--cx-radius-sm` (`app.css:487-507`) | `.seg`, `.seg button` — align metrics + square off (no DS component governs the seg, OQ-1); keep Solid `onClick` mechanism (radios are the marketing no-JS hack, `astro:225-229`, not ported) | `--cx-border`, `--cx-bg-raised` |
| G9 | 7px square CI/review pips on state colors (`astro:526-550`) | 8px `border-radius: 2px` CI square + circle review dot on `--cx-ok/error/warn` (`app.css:746-773`) | `.ci-badge`/`.review-badge` rules; consumers `IssueCard.tsx:79,83`, `Bridge.tsx:61,64`, `DoneView.tsx:37,41` | `--cx-ci-pass/fail/pending`, `--cx-review-approved/changes/pending` (`tokens.css:146-151`); compact-pip shape per D3 |
| G10 | One advancing card: blue border + chase-light sweep, 1.8s, reduced-motion `display: none` (`astro:586-610,620-631`) | Nothing | New `.cx-card[data-advancing="1"]` rule + keyframe (consumes existing tokens only); no live data source for the flag | `--cx-accent` (blue flow per `surfaces.md:232-234`), `--cx-pulse-period`, `--cx-ease-out` — D2 |
| G11 | PRs view is a board: same lanes, PR-lifecycle columns, PR cards with coord-as-key + ci/review + `resolved/total threads` foot (`astro:120-129,309-359`) | Grouped flat list: `.pr-tab` > `.pr-group` > `.pr-row` (`Bridge.tsx:274-320`, `app.css:783-861`) | `Bridge.tsx` PRs branch; new `prLifecycle`/`prBoardRows` in `board-render.ts`/`board.ts`; `PR_LANES` in `constants.ts`; delete `.pr-row*` selectors | `--cx-issue-*` reused for column tint; card tokens as G5 — D1 |
| G12 | Empty cell renders empty (`astro:490-496` `min-height: 56px`, no placeholder) | Status-mode empty cell renders a `"—"` placeholder (`Bridge.tsx:207` `.term-empty` fallback); swimlane-mode empties are dimmed (`.swim-cell.dim`, `app.css:620-623`, applied `Bridge.tsx:252-255`) | Remove the `Bridge.tsx:207` fallback; `min-height` on `.bridge-cell`; keep `.dim` (a live-data affordance the reference lacks, consistent with the KEPT list — F11) | — |
| G13 | Cell `padding: 10px; gap: 8px; min-height: 56px` (`astro:490-496`); gutter col 168px, cols `minmax(0,1fr)` (`astro:260`) | Cell `padding: 7px; gap: 7px`, no min-height (`app.css:609-618`); gutter 180px, cols `minmax(210px,1fr)` (`Bridge.tsx:131-134`) | `.bridge-cell` metrics; keep live `minmax(210px,1fr)` (the board scrolls; `minmax(0,1fr)` is the reference's fit-to-1040px constraint, not ours) | `--cx-space-2/3` |

Live-only features the reference lacks — all KEPT (real data / real
affordances): the `N agents · M in-flight` toolbar sub (`Bridge.tsx:140-142`),
the Swimlanes/Status grouping toggle (`Bridge.tsx:160-175`), the gutter's
`N items` meta + `→` open affordance (`Bridge.tsx:236-242`), the lane-head
counts (`Bridge.tsx:193`), PR-chip and issue-chip cross-links, selection sync,
the current `StateDot` (untouched here — its glyph adoption is SEA-2118).

## Plan

Slices are dependency-ordered; T2–T6 are each independently reviewable and
re-run the visual harness. Class renames land with the slice that owns the
element (`.swim-*` → the `surfaces.md:211-216` `.bridge-lane` /
`.bridge-col-head` / `.bridge-cell` vocabulary), updating `Bridge.test.tsx`
and `visual-smoke.spec.ts` selectors in the same diff.

### T1 — Harness extension (first; the acceptance vehicle)

Extend the visual-smoke spec with the shots this record's review needs: the
PRs tab (click the `PRs ·` seg button, wait for the PRs surface), a cropped
column-head strip (tint review), and a cropped single-card close-up. Baseline
run lands the "before" set.

Interfaces: consumes `apps/ui/e2e/visual-smoke.spec.ts:11-20` conventions
(`SCREENS = "e2e/__screens__"`, selector-waits, no sleeps); produces three new
`test()` blocks writing `bridge-prs.png`, `bridge-colheads.png`,
`bridge-card.png`. No production code.

### T2 — Grid shell re-clothe (G1–G4, G7, G8, G12, G13)

The board frame, hairline borders, panel-tier gutters, tinted column heads,
display heading, seg metrics (squared off), empty-cell fix, cell metrics.
Renames `.swim-*` selectors/classes to `.bridge-lane`/`.bridge-col-head`/
`.bridge-cell`/`.bridge-corner` in `Bridge.tsx` + `app.css`. **The
`.pr-group-head` head reuses `.swim-gutter` (`Bridge.tsx:291`, styled
`app.css:797-805`) — its class reference updates in THIS slice's rename, so the
grouped PRs list stays coherent until T6 replaces it (F10).** Column tint:
`Bridge.tsx` sets `style={{ "--lane-tint": lane.color }}` on each head (lane
colors are already `--cx-issue-*` var strings, `constants.ts:18-30`); CSS
applies the 8%-mix background (OQ-2). Drops the `lane-dot`, keeps `lane-count`.

Interfaces: consumes `BOARD_LANES: Lane[]` (`constants.ts:17-31`,
`Lane = { state: IssueState; label: string; color: string }`); produces the
renamed markup in `Bridge.tsx:179-271` and replacement `app.css` rules;
updates `visual-smoke.spec.ts` waits if selectors changed (`.bridge` root
kept). Test cycle: vitest `Bridge.test.tsx` green after rename; harness
re-run.

### T3 — Card re-clothe: `.cx-card` adoption (G5, G6)

Wire `design/components/card.css` into the cascade (import in `App.tsx` after
`base.css`, before `app.css`, preserving the load-bearing order the cutover
froze), flip the `IssueCard` container from `class="card"` +
`classList={{ selected }}` (`IssueCard.tsx:46-48`) to `class="cx-card"` +
`data-selected`, drop the priority left-stripe (selection owns the left rule
per `card.css:33-37`), and restyle the `card-issue`/`card-title` sub-part
rules to the quiet-key/bright-title hierarchy. Delete the superseded `.card`
base/hover/selected/priority rules from `app.css` (the grandfathered raw
transitions at `app.css:637-642` die with them — the cx-card transition is
already tokenized, `card.css:17-19`).

**Card internal layout (F3):** the bare `.cx-card` is `display: block` with
`--cx-space-3` (12px) padding (`card.css:5-8`), while the live `.card` is a
`display: flex; flex-direction: column; gap: 6px; text-align: left` button
(`app.css:626-630`) and the reference card is `9px 10px` padding
(`astro:503-508`). So T3 adds a **board-scoped** layout block —
`.bridge-cell > .cx-card { display: flex; flex-direction: column; gap: 6px;
text-align: left; padding: 9px 10px }` — overriding the DS card's block/12px
default for the dense board only. This is a spacing/structure override (the
reference governs spacing), scoped so it never leaks to other `.cx-card`
surfaces.

**Cross-surface note (F6):** `DoneView.tsx:31` renders the shared
`.card-issue`/`.card-pr`/`.card-diff` sub-parts, so the quiet-key restyle
intentionally propagates to the Done view (one convention, not two). The
existing `done.png` harness shot covers it — the T3 diff notes the propagation
so it isn't read as an accidental out-of-scope change in Matt's screenshots.

Interfaces: consumes `IssueCard` props (`{ issue: Issue; onOpenPr?: () =>
void }`, `IssueCard.tsx:18-21`) unchanged; produces the class flip + `app.css`
sub-part restyle + board-scoped layout block + deletions. Test cycle: vitest;
harness card close-up.

### T4 — Badge recolor + form onto `--cx-ci-*` / `--cx-review-*` (G9; D3)

Re-point every `.ci-badge[data-status]` / `.review-badge[data-verdict]` rule
(`app.css:746-773`) from the generic `--cx-ok/error/warn` onto the dedicated
`--cx-ci-pass/fail/pending` and `--cx-review-approved/changes/pending`
namespaces (`tokens.css:146-151`), and map the live `commented` verdict
(`board-render.ts:102-112` returns `"changes" | "approved" | "commented"`)
onto the `--cx-review-pending` color (D3). All three consumer sites
(`Bridge.tsx:61,64`, `IssueCard.tsx:79,83`, `DoneView.tsx:37,41`) share the
rules, so one edit covers them.

**Badge FORM per SEA-2117 (D3).** The badge-clarity design (SEA-2117, pixel-art
1-bit glyphs leading) owns what the badge *looks like*; T4 consumes that frozen
form. If SEA-2117 is frozen when T4 runs, T4 renders the chosen glyph/pip form
(a `BadgeGlyph`-style inline SVG mirrors the state-dot emission contract and the
consumer sites flip from bare `<span>` to the component). If SEA-2117 has NOT
frozen, T4 ships the interim compact square pip recolored onto the tier (7px,
no radius — `astro:526-532`) and the glyph form lands as a fast follow — T4
does not block on SEA-2117.

Interfaces: consumes `ciBadge(pr): "pending" | "success" | "failure" |
undefined` (`board-render.ts:119-121`) and `reviewBadge(pr): "changes" |
"approved" | "commented" | undefined` (`board-render.ts:102-112`) unchanged;
produces the recolor + the SEA-2117 form (or interim pip). Test cycle:
`Bridge.test.tsx:145-148` badge-presence assertions stay green; stylelint;
harness.

### T5 — Advancing-card hook (G10; D2, dormant until data lands)

Add the `.cx-card[data-advancing="1"]` rule: `border-color: var(--cx-accent)`
plus a sweeping `::after` chase-light gradient
(`color-mix(in srgb, var(--cx-accent) 32%, transparent)` — the reference's
32% mix, `astro:598-603`), animated at `var(--cx-pulse-period)`
`var(--cx-ease-out)` infinite (existing tokens only — no coined literal).
**Fidelity/guard notes (F12):** `--cx-pulse-period` (`tokens.css:224`) resolves
to 1.6s via `--rigel-pulse-period` (`tokens.css:39`)
where the reference hard-codes a 1.8s sweep (`astro:606`) — we accept the
token value rather than coin the literal. The period token is **zeroed** under
reduced motion (`tokens.css:241-248`), which would leave a `0s`-infinite
animation running; the explicit `display: none` reduced-motion guard
(mirroring `astro:620-631`) is therefore **load-bearing, not redundant** — a
reviewer must not strip it. The attribute has NO live data source (`Issue`
carries no transition timestamp — `stub-data.ts` has no `updatedAt`/
`advancing`), so it ships dormant: `IssueCard` renders `data-advancing` only
when a future store accessor provides it. Verified in the harness by toggling
the attribute via `page.evaluate` before the shot.

Interfaces: produces the CSS rule + keyframe and an optional
`advancing?: boolean`-style prop seam on `IssueCard` left unwired; consumes
`--cx-accent`, `--cx-pulse-period`, `--cx-ease-out`. Test cycle: stylelint
(proves no raw motion literal); harness advancing shot via attribute toggle.

### T6 — PRs view board-ification (G11; D1, last)

Replace the grouped list with the same swimlane grid over PR-lifecycle
columns, per `surfaces.md:206-210` and the reference (`astro:309-359`). New
pure derivations (red-green: unit tests first per house rule):

```ts
// constants.ts
export type PrLifecycle = "in_progress" | "in_review" | "ready" | "merged";
export const PR_LANES: { state: PrLifecycle; label: string; color: string }[];
// labels: "In progress" | "In review" | "Ready to merge" | "Merged"
// colors: --cx-issue-in_progress / --cx-issue-in_review / --cx-accent (OQ-3) / --cx-issue-done

// board-render.ts — defined in terms of the EXISTING roll-up seams (F2),
// NOT raw pr.reviews, so it inherits the latest-per-author +
// changes_requested>approved>commented precedence:
export function prLifecycle(pr: PullRequest): PrLifecycle;
//   pr.forgeState === "merged"                                  → "merged"
//   reviewBadge(pr) === "approved" && ciBadge(pr) === "success"
//     && every thread resolved (D1a)                            → "ready"
//   reviewBadge(pr) !== undefined || pr has open threads        → "in_review"
//   else (incl. draft-open)                                     → "in_progress"
// Totality (F2): input is narrowed to board rows (forgeState !== "closed"),
// so "closed" is unreachable; a defensive `default → "in_progress"` keeps it
// total if the type ever widens.

// board.ts
export function prBoardRows(all: readonly Issue[]): PrRow[];
// like prRows (board.ts:132-134) but forgeState !== "closed" — merged PRs
// (within the store's 1-day window, D1b) included so the Merged column is
// sourceable. The 1-day retention is SERVER-SIDE / store-lane (D1b); this
// function renders whatever merged rows the store hands it.
export function prBoardGroups(
  agents: readonly Agent[], all: readonly Issue[],
): { agent: Agent | null; rows: PrRow[] }[];
// grouping/order contract identical to prRowGroups (board.ts:139-152),
// INCLUDING the agent:null "Unassigned" group LAST iff it has rows.
```

**Unassigned lane gutter (F8):** `prBoardGroups` keeps `prRowGroups`'s trailing
`agent: null` group (`board.ts:149-150`), so the swimlane grid gets an
Unassigned lane — but a null agent has no `StateDot`/handle/count/`→` open
affordance (the gutter anatomy at `Bridge.tsx:225-242`). T6 designs that
gutter explicitly: no StateDot, a faint "Unassigned" label, no open affordance
— mirroring the current list's distinct `.pr-group-head.unassigned` head
(`Bridge.tsx:284-287`) that this slice deletes.

The PR card reuses the T3 card anatomy with the PR's own facts
(coordinate-as-key via the existing `pr-row-coord` derivation
`Bridge.tsx:55-58`, ci/review pips, title, foot = `@assignee` +
`resolved/total threads` from `pr().threads` as `Bridge.tsx:44` counts them)
and keeps the issue-key `.cx-chip` cross-link flipping to Issues
(`Bridge.tsx:72-88` semantics). **`prCount` tab badge stays open-only**
(`board.ts:158-167`) — consistent with the reference, which excludes the
Merged column from its count (`astro:218-221`). **F7:** `prCount`'s doc
comment (`board.ts:154-155` "equals the visible row count") becomes false once
the tab shows merged rows the count excludes — the comment updates to "equals
the non-Merged visible rows" in this diff, and `Bridge.test.tsx:57-64`'s
framing follows. Delete `PrRowItem` and the `.pr-tab`/`.pr-group`/`.pr-row*`
selector family (`app.css:783-861`) in the same diff.

Interfaces: consumes `PrRow = { issue: Issue; pr: PullRequest }`
(`board.ts:128`), `prBadge`/`ciBadge`/`reviewBadge`/`issueKey`
(`board-render.ts`); produces the three functions above + `PR_LANES` + the
board markup + deletions + unit tests for `prLifecycle`/`prBoardRows`
(precedence: merged > ready > in_review > in_progress; thread-gating per D1a).
Test cycle: new vitest units red→green; `Bridge.test.tsx` PR-tab assertions
updated; harness `bridge-prs.png` — and the stub board set (F2) carries at
least one merged PR whose parent issue is in `store.issues()`, so the Merged
column is non-empty in that shot and the D1 board-ification is visually
verified rather than vacuously green (G11).

### Out of scope

- **Roving-tabindex 2-D grid** (`surfaces.md:236-239,264-265` flip item 6) —
  a keyboard/interaction change, not clothing; filed as a follow-up issue at
  dispatch time (D4).
- **State-dot 1-bit glyph adoption** — its own lane, SEA-2118 (D5); the board
  ships on the current `StateDot`.
- **Review/CI badge form redesign** — its own design lane, SEA-2117 (D3); T4
  recolors onto the tier and consumes the badge form SEA-2117 freezes.
- **Motion primitive spec authorship** — D9/foundation-T8 owns adding a
  card-advance topology to `motion.md`; T5 here only consumes tokens (D2).
- **Merged-column retention windowing** — server-side / store lane (D1b); the
  UI renders the merged rows it is handed.
- **The board-empty centered message** (`surfaces.md:242-243`) — the stub
  store always has issues; deferred with the roving-tabindex follow-up.

## Ledger

Candidate `DECISIONS.md` rows for the driver at PR time (this record does not
edit the ledger): (1) PRs view board-ification supersedes the grouped-list PRs
tab, with thread-gating and a server-side 1-day Merged window; (2) board-card
priority left-stripe retired in favor of the cx-card selection left rule; (3)
advancing-card affordance ships as a dormant hook pending D9 motion-spec
coverage and a real data source.

## Tasks

- [ ] T1 — extend `visual-smoke.spec.ts`: `bridge-prs.png`,
      `bridge-colheads.png`, `bridge-card.png`; capture baseline set
- [ ] T2 — grid shell: hairline borders, panel gutters, `--lane-tint` column
      tint, display heading, seg metrics (squared), `.swim-*`→`.bridge-*`
      rename (incl. the `.pr-group-head` head reference, F10), empty-cell fix;
      tests + harness green
- [ ] T3 — wire `card.css`; `IssueCard` → `.cx-card[data-selected]`; drop
      priority stripe; quiet-key/bright-title; board-scoped card layout block
      (F3); note DoneView propagation (F6); delete legacy `.card` rules
- [ ] T4 — pips → 7px squares on `--cx-ci-*`/`--cx-review-*`; `commented` →
      review-pending color; stylelint green
- [ ] T5 — `data-advancing` chase-light rule on existing motion tokens +
      load-bearing reduced-motion guard (F12); harness shot via attribute
      toggle *(D2)*
- [ ] T6 — PRs board: `prLifecycle`/`prBoardRows`/`prBoardGroups`/`PR_LANES`
      (unit tests first; thread-gating D1a; totality F2), unassigned-lane
      gutter (F8), PR-card markup, `prCount` comment update (F7), delete
      `PrRowItem` + `.pr-*` CSS *(D1)*
- [ ] Driver: ledger delta / `Ledger-impact:` line in the PR body; attach
      before/after PNGs; file the roving-tabindex + empty-board follow-up
      issue at dispatch

## Open Questions

Remaining forks are non-load-bearing detail (the load-bearing ones closed as
D1–D5 above); each carries a stated assumption the design proceeds on.

**OQ-1 (non-load-bearing) — primitive-shape fidelity where DS ≠ reference.**
Reference cards/pips/seg are hard-square; the DS `.cx-card` carries
`--cx-radius-md` (`card.css:11`) and the live pips/seg carry small radii.
*Assumption:* DS components govern shape; the reference governs structure,
spacing, and hierarchy — **except where no DS component governs the element**
(the CI/review pips and the seg control), where the reference's square shape
wins. Reviewable in the screenshot pass.

**OQ-2 (non-load-bearing) — column-head tint alpha.** `surfaces.md:213-215`
specs "low alpha" without a number, and the reference CSS carries no tint at
all (`astro:451-458`). *Assumption:* `color-mix(in srgb, var(--lane-tint) 8%,
var(--cx-bg-raised))`. Reviewed in `bridge-colheads.png`; a one-line knob.

**OQ-3 (non-load-bearing) — "Ready to merge" column color.** `--cx-issue-*`
has five issue-axis values and no "ready" (`tokens.css:139-143`).
*Assumption:* `--cx-accent` (the blue flow color — ready-to-merge is work in
motion, not a lifecycle state). No token minted.
