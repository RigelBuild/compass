# Compass native app — embedded vs remote architecture

Status: Draft
Supersedes: DL-044 (compass-tauri-shell)
Linear: RIG-1662

> **Superseded in part by the client-only pivot
> ([`../compass-native-client-only/design.md`](../compass-native-client-only/design.md),
> DL-235, Matt 2026-08-23).** The Compass native app retired embedded mode and
> became a native **client** that connects over the authenticated TLS door to a
> headless stack — the dual-mode charter this record froze (DL-106, "one binary,
> two modes") is `Superseded by DL-235`. What that pivot changed here, by
> disposition (the frozen prose below is left intact as the record of what was
> ratified):
>
> - **DL-106** (one binary, two modes) — fully dead; the app is client-only.
> - **DL-107** (frame contract + both-modes bridge) — stays Active; the
>   embedded-pumps-to-UDS clause is now vacuous, the frame-contract half it
>   protects is untouched.
> - **DL-108 / §A3** (embedded lifecycle: Go stack supervisor) — stays Active as
>   the standalone `compass-stack` CLI; only the app's *shell-spawn invocation*
>   of it retired (DL-236 refines by citation).
> - **DL-109 / §A4** (mode selection: absent → embedded default; keychain bearer)
>   — the keychain-first bearer clause stays Active; the embedded-default
>   mode-selection half is superseded by DL-237 (app.toml is client-only).
> - **DL-110 / §OQ1** (Wails v3 shell) — stays Active, but its ledger-row clause
>   "importing `go/internal/stack` … directly" was never accurate (the app execs
>   the `compass-stack` binary; it never imported the package — verifiable in
>   HEAD: `grep internal/stack go/cmd/compass-app/` returns nothing, and the
>   pre-retirement `embedded.go` header said the app supervises "through the
>   `compass-stack` binary … not by importing `go/internal/stack`") and is doubly
>   stale post-pivot; read it as "Wails v3, `go/cmd/compass-app`" only.
> - **DL-111** (WhoAmI), **DL-112** (GHCR agent image) — stay Active; the agent
>   image is now `podman pull`ed by the headless `compass-stack`, not the app.

## Problem / Intent

Compass today is a headless Go stack (`compass-server` + `compass-runner`) plus a
browser-dev-only SolidJS UI; there is no installable native app. RIG-1662 requires
ONE native binary with TWO modes: **embedded** (the app launches and supervises the
whole single-user stack locally — server, runner, and their prerequisites) and
**native-client** (the same app connects to an already-established remote server
over its authenticated TLS network door). This record settles the
embedded-vs-remote architecture — the transport seam, mode selection, embedded
lifecycle, and auth — before any shell implementation. It supersedes DL-044,
whose Rust-`compassd`/Tauri-crate context no longer exists after the Go pivot.

## Approach

The shell framework is **Wails v3 (Go)** — Matt's ruling on the one load-bearing
framework fork (DL-110; the options and reasoning trail are kept in
§Decisions/OQ1). The design was held framework-agnostic wherever it could be, so
the framework-dependent residue is confined to a single UI-side IPC shim (A2) and
the shell scaffold (T3); everything else holds independent of that choice.

### A0 — The current stack, as-built (what the two modes must bridge)

Source facts the design composes with (`RigelBuild/compass` @ main 2624bcb5):

- **The server serves three doors.** Default is a Unix domain socket
  (`go/cmd/compass-server/main.go:42-45`: "Unix socket to serve compass.v1 on.
  Defaults to $XDG_RUNTIME_DIR/compass/server.sock, falling back to
  $HOME/.compass/server.sock"); an opt-in loopback dev door (`main.go:46-49`,
  "Off by default; the shipped path is socket-only"); and the authenticated TLS
  network door (`main.go:50-54`: "Serve the authenticated gRPC network door on
  this TCP address (e.g. 0.0.0.0:8443) … Requires --tls-cert and --tls-key
  together — a bearer token over cleartext is credential disclosure"). The
  network door is ALPN-h2 with gRPC-Web negotiated on the same port
  (`go/server/network_door.go:122-126`) and a single-origin CORS policy that
  allows the `Authorization` request header and exposes the gRPC-Web status
  trailers (`network_door.go:134-149`).
- **The runner is a second process that dials OUT.** `go/cmd/compass-runner/main.go:3-5`:
  "It dials OUT to the Server over gRPC with its per-Runner token, enrolls, and
  hosts per-agent containers — the Server never runs container-engine code." Its
  token is env-only (`main.go:105-107`: "The per-Runner token is a bearer secret:
  env only, never a flag (a flag leaks into the process table)").
- **`RunnerService` is mounted ONLY on the network door.** `go/server/serve.go:216-219`:
  "the RunnerService door a Runner enrolls over is mounted only on the network
  door (buildNetworkServer) — Runners are remote, so they dial the authenticated
  TLS door, never the loopback socket." So embedded mode cannot be socket-only:
  the local runner still needs a loopback TLS door to enroll over.
- **The server requires Postgres.** `go/cmd/compass-server/main.go:108-109`:
  `return errors.New("a Postgres DSN is required: pass --database or set
  $COMPASS_DATABASE_DSN")`; the store is pgx-only with embedded migrations
  (`go/internal/store/store.go:60-61`: `func Open(ctx context.Context, dsn
  string) (*Store, error)` over `pgxpool.New`). Embedded mode therefore needs a
  database story, resolved as an app-state-dir private Postgres (DL-108).
- **The runner hard-requires uid 1000.** `go/cmd/compass-runner/main.go:178-188`
  (`verifyRunnerUID`): "the runner must run as uid %d, but it is running as uid
  %d: the agent image bakes the agent user, /nix and $HOME as uid %d, and
  podman's --userns=keep-id maps the host uid into the container unchanged".
  An embedded app on any other uid cannot start the runner today; embedded mode
  preflights-and-refuses with legible copy, and arbitrary-uid support is a named
  GA follow-up (§Decisions/OQ5).
- **The runner also hard-requires an agent container image.**
  `go/cmd/compass-runner/main.go:111-114`: `return errors.New("an agent image is
  required: pass --image or set $COMPASS_AGENT_IMAGE")`. The devenv chain
  deliberately EXCLUDES the image build from `up` (`devenv.nix:145-146`: the
  agent-image build is "a heavy closure — kept off the hot up path"), and an
  installed app has no nix build — so distributing the image is its own
  problem, resolved as a GHCR pull at first run (DL-112).
- **The dogfood devenv loop already IS the embedded stack, hand-rolled.**
  `devenv.nix:122-143` orders exactly the chain embedded mode must productize:
  postgres → `dogfood:gen-cert` (self-signed TLS anchor, skip-if-present) →
  `compass-server` (`--socket` + `--dev-http` + `--listen 127.0.0.1:<port>` +
  cert/key, `devenv.nix:199-205`) → `dogfood:mint-runner-token`
  (`cmd/compass-mint-runner-token`, writes the enrollment token 0600) →
  `compass-runner` (dials `https://127.0.0.1:<port>` with `--ca` = the generated
  cert, `devenv.nix:292-297`).
- **The UI resolves its connection once at boot, at one seam.**
  `apps/ui/src/live/connection.ts:58-79` (`resolveConnection`) requires
  `VITE_COMPASS_BASE_URL` + `VITE_COMPASS_CALLER_ID` (+ optional token);
  `apps/ui/src/live/client.ts:30-35` builds both clients from that one
  `Connection` (`createCommsWebClient(conn.baseUrl, conn.token)`). connection.ts
  states the invariant this record keeps (`connection.ts:9-12`): "this module is
  the one place the baseUrl+token+caller are resolved, so adding a mode never
  leaks a local-assumption above the transport boundary."
- **The shell-IPC transport shape already exists in the UI.**
  `apps/ui/src/daemon-transport.ts:1-10`: a custom `fetch` that "sends the
  request bytes over `invoke`, receives the response as ordered frames on a
  Tauri `Channel`, and reassembles them into a `Response` whose body is a
  `ReadableStream` — so `createGrpcWebTransport({ fetch })` streams
  `SubscribeEvents` incrementally." Its frame contract
  (`daemon-transport.ts:19-23`) is `head | body | end | error`. The Tauri import
  is one line; the contract itself is framework-agnostic.

### A1 — One binary, two modes; mode = which Connection the resolver produces

The mode difference is confined to exactly two things: **which `Connection` the
UI's boot resolver produces**, and **whether the embedded stack supervisor runs**.
Nothing above the transport boundary knows the difference — the DL-044 invariant
carried forward verbatim: *the shell and UI must never assume "local" beyond the
transport boundary; no socket path or `localhost` leaks above the
`fetch`/command seam* (`compass-tauri-shell.md:121-123`).

Concretely, `resolveConnection` (`connection.ts:58`) is generalized from
Vite-env-only to a **connection provider** seam: the browser dev path keeps the
env provider unchanged; the native app supplies a shell-provided
`Connection`-plus-transport at boot (T1). `createLiveClients`
(`client.ts:30-35`) and everything above it are untouched.

### A2 — The transport seam: one frame contract, two bridges

Both modes reuse `createGrpcWebTransport({ fetch })` with the existing custom-
`fetch` shape. The shell exposes ONE IPC contract, lifted verbatim from
`daemon-transport.ts` and made framework-neutral:

- Request: `compass_rpc({ requestId: string, path: string, headers:
  {name,value}[], body: number[] })` plus `compass_rpc_cancel({ requestId })`.
- Response: an ordered frame stream `{kind:"head", status, headers} |
  {kind:"body", chunk/*base64*/} | {kind:"end"} | {kind:"error", message}` —
  the exact `ResponseFrame` union at `daemon-transport.ts:19-23`.

Wails v3 (DL-110) implements the stream as runtime events keyed by `requestId`.
The UI-side adapter (`daemon-transport.ts`) keeps its logic and swaps only the
two framework calls (`invoke`, `Channel`, today Tauri-shaped in the UI) for the
Wails runtime behind a thin `ShellIpc` shim, so the shell binding touches only
this file.

Behind the IPC command, the shell routes by mode:

- **Embedded:** pump to the server's UDS (resolved by the same rules as
  `server.DefaultSocketPath`, `go/server/socket.go:29-32`) over native h2c gRPC.
  Zero TCP for the UI path; the socket's 0600 filesystem ACL stays the local
  credential.
- **Native-client:** pump to the remote TLS network door with the stored bearer
  attached. When the door presents a publicly-trusted certificate the webview's
  OWN `fetch` *could* dial it directly, but only after the operator configures
  the app's webview origin server-side: the door's gRPC-Web CORS is
  single-origin and default-closed (`CORSAllowedOrigin` is one operator-set
  origin, empty = no CORS on the network door, `serve.go:76-78`; exposed headers
  `network_door.go:134-149`), so a cross-origin fetch from the app origin is
  blocked until that origin is added. The shell bridge is routed through
  regardless, for a stronger reason: private-CA/self-signed trust (the dogfood
  cert shape) lives shell-side, mirroring the runner's `--ca` trust anchor
  (`compass-runner/main.go:58-63`), never in webview trust stores.
  Routing both modes through the same bridge keeps one code path and keeps the
  stored token out of webview-reachable env at rest: in bridge mode the UI's
  `Connection` carries no bearer (`client.ts:30-35` is handed an empty token) —
  the shell holds the token and injects the `Authorization` header on the remote
  pump. The token still enters once through the webview (the connect screen, T5),
  so the property is about storage and replay, not entry.

### A3 — Embedded lifecycle: a Go stack supervisor, spawned by the shell

Embedded mode productizes the devenv dogfood chain (A0). The orchestration —
database readiness, cert minting, server launch, runner-token minting, runner
launch, health probing, drain-on-stop — is **Go logic and lives in Go**, as a
new supervisor entry point `go/cmd/compass-stack` wrapping a library package
`go/internal/stack`. The Wails v3 shell (DL-110) spawns and monitors ONE
child — `compass-stack up` — instead of five processes. This does three things:

1. **Keeps the Wails shell thin.** The thin-shell invariant
   (README.md:36-37: "The shell holds no logic — it launches and supervises the
   server and points the webview at the contract") holds because orchestration
   is Go logic the shell imports, not shell code — the reason the supervisor is
   Go regardless of the framework ruling (DL-110).
2. **Gives the stack a CLI life of its own.** `compass-stack up` is also the
   headless single-user path (a server box without the app), and the seam CI
   smoke tests drive.
3. **Makes linger-on-quit one flag, not a design.** Whether the stack outlives
   the app is a single supervisor flag (`--linger`, default on per DL-108), not a
   per-process lifecycle.

The supervisor's sequence mirrors `devenv.nix:122-143`: ensure the app-state-dir private Postgres (DL-108) →
ensure the TLS anchor via `cmd/compass-gen-cert` — but **expiry-aware**, not the
devenv chain's plain skip-if-present, which is a documented time bomb: gen-cert
is "skip-if-present forever against a finite `--validity`, so once the cert
expires the loop fails with an opaque TLS error" (`devenv.nix:152-155`). An
installed app cannot ship that cliff, so the supervisor checks the anchor's
`NotAfter` and rotates (regenerate + restart both children, since the runner's
`--ca` IS the server cert, `compass-runner/main.go:58-63`) when near expiry →
start `compass-server` on a **configured** loopback port (`--socket
<state>/server.sock --listen 127.0.0.1:<port> --tls-cert … --tls-key …`); the
port is a `Config` field with a bind-failure → legible-error story, NOT an
ephemeral `:0`, because the server exposes no bound-address API to discover a
`:0` port from (`bindListeners` keeps the listener internal,
`network_door.go:67-93`) → readiness = `GetServerInfo` answering (the devenv
probe rationale, `devenv.nix:229-235`: the socket binds before migrations, so
file-existence is a false ready) → ensure runner token
(`cmd/compass-mint-runner-token`, idempotent, 0600) → start `compass-runner
--server https://127.0.0.1:<port> --ca <anchor> --image <agent-image from GHCR, DL-112>
--runtime-dir $XDG_RUNTIME_DIR/compass-runner` with `COMPASS_RUNNER_TOKEN` via
env only. The runner spawn MUST carry `--image` (it refuses to boot without one,
A0; pulled from GHCR per DL-112) and `--runtime-dir` under `$XDG_RUNTIME_DIR` — the `/run/compass`
default is root-owned and a deep state-dir path overflows the 107-byte AF_UNIX
`sun_path` cap the runner's per-container sockets live under
(`devenv.nix:270-274`). Spawn-if-absent + attach: a live `GetServerInfo` on the
socket means attach, never double-spawn; an O_EXCL state-dir lockfile guards the
probe→spawn window so two concurrent `up`s (app launch racing a manual CLI `up`)
cannot both spawn.

On app quit, per DL-108: the stack lingers by default;
quit closes the window, an explicit "Quit and stop stack" action runs
`compass-stack down`, which SIGTERMs the tree and waits out the server's drain.

### A4 — Mode selection and native-client configuration

Mode selection is **settled here** (not an open question): one config file owns
the choice — `$XDG_CONFIG_HOME/compass/app.toml` (fallback
`~/.config/compass/app.toml`), read by the shell at launch:

- absent file or `mode = "embedded"` → embedded (the zero-config default: first
  launch of the installed app just works — the single-user charter forbids a
  question screen on first run);
- `mode = "client"` + `server_url = "https://host:8443"` (+ optional `ca_cert`
  path for a private anchor) → native-client. `--mode`/`COMPASS_APP_MODE`
  override for dev.

A first-run wizard or a settings-screen mode-switch is a UI nicety layered on
this same config later, not an alternative mechanism; switching modes is an
edit-and-relaunch operation (one live `Connection` per app run, resolved once
before render), never a runtime toggle.

Native-client credentials map onto the existing seam
(`VITE_COMPASS_BASE_URL`/`VITE_COMPASS_CALLER_ID`/token,
`connection.ts:42-46`): the base URL comes from config; the bearer token is
entered once in a connect screen and stored in the OS keychain (DL-109), never in
the config file. **Caller identity is resolved by a new `WhoAmI` RPC.** Account
ids are 128 random bits minted per database, so the id cannot be guessed or
defaulted (`apps/ui/.env.development:32-35`), and today there is no way to learn
one (`connection.ts:28-35` documents the parked gap). This record adds a
`WhoAmI` RPC on `compass.v1` (DL-111, a cross-lane compass-server change):
each mode calls it right after the transport is up and reads back the caller
account id from its own credential — embedded over the socket (ambient-admin
identity), native-client over the authenticated door (the bearer's subject).
This fixes BOTH modes with one mechanism and removes the caller-id field from
the native-client connect screen entirely; no operator ever pastes an account
id.

### A5 — Scope boundary

In scope: the two modes, the transport bridge, the stack supervisor, mode
selection/config, token entry + storage, and the e2e smoke for each mode.
Deferred to follow-up issues: tray/notifications/deep links, signed installers
with auto-update, non-Linux runner support (the runner loop is Linux/podman-only,
`devenv.nix:157-158`), and any native-path rendering endgame.

**External / cross-lane dependencies.** T4 (Dogfood) is hard-blocked on two
deliverables this lane does not own, both of which must land before the T4 gate
can pass: (1) the `compass.v1` `WhoAmI` RPC on **compass-server** (DL-111 —
T4 builds the UI `Connection` by calling it), and (2) the `compass-agent` GHCR
publish lane on **compass-runner + CI** (DL-112 — T4's "one agent session
reaches a running container" gate needs the image pullable). Both are tracked as
cross-lane seams here and filed as owned issues at decomposition (this lane files
and sequences them post-freeze, per the epic's decompose-after-freeze order); T4
stays blocked until both merge.

## Alternatives considered

### Two binaries (a client app + a separate "server pack")

Ship the native client alone and leave the embedded stack to a separate
installer/systemd unit. Rejected: it forfeits the RIG-1662 charter ("a single
user runs the entire Compass stack as one native application with nothing else
to stand up") — the single-user path would still require standing up a second
artifact, and the two artifacts' version skew becomes a support surface. The
cost of one-binary is small because the supervisor is already its own Go
command (A3): the app embeds an orchestration client, not the orchestration.

### Token-guarded loopback TCP proxy instead of the IPC bridge (embedded)

Bind `127.0.0.1:<ephemeral>` in the shell and point the webview's normal fetch
at it. Rejected for the same reasons DL-044 rejected it
(`compass-tauri-shell.md:61-69`): a loopback TCP port is network-reachable and
has no filesystem ACL, reintroducing the surface the owner-only socket exists
to remove; a per-launch token only approximates owner-only. Retained as the
documented fallback if a framework's IPC streaming proves unworkable.

### Webview dials the network door directly in native-client mode (no bridge)

The door serves gRPC-Web, but its CORS is single-origin and default-closed
(`CORSAllowedOrigin`, `serve.go:76-78`; exposed headers `network_door.go:134-149`),
so the webview's own `fetch` works only against a publicly-trusted cert AND after
the operator adds the app's webview origin server-side — not the "zero shell
code" it first appears. Rejected as the *only* path: it cannot carry a private-CA trust
anchor (webviews use the OS trust store), which is exactly the self-signed
dogfood/self-hosted shape (`devenv.nix:128-131`), and it forks the transport
into two code paths. It survives as an internal optimization the bridge MAY
apply when the cert chain is publicly trusted; the seam contract stays the
bridge either way.

### Shell-side orchestration of the five-step stack (no `compass-stack`)

Have the shell itself implement postgres-readiness, cert minting, token
minting, and two-process supervision. Rejected: it violates the thin-shell
invariant (README.md:36-37) and would re-implement orchestration that is already
Go logic the Wails shell imports (drift against `devenv.nix`'s evolving chain,
and — under the rejected Rust option — a second language for that logic). The supervisor is Go logic and lives in
`go/` where its pieces (`server.DefaultSocketPath`, the mint/gen-cert commands,
the readiness rationale) already are.

### A local RunnerService door instead of the loopback TLS door (embedded)

Have the server mount `RunnerService` on a second LOCAL door — a UDS or loopback
listener carrying the runner bearer, no TLS — so embedded mode's runner enrolls
without a cert at all. This would delete gen-cert, TLS, and the whole
cert-rotation problem (A3) from embedded mode. It is NOT rejected on merit — it
is a real simplification — but it is a **compass-server change** (the door
mounting is `serve.go:216-219`, currently network-door-only) with its own
security review (a local bearer door is a new local attack surface), so it is
out of this record's shell-scope. Recorded as the structural alternative to A3's
"embedded needs a loopback TLS door" MUST; if compass-server takes it, A3's cert
lifecycle simplifies to nothing. Raised with the compass-server owner as a seam.

### How the framework choice was isolated (now decided: Wails v3)

RIG-1662 reserved the framework fork to Matt, so the record was held
framework-agnostic — the choice lands in just two files (the UI-side IPC shim,
the shell project scaffold). Matt ruled it **Wails v3** (DL-110; the Tauri-vs-Wails
tension and reasoning trail are in §Decisions/OQ1). The isolation kept that
ruling cheap to apply and keeps a later reversal cheap.

## Global Constraints

> **Amendment (RIG-1770, GTK4 migration — DL-282, 2026-08-28).** The "Linux
> system libs" constraint below names `webkit2gtk-4.1`; that is the
> pre-migration stack. The shell now builds Wails' default GTK4 +
> webkitgtk-6.0 (repo tag `gtk4`; closure `gtk4`/`webkitgtk_6_0` in
> `tools/toolchain/gtk-closure.nix`), so read the shell's system prerequisite
> as the gtk4 + webkitgtk-6.0 dev packages. The frozen prose is retained per
> the banner-amendment convention.

- **Go 1.25 floor** for anything under `go/` (`go/go.mod:15`: `go 1.25.0`); one
  Go module for the whole backend (`go/go.mod:1-3`), so `compass-stack` joins
  the existing module, never a second one.
- **Contract access only through generated clients.** Every UI→server call goes
  through `@compass/client` factories over `compass.v1` (README.md:31-35);
  the shell bridge moves bytes and holds no compass.v1 command logic.
- **Thin-shell invariant** (README.md:36-37): window + spawn/supervise + bridge
  only. Any RPC-handling logic in the shell is a bug.
- **No local-assumption above the transport boundary** (carried from DL-044,
  `compass-tauri-shell.md:121-123`): no socket path, `localhost`, or mode
  conditional above the `fetch`/IPC seam; `connection.ts` stays the single
  resolution point (`connection.ts:9-12`).
- **Bearer secrets are env/keychain/0600-file only, never argv** — the pattern
  the runner sets (`compass-runner/main.go:105-107`) and the server's admin
  token follows (`network_door.go:36-39`, 0600, never logged). The app's stored
  remote token follows the same rule (DL-109).
- **No cleartext network listener, ever**: the network door requires TLS
  (`compass-server/main.go:50-54`); embedded mode's local runner door binds
  loopback-only with the self-signed anchor, mirroring `devenv.nix:203-205`.
- **Runner constraints are inherited, not solved here**: uid-1000
  (`compass-runner/main.go:178-188`), rootless podman, Linux-only
  (`devenv.nix:157-158`). Embedded mode surfaces a clear preflight error on an
  unsupported host rather than a deep failure (T4).
- **License AGPL-3.0-only** for the shell project, matching `apps/ui`
  (`apps/ui/package.json:4`).
- **moon-registered CI lanes**: any new project (shell, `compass-stack` tests)
  registers in the moon workspace map so its `build`/`test`/`ci` tasks run in
  CI; nothing ships ungated.
- **Linux system libs gate the shell build (land-first, before T3)**: Wails v3
  on Linux links the system WebKitGTK, so `webkit2gtk-4.1`, `libsoup-3`,
  `pkg-config` (plus Wails' gtk build deps) must be present in **both**
  `devenv.nix`'s Linux-only `packages` block and `ci/ci-toolchain.nix`, or the
  shell's `moon` build fails to link in the private CI image. Carried from
  DL-044 (`compass-tauri-shell.md:128-132`), which paid for this on the Rust
  shell; the WebKitGTK dependency is identical for the chosen Wails v3 shell
  (DL-110).

## Plan

Dependency-ordered, sequenced toward Dogfood first (embedded mode is the
dogfood-adjacent path) then Beta/GA (native-client). T1 and T2 are
framework-independent and start immediately; T3 scaffolds the decided Wails v3
shell (DL-110).

### T1 — UI connection-provider seam (framework-agnostic; no shell required)

- **Do:** generalize the boot connection resolution from Vite-env-only to a
  provider seam. Add `apps/ui/src/live/provider.ts` exporting
  `interface ConnectionProvider { resolve(): Promise<ResolvedConnection> }`
  where `ResolvedConnection = Connection & { fetchImpl?: typeof fetch }`; the
  default provider wraps the existing `connectionFromEnv()`
  (`connection.ts:85-87`) with `fetchImpl` undefined (browser fetch). Thread
  `fetchImpl` into client construction: `createLiveClients(conn)`
  (`client.ts:30-35`) passes it to the `@compass/client` factories'
  transport-options parameter (extend `createCommsWebClient`/
  `createCompassWebClient` in `packages/compass-client` with an optional
  `{ fetch?: typeof fetch }` argument). Refactor `daemon-transport.ts` so its
  framework calls (`invoke`, `Channel` — `daemon-transport.ts:15`) sit behind a
  local `ShellIpc` interface (`rpc(args, onFrame): Promise<void>`,
  `cancel(requestId): void`), keeping the frame contract
  (`daemon-transport.ts:19-23`) and all stream/cancel/abort logic intact.
- **Interfaces:** consumes `apps/ui/src/live/connection.ts` (`resolveConnection`,
  `Connection`), `apps/ui/src/live/client.ts` (`createLiveClients`),
  `apps/ui/src/daemon-transport.ts`, `packages/compass-client` factory
  signatures. Produces `ConnectionProvider`, `ShellIpc`, and factories accepting
  an injected `fetch` — the one seam both modes plug into.
- **Gate:** unit tests — env provider parity with today's behavior (required
  vars still throw, `boot.ts` failure screen untouched); a fake `ShellIpc`
  drives `daemonFetch` through unary + multi-frame stream + mid-stream cancel;
  UI typechecks; browser dev path unchanged (smoke: `vite dev` against
  `--dev-http` still boots).

### T2 — `go/internal/stack` supervisor + `go/cmd/compass-stack` (embedded backbone)

- **Do:** a Go library `go/internal/stack` productizing the devenv dogfood chain
  (`devenv.nix:122-143`): `func Up(ctx context.Context, cfg Config) (*Stack, error)`
  with `type Config struct { StateDir string; SocketPath string; ListenAddr
  string; DatabaseDSN string; AgentImage string; RuntimeDir string }` and
  `func (s *Stack) Down(ctx context.Context) error`, `func (s *Stack)
  Health(ctx context.Context) (Status, error)`. Sequence: ensure the
  app-state-dir private Postgres is up and reachable (a `compass-stack`-supervised
  child, DL-108) → ensure the TLS anchor via `cmd/compass-gen-cert` logic,
  **expiry-aware** (check `NotAfter`,
  rotate + restart when near expiry — `devenv.nix:152-155`, not plain
  skip-if-present) → spawn `compass-server` with `--socket`, a **configured**
  `--listen 127.0.0.1:<ListenAddr>` loopback door (NOT `:0` — no bound-addr
  discovery API exists, `network_door.go:67-93`), `--tls-cert/--tls-key`,
  `--database` → poll readiness with `GetServerInfo` over the socket (NOT
  file-existence — the socket binds before migrations, `devenv.nix:229-235`) →
  ensure runner token via `cmd/compass-mint-runner-token` logic (idempotent,
  0600) → ensure the agent image present in the local podman store (`podman pull`
  the `AgentImage` GHCR ref, per DL-112; the runner refuses to boot without
  one, `compass-runner/main.go:111-114`) → spawn `compass-runner --server
  https://127.0.0.1:<port> --ca <anchor> --image <AgentImage> --runtime-dir
  <RuntimeDir>` with `COMPASS_RUNNER_TOKEN` in env only (`RuntimeDir` under
  `$XDG_RUNTIME_DIR`, validate its length against the 107-byte `sun_path` cap,
  `devenv.nix:270-274`).
  Attach-if-live under an O_EXCL state-dir lockfile: a `GetServerInfo` answering
  on the socket short-circuits to attach, and the lockfile closes the
  probe→spawn TOCTOU so two concurrent `up`s cannot both spawn; on attach,
  compare `GetServerInfo.version` against the bundled version and surface a
  restart-stack prompt on mismatch (an upgraded app must not silently drive a
  previous version's lingering stack). `cmd/compass-stack` wraps it as
  `compass-stack up|down|status` with `--state-dir`/`--linger`.
- **Interfaces:** consumes `server.DefaultSocketPath` (`go/server/socket.go:29`),
  the gen-cert and mint-runner-token command internals (refactor their cores
  into importable packages if still `main`-only), the `compass-server`/
  `compass-runner` CLIs (`compass-server/main.go:42-63`,
  `compass-runner/main.go:40-63`), `GetServerInfo` via the generated Go client.
  Produces the `stack.Up/Down/Health` API + the `compass-stack` binary.
- **Gate:** unit tests on sequencing/attach/failure surfaces with stubbed
  process execs, incl. the cert-expiry rotation branch and the sun_path length
  guard; a Linux integration test: `compass-stack up` on a temp state dir
  reaches Health=ready (server answering AND runner enrolled with an image),
  `down` drains cleanly, a second `up` attaches instead of double-spawning, and
  two concurrent `up`s produce exactly one stack. moon `ci` lane green.

### T3 — Shell scaffold + IPC bridge (Wails v3, DL-110)

- **Do:** scaffold the shell project in **Wails v3** (DL-110) — its Go module under
  `go/` (e.g. `go/cmd/compass-app`) so it imports `go/internal/stack` and the
  bridge pump directly; one window
  loading the built UI (`apps/ui` dist); implement the bridge: a `compass_rpc`
  command accepting `{requestId, path, headers, body}` and streaming the
  `head|body|end|error` frames back (Wails v3 runtime events keyed by
  `requestId`), plus `compass_rpc_cancel`. Backend of
  the bridge: an h2c gRPC-Web pump to a dial target set at window creation —
  `unix:<socket>` or `https://<host>:<port>` (+ optional CA anchor + bearer
  header injection). Implement the T1 `ShellIpc` against the framework's API.
- **Interfaces:** consumes T1's `ShellIpc` interface and frame contract, the
  server UDS (embedded target), the network door (remote target,
  `network_door.go` TLS/CORS/ALPN behavior). Produces a shell binary where the
  UI completes `GetServerInfo` and a live `SubscribeEvents`-class stream over
  the bridge, zero webview TCP in embedded mode.
- **Gate:** bridge tests — unary, multi-frame server stream, mid-stream cancel,
  abort propagation, gRPC trailer/error mapping — against a stub compass.v1
  server on a temp UDS; UI loads and typechecks in the shell; moon lane green.

### T4 — Embedded mode end-to-end (Dogfood milestone)

- **Do:** wire mode selection (A4): read `$XDG_CONFIG_HOME/compass/app.toml`
  (absent → embedded); in embedded mode run host preflight (Linux, rootless
  podman present, uid preflight-and-refuse per `verifyRunnerUID` —
  `compass-runner/main.go:178-188` (arbitrary-uid support is a GA follow-up,
  §Decisions/OQ5) — the app-state-dir private Postgres per DL-108 — agent image
  pullable from GHCR and present in the local store per DL-112) with actionable
  failure copy, then spawn
  `compass-stack up` as the one
  supervised child, surface its Health in the UI (attach/starting/failed states),
  point the bridge at the stack's UDS, and construct the UI's `Connection` by
  calling the new `WhoAmI` RPC over the socket to learn the caller account id
  (DL-111; token unset — the socket's ambient-admin ACL is the credential,
  and the id is returned, never guessed). App
  quit behavior per DL-108 (`--linger` default + explicit "Quit and
  stop stack").
- **Interfaces:** consumes T2's `compass-stack` CLI + Health, T3's bridge dial
  target, T1's `ConnectionProvider`, and the DL-112 agent-image + DL-111
  caller-identity resolutions. Produces the installable single-user app: launch →
  stack up → board renders live over the socket.
- **Gate:** e2e smoke on the Linux dev box: fresh state dir → launch app →
  stack reaches ready → UI shows live server info and a streamed event AND **one
  agent session reaches a running container** (the product's actual purpose — a
  board that renders over an empty stack is a false green); quit → relaunch
  attaches to the lingering stack (or restarts it, per DL-108). Manual QA + a
  scripted `compass-stack`-level CI variant (headless, no webview).

### T5 — Native-client mode end-to-end (Beta/GA)

- **Do:** `mode = "client"` support: config parsing (`server_url`, optional
  `ca_cert`), a connect screen collecting **only** the bearer token (the caller
  account id is read via the `WhoAmI` RPC after auth, DL-111 — no caller-id
  field; the parked `connection.ts:28-35` gap is retired);
  the pasted token is handed to the shell over an IPC command for keychain
  write and header injection, after which the UI's `Connection` drops it (per DL-109, never
  the config file, never argv — the bridge injects the bearer, the UI carries
  none); bridge dial target = the remote door with bearer injection and optional
  private CA anchor; `GetServerInfo` as the post-connect probe
  (`client.ts:48-51`) with legible failure states (bad URL / bad cert / bad
  token distinguished) AND an app-vs-server `apiVersion` compatibility check
  (exact-match-or-legible-error for the MVP — `probeServer` already returns
  version + apiVersion, `client.ts:48-51`).
- **Interfaces:** consumes T3's remote dial target, the network door's bearer
  contract (`network_door.go:223-232` bearer interceptors;
  `issueAndWriteAdminToken` `network_door.go:333-339` as the token source the
  operator reads), OS keychain API per framework. Produces the same binary
  connecting to an established remote server.
- **Gate:** e2e against a locally-started `compass-server --listen` with the
  self-signed anchor: connect screen → token accepted → board renders; wrong
  token → legible unauthenticated state; version mismatch → legible
  incompatibility state; token survives app restart via keychain; nothing secret
  in config file or process table (asserted); the UI-side `Connection` is
  asserted to hold no bearer (the shell injects).

### T6 — Packaging + CI baseline

- **Do:** reproducible build of the app bundle for Linux (the dev/dogfood
  target; macOS packaging tracked as follow-up per A5), embedding the built UI
  and the `compass-stack`/`compass-server`/`compass-runner` binaries (the agent
  GHCR at first run per DL-112, so a
  GHCR publish lane for `compass-agent` is a T6 dependency);
  moon-registered `build`/`test`/`ci` lanes for the shell project; a
  version-stamp path (`-ldflags -X main.version` mirrors
  `compass-server/main.go:24-27`).
- **Interfaces:** consumes T3/T4/T5 outputs, the moon workspace map, existing
  `-ldflags` version convention. Produces an installable artifact CI builds on
  every PR.
- **Gate:** CI green building the bundle from a clean checkout; the artifact
  launches on the dev box and passes the T4 smoke.

## Tasks

- [ ] **T1** UI connection-provider seam: `ConnectionProvider`, `ShellIpc`,
  injected-`fetch` client factories; env-provider parity + adapter stream tests.
- [ ] **T2** `go/internal/stack` + `compass-stack up|down|status` (expiry-aware
  cert, configured port, `--image`/`--runtime-dir` runner spawn, lockfiled
  attach); sequencing/attach tests + Linux integration up→ready→down.
- [ ] **T3** Shell scaffold + `compass_rpc` bridge (Wails v3, DL-110); unary/
  cancel bridge tests over a stub UDS server.
- [ ] **T4** Embedded mode e2e (Dogfood): preflight (incl. GHCR agent-image pull,
  DL-112), `compass-stack` spawn, Health surfacing, `WhoAmI` caller-identity (DL-111),
  socket-backed
  board with one live agent session; smoke on the dev box.
- [ ] **T5** Native-client mode e2e (Beta/GA): config + connect screen +
  shell-injected keychain token + `WhoAmI` caller-id + apiVersion check + remote
  bridge; auth-failure
  legibility tests.
- [ ] **T6** Packaging + CI lanes; clean-checkout bundle build passes T4 smoke.

## Decisions

These were the load-bearing forks; each was escalated to Matt and is now
**resolved**. Each ruling is baked into the Approach, Global Constraints, and
Plan above (cited by its DL row) and stamped as an `Active` row in
§Ledger-impact — this section is the reasoning trail behind each, retaining the
Question/Options that led to the ruling. The resolutions froze on merge and are
the contract the executing lanes read.

### OQ1 — Shell framework: Tauri (Rust) vs Wails (Go) vs other  *(blocks T3)*

**Question:** which framework hosts the webview and implements the IPC bridge?

**Options and the real tension:**

- **Tauri 2 (Rust).** The pre-pivot stated choice: the README architecture
  diagram still says "Desktop shell (Tauri) + web UI (SolidJS)" (README.md:20),
  `apps/ui` already declares `@tauri-apps/api ^2` + `@tauri-apps/plugin-opener
  ^2` (`apps/ui/package.json:12-13`), and the existing `daemon-transport.ts` is
  written against Tauri's `invoke`/`Channel` (`daemon-transport.ts:15`), so T3's
  UI side is nearly done. `tauri::ipc::Channel` is purpose-built ordered
  streaming that works on WebKitGTK (the DL-044 finding,
  `compass-tauri-shell.md:48-50`), and Tauri's packaging/signing/auto-update
  ecosystem is the most mature in class. Cost: it **reintroduces a Rust
  toolchain** to a stack the pivot made fully Go+TS (`go/go.mod:1-3`: "One
  module for the whole backend"; no `crates/` exists in the repo today) —
  rust-toolchain pinning, a second language's CI lane, and a maintenance
  surface no other component shares.
- **Wails v3 (Go).** Keeps the shell in the backend's own language: the bridge
  pump, TLS/CA handling, and process supervision reuse the same Go packages the
  server/runner already use (`connectrpc.com/connect`, the stack library from
  T2 imported directly rather than shelled out to — the shell could even run
  `stack.Up` in-process, see OQ2). One toolchain, one CI story. Costs: smaller
  ecosystem and less battle-tested packaging/signing/auto-update than Tauri;
  v3 (the version with the cleaner event/bindings model) is newer; the
  UI-side `ShellIpc` implementation is written fresh against Wails runtime
  events rather than adapted from the existing file.
- **Both share the webview risk.** WebKitGTK on Linux is the weak point for
  either (both use the system webview), so the Linux rendering story does not
  differentiate them. A deferred native-path rendering endgame (RIG-1006)
  would replace the webview under either choice and slightly favors not
  over-investing in framework-specific surface.

**Recommendation: Wails v3.** The pivot's own logic — one Go module, one
toolchain, orchestration lives in Go — argues for finishing the job rather than
re-adding Rust for the one thin component the stack has left. The embedded mode
is the product's center of gravity (Dogfood first), and it is exactly where
Wails collapses complexity: the shell imports `stack.Up` and the bridge pump as
libraries instead of supervising a Rust↔Go process boundary — this argument
requires the shell's Go module to live INSIDE `go/` (e.g. `go/cmd/compass-app`)
so it can import `go/internal/stack`; name that placement when picking Wails.
T1 deliberately shrinks the switching cost to one `ShellIpc` implementation
either way, so this recommendation is cheap to overrule; if
packaging/auto-update maturity is weighted highest, Tauri is the defensible
counter-pick (its shell is a separate Rust crate, `compass-stack` stays the
supervised child).

**Decision (Matt): Wails v3 (Go).** The shell is a Wails v3 project whose Go
module lives under `go/` (`go/cmd/compass-app`) so it imports `go/internal/stack`
and the bridge pump directly. Supersedes DL-044's Tauri framework choice
(DL-110). `apps/ui`'s `@tauri-apps/*` deps are dropped in T1; `daemon-transport.ts`
is re-pointed at Wails runtime events behind `ShellIpc`.

### OQ2 — Embedded server lifecycle: in-process vs spawned; linger-on-quit  *(blocks T4 final behavior; T2 proceeds either way)*

**Question:** (a) does the embedded server run in-process (Wails-only option)
or as spawned child processes; (b) does the stack outlive the app on quit?

**Options:**

- **(a) Spawned children via `compass-stack` (works under either framework).**
  The server/runner remain the shipped binaries; the supervisor is testable
  headless; a wedged stack can be restarted without relaunching the app; crash
  isolation (a server panic never takes the window down). In-process (Wails
  importing `server.Serve` directly) saves process management but couples
  server lifetime to the app process, loses the headless CLI path, and the
  runner STILL must be a separate process (it hosts containers and hard-checks
  its own uid, `compass-runner/main.go:178-188`) — so in-process eliminates at
  most one of the two children.
- **(b) Linger vs die-with-app.** DL-044 chose detached-spawn so the daemon
  outlives the shell (`compass-tauri-shell.md:73-77`). The two-process Go stack
  reframes it: agents run in containers under the runner, and killing the stack
  on quit kills in-flight agent sessions — a single user closing the window
  over lunch should not lose running work. Against lingering: a single-user
  desktop app that leaves three background processes after "quit" surprises
  users and complicates upgrades.

**Recommendation:** (a) spawned children always — in-process saves too little
(the runner stays a process regardless) and costs the headless path; this also
keeps OQ1 fully decoupled. (b) **linger by default** with an explicit
"Quit and stop stack" action and a visible "stack running" indicator on next
launch (attach-if-live is already T2 behavior). Rationale: in-flight agent work
is the valuable thing; losing it on window-close is worse than a lingering
process. If Matt prefers die-with-app, only the T4 quit handler and the
`--linger` default flip.

**Decision (Matt): as recommended** — spawned children via `compass-stack`
always (never in-process), linger-by-default with an explicit "Quit and stop
stack". Parameter of the supervisor decision (DL-108), not a separate ledger row.

### OQ3 — Native-client token acquisition and storage  *(blocks T5)*

**Question:** how does the remote client obtain and store its bearer?

**Today's source of truth:** the network door mints a bootstrap-admin token to
a 0600 file under the server's state dir, "for the operator to read"
(`network_door.go:333-339`); no token-issuing RPC exists for clients.

**Options:** (i) paste-a-token connect screen, stored in the OS keychain
(Secret Service/Keychain per platform); (ii) same, stored in a 0600 file under
the app's state dir (the server's own on-disk pattern); (iii) defer entry to
env (`VITE_COMPASS_TOKEN`-style) for beta, no storage; (iv) design a
device-authorization/token-exchange RPC now.

**Recommendation:** (i), falling back to (ii) on hosts without a keyring
(headless-ish Linux), matching the precedent already ruled for the server side
("keyring stays the default for the single-user desktop-local Server …
container/headless deploys wire … a machine-authenticated provider",
`compass-agent-container-runtime.md:1114-1118`). (iv) is the right GA endgame
but is server-side scope (a new compass.v1 surface) that must not gate the
native-client MVP; park it as a named follow-up. The pasted token is handed to
the shell over IPC for keychain write + header injection; the UI-side
`Connection` never retains it (A2).

**Decision (Matt): as recommended** — paste-a-token connect screen → OS keychain,
0600-file fallback on keyring-less hosts. Covered by DL-109 (credentials
keychain-first, never config/argv).

### OQ4 — Embedded database: what does "ensure Postgres" mean in an installed app?  *(blocks T2 `ensure database` + T4 preflight)*

**Question:** the server refuses to start without a Postgres DSN
(`compass-server/main.go:108-109`; store is pgx-only,
`go/internal/store/store.go:60-67`). The devenv loop leans on
`services.postgres` (`devenv.nix:159-164`) — an installed app has no devenv.
What does embedded mode do?

**Options:** (i) bundle and supervise a private Postgres under the app state
dir (`initdb` + `pg_ctl` as another `compass-stack` child; the heaviest but
fully self-contained); (ii) require a host Postgres and preflight it (cheap,
but breaks the "nothing else to stand up" charter); (iii) an embedded-Postgres
Go package running the same engine in-app-dir (a maintained wrapper around (i));
(iv) add a SQLite/embedded-store backend to the server (largest change: the
store is pgx-specific with Postgres-only constructs like advisory locks,
`store.go:113-117`).

**Recommendation:** (i)/(iii) — a `compass-stack`-supervised private Postgres in
the app state dir. It preserves the charter, touches zero server code, and the
supervisor already exists to own it (T2's "ensure database" step). (iv) is a
store rewrite this record must not smuggle in; (ii) is acceptable only as the
beta stopgap if Matt wants T4 sooner — the seam is one `Config.DatabaseDSN`
field either way, so the choice does not reshape the design.

**Decision (Matt): as recommended** — a `compass-stack`-supervised private
Postgres in the app state dir (host-Postgres acceptable only as a beta stopgap).
Folded into the supervisor decision (DL-108, the "ensure database" step).

### OQ5 — uid-1000 preflight vs fix  *(sub-fork found in source; blocks T4 preflight copy only)*

**Question:** the runner refuses any uid but 1000 (`verifyRunnerUID`,
`compass-runner/main.go:178-188`) because the agent image bakes uid 1000 and
podman `--userns=keep-id` maps the host uid through unchanged. A general
desktop user is frequently uid 1000 on single-user Linux — but not always.
Does embedded mode (i) preflight-and-refuse with clear copy (status quo,
zero runner change), or (ii) fund the runner/image work to support arbitrary
uids (e.g. `--userns=keep-id:uid=1000` mapping) as part of this epic?

**Recommendation:** (i) for Dogfood — the dev box satisfies it and the failure
is now legible instead of "deep inside the first nix build"
(`compass-runner/main.go:170-172`). File (ii) as a named GA-blocking follow-up
issue, because "works only as uid 1000" is not shippable to general users; it
is runner/image scope, not shell scope, and must not ride this record.

**Decision (Matt): as recommended** — preflight-and-refuse with legible copy for
Dogfood; arbitrary-uid support filed as a named GA-blocking follow-up issue
(runner/image scope, not this record). No ledger row — inherited constraint.

### OQ6 — Agent-image distribution for embedded mode  *(blocks T4; reshapes T6)*

**Question:** the runner refuses to boot without an agent container image
(`compass-runner/main.go:111-114`) and every agent session runs inside one — but
the devenv chain deliberately keeps the image build OFF the hot `up` path as "a
heavy closure" (`devenv.nix:145-146`), and an installed app has no nix build. How
does the image reach an end user's local podman store?

**Options:** (i) bundle the image as an OCI tarball inside the app bundle and
`podman load` it as a `compass-stack` step (fully self-contained, but a large
artifact and a re-load on every image bump); (ii) pull from a published registry
at first run (small bundle, but needs a publishing lane and network at setup);
(iii) local nix build (dev-only; dead for GA). Each reshapes T6 packaging
differently.

**Recommendation:** (i) for Dogfood/self-contained GA (the charter is "nothing
else to stand up"), with (ii) as the bandwidth-friendly path once a publish lane
exists. This is genuinely undecided and cross-lane (compass-runner owns the image
build, T6 owns packaging), so it is Matt's call, not a proxy pick. Whatever
lands, T4's gate requires one agent session reaching a running container — a
board that renders over an imageless stack is a false green.

**Decision (Matt): pull from GHCR.** The `compass-agent` image is published to
GHCR and `podman pull`ed at first run (option ii), NOT bundled — a GHCR publish
lane for `compass-agent` becomes a T6 dependency, and embedded preflight surfaces
a legible error when the pull is unavailable offline. New ledger row DL-112
(cross-lane: compass-runner owns the image build + publish, T6 owns the pull
step + packaging).

### OQ7 — Embedded caller-identity mechanism  *(blocks T4; cross-lane compass-server)*

**Question:** the UI's `Connection` needs a caller account id, but account ids
are 128 random bits minted per database and cannot be guessed
(`apps/ui/.env.development:32-35`); there is no WhoAmI RPC, and the additive
`caller_account_id`-on-`GetServerInfo` fix is *parked* (`connection.ts:28-35`).
Native-client limps by having the operator paste the id beside the token, but
**embedded mode has no connect screen and no operator** — so how does embedded
mode learn the bootstrap-admin id to construct a valid `Connection`?

**Options:** (a) land the parked additive `caller_account_id` on `GetServerInfo`
as part of this epic — small, additive, fixes BOTH modes (and deletes the
caller-id field from the T5 connect screen); (b) have `compass-stack` query the
admin id from the store (it holds the DSN) and surface it via
`compass-stack status`/Health for the shell to inject; (c) a dedicated WhoAmI
RPC.

**Recommendation:** (a) — it retires an acknowledged interim seam instead of
building embedded mode on top of it, and fixes native-client in the same stroke.
It is a compass-server change (a new field on an existing RPC), so it is a
cross-lane decision for Matt + the compass-server owner, not a shell-lane pick.
Until it lands, embedded mode has no correct fallback — (b) is the only
shell-scoped stopgap, and it is a worse seam. This is the sharpest cross-lane
dependency in the epic.

**Decision (Matt): a dedicated `WhoAmI` RPC** (option c). This epic adds a new
`compass.v1` `WhoAmI` RPC returning the caller's account id derived from its own
credential (embedded: the socket's ambient-admin identity; native-client: the
bearer's subject). It retires the parked interim seam (`connection.ts:28-35`)
and removes the caller-id field from the native-client connect screen entirely.
New ledger row DL-111 (cross-lane compass-server: a new authenticated RPC on the
contract; compass-ui consumes it in the boot resolver).

## Ledger-impact

New decision rows (the compass single-writer assigned this lane the contiguous
block DL-106..112; the compass-native lane is NOT the DL single-writer):

- `DL-106` — One native binary, two modes (embedded / native-client); the
  mode difference is confined to the connection provider and the stack
  supervisor; nothing above the transport boundary assumes local (§A1).
- `DL-107` — One framework-neutral shell IPC contract (`compass_rpc` +
  `head|body|end|error` frames) carrying gRPC-Web over a custom `fetch` for
  both modes; embedded pumps to the UDS, native-client to the TLS network door
  with shell-side private-CA trust and shell-injected bearer (§A2).
- `DL-108` — Embedded lifecycle is a Go stack supervisor
  (`go/internal/stack` + `compass-stack up|down|status`) of spawned children
  (never in-process), linger-by-default, productizing the dogfood chain with a
  supervised private Postgres, expiry-aware cert rotation, and lockfiled attach
  (§A3; OQ2/OQ4).
- `DL-109` — Mode selection via `$XDG_CONFIG_HOME/compass/app.toml`
  (absent → embedded); native-client bearer entered in a connect screen and
  stored keychain-first (0600-file fallback), never config-file or argv
  (§A4; OQ3).
- `DL-110` — The native shell is **Wails v3 (Go)**, its module under `go/`
  (`go/cmd/compass-app`) importing `go/internal/stack` and the bridge pump
  directly; the UI-side `ShellIpc` targets Wails runtime events and `apps/ui`'s
  `@tauri-apps/*` deps are dropped (OQ1). Re-decides DL-044's framework on the
  Go stack.
- `DL-111` — A new `compass.v1` `WhoAmI` RPC returns the caller's account id
  from its own credential (embedded: socket ambient-admin; native-client: bearer
  subject), retiring the parked `caller_account_id` seam (`connection.ts:28-35`)
  and the connect-screen caller-id field (§A4; OQ7). Cross-lane compass-server +
  compass-ui.
- `DL-112` — The `compass-agent` image is published to GHCR and `podman pull`ed
  by `compass-stack` at first run (not bundled); a GHCR publish lane for
  `compass-agent` is a T6 dependency (OQ6). Cross-lane compass-runner + T6.
- **DL-044 flip:** `Status` → `Superseded by DL-106` in
  `docs/designs/product/DECISIONS.md:177`, and `compass-tauri-shell.md`'s
  `Status:` header updated per the record-Status grammar. DL-044's transport
  seam and thin-shell intent are re-expressed here on the Go stack
  (§A2/§A3/Global Constraints) and its framework choice is re-decided by DL-110
  (Wails, not Tauri); its Rust-`compassd` context is retired.
- Same-PR citation sweep for `per DL-044` when the row flips (none found in
  `compass` code this session; sweep at flip time).
- On merge (the freeze), THIS record's own `Status: Draft` flips to `Active` per
  the record-Status grammar; the 7 new DL rows already stamped `Active` cite it,
  so the flip is a required same-PR merge step.

`Ledger-impact:` new rows DL-106..112 + DL-044 → Superseded by DL-106
(block assigned by the compass single-writer).
