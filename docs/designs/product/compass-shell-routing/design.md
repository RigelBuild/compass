# Compass shell routing (@solidjs/router adoption)

Status: Draft
Tracker: SEA-1693
Ledger-impact: reserves DL-127 (shell-routing: @solidjs/router adoption + routes-vs-store source-of-truth call, one row); compass appends at ship

## Problem / Intent

The Compass ADE shell dispatches its six surfaces from an in-memory signal —
`App.tsx:110-126` renders a `<Switch>` over `store.view()` (`"channel" |
"agent" | "bridge" | "backlog" | "done" | "settings"`, store.ts:75-81) — with
zero URL routing anywhere in `apps/ui` (no `window.location` / `location.hash`
/ `hashchange` / `history.pushState` / `@solidjs/router` usage; grep-confirmed
empty). SEA-1655's frozen deep-link `#/channel/<channelId>/topic/<topicId>`
(compass-zulip-threading-model §D5, design.md:272-273 "Deep-link route
`#/channel/<channelId>/topic/<topicId>`"; §T5 design.md:704) and general
shareable/bookmarkable/back-button navigation require real URL routing. This
record introduces it as a shell-wide routing layer: the router base merges
first, then SEA-1655 T5 stacks the topic route on it.

## Approach

**@solidjs/router in HashRouter mode** — Matt's ruling; this record designs
the *how*, not the library choice. Rationale as ruled:

- Official/first-party Solid router, tiny, zero framework lock-in above the
  transport boundary.
- `HashRouter` emits exactly the frozen route shape
  (`#/channel/<channelId>/topic/<topicId>`) and needs no server — the app is a
  client-only SPA loaded in a Wails v3 native webview (DL-110); no server
  renders HTML (`index.tsx:65-72` is a plain client
  `render(() => <StoreContext.Provider value={store}><App /></StoreContext.Provider>, root)`).
- `MemoryRouter` mode gives deterministic tests (see §A4).
- Forward-compat: SolidStart — the eventual hosted-Compass SSR path — is built
  ON @solidjs/router, sharing the same route primitives, so this is the
  lowest-friction on-ramp to SSR later. (SSR migration is a documented
  non-load-bearing future, not designed here.)

### A1 — Route table

The six `View` values (store.ts:75-81) map to routes 1:1; params carry the
selection that today lives only in signals:

| Route | Surface (today's `<Match>`) | Notes |
| --- | --- | --- |
| `/` | `<Bridge />` (`view() === "bridge"`, App.tsx:111-113) | Default surface, matching the boot default `createSignal<View>("bridge")` (store.ts:654). |
| `/channel/:channelId` | `<ChannelView />` (App.tsx:114-116) | `:channelId` replaces bare `selectedChannelId` for this surface. |
| `/channel/:channelId/topic/:topicId` | Topic view — **SEA-1655 T5, not this record** | Reserved here so the frozen deep-link nests under the channel segment; T5 adds the `<Route>`. |
| `/agent/:agentId` | `<AgentView />` (Switch fallback, App.tsx:110) | The fallback becomes an explicit param route. |
| `/backlog` | `<BacklogView />` (App.tsx:117-119) | |
| `/done` | `<DoneView />` (App.tsx:120-122) | |
| `/settings` | `<SettingsView />` (App.tsx:123-125) | |
| `*` (catch-all) | Redirect to `/` | An unknown/stale deep-link lands on the board, never a blank screen. |

Under HashRouter these render as `#/`, `#/channel/<id>`, `#/agent/<id>`, etc.
— the `#/channel/<channelId>/topic/<topicId>` string is exactly the frozen
SEA-1655 route.

The shell chrome (topbar, sidebars, `UsageBar`) stays outside the routed
region: `App` becomes the root layout route and only the `<main class="main">`
center (App.tsx:109-127) swaps per-route. Concretely, today's

```tsx
// App.tsx:110-126 (current)
<Switch fallback={<AgentView />}>
  <Match when={store.view() === "bridge"}><Bridge /></Match>
  <Match when={store.view() === "channel"}><ChannelView /></Match>
  <Match when={store.view() === "backlog"}><BacklogView /></Match>
  <Match when={store.view() === "done"}><DoneView /></Match>
  <Match when={store.view() === "settings"}><SettingsView /></Match>
</Switch>
```

becomes `props.children` inside `App`, with the route table declared at the
boot root (`index.tsx`):

```tsx
// index.tsx (target shape)
render(
  () => (
    <StoreContext.Provider value={store}>
      <HashRouter root={App}>
        <Route path="/" component={Bridge} />
        <Route path="/channel/:channelId" component={ChannelView} />
        <Route path="/agent/:agentId" component={AgentView} />
        <Route path="/backlog" component={BacklogView} />
        <Route path="/done" component={DoneView} />
        <Route path="/settings" component={SettingsView} />
        <Route path="*" component={RedirectHome} />
      </HashRouter>
    </StoreContext.Provider>
  ),
  root,
);
```

The store's `createRoot` singleton wiring (index.tsx:44-63) is untouched — the
router mounts *inside* the existing `StoreContext.Provider`, so `useStore()`
keeps working unchanged in every component.

### A2 — Source of truth: URL drives the store (Decision)

Today, view + selection are store signals, and the navigation actions set both:

- `openChannel` (store.ts:1010-1023) resolves the channel, delegates a 1:1
  agent DM to `openAgent`, else `setSelectedChannelId(channelId); setView("channel")`.
- `openAgent` (store.ts:976-1005) does `setView("agent")`, anchors
  `selectedAgentId`/`selectedIssueId`, and (first open only) resets the
  workspace tabs/repo/log state.
- `selectIssue` (store.ts:1028-1032) sets `selectedIssueId` and syncs
  `selectedAgentId` to the assignee — it deliberately does NOT change view
  ("stays on the board", store.ts:1025-1027).
- `showBridge`/`showBacklog`/`showDone`/`showSettings` (store.ts:1651-1654)
  are bare `setView(...)` calls.

**Decision: the URL becomes the source of truth for *view + routed
selection*.** Route params drive the store; the store's imperative navigation
actions become `navigate(...)` calls; a single route-sync effect applies the
inbound location to the store. Rationale:

- It is the idiomatic @solidjs/router model — `useParams`/`useNavigate` are
  the primitives; deep-links, back/forward, and refresh work for free because
  there is no second copy of the truth to reconcile.
- The mirror alternative (store stays truth, an effect writes `location.hash`
  and a `hashchange` listener writes back) keeps TWO authorities and needs
  loop-breaking guards in both directions; every future route (SEA-1655's
  topic, and anything after) pays that tax again.
- The churn is bounded but **not zero**. Components keep their read surface:
  `store.view()` and `store.selectedChannelId()` stay signals, written by the
  single route-sync effect (§A3), so the ~15 readers — the `ChannelRow`
  selected-state (`LeftSidebar.tsx:146-147`:
  `store.selectedChannelId() === channel().id && store.view() === "channel"`)
  and the App topbar tabs (App.tsx:48, 61) — keep their exact call sites and
  compile unchanged. What changes is **timing**: navigation becomes
  *asynchronous*. `openChannel`/`openAgent`/`show*` now `navigate`, and the
  store signal updates one reactive tick later when the route-sync effect runs
  (navigate → location update → effect → `setView`). Two consequences booked
  explicitly, not priced at zero: (1) every action-then-assert test migrates to
  an `await`/microtask flush (§A4, T3) — they do **not** pass unchanged; (2) a
  one-reactive-tick staleness window exists on a deep-link boot (a routed
  surface may paint one frame before its selection signal is written). This
  sync→async contract change is the real, recurring price of routes-as-truth;
  its ratification is the sole load-bearing Open Question.

**Scope split — routed vs. store-only state.** Only what the URL names is
routed truth: the surface (view) and the surface's identifying param
(`channelId` on `/channel/...`, `agentId` on `/agent/...`). Everything else
stays store-only exactly as today: `selectedIssueId` (a board-scoped selection
with no surface of its own — `selectIssue` never navigates), the workspace tab
state, `openThreadRootId`, sidebar open/collapsed state, and the pinned-agent
set. `selectIssue` (store.ts:1028-1032) is therefore **unchanged** by this
record.

### A3 — How the store reworks

The store cannot call `useNavigate()` itself — it is created outside the
router's tree, in an app-lifetime `createRoot` (index.tsx:44-63) — so the
router dependency is injected as a thin seam, keeping `createAppStore`
router-free and test-constructible exactly as today:

- **Router seam (injected, not imported).** The store gets two bound
  capabilities, wired once from `App` (which IS inside the router):
  `bindRouter({ navigate, currentPath })` where
  `navigate: (path: string) => void` and `currentPath: () => string` come from
  `useNavigate()`/`useLocation()`. `App` calls it once at mount:
  `store.bindRouter({ navigate: useNavigate(), currentPath: () => useLocation().pathname })`.
  This keeps `createAppStore` router-import-free and test-constructible exactly
  as today. **Invariant:** no store code path navigates before `App` mounts and
  binds — the only pre-bind action sources are the async comms stream (arrives
  post-render) and `loadAssignedIssues` (store.ts:672-679, does not navigate).
  To make a future violation loud rather than silent, the pre-bind `navigate`
  default **warns** (dev) instead of being a quiet no-op.
- **`openChannel(channelId)`** keeps its resolution logic (unknown-id no-op,
  DM delegation to `openAgent`, `setOpenThreadRootId(null)`) but replaces
  `setSelectedChannelId(channelId); setView("channel")` (store.ts:1021-1022)
  with `navigateTo("/channel/" + channelId)`.
- **`openAgent(agentId)`** becomes navigate-only:
  `setOpenThreadRootId(null); navigateTo("/agent/" + agentId)`. Its entire
  workspace-reset/anchoring body (store.ts:976-1005 — `selectedAgentId`,
  `selectedIssueId`, `activeRepoId`, tabs, `agentViewAgentId`, log state) moves
  into the route-sync effect (below), so the click path and a direct deep-link
  to `/agent/:agentId` run the **same** anchoring code once — not two homes.
- **`showBridge`/`showBacklog`/`showDone`/`showSettings`**
  (store.ts:1651-1654) become `navigateTo("/")`, `navigateTo("/backlog")`,
  `navigateTo("/done")`, `navigateTo("/settings")`.
- **Route-sync effect** — one `createEffect` in `App` (inside the router)
  reads `useLocation()` + `useParams()` and is the **single writer** of the
  routed dimension (`view`, and `selectedChannelId`/`selectedAgentId` when a
  param names them): it derives `View` from the matched route and applies the
  `channelId`/`agentId` param. For `/agent/:agentId` it runs the full anchoring
  lifted from `openAgent` (store.ts:976-1005), keyed on the existing first-open
  guard (`agentViewAgentId`, store.ts:990-994) so a re-open takes the re-open
  branch exactly as today. On any route **without** an agent param the effect
  leaves `selectedAgentId`/`selectedIssueId` untouched (it never nulls them),
  preserving today's topbar agent-tab persistence across `showBridge`/etc.
  (App.tsx:57-68).
- **Pending-aware unknown-id fallback.** The effect must NOT redirect a param
  whose id is merely *not yet loaded*. In live mode the store boots with
  `EMPTY_COMMS_STATE` (store.ts:754-755) and channels arrive asynchronously via
  `runCommsStream`, so a deep-link boot to `#/channel/<valid-id>` would, on the
  effect's first run, see empty `channels()` and wrongly bounce to `/`. The fix
  gates the redirect on first-snapshot arrival: hold the route (`ChannelView`
  renders its own empty
  state on null data) until the first snapshot has arrived (an explicit
  stream-synced flag — NOT non-emptiness: a genuinely empty workspace is a
  valid *resolved* state, and gating on non-emptiness would hang a deep-link
  into it forever), and only then redirect an id genuinely absent from the
  loaded set. The post-sync `adoptComms` correction (below) handles a channel
  that vanishes after load.
- **`view()` / `selectedChannelId()` readers**: unchanged call sites. The
  signals stay and the route-sync effect writes them — one shape, there are no
  "route-derived memos" (the store is outside the router tree and cannot call
  `useLocation`, so it consumes the bound `currentPath` seam instead). The ~15
  readers across `App.tsx:48,61,111-125`,
  `LeftSidebar.tsx:41-42,146-147,381-418`, and tests keep compiling; only their
  update *timing* shifts (§A2 async note).
- **Stream-adoption guard**: `adoptComms` (store.ts:785-790) re-points the
  selection when the selected channel vanishes from a pushed snapshot — this
  fires on snapshot arrival, not on route change, so it cannot live in the
  route-sync effect. Under routes-as-truth it reads the bound `currentPath()`:
  if the current route is `/channel/:id` and `id` vanished,
  `navigateTo("/channel/" + firstChannelId(next))` (or `/` when none); on any
  other surface it keeps today's signal fallback (the signal still seeds "last
  visited channel"). This is why the seam carries `currentPath`, not just
  `navigate`.

### A4 — Test story

`HashRouter` reads the real `location.hash`, which is shared global state — so
component tests use **`MemoryRouter`**, Solid Router's in-memory integration
(per the official "Alternative routers" doc: `MemoryRouter` keeps history in
memory, for testing and non-browser environments), keeping
`bun test --conditions browser` (apps/ui/moon.yml:37) deterministic.

The existing mount pattern (App.test.tsx:84-95) builds the store inside
`render` and wraps `<App />` in `StoreContext.Provider`. It gains a router
wrapper + optional initial route via a shared helper:

```tsx
// test-router.tsx (new helper) — mirrors index.tsx's production shape
function mountApp(initialPath = "/"): { store: AppStore; container: HTMLElement } {
  let store!: AppStore;
  const history = createMemoryHistory();
  history.set({ value: initialPath });
  const { container } = render(() => {
    store = createAppStore({ initialComms: STUB_COMMS_STATE });
    return (
      <StoreContext.Provider value={store}>
        <MemoryRouter history={history} root={App}>
          {/* same <Route> table as index.tsx, imported from one shared routes module */}
        </MemoryRouter>
      </StoreContext.Provider>
    );
  });
  return { store, container };
}
```

The route table itself is extracted to one shared module (`routes.tsx`) so
production (`HashRouter`) and tests (`MemoryRouter`) render the identical route
table — one `<Route>` declaration site, no drift between prod and test.

Existing store-driving tests (`store.openChannel(...)` then
`expect(store.view()).toBe("channel")`, App.test.tsx:191-192;
LeftSidebar.test.tsx:180-181, 206-207) do **not** pass unchanged. Because
`openChannel` now `navigate`s and the route-sync effect writes `view` one
reactive tick later, each such assertion must be preceded by an `await` /
microtask flush so the effect runs before the read. Migrating these
action-then-assert sites is explicit T3 scope (§Plan), not incidental.

### A5 — Native-seam composition (flagged for compass-native co-review)

Routing sits entirely ABOVE the transport/connection boundary (DL-106/107).
Invariant this section preserves (verbatim, from the SEA-1688 owner):

> The transport boundary is the ONLY seam — nothing above it (the router
> included) may assume local/embedded. A deep-link/route MUST resolve to the
> same `createGrpcWebTransport({fetch})` call regardless of
> embedded-vs-native-client mode; the provider-supplied fetch
> (WHATWG-compatible) is the single injection point (DL-106). The
> ConnectionProvider interface (SEA-1688) exposes that fetch and carries ZERO
> Wails/shell type, so apps/ui has zero shell dependency.

Consequences for this record: a route change never dials anything and never
assumes a local server — it only moves UI state; whatever data the routed
surface needs flows through the store's existing accessors over the one
transport seam `createLiveClients` owns (`live/client.ts:30-35`) — the single
place transport is chosen. The mode difference is a `fetch` swap *below* the
store: `createGrpcWebTransport({fetch})` with the dev default fetch or the
shell's `compass_rpc` custom fetch (`daemon-transport.ts:8-13`), the identical
call. That fetch injection into `createLiveClients` is SEA-1688 T1 work and is
not yet wired (`client.ts:30-35` today constructs clients without a `fetch`
param); routing sits above the seam `createLiveClients` **will** expose, so it
is unaffected either way. Deep-links resolve identically in embedded and
native-client mode, and the routing layer imports nothing from any shell/Wails
API. The ConnectionProvider fetch/provider TS signature is SEA-1688's own
record — not designed here.

Caller identity is **not** a routing concern: the route-sync effect only moves
UI state and never re-derives "me". A caller-scoped deep-link (e.g. a link to
"my" channel) resolves identity through the transport — the `WhoAmI` RPC via the
store (DL-111) — never off the interim env-sourced `callerId`
(`connection.ts:28-35`, retired by DL-111). This record cites no interim
identity field as a source.

## Alternatives considered

### TanStack Solid Router — rejected

Reached v1 only in Nov 2025; carried a supply-chain CVE; not first-party to
Solid; and its SSR future is TanStack Start, not SolidStart — adopting it
would lock the hosted-Compass SSR path away from the Solid-native stack.

### Vike — rejected

An SSR meta-framework. Inapplicable: the shell is a client-only SPA in a Wails
webview (DL-110); there is no server rendering HTML to hook.

### @solidjs/router in path (Router/BrowserRouter) mode — rejected

Path routing needs a server that answers every deep route with the app shell;
in a webview loading a static bundle, a refresh on `/channel/x` 404s or blanks.
HashRouter is precisely why SEA-1655's frozen route is spelled `#/...`.

### Keep in-memory dispatch (status quo) — rejected

Cannot satisfy the frozen deep-link `#/channel/<channelId>/topic/<topicId>`
(zulip-threading design.md:272-273), nor bookmarks/back-button/shareable URLs.

### Hand-rolled hashchange listener — rejected

A bespoke `location.hash` parser + `hashchange` listener could carry two
routes, but re-implements param matching, navigation, and history semantics
the first-party router already provides — and forfeits the SolidStart on-ramp
(SolidStart is built on @solidjs/router, same route primitives).

## Global Constraints

- `@solidjs/router` **latest stable, floor 1.0.0** (1.0.0 is current on the
  npm registry as of 2026-08-03; re-check at impl time).
- **HashRouter mode** — routes render as `#/...`; client-only, no SSR now
  (SolidStart migration is a documented non-load-bearing future).
- **Nothing above the transport boundary assumes local** (DL-106/107): the
  routing layer imports zero shell/Wails API; a route change never dials.
- No `: any` / `as any`; `Set`/`Map` over `Record` for lookups.
- `bun test --conditions browser` (apps/ui/moon.yml:37) stays green; component
  tests route via `MemoryRouter`, never real `location.hash`.
- One shared route-table module — production and tests render the same table.
- Composes with (does not supersede) DL-106/107/110; the routing decision
  lands in `docs/designs/product/DECISIONS.md` as **DL-127** (one row, net-new,
  supersedes no active row) — appended by the ledger single-writer at ship
  (freeze = merge), not by this record.
- Router base only — the `/channel/:channelId/topic/:topicId` route component
  is SEA-1655 T5's, stacked on this base.

## Plan

Router base only (SEA-1655 T5 stacks separately). Every task inherits
`## Global Constraints`.

**Sequencing — one PR.** T1, T2, and T3 land together as a single PR. T1 alone
is a broken intermediate: removing the `store.view()` `<Switch>`
(App.tsx:110-126) hands the center to the router, but until T2 no store action
`navigate`s — every sidebar/topbar click still calls `setView`
(store.ts:1021-1022), which now controls nothing — so in-app navigation goes
dead and the existing suites go red. The "`bun test --conditions browser` stays
green" constraint (Global Constraints) holds at the **PR boundary**, not per
task.

### T1 — Shared route table + HashRouter at boot

Add `@solidjs/router`; create `apps/ui/src/routes.tsx` exporting the route
table (A1) as a shared fragment; rework `index.tsx` to mount
`<HashRouter root={App}>` inside the existing `StoreContext.Provider`
(index.tsx:65-72), and `App.tsx` to render `props.children` in
`<main class="main">` in place of the `store.view()` `<Switch>`
(App.tsx:110-126). The `*` catch-all redirects to `/`. The store singleton
wiring (index.tsx:44-63) is untouched.

Interfaces:

- consumes: `HashRouter`, `Route` from `@solidjs/router`; existing view
  components (`Bridge`, `ChannelView`, `AgentView`, `BacklogView`, `DoneView`,
  `SettingsView`).
- produces: `routes.tsx` → `export const AppRoutes: Component` (the shared
  `<Route>` fragment); `App: Component<RouteSectionProps>` (root layout taking
  `props.children`).

Acceptance: `vite dev` boots to `#/`; hand-editing the hash to `#/backlog`,
`#/settings`, `#/agent/acc-cook` renders the matching surface; unknown hash
lands on `#/`.

### T2 — Store rework: navigate seam + route-sync effect

Implement A3: add the injected router seam
`bindRouter({ navigate, currentPath })` to the store; rework `openChannel`
(store.ts:1010-1023) and `showBridge`/`showBacklog`/`showDone`/`showSettings`
(store.ts:1651-1654) to `navigate` instead of `setView`, and reduce `openAgent`
(store.ts:976-1005) to `setOpenThreadRootId(null)` + navigate; add the
route-sync effect in `App` (via `useLocation`/`useParams`) as the single writer
of the routed dimension of `view`/`selectedChannelId`/`selectedAgentId`,
hosting the full agent anchoring (keyed on the existing `agentViewAgentId`
first-open guard) and the **pending-aware** unknown-id fallback (no redirect
before first snapshot); re-point `adoptComms`'s vanished-channel fallback
(store.ts:785-790) to read the bound `currentPath()`. `selectIssue`
(store.ts:1028-1032) is unchanged.

Interfaces:

- consumes: `useNavigate`, `useLocation`, `useParams` from `@solidjs/router`
  (in `App` only — the store stays router-import-free).
- produces: `AppStore.bindRouter(r: { navigate: (path: string) => void;
  currentPath: () => string }): void`; unchanged public reads `view(): View`,
  `selectedChannelId(): string | null`, `selectedAgentId(): string | null`;
  unchanged action signatures `openChannel(channelId: string): void`,
  `openAgent(agentId: string): void` (now navigate-backed, asynchronous).

Acceptance: clicking a `ChannelRow` updates the hash to `#/channel/<id>` and
renders `ChannelView`; browser back returns to the prior surface;
`LeftSidebar.tsx:146-147` selected-state and `App.tsx:48,61` active-tab
behavior unchanged; a 1:1 agent DM click lands on `#/agent/<id>`.

### T3 — Test harness: MemoryRouter mount helper

Implement A4: add the `MemoryRouter` + `createMemoryHistory` mount helper
rendering the shared `AppRoutes` table with an `initialPath`; **migrate every
action-then-assert site** in the existing `<App/>`-mounting suites
(App.test.tsx:84-95 `mountApp`; the LeftSidebar/RightSidebar suites that assert
`store.view()`/`selectedChannelId()` transitions after `openChannel`/`openAgent`)
to `await` a microtask flush between the action and the assertion, since
navigation is now asynchronous (§A2); add route-behavior tests: deep-link
initial path renders the right surface, `openChannel` moves the memory history,
a not-yet-loaded channel deep-link is held (not bounced) until the first
snapshot, and an unknown path redirects to `/`.

Interfaces:

- consumes: `MemoryRouter`, `createMemoryHistory` from `@solidjs/router`;
  `AppRoutes` (T1); `createAppStore({ initialComms })` (existing test
  pattern, App.test.tsx:87).
- produces: `mountApp(initialPath?: string): { store: AppStore; container: HTMLElement }`
  in a shared test helper module.

Acceptance: `bun test --conditions browser` green across apps/ui; no test
touches real `location.hash`.

## Tasks

- [ ] T1 — Shared route table + HashRouter at boot (`routes.tsx`, `index.tsx`,
      `App.tsx` Switch removal, catch-all redirect)
- [ ] T2 — Store rework: `bindRouter({navigate,currentPath})` seam,
      navigate-based `openChannel`/`openAgent`/`show*`, single-writer route-sync
      effect (agent anchoring + pending-aware unknown-id fallback), `adoptComms`
      fallback re-point via `currentPath()`
- [ ] T3 — Test harness: `MemoryRouter` `mountApp(initialPath)` helper, suite
      migration, route-behavior tests

## Open Questions

- **Resolved (ratified by Matt, 2026-08-03).** Routes-as-truth makes store
  navigation **asynchronous**: `openChannel`/`openAgent`/`show*` `navigate`, and
  `view`/`selectedChannelId` update one reactive tick later via the route-sync
  effect (§A2/§A3). This is a sync→async contract change — all action-then-assert
  tests migrate to `await` (T3), and a one-tick deep-link staleness window is
  accepted. The alternative (store-as-truth mirror: store stays authority, one
  effect writes `location.hash`, a listener applies inbound changes) keeps every
  reader and test synchronous but pays a bidirectional loop guard re-paid per
  future route. **Decision: routes-as-truth, async cost accepted** — idiomatic
  Solid Router, free back/forward/deep-link, and SEA-1655 T5 stacks on it
  cleanly.
- **Non-load-bearing (deferred):** SolidStart SSR migration for
  hosted-Compass. Documented as the forward-compat rationale only;
  @solidjs/router is SolidStart's router, so the route table carries over.
  No design work now.
- **Non-load-bearing (deferred):** whether `selectedIssueId` ever becomes a
  routed param (e.g. `/issue/:id`). Today `selectIssue` deliberately stays on
  the board (store.ts:1025-1027) and no surface is keyed by issue id; revisit
  only if a per-issue surface is designed.
- **Composition (separate record, does not block this freeze):** the store's
  server-state loading is moving to `@tanstack/solid-query` +
  `@connectrpc/connect-query-core` (Matt, 2026-08-03) — unary reads
  (`listMessages`/`listTopics`/issues) become cached, paginated queries beneath
  the store's accessor seam. That does not change routing: the route-sync effect
  still writes only UI-state signals, and the **pending-aware unknown-id
  fallback** (§A3) still holds — it just reads its "data ready" signal from the
  query layer's loaded/pending state instead of the wholesale-stream
  first-snapshot flag once that record lands. The principle (never redirect a
  not-yet-loaded id) is loading-primitive-agnostic. Sequencing between this
  router base and the query-layer record is a lane call, not a design fork.
