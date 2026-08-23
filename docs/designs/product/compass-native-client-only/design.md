# Compass native app — client-only (retire embedded mode)

Status: Draft
Linear: RIG-2542
Supersedes: DL-106 (dual-mode charter), DL-215/DL-217 (embedded packaging).
DL-108/DL-109 stay Active with DL-236/DL-237 refining them by citation (partial
supersession, DL-213/DL-183 pattern). See §Ledger-impact for the full
disposition of DL-107..112, DL-183, DL-214..217. Refines nothing in
`compass-native-client-mode/design.md` — that record's client surface is
carried forward whole.

## Problem / Intent

Matt ruled (2026-08-23): **the Compass native app retires embedded mode and
becomes client-only.** The driving constraint is upstream: the container Runner
is being dropped once the microVM Runner (RIG-1717 task I1) lands, and the
microVM path requires a KVM/nested-virt floor — so running any agent locally
becomes KVM-gated where today's rootless-container path was not. The honest
driver is two-fold: (1) the container Runner going away removes embedded mode's
current no-KVM story, and (2) the app's ambition is a cross-platform client
(macOS included), and no Mac without nested virt — nor any locked-down
corporate laptop — can host a local agent under a KVM floor. Carrying a local
supervisor whose one job (run an agent locally) is becoming conditional on a
capability the target machines increasingly lack is cost without a durable
user. (Note the current install base is Linux gtk3 desktops, which commonly DO
expose KVM — so this is a forward-looking retirement driven by the runtime
direction and the macOS target, not a claim that embedded is broken on today's
boxes.) The new shape: `compass-app` is a native **client** that connects over
the authenticated TLS door to a Compass stack running **headless**, brought up
by `compass-stack up` — kept as a standalone CLI, severed from the app. That
stack normally runs on a dedicated KVM-capable machine, but the single-box
developer shape is **not** removed: `compass-stack up` on the same machine with
`server_url = https://localhost:<port>` and the stack's private CA is a
sanctioned client-only configuration (see OQ-6) — the user loses the app's
*zero-config supervision*, not the ability to run everything on one box.

This supersedes the frozen dual-mode charter (DL-106,
`compass-native-app/design.md:98-105` — "One binary, two modes; mode = which
Connection the resolver produces") and the embedded halves of the packaging
(T6) and teardown records. The client mode this record keeps is **already
built end to end** (T5, `compass-native-client-mode/design.md`): connect
screen, keychain bearer, TLS dial, WhoAmI, board-over-bridge. The work here is
DELETION of built, merged, working embedded code plus a re-scope of the
`app-bundle/` packaging lane — sequenced as a clean cutover, no shims.

## Global Constraints

1. **Go module + floor.** One module, `go/go.mod:13`
   (`module github.com/RigelBuild/compass/go`), floor `go 1.25.0`
   (`go/go.mod:15`); the toolchain is proto-shimmed — build/vet only via
   `direnv exec <workspace-root> go …`, never bare `go`.
2. **Wails v3 shell — KEPT (DL-110).** The shell stays
   `go/cmd/compass-app` (gtk3-tagged Wails v3); the frame contract
   (`compass_rpc` + `head|body|end|error`, DL-107's contract half) is
   untouched — `bridge_service.go:8-11`: "the UI issues
   `compass_rpc({requestId, path, headers, body})`; this service forwards the
   call through the pump and streams each ordered response frame back as a
   Wails runtime event".
3. **No cleartext.** `server_url` is https-only —
   `go/internal/appconfig/appconfig.go:121-122`: "validateServerURL enforces
   that a client server_url is an absolute https URL. http and relative URLs
   are rejected"; the TLS target pins TLS 1.3
   (`go/internal/bridge/tls_target.go:36`:
   `cfg := &tls.Config{MinVersion: tls.VersionTLS13}`).
4. **Secret hygiene (DL-109's keychain half, kept).** The bearer lives only in
   the OS keychain (0600 fallback) + the shell-injected header; never in
   app.toml, argv, or the UI `Connection`.
5. **Clean cutover (AGENTS.md).** Migrate every caller; no shims, no aliases,
   no dead `mode="embedded"` branch kept "just in case". The one sanctioned
   residue is a legible *rejection* of a retired config value (error copy is
   not a shim).
6. **The microVM/KVM direction is consumed, not designed.** RIG-1717 / RIG-2394
   own the runtime; this record depends on Matt's ruling that the runtime is
   **KVM-only** (it does not degrade to the container runtime). The newer
   microVM Runner record already reflects KVM-only (`microvm-runner.md:63-65`,
   D2 "sole runtime"; `:211-228` KVM-absent hard-fail); the older
   elastic-runtime record's stale degrade-to-container clauses are reconciled by
   the amendment this PR adds (`microvm-kvm-only-amendment.md`; see OQ-1).
7. **Scripts-over-bash; tests are Go/TS test code; moon-registered CI lanes**
   — inherited from the parent records unchanged.
8. **Design-decision ledger.** This record's rulings land as DL rows in
   `docs/designs/product/DECISIONS.md` in the design PR (proposed rows in
   §Ledger-impact); the driver writes the ledger edit.

## Approach

### A1 — Thesis: `compass-app` is a native-client-only binary

The app keeps exactly the T5 client surface and loses everything else. Client
mode is built and merged end to end (`compass-native-client-mode/design.md`):

- **Mode dispatch exists and already isolates the arms.**
  `go/cmd/compass-app/main.go:240-273` (`launch`) switches on `cfg.Mode`; the
  client arm is self-contained — `main.go:266-269`: "Client mode opens the
  window immediately: no pre-window probe (the single auto-connect is the UI's
  boot-time shellConnect(\"\"), T5.5)" → `runClient(cfg, stateDir)`. The
  embedded arm (`main.go:241-265`) is the only caller of `resolveStackBin`,
  `runEmbedded`, `runStackUp`/`runStackDown`, and
  `bridge.NewUnixTarget(socket)` inside the app.
- **The client launch is already stack-free by design.**
  `embedded.go:94-96`: "resolveStackBin and this controller are embedded
  concerns only: a client-only install has no compass-stack binary and no
  stack to stop, so neither may gate a client launch (design §T5.6)". Deleting
  the embedded arm removes a branch the client arm never depended on.
- **The transport seam (§A2 of the parent, DL-107) is kept whole on the TLS
  side.** `go/internal/bridge/tls_target.go:13-16`: "NewTLSTarget builds a
  Target that dials the daemon's TLS network door (native-client mode) over
  HTTP/2-over-TLS … leaving Pump.Do unchanged." The frame contract, the pump,
  the bridge service, the keychain tokenstore, Connect/WhoAmI, and the
  multi-window surface (DL-160, M1–M4) all survive verbatim.

**`compass-stack` is NOT deleted.** It remains the headless single-user
bring-up on a dedicated KVM-capable machine — the CLI life the parent record
gave it on purpose (`compass-native-app/design.md:169-171`: "`compass-stack up`
is also the headless single-user path (a server box without the app), and the
seam CI smoke tests drive"). What retires is the app's *supervision* of it: the
shell no longer spawns, monitors, or tears down a stack. DL-183's cross-process
teardown mechanism (`stack.pgids`, fresh-`down` signaling) stays with
`compass-stack down` — it is the headless path's quit story now, not the app's.

### A2 — Retire vs keep boundary (explicit)

| Surface | Disposition | Grounding |
| --- | --- | --- |
| `go/cmd/compass-app/embedded.go` — `embeddedPipeline`, `runEmbedded`, `stackUpArgs`/`stackDownArgs`, `runStackUp`/`runStackDown`, `captureStderr`, `whoAmIOverUDS`, `resolveStackBin`, `prependExecDirToPath`, `resolveImage`, `embeddedDatabaseDSN`, `realPreflight`, `classifyPreflight` | **RETIRE** (delete; `runClient` and the shared resolvers move to a client-named file) | `embedded.go:3-16` (file header: "The embedded-mode launch pipeline (SEA-1685 T4.1)"); `runClient` lives here today (`embedded.go:115-147`) and is KEPT |
| `go/cmd/compass-app/main.go` `launch` embedded arm + `bringUpTimeout` + `--socket`/`--image`/`--compass-stack` flag plumbing + `resolveSocket` | **RETIRE** (the switch collapses to the client arm; `default:` rejection stays) | `main.go:240-265`; `main.go:45-49` (`bringUpTimeout` doc: "bounds the whole embedded bring-up") |
| `go/cmd/compass-app/preflight_adapters.go`, `go/cmd/compass-app/lifecycle.go` (+ `lifecycle_test.go`), embedded tests (`embedded_test.go`, `embedded_path_test.go`, `cross-process` podman tests) | **RETIRE (whole files)** — `lifecycle.go` is wholly the embedded `quitController` ("Quit and stop stack" → `compass-stack down`; `//go:build unix && gtk3`, `lifecycle.go:1-16`), not a file with a keep-half | `lifecycle.go:3-16` (file header: embedded QUIT lifecycle on the DL-108 contract); `lifecycle.go:43-49` (`quitController` holds the `stackDown` seam) |
| `go/internal/preflight` (whole package: `preflight.go`, `uid.go`, `doc.go`, tests) | **RETIRE** — orphaned by T-1: its only module-wide importers are `cmd/compass-app/{embedded.go:38, embedded_test.go:36}` + `preflight_adapters.go`, all deleted here | `grep internal/preflight` across `go/` → 3 hits, all in the deleted embedded surface (`embedded.go:38`, `embedded_test.go:36`, `preflight_adapters.go:4`) |
| `go/cmd/compass-app/bridge_service.go`, `window_set.go`, `version.go`, multi-window e2e | **KEEP** — window close-cancel is NOT in `lifecycle.go`; it lives in `bridge_service.go:315-327` (`cancelWindow`) and `main.go:190-204` (`WindowClosing` handler), both kept | `bridge_service.go:3-12`; `bridge_service.go:315-327`; `main.go:190-204`; DL-160/DL-219..229 surfaces |
| `go/internal/appconfig` — `ModeEmbedded`, `modeStrEmbedded`, embedded-default parse arm, `applyOverride`'s embedded arm | **RETIRE** (client becomes the only mode; see A3 step 2) | `appconfig.go:17-25` (`ModeEmbedded Mode = iota` — "the zero-config default"); `appconfig.go:93-95` (`case "", modeStrEmbedded: return Config{Mode: ModeEmbedded}, nil`) |
| `go/internal/appconfig` — `ModeClient`, `parseClient`, `validateServerURL`, unknown-key rejection, `--mode`/`$COMPASS_APP_MODE` override machinery | **KEEP** (simplified: `client` is the only valid mode) | `appconfig.go:106-119`, `:121-122` |
| `go/internal/bridge` — `Pump`, `Target`, `NewTLSTarget`, `SetBearer` | **KEEP** | `tls_target.go:13-16,35-42` |
| `go/internal/bridge.NewUnixTarget` | **RETIRE from the app's production code** — the app's only production caller is the embedded arm (`main.go:263`: `bridge.NewUnixTarget(socket)`). It stays as a hermetic test transport: the pump suite (`pump_test.go:26,90,167,…`) AND two KEPT app-side tests (`bridge_service_test.go:86`, `multiwindow_e2e_test.go:100`) construct it as their h2c-UDS stub. Keep the constructor; demote its doc from "embedded/Dogfood mode" to test-harness status. The T-1 grep-gate is therefore scoped to non-`_test.go` files | `pump.go:77-81` ("Today the only constructor is [NewUnixTarget] (embedded/Dogfood mode…)"), `main.go:263`, `bridge_service_test.go:86`, `multiwindow_e2e_test.go:100` |
| `go/cmd/compass-stack` + `go/internal/stack` (incl. DL-183 pgid teardown) | **KEEP, severed from the app** — standalone headless CLI for the dedicated machine | `compass-native-app/design.md:169-171`; `compass-stack-cross-process-teardown/design.md:5-16` |
| `go/cmd/compass-server`, `compass-runner`, `compass-postgres`, `compass-mint-runner-token`, `compass-gen-cert` | **KEEP** (headless-stack components, untouched by this record) | consumed by `compass-stack`, not the app |
| `app-bundle/` (build.sh, moon.yml, bundle-env.nix, compass.desktop, SMOKE.md) + the `.moon/workspace.yml:40` registration (`compass-app-bundle: 'app-bundle'`) | **RE-SCOPE to a thin client bundle** — drop the four sidecars, the postgres store symlinks (DL-217), and the PATH-threading contract (DL-215); the bundle becomes `compass-app` + `dist/` + desktop file + LICENSE. See A4 step 5 and OQ-3 (headless-stack distribution) | `compass-native-packaging/design.md:156-168` (the staged layout being cut down); `.moon/workspace.yml:40` |
| CI: multi-window gtk3 e2e step | **KEEP** (it exercises the shell/windowing, not the stack) | `ci.yml:328-334`: "The ONE CI lane that compiles + runs the native app" |
| CI: `dogfood-e2e` job | **KEEP, re-labelled** — it stands up its own stack via `compass-stack`/`compass-postgres` and gates the *headless* path now, not the app (`ci.yml:479-480`: "stands up its OWN private postgres (go/cmd/compass-postgres) inside the stack") | `ci.yml:459-523` |
| T6.5 packaged-artifact smoke (`app-bundle/SMOKE.md`, T4-smoke-from-tarball) | **RETARGET** — the packaged smoke becomes: unpack client bundle → launch → connect to a live headless stack → board renders + one agent session runs (on the stack's machine). The embedded-bring-up steps drop out | `compass-native-packaging/design.md:500-516` |
| UI: connect screen, `ShellIpc`, `ConnectionProvider`, `window.__COMPASS_MODE__` startup JS | **KEEP** (the injected global collapses to `"client"` — it is NOT deleted: `apps/ui/src/index.tsx:61-64` forks on it `shellMode() === "client" ? bootNativeClient : bootConnection(envProvider)`, where the else-arm is the browser-dev env provider, not the embedded arm — so the global distinguishes shell-from-browser and deleting it would boot the packaged app into the browser provider. Pin to `"client"`; narrow the TS union as UI follow-through. See OQ-5) | `main.go:279-284` (`shellStartupJS` assigns `window.__COMPASS_MODE__`); `apps/ui/src/index.tsx:61-64` |

### A3 — appconfig: the clean cut (client is the only mode)

Recommended shape (the clean cut, per Global Constraint 5):

- `mode` in app.toml becomes **optional and single-valued**: absent or
  `mode = "client"` → client. `server_url` becomes REQUIRED whenever the app
  runs (today `parseClient` already enforces it, `appconfig.go:107-110`). An
  absent app.toml is now a **legible first-run error** pointing at the connect
  configuration — the zero-config-embedded default (`appconfig.go:18-20`,
  "absent file or mode=\"embedded\" resolves here, so first launch of the
  installed app just works") is retired with its mode. Whether first-run UX
  should instead be an in-app server-URL entry screen is OQ-2.
- `mode = "embedded"` parses to a **legible rejection** naming the retirement
  and pointing at `compass-stack up` on a dedicated machine — an error string,
  not a compatibility arm (Constraint 5's sanctioned residue). The
  `ModeEmbedded` enum value, `modeStrEmbedded`, and `applyOverride`'s
  embedded-clearing behavior (`appconfig.go:196-198`: "An override to embedded
  clears client-only fields") are deleted; the `Mode` type either collapses to
  a validation-only concept or is removed outright (executor's call — with one
  mode, `Config{ServerURL, CACert}` may not need a Mode field at all).
- `--mode`/`$COMPASS_APP_MODE` override plumbing (`main.go` modeFlag,
  `resolveMode`, `applyOverride`) retires with the second mode — an override
  with one valid value is dead weight.

### A4 — Cutover plan (sequenced, clean, no shims)

Ordering rationale: cut the *dispatch* first so every later deletion is
unreachable code; keep CI green at every step; retarget gates before deleting
what they exercised.

1. **Sever the shell from the supervisor** (T-1): collapse `launch` to the
   client arm; delete the embedded pipeline, stack exec seams, the
   `go/internal/preflight` package and its `preflight_adapters.go`, UDS WhoAmI,
   the whole `lifecycle.go` stack-quit controller, and the
   `--socket`/`--image`/`--compass-stack`/`bringUpTimeout` plumbing. The app
   no longer imports `go/internal/stack`-adjacent anything (it never imported
   the package itself — it execs the binary, `embedded.go:12-16`).
2. **Strip appconfig to client-only** (T-2): the A3 shape. All appconfig/table
   tests flip with it. **T-1 and T-2 MUST land in the same PR** (not "may"):
   T-1 alone collapses the `launch` switch while `appconfig.Load` still
   resolves an absent app.toml to `ModeEmbedded` (`appconfig.go:164`), so the
   zero-config path would hit the collapsed switch's `default:` and fail with
   an illegible `unknown app mode Mode(0)` on `main` — and the tests that would
   catch it are deleted in the same breath. Landing the appconfig cut (absent →
   the legible first-run error) in the same PR keeps the zero-config path
   legible at every committed state.
3. **Retire the app-side embedded contract residue** (T-3): demote
   `NewUnixTarget` to test-harness status; delete `resolveSocket`; drop the
   embedded arm from `shellStartupJS` (the UI stops seeing
   `__COMPASS_MODE__ = "embedded"`).
4. **Retarget the smoke gates** (T-4): re-scope `dogfood-e2e`'s framing to the
   headless stack (mechanically it already is — it drives
   `compass-stack`/`compass-postgres`, not the app); rewrite
   `app-bundle/SMOKE.md` as the client-bundle smoke against a live headless
   stack; the multi-window gtk3 e2e keeps gating the shell.
5. **Re-scope `app-bundle/` to the thin client** (T-5): drop sidecar
   staging, postgres symlinks (DL-217), PATH threading (DL-215's threading
   half — `prependExecDirToPath` dies in step 1; the sibling `dist/`
   resolution, `main.go:317-321`, stays). The headless stack's own
   distribution (does `compass-stack`+server+runner+postgres get its own
   tarball lane?) is OQ-3 — this record does not silently orphan it.
6. **Docs + ledger** (T-6): mark the superseded halves of the four frozen
   records per the freeze rule (add, never rewrite frozen prose); land the
   §Ledger-impact rows; close/re-scope RIG-2477 per A5.

Each step is a PR-sized task with its own gate (see §Plan); steps 1–3 are the
load-bearing deletion and MUST land before 5 (the bundle can't drop sidecars
while the shell still spawns one).

### A5 — RIG-2477 disposition

RIG-2477 ("Desktop-app release-artifact distribution … deferred from RIG-1746
releases lane") asked how the *embedded-bundle* tarball — with its
postgres/podman/uid-1000/Linux-only baggage — ships as a release artifact. The
question **collapses**: the client-only bundle is one gtk3 Wails binary +
dist, with no sidecars, no postgres closure, no podman prerequisite, and a
realistic macOS target (the Linux-only-ness was the runner's,
`compass-native-app/design.md:346-349`, and the runner no longer ships in the
app). Disposition: **re-scope RIG-2477** (not close) to "client-app release
artifact: attach the thin client bundle to the RIG-1746 releases, per-OS
matrix (Linux now, macOS as the first new target)". The nix-store-rpath
question (DL-214) softens but does not vanish for the gtk3 Linux build; macOS
packaging becomes the follow-up formerly blocked on "embedded is
Linux-only". The re-scope is written into the issue by the driver; this
record's T-6 carries it.

## Alternatives considered

### Keep dual-mode, KVM-gate the embedded arm (rejected)

Keep `mode="embedded"` and add KVM/nested-virt to the preflight fatal set
(beside OS/UID/podman, `embedded.go:473` "FATAL (host capabilities `up` cannot
create): OS, UID, rootless podman"). Weighed and rejected by Matt: the KVM
floor makes the embedded arm a mode most target machines (laptops, all Macs
without nested virt) can never enter, so the dual-mode charter's zero-config
first-launch promise ("first launch of the installed app just works",
`appconfig.go:18-20`) is already dead — the preflight would be the common
path, not the edge. Carrying the supervisor, the four sidecars, the postgres
closure (DL-217), the uid-1000 constraint, and the cert-rotation lifecycle
(parent §A3) as permanent maintenance for a mode that mostly refuses to run is
cost without a user.

### Client-primary, keep embedded as a dev convenience (rejected)

Default to client, keep `mode="embedded"` undocumented for developers.
Rejected: the dev loop already has a better tool — the devenv chain
(`compass-native-app/design.md:73-74`: "The dogfood devenv loop already IS the
embedded stack, hand-rolled") and `compass-stack up` by hand, both of which
survive this record. A hidden mode is the worst of both: all of dual-mode's
code weight (packaging sidecars, preflight, teardown, mode-conditional tests)
with none of its product claim, and a standing violation of the clean-cutover
rule (a permanent shim). Matt chose client-only.

### Delete `compass-stack` too (rejected)

Out of scope and wrong: the headless single-user path on a dedicated machine
is now the ONLY way a self-hoster runs Compass, and `compass-stack up` is that
path by design (`compass-native-app/design.md:169-171`). Its supervision,
teardown (DL-183), and CI exercise (`dogfood-e2e`) all stay — only the app's
invocation of it goes.

## Plan

Dependency order: (T-1 + T-2 as one PR) → T-3 → (T-4 ∥ T-5) → T-6. T-1 and T-2
are the Go deletion and **MUST share a PR** (see A4: T-1 alone leaves the
zero-config path failing illegibly on `main`); T-4/T-5 retarget gates and
packaging; T-6 is docs/ledger/tracker follow-through.

### T-1 — Sever the shell from the supervisor

- **Do:** collapse `launch` (`go/cmd/compass-app/main.go:237-277`) to the
  client arm: keep `runClient` + the `default:` rejection, delete the
  `appconfig.ModeEmbedded` case and `bringUpTimeout` (`main.go:45-49`). Delete
  from `embedded.go`: `embeddedPipeline`, `embeddedParams`, `runEmbedded`,
  `stackUpArgs`/`stackDownArgs`, `runStackUp`/`runStackDown`, `captureStderr`,
  `whoAmIOverUDS`, `resolveStackBin`, `prependExecDirToPath`, `resolveImage`,
  `embeddedDatabaseDSN`, `realPreflight`, `classifyPreflight`,
  `defaultAgentImage`; move `runClient` (+ `resolveStateDir`, still needed for
  the tokenstore path) into a client-named file (`client.go`) and delete
  `embedded.go`, `preflight_adapters.go`, and the whole `go/internal/preflight`
  package (orphaned once its only importers — the deleted embedded files — are
  gone). Remove the `--socket`/`--image`/`--compass-stack` flags and
  `resolveSocket` (`main.go:302-317`); delete `lifecycle.go` +
  `lifecycle_test.go` outright (the file is wholly the embedded stack-quit
  `quitController` — window close-cancel lives in `bridge_service.go:315-327`
  and `main.go:190-204`, both kept). Delete `embedded_test.go`,
  `embedded_path_test.go`, the cross-process podman sibling-resolution test,
  and the embedded arms of table tests.
- **Interfaces:** consumes `runClient(cfg appconfig.Config, stateDir string)
  (*bridgeService, error)` (`embedded.go:127-147`, kept verbatim). Produces a
  `launch` (or a direct `runClient` call — with one arm the dispatch may
  inline) whose only production bridge target is
  `bridge.NewTLSTarget(cfg.ServerURL, caPEM)`.
- **Test cycle:** `direnv exec . go build ./... && go vet ./...` (untagged) +
  `-tags 'unix gtk3'` build; client-mode unit tests (bridge_service_connect,
  window_set, close-cancel) green; grep-gate over **non-`_test.go`** files:
  zero references to `compass-stack`, `ModeEmbedded`, `NewUnixTarget`, or
  `internal/preflight` under `go/cmd/compass-app/` and zero module-wide
  importers of `internal/preflight` (the two kept app tests
  `bridge_service_test.go:86` / `multiwindow_e2e_test.go:100` retain
  `NewUnixTarget` as their sanctioned h2c-UDS stub — excluded from the gate).
  `moon run compass-go:ci` green.

### T-2 — Strip appconfig to client-only

- **Do:** implement A3 in `go/internal/appconfig`: absent/`"client"` mode →
  client; `mode = "embedded"` → the legible retirement error; delete
  `ModeEmbedded`, `modeStrEmbedded`, the embedded parse arm
  (`appconfig.go:93-95`), `applyOverride` + the `--mode`/`$COMPASS_APP_MODE`
  plumbing (`main.go` modeFlag, `resolveMode`). Decide in-code whether `Mode`
  survives as a type (recommend: delete it; `Config{ServerURL, CACert}`).
  Absent app.toml → the A3 first-run error (or the OQ-2 connect-config screen
  if Matt rules that way — build the error now, the screen is additive later).
- **Interfaces:** consumes today's `Parse`/`Load` (`appconfig.go:79-103,
  :145-181`). Produces `appconfig.Load(configHome, home string) (Config,
  error)` with the override parameter dropped; `Config` without `Mode`.
- **Test cycle:** appconfig unit tests rewritten: absent-file error,
  `mode="embedded"` rejection copy, `mode="client"` + valid/invalid
  server_url, unknown-key rejection unchanged. `moon run compass-go:ci` green.

### T-3 — Retire the embedded contract residue

- **Do:** demote `bridge.NewUnixTarget`'s doc (`pump.go:77-81`) from
  "embedded/Dogfood mode" to test-harness status (the pump test suite keeps
  it as its hermetic h2c-UDS stub, `pump_test.go:26`); drop the embedded arm
  from `shellStartupJS` (`main.go:279-284`) so `window.__COMPASS_MODE__` is
  always `"client"` — or delete the global outright if the UI's only read of
  it is the mode fork (verify in `apps/ui` at execution; coordinate with the
  UI lane). Remove `AccountID`'s embedded-population path note
  (`bridge_service.go:81-83` already documents the client-empty behavior).
- **Interfaces:** consumes T-1 (no production `NewUnixTarget` caller left).
  Produces the final app-side surface: one target constructor in production,
  one startup-JS shape.
- **Test cycle:** pump tests green unchanged; UI typecheck + the T5.7 e2e gate
  script green against the client-only startup JS.

### T-4 — Retarget the smoke gates

- **Do:** `dogfood-e2e` (`ci.yml:459-523`) keeps running as the HEADLESS-stack
  gate — verify it has no `compass-app` dependency (it drives
  `compass-stack`/`compass-postgres`/podman directly) and rename/re-comment
  its framing from "embedded" to "headless stack" where the YAML prose says
  otherwise. The multi-window gtk3 e2e (`ci.yml:328-334`) stays as the shell
  gate. Rewrite `app-bundle/SMOKE.md` as the client-bundle smoke: unpack →
  launch → connect (bearer via connect screen) to a live headless stack →
  board renders → one agent session reaches a running container ON THE STACK
  MACHINE → quit/relaunch reconnects via keychain.
- **Interfaces:** consumes the existing CI jobs + T5.7's e2e gate script.
  Produces the retargeted smoke definitions the client-only bundle is judged
  by.
- **Test cycle:** CI green on the PR; the SMOKE.md procedure executed once
  end-to-end on the dev box against a locally-run `compass-stack up`.

### T-5 — Re-scope `app-bundle/` to the thin client bundle

- **Do:** cut `app-bundle/build.sh` staging to: gtk3 `compass-app` (cgo,
  nix cc-wrapper, store-rpathed — the DL-214 mechanics keep applying to the
  one binary), `bin/dist/` (UI build), `compass.desktop`, LICENSE. Drop the
  four sidecar builds, the postgres store symlinks (DL-217's staging), and
  the sanity assertions for them; keep the version stamp (T6.1's
  `version.go` stays — one binary still wants `--version`) and the
  `bin/dist/index.html` assertion. Trim `bundle-env.nix`'s postgresql delta.
  moon `inputs` narrow accordingly (drop the pure-Go breadth only if the
  shell's own inputs allow — the shell still imports `go/internal/bridge`
  etc., so `/go/**` likely stays).
- **Interfaces:** consumes T-1 (no sidecar spawn left to satisfy),
  `compass-ui:build` dist, `gtk-e2e-env.nix` outputs. Produces
  `compass-app-<v>-linux-amd64.tar.gz` (client-only), the artifact RIG-2477's
  re-scope attaches to releases.
- **Test cycle:** `moon run compass-app-bundle:build` green from a clean
  checkout; tarball passes the T-4 SMOKE.md; `moon query projects` still
  lists the project.

### T-6 — Docs, ledger, tracker follow-through

- **Do:** per the freeze rule (add, never rewrite frozen prose): append a
  supersession banner to `compass-native-app/design.md` (DL-106 → superseded),
  `compass-native-packaging/design.md` (embedded-bundle halves), and
  `compass-stack-cross-process-teardown/design.md` (app-invocation half;
  mechanism lives on under `compass-stack`). Land the §Ledger-impact rows in
  `DECISIONS.md`. Re-scope RIG-2477 per A5; file the OQ-3 headless-stack
  distribution follow-up. (The OQ-1 elastic-runtime reconciliation is not a
  T-6 item — it ships in this PR as
  `compass-elastic-session-runtime/microvm-kvm-only-amendment.md`.)
- **Interfaces:** consumes this record (frozen). Produces the ledger delta +
  tracker state.
- **Test cycle:** ledger row IDs verified free at landing; banner links
  resolve; driver review.

## Tasks

- [ ] **T-1** Sever the shell from the supervisor: `launch` collapses to the
  client arm; `embedded.go`/`preflight_adapters.go` + embedded tests deleted;
  `--socket`/`--image`/`--compass-stack` plumbing removed; `compass-go:ci`
  green.
- [ ] **T-2** appconfig client-only: embedded arm deleted, `mode="embedded"`
  rejection copy, override plumbing removed; unit tests rewritten.
- [ ] **T-3** Residue: `NewUnixTarget` demoted to test-harness doc;
  `shellStartupJS` client-only; UI typecheck + T5.7 gate green.
- [ ] **T-4** Smoke gates retargeted: `dogfood-e2e` framed as the
  headless-stack gate; `SMOKE.md` rewritten as the client-bundle smoke; run
  once end-to-end.
- [ ] **T-5** `app-bundle/` re-scoped to the thin client bundle (no sidecars,
  no postgres symlinks); clean-checkout build + smoke green.
- [ ] **T-6** Supersession banners, ledger rows, RIG-2477 re-scope, OQ-3
  follow-up filed. (The OQ-1 elastic-runtime reconciliation ships in THIS PR as
  `microvm-kvm-only-amendment.md`, not a T-6 follow-up.)

## Open Questions

All designed-against-assumption per the batched-clarifications rule; each
carries a recommendation for Matt.

### OQ-1 [resolved, cross-lane] — elastic-runtime record reconciled in this PR

The frozen RIG-1717 record read the OPPOSITE of this record's driving
constraint: on a KVM-absent box it "degrades to the container runtime"
(`compass-elastic-session-runtime/design.md:466-467`, restated at `:600-601`
and Task I1 `:828`). **Resolved (Matt, 2026-08-23): the runtime is KVM-only —
it does not degrade to the container runtime; the older elastic-runtime text is
stale and the newer microVM Runner record (RIG-2394) already reflects
KVM-only.** Matt authorized reconciling it here rather than deferring to the
runner/infra lane. This PR adds
`compass-elastic-session-runtime/microvm-kvm-only-amendment.md` — an
authoritative amendment (the `virtualfs-descope-amendment.md` precedent in that
directory) superseding the parent's three degrade-to-container clauses with the
`VerifyMicroVMSupport` KVM-absent hard-fail (`microvm-runner.md:211-228`,
D2 `:63-65`). The transitional-container timeline (OQ-5, `design.md:892-894`)
is unchanged. No cross-lane dependency remains open; the client-only premise is
now consistent with the frozen runtime corpus.

### OQ-2 [load-bearing] — first-run UX with no app.toml

With the zero-config embedded default gone, an absent app.toml can no longer
"just work". Options: (a) a legible startup error naming the app.toml shape
and how to point at a stack (cheapest; ships with T-2); (b) an in-app first-run
screen collecting `server_url` (+ optional CA) and writing app.toml.
Option (b)'s cost is likely SMALL, not a new surface: the window already opens
pre-probe (`main.go:266-269`) and the connect screen already exists for the
bearer, so (b) is plausibly a `server_url` field on an existing screen, not a
from-scratch connect-config flow. **Recommendation:** (a) now — T-2 builds it —
with (b) filed as an additive UI follow-up; a client-only app whose only
out-of-box state is an error string is a rough first-run, so (b) should follow
soon, but it does not block the cutover.

### OQ-3 [load-bearing] — headless-stack distribution

Retiring the embedded bundle orphans the QUESTION of how a self-hoster gets
`compass-stack`+server+runner+postgres onto the dedicated machine. RIG-2477's
body records that the RIG-1746 releases lane "attaches per-arch binaries for
`compass` (CLI), `compass-server`, `compass-runner` only" and "deliberately
EXCLUDES the desktop lane (`compass-app`, `compass-stack`)" — so
`compass-stack`/`compass-postgres` have no release-artifact home today, and
the postgres-tooling question DL-217 answered for the bundle re-opens for the
headless machine. **Recommendation:** file a follow-up in the releases/infra
lane ("headless single-user stack distribution: add compass-stack +
compass-postgres to the release matrix; postgres tooling = host prerequisite
on a dedicated machine, superseding DL-217's bundle answer"); explicitly OUT
of this record's plan beyond filing. Not a hidden blocker: the embedded bundle
never shipped as a release artifact either (RIG-2477 was itself a DEFER), so
the self-hoster's position is unchanged by this record.

### OQ-4 [non-load-bearing] — delete vs keep the `Mode` type

With one mode, `appconfig.Mode` may vanish (`Config{ServerURL, CACert}`) or
stay as a one-value enum for future modes. **Recommendation:** delete it —
clean cutover; a future mode re-adds an enum when it has two values again.
T-2 proceeds on deletion.

### OQ-5 [non-load-bearing] — `window.__COMPASS_MODE__` fate

The injected global is NOT deletable: `apps/ui/src/index.tsx:61-64` forks the
boot connection on it — `shellMode() === "client" ? bootNativeClient(root) :
bootConnection(root, envConnectionProvider)` — where the else-arm is the
**browser-dev** env provider (`shell-globals.ts` returns undefined off-shell),
not the embedded arm. It distinguishes shell-from-browser, so deleting it
would pass a naive single-consumer grep and then boot the packaged app into
the browser provider — a broken launch. **Recommendation:** keep the global,
pin the shell-injected value to `"client"` (drop the embedded literal from
`shellStartupJS`), and narrow the TS union to `"client"` as UI follow-through.
Coordinate the narrowing with the UI lane.

### OQ-6 [resolved] — one-box localhost-TLS is a sanctioned client-only config

Retiring embedded mode does not force a second machine: a developer can run
`compass-stack up` locally and point the client at
`server_url = https://localhost:<port>` with the stack's private CA — the
client dials the same TLS door, just on loopback. **Resolved (Matt,
2026-08-23): yes, one-box is supported.** So the retirement reads as "you lose
zero-config *supervision*," not "you now need two machines" — the §Problem
statement stands, and OQ-2(a)'s first-run error copy names both paths (a remote
`server_url`, or `compass-stack up` locally + `https://localhost`). This is a
documentation/positioning point, not an architectural one; nothing in the
cutover changes.

## Ledger-impact (proposed rows — land with the design PR, not before)

Next free ID verified against `DECISIONS.md` at submit (highest existing:
DL-234 — the RIG-2483 command-palette rows; `grep` for `DL-23[5-9]` returned
no matches). Allocated DL-235..238.

New rows:

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-235 | The Compass native app is CLIENT-ONLY: `compass-app` retires embedded mode entirely (supervisor invocation, host preflight, UDS bridge target, embedded config arm) and connects exclusively over the authenticated TLS door to a headless Compass stack — normally on a dedicated KVM-capable machine, or the same box via `compass-stack up` + `https://localhost` (OQ-6); driven by the microVM Runner's KVM floor retiring local agent execution. Supersedes DL-106 (dual-mode charter) | Active (Matt, 2026-08-23) | this record §A1 |
| DL-236 | `compass-stack` survives the app's retirement of embedded mode as the standalone headless single-user bring-up CLI (up/down/status, DL-183 teardown intact); the app never spawns, supervises, or tears down a stack. Refines DL-108 (which stays Active — the supervisor design lives; only the app's shell-spawn invocation retires) | Active (Matt, 2026-08-23) | this record §A1/§A2 |
| DL-237 | app.toml is client-only: absent or `mode="client"` → client (server_url required); `mode="embedded"` is a legible retirement error, never a compatibility arm; the `--mode`/`$COMPASS_APP_MODE` override retires with the second mode. Supersedes the embedded-default mode-selection half of DL-109 (whose keychain-first bearer clause stays Active) | Active (Matt, 2026-08-23) | this record §A3 |
| DL-238 | The app bundle is the THIN CLIENT: `compass-app` + dist + desktop file + LICENSE, no sidecar binaries, no postgres tooling; headless-stack distribution is a releases-lane follow-up (OQ-3), and RIG-2477 re-scopes to client-app release artifacts with macOS as the first new target | Active (Matt, 2026-08-23) | this record §A4/§A5 |

Status flips on existing rows (the driver edits `DECISIONS.md` in the same PR).
The ledger's status enum is exactly `Active` or `Superseded by DL-<n>` (no
"Amended" status; §Conventions), so a PARTIAL supersession follows the DL-213 /
DL-183 pattern — the old row **stays Active** and the NEW row carries the
supersede-by-citation in its own Decision cell:

- **DL-106** (one binary, two modes) → `Superseded by DL-235` — the dual-mode
  charter is fully dead.
- **DL-108** (embedded lifecycle: supervisor spawned by the shell) →
  **stays Active** — the supervisor design (private postgres, cert rotation,
  lockfiled attach, DL-183 teardown) survives whole as the headless CLI; only
  the app's shell-spawn *invocation* retires. DL-236 carries the refinement by
  citation (exactly as DL-183 already "Refines DL-108 (which stays Active)"); a
  Superseded flip would both misread the live supervisor design as dead and
  break DL-183's existing citation.
- **DL-109** (mode selection app.toml, absent→embedded default; keychain
  bearer) → **stays Active** — the keychain-first bearer clause is live; DL-237
  supersedes the embedded-default mode-selection half by citation.
- **DL-214** (tarball-of-nix-closure bundle format) → stays Active for the
  thin client's Linux gtk3 build (one store-rpathed binary instead of five).
- **DL-215** (sidecar bin/ layout + PATH threading) → `Superseded by DL-238`
  (the `dist`-beside-executable half survives inside DL-238's thin layout;
  the sidecar/PATH-threading half retires).
- **DL-216** (moon-registered affected-gated bundle CI) → stays Active
  unchanged (the lane persists, its content shrinks).
- **DL-217** (postgres tooling in the bundle) → `Superseded by DL-238`
  (no postgres in a client bundle; the headless machine's answer is OQ-3's
  follow-up).
- **DL-107** (frame contract + both-modes bridge) → stays Active; its
  embedded-pumps-to-UDS clause becomes vacuous (the contract half is what the
  row protects).
- **DL-110** (Wails v3) → stays Active, but its row text ("importing
  `go/internal/stack` … directly") is already inaccurate (the app execs the
  `compass-stack` binary, it never imported the package — `embedded.go:12-16`)
  and doubly so post-T-1; T-6's banner pass annotates the stale clause rather
  than leaving a wrong Active row.
- **DL-111** (WhoAmI), **DL-112** (GHCR agent image — now consumed only by the
  headless stack's `compass-stack`) → stay Active.
