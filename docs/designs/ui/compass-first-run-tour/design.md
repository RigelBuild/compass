# Compass native first-run product tour (RIG-2797)

Parent: the Compass onboarding/discoverability net. The coaching-tooltips
record explicitly deferred "a first-run coach" to a future record
(`compass-coaching-tooltips/design.md` §Deferred — "The wider onboarding
affordances — empty-state keyboard nudges, a persistent key-hint footer, a
first-run coach — are explicitly deferred by Matt"); this is that record.
Consumes (does not build) the PostHog embed seam of the observability record
(PR #656 T6, `docs/designs/platform/compass-observability-architecture/`).

## Problem / Intent

A first launch of Compass drops the user cold onto the Bridge with zero
orientation: nothing introduces the board, the agent tree, the comms surfaces,
or the keyboard-first posture the whole UX is built around. Ship a first-run
product tour built **natively in SolidJS v2** — real components anchored to the
app's real chrome, driven by the app's own router and store, entering on the
brand chase-light motion — that wows on first boot, is skippable and
replayable, and (once the analytics embed exists) reports its funnel through
headless PostHog capture. No PostHog-rendered UI ships in-app, ever
(Matt's RIG-2793 ruling — the tour split off that thread — restated by
PR #656 T6: "no PostHog-rendered UI ships in the product").

## Approach

One store-owned tour controller plus one App-root overlay component: the tour
is a sequence of **steps**, each either a centered dialog (welcome / finale) or
a **callout anchored to a real UI element**, advancing through the app's real
navigation so the user watches the actual product move — the tour IS the app,
never a screenshot-overlay fighting it.

### A1 — Host shape: an App-root overlay layer, store-gated

The tour mounts exactly where the app's other transient layers do. `App` is the
router's always-mounted root layout (`mount.tsx:44-50` — `createRouter({
routes: appRoutes, history: hashHistory() })` with `<Router>{(props) => <App
{...props} />}</Router>`), and it already hosts the shortcuts overlay and the
palette behind store signals (`App.tsx:185-191`):

> ```tsx
> <Show when={store.shortcutsOpen()}>
>   <ShortcutsOverlay />
> </Show>
>
> <Show when={store.paletteOpen()}>
>   <Palette />
> </Show>
> ```

`TourOverlay` is a third sibling behind `store.tour.open()`. Because App wraps
every route, the overlay survives the route changes the tour itself performs —
a step can navigate to `/backlog` and keep narrating. The controller state
(current step index, open flag) lives in the store beside the sibling overlay
signals (`store.ts:268-273` — `shortcutsOpen` / `hideShortcuts` /
`toggleShortcuts` is the established shape), built in `createAppStore` where
the navigation closures it drives already exist (`store.ts:1950-1965` —
`showBridge`/`showBacklog`/`showDone`/`showSettings` each `hideShortcuts()`
then `navigateTo(...)`).

### A2 — Step model: declarative steps over real anchors

A step is data, not a component:

```ts
interface TourStep {
  /** Stable id — the analytics event dimension and the resume cursor. */
  readonly id: string;
  /** Centered dialog (welcome/finale) or anchored callout. */
  readonly kind: "dialog" | "callout";
  /** For callouts: the `data-tour` anchor value to attach to. */
  readonly anchor?: string;
  /** View the step needs; controller navigates via the store closure on
   *  entry. Only the four closure-backed static views (A1); parameterized
   *  surfaces (agent workspace) are reached by anchor, never navigated. */
  readonly route?: "/" | "/backlog" | "/done" | "/settings";
  readonly title: string;
  readonly body: string;
}
```

Anchoring is by **`data-tour="<anchor>"` attributes on the real elements** —
e.g. the LeftSidebar Bridge button (`LeftSidebar.tsx:442-454`, the
`CoachTipTrigger as="button" class={["bridge-link", …]}` view buttons), the
topbar view-tabs nav (`App.tsx:86` — `<nav class="view-tabs"
aria-label="View">`), the board grid, the right sidebar. Steps that need a
surface navigate first through the store's own closures (A1), so the router
stays the single navigation writer (`App.tsx:51-54` — `store.bindRouter`
feeds `useNavigate()`; the store's `navigateTo` is the one mutation path).
Navigation is **asynchronous** — `navigateTo` sets the pending route, the
single-writer route-sync effect then applies it and the routed surface mounts
into `<main>` on a later tick (`store.ts:747-748` sets the pending route;
the effect at `store.ts:771-806` applies it, held until `firstSnapshotArrived`
at `store.ts:775-780`). So the controller does **not** resolve the anchor
synchronously after navigating: it resolves reactively once the target route
is active, retrying `document.querySelector('[data-tour="<anchor>"]')` across
a bounded window (microtask/rAF retries or a `MutationObserver` with a
timeout). Only after that bounded wait does a still-missing anchor (surface
genuinely absent, feature-flagged away) **skip the step** rather than error —
graceful drift-tolerance, not a swallowed race (B1; the aggregate
anchor-existence contract test in T2 guards against silent whole-tour decay).

### A3 — Callout substrate: Kobalte Popover with an external `anchorRef`

Step callouts ride the installed Kobalte v2-alpha **Popover**: its root options
carry exactly the two capabilities a tour needs — a controlled `open` and an
anchor that is NOT its own trigger child
(`apps/ui/node_modules/@kobalte/core/dist/index/QQ67U6Bm.d.ts:139-147`):

> ```ts
> interface PopoverRootOptions extends Omit<PopperRootOptions, …> {
>   /** A ref for the anchor element.
>    * Useful if you want to use an element outside `Popover` as the popover anchor. */
>   anchorRef?: Accessor<HTMLElement | undefined>;
>   /** The controlled open state of the popover. */
>   open?: boolean;
> ```

so the callout anchors to the resolved `data-tour` element without wrapping it.
`modal` stays false: positioning, portal stacking, flip/shift on viewport
edges, and dismissal are the a11y-hard behaviors DL-150 scopes Kobalte to (the
CoachTip precedent: `CoachTip.tsx:2-4` — "Built on the Kobalte v2-alpha
`Tooltip` primitive (a11y-hard behavior … DL-150)"). The visual box is a new
`.cx-tour-callout` class in the house component-CSS convention (elev float
like `.cx-tooltip`, consuming only `--cx-*` tokens per DL-154's stylelint
guard).

The **welcome and finale dialogs** are hand-rolled on the `.cx-dialog`
convention instead — the DL-230-ratified pattern for modal chrome
(`ShortcutsOverlay.tsx:6-10` — "Hand-rolled modal on the `.cx-dialog`
convention (ratified D5 — no @kobalte/core), with … focus-RESTORE … and a
minimal Tab/Shift+Tab focus TRAP"), reusing its focus-capture/restore shape
(`ShortcutsOverlay.tsx:50-58`). Two substrates, both already ratified: Kobalte
where anchored-positioning a11y is the hard part, `.cx-dialog` where modal
chrome is (same split DL-230/DL-245 drew between the overlay and CoachTip).

### A4 — Sequencing, skip, resume

- **Advance/back**: Next/Back buttons in the callout footer plus
  `ArrowRight`/`ArrowLeft`. Keys are **component-local**, never a second
  window keymap listener (DL-223's one-listener law): the callout footer holds
  focus, so its own handlers fire. Mid-step interaction with the live app is
  pointer-driven (the non-`modal` Popover does not trap focus).
- **Skip tour on every step**: each dialog and callout carries an explicit
  **Skip tour** button (Matt, OQ-4). Skip, `Escape`, and outside-click all
  route to one path, `store.tour.dismiss()`: the Popover surfaces `Escape` and
  outside-click through `onOpenChange`
  (`apps/ui/node_modules/@kobalte/core/dist/index/QQ67U6Bm.d.ts:150-152`), and
  the controller sends `onOpenChange(false)` to `dismiss()`, so the controlled
  `open` never desyncs from Kobalte's own dismissal. The welcome and finale
  `.cx-dialog` steps trap focus and close only by their own controls
  (Skip / Start / Done) or `Escape`.
- **Dismiss and complete persist per account** (A5): dismissal at step *k*
  records `{ outcome: dismissed, step: k }`, completion records
  `{ outcome: completed }`. Neither ever auto-reopens.
- **Resume**: a replay from a dismissed state offers "resume from step *k*"
  with restart available.
- **Replay affordance**: a `tour.start` command registered in the keyboard
  spine beside the view commands (`spine.ts:89-96` — the `view.shortcuts`
  registration is the shape: `{ id, title, keywords, scope: "global", run }`),
  so the palette's action mode lists it (DL-229). No default chord (Matt,
  OQ-4).

### A5 — First-run detection + persistence: per-account server state

Matt's ruling (OQ-2): a person must never get the tour twice. So "seen" state
lives **on the server, per account**, not in browser storage. Browser storage
is per device and per URL, and `workspaceKey` is
`` `${connection.baseUrl}#${callerId}` `` (`index.tsx:134`), so a LAN IP, a
tailnet host, and a second device would each re-arm the tour.

**Server.** No preference or UI-state storage exists today (no table in
`go/internal/store/migrations/`, no RPC in `proto/compass/v1/`). Add the
smallest one:

- Migration `0008_account_tour_state.sql` (`0007` is taken by the forge
  scope store; take the next free number at implementation — migrations are
  append-only). Table `account_tour_state`: `tenant_id` defaulted from
  `current_setting('compass.tenant_id', TRUE)` like every tenant-owned table,
  `account_id`, `outcome` (`started` / `dismissed` / `completed`), `step_id`
  (resume cursor, nullable), `updated_at`; primary key
  `(tenant_id, account_id)`. ENABLE + FORCE row-level security with the same
  `tenant_isolation` policy shape as `0001_init.sql` (non-empty GUC and tenant
  equality in USING and WITH CHECK).
- Two RPCs on `CompassService`, beside `WhoAmI`: `GetTourState` and
  `SetTourState`. Both key on `auth.CallerFrom(ctx)` — the `WhoAmI` handler's
  pattern (`go/server/service.go`, "never a client-supplied field") — and fail
  closed with `Unauthenticated` without a caller. The request carries no
  account id. `SetTourState` is an idempotent upsert. No row means unseen.

**Write timing.** The UI writes `started` (with the step id) **when the tour
opens**, before the user sees step 1. Closing the app mid-tour therefore
counts as seen. Step changes update `step_id` best-effort; skip and finish
write `dismissed` / `completed`. Writes never block the UI; a failed write is
logged and not retried in a loop.

**Auto-start rule.** The tour auto-opens only when `GetTourState` succeeds and
returns no row. A failed or slow read arms nothing: a missed tour can be
replayed from the palette, a repeated one cannot be undone. Replay via
`tour.start` (A4) always works.

**Offline fixture build.** `boot-fixture.ts` has no server. Tour state there is
an in-memory signal, so a fixture page load can show the tour once per load.
This is a dev and demo build, not an account, so the rule above does not apply
to it.

### A6 — Entrance: the chase-light welcome, reduced-motion by token

The welcome step is the tour's brand moment, built from the shipped chase-light
vocabulary — no new motion primitive and no client animation runtime
(`motion.md:14-16` — "pure CSS/SVG — no client-side animation runtime"):

- The welcome dialog's frame draws in as a **perimeter chase** in the
  chase-light *vocabulary* — discrete cells lit in sequence by a phase-offset
  `steps(1, end)` keyframe over `--cx-pulse-period`. It reuses the loader's
  vocabulary, **not its keyframe**: the shipped loader
  (`apps/ui/src/design/components/loader.css:70-74` — `animation:
  cx-loader-chase var(--cx-pulse-period) steps(1, end) infinite;
  animation-delay: calc(var(--cx-pulse-period) * (var(--i, 0) / 24 - 1))`)
  uses **negative** delays that only resolve under `infinite` looping — under
  `iteration-count: 1` the first cell is already elapsed at t=0 and the rest
  play tail fractions, a broken flicker. The finite entrance therefore needs a
  **new one-shot variant**: positive per-cell delays (`--cx-pulse-period *
  var(--i) / 24`) plus `animation-fill-mode: both`, run once — so it echoes the
  boot-sequence "powering on" choreography (`motion.md:180-191`) without
  spending the viewport's unbounded-pulse budget (`motion.md:39-41`). Its lit
  cell reuses the loader's sanctioned phosphor purple (`loader.css:56-62`) — the
  entrance must not introduce a second purple mark (DL-155). T5 documents the
  one-shot variant in `motion.md`.
- Step-to-step callout movement is everyday translate + fade at
  `--cx-motion-base` with `--cx-ease-out` (`motion.md:161-164`); durations are
  tokens, never literals (`motion.md:20-30` — "A literal `200ms` … is a review
  failure").
- **Reduced-motion is automatic**: `tokens.css:241-257` zeroes
  `--cx-motion-fast/base`, `--rigel-motion-slow`, and `--rigel-pulse-period`
  under both `prefers-reduced-motion: reduce` and `[data-reduce="on"]`, so the
  entrance collapses to an instant final state (substitution, not removal —
  `motion.md:42-49`). The tour's meaning is fully carried by text; motion never
  sole-carries it.

### A7 — Analytics: a thin no-op-safe indirection over the T6 embed

The tour never imports `posthog-js`. A tiny module,
`apps/ui/src/tour/analytics.ts`, exposes `captureTourEvent(event: TourEvent)`
(the T4 union below) and resolves the PR #656 T6 embed **at call time**: when
the analytics enable flag is off, every call is a silent no-op and the tour is
fully functional un-instrumented. (The embed is *present* by the time any
capture ships — T4 sequences after T6, and a statically-bundled build cannot
soft-import an absent module — so flag-off is the only live no-op path; OTel
`trace_id` stamping is the embed's own concern per the obs record's J1, not
this indirection's.) This satisfies the T6
contract ("embed `posthog-js` behind an off-by-default enable flag +
configurable host"; "PostHog contributes only headless data — event capture,
and flag/early-access-feature JSON payloads … never a PostHog widget" — PR
656 T6) while keeping the dependency one-directional: the tour's UI tasks
(T2/T3/T5 below) have zero dependency on T6; only the instrumentation task
(T4) sequences after it. Events: `tour_started` (with `trigger: "first-run" |
"replay" | "resume"`), `tour_step_viewed` (`step_id`, `index`),
`tour_dismissed` (`step_id`), `tour_completed`.

### A8 — Relationship to the existing discoverability net

The tour does not restate what the shipped net teaches. CoachTip owns
point-of-use chord coaching (`CoachTip.tsx:1-8`), the `?` overlay owns the full
keymap reference (`ShortcutsOverlay.tsx:1-4`), the palette owns
action/navigation search (`Palette.tsx:2-5`). The tour's job is orientation:
it points **at** those surfaces — the finale step teaches "press `Mod+K` for
the palette, `?` for shortcuts, hover anything for its chord" and hands off —
rather than re-teaching individual chords. Step copy therefore names the
surfaces, and any displayed chord resolves through `shortcutFor` (DL-234's
single-derivation rule, `CoachTip.tsx:5-6` — "never hand-authored"), reusing
`ShortcutChip`/CoachTip rendering conventions.

### A9 — Remote content: static steps day-1

Step definitions ship **static, in-code** (a `TOUR_STEPS: readonly TourStep[]`
table; Matt, OQ-1). The headless remote-content path T6 names
(`getFeatureFlagPayload` / `getEarlyAccessFeatures` JSON rendered by our own
component) is not adopted day-1: it would make first-run content depend on an
off-by-default network SDK (A7). Because steps are data (A2), a later remote
override is a data-source swap behind the same `TourStep[]` type.

### A10 — Demo agents and content

A fresh workspace has no agents and an empty board, so the tour would point at
empty chrome (Matt, OQ-3: "otherwise it's hard to understand"). While the tour
is open, the store shows a small **demo dataset**: two or three demo agents
with presence, a handful of demo issues across board columns, and one demo
channel with a few messages. Data lives in a new `apps/ui/src/tour/demo.ts`,
shaped like the fixture data in `stub-data.ts` / `comms-stub.ts`.

- **One seam, in the store.** The store's existing read accessors — `agents()`,
  `issues()`, `channels()`, `messages()` (`store.ts`, `AppStore` interface) —
  return real rows plus demo rows while `tour.demoActive()` is true. Every
  surface (LeftSidebar tree, board, RightSidebar, AgentView) already reads
  through these accessors, so no component gets a second data path.
- **Demo rows are visibly marked.** Every demo id has the `demo:` prefix, and
  rows render a "Demo" badge.
- **Demo rows never reach the server.** Every store mutation closure (move
  issue, send message, start session, and the rest) returns early on a
  `demo:` id. Demo rows are never written to the query cache or the stream
  state; the seam adds them at read time only.
- **Teardown.** Skip, finish, and any dismiss clear `demoActive`, so the demo
  rows disappear in the same tick.
- **Real data stays visible.** On a replay in a busy workspace, demo rows sit
  beside real ones instead of hiding them.
- **Agent-workspace beat.** It anchors to a demo agent's row in the agent tree,
  so it no longer skips itself on an empty workspace. The route union (A2)
  stays the four static views.

Risk: a missed mutation guard would send a `demo:` id to the server. The
server rejects unknown ids, but T2 adds a test that runs every mutation
closure with a `demo:` id and asserts no client call.

### A11 — Faint spotlight on callout steps

Callout steps dim the app faintly and leave a clear cutout over the anchor
(Matt, OQ-5: "faint spotlight").

- A fixed full-viewport `.cx-tour-spotlight` layer below the callout. Its fill
  is a new token `--cx-tour-scrim`, much lighter than the dialog
  `--cx-scrim` (`design/tokens.css`), so the live app stays readable.
- The cutout is a CSS `mask` (a radial or rounded-rect transparent region)
  positioned from the anchor's `getBoundingClientRect()` plus padding. It is
  recomputed on step change, resize, and scroll (one rAF-throttled
  listener, removed on close).
- The layer is `pointer-events: none`, so it never blocks clicks on the anchor
  or the app.
- Cutout moves use `--cx-motion-base`; under reduced motion the token is zero,
  so the cutout jumps.
- Dialog steps (welcome, finale) keep the normal `.cx-dialog-backdrop`.

Cost: one more fixed layer and one resize/scroll listener while the tour is
open. No mask or clip-path exists in the UI yet, so T6 adds a short note to
`components.md`.

## Alternatives considered

### PostHog Product Tours (rejected — hard rule)

PostHog ships hosted Product Tours / surveys / announcement banners rendered by
`posthog-js` widgets. Rejected outright, and not on taste alone: Matt ruled it
("no PostHog UI elements in our own app; they would look off and are not
Solid" — PR #656 T6, which also names the first-run tour explicitly: "Any
in-app engagement surface (first-run tour, changelog/announcement banner) is
built natively in Solid"). Concretely: a PostHog widget is a generic DOM
overlay injected outside the Solid tree — it cannot anchor through our
reactive store, cannot drive the router, cannot consume `--cx-*` tokens or the
chase-light primitive, ignores `[data-reduce="on"]`, and would ship UI from a
network SDK that is **off by default** on self-hosted deploys (T6), i.e. the
tour would simply not exist for most self-hosters. PostHog stays
measurement-only (A7).

### Route-driven tour (`/welcome` route or `?tour=` param)

A dedicated route or query param carrying tour state. Rejected: the tour must
*itself* navigate the real routes (`routes.tsx:38-47` — the seven view routes
the steps walk), so encoding the tour in the route makes every step a
double-navigation and collides with the store's single-writer route sync
(`App.tsx:46` — "the single-writer route-sync effect (store.ts applyRoute)").
A `/welcome`-style full-screen route also contradicts the premise: the tour
overlays the live app, it is not a separate surface. The catch-all already
redirects unknown paths home (`routes.tsx:29-33`), so a stale `?tour` deep
link would add redirect edge cases for nothing. The App-root overlay (A1) gets
route-survival for free.

### Hand-rolled callout anchoring

Position step callouts with our own `getBoundingClientRect` + scroll/resize
listeners. Rejected for the callouts: anchored-popper behavior (portal
stacking, viewport flip/shift, anchor tracking across layout shifts) is
exactly DL-150's "a11y-hard behavior" Kobalte scope, the same reasoning that
chose Kobalte Tooltip for CoachTip over the hand-rolled path
(coaching-tooltips record §Alternatives: "Re-deriving that behavior by hand is
a second convention beside a ratified one"). The installed alpha's Popover
exposes the external-`anchorRef` controlled shape the tour needs verbatim
(`apps/ui/node_modules/@kobalte/core/dist/index/QQ67U6Bm.d.ts:139-147`, A3).
Kept hand-rolled: the two **modal** steps
(welcome/finale), where `.cx-dialog` is the ratified convention (DL-230) and
the hard parts (focus trap/restore) are already solved in-tree
(`ShortcutsOverlay.tsx:50-58`).

### "Seen" state: browser storage

Store the "seen" flag in `localStorage` with the pin-set pattern
(`safeLocalStorage`, `store.ts:650-656`). Rejected (Matt, OQ-2): storage is per
device and per URL, so the same person would get the tour again on a second
device, a second URL for the same server, or a cleared browser. Per-account
server state (A5) costs one small table and two RPCs.

### One Kobalte substrate for everything (Dialog for modal steps too)

Using Kobalte `Dialog` (installed: `dist/dialog/`) for welcome/finale instead
of `.cx-dialog`. Rejected: DL-230 already ruled modal chrome hand-rolled
("Kobalte reserved for load-bearing a11y (the palette combobox)"), and a
second modal convention beside the shipped ShortcutsOverlay pattern is exactly
the second-convention smell. The a11y-hard piece of the tour is anchored
positioning, and only the Popover carries that.

## Global Constraints

- **No PostHog-rendered UI, ever (hard rule, Matt).** PostHog is
  measurement/data-only: `posthog-js` event capture, plus optional headless
  flag/EAF JSON payloads rendered by our own Solid components. No PostHog
  Product Tours, surveys, banners, or any PostHog-rendered widget (PR #656
  T6). Any violation is a review failure, not a judgment call.
- **Flag-off no-op.** All tour analytics route through the A7 indirection;
  with the analytics enable flag off, every capture call is a silent no-op and
  the tour is fully functional un-instrumented. No hard `posthog-js` import
  anywhere in tour code (the module resolves the embed at call time, so it is
  a defensive guard, not a hard dependency). Sequencing: only the
  instrumentation task (T3) waits on T6, and by then the embed is present — so
  flag-off is the only live no-op path; the tour UI does not wait on T6.
- **SolidJS v2** (`apps/ui/package.json:26` — `"solid-js": "^2.0.0-rc.1"`).
  No v1 idioms; component props are NEVER destructured (accessors / thunked
  derivation, the `CoachTip.tsx:49-53` shape). Router is `@solidjs/router`
  `^2.0.0-next.17` (`package.json:18`), config-based; navigation only through
  the store's closures (single-writer route sync, `App.tsx:46/51-54`).
- **Kobalte `2.0.0-alpha.0`** (`package.json:15`), scoped per DL-150 to
  a11y-hard behavior: the tour uses `Popover` (external `anchorRef`,
  controlled `open` —
  `apps/ui/node_modules/@kobalte/core/dist/index/QQ67U6Bm.d.ts:139-147`) for
  callouts only; modal steps ride the `.cx-dialog` hand-rolled convention
  (DL-230, `ShortcutsOverlay.tsx:6-10`). All visuals via `.cx-*` classes.
- **Motion**: chase-light vocabulary only, pure CSS/SVG, no client animation
  runtime (`motion.md:14-16`); every duration/easing a `--cx-*` token — a
  literal duration is a review failure (`motion.md:20-30`); at most one
  unbounded pulse per viewport region (`motion.md:39-41`), so the tour's
  entrance chase runs finite iterations. **Reduced-motion gate**: the tour
  MUST fully degrade under `prefers-reduced-motion: reduce` and
  `[data-reduce="on"]` — automatic via the token zeroing
  (`tokens.css:241-257`); any tour keyframe not driven by a zeroed token needs
  an explicit substitution rule (`motion.md:42-49`).
- **Persistence**: tour state only through `GetTourState` / `SetTourState`
  (A5), keyed on the server-side caller. No client-supplied account id. The
  tour auto-opens only on a successful read with no row. No tour state in
  `localStorage`.
- **Server**: the new migration is append-only and tenant-scoped with ENABLE +
  FORCE row-level security like its neighbors; a cross-tenant pgtest proves
  isolation.
- **Demo data** (A10): read-time only, `demo:`-prefixed, never sent to the
  server, gone on close.
- **Keyboard**: no second window keydown listener (DL-223 — one `installKeymap`
  listener, `App.tsx:59-65`); tour-local keys are component-scoped handlers
  (the ShortcutsOverlay pattern). The replay command `tour.start` registers in
  the spine beside its behavior (DL-229; shape per `spine.ts:89-96`). Any
  displayed chord resolves via `shortcutFor` — never hand-authored (DL-234).
- **Chrome coordination**: the tour orients and hands off to CoachTip / the
  `?` overlay / the palette (A8); it never duplicates their teaching.
- **Tooling**: TS `strict: true`; Biome 2.5.4 (tabs); stylelint from
  `apps/ui` (DL-154 token guard); tests `cd apps/ui && bun test --conditions
  browser <files>` with `@solidjs/testing-library`; red → green per
  `rule://red-green-testing`; markdownlint on docs.
- **Ledger**: rows DL-272..275 (reserved for this record), updated in place;
  no row superseded.

## Plan

Dependency order: T1 → T2 → T3 → (T4 ∥ T5) → T6. T1–T3, T5, and T6 have **no**
dependency on PR #656; **T4 alone sequences after the #656 T6 embed lands**.

### T1 — Server: per-account tour state

Migration `0008_account_tour_state.sql`, sqlc queries in
`go/internal/store/queries/`, and `GetTourState` / `SetTourState` on
`CompassService` (A5). Regenerate the TS client.

Interfaces:

```proto
// proto/compass/v1/compass.proto, beside WhoAmI
rpc GetTourState(GetTourStateRequest) returns (GetTourStateResponse);
rpc SetTourState(SetTourStateRequest) returns (SetTourStateResponse);

enum TourOutcome {
  TOUR_OUTCOME_UNSPECIFIED = 0; // no row: never seen
  TOUR_OUTCOME_STARTED = 1;
  TOUR_OUTCOME_DISMISSED = 2;
  TOUR_OUTCOME_COMPLETED = 3;
}
message GetTourStateRequest {}
message GetTourStateResponse {
  TourOutcome outcome = 1;
  string step_id = 2; // resume cursor; empty when none
}
message SetTourStateRequest {
  TourOutcome outcome = 1; // UNSPECIFIED is InvalidArgument
  string step_id = 2;
}
message SetTourStateResponse {}
```

Red → green (pgtests): no row reads `UNSPECIFIED`; set then get round-trips;
a second set overwrites; a caller in tenant A cannot read tenant B's row;
no caller fails `Unauthenticated`; `UNSPECIFIED` on set is
`InvalidArgument`.

### T2 — Tour state + demo seam in the store

New `apps/ui/src/tour/state.ts` (pure), `apps/ui/src/tour/demo.ts` (A10), and
store wiring beside the sibling overlay signals (`store.ts:1942-1965`).

```ts
// apps/ui/src/tour/state.ts (pure — no DOM, no store import)
export interface TourStep {
  readonly id: string;
  readonly kind: "dialog" | "callout";
  readonly anchor?: string; // data-tour value, callout steps only
  readonly route?: "/" | "/backlog" | "/done" | "/settings";
  readonly title: string;
  readonly body: string;
}

/** Step ids are frozen as the analytics dimension and resume cursor:
 *  "welcome", "board", "sidebar-tree", "agent-workspace", "keyboard",
 *  "finale". Copy is impl-owned. */
export const TOUR_STEPS: readonly TourStep[];
```

```ts
// AppStore additions (store.ts, beside shortcutsOpen)
interface AppStore {
  tour: {
    open: Accessor<boolean>;
    stepIndex: Accessor<number>;
    /** True while demo rows are merged into the read accessors (A10). */
    demoActive: Accessor<boolean>;
    /** True only after GetTourState succeeded with no row (A5). */
    shouldAutoStart: Accessor<boolean>;
    start: (trigger: "first-run" | "replay" | "resume") => void;
    next: () => void;
    back: () => void;
    /** Writes DISMISSED + current step id, closes, clears demo rows. */
    dismiss: () => void;
    /** Writes COMPLETED, closes, clears demo rows. */
    complete: () => void;
  };
}
```

Live boot reads `GetTourState` after `WhoAmI` (`index.tsx`); fixture boot uses
an in-memory state (A5). Red → green: auto-start only on a successful empty
read, never on a failed read; `start` writes `STARTED` before step 1 shows;
dismiss and complete write their outcome; `next` past the last step calls
`complete`; demo rows appear in `agents()`/`issues()`/`channels()`/
`messages()` only while `demoActive`; every mutation closure called with a
`demo:` id makes no client call.

### T3 — `TourOverlay` + spotlight + `.cx-tour-*` CSS

New `apps/ui/src/components/TourOverlay.tsx` and
`apps/ui/src/design/components/tour.css`; App-root mount as a third overlay
sibling (`App.tsx:185-191`). Callouts on Kobalte Popover with `anchorRef`
resolving `[data-tour]` (A2/A3); dialog steps on `.cx-dialog` with the
ShortcutsOverlay focus capture/restore and trap (`ShortcutsOverlay.tsx:50-58`);
Skip tour on every step (A4); faint spotlight on callout steps (A11); "Demo"
badge on demo rows (A10). Adds `data-tour` attributes to anchored chrome
(LeftSidebar view buttons `LeftSidebar.tsx:442-454`, topbar nav `App.tsx:86`,
board grid, right sidebar). Entrance and transitions per A6.

```tsx
// apps/ui/src/components/TourOverlay.tsx
/** Reads store.tour + TOUR_STEPS; renders the current step. Hosted at the
 *  App root behind <Show when={store.tour.open()}>. No props. */
export const TourOverlay: Component;
```

Red → green (`TourOverlay.test.tsx`, shared test router): dialog step traps
and restores focus; callout anchors to its `[data-tour]` element; an anchor
that mounts one tick after navigation still anchors; a missing anchor skips
only after the bounded wait; Skip, `Escape`, and outside-click all call
`dismiss`; arrow keys work with no window-level listener (DL-223); the
spotlight layer is `pointer-events: none` and its cutout tracks the anchor
rect; reduced motion keeps all assertions passing. **Aggregate anchor test**:
for every callout step, mount the real `App` on that route with demo rows
active and assert the anchor resolves.

### T4 — Analytics indirection (after #656 T6)

New `apps/ui/src/tour/analytics.ts` (A7) and capture calls in T2's
transitions. Merges only after the #656 T6 embed defines the enable flag and
capture seam.

```ts
// apps/ui/src/tour/analytics.ts
export type TourEvent =
  | { name: "tour_started"; trigger: "first-run" | "replay" | "resume" }
  | { name: "tour_step_viewed"; step_id: string; index: number }
  | { name: "tour_dismissed"; step_id: string }
  | { name: "tour_completed" };

/** Resolves the T6 embed at call time; flag off → silent no-op. NEVER a
 *  static `posthog-js` import. */
export function captureTourEvent(event: TourEvent): void;
```

Red → green: with the flag off every T2/T3 flow still passes; with a stubbed
embed and the flag on, transitions emit the four events.

### T5 — Replay command + first-run arming

Register `tour.start` in the spine beside `view.shortcuts` (`spine.ts:89-96`
shape), `scope: "global"`, keywords `["tour", "welcome", "onboarding",
"help"]`. No keymap row (Matt, OQ-4). App mount checks
`store.tour.shouldAutoStart()` and calls `start("first-run")` behind idle
time (`motion.md:195-197`), after the boot layer clears (`motion.md:180-203`);
until that boot lane lands, the tour owns first launch.

Red → green: `tour.start` resolves in the registry and opens with `"replay"`
(or `"resume"` when the server holds a dismissed step); auto-start fires once
on an empty read and never after any outcome is stored.

### T6 — Docs

- `apps/ui/src/design/components.md`: `.cx-tour-*` classes, the spotlight
  mask, and the `--cx-tour-scrim` token; `motion.md`: the one-shot entrance
  chase.
- Gates: rumdl on touched docs; `cd apps/ui && bun test --conditions browser`
  affected suites; Biome + stylelint; Go tests and pgtests for T1.

## Tasks

- [ ] T1: `account_tour_state` migration + sqlc queries + `GetTourState` /
  `SetTourState` on `CompassService` + TS client regen + pgtests (incl.
  cross-tenant).
- [ ] T2: `tour/state.ts`, `tour/demo.ts`, `store.tour` controller, server
  read/write wiring, demo read seam + mutation guards + tests.
- [ ] T3: `TourOverlay.tsx` + `tour.css` (Popover callouts, `.cx-dialog`
  welcome/finale, Skip tour, faint spotlight, Demo badge, chase-light
  entrance) + `data-tour` anchors + tests.
- [ ] T4 (after #656 T6): `tour/analytics.ts` + capture wiring + tests.
- [ ] T5: `tour.start` spine registration + idle-deferred first-run arming +
  tests.
- [ ] T6: `components.md` / `motion.md` updates; gates.

## Open Questions

Ruled by Matt on PR #662, 2026-10-04:

- **OQ-1 — Remote tour content.** Static in-code steps day-1 (A9).
- **OQ-2 — "Seen" state.** Per account, on the server; a person never gets
  the tour twice (A5).
- **OQ-3 — Step arc.** Arc welcome → board → sidebar-tree → agent-workspace →
  keyboard → finale, illustrated with demo agents and content (A10).
- **OQ-4 — Chord for `tour.start`.** None. Add a Skip tour button (A4).
- **OQ-5 — Spotlight.** Faint spotlight on callout steps (A11).
- **OQ-6 — Re-offer on major releases.** Not now; may revisit.

Deferred (impl proceeds on the stated assumption):

- **OQ-7 — Demo rows beside real rows on replay.** Assumption: show both,
  demo rows badged (A10). Hiding real rows during a replay is the alternative.
- **OQ-8 — RPC home.** Assumption: the two RPCs ride `CompassService` beside
  `WhoAmI`. A separate preferences service is the alternative if more
  per-account UI settings follow.

## Ledger delta

Rows DL-272..275 in `DECISIONS.md` § UI shell, stamped `Active (Matt,
2026-10-04)`:

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-272 | The first-run product tour (RIG-2797) is built natively in SolidJS v2 as a store-gated App-root overlay (a third sibling of the shortcuts overlay + palette) whose steps anchor to real chrome via `data-tour` attributes and navigate the real router through the store's closures; while open it shows tour-only demo agents and content (`demo:` ids, badged, read-time only, never sent to the server). PostHog-rendered UI (Product Tours/surveys/banners or any `posthog-js` widget) NEVER ships in-app — PostHog is measurement/data-only (Matt's RIG-2793 ruling, restated by #656 T6) | Active (Matt, 2026-10-04) | [first-run tour §A1](#a1--host-shape-an-app-root-overlay-layer-store-gated) |
| DL-273 | Tour callout substrate is the Kobalte v2-alpha `Popover` (external `anchorRef` + controlled `open` — DL-150 a11y-hard scope) over a faint, pointer-transparent spotlight with a cutout at the anchor; the welcome/finale modal steps stay hand-rolled on the `.cx-dialog` convention (DL-230); every step has a Skip tour control; a missing `data-tour` anchor skips the step only after a bounded reactive resolve, never errors | Active (Matt, 2026-10-04) | [first-run tour §A3](#a3--callout-substrate-kobalte-popover-with-an-external-anchorref) |
| DL-274 | Tour "seen"/resume state is per account on the server (`account_tour_state`, tenant RLS; `GetTourState`/`SetTourState` keyed on the authenticated caller, never a client-supplied id). The UI writes `started` when the tour opens, so no person sees it twice; it auto-opens only after a successful read with no row, and a failed read arms nothing | Active (Matt, 2026-10-04) | [first-run tour §A5](#a5--first-run-detection--persistence-per-account-server-state) |
| DL-275 | Tour analytics ride a thin call-time indirection (`captureTourEvent`) over the #656 T6 PostHog embed: flag off → silent no-op, no static `posthog-js` import in tour code; only the instrumentation task sequences after T6 — the tour UI has zero dependency on it. Step content ships static in-code; the headless flag/EAF remote-content path is a deferred additive behind the same `TourStep[]` type | Active (Matt, 2026-10-04) | [first-run tour §A7](#a7--analytics-a-thin-no-op-safe-indirection-over-the-t6-embed) |
