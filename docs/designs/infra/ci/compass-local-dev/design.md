# Compass local dev: UI in `devenv up`, macOS full setup, gate hazard

Status: Draft

> **Design record (platform).** Local development experience on Linux and
> macOS for the Compass repo. Three lanes: (1) `devenv up` starts the browser
> UI with a route to the server's gRPC-Web dev door — direct-dial, no proxy
> (decided; Matt, 2026-08-14); (2) macOS gets the full
> setup — native postgres/server/UI, the podman runner loop in a Linux VM,
> and a native desktop-shell build path that does not exist today;
> (3) the pre-push tool-drift hazard is DOCUMENTED with its interim
> workaround — the durable fix is a harness change owned by the zireael
> (jj-hp) lane, out of scope for this repo (Matt, 2026-08-14). All file:line
> citations are from this checkout (`RigelBuild/compass` working copy).

## Problem / Intent

Local dev has three gaps. First, `devenv up` stands up the whole backend but
no UI: `devenv.nix:218` (`processes = lib.optionalAttrs pkgs.stdenv.isLinux`)
defines only `compass-server` and `compass-runner`, and the UI dev server has
no route to the gRPC-Web door — `apps/ui/vite.config.ts:4-6` says so itself:
"The UI consumes the generated @compass/client; the daemon transport + dev
proxy arrive with the local-transport work." Second, macOS has no path at
all: `services.postgres` (devenv.nix:211), `processes` (devenv.nix:218), and
`tasks` (devenv.nix:346) are all `lib.optionalAttrs pkgs.stdenv.isLinux`, and
the native desktop shell has no darwin entrypoint (grounded in §A2c). Third,
the pre-push gate (`hk.pkl:31-33`, `check = "moon ci"`) runs in jj-vine's
temp worktree `/tmp/jj-hooks-worktree-*`, which has the tree but no direnv
activation, so `moon ci` resolves biome/moon/proto from the ambient system
PATH instead of the devenv pins — `biome.json:2` pins schema **2.5.4**
(`"$schema": "https://biomejs.dev/schemas/2.5.4/schema.json"`, matching
`bun.lock:237` `"@biomejs/biome@2.5.4"`) while the system biome is 2.4.16,
which reformats files 2.5.4 leaves clean → a false-red gate. Matt ruled
(2026-08-14) the durable fix is **at the harness**: jj-hp (the jj-hooks
crate, zireael) will propagate the repo's devenv env into its temp worktree —
a one-time fix benefiting every repo, owned and routed in the zireael lane.
This record therefore only documents the hazard and the interim workaround;
it designs no compass-side gate change.

Local dev is the developer's own box — not a deployed `main` or `preview`
environment. Those deployed environments are internal infrastructure, defined
in the private infrastructure design repo, and are not redesigned here. The
repo-facing PR-validation surface (the expanded e2e harness and its
results-on-PR) is Record B
(`docs/designs/platform/compass-pr-validation/design.md`).

## Approach

### A1 — `compass-ui` in `devenv up` + the route to the dev-http door

Two pieces: the route from the browser to the dev-http door (direct-dial —
decided; the proxy kept documented as the prod-parity alternative) and the
process.

**The route.** The server already serves a loopback gRPC-Web door for exactly
this consumer — `devenv.nix:219-220`: "compass-server: serves compass.v1 on a
Unix domain socket (the shipped local door) plus a loopback gRPC-Web port for
the browser UI dev server", with `ports.devhttp.allocate = 50051`
(devenv.nix:260). The UI dials whatever `VITE_COMPASS_BASE_URL` resolves —
`apps/ui/src/live/connection.ts:46-50`:

```ts
const baseUrl = env.VITE_COMPASS_BASE_URL?.trim();
if (!baseUrl) {
    throw new Error(
        "VITE_COMPASS_BASE_URL is required to reach the Compass server; " + ...
```

Crucially, the dev door already carries a permissive wildcard CORS policy
built expressly for a browser dev server: `devCORS()` sets `AllowedOrigins:
["*"]` with the Connect/gRPC-Web headers allowed and the grpc-status
trailers exposed (`go/server/serve.go:689-696`, `AllowedOrigins` at `:691`),
and the door is documented as existing "for a browser dev server"
(serve.go:65-67; the dev server is wrapped with that policy at
serve.go:604, `devCORS().Handler(devMux)`).
An earlier draft of this record motivated a vite proxy
with "the browser stays same-origin (no CORS surface on the dev door)" —
that premise is false: the CORS surface exists today, by design, for this
exact consumer.

**Direct-dial (decided).** No proxy at all: the compass-ui
process sets `VITE_COMPASS_BASE_URL = "http://127.0.0.1:${toString
config.processes.compass-server.ports.devhttp.value}"` — the same binding
the server's own `--dev-http` flag reads (devenv.nix:253) — and the browser
fetch streams the gRPC-Web response natively, with no middlebox to buffer
it. `connection.ts` was built for exactly this client: "`token` undefined is
a deliberate no-auth client (the dev door)"
(apps/ui/src/live/connection.ts:17-19). Devenv stays the single owner of the
port number, `vite.config.ts` needs no proxy block, and the proxy-streaming
risk T2's dropped proxy leg existed to verify is gone entirely — T2's live
arm is the streaming smoke against the direct dev-door.

**The proxy alternative (prod-parity) — documented alternative, not a live
fork.** Direct-dial is decided; this shape is recorded only in case prod
same-origin parity is later wanted. The proxy's one genuine benefit is
same-origin parity with production, where the deployed UI is same-origin or
sits behind the single-origin network-door CORS (`networkCORS(origin)`,
go/server/network_door.go:141-148). Its cost is a middlebox whose unbuffered
forwarding of server-streaming RPCs (`SubscribeEvents`, `SubscribeComms`)
must be proven, not trusted — the streaming smoke T2's dropped proxy leg
described. If that parity is ever wanted, the shape is:

```ts
// apps/ui/vite.config.ts — the server block grows a proxy
const devhttpPort = process.env.COMPASS_DEVHTTP_PORT ?? "50051";
server: {
    port: 5173,
    strictPort: true,
    // Every compass.v1 service path is a gRPC-Web POST of the form
    // /<package>.<Service>/<Method>; match on the package prefix.
    proxy: {
        "^/compass\\.": {
            target: `http://127.0.0.1:${devhttpPort}`,
            changeOrigin: true,
        },
    },
},
```

with `COMPASS_DEVHTTP_PORT` env-injected from
`config.processes.compass-server.ports.devhttp.value` (never hardcoded
twice; the vite default 50051 covers only a bare `moon run compass-ui:dev`
outside `up`) and `VITE_COMPASS_BASE_URL=http://127.0.0.1:5173` (the vite
origin).

**The process.** A third devenv process, launched the way the existing two
resolve proto's shims (devenv.nix:243-244 sets
`PROTO_HOME`/`PATH` in compass-server's exec preamble; same pattern here for
`bunx`):

```nix
compass-ui = {
  exec = ''
    export PROTO_HOME="''${PROTO_HOME:-$HOME/.proto}"
    export PATH="$PROTO_HOME/shims:$PROTO_HOME/bin:$PATH"
    exec bunx vite
  '';
  cwd = "${config.devenv.root}/apps/ui";
  env = {
    # Direct-dial (decided): the browser dials the dev door.
    VITE_COMPASS_BASE_URL =
      "http://127.0.0.1:${toString config.processes.compass-server.ports.devhttp.value}";
  };
  after = [ "devenv:processes:compass-server" ];
};
```

`after` the server's readiness probe (devenv.nix:288-294 gates on a real
`GetServerInfo` answer over the dev-http door) so the first browser load
never races the migrating store. The command matches the existing moon task
(`apps/ui/moon.yml:11-12`: `dev: command: 'bunx vite'`) — one convention, two
entry points. (The exec preamble above is grounded against current main;
RIG-1983 removes proto — see Global Constraints GC-1 — so the preamble
becomes a plain `exec bunx vite` after the cutover.)

**Guard composition.** `processes` is one wholesale-guarded attrset today
(devenv.nix:218). Restructure into a cross-platform base merged with the
Linux-only remainder, mirroring the `env` block's existing pattern
(devenv.nix:89 + :127: `env = { ... } // lib.optionalAttrs
pkgs.stdenv.isLinux { ... }`):

```nix
processes = {
  compass-server = { ... };   # unguarded once A2's macOS lane lands (T4)
  compass-ui = { ... };
} // lib.optionalAttrs pkgs.stdenv.isLinux {
  compass-runner = { ... };   # podman loop: Linux-native only (macOS: VM, A2)
};
```

T3 lands `compass-ui` inside the existing Linux guard (its `after` on
compass-server would dangle otherwise); T4 moves the server+UI pair out of
the guard together.

### A2 — macOS full setup

Per Matt's 2026-08-14 macOS ruling (native services + runner-loop-in-VM):
postgres/server/UI run native on macOS; the podman-backed
runner loop runs against a Linux VM. Three sub-lanes.

**A2a — native services.** `services.postgres`, `compass-server`,
`dogfood:gen-cert`, and `dogfood:mint-runner-token` have nothing structurally
Linux-bound: postgres is a stock devenv service, and the server/cert/mint
lanes are pure-Go builds run through the pinned toolchain. The guard relaxes
by removing `lib.optionalAttrs pkgs.stdenv.isLinux` from `services.postgres`
(devenv.nix:211) and from the server process + its two tasks, leaving
Linux-only: `compass-runner` (devenv.nix:321 — native podman loop),
`dogfood:agent-image` (devenv.nix:401-406 — the vendored-fork nix container
build), and `dogfood:clean` (devenv.nix:413-426 — host rootless podman). The
`PKG_CONFIG_PATH` env stays Linux-guarded exactly as it is — its comment
already states the macOS posture (devenv.nix:122-123: "on macOS the app
links the system WebKit framework, so the closure is Linux's alone").

**A2b — the runner loop in a Linux VM.** The runner cannot run natively on
macOS against a remote podman connection: the per-container agent sockets are
AF_UNIX bind-mounts whose source must be local to the container host
(`go/internal/runner/gateway/socket.go:11-12`: "The listener is created at
Provision (before `podman run`, so the bind-mount source exists)"), and a
macOS-side socket cannot cross the VM boundary as a bind-mount source. The
runner also asserts host facts a mac cannot satisfy: devenv.nix:202-204
("Prereqs: a Linux dev box with rootless podman and the uid-1000
subuid/subgid ranges configured"), and `go/cmd/compass-runner/main.go:89-97`
verifies podman ≥ 4.3 userns-remap support at startup. So the whole runner
process runs INSIDE the VM — per Matt's 2026-08-14 macOS ruling ("reuse the
existing Linux-native loop unchanged; the VM is the new moving part") — and
composes
with the runner's own posture: "Runners are remote by design, so this dials
the authenticated TLS door" (devenv.nix:302-304). Shape:

- VM engine: `podman machine` (recommended over colima; OQ1).
- `devenv up` on macOS does NOT provision or start the VM (mutating global
  machine state from `up` violates least surprise) — a macOS-guarded
  `dogfood:vm-runner` task checks `podman machine inspect` and fails fast
  with instructions when the VM is absent or stopped, then deploys + starts
  the runner in it.
- The runner binary is cross-compiled from the mac checkout
  (`GOOS=linux CGO_ENABLED=0 go build ./cmd/compass-runner` — the runner is
  pure Go; cgo appears only in the `-race` test lane, go/moon.yml:151-153
  `CGO_ENABLED: '1'`), copied into the VM over `podman machine ssh`, and
  launched with the enrollment token from `dogfood:mint-runner-token` and
  the gen-cert trust anchor.
- The VM dials the host's TLS network door (`ports.network.allocate =
  50052`, devenv.nix:265). But compass-server binds that door to LOOPBACK
  today: `--listen "127.0.0.1:${toString
  config.processes.compass-server.ports.network.value}"` (the
  compass-server exec's `--listen` line, devenv.nix). A podman-machine
  guest dialing "the VM→host gateway" reaches the host's gvproxy/vmnet
  interface, NOT 127.0.0.1 — whether gvproxy NATs the host-gateway address
  through to host loopback is an unverified implementation detail; if it
  does not, the runner loop is dead until the darwin `--listen` bind
  changes. DECIDED (Matt, 2026-08-14): rebind `--listen` on darwin to the
  vmnet/host-gateway-facing address — the narrowest address the VM can
  reach, never 0.0.0.0 — pending T5's opening spike to confirm the exact
  address and its discovery method (the spike is part of the decided plan,
  not an open fork). That rebind widens a laptop door beyond loopback — a
  stated consequence of the decision, accepted with the never-0.0.0.0
  narrowest-address constraint.
  Consequences T5 owns: the spike-resolved dial/bind address, and the
  gen-cert SAN set — "SAN defaults (127.0.0.1,::1,localhost)"
  (devenv.nix:349-350) — must grow that address, so `compass-gen-cert`
  gains a `--san` flag the macOS lane passes. The darwin `--listen`
  variance touches the compass-server process attr T4 unguards: T4 moves
  it out of the Linux guard unchanged, T5 may add darwin variance (e.g.
  `lib.optionalString`/a conditional flag — more intrusive than a plain
  guard move) — the two tasks co-edit this attr, and both their interfaces
  acknowledge it.
- Agent-image delivery into the VM (the `compass-agent:latest` ref the
  runner resolves, devenv.nix:333) is `podman machine`'s containers-storage;
  the nix image build (`dogfood:agent-image`) stays Linux-only, and macOS
  pulls the GHCR-published image per DL-112 (DECISIONS.md:211) once that
  publish lane exists — until then macOS runs runner-loop-less by default
  (server+UI still fully usable).

**A2c — the native desktop shell darwin build path.** None exists today.
Grounding:

- `go/cmd/compass-app/main.go:1` — `//go:build unix && gtk3` — the real
  Wails v3 shell, GTK3/WebKitGTK-linked.
- `go/cmd/compass-app/main_nogtk3.go:1` — `//go:build unix && !gtk3` — and
  `:25-27`:

  ```go
  slog.Error("compass-app was built without the desktop shell; rebuild with -tags gtk3 " +
      "(the shell requires the GTK3 + WebKit2GTK 4.1 stack)")
  os.Exit(1)
  ```

- `go/moon.yml:141` — the module build task is `command: 'go build ./...'`
  with no tags — so the gate compiles the stub, and NO moon task builds the
  real shell (main_nogtk3.go:14 mentions a "go/moon.yml build lane +
  the dev-box link" for the gtk3 binary; no `-tags gtk3` task exists in
  go/moon.yml — the dev-box link is manual today).

Because `darwin` satisfies Go's `unix` constraint, the existing tags are a
trap on macOS: `-tags gtk3` on darwin selects main.go and fails at the GTK
cgo link; the default build selects the exit-1 stub.

The surgery is bigger than main.go alone. main.go's `run()` is welded to
symbols in three MORE gtk3-gated files: it constructs
`embeddedPipeline{realPreflight, runStackUp, whoAmIOverUDS}` and calls
`launchByMode` (main.go:91-99), and wires `quitController{runStackDown, ...}`
(main.go:126-131); those symbols live in `embedded.go`, `lifecycle.go`, and
`preflight_adapters.go` — all `//go:build unix && gtk3` (embedded.go:1,
lifecycle.go:1, preflight_adapters.go:1) — and are semantically Linux-bound:
`realPreflight` probes host podman, and the embedded pipeline spawns
`compass-stack`; both are meaningless on a mac where podman lives in a VM
(A2b). So the darwin build realistically ships **client-mode-only
initially; embedded mode on darwin is deferred** until the VM runner-loop
lane (A2b) can carry it. This is an initial-posture note, not a product
decision: DL-109's mode default ("Defaults to $COMPASS_APP_MODE, then
app.toml, then embedded", main.go:57-59) governs the shipped binary's
behavior with no explicit mode; darwin having no embedded mode yet is a
platform-capability gap, reversible once the VM lane lands, not a change to
the product default (Matt ruled 2026-08-14: not product-visible). T6's
interface commits the build contract: mode resolution on darwin must not fall
through to the embedded default.

The darwin lane is build-tag surgery plus a new entrypoint (T6):

- `main.go` → `//go:build linux && gtk3` (it links GTK3/WebKit2GTK 4.1,
  Linux-only in this toolchain per its own doc comment,
  main_nogtk3.go:5-7). The welded trio (`embedded.go`, `lifecycle.go`,
  `preflight_adapters.go`) is retagged `linux && gtk3` alongside it —
  same gate, stated explicitly rather than left implicit behind `unix`.
- `main_nogtk3.go` → the stub covers non-darwin unix without gtk3 AND
  darwin without the shell tag:
  `//go:build (unix && !gtk3 && !darwin) || (darwin && !shell)`.
- New `main_darwin.go` behind an OPT-IN tag: `//go:build darwin && shell`.
  It wires the same Wails v3 shell as main.go (same `appconfig` + `bridge`
  imports, main.go:30-32), client-mode-only initially per the posture
  above. [INFERENCE] Wails v3 selects Cocoa/WKWebView on darwin natively
  (no pkg-config, system frameworks only, devenv.nix:122-123) — this is a
  T6 spike-to-verify before the entrypoint is written, not a settled fact.
  Composes DL-106/DL-109/DL-110 (DECISIONS.md:205,208,209) unchanged: same
  one-binary/two-modes contract, same app.toml mode selection, same Wails
  v3 framework. The shared shell body (window setup, asset serving, bridge
  pump) is extracted into an untagged `shell.go` in the same package so
  main.go and main_darwin.go are thin platform mains, not a fork of 180
  lines.
- The opt-in `shell` tag is recommended because it preserves the "module
  gate never compiles the native app" property (devenv.nix:125-126) on
  macOS too: the default darwin `go build ./...` keeps compiling the cheap
  stub, and only `compass-go:app` builds the real Cocoa/cgo shell (which
  needs Xcode CLT) — symmetric with Linux's gtk3 opt-in, and a smaller,
  more consistent change than making darwin the one platform whose default
  build is the heavy native shell.
- Windows is a non-regression: every compass-app file is unix-gated today,
  so `go build ./...` already excludes windows; the retag preserves that.
- A new moon task `compass-go:app` builds the real shell on both platforms
  (`-tags gtk3` on Linux; `-tags shell` on darwin). `runInCI: false` —
  compass CI does not compile the native app (devenv.nix:125-126: "it is a
  dev-box build input, and compass CI does not compile the native app").

### A3 — the pre-push gate hazard: document + interim workaround (fix is upstream)

Re-scoped by Matt (2026-08-14): the durable fix lands in **zireael** (jj-hp,
the jj-hooks harness) — it will propagate the repo's devenv env into the temp
worktree so `moon ci` resolves the devenv pins in every repo, one fix
fleet-wide. Designing a compass-side `devenv shell -- moon ci` wrapper here
would be a competing, redundant fix in the wrong repo. This record's lane is
documentation only:

- **The hazard, stated where it bites:** extend the `hk.pkl` header comment
  (hk.pkl:1-17, which already documents the bypass knobs) with the
  temp-worktree drift hazard: the hook runs in `/tmp/jj-hooks-worktree-*`
  with no direnv activation, so tools resolve from the system PATH; a system
  biome ≠ 2.5.4 (biome.json:2) produces false reds (never `--write` from the
  worktree — 2.4.16 output breaks main CI, which runs 2.5.4).
- **The interim workaround (until the jj-hp fix ships):** verify in the
  devenv shell of the source checkout (`moon ci` there runs the pinned
  2.5.4), then push with hooks skipped (`HK=0 git push` / `--no-verify`,
  already documented at hk.pkl:15-17; via jj-vine, its no-hooks path), and
  confirm GitHub CI green at source — CI installs pinned toolchains itself
  and never had the bug (devenv.nix:103-107).
- **The pointer:** the durable fix is tracked in the zireael lane
  (jj-hp env propagation); compass carries no gate-invocation change.

A compass-side fail-closed version assertion (a preamble asserting
`biome --version` == the biome.json pin before `moon ci`) is optional
belt-and-braces, not load-bearing once jj-hp propagates the env — parked in
OQ4, not a task.

## Global Constraints

- **GC-1 — RIG-1983 sequencing (drop-proto).** RIG-1983's design froze
  (#300, squash bab90139) and its implementation is in flight (T1 = PR #308
  nix pin files + toolchain-tools derivations; T2+T4 devenv cutover +
  toolchain-parity rework, atomic; T5 ci.yml two-phase). It reworks exactly
  the devenv.nix surfaces this record edits: proto/.prototools removal, pins
  moving into nix derivations, the `enterShell` proto activation
  (devenv.nix:155-161), and the exec preambles' `PROTO_HOME` shims
  (devenv.nix:243-244). This record grounds against CURRENT main (still
  .prototools: bun 1.3.13, node 24.18.0, moon 2.4.2, go 1.26.5 —
  .prototools:6-13) and does NOT guess the post-cutover shape.
  **Implementation of T3/T4/T5 MUST be sequenced after the RIG-1983 cutover
  lands and rebased onto it** — the toolchain block, the Linux-only guards,
  and the exec preambles may all move.
  All devenv.nix line-number citations in this record (:218, :243-244,
  :253, :260, :265, etc.) DIE at the RIG-1983 rebase — the implementing
  agent MUST re-ground T3/T4/T5 against the post-cutover file, never
  pattern-match stale line numbers.
- **Tool pins (as of this record):** bun 1.3.13 / node 24.18.0 / moon 2.4.2 /
  go 1.26.5 (.prototools:6-13); biome 2.5.4 (biome.json:2, bun.lock:237);
  vite port 5173 strictPort (apps/ui/vite.config.ts:11); dev-http door 50051,
  TLS network door 50052 (devenv.nix:260,265).
- **Never-heavy-on-up:** nothing added to `devenv up` may pull a heavy
  closure (the precedent: `dogfood:agent-image` is deliberately opt-in,
  devenv.nix:397-400).
- **Port ownership:** devenv is the single owner of allocated port numbers
  (`config.processes.compass-server.ports.*`); no second hardcoded copy —
  consumers take them via process env.
- **Platform guards:** macOS relaxation is per-attr, never a deleted guard
  wholesale; anything podman-native or GTK-linked stays
  `lib.optionalAttrs pkgs.stdenv.isLinux`.
- **Frozen decisions composed, not reopened:** DL-106/DL-109/DL-110/DL-111/
  DL-112/DL-183 (native app), DL-025/026/027/078 (container/secrets/config).
  The deployed `main`/`preview` env model is owned by the private
  infrastructure design repo; this record is the developer-box lane only.
- **No CI scope:** compass CI does not compile the native app; the new
  `compass-go:app` task is `runInCI: false`.

## Plan

Order: T1/T3 (UI-in-up — the compass-critical path), T2 (the direct-dial
streaming smoke), then T4/T5/T6 (macOS), T7 (gate-hazard docs, small, can
land any time). T3–T5 rebase onto RIG-1983 per GC-1.

### T1 — Wire the UI's base URL to the dev-http door  (owner: compass-repo)

Direct-dial per §A1 (decided): a one-line env wire —
`VITE_COMPASS_BASE_URL = "http://127.0.0.1:${toString
config.processes.compass-server.ports.devhttp.value}"` injected on the
`compass-ui` process (lands with T3's process definition). The dev door's
  wildcard CORS (`go/server/serve.go:689-696`, `AllowedOrigins:["*"]` at
  `:691`; served at `serve.go:604`) already admits the vite origin;
no vite.config.ts proxy — the only vite.config.ts edit is updating the
stale header comment (vite.config.ts:4-6: the transport route now exists).
(The §A1 proxy shape stays documented as the prod-parity alternative; T1
carries no proxy work.)

- **Interfaces:** consumes `config.processes.compass-server.ports.devhttp.value`
  (int, 50051); produces the env value
  `VITE_COMPASS_BASE_URL=http://127.0.0.1:50051` on the compass-ui process.
- **Test cycle:** with `devenv up` running server-only, start `moon run
  compass-ui:dev` with `VITE_COMPASS_BASE_URL=http://127.0.0.1:50051`;
  verify `curl -X POST
  http://127.0.0.1:50051/compass.v1.CompassService/GetServerInfo
  -H 'Content-Type: application/json' -d '{}'` answers (the same probe the
  readiness check uses, devenv.nix:288-294), and a browser session loads
  the board and holds a live `SubscribeEvents` stream (the browser dials
  the door directly; no middlebox in the path).

  T1 is independently verifiable before T3's `devenv up` process exists: its
  deliverable is the validated env value proven via the standalone `moon run
  compass-ui:dev` task (`apps/ui/moon.yml:11-12`, exists today); T3 is that
  value's placement into the `devenv up` process definition.

### T2 — UI streaming e2e smoke against the dev door  (owner: compass-repo)

Direct-dial (decided) drops the proxy-streaming leg this task originally
existed to verify — there is no middlebox to prove unbuffered. What remains
is the direct-dial arm's streaming smoke: a check that a server-streaming
RPC against the direct dev-door delivers frames incrementally
(a `SubscribeEvents` subscription observing an event created after
subscribe).

- **Interfaces:** consumes T1's env wire + the running `devenv up` stack;
  produces a repeatable check named in the PR (command + expected output).
- **Test cycle:** the check itself, red (compass-server stopped or the
  event never created) → green (running stack, frames arrive
  incrementally).

**Resolved (RIG-2001) — OQ5 decided: a documented manual smoke, not a bun
test.** The hermetic-automated arm cannot faithfully cover this task's
subject. A `SubscribeEvents` check against the *browser's own* direct-dial
transport (`createCompassWebTransport` → gRPC-Web over real `fetch`,
`apps/ui/src/live/client.ts:43-46`) needs a real HTTP gRPC-Web server fake;
`apps/ui`'s only in-fence fake is `createRouterTransport` — the vendor's
*no-HTTP* in-memory path (`packages/compass-client/src/index.ts:83-88`),
which `apps/ui/src/live/events.test.ts` already uses to cover the *driver's*
incremental apply, and which never exercises the HTTP transport this task is
about. A real-HTTP fake would need `@connectrpc/connect-node`, which the
`biome.json` `noRestrictedImports` fence blocks in `apps/ui` (`biome.json:17`,
"reach the daemon only through @compass/client"), and hand-framing gRPC-Web
over `createCompassClientOverFetch` would mostly exercise the vendor decoder,
not compass code. The full-stack automated e2e harness is Record B's lane,
not this record's (OQ5). So per OQ5's stated rule the check is a documented
manual smoke.

**The check** — run inside the devenv shell with `devenv up` (or at least
`compass-server`) running; the dev-http door is devenv-owned on
`127.0.0.1:50051`:

```bash
# GREEN: the door streams incrementally and holds the tail open.
timeout 5 buf curl --http2-prior-knowledge --protocol grpcweb --schema proto \
  -d '{"sinceSeq":"0"}' \
  http://127.0.0.1:50051/compass.v1.CompassService/SubscribeEvents
```

Expected GREEN: the snapshot boundary frame (an `instanceEpoch`, no `seq`),
then the positioned `serverStatus: SERVER_STATE_READY` frame (`seq: "1"`),
after which the stream stays open and idle until `timeout` ends it — the
frames arrive as separate reads, not one buffered batch at close, which is
the unbuffered incremental delivery direct-dial exists to prove.
`--protocol grpcweb` is the browser's own transport (the app builds a
gRPC-Web transport, `client.ts:44`); `h2c` because the dev door serves
cleartext HTTP/2 (`go/server/serve.go:604,704-707`).

Expected RED: with the door down (`compass-server` stopped, or the wrong
port) the same command returns immediately with `code: unavailable` /
`connection refused` — no frames, no held-open tail.

**Event-created-after-subscribe (the fuller form).** The dev door exposes no
client-triggerable board mutation (board transitions route
Server→RunnerHub→Runner and are agent-driven), so the strongest form needs a
runner: with the stream above still open, `SpawnAgent` (or the seeded
root-supervisor coming online) fans an `AgentSessionStatus` frame onto the
already-open subscription — a positioned frame arriving *after* subscribe,
seen as a new read on the same held-open stream. The browser end-to-end
confirmation is the same property through the real app: `moon run
compass-ui:dev` (or the T3 `devenv up` `compass-ui` process), load
`http://127.0.0.1:5173`, and the board renders from the snapshot burst then
live-updates as the stream tails — the browser dialing the dev door directly,
no middlebox in the path.

### T3 — `compass-ui` process in `devenv up`  (owner: compass-repo)

Add the `compass-ui` process per §A1 inside the existing Linux guard: `exec
bunx vite` from `cwd = ${config.devenv.root}/apps/ui`, env
`VITE_COMPASS_BASE_URL=http://127.0.0.1:${ports.devhttp.value}` (direct-dial,
decided), `after =
["devenv:processes:compass-server"]`. Update the `devenv up` chain comment
(devenv.nix:174-196) to include the UI. **Rebase onto RIG-1983's cutover
(GC-1): the exec preamble shape follows whatever toolchain activation the
cutover leaves.**

- **Interfaces:** consumes `config.processes.compass-server.ports.devhttp.value`
  (int, 50051) and the compass-server readiness probe; produces process
  `devenv:processes:compass-ui` serving `http://127.0.0.1:5173`, whose
  browser client dials the dev door at `http://127.0.0.1:50051` directly.
- **Test cycle:** `devenv up`; wait for compass-ui; the T1 curl probe
  answers and a browser load of `http://127.0.0.1:5173` reaches the board;
  kill compass-server and verify the UI process survives (no spurious
  dependency beyond start ordering).

### T4 — macOS native services: relax the guards  (owner: platform)

Per §A2a: unguard `services.postgres`, `compass-server` (+ `compass-ui`),
`dogfood:gen-cert`, `dogfood:mint-runner-token`; keep `compass-runner`,
`dogfood:agent-image`, `dogfood:clean`, and `PKG_CONFIG_PATH` Linux-only.
Restructure `processes`/`tasks` as base-set `//` `lib.optionalAttrs
pkgs.stdenv.isLinux { ... }` (the devenv.nix:89/:127 env pattern). T4 moves
the compass-server exec (including its `--listen` line) out of the guard
UNCHANGED; T5 then adds the decided darwin variance to that same attr
(§A2b's darwin `--listen` rebind) — the two tasks co-edit it. **Rebase
onto RIG-1983 (GC-1).**

- **Interfaces:** consumes the current guard structure (devenv.nix:211,
  :218, :346); produces `devenv up` on darwin = postgres, gen-cert, server,
  mint, and UI; on Linux unchanged (byte-identical process set). Co-edits
  the compass-server `--listen` process attr with T5 (T4 unguards it
  unchanged; T5 adds the darwin bind variance).
- **Test cycle:** on Linux, `devenv up` process set is unchanged (compare
  `devenv processes list` before/after); on a mac, `devenv up` reaches
  compass-ui ready and the T1 probe answers.

### T5 — macOS runner loop in the Linux VM  (owner: compass-runner)

Per §A2b: a darwin-guarded `dogfood:vm-runner` task that (a) asserts a
running `podman machine` (fail-fast with setup instructions), (b)
cross-compiles `GOOS=linux CGO_ENABLED=0 go build -o
${config.devenv.state}/compass/compass-runner-linux ./cmd/compass-runner`,
(c) copies binary + `tls.crt` + `runner.token` into the VM, (d) launches
the runner in the VM dialing the host's network door. Plus:
`compass-gen-cert` gains `--san` (append to the default SAN set), and the
macOS lane passes the spike-resolved VM-reachable host address. The darwin
`--listen` rebind is DECIDED (§A2b): rebind to the vmnet/host-gateway-facing
address — the narrowest address the VM can reach, never 0.0.0.0. T5 OPENS
with a spike confirming the exact address and its discovery method (part of
the decided plan, not an open fork). The rebind
co-edits the compass-server process attr T4 unguards (darwin variance via
`lib.optionalString`/a conditional flag — more intrusive than T4's plain
guard move; both tasks' interfaces acknowledge the co-edit).

- **Interfaces:** consumes `podman machine ssh/inspect`, the token file
  `${config.devenv.state}/compass/runner.token` (devenv.nix:328,383), the
  trust anchor `tls.crt` (devenv.nix:332), and the network door on port
  50052 at the spike-resolved VM-reachable host address (the exact address
  and its discovery method are the opening spike's outputs, not assumed
  here); produces a new `compass-gen-cert` flag `--san` (string,
  comma-separated, additive to `127.0.0.1,::1,localhost`), the
  `dogfood:vm-runner` task, and the darwin
  variance on compass-server's `--listen` attr (co-edited with T4).
- **Test cycle:** on a mac with a running podman machine: `devenv up`, run
  `dogfood:vm-runner`, verify the runner enrolls (server logs the `dogfood`
  runner) and a `ProvisionAgentWorkspace` round-trip creates a container in
  the VM (`podman machine ssh podman ps` shows `compass-agent-*`). Negative:
  with the VM stopped, the task fails with the instruction message, not a
  hang.

### T6 — Darwin native-shell entrypoint + `compass-go:app` build task  (owner: compass-app)

Per §A2c: retag `main.go` to `linux && gtk3`, and the welded trio
`embedded.go` + `lifecycle.go` + `preflight_adapters.go` (all `unix &&
gtk3` today; main.go's `run()` constructs their symbols, main.go:91-99 and
:126-131) to `linux && gtk3` alongside it — darwin ships CLIENT-MODE-ONLY
initially, embedded deferred to the A2b VM lane; retag `main_nogtk3.go` to
`(unix && !gtk3 && !darwin) || (darwin && !shell)` so the stub covers the
default darwin build; extract the shared shell body into an untagged
`shell.go`; add `main_darwin.go` (`//go:build darwin && shell`, the opt-in
tag) wiring the same appconfig/bridge shell over Wails' Cocoa/WKWebView
backend ([INFERENCE] Wails v3 selecting Cocoa natively on darwin is a
spike-to-verify before the entrypoint is written); add moon task
`compass-go:app` (`runInCI: false`) building `-tags gtk3` on Linux and
`-tags shell` on darwin. DL-109 graze flagged in §A2c: darwin's embedded
default would be broken-by-default, so the darwin main must not fall
through to it.

- **Interfaces:** consumes `go/internal/appconfig` + `go/internal/bridge`
  (main.go:30-31) unchanged; produces `go/cmd/compass-app/main_darwin.go`
  (client-mode-only posture — mode resolution on darwin must not fall
  through to the embedded default, the DL-109 graze), the retagged
  entrypoints (main.go, main_nogtk3.go, embedded.go, lifecycle.go,
  preflight_adapters.go), the untagged shell.go, and task `compass-go:app`
  → binary `compass-app` for the host platform. `go build ./...`
  (go/moon.yml:141) still compiles the cheap stub on every platform (linux
  default AND darwin default); windows stays excluded — all compass-app
  files are unix-gated today, and the retag preserves that non-regression.
- **Test cycle:** on Linux, `go build ./...` and `go build -tags gtk3
  ./cmd/compass-app` both compile and the gtk3 binary still launches; on a
  mac, `go build ./...` compiles the stub, and `moon run compass-go:app`
  produces a binary that opens the window against a hand-started stack
  (the main.go:10-12 posture: "the window points at a daemon a developer
  starts by hand"). Existing Go gate battery (`compass-go:ci`) green.

### T7 — Gate-hazard documentation  (owner: compass-repo; small, independent)

Per §A3: extend the `hk.pkl` header comment (hk.pkl:1-17) with the
temp-worktree drift hazard, the interim workaround (verify inside the devenv
shell → push with hooks skipped → confirm GitHub CI green), the
never-`--write`-from-the-worktree warning, and the pointer that the durable
fix (devenv env propagation into the hook worktree) is tracked in the
zireael/jj-hp lane. No behavioral change in this repo.

- **Interfaces:** consumes hk.pkl:1-36 (comment-only edit; the `check =
  "moon ci"` step is untouched); produces the updated comment block.
- **Test cycle:** `hk run pre-push` still loads the config (comment-only
  change, hk parses it); markdownlint/biome-clean.

## Tasks

- [ ] T1 — wire `VITE_COMPASS_BASE_URL` to the dev-http door (direct-dial,
      decided) (compass-repo)
- [ ] T2 — streaming e2e smoke against the direct dev-door (compass-repo;
      documented manual smoke per OQ5, resolved)
- [ ] T3 — `compass-ui` process in `devenv up`, after compass-server ready
      (compass-repo; rebase on RIG-1983)
- [ ] T4 — relax Linux guards: postgres/server/UI/gen-cert/mint
      cross-platform; runner/agent-image/clean/PKG_CONFIG_PATH stay Linux
      (platform; rebase on RIG-1983; co-edits `--listen` attr with T5)
- [ ] T5 — darwin `--listen` bind spike + `dogfood:vm-runner` (podman
      machine) + `compass-gen-cert --san` (compass-runner)
- [ ] T6 — darwin Wails entrypoint (`darwin && shell` opt-in) + welded-trio
      retag + shared `shell.go` + `compass-go:app` moon task (compass-app)
- [ ] T7 — hk.pkl hazard/workaround/zireael-pointer comment (compass-repo)

## Alternatives considered

The two losing arms are treated inline where the decision is made; collected
here for the canonical audit surface.

- **Vite dev-proxy instead of direct-dial** (losing arm of the browser→dev-door
  route, §A1 `:78-136`). Weighed for prod-parity (the browser stays
  same-origin, mirroring a reverse-proxied production deploy) against a
  middlebox in the streaming path (a vite proxy buffering/!flushing a long-
  lived `SubscribeEvents` stream is a real failure class). Rejected for local
  dev: the dev door already serves permissive wildcard CORS built for exactly
  this consumer (`serve.go:689-696`), so direct-dial has no CORS cost, and
  removing the middlebox removes the streaming-buffering risk. The proxy shape
  stays documented in §A1 as the prod-parity reference.
- **colima instead of podman machine** for the macOS runner VM (losing arm of
  OQ1). Rejected: the runner execs `podman` directly, and podman machine is
  the first-party path with matching containers-storage semantics; colima adds
  a docker-compat layer this loop does not need. Full treatment in OQ1.

## Open Questions

This section holds ONLY non-load-bearing deferrals — no live load-bearing
question remains; the ruled forks (browser→dev-door route: direct-dial, the
former OQ2; darwin `--listen` rebind) are folded into the body as decisions
(Matt, 2026-08-14). The numbering keeps the original OQ ids for traceability,
so OQ2 is intentionally absent — it was promoted to a decision, not dropped.

- **OQ1 (non-load-bearing) — macOS VM engine: podman machine vs colima.**
  Recommendation: `podman machine` — the runner execs `podman` directly
  (go/internal/runner/host.go:152 "before `podman run`") and podman machine
  is the first-party path with matching containers-storage semantics; colima
  adds a docker-compat layer we don't need. Costs: podman machine's default
  VM needs the uid-1000 subuid/subgid + userns prereqs configured in the VM
  image (devenv.nix:202-204), which T5 must script or document either way.
- **OQ3 (non-load-bearing) — macOS agent image: GHCR pull (DL-112) vs
  building in the VM.** Recommendation: GHCR pull per DL-112
  (DECISIONS.md:211) once the publish
  lane exists; until then macOS runs without the runner loop by default
  (server+UI fully usable). Building the nix image inside the VM is possible
  but violates never-heavy and duplicates the publish lane.
- **OQ4 (non-load-bearing) — optional compass-side version assertion in the
  gate.** A fail-closed preamble asserting `biome --version` matches
  biome.json:2 before `moon ci` would convert any future PATH-drift into an
  explicit "wrong toolchain" red. Recommendation: SKIP — once jj-hp
  propagates the
  devenv env the assertion is dead weight, and landing it now creates a
  compass-side change the harness fix immediately obsoletes. Revisit only if
  the zireael fix slips a month+.
- **OQ5 (non-load-bearing) — T2 automation depth. RESOLVED (RIG-2001):
  documented manual smoke, not a bun test** — the hermetic arm cannot cover
  the browser's real gRPC-Web-over-HTTP transport in-fence
  (`@connectrpc/connect-node` is fenced in `apps/ui`; the only in-fence fake,
  `createRouterTransport`, is no-HTTP and already covers the driver in
  `events.test.ts`), and full-stack e2e is Record B's lane. The check plus its
  verified green/red output live in T2 above. (The original recommendation was
  conditional — automate only if hermetic against a scripted fake; the hermetic
  arm proved infeasible in-fence, so the decision is the manual smoke.)

Ledger-impact: none — this record composes frozen decisions (DL-106/109/110/
111/112/183, DL-025/026/027/078) and makes only operational/local-dev
choices; no new product decision. (The ledger gate scopes product records:
`tools/design-ledger-gate/index.ts:45` `PRODUCT_DIR = "docs/designs/product"`;
platform records are out of its record set per its own tests,
index.test.ts:124-126.)
