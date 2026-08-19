# Compass UI dev-boot smoke gate (RIG-1536)

Status: Draft

## Problem / Intent

`vite dev` can serve a completely broken app — blank page, `render()` never
runs — while every gated task in the repository stays green. RIG-1535 was
exactly that on `main`, and it surfaced only because a human drove a real
browser. This record adds a `dev-smoke` gate so a dev-boot regression reds CI
instead of waiting for a human.

Why the current gate cannot see it, each cause verified on today's `main`
(`ba12d920`):

- **No task exercises the serving path.** `apps/ui/moon.yml:59-60` wires the
  gate as

  ```yaml
  ci:
    deps: ['typecheck', 'build', 'test', 'stylelint']
  ```

  The `dev` task (`moon.yml:11-14`, `command: 'bunx vite'`) is in no `ci`
  chain — correctly, it's a server preset — so nothing that CI runs ever
  instantiates the dev server.
- **`vite build` is a different code path from `vite dev`.** The RIG-1535
  defect class lives in dev-serving only: `vite-plugin-solid` prepends the
  `development` export condition while serving, and Vite itself re-supplies it
  (`vite.config.ts:32-35`: "Vite re-supplies the condition itself
  (`DEV_PROD_CONDITION` in its default client conditions, expanded to
  `development` outside a production build)"). A production `vite build` never
  applies it, so `build` is green over a dev-broken config by construction.
- **The unit suite never instantiates Vite's dev server.** `moon.yml:43` runs
  `bun test --conditions browser src/` under happy-dom; the one suite that
  touches Vite at all (`src/preview-build.test.ts:1-2`, the previewable-build
  gate) runs a real `vite build` — again the build path, not serving.
- **Cache-correctness is necessary but not sufficient.** `main` has already
  repaired the caching half of RIG-1536's analysis: `vite.config.ts` IS in
  `test`'s inputs today (`moon.yml:50`), with the comment at `moon.yml:47-49`
  ("Without these a vite.config-only change would leave `test` unaffected and
  serve the gate a cached-green"). So `test` now re-runs on a config edit — and
  still passes over the dev-broken config, exactly as RIG-1536 measured
  ("re-ran, hash moved … still PASSES"). A freshly-computed green over the same
  defect. The only real repair is an instrument that reads the served module
  graph.

Compounding: the RIG-1535 fix itself can rot silently. `vite.config.ts:44-49`
pins two nested dep paths:

```ts
optimizeDeps: {
    include: [
        "solid-markdown > remark-parse > mdast-util-from-markdown > micromark > debug",
        "solid-markdown > remark-parse > unified > extend",
    ],
},
```

and the comment at `vite.config.ts:40-43` names the hazard: "Vite silently
skips an unresolvable INTERMEDIATE segment (it keeps the previous basedir), so
if these paths ever rot there is no warning, just the blank page again." A
dependency bump can re-break dev with zero diff to any file a gate hashes.

Issue: RIG-1536. Two of its claims are stale against today's `main` and are
corrected here: (1) "no CI exists on `main` today" — `.github/workflows/ci.yml`
exists and runs `moon ci :ci` on PRs (`ci.yml:230-231`) and `moon run :ci` on
main pushes + nightly (`ci.yml:240-241`); (2) "`vite.config.ts` is absent from
`test`'s inputs" — it is present now (`moon.yml:50`, see above).

## Approach

A headless-boot smoke (RIG-1536's Shape B) as a Playwright spec, wired as a
`dev-smoke` moon task into `compass-ui:ci` with `cache: false`.

- **The assertion**: boot the real dev server, load the page, and require BOTH
  (a) the app's mount selector `.bridge` becomes visible, and (b) zero
  `pageerror` events over the whole load. Both clauses are load-bearing: the
  `.bridge` wait alone misses a mount that renders but then throws
  asynchronously (a deferred chunk explodes after mount); `pageerror` alone
  misses a mount that silently no-ops (renders nothing, throws nothing). The
  `.bridge` wait is what makes clause (a) sound where a bare `#root`-non-empty
  check would not: `renderBootError` paints the caught-error screen INTO
  `#root` — the fixture-boot catch routes through it (`index.tsx:52-58`), and
  `boot.ts` documents the contract ("this module catches at that boundary and
  paints the message into #root") — so `#root`-non-empty false-greens on a
  caught boot failure, while `.bridge` visibility never does. A `#root`-non-
  empty check is therefore redundant given the `.bridge` wait, and is kept (if
  at all) only as a cheap secondary guard, never a co-equal clause.
- **The server**: the existing fixture-boot dev server, exactly as the visual
  harness runs it — `playwright.config.ts`, on an OS-assigned ephemeral port
  (RIG-2283) so it never collides with a dev server already on the box:

  ```ts
  command: `bunx vite --port ${devPort} --strictPort --mode fixture`,
  ```

  Fixture mode is still a dev SERVE (the `development` condition applies — the
  defect reproduces) and boots fully offline on the in-memory stub store
  (`e2e/visual-smoke.spec.ts:5-8`: "the app boots fully on the in-memory
  fixture store (stub-data.ts) with no daemon on :50051 and no
  VITE_COMPASS_BASE_URL — offline by construction"). No daemon, no env
  plumbing, deterministic in CI.
  Fixture mode cannot mask the defect class, structurally: the defect kills
  module-graph EVALUATION, which under `vite dev` happens for the entire
  static import chain before any runtime branch executes — `index.tsx:9`
  statically imports the mount module (`import { mountShell,
  newAppQueryClient } from "./mount"`), `mount.tsx:17,19` statically imports
  `App` and `AppRoutes`, and `routes.tsx:14-20` eagerly imports every route
  component (no `lazy()` anywhere in the table). The fixture/live fork
  (`index.tsx:49-51`) is a runtime branch over an already-loaded graph, so a
  broken chain reds before it even executes; dev-serving applies the
  `development` condition identically in every mode.
- **The tooling**: the repo's existing Playwright harness. `@playwright/test`
  `1.62.1` is already a devDependency (`package.json:31`), `playwright.config.ts`
  already exists with the webServer + nix-chromium wiring, and
  `e2e/visual-smoke.spec.ts` is a working spec against it. Adopting the issue's
  `puppeteer-core` proposal now would plant a second browser-automation
  convention beside a working first one.
- **The wiring**: a `dev-smoke` task in `apps/ui/moon.yml` with explicit
  `inputs` AND `cache: false`, added to `ci.deps`. The two settings compose,
  each doing a different job. `inputs` drive PR-path affected-detection: a
  project task with no `inputs` key defaults to project-relative `**/*`,
  which excludes the workspace-root `/bun.lock` — every sibling task that
  needs the lockfile lists it explicitly (`'/bun.lock'` in
  `typecheck`/`build`/`test`, `moon.yml:18,22,50`) — so `dev-smoke` mirrors
  `build`'s list (`moon.yml:22`) plus its own two files
  (`'e2e/dev-boot.spec.ts'`, `'playwright.config.ts'`), and a lockfile-only
  dependency-bump PR is detected directly, with no reliance on any
  propagation mechanism. `cache: false` is still not optional: the task's
  subject is the served module graph — resolved at run time out of
  `node_modules` — not an input moon can hash, so whenever the task IS
  scheduled it must really run; a cached green would survive exactly the
  change (a dependency bump rotting the `optimizeDeps` pins) it exists to
  red on. Precedent in the same file: `ci` itself is `cache: false`
  (`moon.yml:61-62`).
- **CI browser provisioning**: CI has no Chromium today — `grep` of
  `.github/workflows/ci.yml`, `devenv.nix`, and `tools/toolchain/` finds no
  playwright/chromium/puppeteer reference, and `playwright.config.ts:31-33`
  falls back to a dev-box path (`/etc/profiles/per-user/mattw/bin/chromium`)
  overridable via `PLAYWRIGHT_CHROMIUM_PATH`. Chromium enters CI the same way
  every other tool does: added to `devenv.nix`'s `packages` list, which
  `parity.ts --print-nix-attrs` parses and `gate-tools.nix` resolves onto the
  CI PATH (`gate-tools.nix:5-8`: "It is never hand-listed here: adding a tool
  to the dev shell must extend CI and the gate with no edit to this file or to
  the workflow"). The workflow then exports `PLAYWRIGHT_CHROMIUM_PATH` from the
  resolved store path.

**Red→green acceptance** (the gate is only real if it has been seen to fail):
with the `optimizeDeps.include` block of `vite.config.ts:44-49` temporarily
removed — the `main`-era dev-broken configuration RIG-1535 fixed — `moon run
compass-ui:dev-smoke` MUST fail; with the block restored it MUST pass. Both
runs recorded in the implementation PR body. This is the record's rendering of
RIG-1536's two-clause general check: "1. can editing the artifact invalidate
the gate? 2. if you make it invalidate, does the gate then FAIL?"

Precedent for this shape of hygiene gate: `src/design-citations.test.ts:12-13`
("this test gives the sweep teeth so it can't regress") — a manual sweep that
got a permanent instrument. Same move here: the manual "drive a browser at the
dev server" check becomes a task.

## Alternatives considered

### Guard shape

- **Shape B — headless boot (chosen).** Boots the real server, loads the real
  page, asserts the app mounted with zero page errors. Catches the WHOLE
  defect class "dev serves but the app doesn't boot" — CJS-interop holes,
  a future plugin misconfiguration, a top-level throw in the module graph, a
  broken index.html — not just the one failure mode we've seen. Cost: needs a
  browser in CI (~seconds of boot + load per run, plus a one-time Chromium
  provisioning task) and carries browser-harness flake risk, mitigated by the
  existing harness's determinism choices (`playwright.config.ts`:
  ephemeral port, `strictPort`, `reuseExistingServer: false`, generous
  webServer timeout).
- **Shape A — module-graph crawl.** RIG-1536 validated it: ~1.8s, no browser,
  crawl the served graph from `/src/index.tsx`, fail on any raw-served `/@fs`
  dependency default-importing into a CJS-only package with no
  `__vite__cjsImport` shim; proven red-on-main / green-on-fix over 308 modules.
  Loses on breadth and maintenance: it detects exactly the CJS-interop class
  and nothing else, and it is a bespoke crawler encoding Vite serving
  internals (`/@fs` URLs, interop shim markers) that Vite is free to change —
  a maintenance liability with no other consumer. Kept as the named fallback
  if the CI browser cost is vetoed (Open Question 3).
- **Both.** Maximum signal, but B subsumes A's verdict TODAY: every CJS leaf
  (`debug`, `extend`) sits in the STATIC boot graph — `routes.tsx:17` eagerly
  imports `ChannelView`, `ChannelView.tsx:30` imports `MarkdownText`,
  `MarkdownText.tsx:18` imports `solid-markdown`; shiki is ESM — so a graph
  that fails the crawl produces a blank page that fails the boot. That is a
  present-day fact, not a structural guarantee: a crawl of the served graph
  can follow DYNAMIC imports, while B executes only the chunks the boot path
  loads. The named residual: a CJS-interop rot inside a lazily-loaded chain —
  the ~15 dynamic `import()`s in `apps/ui/src/markdown/highlighter.ts:38-56`,
  which load only on the first code block highlighted, never on the bridge
  board — would pass B's boot while failing A's crawl; a future `lazy()`
  route or CJS-bearing dynamic import likewise exits B's coverage silently.
  Beyond that, A's only unique value is a faster, more pointed failure
  message. Two instruments for one contract is maintenance without coverage;
  the residual is accepted and named here.

### Automation tooling

- **Playwright reuse (chosen).** Already the repo's browser convention:
  `@playwright/test` pinned at `1.62.1` (`package.json:31`), a working config
  with the exact webServer this gate needs, a `test:visual` script
  (`package.json:8`). Zero new dependencies; the gate is one spec file plus a
  moon task.
- **`puppeteer-core` (the issue's proposal, rejected).** RIG-1536 predates the
  repo's Playwright adoption (SEA-2034 T1); its claim "Chromium and
  `puppeteer-core` are already in the environment" no longer describes the
  repo — `puppeteer-core` is not in `package.json` (its devDependencies are
  fully listed at `package.json:27-43`; no puppeteer entry), and
  `PUPPETEER_EXECUTABLE_PATH` is a dev-box env var, not CI state. Adopting it
  would add a dependency to stand up a SECOND browser-automation convention
  beside a working first one — forbidden by the repo convention rule.

### Do nothing / inputs-only repair

Rejected with measurement, by RIG-1536 itself: adding `vite.config.ts` to
`test`'s inputs (already done on `main`, `moon.yml:50`) converts a stable-hash
green into a freshly-computed green over the same defect — "which is worse
than leaving it", because a moved hash reads as "the config was checked". For
a task whose runner never reads the served graph, the input list is the wrong
lever.

## Global Constraints

- **Tooling**: `@playwright/test` at the repo pin `1.62.1`
  (`package.json:31`) — no version bump in this work, no `puppeteer-core`, no
  second browser-automation convention.
- **Browser**: Chromium via `launchOptions.executablePath`
  (`playwright.config.ts:31-33`), env-overridable through
  `PLAYWRIGHT_CHROMIUM_PATH`; CI supplies a nix-resolved Chromium through the
  `devenv.nix` `packages` → `gate-tools.nix` pipeline (never a `setup-*`
  action, never Playwright's own unpatched-for-NixOS download —
  `playwright.config.ts:8-14`).
- **`dev-smoke` carries explicit `inputs` AND `cache: false`**, both
  mandatory and composing (see T2): `inputs` drive PR-path
  affected-detection (a no-`inputs` project task defaults to
  project-relative `**/*`, which excludes the workspace-root `/bun.lock`);
  `cache: false` forces a real run whenever the task is scheduled, because
  the subject is the served module graph resolved out of `node_modules` at
  run time, not a moon-hashable input — a cached green would survive exactly
  the dependency-bump rot (`vite.config.ts:40-43`) the gate exists to red on.
- **Boot environment**: the fixture-mode dev server on an OS-assigned
  ephemeral port (`bunx vite --port <ephemeral> --strictPort --mode fixture`,
  `playwright.config.ts`) — offline by construction, no
  `VITE_COMPASS_BASE_URL`, no daemon, no caller-id env var (the UI has none:
  `.env.development:46-48` — caller identity comes from the WhoAmI RPC, and in
  fixture mode from the stub store). The port is picked at config-load and
  pinned in `process.env` so the runner, workers, and webServer launch agree;
  a fixed port (vite's 5173 default) collides with any dev server already on
  the box — Matt's review vite, or a second agent running this harness — which
  `--strictPort` turns into a hard launch failure (RIG-2283). RIG-1536's
  `VITE_COMPASS_CALLER_ID` note predates fixture boot and is obsolete — the
  var does not exist in the tree (grep of `apps/ui` finds zero occurrences).
- **Task hygiene**: the new spec must never be picked up by `bun test` —
  `moon.yml:37-42` documents why `test` is scoped to `src/` (a
  `@playwright/test` spec under `bun test` throws); the new spec therefore
  lives under `e2e/`, keeping that boundary intact.
- **No planning metadata in source**: the spec and moon task carry
  maintainer-voice comments only; the issue reference lives in this record.

## Plan

### T1 — the `dev-boot` Playwright spec

Add `apps/ui/e2e/dev-boot.spec.ts`: one test that navigates to `/#/` on the
config's `baseURL` (`playwright.config.ts`, `http://localhost:${devPort}` on
the ephemeral port; the shared `webServer` block boots the fixture-mode dev
server), collects every
`pageerror` from before navigation to the end of the test (the
`page.on("pageerror", …)` listener attaches before `page.goto`), waits for the
app's mount (the `.bridge` root surface, the same selector the visual harness
keys on at `e2e/visual-smoke.spec.ts:22`), and asserts (a) `#root` innerHTML is
non-empty and (b) the collected `pageerror` list is empty.

The failure path is ordered so the collected `pageerror` list is REPORTED even
when the `.bridge` wait times out: either race the `.bridge` wait against a
promise that rejects on the first `pageerror`, or assert the `pageerror` list
in a `finally`/soft-assertion that still runs on the timeout path. Without
this ordering, the exact defect class the gate exists for — the module graph
dies before mount, so `.bridge` never appears — surfaces as an undiagnostic
30s locator timeout ("Timeout 30000ms exceeded waiting for
locator(\".bridge\")") and the log never names the broken module; with it,
every red names the underlying error.

- `Interfaces:` consumes `apps/ui/playwright.config.ts` (unchanged: `baseURL`,
  `webServer`, `use.launchOptions.executablePath`); produces
  `apps/ui/e2e/dev-boot.spec.ts` exporting Playwright tests only (no runtime
  exports). Uses `page.on("pageerror", (e: Error) => …)` and
  `page.locator("#root")` / `page.locator(".bridge")`.
- Test cycle: `bunx playwright test e2e/dev-boot.spec.ts` from `apps/ui` passes
  on the current tree.

### T2 — the `dev-smoke` moon task + `ci` wiring

In `apps/ui/moon.yml`, add:

```yaml
dev-smoke:
  command: 'bunx playwright test e2e/dev-boot.spec.ts'
  deps: ['install']
  inputs: ['src/**/*', 'index.html', 'tsconfig.json', 'package.json', 'vite.config.ts', 'e2e/dev-boot.spec.ts', 'playwright.config.ts', '/bun.lock', '/packages/compass-client/src/**/*', '/packages/compass-client/package.json']
  options:
    cache: false
```

and extend `ci.deps` (`moon.yml:59-60`) to
`['typecheck', 'build', 'test', 'stylelint', 'dev-smoke']`. Comment the task
with the two-setting rationale (inputs for affected-detection, cache-false so
a scheduled run is always a real run) in the file's existing voice. The
explicit `inputs` are load-bearing, not hygiene: a project task with no
`inputs` key defaults to project-relative `**/*`, which excludes the
workspace-root `/bun.lock` — so without them a dependency-bump PR touching
only `bun.lock` (the PR shape most likely to rot the `optimizeDeps` pins)
could be skipped by the PR-path affected gate. The list is `build`'s
(`moon.yml:22`) plus the spec and the Playwright config, the two files whose
edits must re-schedule the gate.

- `Interfaces:` consumes T1's spec path; produces the `dev-smoke` task target
  `compass-ui:dev-smoke` and the extended `compass-ui:ci` dep list.
- Test cycle: `moon run compass-ui:dev-smoke` passes; a second invocation
  re-executes (no cache hit); and `moon ci :ci` against a synthetic
  lockfile-only diff (touch `/bun.lock` on a scratch branch) shows
  `compass-ui:dev-smoke` scheduled — the `moon run` cycle ignores affected
  entirely, so only the `moon ci` step exercises the inputs'
  affected-detection.

### T3 — CI Chromium provisioning

Add nixpkgs `chromium` to the `packages` list in `devenv.nix` (`:40`), with a
comment in the list's established style naming this gate as the consumer. The
parity pipeline picks it up with no workflow edit for PATH purposes
(`gate-tools.nix:5-8`), but Playwright needs the executable PATH, not the
command on PATH. Set `PLAYWRIGHT_CHROMIUM_PATH` into `GITHUB_ENV` after the
nixpkgs tools land on PATH, choosing the form by arm — because a step's
`$GITHUB_PATH` append only affects SUBSEQUENT steps, never the current one:
a separate later step uses
`echo "PLAYWRIGHT_CHROMIUM_PATH=$(command -v chromium)" >> "$GITHUB_ENV"`;
extending the phase-two step at `ci.yml:197-212` instead must use the
in-scope store path directly,
`echo "PLAYWRIGHT_CHROMIUM_PATH=$out/bin/chromium" >> "$GITHUB_ENV"` (there
`$(command -v chromium)` would resolve nothing — `$out/bin` is not yet on the
running step's PATH — silently falling back to the dev-box path in
`playwright.config.ts:31-33`).

- `Interfaces:` consumes `devenv.nix` `packages` list and
  `.github/workflows/ci.yml` phase-two step; produces `PLAYWRIGHT_CHROMIUM_PATH`
  in the CI job env, consumed by `playwright.config.ts:31-33`.
- Test cycle: CI run on the implementation PR shows `compass-ui:dev-smoke`
  executed and green; `bun tools/toolchain/parity.ts` stays green locally.
- Risk to name up front: the Chromium SANDBOX on the runner. GitHub's
  ubuntu-24.04 images restrict unprivileged user namespaces via AppArmor, and
  a nix-wrapped Chromium (no setuid sandbox helper) launched via a bare
  `executablePath` (`playwright.config.ts:26-34` — `launchOptions` carries no
  `args`) can fail to launch there. A launch failure reds the gate loudly —
  not a false green — but it would present as "gate is flaky/broken in CI" on
  the first T3 run. Resolve launch args (`--no-sandbox` via
  `launchOptions.args`, or `chromiumSandbox: false`) by first-run evidence —
  the same decide-by-measuring posture as the closure-weight fallback below.
- Risk to verify during implementation: nixpkgs `chromium` is a heavy closure;
  if realizing it in the parity gate is unacceptable, fall back to the
  `env`-block pattern `devenv.nix` already uses for the WebKitGTK closure
  (`devenv.nix:141-145`) plus an explicit workflow-level nix build — decide by
  measuring the first CI run, not up front.

### T4 — red→green proof

On the implementation branch, temporarily delete the `optimizeDeps.include`
block (`vite.config.ts:44-49`), run `moon run compass-ui:dev-smoke`, and record
the failure output; restore the block, re-run, record the pass. Both
transcripts go in the implementation PR body. Never committed — the broken
state exists only in the working copy between the two runs.

With T1's failure-path ordering, the red transcript names the broken module
(`debug`/`extend`) rather than a bare locator timeout — that naming is part of
what T4 records.

- `Interfaces:` consumes T1+T2; produces the two transcripts in the PR body.
- Test cycle: IS the test cycle — red on the RIG-1535-era config, green on
  current `main`'s.

## Tasks

- [ ] T1: `apps/ui/e2e/dev-boot.spec.ts` — headless boot spec (wait for the
      `.bridge` mount selector AND zero `pageerror`, with `#root`-non-empty as
      the redundant secondary guard; pageerrors reported even on a mount
      timeout), passing locally via `bunx playwright test e2e/dev-boot.spec.ts`.
- [ ] T2: `apps/ui/moon.yml` — `dev-smoke` task (explicit `inputs` +
      `cache: false`) wired into `ci.deps`; `moon run compass-ui:dev-smoke`
      green and uncached; `moon ci :ci` schedules it on a synthetic
      lockfile-only diff.
- [ ] T3: CI Chromium — `chromium` in `devenv.nix` packages;
      `PLAYWRIGHT_CHROMIUM_PATH` exported in the workflow; parity gate green;
      `compass-ui:dev-smoke` green in a real CI run.
- [ ] T4: red→green proof — failure transcript on the de-pinned config, pass
      transcript on the restored one, both in the PR body.

## Resolved decisions

All three ratified by Matt at the design-PR gate; the Approach above is written
to each. The design-critic red-team found no genuine fork here — each carried a
validated recommendation, and Matt ratified the set.

1. **Guard shape: headless boot (Shape B) alone** (Matt, 2026-08-18). B covers
   the whole "dev serves, app doesn't boot" class, not just CJS-interop holes;
   the module-graph crawl (Shape A) is ~1.8s and browser-free but a bespoke
   crawler over Vite serving internals that only catches the one class, and B
   subsumes its verdict today. Accepted cost: a CI browser and some flake
   surface. Named residual: a CJS rot in a LAZILY-loaded chain (e.g. shiki at
   `highlighter.ts:38-56`) exits B's static-boot coverage; Shape A stays the
   documented fallback if the CI browser cost is later vetoed.
2. **Tooling: reuse the Playwright harness** (Matt, 2026-08-18) — not the
   issue's `puppeteer-core`. The repo already has `@playwright/test` 1.62.1, a
   `playwright.config.ts`, and a working spec; `puppeteer-core` is not a
   dependency, and adopting it would plant a second browser-automation
   convention for zero capability gain. The issue's puppeteer suggestion
   predates the repo's Playwright adoption.
3. **CI wiring: `dev-smoke` in `compass-ui:ci`** (Matt, 2026-08-18) — every
   affected UI PR, every main push, and nightly, not full-sweep-only. The boot
   cost is a dev-server start + one page load (tens of seconds) inside a job
   whose timeout is 90m for nix builds (`ci.yml:107-109`) — negligible
   marginal cost, and a PR that breaks dev boot should red on the PR, not a day
   later. Named residual: `dev-smoke`'s explicit `inputs` (T2 — `build`'s list
   plus the spec and `playwright.config.ts`) make a `bun.lock`-only dependency
   bump schedule the gate directly, but a repo-external rot (nothing changed in
   this project at all) is caught only by the main-push/nightly full sweep —
   the designed backstop (`ci.yml:31-36`).

Assumption designed against (flagged, not blocking): nixpkgs `chromium` is
buildable/substitutable on the CI runner within acceptable time; T3 carries the
measured fallback if not.
