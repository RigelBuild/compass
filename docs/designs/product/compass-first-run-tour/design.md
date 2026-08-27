# Compass native first-run product tour (RIG-2797)

Status: Draft

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

### A4 — Sequencing, dismissal, resume

- **Advance/back**: Next/Back buttons in the callout footer plus
  `ArrowRight`/`ArrowLeft`. Keys are **component-local**, never a second
  window keymap listener (DL-223's one-listener law): the callout footer holds
  focus, so its own handlers fire — and mid-step interaction with the live app
  is therefore pointer-driven (the non-`modal` Popover does not trap focus).
  **Dismissal is single-pathed**: the Popover surfaces `Escape` and
  outside-click through `onOpenChange`
  (`apps/ui/node_modules/@kobalte/core/dist/index/QQ67U6Bm.d.ts:150-152`), and
  the controller routes `onOpenChange(false)` straight to
  `store.tour.dismiss()`, so the controlled `open` never desyncs from
  Kobalte's internal dismissal. Outside-click dismisses (consistent with a
  non-modal layer); the welcome and finale `.cx-dialog` steps trap focus and
  are dismissed only by their own controls or `Escape`.
- **Skip = dismiss-with-memory**: dismissal at step *k* persists
  `{ dismissedAt: k }`; the tour never auto-reopens. Completion persists
  `{ completed: true }`.
- **Resume**: a replay entered from a persisted mid-tour state offers "resume
  from step *k*" (the persisted cursor) with restart available.
- **Replay affordance**: a `tour.start` command registered in the keyboard
  spine beside the view commands (`spine.ts:89-96` — the `view.shortcuts`
  registration is the shape: `{ id, title, keywords, scope: "global", run }`),
  so the palette's action mode inventories it (DL-229: register =
  actionable-from-anywhere) — no new chrome button needed.

### A5 — First-run detection + persistence: the `safeLocalStorage` pattern

Tour state persists exactly as the pin set does — best-effort, synchronous
write-through, workspace-namespaced:

- `safeLocalStorage()` (`store.ts:650-656`) — "The `localStorage` handle, or
  undefined where it is absent or throwing (SSR, a privacy-locked context).
  Persistence is best-effort" — is the accessor; absence means the tour state
  is session-only, never a crash.
- The key follows the pin set's shape, `compass.tourState.<key>`, mirroring
  `compass.pinnedAgents.${workspace}` (`store.ts:671/704`). **But the
  namespacing grain is a live fork, not a settled default** (OQ-2): the pin
  set is workspace-namespaced because agent ids are deployment-local
  (`store.ts:660-663`), a rationale that does NOT transfer to a
  "has-this-human-seen-the-product" flag. `workspaceKey` is
  `` `${connection.baseUrl}#${callerId}` `` (`index.tsx:134`; fallback
  `store.ts:862`), and `baseUrl` is unstable across a LAN IP vs a tailnet
  hostname vs a port — so a per-workspace key re-arms the auto-tour on every
  new deployment *and* every URL variant of the same one. The candidate grains
  (client-global `compass.tourState`, vs per-workspace, vs a global "seen" +
  per-workspace resume cursor) are OQ-2; DL-274's wording tracks whichever
  grain Matt ratifies.
- Writes are synchronous write-through inside the state transition, the
  `pinAgent` shape (`store.ts:2058-2065` — `setPinnedAgents((prev) => { …
  savePinnedAgents(workspaceKey, next); return next; })`), with per-field
  defensive hydration (the `loadPinnedAgents` self-healing parse,
  `store.ts:667-694`: bad JSON / wrong shape → the empty default).

First-run detection distinguishes **empty** storage from **absent** storage.
`safeLocalStorage()` returns undefined in a privacy-locked context
(`store.ts:651-657`), which would leave `loadTourState` perpetually empty and
auto-open the tour on *every* launch. So `shouldAutoStart` requires storage to
be **present and holding no tour state**; locked/absent storage arms nothing
(the tour stays replay-only via `tour.start`, A4). Given present storage, the
tour **auto-opens only on a genuinely fresh workspace**; a dismissed or
completed state never re-triggers (A4).

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
(the T3 union below) and resolves the PR #656 T6 embed **at call time**: when
the analytics enable flag is off, every call is a silent no-op and the tour is
fully functional un-instrumented. (The embed is *present* by the time any
capture ships — T3 sequences after T6, and a statically-bundled build cannot
soft-import an absent module — so flag-off is the only live no-op path; OTel
`trace_id` stamping is the embed's own concern per the obs record's J1, not
this indirection's.) This satisfies the T6
contract ("embed `posthog-js` behind an off-by-default enable flag +
configurable host"; "PostHog contributes only headless data — event capture,
and flag/early-access-feature JSON payloads … never a PostHog widget" — PR
656 T6) while keeping the dependency one-directional: the tour's UI tasks
(T1/T2/T4 below) have zero dependency on T6; only the instrumentation task
(T3) sequences after it. Events: `tour_started` (with `trigger: "first-run" |
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

### A9 — Remote content: seam acknowledged, not adopted day-1

Step definitions ship **static, in-code** (a `TOUR_STEPS: readonly TourStep[]`
table). The optional headless remote-content path T6 names
(`getFeatureFlagPayload` / `getEarlyAccessFeatures` JSON rendered by our own
component) is deliberately NOT adopted day-1: it would make first-run content
depend on an off-by-default network SDK, inverting the no-op requirement (A7).
Because steps are data (A2), a later remote override is a pure data-source
swap behind the same `TourStep[]` type — the seam is the type, no rework. See
OQ-1.

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

### "Seen" state: store-backed signal only, or server-persisted

- **Signal-only (no persistence)**: the tour would replay on every launch —
  fails the premise.
- **Server-persisted per-account preference**: durable across devices, but
  there is no UI preferences surface in the contract today (the store persists
  exactly one client pref, the pin set — `store.ts:612-618`: "prefs in
  `localStorage` — the pinned-agent set"), so this buys a cross-device nicety
  at the cost of a new RPC + schema + migration on a cold-start feature.
  Rejected day-1; the localStorage state is self-healing and losing it merely
  re-offers a skippable tour. A future preferences-sync lane can lift the same
  `TourState` shape server-side (see OQ-2).
- **Chosen**: workspace-namespaced `localStorage` via the shipped
  `safeLocalStorage` pattern (A5) — zero new backend surface, identical
  semantics to the pin set, degrades to session-only where storage is locked.

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
- **Persistence**: tour state only via the `safeLocalStorage` pattern
  (`store.ts:650-656`), workspace-namespaced
  (`compass.tourState.<workspaceKey>`, key derivation `index.tsx:134`),
  best-effort, synchronous write-through (`store.ts:2058-2065`), self-healing
  hydration (`store.ts:667-694`). Never a crash on locked storage.
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
- **Ledger**: new rows DL-272..275 (next contiguous block above the landed
  max DL-268 at `DECISIONS.md:371`, leaving DL-269..271 for RIG-2751 #645
  ahead in the merge queue), driver-folded in the same PR; no row superseded.

## Plan

Dependency order: T1 → T2 → (T3 ∥ T4) → T5. T1/T2/T4/T5 have **no**
dependency on PR #656; **T3 alone sequences after the #656 T6 embed lands**
(and is a no-op-safe shim even then, so it can merge with T6 still dark).

### T1 — Tour state: controller + persistence in the store

New files `apps/ui/src/tour/state.ts` (pure) and store wiring in
`apps/ui/src/store.ts` beside the sibling overlay signals
(`store.ts:1942-1965`). Persistence via the shipped `safeLocalStorage`
pattern (A5): key `compass.tourState.<workspaceKey>`, self-healing hydration,
synchronous write-through on every transition.

Interfaces:

```ts
// apps/ui/src/tour/state.ts (pure — no DOM, no store import)
export interface TourStep {
  readonly id: string;
  readonly kind: "dialog" | "callout";
  readonly anchor?: string; // data-tour value, callout steps only
  // Only the four closure-backed static views navigate (A1/B2);
  // parameterized surfaces (agent workspace) are anchored, never navigated.
  readonly route?: "/" | "/backlog" | "/done" | "/settings";
  readonly title: string;
  readonly body: string;
}

export interface TourState {
  readonly completed: boolean;
  /** Step id the user dismissed at; undefined = never dismissed. */
  readonly dismissedAt?: string;
}

/** Hydrate persisted state; bad JSON / wrong shape → undefined (fresh). */
export function loadTourState(workspace: string): TourState | undefined;
/** Best-effort write-through (savePinnedAgents shape, store.ts:697-708). */
export function saveTourState(workspace: string, state: TourState): void;

/** The static step table (A2/A9). Copy is impl-owned; ids are frozen here
 *  as the analytics dimension: "welcome", "board", "sidebar-tree",
 *  "agent-workspace", "keyboard", "finale". */
export const TOUR_STEPS: readonly TourStep[];
```

```ts
// AppStore additions (store.ts, beside shortcutsOpen at store.ts:268-273)
interface AppStore {
  tour: {
    open: Accessor<boolean>;
    stepIndex: Accessor<number>;
    /** Arm-on-fresh: true only when no persisted state exists (A5). */
    shouldAutoStart: Accessor<boolean>;
    start: (trigger: "first-run" | "replay" | "resume") => void;
    next: () => void;
    back: () => void;
    /** Persists { dismissedAt: currentStepId } and closes. */
    dismiss: () => void;
    /** Persists { completed: true } and closes. */
    complete: () => void;
  };
}
```

`start`/`next` perform the step's `route` navigation through the existing
closures (`store.ts:1952-1969`) before opening/advancing — and because `route`
is the four-view union (B2), every step's navigation target is a closure that
exists; the agent-workspace step carries no `route` and is reached by anchor.
Red → green (`apps/ui/src/tour/state.test.ts` + store tests): hydration
self-healing (bad JSON → fresh), namespacing (two keys don't cross-suppress),
dismiss-at-k persistence, `next` past the last step calls `complete`;
auto-start fires only when storage is **present and empty**, and **not** when
storage is absent/locked (S3 — no auto-open every launch).

### T2 — `TourOverlay` component + `.cx-tour-*` CSS

New `apps/ui/src/components/TourOverlay.tsx` +
`apps/ui/src/design/components/tour.css`; App-root mount as a third overlay
sibling (`App.tsx:185-191`). Callouts on Kobalte Popover with `anchorRef`
resolving `[data-tour]` (A2/A3); dialog steps on `.cx-dialog` with the
ShortcutsOverlay focus capture/restore + trap shape
(`ShortcutsOverlay.tsx:50-58`); missing anchor skips the step. Adds inert
`data-tour` attributes to the anchored chrome (LeftSidebar view buttons
`LeftSidebar.tsx:442-454`, topbar nav `App.tsx:86`, board grid, right
sidebar). Entrance chase + step transitions per A6, tokens only.

Interfaces:

```tsx
// apps/ui/src/components/TourOverlay.tsx
/** Reads store.tour + TOUR_STEPS; renders the current step. Hosted at the
 *  App root behind <Show when={store.tour.open()}>. No props. */
export const TourOverlay: Component;
```

Consumes: T1's `store.tour` + `TOUR_STEPS`; Kobalte `Popover`
(`anchorRef?: Accessor<HTMLElement | undefined>`, `open?: boolean` —
`apps/ui/node_modules/@kobalte/core/dist/index/QQ67U6Bm.d.ts:139-147`);
`.cx-dialog` conventions. Produces: the `data-tour`
anchor contract (values = `TourStep.anchor`).

Red → green (`TourOverlay.test.tsx`, mounted via the shared test router,
`test-router.tsx` memoryHistory): dialog step renders with focus trapped and
restored on close; callout step anchors to the `[data-tour]` element; an
anchor that mounts one tick *after* navigation still anchors (B1 — the bounded
reactive resolve, not a synchronous miss); a genuinely missing anchor advances
to the next step only after the bounded wait (no error); Escape / outside-click
both route through `onOpenChange(false)` to dismiss and persist `dismissedAt`
(S5); ArrowRight/ArrowLeft advance/retreat with the callout focused and **no**
window-level keydown listener added (DL-223); a step with `route` navigates
(location changes) before rendering; reduced-motion (`[data-reduce="on"]`)
leaves all assertions passing. **Aggregate anchor contract test** (S4): for
every callout step in `TOUR_STEPS`, mount the real `App` on that step's route
via the memory-router harness and assert its `data-tour` anchor resolves — so
DOM drift that would silently empty the tour is a red CI check, while the
runtime skip above stays the graceful degradation.

### T3 — Analytics indirection (sequences after #656 T6)

New `apps/ui/src/tour/analytics.ts` (A7) + capture calls wired into T1's
transitions. **Ordering: merges only after the #656 T6 embed defines the
enable-flag + capture seam**; the module still guards for the flag being off.

Interfaces:

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

Consumes: the T6 embed's exported capture handle + enable flag (exact import
path fixed by T6's impl; this module is the only file that names it).
Red → green: with the enable flag off, every tour flow from T1/T2 tests still
passes (the no-op contract); with a stubbed embed and the flag on, transitions
emit the four events with correct payloads.

### T4 — Replay command + first-run arming

Register `tour.start` in the spine beside `view.shortcuts`
(`spine.ts:89-96` shape; deps extended with T1's `startTour` closure,
`spine.ts:70-79`), `scope: "global"`, keywords `["tour", "welcome",
"onboarding", "help"]` — palette-discoverable per DL-229. Wire auto-start:
App mount checks `store.tour.shouldAutoStart()` (S3 — present-and-empty storage
only) and calls `store.tour.start("first-run")` deferred behind idle time (the
boot-sequence posture, `motion.md:195-197` — never blocking first input).
**Sequencing against the boot sequence** (S6): the first-load boot
choreography (`motion.md:180-203`) is itself an idle-deferred wow on the same
fresh-workspace first frame, so the tour auto-start waits for the boot layer to
clear before opening; until that boot lane lands in-tree, the tour is the
first-launch owner and this stays an explicit integration point.

Interfaces: consumes T1's `store.tour.start`; touches
`apps/ui/src/keyboard/spine.ts` (one registration + one dep), `App.tsx`
(arming effect). No keymap row (no default chord — palette-only; a chord is
OQ-4).

Red → green: `tour.start` resolves in the registry and opens the tour with
`trigger: "replay"` (or `"resume"` when a `dismissedAt` cursor exists);
auto-start fires exactly once on a fresh workspace and never when
completed/dismissed state is persisted.

### T5 — Docs + ledger follow-through

- `apps/ui/src/design/components.md`: add the `.cx-tour-callout` /
  `.cx-tour-*` class contract section; `motion.md`: note the tour entrance as
  a finite-iteration chase-light consumer.
- Ledger delta rides the same PR (driver-folded): DL-272..275 (below).
- Gate: markdownlint on touched docs; `cd apps/ui && bun test --conditions
  browser` affected suites; Biome + stylelint.

Interfaces: none new; docs only.

## Tasks

- [ ] T1: `tour/state.ts` (TourStep/TourState, load/save via
  `safeLocalStorage`, `TOUR_STEPS`) + `store.tour` controller wired beside
  the sibling overlay signals + red→green state/persistence tests.
- [ ] T2: `TourOverlay.tsx` + `design/components/tour.css` (Kobalte Popover
  callouts on `data-tour` anchors, `.cx-dialog` welcome/finale, chase-light
  entrance, reduced-motion by token) + `data-tour` attributes on anchored
  chrome + red→green component tests.
- [ ] T3 (after #656 T6 lands): `tour/analytics.ts` no-op-safe
  `captureTourEvent` + capture wiring in the controller + red→green
  no-op/emission tests.
- [ ] T4: `tour.start` spine registration (palette-discoverable) +
  idle-deferred first-run arming in App + red→green replay/arming tests.
- [ ] T5: `components.md`/`motion.md` updates; ledger delta DL-272..275
  noted for the driver; markdownlint/Biome/stylelint gates.

## Open Questions

Load-bearing (need Matt's ruling before impl):

- **OQ-1 — Remote tour content: adopt the headless flag/EAF payload path
  day-1, or ship static steps only?** Recommendation: **static in-code steps
  day-1** (A9) — remote content would couple first-run UX to an
  off-by-default network SDK, and the `TourStep[]` type keeps the swap
  additive later. Load-bearing because a day-1 remote path changes T1's data
  source and T3's scope.
- **OQ-2 — "Seen" state: storage tier AND namespacing grain.** Two coupled
  forks. (a) *Tier*: workspace-scoped localStorage day-1, or per-account server
  sync? Recommendation: **localStorage day-1** (Alternatives §"Seen" state) —
  worst failure is a re-offered skippable tour; server sync needs new contract
  surface no other UI pref has today. (b) *Grain* (S2): the pin set's
  per-workspace key re-arms the auto-tour on every new deployment and every URL
  variant (`baseUrl` is unstable), which is wrong for a per-human "seen" flag.
  Recommendation: **client-global `compass.tourState`** (or global "seen" +
  per-workspace resume cursor if resume must stay workspace-local).
  Load-bearing: the grain sets DL-274's wording, and a server answer adds an
  RPC + schema task that re-sequences the plan.
- **OQ-3 — Step inventory + copy.** The frozen step *ids* (T1) assume the
  six-beat arc welcome → board → sidebar-tree → agent-workspace → keyboard →
  finale. Recommendation: ratify the arc now, leave copy impl-owned (reviewed
  at PR). Load-bearing because ids are the analytics dimension and the resume
  cursor — renaming after T3 ships breaks funnel continuity. **Arc caveat**
  (B2): the `agent-workspace` beat has no static route (it is `/agent/:agentId`
  — `routes.tsx:42`) and a genuinely fresh workspace may have zero agents, so
  that step is designed as **anchored to the agent tree / right sidebar, not
  navigated**, and it self-skips (A2 bounded resolve) when no agent exists —
  its `tour_step_viewed` will under-report on empty workspaces by design.
  Confirm the arc with that beat so scoped.

Deferred (non-load-bearing; impl proceeds on the stated assumption):

- **OQ-4 — A default chord for `tour.start`?** Assumption: none — palette +
  keywords suffice for a rare action; a chord can ride a later keymap wave
  (the DL-252 pattern) without touching this design.
- **OQ-5 — Spotlight/backdrop dimming on callout steps** (dim the app,
  punch a hole over the anchor). Assumption: ship without it — a dimmed
  backdrop fights the "the tour IS the live app" premise and adds a
  clip-path maintenance surface; revisit after first dogfood feedback.
- **OQ-6 — Tour re-offer on major releases** (a "what's new" re-run keyed on
  version). Assumption: out of scope; the changelog/announcement surface PR
  #656 T6 names is a separate native component and a separate record.

## Ledger delta

Proposed rows (next contiguous block above the landed max DL-268, `DECISIONS.md:371`, leaving DL-269..271 for RIG-2751 #645 ahead in the merge queue; driver folds
into `DECISIONS.md` in the same PR, stamping `Active (Matt, <merge date>)` per
the ledger's status convention — the cells below carry the ratification
provenance, the driver supplies the date):

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-272 | The first-run product tour is built natively in SolidJS v2 as a store-gated App-root overlay (a third sibling of the shortcuts overlay + palette) whose steps anchor to real chrome via `data-tour` attributes and navigate the real router through the store's closures; PostHog-rendered UI (Product Tours/surveys/banners or any `posthog-js` widget) NEVER ships in-app — PostHog is measurement/data-only (Matt's RIG-2793 ruling, restated by #656 T6) | Active (Matt, on merge) | [first-run tour §A1](product/compass-first-run-tour/design.md#a1--host-shape-an-app-root-overlay-layer-store-gated) |
| DL-273 | Tour callout substrate is the Kobalte v2-alpha `Popover` (external `anchorRef` + controlled `open` — DL-150 a11y-hard scope); the welcome/finale modal steps stay hand-rolled on the `.cx-dialog` convention (DL-230), reusing the ShortcutsOverlay focus trap/restore; a missing `data-tour` anchor skips the step only after a bounded reactive resolve, never errors | Active (Matt, on merge) | [first-run tour §A3](product/compass-first-run-tour/design.md#a3--callout-substrate-kobalte-popover-with-an-external-anchorref) |
| DL-274 | Tour "seen"/resume state persists as best-effort localStorage via the shipped `safeLocalStorage` + write-through pin-set pattern, self-healing on hydrate, auto-opening only when storage is present and empty (locked storage → replay-only); the namespacing grain (client-global vs per-workspace vs global-seen + per-workspace resume) is set by OQ-2's ruling, and server-side preference sync is a named future, not day-1 | Active (Matt, on merge) | [first-run tour §A5](product/compass-first-run-tour/design.md#a5--first-run-detection--persistence-the-safelocalstorage-pattern) |
| DL-275 | Tour analytics ride a thin call-time indirection (`captureTourEvent`) over the #656 T6 PostHog embed: flag off → silent no-op, no static `posthog-js` import in tour code; only the instrumentation task sequences after T6 — the tour UI has zero dependency on it. Step content ships static in-code; the headless flag/EAF remote-content path is a deferred additive behind the same `TourStep[]` type | Active (Matt, on merge) | [first-run tour §A7](product/compass-first-run-tour/design.md#a7--analytics-a-thin-no-op-safe-indirection-over-the-t6-embed) |
