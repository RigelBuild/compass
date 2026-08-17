# Compass UI fixture boot — fully offline, fixture-seeded UI

Status: Draft
Ledger-impact: none (assessment at the end)

## Problem / Intent

The Bridge re-clothe visual-smoke harness (`apps/ui/e2e/visual-smoke.spec.ts`)
is Matt's before/after review vehicle, but the app can only boot against a live
daemon, and the ambient dogfood daemon's board is empty — so the board
screenshots show no issue cards and the baselines are uninformative. Matt has
chosen a **fully offline UI boot mode seeded with fixtures** (over seeding the
server, which would require live GitHub/Linear credentials). This record
designs how that mode works, what its hard safety wall is, and plans the
implementation slices.

## Global Constraints

- **Stack**: SolidJS ^1.9.13 + Vite + TypeScript; pure CSS custom props. No new
  runtime dependency.
- **Hard wall (the load-bearing safety property)**: the fixture boot path must
  be *structurally incapable* of shipping in a production bundle — selected by
  a build-time signal a production build cannot accidentally set, dead-code
  eliminated from every non-fixture build, and guarded by a build-scan test.
- **Determinism**: the fixture-booted app must render byte-stable across runs —
  no clock reads, no randomness, no ordering nondeterminism reaching the DOM;
  animations neutralized at capture time so screenshots diff cleanly.
- **Reuse, don't fork**: the fixtures are the existing `STUB_ISSUES` /
  `STUB_AGENTS` (`apps/ui/src/stub-data.ts`) and `STUB_COMMS_STATE`
  (`apps/ui/src/comms-stub.ts`); the test double is the existing
  `compass-fake.ts`. No parallel fixture set.
- **UI-layer only**: no Go backend, daemon, or dogfood-e2e changes. The Go
  `go test -tags podman ./e2e/...` tier stays the real-transport integration;
  this mode is complementary UI-layer determinism.
- **Live path untouched**: with fixture mode off, the live-daemon boot path
  (`index.tsx` → `envConnectionProvider` → `createLiveClients` → WhoAmI →
  `createAppStore`) behaves exactly as today.
- **No committed secret / no new env file**: the mode must not require any
  `.env*` addition; the env-secrecy gate (`apps/ui/src/env-secrecy.test.ts`)
  stays as-is.

## Approach

### What actually blocks offline boot (ground truth)

The store already knows how to be offline. Its issue signal seeds from the
fixture and is only overwritten when a compass client is supplied —
`apps/ui/src/store.ts:685`:

```ts
const [issues, setIssues] = createSignal<Issue[]>(STUB_ISSUES);
```

and the overwrite is gated — `store.ts:946,959-961`:

```ts
if (options.compass) {
    ...
    void runEventStream({
        client,
        onIssues: setIssues,
```

Same for the fleet — `store.ts:847-849`:

```ts
const agents = createMemo<readonly Agent[]>(() =>
    options.comms ? joinAgents(accounts(), presence()) : STUB_AGENTS,
);
```

and the daemon banner — `store.ts:945`: `const [daemon, setDaemon] =
createSignal<DaemonInfo>(STUB_DAEMON);`, only probed live behind the same
`options.compass` gate. Both clients are declared optional on the options
type — `store.ts:573` `readonly comms?: CommsClient;` and `store.ts:595`
`readonly compass?: CompassClient;` — and the header at `store.ts:557-559`
says it outright: *"optional so a unit test constructs the store with NO
network client at all: the comms surface then holds `initialComms` (the
fixture, in tests) and every write rejects"*. Every component test in the repo
(`store.test.ts:35-38`, `test-router.tsx:43-46`, …) already builds exactly
this store with `initialComms: STUB_COMMS_STATE`.

What forces a daemon is solely the boot chain in `apps/ui/src/index.tsx`:

- `index.tsx:46-49` — the mode dispatch:

  ```ts
  const bootConnectionForMode =
      shellMode() === "client"
          ? () => bootNativeClient(root)
          : () => bootConnection(root, () => envConnectionProvider().resolve());
  ```

- `apps/ui/src/live/connection.ts:46-51` — resolve throws without a door URL:

  ```ts
  const baseUrl = env.VITE_COMPASS_BASE_URL?.trim();
  if (!baseUrl) {
      throw new Error(
          "VITE_COMPASS_BASE_URL is required to reach the Compass server; " +
  ```

- `index.tsx:80-87` — real clients + a WhoAmI round-trip that stops boot:

  ```ts
  const clients = createLiveClients(connection);
  const callerId = await bootCaller(root, () => resolveCaller(clients.compass));
  ...
  if (!callerId) {
      return;
  }
  ```

So the design is small by construction: **add a third boot arm that builds the
already-supported clientless (offline) store from the existing fixtures and
mounts the same render tree** — no fixture network client, no fake transport,
no WhoAmI. The visual-smoke harness header (`visual-smoke.spec.ts:3-9`) still
declares this shape — it "Navigates the HashRouter surfaces of the stub-data
app … for Matt's before/after review", with no daemon; the live wiring
(SEA-1729) is what invalidated it in practice.

### A1 — Mode selection + the hard wall (the load-bearing choice)

Three mechanisms weighed:

1. **Runtime env flag** (`VITE_COMPASS_FIXTURE=1` read at boot). Rejected:
   Vite inlines every `VITE_*` value into the bundle, so the flag is settable
   from any build environment or an untracked `.env` file — exactly the
   surface the env-secrecy gate exists to distrust (`env-secrecy.test.ts:1-9`).
   The fixture code would also ship in every production bundle unless the
   check happens to constant-fold. Weakest wall.
2. **Dedicated entry point** (a second `fixture.html` + entry module,
   multi-page Vite build). Strong wall (the prod `index.html` never references
   the entry) but heaviest: a second HTML shell to keep in sync, harness URLs
   move to `/fixture.html#/…`, and `vite build` still emits the fixture chunk
   unless the input list is also mode-switched — i.e. it needs the mode switch
   *anyway*.
3. **Vite mode** (`vite --mode fixture`) gating a **statically-replaced
   branch with a dynamic import** in the existing `index.tsx:46-49`
   dispatch. **Recommended.**

Why (3) is the wall:

- `import.meta.env.MODE` is **not settable from the environment** — no env file
  or process variable sets it, unlike any `VITE_*` var. The only other setter
  is the `mode` field of `vite.config.ts`, which is in-repo and reviewable, so
  the mode cannot be flipped by a build's ambient environment.
- Vite statically replaces `import.meta.env.MODE` at build/serve time, so
  `if (import.meta.env.MODE === "fixture")` becomes `if ("production" ===
  "fixture")` in a production build: the branch is dead-code-eliminated, and
  because the fixture boot module is reached **only via a dynamic `import()`
  inside that branch**, its chunk is never even emitted. The fixture code is
  absent from the artifact, not merely dormant — that is "structurally
  incapable of shipping". This DCE guarantee holds only while the comparison
  stays **inline** (`if (import.meta.env.MODE === "fixture")`); hoisting it to
  `const m = import.meta.env.MODE; if (m === "fixture")` defeats the fold —
  which is why the build-scan gate (below), not the DCE alone, is the
  authoritative wall.
- **Invariant the chunk-absence depends on:** `boot-fixture.ts` must have **no
  static importer anywhere in the production `src` graph** — it is reached
  ONLY via the dynamic `import()` in the fixture branch. A single
  `import { … } from "./boot-fixture"` in shipped code re-emits its chunk into
  the production bundle and the negative build-scan assertion fails. (T3's
  `fixture-wall.test.ts` may import `FIXTURE_SENTINEL` from `boot-fixture.ts`
  because a test is excluded from the production build — that is not a static
  importer in the shipped graph.)
- Defense in depth, three layers — the build-scan gate is authoritative, the
  other two are accident-insurance around it:
  - a **build-scan gate test** — the authoritative layer — mirroring the
    existing precedent `apps/ui/src/preview-build.test.ts` (which already runs
    `bunx vite build --outDir …` and asserts on the emitted bundle text,
    `preview-build.test.ts:79`): build production `dist/`, assert a unique
    sentinel literal defined in the fixture boot module is **absent**, plus a
    positive-control assertion that a known live-app literal **is** present so
    the scan can never pass by reading nothing (see T3);
  - a **runtime tripwire** in the fixture boot module — `if
    (import.meta.env.PROD) throw` — which stops the realistic *accident* (a CI
    or dev job that gains `--mode fixture`: `vite build` defaults `NODE_ENV` to
    production, so `import.meta.env.PROD` is true and the throw fires). It does
    **not** stop a deliberate `NODE_ENV=development vite build --mode fixture`
    (Vite lets an explicit `NODE_ENV` override the default, leaving `PROD`
    false) — that is precisely why the build-scan gate, not the tripwire, is
    the wall; the tripwire is insurance, and demo-hosting stays blocked by
    policy (§A4);
  - Vite's env-file loading is mode-keyed, so `--mode fixture` does **not**
    load `.env.development` — `VITE_COMPASS_BASE_URL` is simply absent in
    fixture serves, meaning even a regression that reached the live arm would
    throw at `connection.ts:48` rather than dial anything.

The dispatch composes with the existing comment contract at `index.tsx:23-33`
(shell-injected mode → client/embedded, absent → browser env path): fixture is
a third, browser-only arm, checked before the shell modes since a shell never
launches with `--mode fixture`.

A component/story harness (e.g. Storybook) was not chosen: the harness subjects
are whole-shell, URL-routed compositions (full-page Bridge with both sidebars +
topbar + usage bar, `visual-smoke.spec.ts:19-23`), and the repo already has the
stronger primitive — `test-router.tsx:37-56` mounts the full clientless shell
today. This design productionizes that exact proven construction behind a
served URL, which a component harness cannot give without a new runtime
dependency (against the stack constraint) and still no full-page routed shell.

### A2 — The fixture boot module (`apps/ui/src/boot-fixture.ts`)

A sibling of `boot-native.ts`, ~40 lines, that mirrors `main()`
(`index.tsx:76-145`) minus the network:

1. Build the same `QueryClient` (`index.tsx:97-99` shape).
2. `createRoot(() => createAppStore({ queryClient, initialComms:
   STUB_COMMS_STATE, workspaceKey: "fixture" }))` — **no `comms`, no
   `compass`**. By the gates quoted above this yields: board = `STUB_ISSUES`
   (never overwritten), fleet = `STUB_AGENTS`, banner = `STUB_DAEMON`, comms
   surfaces = the comms fixture. `callerId` defaults to `CALLER_ID`
   (`store.ts:74` `export const CALLER_ID = "acc-matt";`, applied at
   `store.ts:681`), the same identity the fixtures are authored around — no
   WhoAmI needed.
3. Mount the identical render tree. To avoid duplicating the JSX block at
   `index.tsx:133-144`, extract it into a shared `mountShell(root, store,
   queryClient)` helper both `main()` and `bootFixture()` call — the one
   mechanical refactor this record makes to the live path (behavior
   unchanged; the extraction is the whole diff). Named `mountShell`, **not**
   `mountApp` — `test-router.tsx:37` already exports a test-only `mountApp`
   (MemoryRouter, `{store,container}` return); a second same-named export in
   the `src` tree is a grep/import trap.

No fixture `CompassClient` is constructed at all in v1. That is deliberate and
is itself part of the wall: there is no fake client object that *could* leak
into a production code path, because the offline arm passes no client and the
store's existing `options.compass` gate does the rest.

**The compass-fake extension is the explicit upgrade path, not v1.** If a
Bridge shot later needs a *live-update* visual state (an event arriving, a
card advancing — Bridge T5 territory), the seam is
`apps/ui/src/live/compass-fake.ts`: `createFakeCompass()` already implements
the driven client subset (`whoAmI` at `compass-fake.ts:82`, `getServerInfo` at
`:71`, an abort-honoring `subscribeEvents` generator at `:96-105`) behind the
one sanctioned `client as unknown as CompassClient` cast
(`compass-fake.ts:112`). The extension shape: give `createFakeCompass` an
optional scripted-frame option (`opts?: { events?: readonly
SubscribeEventsResponse[]; board?: readonly BoardIssue[] }`) whose
`subscribeEvents` yields the cold-start boundary frame + scripted tail and
whose `listBoardIssues` serves the wire snapshot — the same protocol
`live/events.test.ts`'s `scriptedTransport` already fakes at the transport
layer (`live/events.test.ts:116-122`). The cost it carries — maintaining a domain→wire
rendering of `STUB_ISSUES` — is why it is deferred until a shot actually
needs it (D2).

### A3 — Harness pointing + determinism

`apps/ui/playwright.config.ts:41` currently serves plain dev:

```ts
command: "bunx vite --port 5173 --strictPort",
```

The webServer command gains `--mode fixture`; the specs
(`visual-smoke.spec.ts`) keep their URLs — same origin, same hash routes, and
`.state-dot` / `.bridge` waits now find real cards. `use` also gains the
standard visual-testing determinism knobs (`reducedMotion: "reduce"`,
`deviceScaleFactor: 1`); see the residual-risk bullet below and T4.

Determinism inventory (verified):

- Fixture data carries only fixed literals; the sole wall-clock/random reads
  in the store are the write-path request-id prefix (`store.ts:984`
  `` `ui-${Date.now().toString(36)}-${Math.random()…` ``) which never renders,
  and stream reconnect jitter (`events.ts:121`, `stream.ts:314`) which never
  runs (no clients). Message timestamps render through fixed-input UTC
  formatting (`ChannelView.tsx:35-39`); no relative-"ago" rendering exists in
  `src/components` (grepped: zero matches for `Date.now|ago|toLocale`).
- **Async render swap (latent, must stay latent):** `MarkdownText.tsx:303`
  debounces (`HIGHLIGHT_DEBOUNCE_MS = 150`) then async-tokenizes fenced code
  through Shiki (`:337-341`), swapping a plain `<pre>` for highlighted HTML
  after capture time — `animations: "disabled"` does not touch it. Today it
  never triggers: no fixture message carries a code fence (grep: zero fenced
  blocks / `language-` inputs in `stub-data.ts` / `comms-stub.ts` /
  `session-events-stub.ts`). **Invariant, recorded next to the fixtures:** a
  fixture message must not carry fenced code, or T4's capture must settle past
  the swap.
- **Query microtask (harness-side wait):** `assignedIssues` settles on a
  microtask, not synchronously (`store.test.ts:1757-1758` pins the sync read as
  `[]` pre-tick); it feeds the "Assigned to me" section of `BacklogView.tsx:111`
  and the left-rail count (`LeftSidebar.tsx:427-428`). The CDP screenshot
  round-trip almost always outlasts a microtask, but "almost" is below the
  byte-stable bar — so T4 waits on a row **inside the "Assigned to me"
  section** (`#backlog-section-assigned-to-me .backlog-row`), never a bare
  `.backlog-row` and never the `.backlog-view` container. A bare `.backlog-row`
  is unsound: `BacklogView` renders three sections, and Todo/Backlog filter
  `store.issues()` **synchronously** (`BacklogView.tsx:93-94`) — their rows
  exist before the `assignedIssues` query settles, so a bare-row wait resolves
  early and re-opens the exact race it is meant to close. Only the
  "Assigned to me" section (`BacklogView.tsx:109-113`, fed
  `store.assignedIssues()`) gates on the microtask; the fixture tracker seam
  returns non-empty `STUB_ASSIGNED_ISSUES` for the default handle
  (`tracker.ts:99`, `DEFAULT_TRACKER_CONFIG.handle = "matt@sealed"`), so that
  scoped row is guaranteed to appear once the query resolves.
- Residual risk is CSS animation phase (e.g. a pulsing state dot mid-keyframe
  at capture) and raster-level drift (subpixel AA, late web-font swap).
  Neutralize in the **harness**, not the app: Playwright's
  `screenshot({ animations: "disabled", scale: "css" })` (+ `reducedMotion:
  "reduce"` and `deviceScaleFactor: 1` in `use`, and an `await
  page.evaluate(() => document.fonts.ready)` before each capture) — the app
  ships untouched and the rigel-motion tokens keep working for humans. These
  are the standard visual-testing determinism knobs (D5, D7); they make the
  same-box byte-identity self-test robust and are the substrate a future
  automated diff-gate needs.

### A4 — What this is not

Not a demo-hosting build (the PROD tripwire deliberately blocks
`vite build --mode fixture`; lifting that is a future record), not a
replacement for the Go dogfood-e2e tier, not a change to `.env*` handling, and
not a redesign of the stub fixtures themselves.

## Plan

### T1 — Extract `mountShell` and add the fixture dispatch arm

Extract the render block (`index.tsx:133-144`) into `mountShell(root, store,
queryClient)`; add the statically-gated fixture branch ahead of the existing
`bootConnectionForMode` chain, dynamic-importing the (new, T2) boot module.
Live behavior is byte-identical: `main()` calls `mountShell` with the same
arguments in the same order.

Interfaces:

- Produces `apps/ui/src/mount.tsx`:
  `export function mountShell(root: HTMLElement, store: AppStore, queryClient: QueryClient): void`
  (the `StoreContext.Provider` + `QueryClientProvider` + `HashRouter` tree,
  moved verbatim; named `mountShell` to avoid colliding with the test-only
  `mountApp` at `test-router.tsx:37`). Also exports
  `export function newAppQueryClient(): QueryClient` returning
  `new QueryClient({ defaultOptions: { queries: { retry: 1, staleTime: 30_000 } } })`
  — the single source for the app's query defaults, so the fixture boot (T2)
  cannot silently drift from the live client at `index.tsx:97-99`.
- Modifies `apps/ui/src/index.tsx`: the dispatch gains
  `if (import.meta.env.MODE === "fixture") { void import("./boot-fixture").then((m) => m.bootFixture(root)).catch((error) => renderBootError(root, "Compass UI cannot start", …)); } else { …existing chain… }` — the `.catch` routes a fixture chunk-load failure to the same painter the live chain uses (`index.tsx:57`), so a rejected dynamic import never leaves a blank `#root`;
  `main()` body replaces its inline `new QueryClient({…})` (`index.tsx:97-99`) with `newAppQueryClient()` and its `render(...)` tail with `mountShell(root, store, queryClient)`.
- Consumes: `AppStore` (`store.ts`), `QueryClient` (`@tanstack/solid-query`),
  `App`/`AppRoutes`/`StoreContext` (moved imports).

Test cycle: existing UI suite green (`moon run compass-ui:test` or repo
equivalent); `bunx vite` (no mode) + manual load against devenv still boots
live (smoke); type-check clean.

### T2 — `boot-fixture.ts`: the offline store boot + PROD tripwire

The fixture boot module: tripwire first, then the clientless store, then
`mountShell`. Contains the build-scan sentinel literal.

Interfaces:

- Produces `apps/ui/src/boot-fixture.ts`:
  `export function bootFixture(root: HTMLElement): void` —
  `if (import.meta.env.PROD) throw new Error(FIXTURE_SENTINEL + " must never boot in a production build")`;
  `const queryClient = newAppQueryClient()` (T1 — shared with the live path so query defaults cannot drift);
  `const store = createRoot(() => createAppStore({ queryClient, initialComms: STUB_COMMS_STATE, workspaceKey: "fixture" }))`;
  `mountShell(root, store, queryClient)`.
  Also `export const FIXTURE_SENTINEL = "COMPASS-FIXTURE-BOOT-SENTINEL-7f3a"` (unique, grep-proof).
- Consumes: `createAppStore` (`store.ts:680`), `STUB_COMMS_STATE`
  (`comms-stub.ts:714`), `mountShell` + `newAppQueryClient` (T1).

Test cycle: a bun unit test (`boot-fixture.test.ts`) asserting (a) the mounted
DOM renders ≥1 board card from `STUB_ISSUES` under a happy-dom root, (b)
`bootFixture` throws when `import.meta.env.PROD` is stubbed true. Smoke:
`bunx vite --mode fixture` → board renders the designed fleet with no daemon
running and no `VITE_COMPASS_BASE_URL` set.

### T3 — Hard-wall build gate

The structural guarantee, tested the way `preview-build.test.ts` already
tests bundle facts.

Interfaces:

- Produces `apps/ui/src/fixture-wall.test.ts`: runs
  `bunx vite build --outDir <tmp> --emptyOutDir` with a valid
  `VITE_COMPASS_BASE_URL` in env (the `preview-build.test.ts:79-82` recipe),
  reads all `dist/assets/*.js`, asserts `FIXTURE_SENTINEL` is **absent** AND —
  the positive control that keeps the scan from passing vacuously — that a
  known live-app literal (`"missing #root element"`, `index.tsx:20`) **is**
  present, so a scan that read nothing (assets relocated out of `dist/assets`
  by a future `vite.config` change) fails loudly instead of green.
- Consumes: `FIXTURE_SENTINEL` (T2 — imported for the literal, or duplicated
  as a string constant with a comment pinning the pairing; prefer importing
  from `boot-fixture.ts` so drift is impossible).

Test cycle: the gate itself red→green: first run it against a deliberately
un-gated static import (red), then with the T1 dynamic-import gate (green).

### T4 — Point the visual-smoke harness at fixture mode

Interfaces:

- Modifies `apps/ui/playwright.config.ts`: `webServer.command` becomes `"bunx
  vite --port 5173 --strictPort --mode fixture"`; `use` gains `reducedMotion:
  "reduce"` and `deviceScaleFactor: 1` (pins DPR so subpixel anti-aliasing
  can't drift between runs — the standard visual-testing knob).
- Modifies `apps/ui/e2e/visual-smoke.spec.ts`: every `page.screenshot(...)` /
  `locator.screenshot(...)` gains `animations: "disabled"` and `scale: "css"`
  (DPR-independent raster); each capture is preceded by `await
  page.evaluate(() => document.fonts.ready)` so a late web-font swap cannot
  land between the two runs. Header comment updated (the "boots fully on
  stub-data.ts" claim becomes true again, now guaranteed by fixture mode
  rather than by accident of un-wired live paths).
- Modifies the backlog capture in `apps/ui/e2e/visual-smoke.spec.ts` to wait on
  a row **inside the "Assigned to me" section**
  (`#backlog-section-assigned-to-me .backlog-row`) rather than a bare
  `.backlog-row` or the `.backlog-view` container, so it cannot fire before the
  `assignedIssues` query microtask settles (§A3 — Todo/Backlog rows render
  synchronously, so a bare-row wait would resolve early). The other surfaces
  already wait on content (`.state-dot`, `.bridge`).
- Consumes: T1+T2 landed.

Test cycle: run the harness twice back-to-back on the same box with no daemon
on :50051; assert (via `cmp`) the board PNGs are byte-identical across the two
runs and show issue cards. This byte-identity bar is a **same-binary,
same-box determinism self-test** — proof the harness emits a stable artifact
(no residual microtask/font/animation/DPR nondeterminism), not a
cross-environment regression oracle (Chromium raster differs across a
nix-channel or headless-shell bump; §A3, D7). It is the forcing function
that this boot path is deterministic: `bridge.png` now depicts the designed
board, reproducibly. Making the harness a real automated visual-regression
**gate** (`toHaveScreenshot` + a `maxDiffPixelRatio` threshold + baselines
committed to git + generated in a pinned CI environment) is Matt's stated
direction and lands as its **own follow-up record** — this record is the
deterministic substrate that gate requires, which is why the determinism
knobs above are specified now.

### T5 (deferred fast-follow — filed as its own issue) — scripted event replay

Extend `createFakeCompass` per §A2's shape and add a fixture-boot variant that
passes the fake's `client` as `options.compass`, for live-update / state-change
shots. Matt confirmed (D2 below) the split: static v1 ships now to unblock the
Bridge harness; replay is a named fast-follow, built when a concrete
state-change shot is specified (which is also when it can be scripted
meaningfully). Explicitly **out of v1 scope**; the seam is named here and the
work is tracked as its own issue, not built now.

## Tasks

- [ ] T1 — extract `mountShell` (name avoids the `test-router.tsx:37`
      `mountApp` collision), add the inline `MODE === "fixture"` dispatch arm
      (dynamic import + `.catch` → `renderBootError`); existing suite green,
      live boot unchanged.
- [ ] T2 — `boot-fixture.ts` (tripwire + clientless store + sentinel) with
      unit tests for render-from-fixtures and the PROD throw.
- [ ] T3 — `fixture-wall.test.ts` build gate: `FIXTURE_SENTINEL` absent from a
      production `dist/`, plus the `"missing #root element"` positive control
      so the scan can't pass vacuously.
- [ ] T4 — harness: `--mode fixture` webServer, `animations: "disabled"` +
      `scale: "css"` per capture, `reducedMotion: "reduce"` +
      `deviceScaleFactor: 1` in `use`, `document.fonts.ready` wait per capture,
      backlog waits on a row inside the "Assigned to me" section (not the
      container), regenerate `e2e/__screens__` baselines; verify same-box
      determinism across two runs (D7 — the automated diff-gate is a follow-up
      record, this is its deterministic substrate).
- [ ] T5 — (deferred fast-follow, own issue) `createFakeCompass`
      scripted-snapshot extension for live-update / state-change shots.

## Decisions (Matt-ruled)

All forks below were surfaced to Matt and ruled before freeze (2026-08-16).

- **D1 — Mode-selection mechanism / the hard wall.** **Vite `--mode fixture`**
  gating a statically-replaced `import.meta.env.MODE` branch with a dynamic
  import (§A1 option 3), plus the PROD tripwire and the build-scan gate.
  `--mode` is CLI-only (not env-settable, unlike any `VITE_*` flag), the branch
  is dead-code-eliminated so the fixture chunk is never emitted, and it
  composes with the existing `index.tsx:46-49` dispatch without a second HTML
  entry. Alternatives weighed and rejected in §A1.
- **D2 — Static snapshot vs scripted event replay.** **Static snapshot only in
  v1** (the clientless store — zero new client code, maximal determinism) ships
  now to unblock the Bridge harness; scripted replay (the `compass-fake.ts`
  extension, §A2 / T5) is a **named fast-follow filed as its own issue**, built
  when a concrete state-change shot is specified. All current harness surfaces
  (board, sidebar, agent, backlog, done, settings — `visual-smoke.spec.ts:14-63`)
  read a settled snapshot, so nothing in the first cut needs replay.
- **D3 — Merged-PR fixture for the Bridge T6/G11/F6 premise.** **Already
  satisfied, no scope here**: `STUB_ISSUES` contains done issues whose PRs carry
  `forgeState: "merged"` (`stub-data.ts:1143` — `forgeState: "merged"` on PR
  #436 under the issue at `:1131-1135`, and again at `:1186`), and the
  clientless store's `issues()` is all of `STUB_ISSUES` including those. This
  record does not touch `STUB_ISSUES`; any further shaping (e.g. a merged PR on
  an *in-progress* issue) belongs to Bridge T6, which owns that premise.
- **D4 — Comms in fixture mode.** **`initialComms: STUB_COMMS_STATE` and no
  comms client** — the rail/channel surfaces render populated (the exact
  construction every component test uses, e.g. `test-router.tsx:43-46`) with
  zero stream machinery; `EMPTY_COMMS_STATE` would leave the left rail and
  channel views empty in screenshots for no gain. Writes reject through
  `onCommsError` (absent → undefined → silently dropped), which a
  screenshot-only harness never exercises.
- **D5 — Where to neutralize animations.** **Harness-side only**
  (`animations: "disabled"` per screenshot + `reducedMotion: "reduce"`): the
  app ships no fixture-conditional styling, and rigel-motion stays intact for
  humans exploring `vite --mode fixture` interactively. (Matt noted a future
  interest in animation *tests* — that is its own future scope, not this
  record.)
- **D6 — Should `vite build --mode fixture` be allowed (a hosted demo)?**
  **No, blocked by the PROD tripwire** for now — a hosted-demo bundle is a
  different artifact with its own record; leaving the tripwire out would make
  the wall one accidental CI flag away from shipping fixture code.
- **D7 — Byte-stability boundary, and whether the harness is a gate.** The
  harness stays a **human before/after review tool** in this record and T4's
  byte-identity bar is a **same-binary, same-box determinism self-test** (proof
  the boot path emits a stable artifact — no residual microtask/font/animation/
  DPR nondeterminism), captured on the box-local pinned Chromium
  (`playwright.config.ts:29-31`), never a cross-environment oracle (Chromium
  raster differs across a nix-channel or headless-shell bump). Matt's stated
  direction is to **make the harness a real automated visual-regression gate**
  (`toHaveScreenshot` + a `maxDiffPixelRatio` threshold + baselines committed to
  git + generated in a pinned CI environment) — that lands as its **own
  follow-up design record**, for which this record is the required deterministic
  substrate. That is why the determinism knobs (D5, T4) are specified now even
  though the automated gate is out of this record's scope.

## Ledger assessment

**Ledger-impact: none.** This is a dev-infra/testing boot-path addition: it
supersedes no frozen product decision (the board model DL-069/DL-071, the
live-wiring records SEA-1729, and the test-strategy record's Go e2e tier are
all untouched — the fixture mode is complementary UI-layer determinism, and
the live boot path is behavior-identical when the mode is off). No
`DECISIONS.md` delta owed; the driver handles ledger/PR mechanics either way.
