# Compass Native App — Multi-Window Support

Status: Draft
Parent: [compass-native-app/design.md](../compass-native-app/design.md) (DL-106..DL-112)
Composes with: [compass-native-client-mode/design.md](../compass-native-client-mode/design.md) (frozen T5)

## Problem / Intent

Matt runs a multi-window desktop setup and wants the Compass native app to
replicate it: one window pinned to the Bridge board (`/`), and a second window
for moving around agent workspaces and channels. Today the shell opens exactly
one window — `go/cmd/compass-app/main.go:142-147`, a single
`app.Window.NewWithOptions(application.WebviewWindowOptions{... URL: "/"})`.
Wails v3 supports N windows natively (`WindowManager.NewWithOptions`,
`wails/v3@v3.0.0-beta.0/pkg/application/window_manager.go:54`, callable
repeatedly); this record designs HOW Compass uses that — window topology,
per-window boot semantics, bridge fan-out, and composition with the frozen T5
mode-injection contract.

## Approach

### A1 — Per-window independent UI boot over the one shared shell bridge (the fork, resolved)

Each Wails webview window is a separate browser context — a separate JS runtime,
so a separate SolidJS boot. The chosen shape: **every window runs the existing
boot chain unchanged and independently** (connection resolve → `bootCaller`
WhoAmI → own `QueryClient` → own app-lifetime store → own `SubscribeComms` +
`SubscribeEvents` streams; `apps/ui/src/index.tsx:32-127`), all over the **same
single Go-side bridge**: one `bridgeService` + one pump registered app-wide as a
Wails service (`main.go:105-113`), one daemon socket, shared by all windows.

Why this is correct, grounded in the wiring as-built:

- **The Go side is already shared and window-agnostic.** The bridge service is
  bound once in `application.Options.Services` (`main.go:111-113`) and keyed
  purely by `requestId` (`bridge_service.go:145-148`, in-flight map
  `bridge_service.go:38-45`). Request ids are `crypto.randomUUID()` minted
  per-call in the UI transport (`apps/ui/src/daemon-transport.ts:100`), so two
  windows' calls can never collide. N windows against one service needs zero
  contract change — the concurrent-distinct-ids property is already the tested
  invariant (`bridge_service_test.go:439-490`).
- **The JS side cannot be literally shared.** The store is a per-boot
  `createRoot` singleton whose owner scopes the comms stream teardown
  (`index.tsx:83-113`, `store.ts:893-935`); there is no cross-window sharing
  primitive in separate WebKitGTK webview contexts (no SharedWorker across
  Wails windows). "One store for both windows" would have to mean the shell
  fanning one reduced comms stream to both webviews — which puts compass.v1
  stream/reduction logic in the shell, a direct violation of the thin-shell
  invariant (`compass-native-app/design.md:330-334`: the bridge moves bytes and
  holds no compass.v1 command logic).
- **The costs of independence are small and convergent.** Two windows = two
  `SubscribeComms` + two `SubscribeEvents` server subscriptions and one extra
  WhoAmI unary at boot (`index.tsx:64`). The server already serves N
  subscribers; each stream push replaces the window's comms state wholesale
  (`store.ts:823-834, 863-867`), so both windows converge on every push —
  "divergent caches" is bounded to local-only UI state (in-progress ask
  answers, selection), which is genuinely per-window state anyway.
- **Persisted prefs are best-effort, last-writer-wins across windows.** GTK3
  webviews share the process-default WebKit web context — every view is created
  via `webkit_web_view_new_with_user_content_manager` with no per-window
  `WebKitWebContext` and no ephemeral data manager
  (`wails .../linux_cgo_gtk3.go:1478`), so `localStorage` is one store across
  windows, and `workspaceKey` (`${connection.baseUrl}#${callerId}`,
  `index.tsx:102`) is identical in both. But the store reads pinned agents ONCE
  at boot (`store.ts:784-785`) and rewrites them WHOLESALE on every pin/unpin
  (`store.ts:1908, 1917`) with no `storage`-event listener, so two live windows
  hold independent in-memory pin sets and clobber each other last-writer-wins
  (pin X in window A, then unpin Y in window B → B's write erases A's pin of X).
  Shared storage makes the clobber cross-window rather than preventing it. This
  is an accepted cost of per-window independence, not a coherence guarantee;
  live cross-window pref sync (a `storage` listener or a shell relay) is a named
  deferral (OQ-7). The shared-storage fact is not load-bearing: were storage
  NOT shared, each window would keep an independent pin set — arguably less
  broken — and the per-window design holds either way.
- **Nothing above the transport boundary changes for embedded mode.** The UI
  keeps its one sanctioned mode divergence — which `ConnectionProvider` boot
  installs (`apps/ui/src/live/provider.ts:1-14`) — and multi-window adds no
  second one. In embedded mode `apps/ui` needs **zero changes** for this record.
  The client-mode boot gate is the exception: two windows both run the T5.5
  connect gate, which collides with the frozen single-probe contract — an
  unresolved fork that may add a UI task (OQ-8; see A3 and Open Questions).

### A2 — Window topology: two named windows at startup, a Window menu, board dedup

- **Startup default: open both windows.** Matt's ask is a fixed two-window
  working set, not an on-demand extra window — so the shell opens both at
  launch: window `"board"` (`URL: "/"`, the Bridge board route,
  `apps/ui/src/routes.tsx:30`) and window `"workspace"` (`URL: "/#/backlog"`,
  see A2a). Window creation is factored into one helper so both calls share
  options (assets, mode injection, size) — replacing the single inline
  `NewWithOptions` at `main.go:142-147`.
- **Per-window initial route rides the URL fragment.** The UI router is
  `HashRouter` (`index.tsx:119`), so the route lives after `#`:
  `WebviewWindowOptions.URL` (`webview_window_options.go:59-60`, already used
  at `main.go:146`) carries `"/"` for the board and `"/#/backlog"` for the
  workspace window. Unknown fragments fall back to the board via the `*`
  catch-all (`routes.tsx:37`), so a stale route string degrades safely.
- **Window identity via `Name`.** `WebviewWindowOptions.Name`
  (`webview_window_options.go:44-45`) names each window; "New Board Window"
  dedups by `app.Window.GetByName("board")` (`window_manager.go:17-27`) and
  calls `Focus()` on the existing one (`webview_window.go:1467`) instead of
  opening a duplicate. Workspace windows are deliberately NOT deduped — "moving
  around the workspaces" plausibly wants more than one; extra workspace windows
  get suffixed names (`workspace-2`, …).
- **Menu: a Window submenu beside the existing File menu.** Mirrors the menu
  wiring at `main.go:132-140`: `Window → New Board Window` (dedup+focus) and
  `Window → New Workspace Window` (always opens). The existing
  `File → Quit and stop stack` is app-level and unchanged.
- **Quit semantics unchanged.** On Linux/GTK3 the app quits when the last
  window closes (`application_linux_gtk3.go:194-197`; opt-out is
  `LinuxOptions.DisableQuitOnLastWindowClosed`,
  `application_options.go:310-311` — not taken). Closing one window leaves the
  other running; closing both quits the app with the stack lingering, exactly
  the DL-108 linger contract (`main.go:122-131`).
- **`OnSecondInstanceLaunch` is out of scope.** It handles a second *process*
  launch (`single_instance.go:28-46`), not in-app duplicate windows; adopting
  `SingleInstance` (second `compass-app` launch focuses the existing board
  window) is a deferral, not part of this record (see Open Questions).

### A3 — Composition with T5 mode injection: every window gets the same globals

The frozen T5 record injects the launch mode as a startup global
(`window.__COMPASS_MODE__ = "embedded" | "client"`) **at window creation**, via
the `WebviewWindowOptions` in `run()` (`compass-native-client-mode/design.md:709-730`
OQ-8; T5.6 at `:554-574`). Multi-window composes by construction: the window
factory (A2) is the single place `WebviewWindowOptions` is built, so the same
`JS` startup injection (`webview_window_options.go:119-120`) — mode, and in
windows therefore inherit the same mode/serverURL, so a per-window mode-INJECTION
divergence is structurally impossible and no new mode surface is added.

**This composition claim covers mode injection only, not the client-mode boot
FLOW.** The T5.5 connect gate is per-window and stateful, and the frozen T5
record mandates exactly ONE auto-connect probe per launch
(`compass-native-client-mode/design.md:180-186`, :550-553). Two windows opened
at startup in client mode each run the gate → two concurrent `shellConnect("")`
probes (contradicting the frozen contract, and racing `tokenstore.Write` +
`target.SetBearer`), and with no stored token BOTH render the connect screen
with no re-probe signal for the second window after the first connects. This is
a genuine fork against a FROZEN contract — resolved by Matt, not this record
(OQ-8).

### A4 — Frame routing: target the originating window, fall back to broadcast

Today the bridge emits each response frame app-wide (`svc.events = app.Event`,
`main.go:120`); `app.Event.Emit` dispatches every custom event to **all**
windows (`transport_event_ipc.go:7-27` — a snapshot of `app.windows`, then
`window.DispatchWailsEvent` per window). Correctness is unaffected — event
names are `"compass_rpc:"+requestId` (`bridge_service.go:134-137`) and only the
originating window holds a listener for its UUID — but with two windows every
frame of every stream (two long-lived `SubscribeComms`/`SubscribeEvents` per
window, plus all unaries) is ExecJS'd into the non-owning window and dropped.

So the bridge targets the originating window: Wails puts the calling window
into the bound method's context under `application.WindowKey`
(`messageprocessor_call.go:16, 134-137`); `CompassRPC` captures it at register
time (`bridge_service.go:145-148, 173-183`) and emits that call's frames via
the window's `DispatchWailsEvent` (`webview_window.go:1372`, per-window
delivery), falling back to the existing app-wide `Emit` when no window is in
context (windowless transports, tests). This is byte routing, not RPC logic —
thin-shell safe — and stays behind the existing `eventEmitter` seam
(`bridge_service.go:30-36`) so the fake-emitter tests keep working.

**Destroyed-window frames are intentionally dropped.** `DispatchWailsEvent`
silently no-ops once a window `isDestroyed()` (`webview_window.go:1373-1375`),
so a terminal frame for a call whose window has closed is dropped rather than
broadcast — correct (no listener survives), but M3 must pin it with a
destroyed-window test row rather than inherit it, because that silent drop is
also what hides the F-close leak below.

**Window close leaks the closed window's streams (must be handled — see M3b).**
The bridge tears an in-flight call down ONLY on a terminal frame or an explicit
`compass_rpc_cancel` (`bridge_service.go` register/run doc; contexts are
`WithoutCancel`-derived), and the UI cancel path
(`daemon-transport.ts:94-140`) fires from JS in the webview. A destroyed window
kills its JS context without that path firing, so its two app-lifetime streams
(`SubscribeComms` + `SubscribeEvents`, aborted only on root dispose which
"in the app runs forever", `store.ts:893-935`) never send cancel — their pump
goroutines and the SERVER-side subscriptions live until app exit. In the
single-window app this was moot (last-window-close quits); with A2's unbounded
workspace windows it is a real leak (N closed windows → 2N zombie server
subscriptions). The fix is thin-shell-safe: M3 already gives the bridge a
per-call window handle, so on the window-closing event the shell cancels every
in-flight call registered to that window — pure teardown, no compass.v1
knowledge (M3b).

### Alternatives considered

**Shared frontend state — one store/comms stream fanned to both windows
(rejected).** The superficially attractive reading of "one bridge connection +
one store": the shell subscribes once and pushes reduced state to both
webviews. Rejected on three grounds. (1) It requires the shell to speak
compass.v1 — subscribe, reduce, re-serialize — violating the thin-shell
invariant verbatim (`compass-native-app/design.md:330-334`). (2) There is no
JS-level alternative: separate webview windows are separate JS runtimes; the
store singleton and its `createRoot` owner (`index.tsx:83-113`) cannot span
them. (3) The saving is two server-side stream subscriptions and one WhoAmI —
trivial against a local daemon (embedded) and small against a remote one
(client mode), while the cost is a new shell-resident state layer with its own
consistency bugs. Per-window boot keeps the UI's contract exactly as-built.

The stronger form of the alternative — a dumb byte-tee that coalesces the two
windows' identical stream requests and fans the raw response BYTES to both,
parsing nothing — fails on its own terms too. Coalescing requires deciding two
requests are "the same subscription," i.e. keying on compass.v1 method paths +
request bodies (method-aware routing the invariant's "no compass.v1 command
logic" forbids); and fatally, a window opened mid-stream needs the
subscription's initial snapshot, which for `SubscribeComms` exists only as the
stream's first push — a byte-tee cannot replay it without buffering and
understanding frame boundaries, at which point it IS a compass.v1 reducer. So
even the steelman collapses into a thin-shell violation for a two-subscription
saving.

**Single window at startup + New Window menu only (rejected as default).**
Simpler shell diff, but it makes Matt's described working set a manual two-step
on every launch. The two-window startup IS the product ask; the menu exists in
addition, not instead. (OQ-2 carries this for Matt's confirmation.)

## Global Constraints

- **Wails v3 pinned at `v3.0.0-beta.0`** (`go/go.mod:29`, imported at
  `go/cmd/compass-app/main.go:32`); every window API used here — `Window
  Manager.NewWithOptions`/`GetByName` (`window_manager.go:17-27,54`),
  `WebviewWindowOptions.Name/URL/JS` (`webview_window_options.go:44-60,
  119-120`), `WindowKey` context (`messageprocessor_call.go:16,134-137`),
  per-window `DispatchWailsEvent` (`webview_window.go:1372`) — exists at this
  pin. A beta bump re-verifies these seams before merge.
- **Thin-shell invariant** (`compass-native-app/design.md:330-334`): the shell
  owns windows + spawn/supervise + bridge only; no compass.v1 logic enters the
  shell. Frame routing by originating window (A4) moves bytes, never parses
  them.
- **No mode divergence above the transport boundary** (DL-106,
  `compass-native-app/design.md:335-338`): multi-window adds no new UI-side
  conditional; `apps/ui` is unchanged by this record.
- **T5 mode-injection contract is frozen**
  (`compass-native-client-mode/design.md:709-730`): `window.__COMPASS_MODE__`
  is injected at window creation for EVERY window, via the single window
  factory — never per-window-divergent.
- **Go 1.25 floor, one Go module** (`compass-native-app/design.md:327-329`);
  all shell changes stay in `go/cmd/compass-app` (`//go:build unix && gtk3`,
  `main.go:1`), with the `main_nogtk3.go` stub untouched in behavior.
- **Quit/linger semantics are DL-108's, unchanged** (`main.go:122-131`):
  last-window-close quits the app, the stack lingers; only "Quit and stop
  stack" tears the stack down.

## Plan

Dependency order: M1 → M2 → M3 → M3b; M4 gates. Slices are Go-shell only
(`go/cmd/compass-app`) EXCEPT any UI task OQ-8 adds for the client-mode
two-window connect gate (embedded mode needs no `apps/ui` change).

### M1 — Window factory + two-window startup

- **Do:** extract the inline window creation (`main.go:142-147`) into a
  `newAppWindow(app, name, title, hashRoute string)` helper in
  `go/cmd/compass-app` that builds the shared `WebviewWindowOptions` (size,
  `Name`, `URL: "/"+hashRoute` where the board's route is empty and the
  workspace's is `#/backlog`, and — once T5.6 has landed — the same startup
  `JS` mode injection for every window). `run()` calls it twice at startup:
  `newAppWindow(app, "board", "Compass — Board", "")` and
  `newAppWindow(app, "workspace", "Compass — Workspaces", "#/backlog")`.
- **Interfaces:** consumes `*application.App` (`main.go:108-117`),
  `application.WebviewWindowOptions{Name, Title, Width, Height, URL, JS}`
  (`webview_window_options.go:44-60,119-120`). Produces
  `newAppWindow(app *application.App, name, title, hashRoute string) *application.WebviewWindow`
  and the two startup calls replacing `main.go:142-147`.
- **Test cycle:** a unit test on the options builder (route → URL mapping,
  name uniqueness, identical injection across windows); manual smoke: launch
  opens two windows, board on `/`, workspace on `#/backlog`, both live against
  the daemon.

### M2 — Window menu: New Board Window (dedup+focus) / New Workspace Window

- **Do:** add a `Window` submenu beside the existing `File` menu
  (`main.go:132-140`). "New Board Window": `app.Window.GetByName("board")`
  (`window_manager.go:17-27`) → `Focus()` (`webview_window.go:1467`) if
  present, else `newAppWindow(app, "board", …)`. "New Workspace Window":
  always `newAppWindow` with a suffixed unique name (`workspace-2`, … — probe
  `GetByName` for the first free suffix).
- **Interfaces:** consumes M1's `newAppWindow`, `application.NewMenu`/
  `AddSubmenu`/`OnClick` (as at `main.go:132-140`),
  `WindowManager.GetByName`. Produces the menu wiring inside `run()`; no new
  exported symbols.
- **Test cycle:** unit test on the name-suffix allocator; manual smoke: board
  dedup focuses instead of duplicating, workspace opens N.

### M3 — Per-window frame routing in the bridge service

- **Do:** in `CompassRPC` (`bridge_service.go:145-148`), read the originating
  window from `ctx.Value(application.WindowKey)`
  (`messageprocessor_call.go:134-137`), store it on the `inflightCall`
  (`bridge_service.go:59-65`), and emit that call's frames through the
  window's `DispatchWailsEvent` (`webview_window.go:1372`) instead of the
  app-wide `Emit` — behind the existing `eventEmitter` seam
  (`bridge_service.go:30-36`), widened to a per-call frame sink so a nil/absent
  window falls back to the current app-wide `svc.events.Emit`
  (`main.go:120`). `CompassRPCCancel` is unchanged (id-keyed,
  `bridge_service.go:153-161`).
- **Interfaces:** consumes `application.WindowKey`
  (`messageprocessor_call.go:16`), `application.Window.DispatchWailsEvent`
  (`window.go:13`). Produces the routed emit path in `bridge_service.go`; the
  `newBridgeService(pump, events)` constructor signature is preserved for the
  fallback sink.
- **Test cycle:** extend `bridge_service_test.go` — a fake context carrying a
  fake window records per-window delivery; the existing no-window tests
  (`bridge_service_test.go:39-52`) keep passing via the fallback; the
  concurrent-distinct-ids test (`:439-490`) gains a two-window arm asserting
  no cross-window frame delivery; and a destroyed-window row asserting a frame
  for a closed window's call is dropped (not fallback-broadcast), pinning the
  `isDestroyed()` no-op (A4) rather than inheriting it.

### M3b — Close-time cancel of a window's in-flight calls

- **Do:** on the window-closing event, the shell cancels every in-flight
  bridge call registered to that window (the per-call window handle M3 stores
  on the `inflightCall` is the index), driving the same teardown path as an
  explicit `compass_rpc_cancel` so the pump goroutine and the server-side
  subscription terminate. Pure teardown keyed on the window handle — no
  compass.v1 knowledge enters the shell (thin-shell safe). Without this a
  closed workspace window leaks its two app-lifetime streams for app lifetime
  (A4, F-close), and A2 allows unbounded workspace windows.
- **Interfaces:** consumes the window-closing event
  (`application.WindowClosing`/GTK3 close signal, verify at the pin) and M3's
  per-call window index; produces the close handler and a bridge method to
  cancel-all-for-window. `CompassRPCCancel`'s id-keyed teardown is the shared
  sink.

### M4 — Multi-window smoke gate

- **Do:** extend the native-app e2e/smoke surface (the T5.7 gate's harness, or
  the manual checklist if the harness is script-only): launch → two windows;
  each boots its own store (two WhoAmI, two comms subscriptions observed
  daemon-side); a message posted in one window renders in the other via its
  own stream push; board-window close leaves the workspace window live;
  **on that close the daemon observes BOTH the closed window's subscriptions
  terminate** (the M3b teardown — the leak gate, not just which window
  survives); last-window close quits with the stack lingering
  (`compass-stack status` still up).
- **Interfaces:** consumes M1-M3b; produces the gate script/checklist under the
  same home as the T5.7 gate (its exact path is T5.7's, in flight — bind at
  implementation time).

## Tasks

- [ ] **M1** Window factory `newAppWindow` + two-window startup (board `/`,
  workspace `#/backlog`), identical option/injection surface per window.
- [ ] **M2** Window menu: New Board Window (GetByName dedup + Focus), New
  Workspace Window (suffixed names, unbounded).
- [ ] **M3** Bridge frames routed to the originating window via
  `application.WindowKey` + `DispatchWailsEvent`, app-wide fallback preserved;
  two-window no-cross-delivery test + destroyed-window drop test.
- [ ] **M3b** Close-time cancel of a window's in-flight calls (shell cancels
  every call registered to the closing window, terminating pump + server
  subscription); thin-shell-safe teardown, no compass.v1 knowledge.
- [ ] **M4** Multi-window smoke gate: independent boots, cross-window
  convergence via streams, close semantics, **daemon-side subscription teardown
  on window close**, linger preserved.

## Ledger-impact

**None proposed.** This record refines the frozen native-app shell rows rather
than deciding a new contract class: DL-106 (one binary/two modes — untouched;
multi-window is mode-agnostic by A3), DL-107 (the frame contract — unchanged;
A4 changes delivery targeting, not the frame shape), DL-108 (linger — A2 keeps
it verbatim), DL-110 (Wails v3 — this exercises more of the same pin). The
two-window default and the window-menu shape are product-surface choices under
the existing shell charter, not new decision rows. Suggested commit-body line:
`Ledger-impact: none — refines DL-106/107/108/110; no new decision row`.
If Matt instead wants the two-window default recorded as a ledger decision
(it IS a user-facing default), that is OQ-2's call — the row would be a new
DL under "Native app" citing this record's §A2.

## Open Questions

**Load-bearing (block freeze):**

- **OQ-1 — Shared backend vs per-window independent boot. RECOMMEND:
  per-window independent boot over the one shared Go bridge (A1).** Each
  window boots the unchanged `index.tsx` chain — own store, own QueryClient,
  own comms/events subscriptions, own WhoAmI — all multiplexed over the single
  app-wide `bridgeService`/pump/socket. The rejected alternative (shell fans
  one comms stream to both webviews) requires compass.v1 logic in the shell,
  violating the thin-shell invariant (`compass-native-app/design.md:330-334`),
  and buys only two server subscriptions + one WhoAmI. Costs of the
  recommendation: duplicate subscriptions and transiently divergent caches,
  both bounded — every stream push replaces comms state wholesale
  (`store.ts:823-834`), so windows converge per push. *(shapes every task; the
  record is written against this answer)*
- **OQ-2 — Startup default: auto-open both windows vs single window + menu vs
  persisted window-set restore. RECOMMEND: auto-open both (board + workspace),
  menu in addition (A2).** Matt's ask reads as a fixed two-window working set;
  auto-open makes it the zero-step default, and the `*` route fallback
  (`routes.tsx:37`) keeps a bad route safe. Persisted window-set restore
  (remember geometry/routes across runs) is strictly more machinery
  (state-dir schema, stale-route handling) for a working set that is
  constant; it can layer on later without contract change. *(decides M1's
  startup calls)*
- **OQ-3 — The workspace window's initial route. RECOMMEND: `#/backlog`.**
  The route table (`routes.tsx:30-37`) offers `/backlog`, `/done`,
  `/channel/:channelId`, `/agent/:agentId`. A concrete channel/agent id cannot
  be a startup constant (ids are deployment data); `/backlog` is the stable
  work-navigation surface. If Matt prefers "first subscribed channel", that is
  UI-side post-boot navigation (the store already computes
  `firstChannelId`, `store.ts:849-852`) and would need a small UI task this
  record currently avoids. *(decides M1's second startup call)*
- **OQ-8 — Two-window startup × the frozen T5 client-mode connect gate
  (crosses a FROZEN contract — Matt's call). RECOMMEND: option (a), client mode
  opens ONE window until connected, then opens the second on Connect success.**
  A2 opens both windows immediately; in client mode each independently runs the
  T5.5 boot gate, but frozen T5 mandates exactly ONE auto-connect probe per
  launch (`compass-native-client-mode/design.md:180-186`, :550-553). Two windows
  → two concurrent `shellConnect("")` probes (contradicting the contract, racing
  `tokenstore.Write`/`SetBearer`), and with no stored token both show a connect
  screen with no re-probe path for the second window after the first connects.
  The options (each with a different blast radius):
  (a) **one window until connected, second opens on Connect success** — a small
  shell change (the window factory gains a post-connect second-open path),
  preserves the single-probe contract, no `apps/ui` change, no frozen-contract
  amendment; RECOMMENDED.
  (b) **both windows open; the second renders "waiting for connect" and
  re-resolves when the shell reports armed** — needs a NEW shell→UI IPC signal
  and a UI task (falsifies "embedded-only UI change"), a real T5-contract touch.
  (c) **keep two probes; amend the frozen T5 record to one-probe-PER-window with
  an idempotent Connect** — a change to a FROZEN contract, which by the freeze
  rule requires its own design-record amendment PR before impl.
  This blocks freeze: whichever Matt picks decides M1's client-mode startup and
  whether a UI task exists. (Embedded mode is unaffected — no connect gate.)

**Non-load-bearing (explicit deferrals):**

- **OQ-4 — `SingleInstance` adoption** (second `compass-app` process focuses
  the existing board window via `OnSecondInstanceLaunch`,
  `single_instance.go:28-46`): deferred — orthogonal to in-app multi-window;
  today a second launch is a second app instance, unchanged from the
  single-window shell. Its own small record/task when desktop-launcher
  integration lands.
- **OQ-5 — Window geometry/position persistence** (restore each window's
  size/screen across runs): deferred — pure polish over A2's fixed defaults;
  needs a state-dir schema decision that shouldn't gate the working set.
- **OQ-6 — Per-window title reflecting the active route** (workspace window
  titled by its current channel/agent): deferred — needs a UI→shell title
  channel (Wails runtime `Window.SetTitle` from JS is available); cosmetic.
- **OQ-7 — Live cross-window pref sync** (a `storage`-event listener in the
  store, or a shell relay, so a pin/unpin in one window reflects in the other):
  deferred — today pins are boot-loaded and rewritten wholesale with no storage
  listener (`store.ts:784-785, 1908, 1917`), so two live windows clobber each
  other last-writer-wins (A1). Accepted as a per-window-independence cost; live
  sync is a small UI task that shouldn't gate the working set.
