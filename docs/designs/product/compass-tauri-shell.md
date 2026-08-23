# Compass Tauri desktop shell — design

Status: Superseded by compass-native-app/design.md

Design for the Compass desktop shell (SEA-1022): the thin native app that hosts
the UI webview and connects it to the Compass daemon. Companion to the
[architecture lineage](compass-architecture-lineage/design.md) record and the transport spec
[`../../specs/product/compass.md`](../../specs/product/compass.md).

## Problem / Intent

The SolidJS UI (`apps/ui`) today runs only in a dev browser against the daemon's
opt-in dev-loopback TCP endpoint. The shipped path needs a native desktop shell
that hosts the webview and reaches the daemon over its **owner-only Unix socket**
— no browser, no daemon-exposed TCP port (§7.1). The shell is **thin**: window,
daemon launch/supervision, a webview↔daemon bridge, and loading the UI. It holds
**no backend logic**, so a compromised agent finds no privileged surface in it
(§7.1, §7.3).

This covers the **v1 thin slice**. Native affordances (tray, notifications, deep
links) and packaging (signed installer, auto-update) are deferred to follow-ups.

## Approach

### Webview → daemon transport

A WebView's `fetch`/XHR speaks only HTTP(S) and cannot dial `AF_UNIX`, so a shim
always bridges the webview to the daemon's socket. The choice is load-bearing
because `SubscribeEvents` is **server-streaming** and is the daemon's entire push
channel (§7.2), so the bridge must stream incrementally.

**Chosen: reuse the gRPC-Web transport with a `fetch` that rides Tauri IPC.**
`@connectrpc/connect-web`'s `createGrpcWebTransport` accepts a **`fetch`
override**. The shell passes a `fetch` that serializes the request through a
Tauri `invoke` and returns a `Response` whose `body` is a `ReadableStream` fed by
a Tauri **`Channel`** (`tauri::ipc::Channel` — ordered binary chunks to a JS
`onmessage`, purpose-built for Rust→JS streaming). A Rust command in the shell
dials the daemon's UDS (native gRPC) and pumps the gRPC-Web response frames back
over the Channel. All gRPC-Web framing/parsing/streaming stays in the library —
the shell supplies only bytes-in/bytes-out plumbing.

- `@compass/client` is built for this: it exports `createCompassClient(transport)`
  and re-exports `Transport` so a non-web consumer can supply a custom transport.
  The shell builds the same gRPC-Web transport the dev browser uses, swapping only
  its `fetch`. Server-streaming flows incrementally because the Channel delivers
  chunks as they arrive and the custom `fetch`'s `ReadableStream` yields them to
  the transport's existing parser.
- This uses Tauri's **core IPC** (`invoke` + `Channel`), which streams on
  WebKitGTK. It is **not** the custom-URI-scheme responder path, which cannot
  stream (see rejected alternatives).
- It preserves the daemon's **owner-only socket** end to end: only the shell
  process (running as the user) dials the UDS; the webview reaches the shell via
  in-process IPC that is not network-reachable. **Zero TCP**, honoring §7.1.

**Rejected — custom URI scheme (`compass://`) proxying to the UDS.** Tauri 2's
`register_asynchronous_uri_scheme_protocol` resolves a response as a single
buffered `Vec<u8>` with no chunked/streaming path, and WebKitGTK (a priority
Linux target, §7.3) lacks the underlying streaming API. A streaming RPC would
deliver nothing until the stream closed — fatal for the event channel.

**Rejected — token-guarded loopback TCP proxy.** Binding `127.0.0.1:<ephemeral>`
and byte-proxying the webview's gRPC-Web to the UDS streams natively and reuses
the gRPC-Web stack with no adapter code, but it discards Tauri's security model:
a loopback TCP port is network-reachable, has no filesystem ACL, and reintroduces
the surface the owner-only socket exists to remove. A per-launch bearer token
only approximates owner-only, and it generalizes the token-guarded-loopback
posture (blessed only for the unavoidable WSL2 VM boundary, §7.3) to every
platform. Kept as a fallback only if the `fetch`↔Channel adapter proves
unexpectedly costly.

### Daemon lifecycle — detached spawn + attach

§7.1 requires the daemon to **outlive any UI session**, so the shell does not run
it as a Tauri sidecar (sidecars die with the app). On launch the shell resolves
the socket path (`$XDG_RUNTIME_DIR/compass/compassd.sock`, falling back to
`$HOME/.compass/compassd.sock`), then probes: if a live daemon answers, attach;
otherwise spawn `compassd` **detached** so it survives shell quit. The daemon's
single-instance startup already distinguishes a live daemon from a stale socket,
so a double-spawn race resolves to "refuse + attach." v1 supervision is minimal —
spawn-if-absent plus surfacing daemon liveness from the `SubscribeEvents`
`DaemonStatus` stream; auto-restart-on-crash is a follow-up.

Spawn/attach is the **local-mode** branch (co-located daemon). A future hosted
mode (below) attaches to a remote daemon it does not manage and never spawns, so
"spawn if absent" is the local-transport branch, not an unconditional startup
step.

### v1 scope boundary

- **In v1:** window; detached spawn/attach + liveness surfacing; the
  webview↔daemon bridge; bundle + load the built UI (`apps/ui/dist`); prove the
  full contract path by rendering `GetDaemonInfo` and a live `SubscribeEvents`
  `DaemonStatus`.
- **Deferred (own issues):** tray, OS notifications, deep links; a signed
  installer with auto-update (needs code-signing identities/secrets not yet
  provisioned); Windows/WSL2 (a later epic).

### Shell crate location — `crates/compass-shell/`

A Rust Tauri crate under `crates/` auto-joins the root Cargo workspace
(`members = ["crates/*"]`) with no root-workspace churn. moon
projects are an explicit map, not glob-discovered, so the crate must be
registered in `.moon/workspace.yml` (`compass-shell: 'crates/compass-shell'`)
for its CI lane to appear. It is application-layer like `compass-ui`, and moon
forbids one application project `dependsOn` another, so the shell embeds the UI's
built `dist/` via a task-level `deps: ['compass-ui:build']`, not a project
`dependsOn`. Tauri's `frontendDist` points at `../../apps/ui/dist`.

### Transport is a swappable seam (enables a future hosted/remote daemon)

A hosted deployment — the daemon on a different machine than the client (a
managed runtime) — stays possible without changing this design. Transport is
chosen at client construction (`createCompassClient(transport)`, §7.2/§7.5), so
topology is just which transport the client receives. A hosted mode is a
**sibling transport**: the same custom-`fetch` seam with the shell's Rust command
dialing a TLS remote with real auth instead of the UDS (or a pure browser using a
gRPC-Web transport against the hosted URL, supported today). The hosted-mode work
is daemon-side and out of scope here — the daemon has no authenticated network
listener today (UDS + dev-loopback only), so hosted mode needs a TLS+auth server
transport on the daemon plus a client-side transport-mode selector, its own future
workstream. **Invariant that keeps it open:** the shell and UI must never assume
"local" beyond the transport boundary — no socket path or `localhost` leaks above
the `fetch`/command seam.

## Global Constraints

- **Tauri 2.x.** Rust toolchain from `rust-toolchain.toml`; no per-crate toolchain.
- **Linux system libs gate every build task:** `webkit2gtk-4.1`, `libsoup-3`,
  `pkg-config` (plus Tauri's gtk build deps) must be present in **both**
  `devenv.nix` (the Linux-only `packages` block) **and** `ci/ci-toolchain.nix`, or
  `moon run compass-shell:*` fails to link in the `sealed-ci` image. Land this
  first.
- **Thin shell invariant:** window + spawn/supervise + bridge only. Any
  `compass.v1` command logic in the shell is a bug (§7.1); contract access is only
  through `@compass/client`.
- **No daemon-exposed TCP** on macOS/native Linux (§7.1). The bridge honors this
  (transport above).
- **CI lane** appears once the project is registered in `.moon/workspace.yml`
  (moon uses an explicit project map, not glob discovery); expose `build` /
  `test` / `clippy` / `ci` tasks mirroring `compass-daemon`'s `moon.yml`
  (Petrel fans out the affected `runInCI` tasks — no pipeline edit needed once
  registered). Note nextest exits non-zero on an empty run, so a not-yet-tested
  crate's `test` task needs `--no-tests=pass`.
- **License** `AGPL-3.0-only` (application crate, like `compass-daemon`).

## Plan

Right-sized tasks, each carrying its own test/gate cycle, ordered by dependency.

### Task 1 — Land Tauri system deps in devenv + CI image

- **Do:** add `webkit2gtk-4.1`, `libsoup-3`, `pkg-config` (plus any Tauri gtk build
  deps) to `devenv.nix`'s Linux-only `packages` block and to `ci/ci-toolchain.nix`.
  Confirm a throwaway Tauri stub `cargo build` links in the dev shell and the CI
  image derivation evaluates.
- **Interfaces:** consumes `devenv.nix`, `ci/ci-toolchain.nix`,
  `rust-toolchain.toml`. Produces a dev shell + CI image where a Tauri crate links.
- **Gate:** the libs resolve via `pkg-config`; a stub Tauri `cargo build` succeeds
  under `direnv exec .`.

### Task 2 — Scaffold `crates/compass-shell` (window + static UI)

- **Do:** create the Cargo Tauri crate (joins the workspace glob),
  `tauri.conf.json` (`frontendDist: ../../apps/ui/dist`, tray/updater off),
  `main.rs` opening one window that loads the built UI. `moon.yml` with
  `build`/`test`/`clippy`/`fmt`/`ci`, `dependsOn: ['ui']`, `deps: ['ui:build']`.
  AGPL manifest like `compass-daemon`.
- **Interfaces:** consumes `apps/ui` `dist/` (Vite build output). Produces a
  `compass-shell` binary showing the UI statically (not yet daemon-connected).
- **Gate:** `moon run compass-shell:build` green; launching shows the current UI.

### Task 3 — Daemon spawn + attach (detached, outlives shell)

- **Do:** resolve the socket path (share/reuse the daemon's default-path logic, or
  mirror it with a test asserting parity); probe for a live daemon; attach if
  present, else spawn `compassd` detached. Surface a not-running/failed state to
  the UI.
- **Interfaces:** consumes the `compassd` binary, its socket-path contract, and the
  daemon's single-instance behavior. Produces a running/attached daemon on launch,
  surviving shell quit.
- **Gate:** unit tests for probe→attach vs probe→spawn; a test that the daemon
  survives shell process exit.

### Task 4 — Webview ↔ daemon bridge

- **Do:** a Rust `invoke` command dials the daemon UDS (native gRPC) and pumps
  gRPC-Web response frames back over a `tauri::ipc::Channel`; a JS custom `fetch`
  (request → `invoke`, response `body` → a `ReadableStream` fed by the Channel)
  passed to `createGrpcWebTransport({ fetch })`, with the UI building its client via
  `createCompassClient(thatTransport)`. Handle stream cancellation (webview drops
  the reader) and gRPC trailer/error mapping.
- **Interfaces:** consumes the daemon UDS (native gRPC),
  `@connectrpc/connect-web`'s `createGrpcWebTransport({ fetch })`, and
  `@compass/client`'s `createCompassClient`/`Transport` seam. Produces a webview
  that completes `GetDaemonInfo` and a live streaming `SubscribeEvents`, zero TCP.
- **Gate:** unit tests on the `fetch`↔Channel adapter (unary + a multi-frame server
  stream + mid-stream cancellation); UI typechecks against the new client wiring.

### Task 5 — End-to-end smoke + gate

- **Do:** a launch-the-shell E2E that spawns/attaches the daemon and asserts the UI
  renders `GetDaemonInfo` and a streamed `DaemonStatus{Ready}`.
- **Interfaces:** consumes the full stack (Tasks 1–4). Produces green
  `compass-shell:ci`.
- **Gate:** `moon run compass-shell:ci` green; manual QA: launch → window → UI
  shows live daemon status over the socket.

## Tasks

- [ ] **T1** Tauri system deps in `devenv.nix` + `ci/ci-toolchain.nix` (link-check).
- [ ] **T2** Scaffold `crates/compass-shell` — window loads `apps/ui/dist`; moon
  `build`/`test`/`clippy`/`fmt`/`ci`.
- [ ] **T3** Detached daemon spawn + attach + liveness surfacing; probe/spawn tests.
- [ ] **T4** Bridge — `createGrpcWebTransport({ fetch })` over `invoke` + `Channel`
  to the UDS pump; wire the UI client.
- [ ] **T5** E2E smoke + green `compass-shell:ci`.
