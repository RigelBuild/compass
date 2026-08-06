# Compass dogfood loop

> **Design record.** The full `devenv up` dogfood loop for Compass: build + load
> the `compass-agent` image, bring up postgres + a TLS-door `compass-server`,
> mint a runner token, enroll `compass-runner`, and drive one real agent session
> end to end. The design targets the **`sealedsecurity/compass`** repo (its
> `devenv.nix`, `agent-image/`, and `go/cmd/*` binaries); every `devenv.nix:*`,
> `agent-image/*`, and `go/cmd/*` citation below is a path in that repo at HEAD
> `21241f720`, not this one. It lives in the sealed design corpus because that is
> where the wave's design records freeze.

Status: **decided** — SEA-1360. Every fork (D1–D5) ruled by Matt; the record is
the frozen contract the implementation reads. Design-first per AGENTS.md.

## Problem / Intent

`devenv up` today brings up only the human-facing half of Compass: postgres plus
a `compass-server` serving the Unix socket and the loopback dev-http door
(`devenv.nix:164-167`: `--socket "$COMPASS_SOCKET" --dev-http 127.0.0.1:… --database "$COMPASS_DATABASE_DSN"`).
The Runner half — the TLS network door, a runner enrollment, the agent image,
and a real per-agent container running an agent turn — is never exercised
locally. Matt ruled a FULL dogfood loop, designed first: on `devenv up`, the
`compass-agent` OCI image is built and loaded into rootless-podman
containers-storage, postgres + compass-server come up with the TLS network door
open, a runner token is minted, `compass-runner` enrolls (trusting the
self-signed cert) and idles, and a real session is driven end to end
(`CommsService.CreateAgent` → `CompassService.ProvisionAgentWorkspace` →
`StartAgentSession`), so a real container spawns and runs an agent turn.

Note on ledgers: this record lives in the sealed design corpus
(`docs/designs/platform/`), which the design-ledger-gate governs only for the
**product** corpus (`docs/designs/product/DECISIONS.md`). A platform record adds
no DECISIONS row and declares no ledger delta, so nothing here is ledger-tracked;
the `compass` repo it targets has no design-ledger tooling of its own.

## Global Constraints

- **Linux-only.** Every dogfood process/service/task lives under the existing
  guards: `services.postgres = lib.optionalAttrs pkgs.stdenv.isLinux`
  (`devenv.nix:124`), `processes = lib.optionalAttrs pkgs.stdenv.isLinux`
  (`devenv.nix:131`), and the agent image itself is
  `containers = lib.optionalAttrs pkgs.stdenv.isLinux` (`agent-image/devenv.nix:65`).
- **Rootless podman + uid 1000.** The runner refuses any other uid:
  `verifyRunnerUID` enforces `defaultAgentUID uint32 = 1000`
  (`go/cmd/compass-runner/main.go:148`, `:79-81` "Ahead of every operator-input
  check"). Containers launch with plain `--userns=keep-id`
  (`agent-image/devenv.nix:73-74` citing `internal/runtime/podman.go:357`).
  Prereqs on the dev box: rootless podman with subuid/subgid configured
  (verified on this box: podman 5.8.4, rootless, uid 1000).
- **One shared self-signed cert.** The gen-cert output "is both the Server's
  `--tls-cert` and the Runner's `--ca` trust anchor"
  (`go/cmd/compass-gen-cert/main.go:50-51`); no second keypair, no relaxed
  loopback TLS.
- **Idempotent tasks (F2, DECIDED).** gen-cert skips when both files exist
  (`main.go:85-89` `shouldSkipGen`); mint with `--token-out` is
  skip-if-present/heal-without-rotate (`compass-mint-runner-token/main.go:53-59`).
  Re-running `devenv up` mints/generates nothing new.
- **Ordering (F3, DECIDED).** gen-cert → postgres → compass-server(ready) →
  mint-runner-token → compass-runner. Server readiness stays the existing
  GetServerInfo probe (`devenv.nix:191-197`), which flips only after the store
  is migrated.
- **Session drive is opt-in (D5).** The image build and the live session
  drive land as opt-in tasks, not wired into `devenv up`; `up` itself stays
  light.
- **Lane split.** compass-repo lane owns all `devenv.nix` wiring; the
  compass-server lane owns the net-new admin session driver (D3), with a
  scoped compass-runner mount flag (T5a). Tasks below are tagged so the wiring
  ships independently of the driver.
- **Commits** authored as Matt with the seal co-author trailer
  (rule://commit-conventions); the spawning agent ships the PR.

## Approach

Extend the existing `devenv up` bring-up (postgres + compass-server,
`devenv.nix:115-200`) with the Runner half, composed from binaries that already
exist for exactly this purpose — no new server code is needed for enrollment;
the only net-new code is the admin session driver (compass-server lane, D3).

### Process/task graph

```text
tasks:      dogfood:gen-cert ──────────┐
processes:  postgres ── compass-server(+TLS door, ready=GetServerInfo)
                                        │
tasks:      dogfood:mint-runner-token ◄─┘ (after server ready)
processes:  compass-runner (enrolls, idles) ◄─ token file + cert
opt-in:     dogfood:agent-image (build+load), dogfood:session (drive a turn)
```

All new units go under the existing Linux guards next to `compass-server`
(`devenv.nix:131`). Ordering uses devenv's task/process dependency names —
`after = [ "devenv:processes:<name>" ]` with `@ready` the default for
processes and `@succeeded` for tasks (`forks/devenv/src/modules/processes.nix:186-191`:
"Use task names like \"devenv:processes:postgres\" or \"myapp:setup\"").

### 1. gen-cert (idempotent task)

A `tasks."dogfood:gen-cert"` runs
`compass-gen-cert --cert-out $STATE/compass/tls.crt --key-out $STATE/compass/tls.key`.
Defaults suffice: SANs default to `127.0.0.1,::1,localhost`
(`go/cmd/compass-gen-cert/main.go:38` `const defaultHosts = "127.0.0.1,::1,localhost"`),
and the run is skip-if-present — "when both files already exist, leave them be
so a restart does not swap the cert out from under a live Server/Runner pair"
(`main.go:82-84`). Cert is written 0644 (public trust anchor), key 0600
(`main.go:49-53`). The binary is built the same way the server process builds
its binary today (`devenv.nix:158-163` go build into `$DEVENV_STATE/compass/`).

### 2. compass-server grows the TLS network door

Add `--listen 127.0.0.1:<port> --tls-cert $STATE/compass/tls.crt --tls-key
$STATE/compass/tls.key` alongside the existing `--socket`/`--dev-http` argv
(`devenv.nix:164-167`), with a `ports.network.allocate` port. The three flags
are all-or-none (`go/cmd/compass-server/main.go:146-154`: "Either all three are
set (the authenticated TCP door is enabled) or none are"). Consequences, all
existing behavior:

- The network door mounts CompassService + CommsService behind bearer + admin
  interceptors, "plus the internal RunnerService door a Runner enrolls over"
  (`go/server/serve.go:269-273`); the RunnerService door exists ONLY here —
  "Runners are remote, so they dial the authenticated TLS door, never the
  loopback socket" (`serve.go:199-201`).
- The bootstrap-admin token is minted and written 0600 to
  `<StateDir>/admin-token` on network-door startup (`go/server/network_door.go:35-38`
  `adminTokenFile = "admin-token"`; `serve.go:269-271` "It mints and writes the
  bootstrap token 0600 under the state dir"). StateDir defaults to the socket's
  parent dir (`network_door.go:249-256`), i.e. `$DEVENV_STATE/compass/` given
  `COMPASS_SOCKET = "${config.devenv.state}/compass/server.sock"`
  (`devenv.nix:174`). The driver reads this file for its bearer credential.
  The write is SERIAL before any door serves: `Serve` runs bindListeners →
  `store.Open` (migrate) → `BootstrapAdmin` → `buildNetworkServer` (which
  synchronously mints + writes the token) and only then starts the serve
  goroutines in one errgroup (`serve.go:106-115,168-186,276-306`;
  `network_door.go:249-258`) — so GetServerInfo cannot answer before the token
  exists, and there is no race for the driver. T7's "admin-token exists 0600 at
  server-ready" smoke is the regression guard for this implicit ordering.
- Migrations still run at store open (`go/internal/store/store.go:55-57`
  "applies any pending embedded migrations under an advisory lock"), and the
  existing readiness probe (GetServerInfo over dev-http, `devenv.nix:184-197`)
  already gates on post-migration serving — mint can safely depend on it.

### 3. mint-runner-token (idempotent task, after server ready)

`tasks."dogfood:mint-runner-token"` runs
`compass-mint-runner-token --runner-id dogfood --token-out $STATE/compass/runner.token`
with `COMPASS_DATABASE_DSN` set to the same DSN the server uses
(`go/cmd/compass-mint-runner-token/main.go:100-101`: "resolveDSN mirrors
compass-server's precedence exactly"). It must run after the server is ready
because `store.Open` verifies the migrated schema (`main.go:88`,
`store.go:55-59` "refusing to serve on a failed migration or a version
mismatch") — ordering: `after = [ "devenv:processes:compass-server" ]`
(default `@ready`). With `--token-out` the mint is idempotent: "if the file
exists and its token is already registered in the store, it no-ops; if the
file exists but the store no longer knows the token (e.g. the database was
replaced), it re-registers that same token without rotating it"
(`main.go:53-58`). File is 0600, written atomically, raw token no newline
(`main.go:192-196`).

### 4. compass-runner (process; enrolls and idles)

A `processes.compass-runner` execs:

```text
compass-runner --runner-id dogfood \
  --server https://127.0.0.1:<network-port> \
  --ca $STATE/compass/tls.crt \
  --image compass-agent:latest \
  --runtime-dir $STATE/compass/runner
COMPASS_RUNNER_TOKEN=$(cat $STATE/compass/runner.token)   # env only, never a flag
```

Grounding: `--runner-id`/`--server`/`--image`/`--ca`/`--runtime-dir` flags at
`go/cmd/compass-runner/main.go:40-57`; the token is "env only, never a flag (a
flag leaks into the process table)" (`main.go:91-93`); `--ca` "swaps the
system root pool for a single trusted CA — the local dogfood path, where the
Server's self-signed 127.0.0.1 cert is the trust anchor" (`main.go:106-108`).
The image ref includes the tag: the runner resolves it out of
containers-storage with no pull (`agent-image/devenv.nix:93-97`), and the
runtime documents the ref shape as "e.g. compass-agent:latest"
(`go/internal/runtime/podman.go:71-72`). `--runtime-dir` overrides the
`/run/compass` default (`main.go:56-57`) — root-only to create — to a
state-dir path owned by uid 1000: the per-container agent socket is created at
`RuntimeDir/containers/<container>/agent.sock` (`main.go:56-57` flag doc), so
the dir must be writable by the runner's uid or socket creation EACCESes.
On start the runner enrolls then idles: `Dial` performs the `Enroll` RPC with
the bearer interceptor (`go/internal/runner/runner.go:101-113`), then
`link.RunSessions` "blocks until the stream ends" awaiting Provision/Start
commands (`go/internal/runner/run.go:96-121`). Ordering:
`after = [ "dogfood:mint-runner-token" ]` (a task dep, default `@succeeded`).
The uid-1000 guard runs before any flag validation (`main.go:75-81`), so a
wrong-uid box fails fast with a named cause.

Egress: `--egress-allow` stays EMPTY (pure default-deny) for the base loop —
the `local_path` file:// clone needs no network and no credentials
(`go/internal/runtime/workspace.go:109-110`). Note the consequence for a real
turn: "An empty host set is pure default-deny (only loopback, established
flows, and DNS to the container's own resolver)"
(`go/internal/runtime/egress.go:29-31`), enforced by nftables armed at launch
(`go/internal/runtime/agent.go:148-150`) — so the agent's LLM provider call is
dropped by its own firewall until the provider host is added to
`--egress-allow`. That addition rides the real-turn leg (D1/D2 — the LiteLLM
proxy host is added when the credential path lands); do NOT add a provider
host to the base argv.

### 5. Agent image build + load (opt-in task, VERIFIED command)

`tasks."dogfood:agent-image"` runs, from `agent-image/`:

```bash
nix run path:./forks/devenv#devenv -- container copy agent
```

This single command builds AND loads: `container_copy` first calls
`container_build` (`forks/devenv/devenv/src/devenv/container.rs:54`
`let spec = self.container_build(name).await?;`), then runs the copy script.
With `--registry` omitted the CLI passes `"false"` (`container.rs:85`
`"registry": registry.unwrap_or("false")`) and the script falls back to the
container's configured registry (`forks/devenv/src/modules/containers.nix:288-292`
`if [[ "$1" == false ]]; then registry="${cfg.registry}"`), which agent-image
pins to `registry = "containers-storage:"` (`agent-image/devenv.nix:98`). The
destination is `${registry}${cfg.name}:${cfg.version}` (`containers.nix:295`)
with `version` defaulting to `latest` (`containers.nix:324-327`) — i.e.
`containers-storage:compass-agent:latest`, exactly what the runner's
`--image compass-agent:latest` resolves. (`devenv container build agent` alone
builds the derivation but loads nothing — `container.rs:16-45` only
nix-builds.) The invocation is pinned to the vendored fork's own CLI via
`nix run path:./forks/devenv#devenv` rather than the PATH `devenv`: the
agent-image devenv pins only its MODULE set to the fork
(`agent-image/devenv.yaml:44-45`), so a PATH CLI could diverge from the
verified fork source — the pin eliminates that risk.

This task is opt-in (not `after`-wired into `up`, D5): the image
closure is large ("The agent's store is large and grows",
`agent-image/devenv.nix:100-105`) and rebuilding on every `up` violates the
never-heavy-on-up constraint. The runner starts fine without the image present
— it only resolves the image at Provision time.

### 6. The session drive (net-new admin driver — compass-server lane)

The session drive is a net-new admin client owned by the compass-server lane
(confirmed with that owner): a `compass-dogfood` / `compass drive` CLI verb
under `go/cmd`, sequenced AFTER ITEM 7 (secret key-delivery) and ITEM 1
(spawn/despawn record) so the driven agent can actually receive its LLM key.
It:

1. Reads the bootstrap-admin token from `$STATE/compass/admin-token`
   (`network_door.go:35-38`) and dials `https://127.0.0.1:<network-port>`
   trusting `$STATE/compass/tls.crt`, sending `Authorization: Bearer <token>`.
2. `CommsService.CreateAgent{handle, display_name}` — the request carries only
   those two fields (`proto/compass/v1/comms.proto:450-458`); "The owner is
   the caller" (`comms.proto:40-42`). CreateAgent is `authenticatedOpen` on
   the network door (`go/internal/auth/admin_gate.go:68-71`), so the admin
   bearer passes. Idempotency: find-by-handle via `ListAccounts` first, create
   on miss (CreateAgent has no idempotency key).
3. `CompassService.ProvisionAgentWorkspace{agent_account_id, local_path,
   ref, client_request_id}` — adminOnly (`admin_gate.go:50-56`), relayed
   Server → RunnerHub → Runner (`go/server/service.go:107-137`,
   `go/internal/runnerhub/commands.go:48`). `client_request_id` set to a
   stable key so "a timeout-retry with the same id returns the same
   container_name" (`compass.proto:342-348`). Repo source: `local_path`
   (D4) — "local_path clones a container-local bare mirror over
   file:// for a hermetic, network-free clone" (`compass.proto:331-335`;
   `go/internal/runtime/workspace.go:20-23` notes it is "typically a bare
   mirror bind-mounted read-only from a host cache"). A file:// clone needs no
   forge credentials (`workspace.go:109-110` "a file:// clone of a local
   mirror needs none").
4. `CompassService.StartAgentSession{container_name, initial_prompt}`
   (`compass.proto:359-365`) with a trivial smoke prompt; returns the
   `session_id` (`service.go:147-157`).

**Mount-surface blocker (LOAD-BEARING).** The `local_path` mirror path is
unimplementable against the binaries at this HEAD: the RPC carries only an
in-container path string — `validateLocalPath` constrains it to "a plain
absolute in-container path" (`go/internal/runner/spec.go:202-218`) — and
container mounts come EXCLUSIVELY from `SpecDefaults.Mounts`
(`spec.go:26-32,107` `Mounts: d.Mounts`), which `cmd/compass-runner/main.go`
never populates: the `SpecDefaults` literal sets only
Image/Egress/CheckoutDir/HomeDir/UID/NamePrefix (`main.go:117-124`) and the
flag set has no `--mount`/`--volume` (`main.go:40-64`). `runtime.Mount` exists
and is wired through `podman` (`go/internal/runtime/agent.go:42-43` "Mounts is
read-only host mounts (e.g. a bare-repo mirror cache)", `:213`), but no
operator surface reaches it — so no host bare mirror can become
container-visible, and Provision's file:// clone would target a nonexistent
path. The missing piece is a small runner-lane flag (e.g. repeatable
`--mount host:container[:ro]` into `SpecDefaults.Mounts`); this mount-surface
flag is scoped as T5a (D3); the driver ownership itself is resolved
(compass-server lane).

Ownership and shape are RESOLVED: the compass-server lane owns this driver as a
committed `compass-dogfood` / `compass drive` Go CLI (reusing the generated
Connect clients + the CA-trust client), which reads the admin token, then
CreateAgent → ProvisionAgentWorkspace → StartAgentSession → tails the session.
The runner mount surface below is the T5a scope (D3); the driver is a
compass-server-lane `compass-dogfood` / `compass drive` CLI — the record specs
the RPC contract, the owning lane implements it.

### Model credentials (D1/D2)

The runner forwards a model selector: `--agent-model` / `$COMPASS_AGENT_MODEL`
becomes the agent's `COMPASS_MODEL` (`go/cmd/compass-runner/main.go:50-53`;
`go/internal/runner/agent_exec.go:65-67`). The provider credential is separate:
the agent reads a 0600 `$HOME/.compass/auth-seed.json` "the Runner's
materializer writes (design §T5)" (`packages/compass-agent/src/cli.ts:11-12`,
`:47-49` `authSeedPath`), but the T5 materializer is not yet implemented in
the runner (`grep Materialize|auth-seed` over `go/internal/runner` +
`go/internal/runtime` at this HEAD: no matches; only the secrets-plumbing
types exist, `go/internal/secrets/secrets.go:48-57`). An agent with no seed
still boots and reports — "an agent with no provider credential must still
start and report, not crash on first call"
(`packages/compass-agent/src/cli.test.ts:166-168`) — so the loop can smoke
container-spawn + session-start today. But spawn+idle does NOT satisfy the
ruled acceptance ("runs an agent turn"), and a real turn needs BOTH a
credential in the container AND the provider host in `--egress-allow`
(Approach §4). The production cred path IS being built: the SEA-1327 secrets
materializer (ITEM 7 — FetchSecrets → the runner writes the frozen
`auth-seed.json`) is the writer the compass-server lane owns, so the real-turn
leg sequences AFTER ITEM 7 rather than needing a throwaway seed. How and when
the turn becomes real is D1 (sequencing) + D2 (LiteLLM proxy, key via user
`SetSecret`).

### Cert model (summary)

One `compass-gen-cert` artifact is simultaneously the server's `--tls-cert`
and the runner's (and driver's) `--ca` — "the single cert is its own CA, so
the same file is the Server's --tls-cert and the Runner's --ca trust anchor —
one artifact exercising the real production TLS enroll path locally (no
external CA, no relaxed loopback)" (`go/cmd/compass-gen-cert/main.go:6-9`).

## Alternatives considered

- **Wire everything (image build + session drive) into every `devenv up`.**
  Rejected as the default (D5): the image closure is large and an
  LLM turn on every shell entry is slow, costly, and needs credentials.
  Enrollment-on-up + opt-in drive keeps `up` light while the full loop stays
  one command away.
- **Skip TLS locally (serve the RunnerService over dev-http).** Not possible
  and not desirable: RunnerService is mounted only on the network door
  (`serve.go:199-201`), and the `--dev-http` door fail-closes adminOnly RPCs
  (`serve.go:245-249`). The self-signed-cert path exercises the production
  enroll seam, which is the point of dogfooding.
- **Mint the runner token to stdout and capture in the process script.**
  Rejected: `--token-out` is the designed idempotent path
  (`compass-mint-runner-token/main.go:53-59`); stdout "always mints"
  (`main.go:60-62`) and would rotate the credential on every up.
- **A second devenv for the loop.** Rejected: agent-image is its own devenv
  for image-env isolation reasons (`agent-image/devenv.nix:5-14`), but the
  loop processes belong beside the existing `processes.compass-server` —
  a second process tree would duplicate the postgres/DSN wiring.

## Plan

Tasks are lane-tagged: `[repo]` = compass-repo lane (devenv.nix wiring,
independently shippable); `[compass-server]` = the net-new admin session
driver (T5); `[compass-runner]` = the runner mount-surface flag (T5a). Each
task carries a real red-green cycle plus a smoke assertion
(rule://red-green-testing).

### T1 [repo] — gen-cert task + TLS door on compass-server

Add `tasks."dogfood:gen-cert"` and extend the `processes.compass-server` argv
with `--listen 127.0.0.1:<ports.network> --tls-cert --tls-key`, keeping the
existing `--socket`/`--dev-http` (`devenv.nix:164-167`). Build
`compass-gen-cert` into `$DEVENV_STATE/compass/` the same way the server
binary is built (`devenv.nix:158-163`). Order: the task runs `before` the
compass-server process. Allocate the network port beside `ports.devhttp`
(`devenv.nix:170`).

Interfaces:

- Consumes: `compass-gen-cert --cert-out <path> --key-out <path>` (SAN default
  `127.0.0.1,::1,localhost`, skip-if-present, `--force` to rotate)
  (`go/cmd/compass-gen-cert/main.go:48-64`).
- Consumes: `compass-server --listen <addr> --tls-cert <pem> --tls-key <pem>`
  all-or-none (`go/cmd/compass-server/main.go:50-60,146-154`).
- Produces: `$DEVENV_STATE/compass/tls.crt` (0644), `tls.key` (0600), and a
  server whose network door writes `$DEVENV_STATE/compass/admin-token` (0600)
  on startup (`go/server/network_door.go:35-38,249-256`). Note the coupling:
  the token path is a FUNCTION of the socket path — StateDir defaults to the
  socket's parent, and `compass-server` has no `--state-dir` flag (the
  ServeConfig literal sets only SocketPath/Version/DevHTTP/Listen/TLS/
  DatabaseDSN, `go/cmd/compass-server/main.go:136-143`), so the default ALWAYS
  applies. Relocating `COMPASS_SOCKET` silently moves the driver's credential;
  T7's admin-token smoke covers it.

Test cycle: red — `devenv up` on main has no TLS door: `curl --cacert tls.crt
https://127.0.0.1:<port>/...GetServerInfo` fails (connection refused). Green —
after wiring, the same probe returns 200-class over TLS with the generated
cert, and `admin-token` exists 0600. Idempotence smoke: second `devenv up`
leaves `tls.crt` mtime unchanged.

### T2 [repo] — mint-runner-token task

Add `tasks."dogfood:mint-runner-token"`, `after` compass-server readiness
(GetServerInfo probe, `devenv.nix:184-197`; mint needs the migrated store —
`go/cmd/compass-mint-runner-token/main.go:88` `store.Open`, which "applies any
pending embedded migrations" `go/internal/store/store.go:55-57`).

Interfaces:

- Consumes: `compass-mint-runner-token --runner-id dogfood --token-out
  $DEVENV_STATE/compass/runner.token` with `COMPASS_DATABASE_DSN` from the
  server's env (`devenv.nix:179`; DSN precedence parity per
  `compass-mint-runner-token/main.go:100-101`).
- Produces: `$DEVENV_STATE/compass/runner.token`, 0600, raw token, no newline
  (`main.go:192-196`), idempotent/heal-without-rotate (`main.go:53-58`).

Test cycle: red — task absent; no token file after `up`. Green — token file
exists 0600 after `up`; a second `up` does not rotate it (byte-identical);
deleting the database but keeping the file re-registers the same token
(covered by the binary's own tests, `main_test.go`; the devenv smoke asserts
the no-rotate case).

### T3 [repo] — compass-runner process

Add `processes.compass-runner`, `after` the mint task, exec'ing the runner
with the flags in Approach §4 and `COMPASS_RUNNER_TOKEN` read from the token
file inside the exec script (env-only contract,
`go/cmd/compass-runner/main.go:91-96`). `ready` is left UNSET: the devenv
readiness type supports only `exec`/`http.get`/`notify`
(`forks/devenv/src/modules/lib/ready.nix:8-56`) — there is no log-line
readiness, and the runner exposes no HTTP surface to probe (it idles in
`RunSessions`, `run.go:115-121`). Unset is safe because nothing `after`s the
runner (the session drive is opt-in/manual) and enrollment is asserted by
T7's log grep. Set `restart.on = "on_failure"` (matching compass-server's
convention, `devenv.nix:198`): `Dial`/`Enroll` is single-shot with no retry
(`go/internal/runner/runner.go:101-125`; `run.go:96-104` returns the error),
so without a restart policy a transient enroll failure leaves a permanently
dead runner.

Interfaces:

- Consumes: `compass-runner --runner-id dogfood --server
  https://127.0.0.1:<network-port> --ca $DEVENV_STATE/compass/tls.crt --image
  compass-agent:latest --runtime-dir $DEVENV_STATE/compass/runner` +
  `COMPASS_RUNNER_TOKEN` env (`main.go:40-57,91-96`); `--egress-allow` empty
  for the base loop (Approach §4).
- Produces: an enrolled, idle runner (`Enroll` RPC then `RunSessions`,
  `go/internal/runner/runner.go:101-113`, `run.go:96-121`).

Test cycle: red — no runner process on main. Green — `devenv up` log shows
"runner enrolled" with `runner_id=dogfood`; kill/restart the runner process
and it re-enrolls (reattach path, `run.go:100` `link.Reattached()`). Failure
smoke: a corrupted token file yields the runner's Unauthenticated error
("an Unauthenticated here means a bad/expired/wrong-kind token",
`runner.go:97-99`), proving the door actually authenticates.

### T4 [repo] — agent-image build+load task (opt-in per D5)

Add `tasks."dogfood:agent-image"` running
`nix run path:./forks/devenv#devenv -- container copy agent` from
`agent-image/` (build+load semantics verified in Approach §5; the invocation
is pinned to the vendored fork's CLI). Not wired
`after` into `up` (D5).

Interfaces:

- Consumes: `nix run path:./forks/devenv#devenv -- container copy agent` in
  `agent-image/` (`forks/devenv/devenv/src/cli.rs:1100-1113`,
  `devenv/container.rs:47-108`).
- Produces: `containers-storage:compass-agent:latest`
  (`forks/devenv/src/modules/containers.nix:295`, `agent-image/devenv.nix:67,98`,
  version default `latest` `containers.nix:324-327`).

Test cycle: red — `podman image exists compass-agent:latest` fails on a clean
box. Green — after the task, it succeeds. Linux-only guard: the task is a
no-op (or absent) on non-Linux, matching `agent-image/devenv.nix:65`.

### T5 [compass-server] — the admin session driver (owned by compass-server lane)

Implement the driver in Approach §6: read
`$DEVENV_STATE/compass/admin-token`, dial the TLS door with the shared cert as
trust anchor, then CreateAgent (find-or-create by handle) →
ProvisionAgentWorkspace (`local_path` mirror, stable `client_request_id`) →
StartAgentSession (smoke `initial_prompt`). BLOCKED at this HEAD on the
missing runner mount surface (Approach §6 "Mount-surface blocker"): a
container-visible bare mirror requires a new compass-runner `--mount
host:container[:ro]` flag wiring into `SpecDefaults.Mounts`
(`go/internal/runner/spec.go:26-32,107`) plus devenv wiring to pass it — a
cross-lane dependency whose mount-surface flag is scoped as T5a (D3; the
driver itself is compass-server-owned). Preparing the bare mirror itself is
driver-side work
(the runtime integration test seeds one the same way,
`go/internal/runtime/lifecycle_test.go:99-102` — though that test drives the
runtime API directly, below the runner binary).

Interfaces:

- Consumes: `CommsService.CreateAgent{handle, display_name} →
  {account}` (`proto/compass/v1/comms.proto:450-462`);
  `CompassService.ProvisionAgentWorkspace{agent_account_id, local_path, ref,
  client_request_id} → {container_name}` (`compass.proto:326-355`);
  `CompassService.StartAgentSession{container_name, initial_prompt} →
  {session_id}` (`compass.proto:359-371`); bearer auth per
  `admin_gate.go:50-56,68-71`.
- Produces: a running `compass-agent-*` container (podman), a live session id,
  and a nonzero exit + named cause on any step's failure.

Test cycle: red — driver absent/failing against a stood-up loop. Green —
driver exits 0; `podman ps` shows the agent container;
`GetAgentStatus` (adminOnly, `admin_gate.go:50-56`) lists the session.

### T5a [compass-runner] — the runner mount-surface flag

Add a repeatable `--mount host:container[:ro]` flag on `compass-runner` that
populates `SpecDefaults.Mounts` (`go/internal/runner/spec.go:26-35`, the field
is designed and wired through to podman at `spec.go:100-107` but no operator
surface reaches it today, `cmd/compass-runner/main.go:40-64,117-124`). This
unblocks the `local_path` bare mirror (D3): the devenv wiring passes the host
mirror path so it becomes container-visible for Provision's file:// clone.

Interfaces:

- Consumes: `compass-runner --mount <host>:<container>[:ro]` (repeatable),
  wiring into `SpecDefaults.Mounts` (`spec.go:26-35`).
- Produces: a runner that mounts the host bare mirror read-only into the agent
  container at the path `ProvisionAgentWorkspace{local_path}` names.

Test cycle: red — a runner built without the flag leaves `SpecDefaults.Mounts`
nil, so Provision's file:// clone targets a path absent from the container and
fails. Green — `--mount <host-mirror>:<container-path>:ro` populates
`SpecDefaults.Mounts` and the mirror is readable inside the container, so the
clone succeeds.

### T6 [repo] — teardown: `dogfood:clean` task

Nothing tears agent containers down: on runner shutdown only the agent
SOCKETS close — "every container lives until the Runner process ends" refers
to socket lifetime; `Close` touches `h.sockets` only
(`go/internal/runner/host.go:162-179`), and the podman container survives
`devenv down`. Container names are deterministic (NamePrefix + agent account
id, `go/internal/runner/spec.go:33-35`), and the `client_request_id` dedup is
in-memory (gone after restart) — so the SECOND dogfood run's Provision hits a
`podman create` name collision: without cleanup the loop works exactly once
per boot. Add an opt-in `tasks."dogfood:clean"` that `podman rm -f`s the
`compass-agent-*` containers and sweeps the runner socket dir
(`$DEVENV_STATE/compass/runner/containers/`). Alternative (implementer's
choice): a driver-side stale-container preflight that `podman rm -f`s its own
target container name before Provision.

Interfaces:

- Consumes: `podman ps -a --filter name=compass-agent-` + `podman rm -f`;
  the runner socket dir layout `RuntimeDir/containers/<container>/agent.sock`
  (`go/cmd/compass-runner/main.go:56-57`).
- Produces: a clean slate such that a re-run of the session drive provisions
  successfully.

Test cycle: red — two consecutive session drives without clean: the second
Provision fails on the name collision. Green — drive → clean → drive
succeeds twice.

### T7 [repo] — end-to-end `devenv up` smoke + docs

A scripted smoke (an opt-in `dogfood:session` task per D5
chaining T4+T5, or CI-style script) asserting, in order: postgres up; server
ready with TLS door answering under the generated cert; `admin-token` +
`runner.token` present 0600 (the admin-token assertion doubles as the
regression guard for the serial token-write-before-serve ordering, Approach
§2); runner log shows enrollment; and — gated on the session-drive decision —
T5's green assertions. Update the `devenv up` header comment
(`devenv.nix:115-123`) to describe the full loop; document the box prereqs
(Linux, rootless podman, uid 1000, subuid/subgid) and cert expiry: gen-cert
is skip-if-present forever with a finite `--validity`
(`go/cmd/compass-gen-cert/main.go:57-58,82-89`), so on expiry the loop fails
with an opaque TLS error — rerun `compass-gen-cert --force` to rotate.

Interfaces:

- Consumes: everything T1-T6 produce.
- Produces: one command a developer runs to verify the loop; the record's
  acceptance evidence.

Test cycle: the smoke IS the test; red is any assertion failing on a clean
checkout before the wiring lands, green is all passing after.

## Tasks

- [ ] T1 [repo] gen-cert task + TLS door on compass-server (red-green + idempotence smoke)
- [ ] T2 [repo] mint-runner-token task after server-ready (red-green + no-rotate smoke)
- [ ] T3 [repo] compass-runner process: enroll + idle, restart on_failure (red-green + bad-token smoke)
- [ ] T4 [repo] agent-image build+load opt-in task via the fork-pinned CLI (red-green via podman image exists)
- [ ] T5 [compass-server] admin session driver `compass-dogfood`/`compass drive`: CreateAgent → Provision(local_path) → Start → tail (red-green + podman ps smoke) — owned by compass-server lane, sequenced after ITEM 7 + ITEM 1
- [ ] T5a [compass-runner] runner `--mount host:container[:ro]` flag into SpecDefaults.Mounts (unblocks local_path bare mirror) — scoped here per D3
- [ ] T6 [repo] dogfood:clean teardown task (red-green via second-run provision)
- [ ] T7 [repo] end-to-end `devenv up` smoke + prereq/cert-expiry docs (session leg is opt-in per D5)

## Decisions (ruled by Matt)

F2 (idempotent devenv tasks) and F3 (gen-cert → postgres → server-ready →
mint → runner ordering) are recorded in Global Constraints. The forks below
were batched to Matt at the design-PR gate and ruled as noted.

1. **D1 — Real-turn sequencing (was OQ1): ship the enroll loop now; the
   real-turn leg follows the ITEM 7 + driver chain.** The compass-repo enroll
   loop (postgres, server+TLS, mint, runner enrolled+idle) has no credential
   dependency and ships standalone as `[repo]` tasks T1-T4/T6/T7. The FULL
   "runs a turn" acceptance completes once the SEA-1327 secrets materializer
   (compass-server lane) and the driver (T5) land. A real turn needs three
   coupled pieces, all deferred to that chain: (i) the provider credential —
   the frozen 0600 `$HOME/.compass/auth-seed.json` the SEA-1327 materializer
   writes (`packages/compass-agent/src/cli.ts:47-50,98-102`;
   `cli.test.ts:110-111`); (ii) the provider host in `--egress-allow` (default
   is pure default-deny, `go/internal/runtime/egress.go:29-31`,
   `agent.go:148-150`); (iii) the model selector via
   `--agent-model`→`COMPASS_MODEL` (`go/cmd/compass-runner/main.go:50-53`).
2. **D2 — Model + credential path (was OQ1b): LiteLLM proxy, key via the user
   `SetSecret` (KIND_PROVIDER) path.** The dogfood agent targets the LiteLLM
   proxy as its provider; a developer seeds the key once via the user-facing
   `SetSecret` flow (the production cred path, ITEM 7), so the dogfood exercises
   the real secret path end to end. `--egress-allow` gets the LiteLLM proxy
   host for the real-turn leg. Applies when the real-turn chain (D1) lands.
3. **D3 — Mount-surface scope (was OQ2): a scoped T5a task in this record.**
   Driver ownership is settled (compass-server lane, T5). The remaining gap —
   nothing populates `SpecDefaults.Mounts` at this HEAD, so the `local_path`
   bare mirror cannot reach the container (Approach §6 "Mount-surface
   blocker") — is closed by a net-new compass-runner
   `--mount host:container[:ro]` flag into `SpecDefaults.Mounts` (T5a,
   `go/internal/runner/spec.go:26-32,107`), with the devenv wiring passing it
   ([repo]). Not a separate design record — a single scoped task here.
4. **D4 — `local_path` mirror (was OQ3).** Repo source is `local_path` with a
   bare mirror of the compass repo itself — hermetic, credential-free
   (`compass.proto:331-335`; `go/internal/runtime/workspace.go:109-110`),
   contingent on the D3 mount surface. Follows from D3.
5. **D5 — Session drive is opt-in (was OQ4).** The heavy legs (image build,
   live LLM turn) land as explicit opt-in tasks (`devenv tasks run
   dogfood:session`), not wired into `devenv up`. Note: `devenv up` is an
   explicit command that starts the process stack — it is NOT run automatically
   on shell entry (direnv loads env/tooling on entry; `up` is separate). So the
   split keeps the on-demand heavy legs off the always-on `up` process set: `up`
   brings up the light, idempotent enroll loop; the image build + real turn run
   on demand.

A related public-API fork in the ITEM 7 (SEA-1327 secrets) lane was ruled in
the same batch: the `SetSecret`/`ListSecrets`/`DeleteSecret` RPCs go on a new
`SecretsService` (not folded onto `CompassService`). That governs the ITEM 7
proto edit, not this record's tasks; noted here only for provenance.
