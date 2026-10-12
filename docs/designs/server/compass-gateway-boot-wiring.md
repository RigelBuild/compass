# Compass gateway boot wiring

## Problem / Intent

Tracking: RIG-2862 (remainder: boot wiring). Addendum to
[`compass-server-llm-gateway`](./compass-server-llm-gateway/design.md) §T1 and
to the §Global Constraints "Compass boot entrypoint" bullet of
[`compass-gateway-credential-door-agent-token`](./compass-gateway-credential-door-agent-token.md).

The fork's compass boot entrypoint (`packages/coding-agent/src/cli/gateway-boot.ts`
in RigelBuild/oh-my-pi) calls `runAuthGatewayCommand({ action: "serve" })`.
Its `runServe` (`packages/coding-agent/src/cli/auth-gateway-cli.ts`) throws
without a broker, under `if (!brokerConfig) {`:
`` "`omp auth-gateway serve` requires OMP_AUTH_BROKER_URL …" ``. So the
published image cannot serve under the stack. This record wires that entrypoint to the Compass
Server's doors: agent tokens are verified by `GatewayAuth.VerifyAgentToken`,
each owner gets its own `AuthStorage` over `CompassAuthCredentialStore`, and
the stack mints, registers and mounts the `llm-gateway` service bearer those
door calls need.

## Approach

The work has two halves: a fork boot that talks to the Server, and a stack
spec that gives the boot what it needs.

### Fork: a compass boot, not a reshaped `runServe`

`gateway-boot.ts` stops calling `runAuthGatewayCommand`. It calls
`startAuthGateway` itself with compass adapters. `runServe` in
`packages/coding-agent/src/cli/auth-gateway-cli.ts` stays the broker path for
upstream `omp auth-gateway serve`. The boot reuses `registerGatewayShutdown`
and the `GATEWAY_MODEL_KINDS` list (exported by this change). Two new modules
go in `packages/ai/src/compass/`, next to the store, as gateway adapters:

- **`agent-verifier.ts`: the authorizer.** It sends the presented bearer to
  `POST <COMPASS_SERVER_URL>/compass.v1.GatewayAuth/VerifyAgentToken`, using
  the same Connect JSON transport and service bearer as `#request` in
  `credential-store.ts`. The `AuthGatewayAuthorizer` doc in
  `auth-gateway/types.ts` sets the result mapping: "`null` to answer 401. A
  throw answers 503 … bound any remote call; a timeout should throw."
  - No bearer → `null`, with no call made.
  - 200 → `{ kind: "agent", agentAccountId, ownerUserId }`. An empty id
    throws.
  - `unauthenticated` with message `invalid agent token` → `null`. That
    message is `errGatewayTokenInvalid` in `go/server/gateway_auth.go`
    (`var errGatewayTokenInvalid = errors.New("invalid agent token")`); S0
    pins it as a wire contract (OQ4).
  - Any other `unauthenticated` → throw (503). A bad service bearer gets
    `errServiceUnauthenticated` from `ServiceBearerInterceptor`
    (`go/internal/auth/service_door.go`:
    `connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))`).
    Mapped to 401, a broken gateway would look like a bad agent token.
  - `internal`, `unavailable`, a timeout or a transport error → throw.
  - The deadline is `AbortSignal.any([req.signal, AbortSignal.timeout(5_000)])`.
  - **No cache.** One loopback call per admitted request, so a revoked token
    fails at once (OQ3).
- **`owner-pool.ts`: the per-owner store pool.** `resolveStorage(caller)`
  calls `pool.get(caller.ownerUserId)`. A missing `ownerUserId` throws, which
  the router answers with 503 `credential pool unavailable`
  (`createAuthGatewayRouter` in `server.ts`).
  - **Lazy, single-flight open.** On first use the pool opens, per owner:
    1. `CompassAuthCredentialStore.open({ baseUrl, token, fetch })`, with no
       `agentAccountId`;
    2. `new AuthStorage(store, { sourceLabel: "compass" })`;
    3. `storage.credentials.reload()`.

    Concurrent first requests share one open, and a failed open is not
    cached. The open runs inside `routeScope`. On `rigel-release`,
    `createAuthGatewayRouter` (`packages/ai/src/auth-gateway/server.ts`) wraps
    the router body, `resolveStorage` included:
    `return opts.routeScope ? await opts.routeScope(req, body) : await body();`.
    So `open()` sees the agent token.
  - **Per-request leases.** A stream outlives `route`. Bun idles a response up
    to 255 s (`startAuthGateway` in `server.ts`: `idleTimeout: 255,`). A
    mid-stream OAuth refresh then writes back through the store. A store
    closed under it throws (`#request` in
    `packages/ai/src/compass/credential-store.ts`:
    `if (this.#closed) throw new CompassRpcError("closed", "Compass credential store is closed");`),
    and the rotated refresh token is lost. So:
    - `routeScope` runs each routed request in a lease scope.
    - Each `get` inside it takes a lease on the entry.
    - The lease ends when the response body ends, errors or is cancelled. The
      boot wraps the body: a `TransformStream` `flush`/`cancel` pair, or a
      `ReadableStream` wrapper if Bun lacks the transformer `cancel` hook.
    - A body-less result or a throw ends the lease at once.
  - **Eviction.** Each `get` stamps `lastUsedMs`. A sweep (60 s timer,
    `unref`) closes entries that meet all of:
    - `leases === 0`;
    - `store.hasPendingWrites()` is false;
    - idle longer than `idleMs` (15 min).

    Over `maxOwners` (256), the sweep also closes least-recently-used entries
    that are idle longer than 60 s and pass the same two checks. The cap is
    soft. The `hasPendingWrites()` pin is the door record's rule.
  - **`close()` on shutdown** closes every entry, pinned or leased. A parked
    write is lost at exit. The door record accepts this residual.
  - **No credential-poll timer** (door record: "A credential-poll timer is not
    needed").
- **`routeScope`.** This is the door record's line, plus the lease scope:
  `(req, run) => { const t = presentedBearer(req); const leased = () => pool.withRequestLease(run); return t ? runWithAgentToken(t, leased) : leased(); }`.
- **Catalog: the bundled catalog, by `provider/id` only (OQ1).**
  `ModelResolver` is `(modelId) => Model<Api> | undefined` (`dispatch.ts`). It
  never sees the caller, and `/v1/models` "skips the pool lookup". So no
  per-owner catalog is possible without a router-core change. At start the
  boot builds one map from `getBundledProviders()` and `getBundledModels()`
  (`@oh-my-pi/pi-catalog`), keeping kinds in `GATEWAY_MODEL_KINDS`, keyed
  `` `${provider}/${id}` `` only.
  - **No bare-id fallback.** `indexModelsByRequestId` (`auth-gateway-cli.ts`)
    adds one: `if (!modelById.has(model.id)) modelById.set(model.id, model);`.
    Across every bundled provider, that fallback would route a bare id to the
    first provider in catalog order, which the owner may hold no credential
    for. A bare id gets the router's 404 instead (`server.ts`:
    `` formatError(404, "invalid_request_error", `Unknown model: ${modelId}`) ``).
  - A model whose provider is not in the owner's pool fails in dispatch with
    401 `No credential available for provider X` (`resolveGatewayApiKey`).
  - `pickProbeCandidates` already sends `getBundledModels` output straight to
    providers, so bundled `Model` objects are callable without
    `ModelRegistry`.
  - There is no refresh timer. Stable-name P1 replaces this resolver.
- **Authorizer composition.** `authorize = verifier`. When
  `COMPASS_GATEWAY_ENROLL_TOKEN_FILE` is set, it becomes
  `withEnrollToken(enrollToken, verifier)` (OQ5).
- **No inbound shared bearer.** `bearerTokenAuthorizer` is not used. Agents
  carry per-agent tokens (parent T3/T5), and no Go code reads
  `gateway.token` except the stack mount (`gatewayContainerSpec`) (OQ2).
- **TLS to the door.** The store and the verifier share one compass fetch:
  `(input, init) => fetch(input, { ...init, tls: { ca } })`, with `ca` read
  from `COMPASS_SERVER_CA_FILE`. Provider calls keep the default trust store.
  `NODE_EXTRA_CA_CERTS` is not used: `wrapFetchForExtraCa`
  (`packages/utils/src/tls-fetch.ts`) would apply the CA to every fetch.
- **Fail-fast door probe.** `/healthz` answers 200 with no checks
  (`startAuthGateway`), so `waitGateway` passes even when every door call
  fails. Before bind, the boot calls `VerifyAgentToken` with `token: ""`. It
  expects `unauthenticated` with the message `invalid agent token`. On the
  Server, `GatewayTokens.Verify` (`go/internal/auth/gateway_token.go`) returns
  `ErrGatewayTokenInvalid` for a malformed token before any store read
  (`if !wellFormedToken(token) { return store.GatewayCaller{}, ErrGatewayTokenInvalid }`).
  Any other result exits non-zero. The probe proves network
  reach, TLS and a live service bearer. It reads no credentials, so door
  rule 1 ("no door call with no request behind it") still holds. After the
  bounded restarts the container stays down, and `waitGateway` fails `up`
  loudly.

### Stack: service bearer, server address, start order

- **The service bearer.** Its file is `<state>/gateway/service.token`,
  mounted at `/run/compass/service.token` and named by
  `COMPASS_SERVICE_TOKEN_FILE`. S1 deletes the old `<state>/gateway/gateway.token`.
  The rename keeps the old hex inbound secret (`ensureGatewayToken`:
  `os.WriteFile(path, []byte(hex.EncodeToString(raw[:])), 0o600)`) from being
  adopted through the "unknown → re-register" arm. It also makes version skew fail before bind:
  an old image reads the missing `gateway.token`, and a new image finds no
  `COMPASS_SERVER_URL`.

  `EnsureServiceToken` uses the file-before-store shape of `ensure` in
  `go/internal/stack/adapters/token.go`. S1 makes the status check generic
  over `store.Subject`. `RunnerTokenStatus` (`go/internal/runnerhub/mint.go`)
  hardcodes `if subj.Kind == store.SubjectRunner && subj.ID == runnerID {`. The want is
  `{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID}`:
  - **Registered under that subject:** keep the token.
  - **Unknown** (database replaced): re-register the same token.
  - **Revoked, or another subject:** mint a fresh one. The runner instead
    keeps a revoked token, but no operator CLI mints service tokens.
  - **Minting:** `runnerhub.GenerateRunnerToken`, then write the file at 0600
    atomically, then `PutTokenHash`. If the hash commit fails, remove the
    file.

  `ensureGatewayToken` (`adapters/gateway_container.go`) is deleted.

  Residuals:
  - a revoke while the stack runs means 503 on every request until the next
    `up`;
  - with `ExternalDatabase` and a wiped state dir, each `up` leaves the old
    `llm-gateway` hash live, with no revoke-by-subject. The runner path has
    the same flaw.
- **Server address (Linux).** The container runs with
  `--network pasta:-T,<door port>` and dials
  `https://127.0.0.1:<door port>`. The cert's `127.0.0.1` SAN matches
  (`go/internal/stack/adapters/cert.go`:
  `var certHosts = []string{"127.0.0.1", "::1", "localhost"}`). By default pasta
  does not forward container loopback to the host (`pastaHostGateway` comment
  in `go/e2e/fixture.go`: "pasta does not forward 127.0.0.1"). Local rootless
  podman 5.8.6 probes showed:
  - the default network is refused;
  - `-T` reaches the host listener, and `-p` publishing still works.

  `<state>/tls.crt` (0644, `certgen.WriteFiles`) is mounted read-only at
  `/run/compass/server-ca.crt`. On darwin the server runs on the Mac and the
  container runs in the podman-machine VM (`internal/embedded/machine.go`).
  There `-T` would reach the VM's loopback [INFERENCE]: OQ7. The spec carries
  the URL and network args as fields, so the OQ7 choice changes only S2 and
  `certHosts`.

  The door host must be one that `-T` forwards to the fixed
  `127.0.0.1` URL. `overlapsLoopbackPublish` (`go/internal/stack/config.go`) is
  too wide for this, because it also passes every DNS name:
  `return ip == nil || ip.IsUnspecified() || ip.Equal(net.IPv4(127, 0, 0, 1))`.
  S2 therefore adds its own check (Global Constraints, Door listen host).
- **Start order.** Today `spawnChain` (`stack.go`) starts the gateway at step
  1e, before the TLS anchor (step 2) and the server (step 3). On a first run
  that leaves no CA file and no store to register the token in. The gateway
  moves to after `waitReady` and before `startRunner`, and in-process
  `drainChildren` becomes runner → gateway → server. The drain gain is small.
  By then the runner and its agents are gone, and once T5 revokes tokens on
  stop, a late write-back parks and is lost anyway (OQ6).

## Alternatives considered

- **Reshape `runServe` with a `--compass` mode.** This mixes broker and
  compass wiring in one upstream command. Rejected.
- **Bare-id index over all providers.** Misroutes shared bare ids. Rejected
  for `provider/id` only.
- **Keep the shared bearer as a second admitted class (OQ2).** Anyone holding
  the file would get a pool-less `"\u0000shared"` caller. Rejected.
- **Reuse `gateway.token` for the service bearer.** It adopts the old inbound
  secret, and version skew is silent about which meaning the file has.
  Rejected for the rename.
- **`--network host`.** Drops `-p` and network isolation. Rejected.
  `host.containers.internal` is OQ7 option A.
- **ErrorInfo reason on `VerifyAgentToken`.** The verifier would have to
  decode protobuf error details from Connect JSON. Rejected for S0's literal
  pin (OQ4).
- **Cache `VerifyAgentToken` results.** A revoked token would keep working.
  Deferred (OQ3).

## Global Constraints

- **Public repo.** This record and every compass task cite only compass and
  RigelBuild/oh-my-pi.
- **Fork changes stay seam-shaped.** Fork changes are limited to:
  - new files in `packages/ai/src/compass/`;
  - the boot (`gateway-boot.ts`);
  - one export from `auth-gateway-cli.ts` (`GATEWAY_MODEL_KINDS`).

  No change to `auth-storage.ts`, `packages/ai/src/auth/`, or
  `auth-gateway/server.ts` or `dispatch.ts`.
- **Fork base.** `rigel-release` with RIG-5205 (`routeScope`, `d0ac08ac56cc`)
  and RIG-5206 (fork #146). #146 must first be rebased onto a `rigel-release`
  that contains `d0ac08ac56cc`; its workspace has no `routeScope`.
- **Server dependency.** A default-pinned image needs RIG-5204. On main,
  `ListCredentialPoolRequest` still requires `agent_account_id`
  (`gateway_credentials.proto`: "empty is INVALID_ARGUMENT").
- **Fail before bind.** The boot exits non-zero and never binds when:
  - `COMPASS_SERVER_URL` is unset or fails `assertSecureBaseUrl`;
  - an https URL has no readable `COMPASS_SERVER_CA_FILE`;
  - `COMPASS_SERVICE_TOKEN_FILE` is unset or fails `readGatewayToken`;
  - `COMPASS_GATEWAY_ENROLL_TOKEN_FILE` is set but unreadable, or fails
    `withEnrollToken`;
  - the door probe gets anything but `invalid agent token`.

  The compass values have no defaults.
- **Secrets never logged.** The service bearer, agent tokens and enroll token
  never appear in a log line or an error. Only `errorName(error)` reaches
  logs.
- **Env contract.** The stack passes these with `-e`, never as image `ENV`.
  I1 removes the `COMPASS_GATEWAY_TOKEN_FILE` `ENV` from `Dockerfile.gateway`
  and its entry from `SECRET_NAME_ALLOWLIST` (`tools/gateway-image/core.ts`).

  | Name | Value | Supplier |
  |---|---|---|
  | `COMPASS_SERVICE_TOKEN_FILE` | `/run/compass/service.token` | stack (S2), new mount |
  | `COMPASS_SERVER_URL` | `https://127.0.0.1:<door port>` (Linux; darwin: OQ7) | stack (S2) |
  | `COMPASS_SERVER_CA_FILE` | `/run/compass/server-ca.crt` | stack (S2), new mount |
  | `COMPASS_GATEWAY_BIND` | `0.0.0.0:4000` | stack, existing |
  | `COMPASS_GATEWAY_DRAIN_MS` | `20000` | stack, existing |
  | `COMPASS_GATEWAY_ENROLL_TOKEN_FILE` | optional | E2/E3 lane, later |

- **Door listen host.** S2 accepts only a `ListenAddr` host of `127.0.0.1`,
  `localhost`, or an unspecified IP (`0.0.0.0`, `::`). Each one accepts on
  `127.0.0.1`, which is the URL the gateway dials. Any other host, including
  any other DNS name, errors in the spec and names `--gateway-external`.
  Option A of OQ7 changes the URL, not this host rule.
- **Order.** Each line below is linear.
  - Compass: S0 → S1 → S2 → P1. S2 calls `EnsureServiceToken`.
  - Fork: #146 rebased → F1 → F2 → F3. F1 has no code dependency on #146.
  - I1 follows F3's merge to `rigel-release`, because `build.ts` checks
    ancestry.
  - P1 also waits on I1's digest and RIG-5204.

  No Pulumi state is involved.

## Plan

### S0 — Pin the verify message (stack)

Lane: compass-server (owns `go/server`).

Interfaces: none new. Add a test in `go/server/gateway_auth_test.go` that
asserts `connect.CodeOf(err) == connect.CodeUnauthenticated` and
`err.(*connect.Error).Message() == "invalid agent token"`. Its comment names
the gateway verifier and boot probe as consumers.

Test cycle: `go test ./server -run VerifyAgentToken`.

### F1 — Agent verifier (fork)

Lane: compass-gateway.

```ts
// packages/ai/src/compass/agent-verifier.ts (exported from ./compass)
export interface CompassAgentVerifierOptions {
	baseUrl: string;       // same rules as CompassAuthCredentialStoreOptions.baseUrl
	token: string;         // llm-gateway service bearer
	fetch?: typeof fetch;
	timeoutMs?: number;    // default 5_000
}
export function createCompassAgentVerifier(opts: CompassAgentVerifierOptions): AuthGatewayAuthorizer;
/** The boot's pre-bind probe: resolves when the door answers `invalid agent token` to "". */
export function probeCompassDoor(opts: CompassAgentVerifierOptions): Promise<void>;
```

Consumes `compassRpcErrorFromResponse` and `decodeCompassJson` (`wire.ts`) and
`presentedBearer` (`auth-gateway/http.ts`).

Test cycle: `bun test` against a fake `fetch`. Cases:

- no bearer → `null`, with no call made;
- 200 → `AgentCaller`, and the request carries the service bearer plus
  `{ token }`;
- `invalid agent token` → `null`;
- other `unauthenticated` → throws;
- `internal` → throws;
- timeout → throws;
- `req.signal` abort cancels the call;
- an empty id in the response → throws;
- the probe resolves only on `invalid agent token`;
- no token appears in any error.

### F2 — Owner pool (fork)

Lane: compass-gateway.

```ts
// packages/ai/src/compass/owner-pool.ts (exported from ./compass)
export interface CompassOwnerEntry { storage: AuthStorage; store: { hasPendingWrites(): boolean } }
export interface CompassOwnerPoolOptions {
	open: (ownerUserId: string) => Promise<CompassOwnerEntry>;
	maxOwners?: number;        // default 256 (soft)
	idleMs?: number;           // default 900_000
	minIdleMs?: number;        // default 60_000; floor for cap eviction
	sweepIntervalMs?: number;  // default 60_000; 0 = no timer (tests)
	now?: () => number;
}
export class CompassOwnerPool {
	constructor(opts: CompassOwnerPoolOptions);
	/** Inside withRequestLease, also leases the entry until that request's lease ends. */
	get(ownerUserId: string): Promise<AuthStorage>;
	/** Runs one routed request. Its leases end when a Response body ends, errors or is cancelled; at once for a body-less result or a throw. */
	withRequestLease<T>(run: () => Promise<T>): Promise<T>;
	sweep(): void;
	close(): void;
}
export function openCompassOwner(opts: CompassAuthCredentialStoreOptions): Promise<CompassOwnerEntry>;
```

Test cycle: `bun test` with fake entries and a fake clock. Cases:

- same instance per owner;
- concurrent first `get` opens once;
- a failed open is retried;
- an idle entry closes;
- **a streamed response held open past `minIdleMs` and `idleMs`, with the
  cap exceeded, is not evicted, and it is evictable once the body ends**;
- a cancelled body and a thrown `run` both release the lease;
- **an idle owner with a parked write survives the sweep, and the next
  request under a live token commits it.** This uses a real
  `CompassAuthCredentialStore` over a fake door (door record requirement);
- the cap evicts LRU entries only when they are idle, unleased and have no
  pending writes;
- `close()` closes all entries.

### F3 — Compass boot (fork)

Lane: compass-gateway.

```ts
// packages/coding-agent/src/cli/gateway-boot.ts
export interface GatewayBootConfig {
	serviceTokenFile: string; serverUrl: string; caFile?: string;
	bind: string; drainMs: number; enrollTokenFile?: string;
}
export function getGatewayBootConfig(env: Readonly<Record<string, string | undefined>>): GatewayBootConfig;
export function bundledGatewayModels(): Map<string, Model<Api>>; // keyed provider/id only
export async function runGatewayBoot(env?: Readonly<Record<string, string | undefined>>): Promise<void>;
// auth-gateway-cli.ts: `export const GATEWAY_MODEL_KINDS` (was module-private)
```

`runGatewayBoot` steps:

1. Read the config.
2. Read the token, the CA and the optional enroll token.
3. `installGlobalProxyFetch()`.
4. Build the compass fetch.
5. `probeCompassDoor`.
6. `startAuthGateway({ bind, authorize, storage: placeholder, resolveStorage, routeScope, resolveModel, listModels, version })`.
7. `registerGatewayShutdown(handle, drainMs, () => pool.close())`.

`storage` is still required by `AuthGatewayRouteOptions`. The placeholder is
an `AuthStorage` over a store that throws on every call, so any accidental
use answers 500 and logs. Delete the "still requires OMP_AUTH_BROKER_URL"
comment.

Test cycle: extend `packages/coding-agent/test/cli/gateway-boot.test.ts`.

- Each fail-before-bind rule throws before any listen.
- The model map holds only routable kinds and no bare ids.
- An end-to-end boot against a fake Server on loopback `http`:
  - `/healthz` → 200;
  - a good token → `/v1/models` 200;
  - a bad token → 401;
  - the Server answering `internal` → 503;
  - a bare model id → 404;
  - a chat request for a provider absent from the pool → 401.
- The fork gateway suite stays green.

### S1 — Service bearer ensure (stack)

Lane: compass-server (owner of `go/internal/stack/adapters`; reviewer of
S1, S2 and P1).

```go
// go/internal/runnerhub/mint.go
func TokenStatus(ctx context.Context, r TokenHashResolver, token string, want store.Subject) (TokenState, store.Subject, error)
// RunnerTokenStatus becomes TokenStatus(ctx, r, token, store.Subject{Kind: store.SubjectRunner, ID: runnerID})
// go/internal/stack/deps.go
type ServiceTokenEnsurer interface {
	EnsureServiceToken(ctx context.Context, path, serviceID string) error
}
// Deps gains: ServiceTokens ServiceTokenEnsurer
// go/internal/stack/adapters/token.go
func (e *TokenEnsurer) EnsureServiceToken(ctx context.Context, path, serviceID string) error
```

`TokenStatus` compares only `Kind` and `ID`. `ensure` takes the want subject,
a keep-revoked flag and a register func. The runner keeps
`StoreRunnerTokenHash`; the service path calls `PutTokenHash` with
`sha256(token)`. `cmd/compass-stack/main.go` wires one
`adapters.NewTokenEnsurer(cfg.DatabaseDSN)` into both fields.

Test cycle: `token_test.go` against the fake store:

- absent → mint, file first;
- registered → kept;
- unknown → re-registered;
- revoked → rotated;
- other subject → rotated;
- hash commit fails → file removed;
- mode 0600;
- the runner cases stay green.

### S2 — Gateway spec, network and order (stack)

Lane: compass-server. Depends on S1.

```go
// go/internal/stack/gateway_container.go
const (
	GatewayServiceTokenMountPath = "/run/compass/service.token"
	GatewayCAMountPath           = "/run/compass/server-ca.crt"
)
type GatewayContainerSpec struct { /* existing fields */ ServerURL string; NetworkArgs []string; CAFile string }
func gatewayContainerSpec(cfg Config, listenAddr string, cert CertResult) (GatewayContainerSpec, error)
```

- **Spec.** `TokenFile` becomes `<state>/gateway/service.token`. Env as in
  the table. A `listenAddr` host outside the Door listen host set errors.
- **Run args.** `gatewayRunArgs` appends `NetworkArgs`
  (`--network pasta:-T,<port>` on Linux) and mounts the token at
  `GatewayServiceTokenMountPath` and the CA at `GatewayCAMountPath` (`:ro,z`;
  the host server reads the same CA file).
- **Start.** `Stack.startGateway` removes the old `gateway.token`, then calls
  `ServiceTokens.EnsureServiceToken(ctx, spec.TokenFile, auth.LLMGatewayServiceID)`,
  then `GatewayContainer.Start`. `ensureGatewayToken` is deleted.
- **Order.** `spawnChain` moves the gateway after `waitReady`;
  `drainChildren` order becomes runner, gateway, server.

Test cycle:

- `gateway_container_test.go`: spec env, mounts and run args pinned; a
  DNS hostname (`compass.example`) and a non-loopback IP both error;
  `127.0.0.1`, `localhost`, `0.0.0.0` and `::` are accepted.
- Harness: server ready before gateway start; gateway drained before server;
  service token ensured and old file removed before the container starts.
- `downdetached` tests stay green.

### I1 — Fork pin, image env and smoke (image lane)

Lane: compass-gateway.

- Bump `tools/gateway-image/fork-pin.json` from `57f41121ff5e…` to the
  `rigel-release` commit holding F1–F3.
- Drop the `COMPASS_GATEWAY_TOKEN_FILE` `ENV` and its allowlist entry.
- Rewrite `smoke.ts`:
  - drop the broker;
  - run a Bun fake Server on host loopback `http` that answers
    `VerifyAgentToken` (`invalid agent token` for `""`) and an empty
    `ListCredentialPool`;
  - boot with `--network pasta:-T,<fake port>`;
  - assert `/healthz` 200, `/v1/models` with the good token `modelsListOk`,
    and a bad token 401;
  - fault `no-server-env` (replaces `no-broker-env`) exits before `/healthz`
    answers.

Test cycle: `core.test.ts`, plus the lane's `smoke` task on local podman.

### P1 — Default image pin (stack)

Lane: compass-server. Depends on S2, I1's digest and RIG-5204.

Set `DefaultGatewayImage` (`go/internal/stack/gateway_image.go`) to I1's
`repo@sha256:…`.

Test cycle: one stack podman test that boots the bundled gateway with no
`--gateway-external` and gets 200 on `/v1/models`. Its agent token is minted
through `auth.NewGatewayTokens(st).Mint` directly. No production code calls
`Mint` today; it is used only by tests. So until T5, no real agent sends
traffic through the gateway, and this test is not end-to-end proof.

## Tasks

- [ ] S0 — compass-server: Go test pins `invalid agent token` as the
      `VerifyAgentToken` wire contract.
- [ ] S1 — compass-server: `TokenStatus` over `store.Subject`;
      `EnsureServiceToken` for `llm-gateway`.
- [ ] S2 — compass-server: `service.token` + delete `gateway.token`; env,
      mounts, `pasta:-T`; loopback-covering door host; gateway after server
      ready; drain gateway before server.
- [ ] F1 — compass-gateway: `createCompassAgentVerifier` +
      `probeCompassDoor`.
- [ ] F2 — compass-gateway: `CompassOwnerPool` (single-flight open, leases,
      `hasPendingWrites` pin, idle sweep, soft cap, close) + stream and
      parked-write tests.
- [ ] F3 — compass-gateway: compass `runGatewayBoot` (env contract, door
      probe, `provider/id` catalog, throwing placeholder, CA-scoped fetch).
- [ ] I1 — compass-gateway: fork pin bump, image env cleanup, broker-free
      smoke.
- [ ] P1 — compass-server: pin `DefaultGatewayImage`.
- [ ] Ledger: claim these with `bun tools/dl-claim` when this record's PR is
      ready. The design pass does not claim them.
  - DL (server): the stack's `llm-gateway` service bearer is
    `<state>/gateway/service.token`. The compass boot admits agents only
    through `GatewayAuth.VerifyAgentToken`, with no inbound shared token.
    Record: `#approach`.
  - DL (server): the compass boot serves the bundled catalog by
    `provider/id` until stable-name P1. Record: `#approach`. Claim after
    Matt rules on OQ1.

## Open Questions

**OQ1 (load-bearing): catalog under per-owner pools.**

- **A (recommended):** every bundled provider, indexed by `provider/id`, with
  no credential gating. A missing provider gets a dispatch 401.
- **B:** a per-owner `ModelRegistry`. Needs a caller-aware `ModelResolver`, a
  router-core change.
- **C:** a union over live pools. It leaks between owners and is
  nondeterministic.
- **D:** A, limited to the parent record's day-1 set ("anthropic (OAuth +
  api_key), openai/openai-codex (OAuth + api_key), google (api_key)"). This
  bounds `/v1/models`, but it turns a new provider into a code change. Parent
  RD-6 says "Additional providers are configuration, not code".
- **E:** read the Server registry now (`GetGatewayModelRegistry` is mounted,
  `go/server/gateway_registry_service.go`). Stable-name P1 owns that swap and
  plans an async, caller-aware `resolveModel`. Today `ModelResolver` is sync,
  so E would need a boot-time fetch, a version-poll timer and a
  registry-to-`Model` mapping. That duplicates P1 before P1 lands.

**Recommendation: A.** Decision needed: accept A (or D) as the interim
catalog.

**OQ2 (load-bearing): drop the inbound shared bearer.** The record admits no
shared caller. The service bearer moves to `service.token` and
`COMPASS_SERVICE_TOKEN_FILE`, and `gateway.token` is deleted. This reverses
the T1 contract ("token file = inbound bearer").

**Recommendation: drop it.** Decision needed: confirm there is no operator or
debug use for a shared inbound token.

**OQ7 (load-bearing): reaching the Server on darwin.** On darwin, pasta runs
in the podman-machine VM, so `-T` would reach the VM's loopback, not the Mac
[INFERENCE; not probed, no Mac host].

- **A:** `https://host.containers.internal:<port>` on both OSes, with Linux
  adding `--network pasta:--map-host-loopback,169.254.1.2` (probed:
  reaches the host). Costs:
  - a DNS SAN `host.containers.internal` in `certHosts`;
  - a SAN check in `needsRotation`, which today checks only the key, parsing
    and expiry, so existing anchors rotate once.
- **B:** per OS: `-T` and `127.0.0.1` on Linux, `host.containers.internal` on
  darwin. This still needs A's SAN and adds a second path to test.
- **C:** `-T` everywhere. The bundled gateway is unsupported on darwin, and
  the spec errors there, pointing to `--gateway-external`.

**Recommendation: A, if a darwin probe shows `host.containers.internal`
reaching a Mac `127.0.0.1` listener; otherwise C for now.** The door probe
(Approach) makes any wrong choice fail `up` loudly instead of failing every
request. Decision needed: A or C, and who runs the darwin probe before S2.

**OQ3 (not load-bearing): verifier cache.** None for now. Revisit after dev
measures VerifyAgentToken load, alongside the door-call rate (door record
Q1).

**OQ4 (resolved by design): the 401 vs 503 split depends on a message.** No
Go test pinned the literal. `TestVerifyAgentTokenInvalidIsUnauthenticated`
checks only the code, plus equal text across tokens. S0 pins the literal.
ErrorInfo was rejected (Alternatives).

**OQ5 (not load-bearing): enroll token supply.** No compass or fork code reads
or writes `COMPASS_GATEWAY_ENROLL_TOKEN_FILE`. Minting and mounting it belong
to E2/E3 of `compass-gateway-oauth-enrollment`.

**OQ6 (not load-bearing): detached down.** `drainTargets` signals the whole
consumer tier (runner, server, gateway) at once. A write-back during the
gateway's drain can park and be lost. The door record accepts this residual.
