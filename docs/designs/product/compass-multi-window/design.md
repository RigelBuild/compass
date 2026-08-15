# Compass Native App — Multi-Window Support

Status: Draft
Parent: [compass-native-app/design.md](../compass-native-app/design.md) (DL-106..DL-112)
Composes with: [compass-native-client-mode/design.md](../compass-native-client-mode/design.md) (frozen T5)

## Problem / Intent

Matt runs a multi-window desktop setup and wants the Compass native app to
support it: open more than one Compass window, each an independent view onto the
same Bridge, and reopen last run's set of windows next launch. Today the shell
opens exactly one window — `go/cmd/compass-app/main.go:128-137`, a single
`app.Window.NewWithOptions(application.WebviewWindowOptions{... URL: "/"})`.
Wails v3 supports N windows natively (`WindowManager.NewWithOptions`,
`wails/v3@v3.0.0-beta.0/pkg/application/window_manager.go:54`, callable
repeatedly); this record designs HOW Compass uses that — window topology,
per-window boot semantics, bridge frame routing, and composition with the frozen
T5 mode-injection contract. The startup default stays ONE window (no user
expects an app to open several from the jump); a Window menu opens more, and the
set is persisted across runs.

## Approach

### A1 — Per-window independent UI boot over the one shared shell bridge (the fork, resolved)

Each Wails webview window is a separate browser context — a separate JS runtime,
so a separate SolidJS boot. The chosen shape: **every window runs the existing
boot chain unchanged and independently** (connection resolve → `bootCaller`
WhoAmI → own `QueryClient` → own app-lifetime store → own `SubscribeComms` +
`SubscribeEvents` streams; `apps/ui/src/index.tsx:32-127`), all over the **same
single Go-side bridge**: one `bridgeService` + one pump registered app-wide as a
Wails service (`main.go:95-96`), one daemon socket, shared by all windows.

Why this is correct, grounded in the wiring as-built:

- **The Go side is already shared and window-agnostic.** The bridge service is
  bound once in `application.Options.Services` (`main.go:95-96`) and keyed
  purely by `requestId` (`bridge_service.go:290-300` register, in-flight map
  `bridge_service.go:79-80`). Request ids are `crypto.randomUUID()` minted
  per-call in the UI transport (`apps/ui/src/daemon-transport.ts:100`), so two
  windows' calls can never collide. N windows against one service needs zero
  contract change — the concurrent-distinct-ids property is already the tested
  invariant (`bridge_service_test.go:442-527`).
- **The JS side cannot be literally shared.** The store is a per-boot
  `createRoot` singleton whose owner scopes the comms stream teardown
  (`index.tsx:83-113`, `store.ts:891-935`); there is no cross-window sharing
  primitive in separate WebKitGTK webview contexts (no SharedWorker across
  Wails windows). "One store for both windows" would have to mean the shell
  fanning one reduced comms stream to both webviews — which puts compass.v1
  stream/reduction logic in the shell, a direct violation of the thin-shell
  invariant (`compass-native-app/design.md:330-334`: the bridge moves bytes and
  holds no compass.v1 command logic).
- **The costs of independence are small and convergent.** Each extra window adds
  one `SubscribeComms` + one `SubscribeEvents` server subscription and one WhoAmI
  unary at boot (`index.tsx:64`). The server already serves N subscribers; each
  stream push replaces the window's comms state wholesale
  (`store.ts:832-834, 869-871`), so all windows converge on every push —
  "divergent caches" is bounded to local-only UI state (in-progress ask
  answers, selection), which is genuinely per-window state anyway.
- **Per-window boot is clean because there is no shared *client* state to
  reconcile.** The state that matters lives server-side: each window's streams
  are replaced wholesale on every push, so windows converge by construction, and
  user preferences (pinned agents and the like) move server-side too — they are
  no longer a `localStorage` value two live windows race to overwrite. With prefs
  held by the server, there is nothing for the shell or a cross-window client
  relay to coordinate: no `BroadcastChannel`, no shell fan-out, no `storage`
  listener. This is a dependency on a separate change (pinned-agent prefs move
  server-side — its own record/task, not this one); until it lands, pins stay
  window-local, which is an accepted transitional cost, not a coherence
  guarantee this record owes.
- **Nothing above the transport boundary changes for embedded mode.** The UI
  keeps its one sanctioned mode divergence — which `ConnectionProvider` boot
  installs (`apps/ui/src/live/provider.ts:1-14`) — and multi-window adds no
  second one. In embedded mode `apps/ui` needs **zero changes** for this record.
  The client-mode connect gate composes cleanly too: with the one-window startup
  default there is exactly one auto-connect probe at boot, and a later window
  inherits the already-armed shell connection (A3), so no `apps/ui` change is
  required for the connect flow either.

### A2 — Window topology: one Bridge window at startup, a Window menu (both modes), a persisted set

- **One window kind: Bridge.** There is no board-vs-workspace split. Every window
  — startup or menu-opened — boots on `/`, the Bridge board route
  (`apps/ui/src/routes.tsx:30`), and the user navigates from there (the router is
  `HashRouter`, `index.tsx:119`, so any in-app route lives after `#` and is
  reached by navigation, not a startup URL). Window creation is factored into one
  helper so every call shares options (assets, mode injection, size) — replacing
  the single inline `NewWithOptions` at `main.go:128-137`.
- **Startup default: exactly ONE window.** No user expects an app to open several
  windows from the jump, so first-ever launch (no persisted set) opens a single
  Bridge window (`URL: "/"`), exactly as the shell does today. Additional windows
  are opened deliberately, via the Window menu.
- **Persist the window-set across runs.** The shell remembers the SET of windows
  open at last exit and reopens that set at next launch (all Bridge windows, so
  the set is just a count/identity list, not per-window routes). First-ever run,
  or a run with an empty/absent persisted set, falls back to the one-window
  default above. Per-window geometry/position is NOT part of this (that stays a
  deferral, OQ-5) — the window-SET is persisted, window placement is not.
- **Window identity via `Name`.** `WebviewWindowOptions.Name`
  (`webview_window_options.go:44`) names each window. Since all windows are the
  same kind, "New Window" always opens a fresh Bridge window with a unique
  suffixed name (`bridge-2`, … — probe `app.Window.GetByName`,
  `window_manager.go:17`, for the first free suffix); there is no dedup-and-focus
  special case, because there is no singleton window kind to dedup against.
  `Focus()` (`webview_window.go:1467`) remains available for a future
  focus-existing affordance but is not wired by this record.
- **Menu: a Window submenu, installed in BOTH modes.** `Window → New Window`
  opens another Bridge window via the factory. On the current baseline the ENTIRE
  menu is built only inside the `if quitter != nil` guard (`main.go:111-121`), so
  client mode (nil quitter) installs NO menu at all. Because the Window menu is
  now the surface for opening windows in BOTH modes, its construction lifts OUT of
  that guard (M2); the embedded-only `File → Quit and stop stack` item stays
  INSIDE the guard, since only embedded owns a stack to stop. The earlier claim
  that "the File menu is app-level and unchanged" was wrong — on the baseline the
  File menu is embedded-only.
- **Quit semantics unchanged.** On Linux/GTK3 the app quits when the last
  window closes (`application_linux_gtk3.go:194-197`; opt-out is
  `LinuxOptions.DisableQuitOnLastWindowClosed`,
  `application_options.go:310-311` — not taken). Closing one window leaves the
  others running; closing the last quits the app with the stack lingering, exactly
  the DL-108 linger contract (`main.go:106-110`).
- **`OnSecondInstanceLaunch` is out of scope.** It handles a second *process*
  launch (`single_instance.go:28-46`), not in-app duplicate windows; adopting
  `SingleInstance` (a second `compass-app` launch focuses an existing window) is a
  deferral, not part of this record (see Open Questions).

### A3 — Composition with T5 mode injection and the connect gate

The frozen T5 record injects the launch mode as a startup global
(`window.__COMPASS_MODE__ = "embedded" | "client"`) **at window creation**, via
the `WebviewWindowOptions` in `run()`
(`compass-native-client-mode/design.md:709-730` OQ-8; T5.6 at `:554-574`).
Multi-window composes by construction: the window factory (A2) is the single
place `WebviewWindowOptions` is built, so the same `JS` startup injection
(`webview_window_options.go:119`) — carrying mode and, in client mode, serverURL
— is applied to every window from the one factory; a per-window mode-injection
divergence is therefore structurally impossible and no new mode surface is
added.

**The client-mode connect gate composes too — the one-window default resolves
it.** The T5.5 connect gate is per-window and stateful, and the frozen T5 record
mandates exactly ONE auto-connect probe per launch
(`compass-native-client-mode/design.md:180-186`, :550-553). With the startup
default of exactly ONE window (A2), there is exactly one auto-connect probe at
boot — the frozen single-probe contract is honored with no amendment. A SECOND
window, opened later via the Window menu or restored from the persisted set, does
NOT re-probe: by the time any second window can exist the shell is already
connected and its target armed (`SetBearer` persisted, `bridge_service.go:227`),
so the new window inherits the armed connection and boots straight to Bridge. The
connect gate thus runs only in the first/boot window; a menu-opened client-mode
window skips it because it is already connected. A later window cannot open
before the first connects — the menu that opens it only exists after boot — so
there is no window that races the initial probe. The earlier framing of this as a
load-bearing open question (two startup windows → two concurrent
`shellConnect("")` probes racing `tokenstore.Write`/`SetBearer`) is dissolved by
the one-window default: the collision it described cannot arise.

### A4 — Frame routing: target the originating window, fall back to broadcast

Today the bridge emits each response frame app-wide (`svc.events = app.Event`,
`main.go:104`); `app.Event.Emit` dispatches every custom event to **all**
windows (`transport_event_ipc.go:7-27` — a snapshot of `app.windows`, then
`window.DispatchWailsEvent` per window). Correctness is unaffected — event
names are `"compass_rpc:"+requestId` (`bridge_service.go:10-11, 164, 310`) and
only the originating window holds a listener for its UUID — but with more than
one window every frame of every stream (a long-lived
`SubscribeComms`/`SubscribeEvents` pair per window, plus all unaries) is ExecJS'd
into every non-owning window and dropped.

So the bridge targets the originating window: Wails puts the calling window
into the bound method's context under `application.WindowKey`
(`messageprocessor_call.go:16, 134`); `CompassRPC`/`register` captures it at
register time (`bridge_service.go:172-175, 290-300`) and emits that call's frames
via the window's `DispatchWailsEvent` (`webview_window.go:1372`, per-window
delivery), falling back to the existing app-wide `Emit` when no window is in
context (windowless transports, tests). This is byte routing, not RPC logic —
thin-shell safe — and stays behind the existing `eventEmitter` seam
(`bridge_service.go:42-45`) so the fake-emitter tests keep working.

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
"in the app runs forever", `store.ts:891-935`) never send cancel — their pump
goroutines and the SERVER-side subscriptions live until app exit. In the
single-window app this was moot (last-window-close quits); with A2's user-opened
extra windows it is a real leak (N closed windows → 2N zombie server
subscriptions). The fix is thin-shell-safe: M3 already gives the bridge a
per-call window handle, so on the window-closing event the shell cancels every
in-flight call registered to that window — pure teardown, no compass.v1
knowledge (M3b).

### Alternatives considered

**Shared frontend state — one store/comms stream fanned to all windows
(rejected).** The superficially attractive reading of "one bridge connection +
one store": the shell subscribes once and pushes reduced state to every
webview. Rejected on three grounds. (1) It requires the shell to speak
compass.v1 — subscribe, reduce, re-serialize — violating the thin-shell
invariant verbatim (`compass-native-app/design.md:330-334`). (2) There is no
JS-level alternative: separate webview windows are separate JS runtimes; the
store singleton and its `createRoot` owner (`index.tsx:83-113`) cannot span
them. (3) The saving is a few server-side stream subscriptions and one WhoAmI
per window — trivial against a local daemon (embedded) and small against a
remote one (client mode), while the cost is a new shell-resident state layer
with its own consistency bugs. Per-window boot keeps the UI's contract exactly
as-built, and (with prefs server-side, A1) leaves no shared *client* state for
such a layer to reconcile in the first place.

The stronger form of the alternative — a dumb byte-tee that coalesces windows'
identical stream requests and fans the raw response BYTES to all, parsing
nothing — fails on its own terms too. Coalescing requires deciding two
requests are "the same subscription," i.e. keying on compass.v1 method paths +
request bodies (method-aware routing the invariant's "no compass.v1 command
logic" forbids); and fatally, a window opened mid-stream needs the
subscription's initial snapshot, which for `SubscribeComms` exists only as the
stream's first push — a byte-tee cannot replay it without buffering and
understanding frame boundaries, at which point it IS a compass.v1 reducer. So
even the steelman collapses into a thin-shell violation for a per-window
subscription saving.

**Auto-opening a fixed multi-window set at startup (rejected as default).** An
earlier draft opened a fixed working set (a board window plus a workspace
window) at every launch. Matt ruled it out: no user expects an app to open
several windows from the jump. The startup default is exactly one Bridge window;
the Window menu opens more on demand, and the persisted window-set (A2) restores
whatever the user actually had open last run — which gives the "reopen my working
set" behavior without imposing a multi-window default on a fresh install.

## Global Constraints

- **Wails v3 pinned at `v3.0.0-beta.0`** (`go/go.mod:29`, imported at
  `go/cmd/compass-app/main.go:34`); every window API used here —
  `WindowManager.NewWithOptions`/`GetByName` (`window_manager.go:17,54`),
  `WebviewWindowOptions.Name/URL/JS` (`webview_window_options.go:44,59,119`),
  `WindowKey` context (`messageprocessor_call.go:16,134`),
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
- **Quit/linger semantics are DL-108's, unchanged** (`main.go:106-110`):
  last-window-close quits the app, the stack lingers; only "Quit and stop
  stack" tears the stack down.

## Plan

Dependency order: M1 → M2 → M3 → M3b; M4 gates. Every slice is Go-shell only
(`go/cmd/compass-app`); embedded and client mode both need only shell changes,
and `apps/ui` is untouched by this record (A1, A3).

### M1 — Window factory + one-window startup + persisted-set restore

- **Do:** extract the inline window creation (`main.go:128-137`) into a
  `newAppWindow(app, name, title string)` helper in `go/cmd/compass-app` that
  builds the shared `WebviewWindowOptions` (size, `Name`, `URL: "/"`, and the
  existing startup `JS` mode injection). T5.6 has already landed, so
  `JS: startupJS` is present on the current inline options (`main.go:136`); the
  factory carries that same field rather than adding it later. All windows are
  Bridge (`URL: "/"`), so the factory takes NO route parameter — the board is
  `/` and any deeper route is reached by in-app navigation (A2). At startup
  `run()` restores the persisted window-set: for each remembered window it calls
  `newAppWindow`; if the set is empty or absent (first-ever run), it opens
  exactly ONE window, `newAppWindow(app, "bridge", "Compass")`. On exit it
  persists the current set (identities/count) to the app state dir.
- **Interfaces:** consumes `*application.App` (`main.go:92-101`),
  `application.WebviewWindowOptions{Name, Title, Width, Height, URL, JS}`
  (`webview_window_options.go:44,59,119`) and the resolved `startupJS`
  (`main.go:124-137`). Produces
  `newAppWindow(app *application.App, name, title string) *application.WebviewWindow`,
  the persisted-set load/store (a small state-dir file), and the startup restore
  logic replacing the single inline `NewWithOptions` at `main.go:128-137`.
- **Test cycle:** a unit test on the options builder (every window yields
  `URL: "/"`, unique `Name`, identical `JS` injection) and on the persisted-set
  round-trip (empty set → one window; N-entry set → N windows); manual smoke:
  first-ever launch opens one Bridge window; after opening a second via the menu
  and relaunching, both reopen.

### M2 — Window menu (both modes): New Window

- **Do:** install a `Window` submenu with a single `New Window` item that opens
  a fresh Bridge window via M1's `newAppWindow` under a unique suffixed name
  (`bridge-2`, … — probe `app.Window.GetByName`, `window_manager.go:17`, for the
  first free suffix). On the current baseline the entire menu is built only
  inside `if quitter != nil` (`main.go:111-121`), so client mode installs no menu
  at all. Lift the menu construction OUT of that guard so a `Window` menu is
  installed in BOTH modes; keep the embedded-only `File → Quit and stop stack`
  item INSIDE the guard (only embedded owns a stack to stop, `main.go:115-119`),
  adding the `File` submenu to the shared menu only when `quitter != nil`.
- **Interfaces:** consumes M1's `newAppWindow`,
  `application.NewMenu`/`AddSubmenu`/`Add`/`OnClick` (as at `main.go:113-121`),
  `WindowManager.GetByName` (`window_manager.go:17`), and `app.Menu.Set`
  (`main.go:121`). Produces the menu wiring inside `run()`, restructured so menu
  construction and `app.Menu.Set` run unconditionally while the `File` submenu is
  gated on `quitter != nil`; no new exported symbols.
- **Test cycle:** unit test on the name-suffix allocator; manual smoke: in both
  embedded and client mode a `Window → New Window` opens another Bridge window;
  embedded still shows `File → Quit and stop stack`, client shows no `File` menu.

### M3 — Per-window frame routing in the bridge service

- **Do:** in `register` (`bridge_service.go:290-300`, called by `CompassRPC`
  `:172-175`), read the originating window from `ctx.Value(application.WindowKey)`
  (`messageprocessor_call.go:134`), store it on the `inflightCall`
  (`bridge_service.go:87-89`), and emit that call's frames through the window's
  `DispatchWailsEvent` (`webview_window.go:1372`) instead of the app-wide `Emit`
  — behind the existing `eventEmitter` seam (`bridge_service.go:42-45`), widened
  to a per-call frame sink so a nil/absent window falls back to the current
  app-wide `svc.events.Emit` (`main.go:104`, emit site `bridge_service.go:317`).
  `CompassRPCCancel` is unchanged (id-keyed, `bridge_service.go:180-188`).
- **Interfaces:** consumes `application.WindowKey`
  (`messageprocessor_call.go:16`), `application.Window.DispatchWailsEvent`
  (`webview_window.go:1372`). Produces the routed emit path in
  `bridge_service.go`. The window handle is captured from `ctx` inside `register`,
  NOT passed to the constructor — the 4-arg
  `newBridgeService(pump *bridge.Pump, events eventEmitter, target *bridge.Target, tokens tokenstore.Store)`
  signature (`bridge_service.go:94`) is unchanged; per-window routing is added
  without touching it.
- **Test cycle:** extend `bridge_service_test.go` — a fake context carrying a
  fake window records per-window delivery; the existing no-window tests
  (the `fakeEmitter` path, `bridge_service_test.go:44-59`) keep passing via the
  fallback; the concurrent-distinct-ids test (`:442-527`) gains a two-window arm
  asserting no cross-window frame delivery; and a destroyed-window row asserting
  a frame for a closed window's call is dropped (not fallback-broadcast), pinning
  the `isDestroyed()` no-op (A4) rather than inheriting it.

### M3b — Close-time cancel of a window's in-flight calls

- **Do:** on the window-closing event, the shell cancels every in-flight
  bridge call registered to that window (the per-call window handle M3 stores
  on the `inflightCall` is the index), driving the same teardown path as an
  explicit `compass_rpc_cancel` so the pump goroutine and the server-side
  subscription terminate. Pure teardown keyed on the window handle — no
  compass.v1 knowledge enters the shell (thin-shell safe). Without this a
  closed window leaks its two app-lifetime streams for app lifetime
  (A4, F-close), and A2 lets the user open and close arbitrarily many windows.
- **Interfaces:** consumes the window-closing event
  (`application.WindowClosing`/GTK3 close signal, verify at the pin) and M3's
  per-call window index; produces the close handler and a bridge method to
  cancel-all-for-window. `CompassRPCCancel`'s id-keyed teardown is the shared
  sink.

### M4 — Multi-window smoke gate

- **Do:** extend the native-app e2e/smoke surface (the T5.7 gate's harness, or
  the manual checklist if the harness is script-only): first-ever launch → ONE
  window on the Bridge board; `Window → New Window` opens a second, which boots
  its own store (a second WhoAmI + comms subscription observed daemon-side); a
  message posted in one window renders in the other via its own stream push;
  relaunch reopens the persisted set (both windows) rather than one; closing one
  window leaves the other live; **on that close the daemon observes the closed
  window's subscriptions terminate** (the M3b teardown — the leak gate, not just
  which window survives); last-window close quits with the stack lingering
  (`compass-stack status` still up).
- **Interfaces:** consumes M1-M3b; produces the gate script/checklist under the
  same home as the T5.7 gate (its exact path is T5.7's, in flight — bind at
  implementation time).

## Tasks

- [ ] **M1** Window factory `newAppWindow` + one-window startup default +
  persisted-set restore (all Bridge, `URL: "/"`), identical option/injection
  surface per window.
- [ ] **M2** Window menu (both modes): `New Window` opens a suffixed Bridge
  window; menu construction lifted out of the `quitter != nil` guard, the
  embedded-only `File → Quit and stop stack` item kept inside it.
- [ ] **M3** Bridge frames routed to the originating window via
  `application.WindowKey` + `DispatchWailsEvent`, app-wide fallback preserved,
  4-arg `newBridgeService` signature unchanged (window captured in `register`);
  two-window no-cross-delivery test + destroyed-window drop test.
- [ ] **M3b** Close-time cancel of a window's in-flight calls (shell cancels
  every call registered to the closing window, terminating pump + server
  subscription); thin-shell-safe teardown, no compass.v1 knowledge.
- [ ] **M4** Multi-window smoke gate: one-window default, menu opens a second,
  persisted set restored on relaunch, cross-window convergence via streams,
  close semantics, **daemon-side subscription teardown on window close**, linger
  preserved.

## Ledger-impact

**None proposed.** This record refines the frozen native-app shell rows rather
than deciding a new contract class: DL-106 (one binary/two modes — untouched;
multi-window is mode-agnostic by A3), DL-107 (the frame contract — unchanged;
A4 changes delivery targeting, not the frame shape), DL-108 (linger — A2 keeps
it verbatim), DL-110 (Wails v3 — this exercises more of the same pin). The
one-window startup default, the Window menu, and the persisted window-set are
product-surface behaviors under the existing native-app shell charter, not new
decision rows. Suggested commit-body line:
`Ledger-impact: none — refines DL-106/107/108/110; no new decision row`.
If Matt judges the persisted-window-set default warrants a ledger decision
(it IS a new user-facing behavior), the row would be a new DL under "Native app"
citing this record's §A2 — flagged here as a one-line question, not decided.

## Open Questions

**Load-bearing:** none open. The forks that once blocked freeze are resolved by
Matt's rulings and folded into the record:

- Shared backend vs per-window independent boot → **per-window independent boot
  over the one shared Go bridge** (A1); prefs move server-side, so there is no
  shared client state to reconcile.
- Startup default → **exactly one Bridge window**; the Window menu opens more
  (A2).
- Per-window initial route → **dissolved**: there is one window kind (Bridge,
  `/`) and no per-window startup route to choose (A2).
- Live cross-window pref sync → **moot**: pinned-agent prefs move server-side, so
  the old `localStorage` last-writer-wins clobber does not arise (A1). The
  server-side prefs move is a named dependency (its own record/task), not this
  record's work.
- Two-window startup × the frozen T5 connect gate → **resolved by the one-window
  default** (A3): one auto-connect probe at boot honors the frozen single-probe
  contract, and a later window inherits the armed shell connection with no second
  probe — no frozen-contract amendment needed.

**Non-load-bearing (explicit deferrals):**

- **OQ-4 — `SingleInstance` adoption** (a second `compass-app` process focuses an
  existing window via `OnSecondInstanceLaunch`, `single_instance.go:28-46`):
  deferred — orthogonal to in-app multi-window; today a second launch is a second
  app instance, unchanged from the single-window shell. Its own small record/task
  when desktop-launcher integration lands.
- **OQ-5 — Per-window geometry/position persistence** (restore each window's
  size/screen across runs): deferred. Note the boundary against A2: the
  window-SET (how many windows, their identities) IS persisted and restored by
  this record (M1); per-window geometry/placement is the piece left out, because
  it needs a richer state-dir schema (coordinates, monitor identity, restore
  semantics on a changed display layout) that shouldn't gate the working set.
- **OQ-6 — Per-window title reflecting the active route** (a window titled by its
  current channel/agent): deferred — needs a UI→shell title channel (Wails
  runtime `Window.SetTitle` from JS is available); cosmetic.
