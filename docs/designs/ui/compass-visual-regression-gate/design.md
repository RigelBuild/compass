# Design: Compass automated visual-regression gate (RIG-2154)

Status: Draft
Owner lane: compass-ux (design) → compass-ux (execution)
Refs: RIG-2154
Governing spec: docs/designs/ui/compass-ui-fixture-boot/design.md (Decision D7)
Origin: fixture-boot D7 ruled that the visual-smoke harness graduates from a human before/after PNG-review tool to a real automated visual-regression gate, built on the deterministic fixture-boot substrate (RIG-2124, merged).

## Problem / Intent

The visual-smoke harness captures 11 PNGs of the core surfaces (7 full-page,
3 element close-ups, 1 clipped strip) but
asserts nothing — it is "a smoke harness: no pixel-diff gating, no
computed-style assertions" (`apps/ui/playwright.config.ts:7-8`), and CI never
runs it: the `ci` task deps are `['typecheck', 'build', 'test', 'stylelint',
'dev-smoke']` (`apps/ui/moon.yml:86`) and `dev-smoke` runs only
`bunx playwright test e2e/dev-boot.spec.ts` (`moon.yml:80`). Fixture-boot D7
ruled the follow-up: "make the harness a real automated visual-regression gate
(`toHaveScreenshot` + a `maxDiffPixelRatio` threshold + baselines committed to
git + generated in a pinned CI environment)"
(`compass-ui-fixture-boot/design.md:510-513`). This record designs that gate.

## Approach

### Oracle: Playwright built-in `toHaveScreenshot`, baselines in-repo (decided)

Built-in `expect(page).toHaveScreenshot()` with committed PNG baselines, not a
cloud service (Percy/Chromatic). This is decided here, not an open question:

- The whole substrate was built for it. Fixture-boot's determinism knobs
  (animations disabled, css-scaled raster, `deviceScaleFactor: 1`,
  `reducedMotion: "reduce"`, `document.fonts.ready` awaited per shot —
  `playwright.config.ts:60-62`, `visual-smoke.spec.ts:24,28-29`) exist
  precisely so a raster comparison is stable; a DOM-serialization service
  makes them redundant while adding an external SaaS dependency, secrets, and
  cost to a harness that is offline by construction ("no daemon on :50051 and
  no `VITE_COMPASS_BASE_URL` — offline by construction",
  `visual-smoke.spec.ts:6-8`).
- The rendering environment is already pinned end to end:
  `chromium-e2e-env.nix` "pins nixpkgs to the SAME devenv.lock revision the
  dev shell and gate-tools.nix resolve, so CI drives byte-for-byte the
  Chromium a Linux dev box does" (`chromium-e2e-env.nix:19-20`), and the moon
  CI leg already exports `PLAYWRIGHT_CHROMIUM_PATH` from it
  (`.github/workflows/ci.yml:365-367`). The cross-environment raster drift
  that motivates cloud services is exactly what this pin removes.
- The API is available at the pin: `@playwright/test` is `1.62.1`
  (`apps/ui/package.json:34`); `toHaveScreenshot`, `maxDiffPixels`,
  `maxDiffPixelRatio`, `expect.toHaveScreenshot` config defaults, and
  `snapshotPathTemplate` (since v1.28) are all long-stable in that line
  (verified against upstream Playwright docs this run).

A cloud service stays available as a later escalation if raster maintenance
cost proves high; nothing in this design forecloses it.

Repo weight is not a concern at snapshot level — the 11 committed baselines
total ~575 KB (largest `bridge-prs.png` ~95 KB, smallest `state-dot.png`
~150 B) — but each regen rewrites all 11, so git *history* grows ~0.5 MB per
baseline-churn event; under active UI development that is plausibly tens of
MB/year of permanent history. Acceptable, and Git LFS for `e2e/__screens__/`
was weighed and rejected: it complicates the nix CI checkout and breaks the
in-diff-view image review the bot-PR baseline workflow (T4) depends on.

### Shape: convert the generator spec in place

`visual-smoke.spec.ts` becomes the gate spec: each capture becomes a
`toHaveScreenshot` assertion of the **same raster options it captures today**,
now asserted rather than written. The 11 captures are not uniform — the spec
takes three shapes, and each converts to its matching `toHaveScreenshot` form:

- **7 full-page** `page.screenshot({ fullPage: true, … })` (bridge,
  bridge-empty, agent, backlog, done, settings, bridge-prs) →
  `await expect(page).toHaveScreenshot("<name>.png", { fullPage: true,
  animations: "disabled", scale: "css" })`.
- **3 element** `locator.screenshot(…)` (right-sidebar on `aside.right`
  `visual-smoke.spec.ts:69`, state-dot on `.cx-state-dot` `:140`, bridge-card
  on `.cx-card` `:204`) → `await expect(locator).toHaveScreenshot("<name>.png",
  { animations: "disabled", scale: "css" })` — no `fullPage`; the locator
  bounds the raster.
- **1 clip** `page.screenshot({ clip: {…} })` (bridge-colheads, a computed
  union rect over `.bridge-col-head` cells, `:180-191`) →
  `await expect(page).toHaveScreenshot("bridge-colheads.png", { clip,
  animations: "disabled", scale: "css" })`, keeping the bounding-box
  computation untouched.

Converting all 11 to `expect(page).toHaveScreenshot({ fullPage: true })` — as
an earlier draft of this record did — would compare full pages against the four
element/clip-sized committed baselines: a guaranteed day-one red, or worse a
regen that silently erases the close-up coverage D7 asked for. No parallel
spec: two specs capturing the same surfaces drift, and the human before/after
review workflow survives unchanged because a passing run leaves the committed
baselines as the review artifact and a failing run produces `-actual`/`-diff`
PNGs.

`snapshotPathTemplate` is set so baselines stay at their current names:
Playwright's default template appends platform/project suffixes
(`bridge-chromium-linux.png`); a template of
`{testDir}/__screens__/{arg}{ext}` keeps the existing 11 files
(`apps/ui/e2e/__screens__/`: bridge.png, bridge-empty.png, bridge-card.png,
bridge-colheads.png, bridge-prs.png, settings.png, done.png, backlog.png,
agent.png, right-sidebar.png, state-dot.png) as the baselines with no rename.
The suffix-free template is safe because the config defines a single
`chromium` project (`playwright.config.ts:73-78`) and the gate only ever runs
on Linux against the pinned Chromium (Global Constraints); a second
project/OS would need the template revisited.

### Threshold

`maxDiffPixelRatio: 0.001` (0.1%) as the config-level default via
`expect: { toHaveScreenshot: { … } }`, per-shot overrides allowed. Zero
tolerance is wrong even on a pinned stack — fixture-boot's byte-identity bar
is explicitly a "same-binary, same-box determinism self-test … not a
cross-environment regression oracle"
(`compass-ui-fixture-boot/design.md:417-421`), and the gate must survive
nix-channel Chromium bumps without a fleet-wide red on every anti-aliasing
shift. 0.1% of a full-page shot is small enough to catch any real layout or
palette change while absorbing sub-pixel raster noise. Tightening later is a
one-line PR once the gate has run history.

`maxDiffPixelRatio` is a fraction of **total image area**, and this suite
spans ~4 orders of magnitude: a full-page shot (~1280×720+, ≥900 K px) at
0.001 allows ~900 differing pixels, while `state-dot.png` (a few hundred px)
gets a budget that rounds to ~0 — effectively byte-exact, the *least* slack on
the shot most exposed to a single anti-aliasing pixel shift after a Chromium
bump. One ratio cannot serve both ends. So the 4 close-ups (right-sidebar,
state-dot, bridge-card, bridge-colheads) carry per-shot `maxDiffPixels`
overrides (start 10–25 px, each with the justifying comment Global Constraints
require) as an absolute floor, while the 7 full-page shots keep the 0.001
ratio. The per-pixel color tolerance `threshold` (YIQ distance, Playwright
default 0.2) is left at its default **as an explicit decision** — it, not the
pixel-count knobs, is what absorbs anti-aliasing colour drift; a Chromium bump
revisits it deliberately.

### Where it runs: a moon task inside the existing moon battery

A new `visual-gate` moon task, added to the `ci` task's deps — not a
dedicated peer job behind the rollup. The peer-job pattern (gtk4-e2e,
dogfood-e2e) exists for legs that "realize a heavy out-of-band … closure the
bare moon gate has no business building" (`ci.yml:1124-1126`). This gate has
no such closure: the moon leg already realizes the pinned Chromium and
exports `PLAYWRIGHT_CHROMIUM_PATH` for `dev-smoke` (`ci.yml:356-367`), and
the harness's webServer is the same `vite --mode fixture` boot dev-smoke's
config already drives (`playwright.config.ts:80`). A peer job would
re-bootstrap nix + toolchain for ~a minute of Playwright. The task mirrors
`dev-smoke`'s two documented disciplines (`moon.yml:64-84`): explicit
`inputs` (dev-smoke's list plus the gate spec and `e2e/__screens__/**`) so
affected-detection schedules it, and `cache: false` because the subject is
the rendered raster resolved at run time, not a moon-hashable input.

### Baselines: generated in CI's pinned environment, updated by a dispatch lane

The load-bearing rule: **baselines are regenerated only in the pinned CI
environment, never committed from a dev box.** The repo already has the exact
machinery pattern: the `regen-forge-fixtures` workflow_dispatch lane runs an
operator-triggered `-update` capture and "opens a BOT PR carrying the
rewritten fixtures for human review" (`ci.yml:2263-2267`), SHA-pinned
`peter-evans/create-pull-request` included (`ci.yml:2375`). The visual gate
gets a sibling lane: dispatch → bootstrap the same toolchain + pinned
Chromium → `bunx playwright test e2e/visual-smoke.spec.ts
--update-snapshots` → bot PR with `add-paths: apps/ui/e2e/__screens__`. Matt
reviews the baseline diff as images in the PR — which is also the review
surface for intentional visual changes: land the code PR with the gate red or
with regenerated baselines from the dispatch lane, per the OQ-2 fork below.

### Failure surfacing

On failure Playwright writes `<name>-actual.png`, `<name>-expected.png`, and
`<name>-diff.png` under `outputDir` (`e2e/.output`,
`playwright.config.ts:54`). The moon-battery job gets an
`if: failure()` `actions/upload-artifact` step (SHA-pinned, per the house
rule every action in `ci.yml` follows) scoped to `apps/ui/e2e/.output/**`, so
a red gate always carries a downloadable actual/expected/diff triplet.
Inline-in-PR diff images are OQ-3.

### Rollout: hard gate from the first landing

No advisory period. The determinism substrate is proven (fixture-boot T4's
byte-identity self-test), the environment is pinned byte-for-byte, the first
baselines are CI-generated in that same environment, and the 0.1% ratio
absorbs residual noise. An advisory mode needs real machinery (a
`continue-on-error` leg outside the moon battery, plus somewhere to look) and
history shows advisory gates go unread. The rollback lever if it flakes:
bump `maxDiffPixelRatio` or drop a noisy shot from the gate — each a
one-line, same-day PR. Presented as OQ-4 since the issue asks, with this as
the recommendation.

### Coverage at v1: all 11 shots

All 11 existing surfaces gate from day one. The set already exists as
committed, determinism-hardened baselines; curating a subset means deciding
per-surface noise levels with zero run history, and the fallback (drop a shot
that proves noisy, one-line PR) is cheaper than guessing up front. Presented
as OQ-5 with this recommendation since the issue asks.

## Global Constraints

- **Determinism knobs are frozen and must match the substrate exactly**:
  `screenshot: "off"`, `reducedMotion: "reduce"`, `deviceScaleFactor: 1`
  (`playwright.config.ts:60-62`); per-shot `animations: "disabled"`,
  `scale: "css"` on every capture, plus `fullPage: true` on the 7 full-page
  shots only (the 3 element and 1 clip captures are bounded by their locator /
  clip rect, not `fullPage` — `visual-smoke.spec.ts:25-30,69,140,180-191,204`);
  `document.fonts.ready` awaited before every capture
  (`visual-smoke.spec.ts:24`); content-selector waits, never fixed sleeps
  (`visual-smoke.spec.ts:8-9`). No task may loosen any of these.
- **Pinned Chromium only**: the gate runs against the Chromium realized from
  `tools/toolchain/chromium-e2e-env.nix` (devenv.lock-pinned nixpkgs,
  `chromium-e2e-env.nix:19-20,41`), resolved via `PLAYWRIGHT_CHROMIUM_PATH`
  (`playwright.config.ts:68-70`, `ci.yml:365-367`). Single `chromium`
  project, Linux only.
- **Baselines from CI only**: `apps/ui/e2e/__screens__/` PNGs are written
  only by the regen dispatch lane (T4) running in the pinned environment.
  A locally generated baseline is a review-rejection offense — a dev-box
  Chromium raster differs and would bake local noise into the oracle
  (`compass-ui-fixture-boot/design.md:419-421`).
- **API floor**: `@playwright/test 1.62.1` (`apps/ui/package.json:34`); no
  version bump inside this record. Every API used (`toHaveScreenshot`,
  `maxDiffPixelRatio`, `expect.toHaveScreenshot` defaults,
  `snapshotPathTemplate`, `--update-snapshots`) is stable at that pin.
- **Moon task discipline**: the gate task carries explicit `inputs` and
  `cache: false`, mirroring `dev-smoke`'s documented rationale
  (`moon.yml:64-84`). CI actions are SHA-pinned like every action in
  `ci.yml`.
- **Threshold default**: `maxDiffPixelRatio: 0.001` set once in
  `playwright.config.ts` `expect.toHaveScreenshot`; the 4 close-up shots
  additionally carry per-shot `maxDiffPixels` floors (10–25 px). Per-pixel
  `threshold` stays at the Playwright default (0.2). Every per-shot override
  carries a comment justifying it.
- House ledger conventions: this record stays `Status: Draft` until merged;
  markdownlint-clean.

## Plan

### T1 — Gate config: `toHaveScreenshot` defaults + snapshot path

Extend `apps/ui/playwright.config.ts` with:
`snapshotPathTemplate: "{testDir}/__screens__/{arg}{ext}"` and
`expect: { toHaveScreenshot: { maxDiffPixelRatio: 0.001 } }` — the config-level
full-page default; the per-shot `maxDiffPixels` floors for the 4 close-ups are
set at their call sites in T2, not here. `threshold` is left unset (default
0.2) as a recorded decision. No project or webServer changes — the determinism
knobs at :57-72 and the fixture-mode webServer at :79-95 are already the
substrate.

Interfaces:

- Modifies: `apps/ui/playwright.config.ts` (top-level `snapshotPathTemplate`,
  `expect` keys on the `defineConfig` object).
- Consumes: existing `e2e/__screens__/` layout (11 PNG names).
- Test cycle: `bunx playwright test e2e/visual-smoke.spec.ts` after T2 lands
  resolves baselines at the unchanged paths (T1+T2 land as one PR — T1 alone
  changes nothing observable because no spec asserts yet).

### T2 — Convert the generator spec to assertions

In `apps/ui/e2e/visual-smoke.spec.ts`, convert each capture to its matching
`toHaveScreenshot` form (the 7/3/1 split from the Shape section), preserving
its exact current raster options:

- **7 full-page** (bridge, bridge-empty, agent, backlog, done, settings,
  bridge-prs): `page.screenshot({ path, fullPage: true, animations, scale })`
  → `await expect(page).toHaveScreenshot("<name>.png", { fullPage: true,
  animations: "disabled", scale: "css" })`.
- **3 element** (right-sidebar `:69`, state-dot `:140`, bridge-card `:204`):
  `<locator>.screenshot({ path, animations, scale })` →
  `await expect(<locator>).toHaveScreenshot("<name>.png", { animations:
  "disabled", scale: "css", maxDiffPixels: <floor> })` on the same locator —
  no `fullPage`.
- **1 clip** (bridge-colheads `:189`): keep the bounding-box union computation
  (`:180-188`), then `await expect(page).toHaveScreenshot("bridge-colheads.png",
  { clip, animations: "disabled", scale: "css", maxDiffPixels: <floor> })`.

The 4 close-ups carry the per-shot `maxDiffPixels` floor (T1's rationale) with
a justifying comment. Keep every navigation, selector wait, and
`document.fonts.ready` await untouched. Drop the now-unused `SCREENS` const;
import `expect` alongside `test` from `@playwright/test`
(`visual-smoke.spec.ts:1` currently imports only `test`). Update the spec
header comment: it is a gate, not a review-only generator.

Interfaces:

- Modifies: `apps/ui/e2e/visual-smoke.spec.ts` (11 capture blocks, imports,
  header comment).
- Consumes: T1's config keys; existing baselines as the initial oracle
  (superseded by T4's CI regen before the gate wires into CI — see T5
  ordering).
- Produces: a spec that exits non-zero on visual drift, writing
  `-actual`/`-expected`/`-diff` PNGs under `e2e/.output` on failure.
- Test cycle: local run passes against freshly `--update-snapshots`-generated
  local baselines (NOT committed); a deliberate CSS perturbation reds the
  matching shot; revert restores green.

### T3 — Moon task + battery artifact upload

Add to `apps/ui/moon.yml` a `visual-gate` task:
`command: 'bunx playwright test e2e/visual-smoke.spec.ts'`,
`deps: ['install']`, `options: { cache: false }`, and explicit `inputs` =
`dev-smoke`'s list (`moon.yml:82`) with `e2e/dev-boot.spec.ts` swapped for
`e2e/visual-smoke.spec.ts` plus `e2e/__screens__/**/*` and `src/**/*.css`
(already covered by `src/**/*`). Add `'visual-gate'` to the `ci` deps list
(`moon.yml:86`). In `.github/workflows/ci.yml`, add to the moon-battery job
an `if: failure()` SHA-pinned `actions/upload-artifact` step uploading
`apps/ui/e2e/.output/**` (name: `visual-gate-diffs`, short retention) with
`if-no-files-found: ignore` — the step fires on *any* bun-leg failure (a red
typecheck, not just a visual diff), and without that knob a no-diff failure
emits a spurious missing-artifact warning. A red gate still always ships the
diff triplet.

Interfaces:

- Modifies: `apps/ui/moon.yml` (new task + `ci` deps), `.github/workflows/ci.yml`
  (one upload step in the moon-battery job).
- Consumes: `PLAYWRIGHT_CHROMIUM_PATH` already exported in that job
  (`ci.yml:365-367`).
- Test cycle: a scratch PR with a deliberate visual change reds `ci` via
  `visual-gate` and carries the `visual-gate-diffs` artifact; a no-op PR
  stays green. Verify affected-detection schedules the task on a
  baseline-only change.

### T4 — Baseline regen dispatch lane

Add a `regen-visual-baselines` workflow_dispatch job to
`.github/workflows/ci.yml`, modeled on `regen-forge-fixtures`
(`ci.yml:2260-2385`) but with two corrections the sibling-of-forge framing
hides:

- **Discriminator input (must-fix):** `regen-forge-fixtures` gates on
  `workflow_dispatch && inputs.pr == ''` (`ci.yml:2272-2274`). A second lane
  with the *same* gate means every bare `ci.yml` dispatch fires BOTH — a
  visual regen would also launch the 90-minute live forge capture and open a
  spurious forge bot PR. Add a `regen` choice dispatch input (`forge` |
  `visual`, **default `forge`**) and extend each lane's `if:` with
  `&& inputs.regen == '<own>'`. Default `forge` preserves the existing
  bare-dispatch behavior of the forge lane; the visual lane fires only on an
  explicit `regen: visual`.
- **JS install (must-fix):** `regen-forge-fixtures`' payload is `go test` and
  installs no JS deps; this lane's payload `bunx playwright test` needs
  `apps/ui` node_modules (`@playwright/test`, vite), so it adds a
  `bun install` / `moon :install` step the "same two-phase bootstrap" phrase
  does not cover.

Otherwise as forge: widened `contents: write` + `pull-requests: write`, the
two-phase toolchain bootstrap (`ci.yml:2304-2328`) plus the pinned-Chromium
realization step (`ci.yml:365-367`'s pattern), then
`bunx playwright test e2e/visual-smoke.spec.ts --update-snapshots` under
`apps/ui`, then SHA-pinned `peter-evans/create-pull-request` with
`add-paths: apps/ui/e2e/__screens__`. No secrets needed (offline fixture
boot).

Interfaces:

- Modifies: `.github/workflows/ci.yml` (one new job + a `regen` dispatch input;
  extends `regen-forge-fixtures`' `if:` with `&& inputs.regen == 'forge'` —
  the only edit this record makes to an existing lane; still does not join the
  rollup's `needs`, same as regen-forge-fixtures per `ci.yml:2271`).
- Produces: a bot PR carrying the regenerated 11 baselines for Matt's image
  review.
- Test cycle: dispatch the lane on a branch; verify the bot PR opens with
  only `__screens__` changes and the images render in the PR diff view.

### T5 — First CI-generated baselines + cutover ordering

Sequencing task, not a code task. Order: land T1+T2+T4 with the gate NOT yet
in `ci` deps (T3's moon.yml edit split out); dispatch T4's lane to produce
the first pinned-environment baselines; merge that bot PR (replacing the 11
dev-box PNGs currently committed); **burn in before flipping the gate** —
re-dispatch T4's lane 5–10 times and diff the resulting bot-PR baselines
against each other: on a byte-for-byte pinned Chromium they should be
identical, and this converts the "should be deterministic" claim into
measured cross-run data at zero extra machinery (the only cross-run evidence
today is same-box, `compass-ui-fixture-boot/design.md:417-421`); then land T3
wiring the gate into `ci`. This guarantees the gate never runs against
dev-box baselines — the first red would otherwise be a false environment-skew
red on an unrelated PR. **Race window:** between merging the baseline bot PR
and landing T3, a visually-material UI PR could merge and make T3's first run
red on main — land T3 immediately after the baseline merge, and re-dispatch
T4 if any UI-touching PR slipped in between. During that window a local
`bunx playwright test e2e/visual-smoke.spec.ts` reds against the stale
dev-box baselines (cosmetic — do not "fix" it). Also update the fixture-boot
record's D7 cross-reference and the harness docs/comments that describe it as
review-only (`playwright.config.ts:4-8` header).

Interfaces:

- Modifies: PR sequencing only, plus `playwright.config.ts:4-8` comment and
  a one-line D7 follow-up note in
  `docs/designs/ui/compass-ui-fixture-boot/design.md` (Status footnote, not a
  content change).
- Test cycle: the 5–10-run burn-in shows identical baselines; after cutover,
  two consecutive CI runs on main are green; a deliberate-perturbation scratch
  PR reds.

## Tasks

- [ ] T1 — `snapshotPathTemplate` + `expect.toHaveScreenshot` defaults in
      `playwright.config.ts` (lands with T2)
- [ ] T2 — convert `visual-smoke.spec.ts` captures to `toHaveScreenshot`
      assertions
- [ ] T3 — `visual-gate` moon task + `ci` dep + failure-artifact upload
      (lands LAST, after T5's baseline cutover)
- [ ] T4 — `regen-visual-baselines` workflow_dispatch lane → bot PR
- [ ] T5 — dispatch T4, merge first CI-generated baselines, then land T3;
      update harness comments + fixture-boot D7 cross-ref

## Open Questions

Load-bearing (need Matt's ruling before the impl issues file):

1. **OQ-1 — Threshold start value.** `maxDiffPixelRatio` at 0.001 (0.1%, the
   issue's suggestion — absorbs anti-aliasing noise, catches layout/palette
   changes), vs 0.0005 (tighter; more sensitive to Chromium-bump raster
   drift), vs 0.002 (looser; risks missing a small real regression like a
   1px border change on a large full-page shot). Note the ratio is
   area-scaled, so the 4 close-up shots additionally take per-shot
   `maxDiffPixels` floors (10–25 px) rather than the ratio, and per-pixel
   `threshold` stays at the default 0.2 (see Threshold section).
   **Recommendation: 0.001** for the full-page default + the close-up floors,
   revisit with run history.
2. **OQ-2 — Intentional-visual-change workflow.** When a PR intentionally
   changes a surface:
   (a) author lands the PR with the gate red, then dispatches the regen lane
   and merges the bot PR (gate red on main briefly);
   (b) author dispatches the regen lane **on the feature branch** (the native
   workflow_dispatch run-from-branch selector) — `peter-evans/create-pull-request`
   defaults its `base` to the checked-out branch, so the bot PR targets that
   branch with the regenerated baselines and no extra input is needed (the
   earlier "`ref` dispatch input" idea was unnecessary machinery); the feature
   PR then lands green with its own baselines;
   (c) allow a documented local `--update-snapshots` + commit, breaking the
   baselines-from-CI-only rule;
   (d) the lane pushes the regenerated baselines as a direct commit to the PR
   branch (`contents: write`, no bot PR) — fewer steps, but removes the human
   in-diff image review the bot PR gives;
   (e) author iterates locally with `--update-snapshots`, then a required CI
   regen replaces those files before merge (merge-queue-style).
   **Two under-weighted costs on (b):** (i) per intentional change the author
   pays dispatch → full toolchain+Playwright bootstrap → merge bot PR into own
   branch → re-run gate — a multi-step, multi-minute loop on what may be the
   *most common* change shape in an actively-developed UI, not the exception;
   (ii) **jj hazard** — merging a bot PR into a jj-managed feature branch puts
   a commit on the GitHub bookmark the local jj working copy lacks, and the
   mandated `sync-before-submit` rebase (or any bookmark rewrite) before the
   next `jj-vine submit` can silently drop the bot's baseline commit,
   resurrecting the red gate with no obvious cause. Any CI-writes-to-your-branch
   scheme (b/d) collides with jj bookmark rewriting.
   **Recommendation: (b)** — keeps main always green and the CI-only rule
   intact — but Matt should rule with the jj collision and the per-PR loop
   cost on the table.
3. **OQ-3 — Diff visibility for adjudication.** (a) CI artifact zip only
   (T3's design — Matt downloads the actual/expected/diff triplet); (b)
   additionally a bot PR-comment embedding the diff images (needs an image
   host or committing diffs to a scratch branch — more machinery, images
   inline); (c) a Playwright HTML report artifact instead of raw PNGs
   (single browsable file, still a download). **Recommendation: (a)** at
   v1 — zero new machinery; escalate to (b) only if the download step
   proves to be real friction. Deferrable: the artifact upload lands
   either way.
4. **OQ-4 — Hard gate vs advisory start.** (a) hard gate from first landing
   (Approach's case: pinned env + CI-generated baselines + 0.1% ratio leave
   little to stabilize, and the rollback is a one-line threshold bump); (b)
   a 2-week advisory period (`continue-on-error` leg) collecting flake data
   first. **Recommendation: (a)**, with the T5 ordering as the safety
   mechanism.
5. **OQ-5 — v1 coverage.** (a) all 11 shots (Approach's case: baselines
   exist, dropping a noisy shot later is one line); (b) a curated core
   subset (bridge, settings, agent, state-dot) to minimize initial noise
   surface. **Recommendation: (a)**.

Non-load-bearing / deferrable: OQ-3's escalation path; whether the regen
lane later folds into a label-triggered automation (out of scope here).
