# Design: Compass DS Token Full Cutover + Canvas Migration

Status: Draft
Supersedes: the incremental/strangler framing of SEA-1876 (component-tier follow-up to PR #220)
Owner lane: compass-ui · DS-tier owner: compass-ux (D2, `compass-ux-foundation/design.md`)

## Problem / Intent

Compass has two parallel token tiers: the DS `--cx-*` semantic tier
(`apps/ui/src/design/tokens.css`, merged) and a legacy GitHub-dark `:root` tier
at the top of `apps/ui/src/app.css` (lines 7-58: `--bg: #0a0d12`, `--text`,
`--st-*`, `--purple`, `--accent`, …) that ~540 `var()` references still
consume. The strangler plan (D10/DL-157, refined by SEA-1876) kept both tiers
alive and migrated surface by surface. Matt's ruling (2026-08-07): *"why do we
have 'legacy' refs in a codebase that hasn't even been dogfooded yet?"* — a
pre-dogfood codebase has no live users to protect, so the strangler frame is
wrong. This record replaces it with a **full cutover**: delete the legacy
`:root` tier, flip every consumer to `--cx-*` in one migration, un-shadow
`base.css`, and flip the app canvas from GitHub-dark (`--bg: #0a0d12`,
`app.css:9`) to Night Owl (`--cx-bg` → `--rigel-night: #011627`,
`tokens.css:82`, `tokens.css:9`).

## Approach

One migration, one PR train, no coexistence period. The legacy tier and all its
consumers go in a single coordinated flip, proven by a Playwright visual-smoke
harness (the repo currently has NO browser harness — `apps/ui/package.json`
carries only `happy-dom`/`@happy-dom/global-registrator` (lines 29, 35); no
Playwright, no chromium, no computed-style tests). Matt reviews the
before/after screenshots; that is the acceptance gate for the visible restyle.

### Preconditions

- **PR #220 merged first.** #220 ("adopt the Compass design-system token base
  on the app shell") is still OPEN as of this record; main `94754d0a` has
  `App.tsx` importing only `./app.css` (`App.tsx:4`) and no `data-theme` on
  `index.html`. #220 lands the three-import cascade (`tokens.css` →
  `base.css` → `app.css`), the `data-theme="night"` root attribute, and the
  60-ref shell-chrome flip. This record's tasks rebase on it and finish the
  job; nothing here re-does #220's work.
- **Coordination with the parallel warden task.** A sibling task (same wave)
  deletes the warden producer, including `.role-pip[data-role="warden"]`
  (`app.css:531-533`, `color: var(--purple)`), removing one of the five
  `--purple` consumers. This record's purple fork (OQ-1) covers the remaining
  four.

### (a) Delete the legacy `:root` tier

`app.css:7-58` is deleted. Every name in it is either flipped to a `--cx-*`
target (below), resolved by an Open Question, or is a **layout knob, not a
design token** — `--topbar-h: 44px`, `--usage-h: 26px`, `--right-w: 400px`
(`app.css:52-57`) have no DS counterpart and are not colors/type/space; they
survive in a new, clearly-commented `/* app layout knobs (not DS tokens) */`
`:root` block so the D7 guard can allowlist them by name. `--pink: #db61a2`
(`app.css:32`) has zero consumers (verified: no `var(--pink)` match anywhere in
`apps/ui/src`) and is deleted outright.

The mechanical mapping (targets verified present in `tokens.css:80-213`):

| Legacy (`app.css:7-58`) | Refs | Target | Note |
| --- | --- | --- | --- |
| `--bg` `#0a0d12` | 8 | `--cx-bg` | canvas flip to `--rigel-night #011627` |
| `--bg-raised` | 12 | `--cx-bg-raised` | |
| `--bg-panel` | 17 | `--cx-bg-panel` | |
| `--bg-card` | 18 | `--cx-bg-panel` | no 4th surface tier in DS; collapse per #220's precedent |
| `--bg-hover` | 20 | `--cx-bg-hover` | |
| `--bg-active` | 13 | `--cx-bg-active` | |
| `--border` | 65 | `--cx-border` | |
| `--border-strong` | 17 | `--cx-border-strong` | |
| `--text` | 44 | `--cx-text` | |
| `--text-dim` | 51 | `--cx-text-dim` | |
| `--text-faint` | 68 | `--cx-text-faint` | |
| `--accent` | 47 | `--cx-accent` | |
| `--accent-dim` | 9 | **OQ-3** | wash vs solid fidelity fork |
| `--add` | 20 | `--cx-ok` | recolor `#3fb950` → `--rigel-success #22da6e` |
| `--del` | 19 | `--cx-error` | recolor `#f85149` → `--rigel-red #ef5350` |
| `--warn` | 18 | `--cx-warn` | recolor `#d29922` → `--rigel-amber #ecc48d` |
| `--purple` | 6 | **OQ-1** | banned from `--cx-*` (one-accent rule) |
| `--st-working/idle/waiting/paused/error/done/stopped/disconnected` | 10 | `--cx-st-*` | see (d) — visible recolor |
| `--st-blocked` / `--st-review` | 2 | `--cx-issue-blocked` / `--cx-issue-in_review` | sole consumers are BOARD_LANES (`constants.ts:18-22`) — clean issue-axis mapping; see OQ-2 |
| `--st-merged` | 3 | **OQ-2** | PR-merged semantic, no DS counterpart |
| `--radius` `8px` / `--radius-sm` `5px` | 39 | `--cx-radius-md` `6px` / `--cx-radius-sm` `3px` (`tokens.css:200-202`) | stated assumption: the px delta is part of the deliberate restyle, reviewed in screenshots — not an OQ. `md` not `lg` (`10px`, `tokens.css:202`): the restyle tightens per the dense-UI direction |
| `--font-mono` (ui-monospace stack) | 35 | `--cx-font-ui` (`--rigel-mono` = Space Mono, `tokens.css:61,189`) | type-face flip, visible. Note: erases the code-vs-body face distinction — 35 `--font-mono` refs collapse onto the same `--cx-font-ui` the body now uses; no `--cx-font-code` exists (`--cx-ed-*` editor block reserved-unbuilt). Intended by the mono-UI design; called out so the screenshot review reads it as intent |
| `--topbar-h` / `--usage-h` / `--right-w` | 3 | kept as layout knobs | renamed block, guard-allowlisted |

Ref counts from a `var(--…)` tally of `app.css` at `94754d0a` (546 total
`var()` refs). The tally also exposed **five referenced-but-never-defined
vars** riding on fallbacks or silently resolving to nothing: `--danger`
(`app.css:1412,3553,3654`, fallback `#f87171`) → `--cx-error`; `--bg-inset`
(`:1282,3574`) → `--cx-bg-raised`; `--surface-2` (`:3299`) → `--cx-bg-hover`;
`--text-muted` (`:3300`) → `--cx-text-dim`; `--fg-muted` (`:3475`, NO fallback
— currently renders `inherit`-broken) → `--cx-text-faint`. The cutover fixes
all five; they are evidence the two-tier period was already leaking bugs.

### (b) Flip every remaining consumer

Beyond `app.css`, exactly one TS/TSX file names legacy vars:
`constants.ts:17-23` — `BOARD_LANES` colors the five board lanes with
`var(--st-paused)`, `var(--st-blocked)`, `var(--st-working)`,
`var(--st-review)`, `var(--st-merged)`. The DS already defines the issue-axis
tokens for exactly these five lanes (`tokens.css:139-143`: `--cx-issue-queued/
blocked/in_progress/in_review/done`), so BOARD_LANES flips to `--cx-issue-*`
one-for-one — including `queued` off the misused `--st-paused` and `done` off
`--st-merged`. This is the clean half of the `--st-merged` story; the PR-badge
half is OQ-2.

### (c) Un-shadow base.css

`app.css:60-103` duplicates `base.css`'s global layer at equal specificity and
loads last (post-#220 cascade: `tokens.css` → `base.css` → `app.css`), so the
legacy copies win: the reset (`app.css:60-69`), `body`
(`app.css:71-81`, `background: var(--bg); color: var(--text)`), `button`
(`:83-87`), and the scrollbar set (`:89-103`). `base.css` carries the DS
versions — `body { font-family: var(--cx-font-ui); … background: var(--cx-bg);
color: var(--cx-text); font-synthesis: none; }` (`base.css:20-30`) and the
`--cx-*` scrollbars (`base.css:39-53`). The cutover **deletes app.css:60-103
wholesale**; base.css becomes the sole global layer, which is what flips the
canvas to Night Owl and the body face to Space Mono in one stroke. The
scrollbar-hover raw hex `#45505f` (`app.css:101`) dies with the block —
base.css already answers it with `--cx-text-faint` ("legacy used a raw grey;
here the faint readable-meta text token", `base.css:49-53`) — OQ-4 asks
whether that answer stands or a dedicated token is coined.

### (d) The `--st-*` → `--cx-st-*` state recolor (visible, not a rename)

The legacy state palette (`app.css:35-45`) and the DS one
(`tokens.css:129-136`) assign **different hues per state** — this is a visible
recolor across every state dot, lane head, and badge:

| State | Legacy (`app.css:35-45`) | DS (`tokens.css:129-136`) |
| --- | --- | --- |
| working | `#3fb950` GitHub green | `--rigel-green #addb67` |
| done | `#2ea043` green | `--rigel-cyan #7fdbca` |
| paused | `#58a6ff` blue | `--rigel-mute #5f7e97` grey |
| waiting / disconnected | `#d29922` | `--rigel-amber #ecc48d` |
| error | `#f85149` | `--rigel-red #ef5350` |
| idle | `#616b78` | `--rigel-mute #5f7e97` |
| stopped | `#414b58` | `--rigel-mute #5f7e97` |

Consumers: the `.state-dot[data-state=…]` block (`app.css:483-514`), the
`.obs-run.live` dot (`app.css:3626`), and BOARD_LANES (above). `paused` STAYS
a real `AgentState` (Matt ruled) — the flip recolors it
(`--st-paused` blue → `--cx-st-paused` grey, `tokens.css:135`), it does not
remove it. Note the D3 component file `design/components/state-dot.css`
already exists on `--cx-st-*` (`.cx-state-dot`, `state-dot.css:13-42`) but is
imported nowhere; whether the cutover swaps `.state-dot` markup to the D3
component is deliberately OUT of scope here (that is the D3 component-adoption
lane) — this record only re-points the existing `.state-dot` rules at
`--cx-st-*`.

### (e) The D7 stylelint guard, wired as error

D2/D7 specify "the stylelint CI check banning raw hex, `--rigel-*`, and
literal durations/easings outside `tokens.css` (warn until adoption step 5,
then error)" (`compass-ux-foundation/design.md:840-842`; DL-154 at `:1020`).
It was never wired: zero stylelint config or dependency exists in the repo
(verified by grep over `apps/ui`). Since this record IS the legacy retirement
(the old "step 5"), the guard lands **directly at error severity** — no warn
phase, there is nothing left to warn about. Allowlist: `tokens.css` (raw hex +
`--rigel-*`), the mark component's CSS for `--rigel-purple` (DL-154), and the
named layout knobs. `app.css` carries 39 raw hex literals today; the sweep
task drives that to the guard's zero.

The guard as D7 specifies it (raw hex, `--rigel-*`, durations/easings) does
NOT catch a stale legacy *var* — `var(--purple)` or `var(--st-working)` passes
it clean. Since the guard is billed as what makes the deleted tier
unrevivable, it also bans the legacy vocabulary by name (a
`declaration-value-disallowed-list` regex over `--bg`, `--text`, `--st-`,
`--accent`, `--purple`, `--radius`, `--font-mono`, minus the named layout
knobs) — cheap, and it converts the OQ-1 undefined-`--purple` failure mode
(below) from an invisible `inherit` into a CI red. The 14 unimported
`design/components/*.css` files (`state-dot.css`, `button.css`, …) are in the
guard's glob; they are expected-clean per D7 authorship (`--cx-*` already), so
a surprise red there is a real finding, not guard noise.

### Stated assumptions & known follow-ups

Decided at a higher level or reviewed in the screenshot pass — recorded so no
silent decision hides, but not batched to Matt as forks:

- **Scrollbar-hover grey** takes base.css's `--cx-text-faint` (`base.css:49-51`),
  which T2's un-shadowing makes live with no further work. Revisit only if the
  T2 screenshots read wrong. (A dedicated single-consumer `--cx-border-strong-hover`
  would be DS noise.)
- **Body font-size drops 13px → 12px** (`--cx-text-sm`, `tokens.css:192`) when
  base.css becomes the sole global layer. Most rules set their own size; any
  rule inheriting body size shrinks 1px. Intended (the DS UI base) — flagged so
  the T2 screenshot review reads it as intent, not a regression.
- **Radius / body-face** — see the mapping-table notes (radius `md` not `lg`;
  the mono-UI face-distinction erasure). Both frozen D7 choices, reviewed in
  screenshots.
- **Zero expected happy-dom test fallout.** No happy-dom test asserts a color,
  hex, computed style, or `var(--…)` string — `App.test.tsx` touches
  `.state-dot` as a class selector only (`:165`); `board.test.ts` asserts lane
  *states*, not colors (`:135-139`); the `test` task is style-blind. T4's
  BOARD_LANES flip changes `Lane.color` strings but nothing asserts them.
  Recorded so executors don't hunt for breakage that isn't there.
- **Dual focus-treatment (known follow-up, out of scope).** Three component
  `:focus` rules (`app.css:2139,2352,3524`, `outline: 2px solid var(--accent)`)
  survive the cutover as focus treatments separate from base.css's
  `:focus-visible` ring (`base.css:56-59`) — so post-cutover the app carries two
  focus vocabularies, a latent violation of D7's single-focus-treatment claim
  (`tokens.css:44-46`). Unifying them is a separate follow-up, named here so it
  is not lost.

### Alternatives considered

**The incremental strangler (SEA-1876's original frame, D10/DL-157).** Keep
both tiers; flip surface-by-surface (content, board, composer, settings) over
several PRs; retire the legacy `:root` last. Rejected: the strangler's whole
value is protecting a live system's users from a big-bang restyle, and
pre-dogfood Compass has no users to protect. Its costs are real and already
observed: a dead tier that must be kept coherent, half-migrated confusion
(the five undefined-var bugs above; the shell/canvas amber-vs-amber divergence
that PR #220 documents as a "documented deferral"), and every future PR paying the
"which tier?" tax. Matt's ruling closes it.

**Big-bang without a browser harness.** Rejected: the flip recolors every
state signal and changes the body typeface; happy-dom asserts none of that.
Matt explicitly ruled the restyle is proven by Playwright screenshots he
reviews. The harness is a precondition task, not an afterthought.

## Plan

## Global Constraints

- **Consumption rule (D2):** post-cutover, component CSS names ONLY `--cx-*`
  (+ the named layout knobs). No raw hex, no `--rigel-*`, no literal
  durations/easings outside `tokens.css` (`compass-ux-foundation/design.md:295-297`).
- **`tokens.css` is read-only for this lane.** Both blocks (brand-mirrored
  `--rigel-*` and the compass-ux-owned `--cx-*` tier). Any new token this
  migration needs (OQ-2, OQ-4) is coined BY compass-ux via coordination, never
  added here unilaterally.
- **Purple is never aliased into `--cx-*`** (`tokens.css:110` "Accent (blue;
  purple is NEVER aliased into --cx-*)"; `tokens.css:45-46` "purple is
  reserved for the brand mark only"; one-accent rule,
  `compass-ux-foundation/design.md:221-226`). OQ-1 cannot be resolved by
  minting `--cx-purple`.
- **Cascade order is load-bearing:** `tokens.css` → `base.css` → `app.css`
  (#220's `App.tsx` import order); nothing may reorder it.
- **`paused` stays:** the `AgentState` union member, its `--cx-st-paused`
  token, and `.state-dot[data-state="paused"]` handling all survive; only its
  color changes.
- **OQ-blocked work:** tasks touching an OQ surface land the mechanical
  remainder and leave the OQ surface on a `/* OQ-n pending */`-commented
  interim that maps to a **defined** token named in this record — never a
  legacy-named var (a legacy name would go undefined when T5 deletes the tier,
  reproducing the undefined-var bug class this record argues against). OQ-1's
  interim is `--cx-text-bright` (`tokens.css:100`); OQ-2's is `--cx-accent`
  (T4); OQ-3's is `--cx-accent-muted` (T3). Matt's ruling then only re-points
  an already-valid ref — no task blocks on his latency.
- **Merge atomically (see OQ-5 topology):** T2-T5 change the palette in
  deliberately-clashing intermediate states (Night-Owl canvas over legacy
  panels between T2 and T3). Those intermediates are fine as commits inside a
  train that merges as one unit; they must NEVER land on `main` independently.
  Whether the train is one PR or a stack merged together is OQ-5 (batched for
  Matt).
- Base revision: main `94754d0a` + PR #220 merged. The driver runs
  format/lint/tests and the PR train; tasks here only edit and report.

### T1 — Playwright visual-smoke harness (FIRST; Matt's acceptance gate)

Stand up the repo's first browser harness: Playwright + chromium driving
`vite dev` (or `vite preview`) against the stub store (the app boots fully on
`stub-data.ts` with no daemon, per `App.tsx:18-23`). Capture full-page
screenshots of the core surfaces: Bridge board, agent view (trace + composer),
right sidebar (PR pane visible), backlog + done views, settings, and a
state-dot close-up crop. Run once pre-cutover (baseline: legacy palette) and
after each flip task; Matt reviews the pairs. Keep it a smoke harness — no
pixel-diff CI gating in this record (screenshots are for human review), no
computed-style assertion suite.
Interfaces: consumes the dev server + stub store; produces
`apps/ui/playwright.config.ts`, `apps/ui/e2e/visual-smoke.spec.ts`, a
`package.json` script (`test:visual`), and a documented output dir of named
PNGs (`e2e/__screens__/<surface>.png`) the driver attaches for Matt.

### T2 — Un-shadow base.css (canvas flip)

Delete `app.css:60-103` (reset, `html/body/#root`, `body`, `button`,
scrollbar set). Verify no other `app.css` rule re-declares `body`/scrollbar
globals. Canvas goes Night Owl; body face goes Space Mono; scrollbar hover
goes `--cx-text-faint` (interim answer to OQ-4).
Interfaces: consumes T1 (baseline captured before this lands); produces the
edited `app.css` + a T1 screenshot pass. The T2 pass validates the
canvas/scrollbar/typeface **globals only** — between T2 and T3 the panels and
text still resolve legacy hexes, a deliberately-clashing intermediate that is
expected, not a coherence defect to review.

### T3 — Mechanical consumer flip (surfaces, lines, text, accents, type, radius)

Apply the mapping table in Approach (a) to every `var(--…)` legacy ref in
`app.css` except the `--st-*`/state block (T4) and the OQ-gated refs:
`--bg*`, `--border*`, `--text*`, `--accent`, `--add`/`--del`/`--warn`,
`--radius*`, `--font-mono`, plus the five undefined-var fixes
(`--danger`/`--bg-inset`/`--surface-2`/`--text-muted`/`--fg-muted`).
`--accent-dim` refs (9) take the #220 interim (`--cx-accent-muted`) with an
`/* OQ-3 pending */` comment. The 4 `--purple` refs (post-warden-delete) flip
to the **defined** interim `--cx-text-bright` (`tokens.css:100`) with an
`/* OQ-1 pending */` comment — a defined token, so T5's tier-deletion never
leaves them undefined; Matt's OQ-1 ruling only re-points them. Move the layout
knobs into the new commented block.
Interfaces: consumes the frozen mapping table + T2; produces the flipped
`app.css` (every legacy `var()` ref either flipped or on a defined-token
`/* OQ-n pending */` interim — zero undefined vars) + a T1 screenshot pass.

### T4 — State recolor: `.state-dot` + BOARD_LANES

Flip `app.css:483-514` and `:3626` from `--st-*` to `--cx-st-*` (eight
states, `paused` included); flip `constants.ts:17-23` BOARD_LANES to
`--cx-issue-*` five-for-five. `--st-merged`'s two remaining refs
(`app.css:1606` `.pr-state[data-state="merged"]`, `:2062` `.done-row-merge`)
take an `/* OQ-2 pending */` interim of `--cx-accent` (blue, the
least-wrong non-purple stand-in) until Matt/compass-ux rule. Update the
`StateDot.tsx:5-9` doc comment's color vocabulary to the Night Owl hues.
Interfaces: consumes T3; produces the flipped state rules + BOARD_LANES + a
T1 screenshot pass including the state-dot crop.

### T5 — Delete the legacy `:root` tier + raw-hex sweep

Delete `app.css:7-58` (everything not already moved/deleted by T2-T4),
including unused `--pink`. Sweep the remaining ~39 raw hex literals in
`app.css` (e.g. `color: #fff` at `:1605`, ask-error fallbacks `#f87171`) to
`--cx-*` equivalents. Grep-verify: zero `--bg`/`--text`/`--st-`/`--accent`/
`--purple`(post-OQ-1)/raw-hex outside the allowlist.
Interfaces: consumes T2-T4; produces an `app.css` whose only `:root` is the
layout-knob block; blocked-on-OQ surfaces carry their named interims.

### T6 — Wire the D7 stylelint guard (error severity)

Add stylelint + config to `apps/ui`: ban raw hex (`color-no-hex` +
declaration-value checks), `--rigel-*` references, and literal
durations/easings outside `design/tokens.css`; **also ban the legacy var
vocabulary by name** (a `declaration-value-disallowed-list` regex over `--bg`,
`--text`, `--st-`, `--accent`, `--purple`, `--radius`, `--font-mono`, minus
the named layout knobs) so a revived legacy var reds CI instead of resolving to
`inherit`. Allowlist the mark component's `--rigel-purple` (DL-154) and the
named layout knobs. Place the config at **`apps/ui`** level (the repo's lint is
whole-repo biome on the root project per `.moon/tasks/tag-bun.yml`; CSS lives
only in `apps/ui`, so an apps/ui-scoped stylelint task is the right seam),
wired into `apps/ui/moon.yml`'s `ci` task deps at **error**. The guard is the
cutover's ratchet — what makes the deleted tier unrevivable. The 14 unimported
`design/components/*.css` files are in-glob and expected-clean (D7 `--cx-*`
authorship); a red there is a real finding.
Interfaces: consumes T5 (a clean tree, or the guard reds); produces
`apps/ui/.stylelintrc.*` (or a `package.json` block, matching repo config
conventions), the toolchain dep, and the `moon.yml` CI wiring.

### T7 — Final screenshot pass + record close-out

Full T1 suite re-run; assemble the before/after pairs for Matt's review;
changelog entry; update this record's Open Questions with the rulings once
they land (driver relays).
Interfaces: consumes T1-T6; produces the reviewed screenshot set + changelog.

## Tasks

- [ ] T1 Playwright visual-smoke harness + legacy baseline screenshots
- [ ] T2 Un-shadow base.css (delete app.css:60-103) — canvas → Night Owl
- [ ] T3 Mechanical consumer flip per mapping table (+ 5 undefined-var fixes)
- [ ] T4 State recolor: .state-dot → --cx-st-*, BOARD_LANES → --cx-issue-*
- [ ] T5 Delete legacy :root tier + raw-hex sweep
- [ ] T6 D7 stylelint guard wired at error
- [ ] T7 Final screenshot pass, changelog, record close-out

## Open Questions

Four **LOAD-BEARING** forks batched for Matt (the driver relays; this lane does
not decide them). OQ-2 additionally needs compass-ux (DS-tier owner)
coordination for any coined token. (The former scrollbar-hover OQ is demoted to
a stated assumption — see *Stated assumptions & known follow-ups* — as it fails
the load-bearing bar: nothing blocks on it, no task carries an interim, and the
recommendation is the already-shipped base.css choice.)

1. **The 4 remaining `--purple` consumers** (LOAD-BEARING; T3 lands the
   `--cx-text-bright` defined interim, this ruling re-points it).
   `.tool-name` (`app.css:1212`), the tool-name-adjacent uses at `:1952` and
   `:2195`, and `.mention-chip.reserved` (`:3367-3368`, color + 16% wash).
   (The 5th, the warden role-pip `:532`, is deleted by the parallel warden
   task.) D2's one-accent rule bans purple from `--cx-*`
   (`tokens.css:110`, `:45-46`; `compass-ux-foundation/design.md:221-226`), so
   no mechanical flip exists. Options: **(a)** recolor to `--cx-accent` blue;
   **(b)** a neutral token (`--cx-text-bright` or `--cx-text-dim`) —
   de-emphasize rather than re-accent; **(c)** a narrow carve-out admitting
   purple for tool-name/reserved-mention semantics (widens the mark-only
   exception D7 guards, `design.md:616-620`); **(d)** keep legacy `--purple`
   as a deliberate documented exception (keeps a second tier alive — against
   this record's whole premise). Recommendation: **(a)** for
   `.mention-chip.reserved` (mentions are interaction-flavored, blue is the
   interaction color) and **(b)** `--cx-text-bright` for the three tool-name
   uses (emphasis, not accent). (c) and (d) undercut the one-accent rule and
   the cutover respectively.

2. **The unmapped `--st-merged` PR-badge token** (LOAD-BEARING; blocks part of
   T4; needs compass-ux). Of the three legacy state tokens with no `--cx-st-*`
   counterpart (`tokens.css:129-136` covers only the eight AgentStates),
   `--st-blocked #f85149` and `--st-review #a371f7` are resolved mechanically:
   their ONLY consumers are BOARD_LANES (`constants.ts:19,21`) and the DS has
   the issue axis — `--cx-issue-blocked` (red) / `--cx-issue-in_review` (amber)
   (`tokens.css:140,142`) — a clean map (visible purple→amber recolor for
   in-review). The real residue is `--st-merged #8957e5` (purple!), consumed
   OUTSIDE the lane/dot vocabulary as a PR-merged badge:
   `.pr-state[data-state="merged"]` bg (`app.css:1606`) and `.done-row-merge`
   color (`:2062`). Options: **(a)** map merged → `--cx-issue-done`
   (cyan; merged ≈ done on the task axis); **(b)** coin a `--cx-pr-merged`
   in the CI/review token family (`tokens.css:145-151`) — value TBD by
   compass-ux, noting GitHub's merged-purple collides with the one-accent
   rule; **(c)** map merged → `--cx-accent` (blue interim, as T4 stages).
   Recommendation: **(a)** — no new token, no purple, semantically honest;
   confirm with compass-ux that the issue axis may serve the PR badge or
   whether (b) is worth the coin.

3. **`--accent-dim` fidelity — the token AND its paired foreground**
   (LOAD-BEARING; token choice, 9 refs at
   `app.css:327,1013,1322,1372-1373,1449,2220,3003,3535`). PR #220 mapped
   `--accent-dim` (solid `#1f6feb`, `app.css:27`) → `--cx-accent-muted`, a
   24%-alpha wash (`tokens.css:117`) — a fidelity drop for the refs used as
   SOLID fills (button bgs `:1449,3535`, chosen-option bg `:1372`, badge bg
   `:3003`), not just borders. **Crucially, every solid-fill consumer pairs
   `--accent-dim` with a hard-coded white foreground** (`.ask-option.chosen`
   `color:#fff` `:1374`; the `:1449` send button; the `:3003` badge `:3004`;
   the `:3535` button `:3538`). Legacy `#1f6feb` is a dark blue — white on it
   passes contrast; but `--cx-accent` = `--rigel-blue #82aaff` (`tokens.css:111`)
   is a light pastel — white on it is ~1.9:1, illegible. So the fork must decide
   the PAIR (fill token + foreground token), not just the fill. Options:
   **(a)** all 9 → `--cx-accent-muted` (#220's precedent; solid fills go
   translucent); **(b)** borders → `--cx-border-focus` (solid `--rigel-blue`,
   `tokens.css:108`), solid fills → `--cx-accent` **with the foreground flipped
   to `var(--cx-bg)`** (dark-on-accent, the standard on-accent treatment);
   **(c)** all 9 → `--cx-accent` (same foreground problem); **(d)** compass-ux
   coins a dark `--cx-accent-strong` for fills, keeping white foregrounds legal.
   Recommendation: **(b)** — `--accent-dim` served two roles the DS separates;
   map each to its DS token and fix the on-accent foreground with it.

4. **PR-train topology** (LOAD-BEARING; shape of the whole execution). Matt
   ruled out COEXISTENCE (two live tiers), not STAGING. The train can be **one
   PR** carrying T1-T7, or a **jj-vine stack** (e.g. T1 / T2 / T3-T5 / T6 / T7)
   merged as a unit. They differ materially: one PR = a ~540-ref diff + a new
   harness + a new toolchain dep in a single review (screenshots carry the color
   review, but the harness and stylelint config get buried under mechanical
   churn); a stack = per-step screenshot pairs and independently-reviewable
   harness/lint steps. A stack whose steps merge INDEPENDENTLY is rejected — it
   would put a mixed-palette state on `main` (see the merge-atomicity
   constraint). Options: **(a)** one PR; **(b)** stacked train, merged
   atomically as a unit; **(c)** stack with independent merges (rejected).
   Recommendation: **(b)** — per-step screenshot review, reviewable harness/lint
   PRs, and no mixed-palette state ever on `main`; it is exactly the shape
   `skill://jj`'s stacked-PR workflow is built for, and fully inside Matt's
   full-cutover ruling.
