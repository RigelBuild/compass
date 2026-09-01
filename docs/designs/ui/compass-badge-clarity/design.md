# Compass badge clarity (RIG-2117)

Status: Active
Owner lane: compass-ux (design) → compass-ui (execution)
Refs: RIG-2117 (CI/review badges convey meaning by color alone, and the token tier maps
CI-pass ≡ review-approved and CI-fail ≡ review-changes to the same hues). Sibling to the
RIG-2111 Bridge re-clothe (its T4 consumes whatever badge form this record freezes) and
sequenced after the RIG-2034 DS-token cutover (merged, main `18e988b5`). State-dot glyph
adoption is a separate lane (RIG-2118); this record is badges only.

## Problem / Intent

The CI and review badges on Compass board surfaces are tiny colored squares/dots whose
meaning is carried by **color alone** — a user cannot tell what a badge means, which axis
it belongs to (CI vs review), or its status without prior knowledge. Worse, the token tier
maps CI-pass and review-approved to the **same green** and CI-fail and review-changes to
the **same red**, so color *cannot* disambiguate even for a trained eye. This record
enumerated several buildable directions — each with a precise render spec — and ran a
side-by-side render (T1); **Matt selected Option B** from it (see `## Decision`). The
implementation below is scoped to that choice.

## Global Constraints

- **SolidJS + pure CSS custom props.** Consume the `--cx-*` tier ONLY; the D7 stylelint
  guard bans raw hex, `--rigel-*` refs, and raw motion literals at `error`.
- **`tokens.css` is read-only.** Every option must map its statuses onto the existing
  `--cx-ci-*` / `--cx-review-*` tokens (plus, where noted, an existing neutral like
  `--cx-text-faint`); no new tokens.
- **Legibility at ~7–9px with no hover is the acid test.** Badges appear on dense cards
  (`IssueCard`), PR rows (`Bridge`), and Done rows (`DoneView`); any option must state how
  it reads at that size.
- **Two axes on one card.** A PR shows BOTH a CI badge AND a review badge, adjacent.
  Because of the color collisions below, under this record's commitment to keep the
  semantic `--cx-*` routing intact (T2) the CI-vs-review distinction must ride a second,
  non-color channel (glyph family / shape / label / position); Option E is the carried
  alternative that spends color instead (so "never color" is this record's chosen
  posture, not a physical law — see the collisions section and Option E).
- **Aesthetic:** match the state-dot pixel-art 1-bit vocabulary — whole-cell 9×9 grids,
  `shape-rendering="crispEdges"`, one CSS px per cell (`components.md:93-95`, quoted
  below). Matt has stated he likes this form and wants the badges to lead with it.
- **No VCS actions in this record's lane; the driver owns commit/push/render.**

## Current state (grounding)

All references are to the clone at main `18e988b5`.

### The two seams and their vocabularies

`apps/ui/src/board-render.ts:102-112` — review roll-up, three display values:

```ts
export function reviewBadge(
  pr: PullRequest,
): "changes" | "approved" | "commented" | undefined {
  const latest = new Map<string, Review["verdict"]>();
  for (const r of pr.reviews) latest.set(r.author, r.verdict);
  const verdicts = [...latest.values()];
  if (verdicts.length === 0) return undefined;
  if (verdicts.includes("changes_requested")) return "changes";
  if (verdicts.includes("approved")) return "approved";
  return "commented";
}
```

`apps/ui/src/board-render.ts:119-121` — CI roll-up, three values via `ChecksSummary`:

```ts
export function ciBadge(pr: PullRequest): ChecksSummary["state"] | undefined {
  return pr.checks?.state;
}
```

with the doc comment (`board-render.ts:114-116`): *"it is already the 3-valued roll-up
(`"pending" | "success" | "failure"`), so no new mapping is invented."*

So the full status matrix is **2 axes × 3 statuses** (plus absent → no badge). None of
the options below change these seams; they change only what the consumer renders per value.

**Display-value vocabulary (used by every option).** The seams emit exactly these
values: CI ∈ `success | pending | failure`, review ∈ `approved | changes | commented`
(there is no review `pending` *display* value — `--cx-review-pending` is a token / CSS
variant name, not a seam output). Where an option references a `review-pending` variant
(Options C and D), it is the visual/token slot the display value `commented` is routed to
(per Q3); no option invents a status the seam cannot emit.

### Current render — color-only pips

`apps/ui/src/app.css:746-773`:

```css
.ci-badge,
.review-badge {
  width: 8px;
  height: 8px;
  border-radius: 2px;
  background: var(--cx-text-faint);
}
.ci-badge[data-status="success"] { background: var(--cx-ok); }
.ci-badge[data-status="failure"] { background: var(--cx-error); }
.ci-badge[data-status="pending"] { background: var(--cx-warn); }
.review-badge { border-radius: 50%; }
.review-badge[data-verdict="approved"]  { background: var(--cx-ok); }
.review-badge[data-verdict="changes"]   { background: var(--cx-error); }
.review-badge[data-verdict="commented"] { background: var(--cx-warn); }
```

Two observations beyond "color-only":

1. **Shape already half-encodes the axis** (CI = 8px rounded square, review = circle),
   but at 8px a `border-radius: 2px` square and a circle are nearly indistinguishable,
   and shape says nothing about *status*.
2. **The tier is bypassed.** These rules consume `--cx-ok`/`--cx-error`/`--cx-warn`
   directly instead of the semantic `--cx-ci-*` / `--cx-review-*` tokens, and `commented`
   renders amber even though the token tier has no `--cx-review-commented` (only
   `--cx-review-pending`, which is faint, not amber). Any option shipped should also
   re-route through the semantic tier (see Open Questions Q3).

### The color collisions (load-bearing)

`apps/ui/src/design/tokens.css:146-151`:

```css
--cx-ci-pass: var(--cx-ok);
--cx-ci-fail: var(--cx-error);
--cx-ci-pending: var(--cx-warn);
--cx-review-approved: var(--cx-ok);
--cx-review-changes: var(--cx-error);
--cx-review-pending: var(--cx-text-faint);
```

`--cx-ci-pass` ≡ `--cx-review-approved` (same green) and `--cx-ci-fail` ≡
`--cx-review-changes` (same red). Color therefore cannot distinguish CI-pass from
review-approved, nor CI-fail from review-changes **as the tokens are routed today**.
The collision lives in the token *aliases*, not in physics: a consumer may legally
re-point the review axis at a different existing `--cx-*` color and break it without
touching read-only `tokens.css` — the shipped company-site board already does exactly
that (Option E, and the reference note below). This record instead commits to keeping
the semantic `--cx-*` routing intact (T2) and carrying the CI-vs-review distinction on
a **second, non-color channel** (glyph family / shape / label / position). Under that
commitment a second channel is required; Option E is the alternative that spends color
instead, and is in the render so the tradeoff is visible rather than assumed.

### The three consumer sites (both badges adjacent, CI always first)

`apps/ui/src/components/IssueCard.tsx:78-85` (inside the card's PR chip):

```tsx
<Show when={p().checks}>
  <span class="ci-badge" data-status={ciBadge(p())} />
</Show>
<Show when={reviewBadge(p())}>
  {(verdict) => (
    <span class="review-badge" data-verdict={verdict()} />
  )}
</Show>
```

`apps/ui/src/components/Bridge.tsx:60-65` (PR row):

```tsx
<Show when={pr().checks}>
  <span class="ci-badge" data-status={ciBadge(pr())} />
</Show>
<Show when={reviewBadge(pr())}>
  {(verdict) => <span class="review-badge" data-verdict={verdict()} />}
</Show>
```

`apps/ui/src/components/DoneView.tsx:36-43` (Done row):

```tsx
<Show when={p().checks}>
  <span class="ci-badge" data-status={ciBadge(p())} />
</Show>
<Show when={reviewBadge(p())}>
  {(verdict) => (
    <span class="review-badge" data-verdict={verdict()} />
  )}
</Show>
```

Three identical shapes: bare `<span>`s, no text, no `aria-label`, **CI first, review
second, always in that order**. That fixed ordering is a free positional channel every
option below leans on. The empty spans also mean the current badges are invisible to
assistive tech — every option should add an `aria-label`/`title` (cross-option fix).

### The aesthetic to match — the state-dot 1-bit vocabulary

`apps/ui/src/design/components.md:93-95`:

> **Geometry:** a 9×9 1-bit bitmap grid, `shape-rendering="crispEdges"` (no
> anti-aliasing). Glyph geometry is fixed by the grids below; `compass-ui`
> emits identical inline SVG.

and `components.md:106-108`:

> `#` = lit cell, `.` = off. The dot box is 9px (one CSS px per grid cell —
> razor-crisp per the brand 1-bit whole-cell rule)

The shipped implementation (`apps/ui/src/design/components/state-dot.css:13-16`) is a
9px inline box with per-state color via `currentColor`. The existing `done` check glyph
(`components.md:159-171`) is reused verbatim as CI-pass in Option A.

### The DS's literal badge (Option D's base)

`apps/ui/src/design/components/badge.css:11-25` — `.cx-badge` is a bordered, padded,
labelled token:

```css
.cx-badge {
  display: inline-flex;
  align-items: center;
  gap: var(--cx-space-1);
  padding: 0 var(--cx-space-2);
  height: var(--cx-space-5);
  border: 1px solid var(--cx-border);
  ...
}
```

with per-status color+border variants `ci-pass|ci-fail|ci-pending` /
`review-approved|review-changes|review-pending` (`badge.css:28-57`) already consuming the
semantic tier correctly.

### The reference — a shape floor, but ahead on color routing

The internal monorepo's company-site board is prior art here — its Bridge board
renders the same bare squares, 7px, no glyphs, so on **shape** it is a floor
this record improves on.

But on **color** that board is already ahead of Compass: it breaks the CI/review
collision at the consumer by coloring the review axis off a
different palette — approved in cyan, commented in mute — so green-CI-pass and
cyan-review-approved never share a hue.

So the reference is a floor on *shape* and prior art on *color routing*. This record can
back-port its chosen glyph form to the site (Q5); Option E is the inverse — adopt the
site's recolor as Compass's cheapest de-collision baseline.

## Options

All options keep `ciBadge`/`reviewBadge` untouched and change only markup + CSS at the
three consumer sites. All options add `aria-label` (e.g. `aria-label="CI: failing"`,
`aria-label="Review: changes requested"`) and a matching `title` tooltip — that a11y fix
ships regardless of the visual form.

### Option A — Pixel-art 1-bit glyph badges (LEAD)

**What the user sees:** each badge becomes a 9×9 crisp-edges 1-bit glyph in the state-dot
style, colored by its status token. The two axes use **disjoint glyph families**: CI
glyphs are *machine verdict marks* (check / ellipsis / cross), review glyphs are *human
conversation marks* (bold check / delta / speech bubble). No two glyphs across the whole
6-cell matrix share a form, so the thin-check (CI pass) vs bold-check (review approved)
and red-X vs red-delta pairs stay distinct even under the token color collisions — and
under total color-blindness the six statuses remain six distinct shapes.

| axis | status | glyph | token |
| --- | --- | --- | --- |
| CI | success | check tick (the state-dot `done` glyph, reused verbatim) | `--cx-ci-pass` |
| CI | pending | three-dot ellipsis `…` ("running…") | `--cx-ci-pending` |
| CI | failure | full X cross | `--cx-ci-fail` |
| review | approved | bold/thick check (heavier than the CI check) | `--cx-review-approved` |
| review | changes | delta/triangle `Δ` (the change mark) | `--cx-review-changes` |
| review | commented | speech bubble | `--cx-review-pending` (see Q3) |

**Why it reads at 7–9px:** identical discipline to the shipped state dots, which already
read at 9px in a ~12px row (`components.md:106-109`). Whole-cell strokes, no
anti-aliasing, one CSS px per cell; each glyph is chosen for silhouette at that size the
same way `working` is a double-chevron rather than a ring.

**CI-vs-review distinction:** three channels — glyph family (marks vs conversation),
fixed position (CI first at all three sites), and residual container shape is dropped
(the glyph IS the badge).

**A11y/CVD:** best of all options — six distinct silhouettes; status never rides color
alone; `aria-label` on each. Scope note: the "six distinct forms" claim is *within the
badge matrix*. By design CI-success reuses the state-dot `done` check verbatim (a
different color — green `--cx-ci-pass` vs cyan `--cx-st-done`), so under total color loss
a check on a PR chip (CI pass) and a check on an agent row (done) share a silhouette;
card-vs-row context carries it, and one board-wide "success" mark is a feature, but the
uniqueness guarantee does not extend across component families.

**Cost:** medium. One new `BadgeGlyph` Solid component emitting inline SVG (built from the
render spec below — see the note on the emission precedent), 6 glyph grids added to
`components.md`, a small CSS file, 3 consumer flips. No token changes.

**Render spec (exact):**

- Markup per badge: `<svg class="cx-ci-glyph" data-status="…" viewBox="0 0 9 9"
  width="9" height="9" shape-rendering="crispEdges" role="img" aria-label="CI: …">` with
  one `<rect x y width="1" height="1" fill="currentColor">` per lit cell. This is the same
  inline-`crispEdges`-SVG *markup contract* the design system documents for the state-dot
  glyph (`components.md:93-95`) and the loader spinner (`loader.css:24-28`,
  `motion.md:81-86`) — note that both are documented specs, not shipped emitters
  (`StateDot.tsx` renders a color-only `<span class="state-dot" role="img">`; the state-dot
  glyph SVG is the RIG-2118 lane), so `BadgeGlyph` is built from the grids below as spec,
  not copied from a rendered artifact. Review twin: `.cx-review-glyph[data-verdict]`.
- Box: 9×9 CSS px, `display: inline-block`, no background, no border.
- Color: `color: var(--cx-ci-pass|--cx-ci-pending|--cx-ci-fail)` /
  `var(--cx-review-approved|--cx-review-pending|--cx-review-changes)` per variant.
- Gap between the two badges: keep the consumer's existing flex gap (5px on `.card-pr`).

**The six glyph grids** (`#` = lit, `.` = off; 9×9, one CSS px per cell):

`ci-success` — check tick (reuses the state-dot `done` grid, `components.md:161-171`):

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

`review-approved` — a **thick/bold check** (heavier than the single CI check; the "extra-affirmed" tick):

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

`review-changes` — delta (hollow triangle, the mathematical change mark):

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

### Option B — Glyph + micro-label

**What the user sees:** the Option A glyph, prefixed by a fixed 2-character axis code in
the DS mono UI face — `CI` before the CI glyph, `RV` before the review glyph — both code
and glyph in the status color. E.g. a failing-CI, changes-requested PR reads
`CI ✕  RV Δ`. On the cramped board gutter it degrades to Option A (glyph only) via a
container query or a `data-compact` prop.

**Why it reads at 7–9px:** the glyph carries the status exactly as in A; the label rides
above the acid-test size (text at `--cx-text-xs`, ~10px) and is a *bonus* channel on
roomier rows (Bridge PR rows, Done rows), not the load-bearing one. Where it can't fit,
it's absent by design.

**CI-vs-review distinction:** strongest of all options — the axis is literally spelled.
Glyph family + position still back it up in compact mode.

**A11y/CVD:** same silhouettes as A, plus real text for sighted users who don't know the
iconography yet. Best learnability.

**Cost:** medium-high — everything in A plus the label span, the compact-mode switch, and
per-surface layout checks (Bridge rows already carry `pr-row-state` text at
`Bridge.tsx:52-54`, so horizontal budget is tight there).

**Render spec (exact):**

- Markup: `<span class="cx-axis-badge" data-axis="ci|review" data-status="…">` containing
  `<span class="cx-axis-code">CI</span>` (or `RV`) + the Option A 9×9 glyph SVG. The
  wrapper's `data-status` is the single source of truth for the whole badge — CI values
  `success|pending|failure`, review values `approved|changes|commented`; the inner glyph
  drops its own `data-verdict`/`data-status` and selects its shape off the wrapper via a
  descendant selector (`.cx-axis-badge[data-axis="review"][data-status="approved"] .glyph`,
  etc.), so there is no second attribute to keep in sync.
- Code text: `font-family: var(--cx-font-ui)`, `font-size: var(--cx-text-xs)`,
  `line-height: 1`, `letter-spacing: 0.5px`, `color: currentColor`. Gap code↔glyph:
  `var(--cx-space-1)`.
- Color routing — the wrapper sets `color` (both the code text and, via `currentColor`,
  the glyph `fill`) through the six explicit selectors:
  `[data-axis="ci"][data-status="success"]{color:var(--cx-ci-pass)}`,
  `…="pending"{--cx-ci-pending}`, `…="failure"{--cx-ci-fail}`;
  `[data-axis="review"][data-status="approved"]{--cx-review-approved}`,
  `…="changes"{--cx-review-changes}`, `…="commented"{--cx-review-pending}` (Q3 mute).
- Compact rule: `.cx-axis-badge[data-compact] .cx-axis-code { display: none; }` —
  IssueCard passes `data-compact`; Bridge and Done rows show the code.
- Total footprint (full): ~26×10px per badge; (compact): 9×9px.

### Option C — Shape-coded pips (no pictogram)

**What the user sees:** stays pip-scale, pure CSS, no SVG — but status is encoded in
**fill state**, and axis in **outer shape**, so color is never alone. CI keeps the square,
review keeps the circle (sharpened from the current near-indistinguishable pair by making
the square hard-cornered and 1px larger):

| axis | status | shape |
| --- | --- | --- |
| CI | success | 9×9 **filled** square, `border-radius: 0` |
| CI | pending | 9×9 **hollow** square (1px border, transparent center) |
| CI | failure | 9×9 hollow square with a 1px **diagonal slash** through it |
| review | approved | 9×9 **filled** circle |
| review | commented | 9×9 **hollow** circle (the `review-pending` visual variant; see the vocabulary note) |
| review | changes | 9×9 hollow circle with a 1px diagonal slash |

**Why it reads at 7–9px:** filled-vs-hollow is the single most robust sub-10px
distinction available (it survives blur, CVD, and low-DPI); the slash adds one more step.
Square-vs-circle at 9px with hard corners is legible where the current 2px-radius square
was not.

**CI-vs-review distinction:** outer shape + position. Weaker than A/B — square vs circle
at 9px demands a trained eye, and "hollow circle" for both pending-ish review states loses
the commented/pending nuance.

**A11y/CVD:** good on status (fill state), weak on axis. `aria-label` mandatory.

**Cost:** lowest — CSS-only edits to the existing `.ci-badge`/`.review-badge` rules plus
a `::after` for the slash; zero markup change beyond `aria-label`.

**Render spec (exact):**

- Base: `width: 9px; height: 9px; box-sizing: border-box;` CI `border-radius: 0`,
  review `border-radius: 50%`.
- Filled: `background: var(--cx-ci-pass)` / `var(--cx-review-approved)`.
- Hollow: `background: transparent; border: 1px solid var(--cx-ci-pending)` /
  `var(--cx-review-pending)`.
- Slashed: hollow in `--cx-ci-fail`/`--cx-review-changes` + `position: relative;
  overflow: hidden;` and `::after { content: ""; position: absolute; inset: -1px;
  background: linear-gradient(to top right, transparent calc(50% - 0.5px), currentColor
  calc(50% - 0.5px), currentColor calc(50% + 0.5px), transparent calc(50% + 0.5px)); }`
  with `color:` set to the status token. (Gradient of transparencies + `currentColor`,
  no raw hex — D7-clean.)

### Option D — Labelled `.cx-badge` box (the heavy alternative)

**What the user sees:** the DS's literal badge (`badge.css:11-25`): a bordered, padded,
`--cx-bg-panel` box with mono text — `CI ✓` / `CI …` / `CI ✕` and `RV ✓` / `RV Δ` /
`RV ◦` — color+border per the existing `.cx-badge[data-status]` variants
(`badge.css:28-57`), which already consume the semantic tier. The status mark inside is
the Option A 9×9 glyph (keeping the pixel-art lead even in the heavy form).

**Why it reads at 7–9px:** it doesn't try to — it *rejects* the pip size and buys clarity
with area. The badge is `--cx-space-5` tall (taller than the 12px row budget), so this
option implies a card-layout change: badges move to their own line or replace the
`#123` PR-number text.

**CI-vs-review distinction:** explicit text. Unambiguous.

**A11y/CVD:** text + glyph + border color; best raw accessibility.

**Cost:** highest — reflows all three consumer layouts (IssueCard's `card-top` line,
Bridge's already-full PR row, DoneView's `done-row-top`); the density loss on the board
gutter is real and likely disqualifying there, which is why this option exists mainly so
the render shows what the heavy end of the spectrum costs.

**Render spec (exact):**

- Markup: `<span class="cx-badge" data-status="ci-pass|ci-fail|ci-pending|
  review-approved|review-changes|review-pending">` containing
  `<span class="cx-axis-code">CI|RV</span>` + the Option A 9×9 glyph SVG.
- All box styling is the shipped `badge.css:11-25` verbatim (padding
  `0 var(--cx-space-2)`, height `var(--cx-space-5)`, `1px solid` status border,
  `--cx-radius-sm`, `--cx-bg-panel`); no new CSS beyond the code span.
- Footprint: ~34×20px per badge — render it beside A–C so the size cost is visible.

### Option E — Consumer-level review recolor (the cheap de-collision baseline)

**What the user sees:** the pips stay exactly as today (no glyphs, no SVG, pip-scale),
but the review axis is recolored at the consumer so it no longer shares a hue with CI —
matching the shipped company-site board: review-approved
becomes cyan, review-commented becomes mute, review-changes stays red. CI keeps
green/amber/red. Green-CI-pass and cyan-review-approved are now different colors, so the
one cross-axis collision this record opens on is gone.

**Why it reads at 7–9px:** it changes nothing about size or shape — it is the current
render with two color assignments swapped, so it inherits today's (poor) legibility
exactly. It does *not* touch the within-axis "meaning by color alone" problem: a user
still cannot tell what green-CI means without knowing the code, and CI pass/fail/pending
remain color-only.

**CI-vs-review distinction:** color (cyan review vs green/amber/red CI) plus the existing
square-vs-circle shape and fixed position. This is the ONE option that spends color as
the disambiguator instead of a second channel.

**A11y/CVD:** weakest — still color-only per status, so it fails the CVD bar (a
red/green-blind user still cannot separate CI pass from CI fail; cyan-vs-green is also a
common confusion). `aria-label` mandatory, and here it is the *only* non-color channel.

**Cost:** lowest of all — CSS-only, ~3 changed color declarations at `app.css:746-773`,
zero markup change beyond `aria-label`. No new tokens, but it **abandons the semantic
`--cx-review-*` tier** that T2 restores: to get cyan it must borrow a non-review token
(the only cyan `--cx-*` tokens are `--cx-st-done` / `--cx-issue-done`,
`tokens.css:130,143` — a review badge wearing the "done" cyan), the tier-purity cost that
makes this a baseline to beat, not the recommendation.

**Render spec (exact):**

- `.ci-badge` unchanged (green/amber/red via `--cx-ci-*`).
- `.review-badge[data-verdict="approved"]  { background: var(--cx-st-done); }` (cyan; the
  borrowed token — no semantic `--cx-review-*` cyan exists).
- `.review-badge[data-verdict="changes"]   { background: var(--cx-review-changes); }` (red).
- `.review-badge[data-verdict="commented"] { background: var(--cx-review-pending); }` (mute).
- Keep the current 8px box, `border-radius: 2px` (CI) / `50%` (review); optionally adopt
  Option C's hard-cornered 9px square to sharpen the axis shape.

## Decision (Matt, from the T1 render)

**Option B — glyph + micro-label — is the chosen form.** Matt picked B from the
side-by-side render (`~/notes/sea2117/badge-options.png`). B is Option A's pixel-art 1-bit
glyph plus a fixed 2-character axis code (`CI` / `RV`) in the DS mono UI face, both code
and glyph in the status color; it degrades to glyph-only (Option A) on cramped surfaces
via `data-compact`. This gives the strongest CI-vs-review distinction (the axis is
spelled) with A's silhouette-under-color-loss guarantee retained, and A as the built-in
compact fallback.

Rationale for the record: B keeps every strength of A (six distinct 1-bit silhouettes,
no status riding color alone, the pixel-art vocabulary Matt likes) and adds a spelled-out
axis on the roomier rows where horizontal budget allows, degrading to A where it does not.
Options C, D, E were rejected: C's square-vs-circle axis channel is too weak at 9px and it
loses the commented nuance; D reflows all three card rows and eats board density; E stays
color-only (fails the CVD bar) and borrows the "done" cyan token (tier-impure). A is not
separately built — it IS B's compact mode.

### Open questions — resolved (Matt accepted the assumed answers)

- **Q1 → distinct glyphs.** CI-fail (X) and review-changes (delta) stay distinct in the
  built form.
- **Q2 → own glyph.** review `commented` keeps its speech-bubble glyph (not folded into a
  neutral dot).
- **Q3 → route to mute.** `commented` moves from today's amber to `--cx-review-pending`
  (faint mute); the speech-bubble glyph (Q2) carries it as the non-color channel, so the
  Q2×Q3 interaction is satisfied (both do not collapse to the neutral pole).
- **Q4 → 9px everywhere.** Glyphs ship at 9px, matching the state-dot size; if the
  `components.md:111-114` size question later moves the dots, it binds these glyphs too.
- **Q5 (back-port to `rigel.build`) stays a non-load-bearing follow-up**, out of scope for
  this lane; T4 notes it.

## Cross-axis behavior (CI vs review on one card)

All three consumer sites render CI then review, adjacent, in that fixed order
(`IssueCard.tsx:78-85`, `Bridge.tsx:60-65`, `DoneView.tsx:36-43`) — and either may be
absent (`ciBadge` is `undefined` with no `checks`, `reviewBadge` with no reviews). The
design consequences every option must satisfy:

1. **Order is a channel but not a sufficient one** — with one badge absent, position
   alone cannot say which axis remains. A/B/D solve this with disjoint glyph
   families/text; C leans on square-vs-circle, its weakest point; E leans on color
   (cyan review vs green CI) + shape, and so carries the axis only for a sighted,
   non-CVD user.
2. **The collision pairs must survive adjacency**: thin-check (CI pass) next to
   bold-check (review approved), and red-X (CI fail) next to red-delta (review
   changes), are the two acid renders the comparison MUST include.
3. **Both-absent** stays as today: no badge, no placeholder.

## Open Questions

**Resolved** — Matt accepted every assumed answer from the T1 render (see Decision
above). The rationale each proceeded on is kept below for the executor.

- **Q1 (load-bearing): do CI-fail and review-changes need distinct glyphs, or is
  CI-vs-review context enough?** Assumption taken: **distinct glyphs** (X vs Δ). Both are
  the same red and either badge can appear alone, so context is not reliable; the delta
  also carries the "changes requested" semantics on its own. Recommendation: keep them
  distinct in every option.
- **Q2 (non-load-bearing): does review `commented` deserve its own glyph, or fold into a
  neutral dot?** Assumption taken: **own glyph (speech bubble)** in A/B/D — it is the one
  status whose meaning ("someone said something, no verdict") a bare dot actively
  obscures. C folds it into the hollow circle by construction. Cheap to swap either way
  after the render.
- **Q3 (load-bearing-ish, token routing): `commented` currently renders amber via raw
  `--cx-warn` (`app.css:771-773`), but the semantic tier only offers
  `--cx-review-pending` (faint mute, `tokens.css:151`) and `tokens.css` is read-only.**
  Assumption taken: map `commented` → `--cx-review-pending` (mute), accepting the visible
  change from amber; a comment-without-verdict is closer to "no verdict yet" than to a
  warning. If Matt wants amber kept, the consumer CSS can use `--cx-warn` directly
  (legal — it's a `--cx-*` token) at the cost of tier purity.
  Prior art supports this assumption: the company-site board already ships `commented` as
  mute. Interaction with Q2: `commented` must keep a
  distinct **non-color** channel — do not resolve BOTH Q2 (fold commented into a neutral
  dot) and Q3 (route commented to faint mute) to the neutral pole, or a comment-without-
  verdict becomes a near-invisible grey dot. Keep the speech-bubble glyph (Q2) if Q3
  demotes the color.
- **Q4 (non-load-bearing): render size — 9px glyphs, or an integer-multiple 18px on
  roomier rows?** Assumption taken: 9px everywhere, matching the state-dot's shipped
  size and the open question already tracked at `components.md:111-114`. Whatever answer
  Matt gives there should bind these glyphs too.
- **Q5 (non-load-bearing): should the chosen option back-port to
  the internal monorepo's company-site board** (which has the same bare
  squares)? Assumption: yes eventually, out of scope for RIG-2117.

## Plan / Tasks

The record's contract is the option set; the T1 render ran and **Matt picked Option B**
(see Decision). Implementation below is now scoped to B.

### T1 — Comparison render

Build a static comparison page/harness showing all options side by side, each option
rendered at all 6 statuses plus the two acid adjacency pairs (Q-pair renders from
Cross-axis behavior §2), on all three surface mockups (card, PR row, Done row) at real
size and 4× zoom. Screenshot for Matt.

- Interfaces: consumes the render specs above verbatim (glyph grids, px, tokens).
  Produces the screenshot(s) Matt picks from.

### T2 — Implement Option B (glyph + micro-label)

- New `BadgeGlyph` Solid component emitting the inline 9×9 `crispEdges` SVG built from the
  Option A render spec (the same inline-`crispEdges`-SVG *markup contract* the DS documents
  for the state-dot glyph and the loader — both specs, not shipped emitters; see Option A),
  wrapped by the `.cx-axis-badge` structure: a
  `.cx-axis-code` span (`CI` / `RV`) + the glyph, container carrying the status token.
  New CSS in `apps/ui/src/design/components/` (the `.cx-axis-badge` / `.cx-axis-code` /
  glyph rules from Option B's render spec), glyph grids appended to `components.md` §Badge.
- Compact mode: `.cx-axis-badge[data-compact] .cx-axis-code { display: none; }` — the
  glyph-only (Option A) fallback for cramped surfaces.
- Route all colors through `--cx-ci-*` / `--cx-review-*` (fixing the raw
  `--cx-ok`/`--cx-error`/`--cx-warn` bypass); map `commented` → `--cx-review-pending` per
  Q3.
- Interfaces: consumes `ciBadge(pr): "pending"|"success"|"failure"|undefined` and
  `reviewBadge(pr): "changes"|"approved"|"commented"|undefined` (unchanged,
  `board-render.ts:102-121`). Produces `.cx-axis-badge[data-axis][data-status]` with the
  `.cx-axis-code` + glyph children per Option B's render spec.

### T3 — Flip the three consumers + a11y

Replace the bare spans at `IssueCard.tsx:79,83`, `Bridge.tsx:61,64`, `DoneView.tsx:37,41`
with the new `BadgeGlyph`/`.cx-axis-badge` component; `IssueCard` passes `data-compact`
(cramped card gutter → glyph-only), Bridge and Done rows show the `CI`/`RV` code. Add
`aria-label` + `title` (`CI: passing/pending/failing`, `Review: approved/changes
requested/commented`) — the a11y fix ships regardless.

### T4 — Docs

Update `components.md` §Badge/Pip/Chip with the shipped variant + grids; note the
`rigel.build` back-port as follow-up (Q5).

### Tasks

- [x] T1: comparison render of all options (6 statuses + 2 acid pairs × 3 surfaces)
- [x] Gate: Matt picked **Option B**; Q1–Q4 resolved (distinct glyphs · own commented glyph · commented→mute · 9px)
- [ ] T2: implement Option B (BadgeGlyph + axis code + compact mode)
- [ ] T3: flip IssueCard/Bridge/DoneView; add aria-label/title
- [ ] T4: components.md update + back-port note
