# Solid v2 migration — compass apps/ui (RIG-2187)

Status: Draft

Issue: RIG-2187 (child of RIG-2186 Target 2).

## Problem / Intent

`apps/ui` is a live SolidJS single-page app: `solid-js ^1.9.13` with
`@solidjs/router ^1.0.0` and `vite ^8.2.0` / `vite-plugin-solid ^2.11.14`
(`apps/ui/package.json:17,24,41-42`), running inside a **Wails v3** desktop
shell (`"@wailsio/runtime": "3.0.0-beta.0"`, `apps/ui/package.json:20`). Two
premise corrections against the issue body: it is **not React** and **not
SolidStart** (no `@solidjs/start` anywhere in `package.json`; the only
router is client-side `HashRouter`, `src/mount.tsx:14` `import { HashRouter }
from "@solidjs/router"`), and the shell is **Wails, not Tauri** — there is no
`@tauri-apps/*` dependency; the runtime binding is
`src/daemon-transport.ts:192` `import { Call, Events } from "@wailsio/runtime"`.
A few stale comments still say "Tauri" (e.g. `src/App.tsx:23` "no daemon and no
Tauri IPC") — prose drift from an earlier plan, not a real binding.

Solid 2.0 is the framework's next major (deterministic microtask batching,
first-class async, the DOM renderer split into `@solidjs/web`). Staying on the
1.x line indefinitely means accumulating against a frozen API while the
ecosystem (router, solid-query, testing-library) moves its main line to 2.
Intent: migrate apps/ui to Solid v2 without ever breaking the shipping app —
mechanical prep lands on main v1-safe, the actual dependency bump is gated on
ecosystem readiness.

## Approach

**Incremental, keep-app-runnable-at-every-slice.** Two phases:

1. **v1-safe prep on main (now).** Solid 2's sharpest behavioral break is
   deterministic microtask batching — "setters don't immediately change what
   reads return; values become visible after the microtask batch flushes (or
   via `flush()`)" (`documentation/solid-2.0/MIGRATION.md`, solidjs/solid
   `next` branch) — plus the `createEffect` compute/apply split. Both reward
   the same prep, legal on v1 today: eliminate read-after-set assumptions,
   shrink the `createEffect` surface into `createMemo` derivations, and give
   static-dep effects an explicit dependency via `on(...)` (an effect whose
   tracked set is deliberately broad and dynamic — the route-sync effect,
   see S1 — stays implicit; v2 keeps implicit tracking). That shape is
   mechanically 1:1 with v2's `createEffect(deps, apply)` but is still
   rewritten at bump time — v2 removes the `on` helper outright ("`on` helper
   → no longer necessary with split effects", MIGRATION.md Removals; the
   `defer` option moves onto `createEffect` directly).
   `src/components/MessageStream.tsx:45-52` already models the target shape:
   `createEffect(on(() => props.scopeId, …))`.

2. **The v2 bump on a migration branch — built now, merge gated.** Per
   Matt's ruling (2026-08-18) the branch is built NOW against `2.0.0-rc.0`
   with branch-only peer overrides, not lazily when the ecosystem moves.
   The dep bump + v2-only codemods (`Index`→`<For keyed={false}>`,
   `onMount`→`onSettled`, `unwrap`→`snapshot`, `@solidjs/web` +
   `jsxImportSource`, store draft-first setters) still cannot land on main
   until both blocker surfaces accept solid-js 2 — `@tanstack/solid-virtual`
   and `solid-markdown` have **no upstream version whose peer range accepts
   solid-js 2** (readiness matrix rows: both cap at `^1.x`), so both are
   carried as forks per Resolved decision 2. The branch is rebased
   periodically; main stays on v1 GA and shippable.

   Cross-issue note (owned by RIG-2186 planning, not this record's tasks):
   Matt also ruled the RIG-2186 accounts-login greenfield starts directly
   on Solid v2 — a new surface with no blocker deps (it need not use
   solid-virtual/solid-markdown) — decoupling its timeline from apps/ui's
   incremental branch.

Per-slice verification, every slice on either phase: full
`bun test --conditions browser src/` green, `tsc` clean, and the app boots in
`vite dev` (the RIG-1536 dev-boot gate).

## Alternatives considered

Three arms for the migration-timing fork (Resolved decision 1 carries the
sharper branch-build-timing framing and Matt's ruling), grounded in the
ecosystem readiness
table under Global Constraints (npm registry dist-tags + per-version
peerDependencies, checked 2026-08-18):

1. **Migrate-now-on-RC.** Bump everything to the v2 track today: solid-js
   `2.0.0-rc.0`, router `2.0.0-next.16` (pre-RC), testing-library
   `1.0.0-beta.2`, vite-plugin-solid `3.0.0-next.27` (pre-RC), solid-query
   `6.0.0-rc.0`. Loses: it pins a live surface to a pre-release of a
   pre-release in three places, forces peer-range overrides on two
   load-bearing deps with **no** v2 release (`@tanstack/solid-virtual` — the
   message-stream virtualizer, `src/components/MessageStream.tsx:32`; and
   `solid-markdown` — the entire message renderer,
   `src/components/MarkdownText.tsx:18`), and stacks the TanStack Query v5→v6
   major on top of the reactivity migration in one bet. Highest risk, zero
   schedule payoff beyond being first.

2. **Wait-for-GA, do nothing until then.** Lowest risk; the two blockers
   likely resolve when their maintainers cut v2 releases post-GA. Loses: the
   timeline is unknown, and it wastes the window — the batching/effect prep is
   v1-legal today, shrinks the eventual diff, and improves the v1 code on its
   own merits. Doing literally nothing buys nothing.

3. **Incremental-behind-a-branch (recommended).** Land the v1-safe prep on
   main now; hold the dep bump + v2-only codemods on a migration branch,
   rebased against main, merged only when the gate clears (solid-js 2 GA — or
   a deliberate Matt-approved RC exception — **and** both blocker deps accept
   solid-js 2 in a published peer range, **and** the toolchain
   (vite-plugin-solid, babel-preset-solid, testing-library) is at
   RC-or-better, **and** solid-query v6 is evaluated).
   Wins: main is never hostage to the ecosystem, the mechanical risk is paid
   down early in small reviewable slices, and the final bump diff is minimal.
   Costs: branch upkeep — real but mechanical: ~8 src files plus broad
   test-file churn, since v2 batching means "setters don't immediately change
   what reads return" and `flush()` is "most useful in tests" (MIGRATION.md
   Batching/reads), so every set-then-assert test gains a `flush()` — the
   ~30 test `createRoot`s and ~10 testing-library files dominate the diff.
   An optional S2b prep slice could wrap set-then-assert test patterns behind
   a tiny helper (identity on v1, `flush()` on v2), shrinking the branch's
   test diff to one helper.

## Global Constraints

### Ecosystem readiness (npm registry, checked 2026-08-18)

| Dep | Current (apps/ui) | GA latest | v2-track version + tag | solid-js 2 in peer range? |
| --- | --- | --- | --- | --- |
| solid-js | ^1.9.13 | 1.9.15 | `2.0.0-rc.0` (`next`) | self |
| babel-preset-solid | 1.9.12 | 1.9.15 | `2.0.0-rc.0` (`next`) | — |
| @solidjs/router | ^1.0.0 | 1.0.0 | `2.0.0-next.16` (`next`) — pre-RC | `^2.0.0-rc.0`, plus a NEW peer `@solidjs/web ^2.0.0-rc.0` |
| @solidjs/testing-library | 0.8.10 | 0.8.10 | `1.0.0-beta.2` (`next`) — beta | `>=2.0.0` + `@solidjs/web >=2.0.0` |
| @tanstack/solid-query | ^5.101.4 | 5.101.4 | `6.0.0-rc.0` (`rc`) — a MAJOR 5→6 bump | `>=2.0.0-rc.0 <3.0.0` |
| @tanstack/solid-virtual | 3.13.36 | 3.13.37 | **none** | `^1.3.0` — **excludes 2** |
| solid-markdown | 2.1.1 | 2.1.1 | **none** | `^1.6.0` — **excludes 2** |
| vite-plugin-solid | ^2.11.14 | 2.11.14 | `3.0.0-next.27` (`next`) — pre-RC | peerDeps unconstrained |

Re-verify this table against the registry before executing the dep-bump slice
— it is a snapshot, and the gate condition is defined against the live
registry, not this table. The re-verify step also checks vite 8 ×
vite-plugin-solid 3 compatibility (the matrix row only tracks the plugin's
solid peer, not its vite range).

### Constraints

- **The dep bump is gated.** No v2 dependency lands on main until (a) both
  blocker surfaces accept solid-js 2 — the consumed `@tanstack/solid-virtual`
  fork (or upstream, if the TanStack PR lands first) and the RigelBuild
  `solid-markdown` fork each publishing a version whose peer range accepts
  solid-js 2 (Resolved decision 2), (b) solid-js 2 is GA (or Matt explicitly
  waives GA at merge time), and (c) the toolchain — vite-plugin-solid,
  babel-preset-solid, @solidjs/testing-library — is at RC-or-better, not
  beta/next: a merge that pins pre-releases on main is exactly what arm 1 was
  rejected for.
- **Version floors at bump time:** solid-js ≥2.0.0, `@solidjs/web` ≥2.0.0
  (a NEW dependency — the DOM renderer split out of core; router v2 and
  testing-library v2 both peer on it), @solidjs/router ≥2.0.0,
  @solidjs/testing-library ≥1.0.0, babel-preset-solid ≥2.0.0,
  vite-plugin-solid ≥3.0.0, @tanstack/solid-query ≥6.0.0 (TanStack Query v6 —
  its own major, migrated as its own slice).
- **The shell is Wails v3, not Tauri.** All shell IPC flows through
  `@wailsio/runtime` (`src/daemon-transport.ts:192`); fix the stale "Tauri"
  prose while touching those files, and never introduce Tauri terminology.
- **The app stays shippable at every slice**: `bun test --conditions browser
  src/` fully green, `tsc` clean, boots in `vite dev`. A slice that cannot
  meet that does not land on main — it goes to the migration branch.
- **No second convention.** Prep slices converge on ONE effect idiom
  (`createEffect(on(deps, apply))` for static-dep effects, implicit tracking
  only where the tracked set is deliberately dynamic — `store.ts:740` — and
  `createMemo` for derivations); never a mix of pre- and post-styles in
  shipped v1 code.
- **No planning metadata in source.** Code comments reference designs/issues
  normally; no migration-scaffold markers left behind.

## Plan

### Touch-point inventory (grepped on this workspace, `apps/ui/src`)

**Reactivity — effects (the v2 compute/apply split + batching):**

- `src/components/MessageStream.tsx:45-48` — `createEffect(on(() =>
  props.scopeId, …))`, the scope-switch scroll re-anchor. Already in the
  v2-shaped `on(deps, apply)` form.
- `src/components/MarkdownText.tsx:335-340` — `createEffect(() => { … const t
  = setTimeout(() => setSettled(next), HIGHLIGHT_DEBOUNCE_MS); … })`, the
  highlight debounce; implicit deps (`code()`, `lang()`), needs the `on(...)`
  shape.
- `src/store.ts:740` — `createEffect(() => applyRoute(r.currentPath()))`, the
  single-writer route-sync effect installed by `bindRouter`. Its tracked set
  is intentionally broad, dynamic, and load-bearing — NOT one implicit dep:
  `applyRoute` (`store.ts:1109`) fans out per route branch into
  `applyChannelRoute`/`applyTopicRoute`/`applyAgentRoute`, whose reactive
  reads (`channels()` at `store.ts:1159`, `firstSnapshotArrived()` at
  `store.ts:1163,1181,1191`, `topics()` at `store.ts:1187`, `comms()` via
  `firstChannelId`, and `issues()`/`selectedIssueId()`/`agentViewAgentId()`
  at `store.ts:1213-1216`) re-run the effect when they change post-snapshot —
  the re-run that drives record-A3's pending-aware deep-links (e.g.
  `routing.test.tsx:259`, "an absent topic deep-link falls back to the
  channel index after the snapshot").

**Reactivity — memo/signal graph (batching semantics, mostly mechanical-safe):**

- `src/store.ts:22-28` imports `createEffect, createMemo, createSignal,
  getOwner, onCleanup`; `createAppStore` (store.ts:680-2057) holds the app's
  signal graph — view/selection (`store.ts:685-695`), router seam
  (`store.ts:709`), tracker (`store.ts:750`), panes (`store.ts:770-814`),
  pins memo (`store.ts:799-801`), comms. Synchronous set-then-read inside
  actions is the batching-audit hotspot.
- `createSignal` in components (local UI state): `BacklogView.tsx:49`,
  `Bridge.tsx:99,101`, `ChannelView.tsx:284-285,413-415`,
  `LeftSidebar.tsx:264`, `RightSidebar.tsx:89,315-316`,
  `StartAgentDialog.tsx:15`, `MarkdownText.tsx:332`.
- `createMemo` in components: `ChannelView.tsx:223,362,501`,
  `SettingsView.tsx:130`.
- `createResource`: `MarkdownText.tsx:341-` (highlight fetch, keyed on
  `settled()`); v2 reworks async — re-map onto the v2 async primitives at bump
  time.
- `createRoot` as app-lifetime owner: `src/index.tsx:124`,
  `src/boot-fixture.ts:43`; pervasive in tests (`store.test.ts:34,57`,
  `store.live.test.ts` ~20 sites, `live/query.test.ts:62,91,149`,
  `RightSidebar.test.ts:131`, `identity.test.ts:36`,
  `store.ask-race.test.ts:102`).

**Control flow — `Index` removal:**

- `src/components/ChannelView.tsx:96,195` — the only two `<Index>` sites
  (ask questions, message blocks); become `<For keyed={false}>` with accessor
  children ("`keyed={false}` receives an item accessor and a stable numeric
  index … This is the direct `Index` replacement", MIGRATION.md List
  rendering). Both sites already use accessor-call children
  (`q().question`), so the rewrite is near-mechanical.
- `For`/`Show`/`Switch`/`Match` are pervasive (e.g. `AgentView.tsx:112-165`,
  `RightSidebar.tsx:590-643`, `App.tsx:116-122`) and need **nothing**:
  default `For` is signature-stable in v2 — "Default `For` / `keyed={true}`
  receives the raw item and an index accessor: `(item, i)`" (MIGRATION.md
  List rendering), identical to v1. Only the `Index` replacement involves
  accessor children.

**Lifecycle — `onMount` → `onSettled`:**

- `src/components/StartAgentDialog.tsx:20` — `onMount(() =>
  dialogRef?.focus())`. The only production `onMount`.

**Router:**

- `src/mount.tsx:14,44-46` — `HashRouter` with `root={App}` wrapping
  `AppRoutes`; `src/routes.tsx:28` — the shared route table;
  `src/App.tsx:2,36-41` — `useLocation`/`useNavigate` feeding
  `store.bindRouter`; tests use `createMemoryHistory`/`MemoryRouter`
  (`src/test-router.tsx:17,40`, `src/routing.test.tsx:2,125,212`). Router v2
  is `2.0.0-next.16` and adds the `@solidjs/web` peer.

**Store (`solid-js/store` → draft-first setters in v2):**

- `src/components/SettingsView.tsx:2,80-88` — the only `createStore` site:
  `import { createStore, reconcile, unwrap } from "solid-js/store"`; draft
  seeding via `structuredClone(unwrap(...))`, reset via
  `setDraft(reconcile(seed()))`. v2 moves store helpers into `solid-js` and
  makes setters draft-first.

**Virtualizer (blocker dep #1):**

- `src/components/MessageStream.tsx:1,32` — `createVirtualizer` from
  `@tanstack/solid-virtual` (peer caps at solid-js `^1.3.0`);
  `src/components/conv-virtual.ts:52` holds the shared options.

**Markdown (blocker dep #2):**

- `src/components/MarkdownText.tsx:18,455-459` — `SolidMarkdown` with
  `renderingStrategy="reconcile"` renders every message body; solid-markdown
  2.1.1 peers on solid-js `^1.6.0`. The file also reaches into
  `solid-markdown/dist` internals in comments (raw-node handling), so a future
  major of solid-markdown needs its own review, not a blind bump.

**solid-query:**

- `@tanstack/solid-query ^5.101.4` (`package.json:18`) — `QueryClient` /
  `QueryClientProvider` (`src/mount.tsx:15,43`), the bare-`createRoot`
  explicit-client store pattern (`src/live/query.test.ts:19-23`, store §A3).
  v2-compat means TanStack Query v6: a stacked major.

**Tests:**

- `@solidjs/testing-library 0.8.10` `render` across component tests
  (`ChannelView.*.test.tsx`, `MarkdownText.test.tsx`, `routing.test.tsx`,
  `test-router.tsx:18`, more); v2-compat is `1.0.0-beta.2` + `@solidjs/web`.
  The bun test runner + happy-dom harness itself is framework-agnostic.

**Shell seam (Wails — expected unaffected, verify):**

- `src/daemon-transport.ts:192,210-246,283-293` — `Call`/`Events`,
  `wailsShellIpc()`, `nativeConnectionProvider()`. Plain TS, no Solid
  primitives in the seam itself.
- `src/components/MarkdownText.tsx:1` — `import { Browser } from
  "@wailsio/runtime"` (openExternal). Also Solid-free.

Totals: effects 3 · memo/signal/resource/root sites ~25 production (+~30
test `createRoot`s) · `Index` 2 · `onMount` 1 · router 4 files · store 1 file ·
virtualizer 2 files · markdown 1 file · solid-query 3 files · testing-library
~10 test files · shell seam 2 files (Solid-free).

### Slices

**Phase 1 — v1-safe prep (lands on main, one PR per slice):**

- **S1. Effect discipline.** Convert `MarkdownText.tsx:336` — the only
  implicit-dep effect that converts — to `createEffect(on(deps, apply))`;
  audit whether `MarkdownText`'s debounce-effect + `createResource` pair can
  shrink to a derivation. Explicit step, per site: a dep-completeness check —
  converting an implicit-dep effect to `on(deps, apply)` NARROWS the tracked
  set (a behavior-change class, not a pure refactor), and v1's `on` runs its
  apply untracked, so each body must be audited for incidental reactive reads
  beyond the declared deps. Concretely, `MarkdownText.tsx:336` reads
  `props.inline` implicitly (`if (props.inline) return;`) in addition to
  `code()`/`lang()`: include `() => props.inline` in the deps tuple, or
  record the invariance argument (inline is structural, set once per
  instance from the hast node). `src/store.ts:740` is EXCLUDED and stays an
  implicit-tracking effect: its tracked set is deliberately broad and
  dynamic (see the inventory) — narrowing it to `currentPath` alone via
  `on(...)` would stop the post-snapshot re-runs that drive the
  pending-aware deep-link fallbacks, a v1 regression. Solid 2 keeps implicit
  tracking (the compute/apply split does not force explicit deps), so that
  effect ports as-is at bump time.
  - Interfaces: consumes `src/components/MarkdownText.tsx:335-340`
    (`createEffect` + `settled` signal). Produces the same observable
    behavior with explicit deps at that one site; `src/store.ts:740` is
    untouched; no public API change.
  - Verify: full suite + tsc + `vite dev` boot; `MarkdownText.test.tsx`
    debounce/stale-resolution tests green unchanged; `routing.test.tsx:259`
    ("an absent topic deep-link falls back to the channel index after the
    snapshot") green — the sentinel for store.ts:740's broad tracked set.
- **S2. Batching audit.** Grep every synchronous set-then-read within one
  tick (store actions in `createAppStore`, composer flows in
  `ChannelView.tsx`, `SettingsView.tsx` `dirtyTick`); restructure any found
  read-after-set to derive instead of re-read, so v2's deferred visibility
  cannot change behavior.
  - Interfaces: consumes `src/store.ts:680-2057` (`createAppStore` actions),
    `src/components/SettingsView.tsx:79-90` (draft/reset/touch). Produces
    identical accessor semantics; store tests are the contract
    (`store.test.ts`, `store.live.test.ts` unchanged and green).
  - Verify: full suite + tsc + `vite dev` boot.
- **S3. Stale-prose fix.** Replace the "Tauri" comments with Wails:
  `src/App.tsx:23`, `src/stub-data.ts:4,8`, `src/live/client.ts:4`,
  `src/live/connection.ts:11`, `src/markdown/highlighter.ts:5`. Leave
  `daemon-transport.test.ts:14-15` (deliberately says the fake needs no
  Tauri) and the `stub-data.ts:735,783` fixture strings (fake issue/commit
  titles, not claims about this app). A docs-only slice riding along with S1
  or S2.
  - Interfaces: comments only; zero behavior.
  - Verify: tsc + suite (unchanged).

**Phase 2 — the v2 bump (migration branch; merges only when the Global
Constraints gate clears):**

- **S4. Core bump + router + testing-library (one atomic slice).**
  solid-js 2 + `@solidjs/web` + babel-preset-solid 2 + vite-plugin-solid 3 +
  `@solidjs/router` 2 + `@solidjs/testing-library` 1 — atomic because
  testing-library 0.8.10 cannot render against solid-js 2, and router v2
  shares the new `@solidjs/web` peer; splitting them would strand a
  non-green interior slice, violating the every-slice-green rule. Codemods:
  `Index`→`<For keyed={false}>` (`ChannelView.tsx:96,195`);
  `onMount`→`onSettled` (`StartAgentDialog.tsx:20`); `jsxImportSource`
  `"solid-js"`→`"@solidjs/web"` in `apps/ui/tsconfig.json:12` ("web projects
  should set `jsxImportSource` to `@solidjs/web`; `solid-js` no longer owns
  JSX runtime types", MIGRATION.md checklist) — without it tsc cannot be
  clean; moved imports (store helpers into `solid-js` —
  `SettingsView.tsx:2`) including the `unwrap`→`snapshot` rename
  ("`snapshot(store)` replaces `unwrap(store)`", MIGRATION.md); the
  `on()`-removal rewrite of the two effect sites carrying `on()` after S1
  (`MessageStream.tsx:45`, `MarkdownText.tsx` post-S1) onto split
  `createEffect(deps, apply)` — `store.ts:740` never gets `on()` and ports
  as-is (v2 keeps implicit tracking); re-map `createResource`
  (`MarkdownText.tsx:341`) onto v2 async; draft-first store setters in
  `SettingsView`. Router surfaces re-verified: `HashRouter`/`MemoryRouter`/
  `createMemoryHistory` (`mount.tsx:44`, `test-router.tsx:17`) and the
  `bindRouter` seam (`App.tsx:38-41`, `store.ts:740`).
  - Interfaces: consumes `package.json` dep block, `tsconfig.json:12`, every
    inventory site above, `src/mount.tsx`, `src/routes.tsx`, `src/App.tsx`,
    `src/test-router.tsx`, `src/routing.test.tsx`. Produces a tree that
    type-checks and tests green against solid-js 2; route table + store
    router seam signatures (`bindRouter({navigate, currentPath})`)
    unchanged.
  - Verify: tsc clean; full suite green (testing-library 1 restores
    runnability); `routing.test.tsx` + `App.test.tsx` green; boot.
- **S5. solid-query v5→v6.** Its own slice — TanStack Query v6 API changes
  on top of the reactivity migration; re-prove the bare-`createRoot`
  explicit-client pattern (`live/query.test.ts:80-99`).
  - Interfaces: consumes `src/mount.tsx:15,43`, `src/live/query.ts` (the
    `createConnectQuery`/`createConnectInfiniteQuery` glue),
    `src/live/query.test.ts`. Produces the same glue API to the store.
  - Verify: `live/query.test.ts` green; full suite; boot.
- **S6. Blocker deps (forks, per Resolved decision 2).**
  `@tanstack/solid-virtual`: consume the RigelBuild fork through a
  `package.json` git ref (`github:RigelBuild/virtual`, fork branch) carrying
  Solid-2 support while the upstream PR to TanStack/virtual is open; if it
  merges, swap to the upstream release and drop the git ref. The adapter is a
  136-line thin wrapper over the framework-agnostic `@tanstack/virtual-core`
  (TanStack/virtual `packages/solid-virtual/src/index.tsx:1-136`), so the v2
  change is small: bump the `solid-js` peer and swap the Solid-1 bindings to
  Solid-2 primitives. `solid-markdown`: publish the owned RigelBuild fork as a
  scoped `@rigelbuild/solid-markdown` package and depend on the published
  version — peer bumped to solid-js 2.x plus the Solid-2 codemods (imports and
  effect lifecycle, no algorithmic rewrite: `solid-js/web`→`@solidjs/web` for
  `Dynamic`, `solid-js/store`→`solid-js` for `createStore`/`reconcile`,
  `mergeProps`→`merge`, `createRenderEffect` per the v2 split; ~668 lines /
  5 files). Re-run the virtualizer geometry suite
  (`ChannelView.scroll.test.tsx`) and the markdown raw-node/highlight
  suite (`MarkdownText.test.tsx`) — both encode dep-internal behavior and
  will catch a changed contract.
  - Interfaces: consumes `src/components/MessageStream.tsx:32`,
    `src/components/conv-virtual.ts`, `src/components/MarkdownText.tsx:455`.
    Produces unchanged component props/contracts; only the two package
    specifiers in `package.json` change — a git ref for the virtualizer, the
    `@rigelbuild/solid-markdown` published package for the markdown renderer.
  - Verify: those two suites + full suite + tsc + boot.
- **S7. Shell-seam smoke + merge.** Runtime-smoke the packaged Wails shell
  under Solid 2 (`daemon-transport.ts`, `MarkdownText.tsx:1`): the packaged
  shell runs the vite production build — the first place the
  babel-preset-solid 2 / vite-plugin-solid 3 / `@solidjs/web` production
  compilation and v2's render-root-owned delegated events run outside
  dev/happy-dom. Final rebase; merge to main.
  - Interfaces: consumes the whole branch. Produces the merged migration.
  - Verify: full suite + tsc + `vite dev` boot + a manual Wails-shell run
    (RPC round-trip + openExternal).

## Tasks

Phase 1 (main, now):

- [ ] S1 — effect discipline: explicit-dep `on(...)` shape for
      `MarkdownText.tsx:336` ONLY (`store.ts:740` stays implicit-tracking);
      audit the debounce/resource pair; `routing.test.tsx` topic-fallback
      test green
- [ ] S2 — batching audit: no read-after-set within a tick in store actions,
      composer flows, SettingsView draft
- [ ] S3 — stale "Tauri" prose → Wails

Phase 2 (migration branch, gated on the readiness table):

- [ ] S4 — solid-js 2 + `@solidjs/web` + babel-preset-solid 2 +
      vite-plugin-solid 3 + router 2 + testing-library 1 (atomic);
      `Index`/`onMount`/`jsxImportSource`/store-import+`snapshot` codemods;
      `on()`-removal at the two `on()` sites (`MessageStream.tsx:45`,
      `MarkdownText.tsx`; `store.ts:740` ports as-is); `createResource`
      re-map; route table + `bindRouter` seam re-verified
- [ ] S5 — solid-query v6 (own slice); bare-root explicit-client pattern
      re-proven
- [ ] S6 — blocker deps via forks: `@tanstack/solid-virtual` fork consumed +
      upstream PR to TanStack/virtual; `solid-markdown` RigelBuild fork
      (owned); geometry + markdown suites green
- [ ] S7 — Wails packaged-shell smoke; rebase; merge

Every task: full `bun test --conditions browser src/` green, tsc clean,
boots in `vite dev`.

## Resolved decisions

All three ruled by Matt at the design-PR gate.

> **Ledger note (2026-08-19):** decision 2's `solid-markdown` clause below
> (codemod-only fork, no in-repo AST renderer, keep `SolidMarkdown`) is
> **superseded by DL-218** ([markdown react10](../compass-ui-markdown-react10/design.md)):
> the owned fork now adopts upstream #44's react-markdown-10 rewrite +
> `hast-util-to-jsx-runtime` renderer re-ported to Solid 2, published as
> `@rigelbuild/solid-markdown@3.0.0`. Decision 2's `@tanstack/solid-virtual`
> clause and the fork-externalization stance are unaffected.

1. **Build the migration branch NOW against `2.0.0-rc.0` with branch-only
   peer overrides; the RIG-2186 greenfield starts directly on Solid v2**
   (Matt, 2026-08-18). Background: the blocker deps gate the *merge*
   regardless of RC vs GA — gate clause (a) binds independently of (b) — so
   RC-vs-GA was nearly moot for merge timing; the real fork was build-now vs
   build-lazily. Ruling: build now — early signal de-risks the eventual
   merge; branch upkeep is the accepted cost (arm 3's restated cost). And
   the RIG-2186 accounts-login greenfield is a NEW surface with no blocker
   deps (it need not use solid-virtual/solid-markdown), so it starts
   directly on Solid v2 while apps/ui follows the incremental branch — a
   cross-issue decision owned by RIG-2186 planning, recorded here for
   context only.
2. **Fork both blocker deps** (Matt, 2026-08-18) — overriding this record's
   earlier adapter-over-core recommendation for the virtualizer.
   `@tanstack/solid-virtual`: fork, add Solid-2 support, and open a PR
   upstream to TanStack/virtual — the monorepo is active (last commit +
   release 2026-08-18) and the adapter is a 136-line thin wrapper over the
   framework-agnostic `@tanstack/virtual-core` (TanStack/virtual
   `packages/solid-virtual/src/index.tsx:1-136`), so an upstream PR bumping
   the adapter's `solid-js` peer and swapping the Solid-1 bindings to
   Solid-2 primitives is likely to land; consume our fork until it merges,
   then drop the fork. Consume it through a `package.json` git ref to the
   RigelBuild fork branch (`github:RigelBuild/virtual`), not a published
   package — it is temporary and drops out when upstream lands.
   `solid-markdown`: fork into the RigelBuild org and
   OWN it — effectively abandoned (last real release 2023-10-31; the
   2025-11-29 commit was a dep bump; single maintainer quiet since; 2 stale
   open PRs). Bump its `solid-js` peer to 2.x and apply the Solid-2 codemods
   (imports + effect lifecycle, ~668 lines / 5 files); NO in-repo direct-AST
   renderer — we would end up rebuilding a SolidMarkdown component
   regardless. The coupling motivating the fork is behavioral, not a
   code-level `dist` import: `MarkdownText.tsx:24-28` documents
   solid-markdown's rehype raw-node pipeline in comments, while the code
   uses the public `SolidMarkdown` component plus a local
   `rehypeInertRawAndBreaks` plugin working around that handling. Publish the
   owned fork as a scoped `@rigelbuild/solid-markdown` package and depend on
   the published version. Neither fork is vendored into the compass tree:
   fork development lives in the public RigelBuild repos on GitHub Actions
   CI, and compass consumes each as a pinned artifact (git ref / published
   package) — matching the org-wide fork-externalization ruling (Matt,
   2026-08-18; orion #1457), which reverses the earlier vendor-into-tree
   consolidation.
3. **Wails-shell risk: low-risk-but-verified stands** (Matt, 2026-08-18; no
   fork — the record's recommendation is the decision). The seam is
   Solid-free plain TS (`daemon-transport.ts` — `Call`/`Events` string-name
   invocations; `MarkdownText.tsx:1` — `Browser.openExternal`), so
   effect-flush timing is the LESSER risk; the packaged shell is the first
   place the babel-preset-solid 2 / vite-plugin-solid 3 / `@solidjs/web`
   PRODUCTION-BUILD compilation and v2's render-root-owned delegated events
   ("Delegated events are now owned by render roots", MIGRATION.md) run
   outside dev/happy-dom. S7's manual packaged-shell smoke (RPC round-trip +
   openExternal) stays the mandatory gate; no automation is justified for a
   one-shot migration.
