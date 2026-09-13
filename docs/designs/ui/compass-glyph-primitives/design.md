# Compass glyph primitives (RIG-3603)

Status: Active
Ledger: DL-367
Owner lane: compass-ux (design) → compass-ui (execution)
Refs: RIG-3603. Adopts the technique frozen by DL-150 (state dot: 9×9
`crispEdges` 1-bit SVG) and DL-199 (badge: pixel-art 1-bit glyph + 2-char mono
axis code). All code references are to main `97741c5ce2f4`.

## Problem / Intent

UI chrome renders ~23 non-ASCII glyphs (`▦`, `🗀`, `⎇`, `⇄`, `▸`, `◆`, `★`, …)
that neither brand face (Departure Mono, Space Mono) covers, so the e2e font
stack pins a ~14 MB Unifont fallback (`tools/toolchain/chromium-e2e-env.nix`)
and end users get whatever their OS substitutes. Separately,
`ActivityBarItem.icon: string` conflates two different primitives — a fixed
chrome symbol and a person's initial — behind one string field. This record
replaces chrome glyphs with dot-matrix `crispEdges` SVG per DL-150/DL-199,
splits the item type at the item per Matt's frozen ruling, and states the
conditions under which the Unifont pin can be retired.

## Global Constraints

- **Solid v2 (`solid-js@2.0.0-rc.1`), TS strict, Biome (tabs).** Props are
  never destructured in components.
- **Technique is frozen, not chosen here.** DL-150/DL-199 already ruled: 1-bit
  whole-cell grids, `shape-rendering="crispEdges"`, one `<rect width="1"
  height="1">` per lit cell, filled with `currentColor` so tier colors keep
  applying through the wrapper (`StateDot.tsx`, `BadgeGlyph.tsx` are the
  shipped exemplars).
- **Night Owl palette via `--cx-*` tokens only.** No raw hex; glyphs carry no
  color of their own — `currentColor` end to end.
- **The even-11px-multiple rule governs Departure Mono font sizes, not SVG
  boxes** (`tokens.css:51-57` — it is about UPM font-pixels). The glyph grid
  therefore neither inherits nor fixes the pre-existing 15px `.r-tab-icon`
  violation (`app.css` `.r-tab .r-tab-icon`). What the grid MUST satisfy is
  whole-pixel placement inside the 34px `.r-tab`, so its 1px cells do not
  straddle device pixels.
- **Item-level union, per Matt's frozen ruling.** `ActivityBarItem` splits AT
  THE ITEM: `{ kind: "glyph"; … }` vs `{ kind: "avatar"; … }`. A field-level
  union (one `icon` field with two shapes) was explicitly rejected. Both the
  refactor and the glyph work land in RIG-3603.
- **Out of scope for SVG conversion:** the 4 comment-only glyphs (`─` U+2500,
  `≙` U+2259, `⌘` U+2318, `═` U+2550); math/punctuation glyphs in comments and
  test names (`§`, `⇒`, `≥`, …), which never render.
- **The `stub-data.ts` fixture log glyphs are not converted, but they are not
  free either.** `➜` (U+279C) and `▪▪▪▪` render through `.term-body`, which
  uses `var(--cx-font-ui)` (`app.css:1273`) — there is NO separate terminal
  font stack — and `agent.png` captures that exact fixture
  (`AGENT_ID = "acc-compass-ui"`). So after the pin is removed they would
  rasterize as tofu. They stay text, but T8 must first replace the two fixture
  strings with ASCII equivalents (`->`, `####`); the module hand-fakes a
  representative fleet, so the strings are not a contract.
- **Unifont-pin removal is a separate stacked PR**, and it is NOT the only
  recapture. T4/T5 change pixels inside existing baselines, so the glyph lane
  recaptures at T6; T8 then recaptures again against the shrunk font set.
- Markdownlint-clean record; Simple Technical English.

## Approach

### The `<Glyph/>` primitive

Adopt the DL-150/DL-199 technique as-is. A new
`apps/ui/src/components/Glyph.tsx` renders a fixed **11×11** 1-bit grid at one
CSS px per cell:

```tsx
<svg
  viewBox="0 0 11 11"
  width="11"
  height="11"
  shape-rendering="crispEdges"
  aria-hidden="true"
>
  <For each={GLYPH_CELLS[props.name]}>
    {([x, y]) => <rect x={x} y={y} width="1" height="1" fill="currentColor" />}
  </For>
</svg>
```

11×11 (not StateDot's 9×9) for two reasons: an odd grid has a true center cell,
matching the 9×9 exemplars, and pictographs (a folder, a branch, a gear) need
more cells than a state dot.

This does **not** reconcile the pre-existing 15px slot violation — that rule
governs Departure Mono font sizes via UPM font-pixels, and an SVG box has no
UPM (D2). The real hazard is placement: an 11px box flex-centered in the 34px
`.r-tab` lands on a half-pixel offset, smearing every 1px cell across two
device pixels, so the box is pinned to a whole-pixel offset (T3+T4).

Bitmap storage follows `BadgeGlyph.tsx` exactly: a module-level
`GLYPH_CELLS: Record<GlyphName, ReadonlyArray<readonly [number, number]>>`
of `[x, y]` lit cells, transcribed from frozen ASCII grids added to
`design/components.md` §Glyphs (`#` = lit). Keying the table on the exhaustive
`GlyphName` union makes a name without a bitmap a compile error, not a runtime
blank — the same guard `BadgeGlyph`'s `GlyphKey` provides.

`GlyphName` starts as the closed set of the four static tabs —
`"status" | "files" | "vcs" | "pr"` — and grows semantic names (never
character names) as the chrome audit (T5) converts further sites.

### The `ActivityBarItem` split (Matt's frozen ruling)

The union splits **at the item**, in `apps/ui/src/constants.ts`:

```ts
interface ActivityBarItemBase {
  title: string;
  group: RightTabGroup;
}

/** A static tab: a fixed chrome symbol from the closed glyph set. */
export interface GlyphTabItem extends ActivityBarItemBase {
  kind: "glyph";
  id: StaticRightTab;
  name: GlyphName;
}

/** A fleet agent tab: a person's initial, derived once from the handle. */
export interface AvatarTabItem extends ActivityBarItemBase {
  kind: "avatar";
  id: `agent:${string}`;
  letter: string;
  group: "fleet";
  agentId: string;
  unreachable?: boolean;
}

export type ActivityBarItem = GlyphTabItem | AvatarTabItem;
```

The split also moves `agentId`/`unreachable` onto the avatar arm only — today
they are optional on every item, but only fleet agent tabs ever carry them
(`RightSidebar.tsx` `FleetPane`/`AgentUnreachable` and the `StateDot` badge all
key off `agentId`). `FleetPane`, `AgentUnreachable`, and `activeFleetItem`
narrow from `ActivityBarItem` to `AvatarTabItem`, deleting their
`agentId ? … : undefined` dances.

The avatar arm owns the initial derivation **once**. Both
`fleetItemForAgent` and `unreachableFleetItem` currently duplicate
`(handle.at(0) ?? "?").toUpperCase()`, which splits a surrogate pair and can
expand to multiple characters for some scripts. A single helper replaces both:

```ts
/** First grapheme of the handle, NFKD-normalized with combining marks
 *  stripped (É→E), uppercased, then clamped to one printable ASCII
 *  character; anything else degrades to "?" (see D1). */
export function avatarInitial(handle: string): string;
```

Implementation, in order: `Array.from(handle)[0]` (code-point safe, not
`.at(0)`, which splits a surrogate pair) → `.normalize("NFKD")` and drop
combining marks → `.toUpperCase()` → return `"?"` unless the result is exactly
one character in the printable-ASCII range.

The NFKD step is what keeps accented-Latin handles distinguishable (`Émile`→`E`
rather than `?`); the clamp is what lets the Unifont pin retire. Both are D1.

### The render site and the CSS split

The single render site (`RightSidebar.tsx`, the `{tab.icon}` span inside the
`.r-tab` button) branches on the discriminant. `tab` is a plain `For` item, so
a ternary narrows cleanly:

```tsx
<span class="r-tab-icon" data-kind={tab.kind} aria-hidden="true">
  {tab.kind === "glyph" ? <Glyph name={tab.name} /> : tab.letter}
</span>
```

The shared `.r-tab .r-tab-icon { font-size: 15px }` rule splits by `data-kind`:

- `.r-tab .r-tab-icon[data-kind="glyph"]` — an 11×11 box (`display: inline-flex`,
  no font properties; the SVG is the content), centered in the unchanged
  `.r-tab` button.
- `.r-tab .r-tab-icon[data-kind="avatar"]` — text, DS mono face at 15px
  (Space Mono is legal at ≤16px), `line-height: 1` as today.

The `StateDot` sibling and its absolute bottom-right badge CSS are untouched.

### Chrome glyph audit

Beyond the activity bar, rendered chrome glyphs (`▸` disclosure, `◆`/`★`/`☆`
markers, `✓`/`✗` status, `⚙` settings, `⊞` grid, `✕` close, `⟨⟩` framing, and
the rest of the ~23-site census) convert to `<Glyph name>` inline at their
sites, extending `GlyphName` per adopted symbol (T5). Fixture log content in
`stub-data.ts` and comment-only glyphs stay untouched (Global Constraints).
Whether typographic marks that a brand face already covers (e.g. `−`, `•`) may
stay as text is settled by D3.

### Unifont retirement conditions

The pin (`tools/toolchain/chromium-e2e-env.nix`, fontDirs: unifont +
unifont_upper) can be removed only when **no rendered character falls outside
brand-face coverage**, which requires ALL of:

1. Every rendered chrome glyph converted or verified covered (T5 audit).
2. The avatar arm clamped to printable ASCII (`avatarInitial`, D1) — handles
   are charset-unconstrained (`agent.proto` `from_handle`, no schema
   validation), so without the clamp an arbitrary initial re-opens the gap.
3. Fixture log glyphs (`➜`, `▪▪▪▪`) replaced with ASCII in `stub-data.ts`
   (T8 step 1). They are content, not chrome, so they are never converted to
   SVG — but they are NOT exempt from coverage: `.term-body` renders through
   `var(--cx-font-ui)` (`app.css:1273`), the same stack as everything else,
   and `agent.png` captures them. There is no terminal font stack to fall back
   on.

Removal changes the fontconfig nix store path and invalidates all 11 visual
baselines in `apps/ui/e2e/__screens__/`, so it lands as its own stacked PR
with its own recapture (T8) — never mixed into a glyph slice. Note this is the
SECOND recapture: T4/T5 already move pixels inside the same baselines, which
T6 recaptures first.

## Alternatives considered

- **Field-level union (keep one `icon` field, vary its type).** REJECTED by
  Matt, explicitly and finally. A field union leaves every consumer testing the
  field's shape instead of the item's kind, keeps `agentId`/`unreachable`
  optional on items that can never carry them, and gives the avatar arm no
  natural owner for the initial derivation. Do not re-litigate.
- **Keep the Unifont pin permanently.** Rejected: ~14 MB of toolchain weight
  to paper over a small closed set of chrome symbols, and it only fixes the
  e2e environment — end-user machines never had the pin, so real browsers
  already render these glyphs from arbitrary OS fallback fonts, off-brand and
  inconsistent. The pin hides the problem from the one environment that could
  catch it.
- **A webfont/icon-font of custom glyphs.** Rejected: re-introduces font
  loading, hinting, and anti-aliasing where DL-150/DL-199 already shipped a
  crisper, zero-asset answer; a font also cannot guarantee whole-cell 1-bit
  rendering at exactly one CSS px per cell.
- **9×9 grid matching StateDot.** Rejected for chrome glyphs: it has no
  center-cell advantage over 11×11 (both are odd), and it gives pictographs —
  gear, folder, branch — too few cells to read at tab size. The baseline
  argument does NOT apply: neither 9 nor 11 resolves the 15px slot violation,
  because that rule governs Departure Mono font sizes, not SVG boxes (D2).

## Plan

Tasks in dependency order. T1–T7 are one PR lane (RIG-3603); T8 is a separate
stacked PR.

### T1 — `<Glyph/>` primitive and bitmap data

Add `apps/ui/src/components/Glyph.tsx`: the `GlyphName` union, the
`GLYPH_CELLS` table for the four static-tab glyphs (status grid, files folder,
vcs branch, pr arrows), and the component rendering the 11×11 `crispEdges`
SVG on `currentColor`, following `BadgeGlyph.tsx`'s shape. Transcribe the four
frozen ASCII grids into `design/components.md` §Glyphs; `GLYPH_CELLS` cites
them. Unit-test that every `GlyphName` key yields a non-empty cell list and
all cells lie in `0..10`.

Interfaces:

```ts
export type GlyphName = "status" | "files" | "vcs" | "pr";
export const Glyph: Component<{ name: GlyphName }>;
```

### T2 — `avatarInitial` helper

In `apps/ui/src/constants.ts`: the single owner of initial derivation, today
duplicated at `:137` and `:154`. Take `Array.from(handle)[0]` (never `.at(0)`,
which splits a surrogate pair), NFKD-normalize it and strip combining marks so
`É`→`E`, uppercase, then clamp to one printable ASCII character, else `"?"`
(D1).
Unit-test: plain handle, empty string, surrogate-pair-leading handle (e.g. an
emoji → `?`), a lowercase handle, an accented Latin handle (`Émile` → `E`), a
non-Latin script (`Живко` → `?`), and a script whose uppercase expands to
multiple characters.

Interfaces:

```ts
export function avatarInitial(handle: string): string;
```

### T3+T4 — `ActivityBarItem` union, constructors, render site and CSS (ONE commit)

**These cannot be separate commits.** `GlyphTabItem` has neither `icon` nor
`agentId`, and the activity-bar loop reads BOTH on the un-narrowed union:
`{tab.icon}` at `RightSidebar.tsx:689` and
`tab.agentId ? store.agentById(tab.agentId) : undefined` at `:662`. Landing the
union without the render branch is a TS-strict error, so a split leaves a red
commit mid-stack and breaks bisection.

In `apps/ui/src/constants.ts`: replace the single interface with the
`GlyphTabItem | AvatarTabItem` union (shapes as frozen in `## Approach`).
`RIGHT_SIDEBAR_TAB_BY_ID` entries become `kind: "glyph"` with `name` replacing
`icon`; `fleetItemForAgent`/`unreachableFleetItem` return `AvatarTabItem` with
`letter: avatarInitial(…)`. Narrow `FleetPane`, `AgentUnreachable`, and
`activeFleetItem` in `RightSidebar.tsx` and the `rightTabGroups` memo typing in
`store.ts` to the arm they actually handle.

At the render site: the `.r-tab-icon` span gains `data-kind={tab.kind}` and
branches `tab.kind === "glyph" ? <Glyph name={tab.name} /> : tab.letter`; the
`:662` agent lookup moves behind a `tab.kind === "avatar"` guard. In `app.css`
split `.r-tab .r-tab-icon` into the `[data-kind="glyph"]` box (no font
properties) and the `[data-kind="avatar"]` 15px mono rule; `.r-tab
.cx-state-dot` untouched. The glyph box MUST sit at a whole-pixel offset inside
the 34px `.r-tab` — an 11px box flex-centered there lands on a half pixel and
smears every 1px cell across two device pixels (see D2).

Unit-test both constructors: a resolvable agent yields `kind: "avatar"` with no
`unreachable`; a pin yields `unreachable: true`; both route the initial through
`avatarInitial`. Component-test BOTH arms and the whole-pixel offset — no test
covers this surface today.

Interfaces:

```ts
export interface GlyphTabItem {
  kind: "glyph";
  id: StaticRightTab;
  name: GlyphName;
  title: string;
  group: RightTabGroup;
}
export interface AvatarTabItem {
  kind: "avatar";
  id: `agent:${string}`;
  letter: string;
  title: string;
  group: "fleet";
  agentId: string;
  unreachable?: boolean;
}
export type ActivityBarItem = GlyphTabItem | AvatarTabItem;
export function fleetItemForAgent(agent: Agent): AvatarTabItem;
export function unreachableFleetItem(pin: PinnedAgent): AvatarTabItem;
```

```tsx
// RightSidebar.tsx render-site contract
<span class="r-tab-icon" data-kind={tab.kind} aria-hidden="true">
  {tab.kind === "glyph" ? <Glyph name={tab.name} /> : tab.letter}
</span>
```

Component tests (the missing regression net): a static tab renders an SVG with
`shape-rendering="crispEdges"` and no text; a fleet tab renders the initial as
text plus its `StateDot`; an unreachable tab renders no `StateDot`.

### T5 — chrome glyph audit and conversion (THREE commits)

Sweep the rendered non-ASCII chrome sites (census at main `97741c5ce2f4`:
`LeftSidebar.tsx` `◆ ★ ☆ ▸ ▼ ◉ ○ ▦ ▤ ✓ ⚙`, `RightSidebar.tsx`
`▸ ▾ 🗀 ⎇ ✓ ✗ • − ·`, `App.tsx` `◇ ▦ ▐ ▌`, `AgentView.tsx` `⊞ ▁ ▏ ✕`,
`UsageBar.tsx` `⎇`, `LogPanel.tsx` `■ ⟨⟩`, `SessionTrace.tsx` `↗`,
`BacklogView.tsx` `▸`, `IssueCard.tsx`/`DoneView.tsx` `−`). The `·` in
`RightSidebar.tsx:32` `FILE_ICON` is part of the census — Space Mono covers
it, so it is a "kept, covered" row, not a conversion.

**Split into three commits by surface**, each independently eyeball-able:
T5a `LeftSidebar` + `App`; T5b `RightSidebar` + `AgentView`; T5c the rest.
Pixel-art authorship for ~15 bitmaps is judgment work, and one review of 10
files × 15 grids is past the size where a reviewer checks each grid against
its rendering. The audit table accretes per commit.

Per site: convert to `<Glyph name={…}>` with a new semantic `GlyphName` +
bitmap, or record it in the audit table as covered-by-brand-face/text-kept
(per D3: convert wholesale). Extend the T1 cell-validity test automatically via the
keyed table.

The audit table carries an **a11y column** per site: *decorative*
(`aria-hidden`, no change) or *name-bearing* (the site must gain or keep a
text/`aria-label` name). Without it each conversion silently decides whether a
glyph carried meaning — e.g. `LogPanel.tsx` `■ stop` keeps its text name, but
bare `✓`/`✗` verdict marks read as nothing once hidden.

Its completeness criterion is checkable, not trusted: **every match of a
non-ASCII sweep over `apps/ui/src` at the pinned rev** appears as a row.

Deliverable includes the audit table appended as a section in this record, not
a new file elsewhere.

Interfaces:

```ts
// GlyphName grows per adopted symbol; names are semantic ("disclosure",
// "check"), never character names ("triangle-right"). Example after T5:
export type GlyphName =
  | "status" | "files" | "vcs" | "pr"
  | "disclosure" | "disclosure-open" | "check" | "cross" | "close"
  | "gear" | "star" | "star-outline"; // …exact set fixed by the audit table
```

### T6 — baseline recapture for the glyph conversion

T3+T4 and T5 change rendered pixels inside committed baselines:
`right-sidebar.png` clips `aside.right`, which holds the activity bar T3+T4
rewrites, and the seven `fullPage: true` captures (`bridge`, `bridge-empty`,
`agent`, `backlog`, `done`, `settings`, `bridge-prs`) hold the LeftSidebar and
App glyphs T5 converts. So this lane must recapture before it can go green;
without this task the T1–T5 PR fails the visual gate with no step that fixes
it.

Recapture via the CI regen lane (`ci.yml` `workflow_dispatch`, `regen: visual`),
then eyeball every diff: only glyph cells may move. `state-dot.png` is the
positive control and MUST NOT change — it is textless, so any diff there means
the conversion leaked into unrelated rendering.

Interfaces:

```text
apps/ui/e2e/__screens__/ — baselines recaptured for the glyph conversion
state-dot.png — positive control, byte-identical before and after
```

### T7 — docs

Update `design/components.md` §Glyphs with the full frozen grid set and the
two-arm activity-bar item contract; note the DL-367 adoption in the section
header. No changelog beyond the record.

Interfaces:

```text
design/components.md §Glyphs — frozen ASCII grids, one per GlyphName ("#" = lit)
```

### T8 — Unifont pin removal + baseline recapture (SEPARATE STACKED PR)

Gated on T6 complete; D1's clamp is what makes it possible. Three steps, in order:

1. Replace the `stub-data.ts` fixture log glyphs with ASCII (`➜` → `->` at
   :525-526, `▪▪▪▪` → `####` at :535-537 and :722). Without this the recapture
   bakes tofu into `agent.png`, because `.term-body` has no separate font stack.
2. Remove `unifont` and `unifont_upper` from
   `tools/toolchain/chromium-e2e-env.nix` fontDirs.
3. Recapture all 11 baselines; eyeball each diff for tofu/substitution before
   accepting.

The PR carries nothing beyond these, so a rendering regression bisects to the
font change alone — relative to the post-T6 baselines, not to today's.

Before opening it, re-run the census: no rendered character may fall outside
SpaceMono + departure-mono coverage. That is the real retirement condition; T6
and D1 are necessary, not sufficient.

Interfaces:

```text
apps/ui/src/stub-data.ts — fixture log strings ASCII-only
tools/toolchain/chromium-e2e-env.nix — fontDirs shrinks to SpaceMono + departure-mono
apps/ui/e2e/__screens__/ — 11 baselines recaptured against the shrunk font set
```

## Tasks

- [ ] T1: `<Glyph/>` primitive + 4 static-tab bitmaps + `components.md` grids + cell-validity test
- [ ] T2: `avatarInitial` helper + unit tests (surrogate pair, empty, accented Latin, non-Latin, multi-char uppercase)
- [ ] T3+T4 (ONE commit): `ActivityBarItem` → `GlyphTabItem | AvatarTabItem` union, both constructors, the render branch for `{tab.icon}` (`:689`) AND `tab.agentId` (`:662`), the `.r-tab-icon` CSS split with a whole-pixel glyph box, and tests for BOTH arms
- [ ] T5a: chrome glyph conversion — `LeftSidebar` + `App` (audit table rows, a11y column)
- [ ] T5b: chrome glyph conversion — `RightSidebar` + `AgentView`
- [ ] T5c: chrome glyph conversion — remaining surfaces; audit table complete against a non-ASCII sweep
- [ ] T6: baseline recapture for the glyph conversion (`state-dot.png` unchanged)
- [ ] T7: `design/components.md` §Glyphs update (DL-367)
- [ ] T8: Unifont pin removal + 11-baseline recapture — SEPARATE stacked PR, gated on T6 + D1

## Resolved decisions

- **D1 — avatar fallback: NFKD-normalize, then clamp to ASCII.** Ruled by Matt.
  `avatarInitial` takes the first grapheme, NFKD-normalizes it and strips
  combining marks (`É`→`E`, `Ó`→`O`), uppercases, then clamps to one printable
  ASCII character, falling back to `?` only for scripts no Latin letter
  represents (CJK, Cyrillic, emoji). Handles are charset-unconstrained (proto
  `from_handle`, no schema validation), and the initial exists to tell agents
  apart (`constants.ts:127-130`: "a per-agent glyph, no hardcoded Supervisor
  ◆") — a bare clamp would collapse distinct non-ASCII handles to identical `?`
  tabs. A font fallback was rejected: it keeps the pin forever and makes T8
  impossible. The tab's `title`/`aria-label` carries the full handle in every
  case. **This is what lets the Unifont pin retire.**
- **D2 — glyph cell grid: 11×11.** Ruled by Matt. An odd grid gives a true
  center cell like the 9×9 exemplars, and the extra resolution matters for
  pictographs (gear, folder, branch). StateDot and BadgeGlyph stay frozen at
  9×9 per DL-150/DL-199; they are physically separate components, so two grid
  sizes coexisting is fine.
  11×11 does **not** resolve the pre-existing 15px slot violation — the
  even-11px-multiple rule (`tokens.css:51-57`) governs Departure Mono font
  sizes via UPM font-pixels, and an SVG box has no UPM. An earlier draft of
  this record claimed it did and was wrong.
  The real hazard is placement: an 11px box flex-centered in the 34px `.r-tab`
  lands on a half-pixel offset, smearing every 1px `crispEdges` rect across two
  device pixels. **T3+T4 MUST pin the glyph box to a whole-pixel offset and
  assert it.** A 12×12 grid would center integrally for free but loses the
  center cell; rejected on that trade.
- **D3 — audit breadth: convert wholesale.** Ruled by Matt. Every chrome symbol
  becomes a `<Glyph/>`, including ones Space Mono already covers. One
  vocabulary is cheaper to hold than a coverage table, and a per-character
  check against two font files is fragile and invisible in review. The cost is
  accepted: converting covered glyphs adds baseline churn without adding
  coverage. Typographic marks inside prose (`−` in a diff stat, `•` as a
  separator, `·` in `FILE_ICON`) stay text where the brand face covers them —
  the audit table records each keep with its reason.
- **D4 — avatar slot type size: keep 15px.** Non-load-bearing, deferred by
  design. The avatar arm keeps 15px mono; a later pass may move it to 16px
  (still ≤16px Space Mono) or an 11px-multiple display treatment. This record
  changes the avatar arm's type, not its look.
