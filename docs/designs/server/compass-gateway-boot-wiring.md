# Compass gateway boot wiring

## Problem / Intent

Tracking: RIG-2862 (remainder: boot wiring). Addendum to
[`compass-server-llm-gateway`](./compass-server-llm-gateway/design.md) §T1 and
to the §Global Constraints "Compass boot entrypoint" bullet of
[`compass-gateway-credential-door-agent-token`](./compass-gateway-credential-door-agent-token.md).

The fork's compass boot entrypoint (`packages/coding-agent/src/cli/gateway-boot.ts`
in RigelBuild/oh-my-pi) calls `runAuthGatewayCommand({ action: "serve" })`.
`runServe` (`packages/coding-agent/src/cli/auth-gateway-cli.ts`) throws under
`if (!brokerConfig) {` with
`` "`omp auth-gateway serve` requires OMP_AUTH_BROKER_URL …" ``. So the
published image cannot serve under the stack. This record wires the
entrypoint to the Compass Server's doors:

- agent tokens are verified by `GatewayAuth.VerifyAgentToken`;
- each owner gets its own `AuthStorage` over `CompassAuthCredentialStore`;
- the stack mints, registers and mounts the `llm-gateway` service bearer that
  those door calls need.

## Approach

### Fork: a compass boot, not a reshaped `runServe`

`gateway-boot.ts` calls `startAuthGateway` itself with compass adapters.
`runServe` stays the broker path for upstream `omp auth-gateway serve`. The
boot reuses `registerGatewayShutdown` and `GATEWAY_MODEL_KINDS`, which this
change exports. The gateway adapters live in `packages/ai/src/compass/`, next
to the store.

- **Transport (`wire.ts`).** The verifier and the store share two helpers:
  - `normalizeCompassBaseUrl` replaces `assertSecureBaseUrl`. It allows https,
    or http on loopback only.
  - `fetchCompassRpcResponse` sends Connect JSON with the service bearer and
    `redirect: "error"`.
- **`agent-verifier.ts`: the authorizer.** It posts the presented bearer to
  `GatewayAuth/VerifyAgentToken`. The mapping follows the
  `AuthGatewayAuthorizer` doc (`auth-gateway/types.ts`): "`null` to answer
  401. A throw answers 503 … bound any remote call".
  - No bearer → `null`, with no call.
  - 200 → `{ kind: "agent", agentAccountId, ownerUserId }`. An empty id
    throws.
  - `unauthenticated` with message `invalid agent token` → `null`. The
    message is `errGatewayTokenInvalid` (`go/server/gateway_auth.go`:
    `var errGatewayTokenInvalid = errors.New("invalid agent token")`), and
    S0 pins it.
  - Any other result or error → throw (503). A bad service bearer gets
    `ServiceBearerInterceptor`'s message (`go/internal/auth/service_door.go`:
    `connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))`).
    Mapped to 401, a broken gateway would look like a bad agent token.
  - The deadline is `AbortSignal.any([req.signal, AbortSignal.timeout(5_000)])`.
    There is no cache (OQ3).
- **`owner-pool.ts`: one `AuthStorage` per owner.**
  `resolveStorage(caller)` calls `pool.get(caller.ownerUserId)`. A missing
  owner throws, which the router answers with 503.
  - **Open.** On first use the pool opens, per owner:
    1. `CompassAuthCredentialStore.open(...)`, with no `agentAccountId`;
    2. `new AuthStorage(store)`;
    3. `credentials.reload()`.

    The open is single-flight, and a failure is not cached. It runs inside
    `routeScope`, because `createAuthGatewayRouter`
    (`packages/ai/src/auth-gateway/server.ts`) wraps the whole body:
    `return opts.routeScope ? await opts.routeScope(req, body) : await body();`.
    So `open()` sees the agent token.
  - **Leases.** A response stream outlives `route`: `startAuthGateway` sets
    `idleTimeout: 255,`. A mid-stream OAuth refresh writes back through the
    store, and a closed store throws (`#request` in `credential-store.ts`:
    `if (this.#closed) throw new CompassRpcError("closed", "Compass credential store is closed");`).
    The rotated refresh token would then be lost. So `routeScope` runs each
    request in a lease scope, and every `get` inside it leases the entry.
    The lease ends when the body ends, errors or is cancelled. A body-less
    result or a throw ends it at once.
  - **Eviction.** A 60 s sweep closes an entry only when all of these hold:
    - it has no leases;
    - `store.hasPendingWrites()` is false (the door record's pin);
    - it has been idle 15 min.

    Over 256 owners, the sweep also closes LRU entries idle more than 60 s
    that pass the first two checks. `close()` at shutdown closes every
    entry; a parked write is lost then, a residual the door record accepts.
    There is no credential-poll timer.
- **`routeScope`.** The door record's line, plus the lease:
  `(req, run) => { const t = presentedBearer(req); const leased = () => pool.withRequestLease(run); return t ? runWithAgentToken(t, leased) : leased(); }`.
- **Catalog: bundled, `provider/id` keys only (OQ1).** The `ModelResolver`
  type `(modelId) => Model<Api> | undefined` never sees the caller, and
  `/v1/models` skips the pool. The boot indexes `getBundledModels()` for
  every bundled provider, keeping `GATEWAY_MODEL_KINDS` only. OQ1 option B
  would replace the resolver with a per-owner one.
  - **No bare ids.** `indexModelsByRequestId` would add a bare-id fallback:
    `if (!modelById.has(model.id)) modelById.set(model.id, model);`. Over all
    providers, that routes a shared bare id to whichever provider comes
    first. A bare id gets the router's
    `` formatError(404, "invalid_request_error", `Unknown model: ${modelId}`) ``.
  - A provider missing from the owner's pool gets a dispatch 401
    (`resolveGatewayApiKey`).
- **Inbound auth.** `authorize` is the verifier, wrapped in
  `withEnrollToken` when `COMPASS_GATEWAY_ENROLL_TOKEN_FILE` is set (OQ5).
  `bearerTokenAuthorizer` is not used, and no shared inbound token remains
  (OQ2, resolved).
- **Outbound TLS.** The store and the verifier share one fetch,
  `fetch(input, { ...init, tls: { ca } })`, with `ca` read from
  `COMPASS_SERVER_CA_FILE`. Provider calls keep the default trust store.
- **Door probe before bind.** `/healthz` answers 200 with no checks, so
  `waitGateway` cannot see a broken door. The boot calls `VerifyAgentToken`
  with `""` and exits non-zero unless the answer is `invalid agent token`.
  `GatewayTokens.Verify` (`go/internal/auth/gateway_token.go`) returns that
  before any store read:
  `if !wellFormedToken(token) { return store.GatewayCaller{}, ErrGatewayTokenInvalid }`.
  The probe proves reachability, TLS and a live service bearer, and it reads
  no credentials.

### Stack: service bearer, server address, start order

- **Service bearer.** `<state>/gateway/service.token` (0600), mounted as
  `COMPASS_SERVICE_TOKEN_FILE`. S2 deletes the old `gateway.token`, which
  `ensureGatewayToken` filled with an unregistered hex secret:
  `os.WriteFile(path, []byte(hex.EncodeToString(raw[:])), 0o600)`. A new
  name means skew fails before bind, never by adopting that secret. S1 makes
  the token status check generic over `store.Subject`, because
  `RunnerTokenStatus` (`go/internal/runnerhub/mint.go`) hardcodes
  `if subj.Kind == store.SubjectRunner && subj.ID == runnerID {`. The rules
  for subject `{SubjectService, "llm-gateway"}`:
  - registered → keep;
  - unknown → re-register;
  - revoked or another subject → mint a fresh one, file first, then
    `PutTokenHash`, and remove the file if the commit fails.

  Residuals:
  - a revoke mid-run means 503 until the next `up`;
  - an external database plus a wiped state dir leaves old hashes live, as on
    the runner path.
- **Server address (OQ7-A, resolved).** The container dials
  `https://host.containers.internal:<door port>` on both OSes and runs with
  `--network pasta:--map-host-loopback,169.254.1.2`. Both mappings land on
  the host's `127.0.0.1` (probe table under Resolved decisions):
  - Linux: by default pasta does not forward container loopback
    (`pastaHostGateway` in `go/e2e/fixture.go`: "pasta does not forward
    127.0.0.1"). The map reaches a host `127.0.0.1` listener, and `-p` still
    publishes.
  - darwin: the name resolves to `192.168.127.254`, which reaches a Mac
    `127.0.0.1` listener under the same pasta spec, and `-p` still
    publishes from that container.
  - TLS: the name needs a DNS SAN. Today the SANs are
    `var certHosts = []string{"127.0.0.1", "::1", "localhost"}`
    (`go/internal/stack/adapters/cert.go`), so the Linux probe failed the
    hostname check. `needsRotation` checks only the key, the parse and the
    expiry (`return !notAfter.After(now.Add(e.rotateWindow))`). S2 adds the SAN
    and makes a missing `certHosts` SAN trigger rotation. Every existing
    anchor then rotates once, on the expiry-rotation path.
  - `<state>/tls.crt` is mounted read-only as the CA.
- **Door host.** `overlapsLoopbackPublish` (`go/internal/stack/config.go`)
  is too wide here, because it also passes every DNS name:
  `return ip == nil || ip.IsUnspecified() || ip.Equal(net.IPv4(127, 0, 0, 1))`.
  S2 has its own rule (Global Constraints).
- **Start order.** `spawnChain` (`stack.go`) starts the gateway at step 1e,
  before the TLS anchor and the server. So on a first run there is no CA file
  and no store to register the token in. The gateway moves to after
  `waitReady` and before `startRunner`. `drainChildren` uses a hard-coded
  list (runner, server, llm-gateway, …), so S2 reorders it to runner →
  gateway → server. That gain is small: agents are gone by then, and T5
  revokes their tokens on stop. Detached down is unchanged (OQ6).

## Alternatives considered

- **A `--compass` mode in `runServe`.** This mixes broker and compass wiring
  in one upstream command. Rejected.
- **A bare-id index over all providers.** It misroutes shared bare ids.
  Rejected.
- **Keeping the shared bearer as a second caller class (OQ2).** Its holder
  would get a pool-less caller. Rejected.
- **Reusing `gateway.token` for the service bearer.** It would adopt the old
  inbound secret, and skew would stay silent. Rejected.
- **`--network host`.** It drops `-p` and network isolation. Rejected.
- **`--network pasta:-T,<door port>` with `127.0.0.1`.** Linux only. On
  darwin it reaches the VM's loopback ("Connection reset by peer"). Rejected
  (OQ7).
- **An ErrorInfo reason on `VerifyAgentToken`.** The verifier would have to
  decode error details from Connect JSON. Rejected for S0's literal pin.
- **A verify cache.** A revoked token would keep working. Deferred (OQ3).

## Global Constraints

- **Public repo.** This record and every compass task cite only compass and
  RigelBuild/oh-my-pi.
- **Fork changes stay seam-shaped.** Allowed:
  - `packages/ai/src/compass/` (new modules, plus F1's `wire.ts` helpers and
    the matching `credential-store.ts` transport refactor);
  - `gateway-boot.ts`;
  - one export from `auth-gateway-cli.ts`;
  - option types and call sites in `dispatch.ts` and `server.ts`, for F2's
    `routeScope` narrowing;
  - only if OQ1 picks B: the caller-aware `resolveModel` in `dispatch.ts`,
    `server.ts` and the seven `routes/` handlers (embeddings, images,
    rerank, speech, systemone, transcriptions, video). Each
    `bootOpts.resolveModel(...)` call receives the admitted caller and awaits
    the result. This is an injection-point change, not routing core. A
    caller-aware `listModels` (B2) also needs a stable-name amendment first.

  No routing-core change, and no change to `auth-storage.ts` or
  `packages/ai/src/auth/`.
- **Fork base.** `rigel-release` with RIG-5205 (`routeScope`, `d0ac08ac56cc`).
  #146 (RIG-5206) is based on it.
- **Server dependency.** A default-pinned image needs RIG-5204, because on main
  `ListCredentialPoolRequest` still requires `agent_account_id`.
- **Host floor (OQ7-A).** The bundled gateway needs passt 2024-08-21 or newer,
  which added `--map-host-loopback`. Ubuntu 24.04's passt lacks it, and
  26.04's has it (Resolved decisions). Below the floor, `up` fails at S2's
  preflight; `--gateway-external` still works. The existing
  `hostcheck.MicroVMFloors` passt floor (`2025_09_19`) covers only the
  microVM backend's `passt` on PATH, not podman's pasta.
- **Fail before bind.** The boot exits non-zero and never binds when:
  - `COMPASS_SERVER_URL` is unset or fails `normalizeCompassBaseUrl`;
  - an https URL has no readable CA file;
  - the service-token file is unset, empty or holds whitespace (the error
    names `COMPASS_SERVICE_TOKEN_FILE`);
  - an enroll-token file is set but invalid;
  - the door probe fails.
- **Inbound transport is loopback only.** The stack publishes the listener only
  on host `127.0.0.1:4100` (`go/internal/stack/gateway_container.go`:
  `gatewayHostEndpoint = "127.0.0.1:4100"`). So in this record agent tokens
  cross loopback only, and the listener has no TLS. Parent OQ-4 ("TLS via the
  stack's existing anchor when bound beyond loopback") applies when T5 exposes
  the gateway to agent containers, and T5 owns it.
- **Secrets never logged.** No token appears in a log line or an error; only
  `errorName(error)` is logged.
- **Env contract.** Passed with `-e`, never as image `ENV`. I1 drops the
  `COMPASS_GATEWAY_TOKEN_FILE` `ENV` and its allowlist entry.

  | Name | Value | Supplier |
  |---|---|---|
  | `COMPASS_SERVICE_TOKEN_FILE` | `/run/compass/service.token` | S2 |
  | `COMPASS_SERVER_URL` | `https://host.containers.internal:<door port>` | S2 |
  | `COMPASS_SERVER_CA_FILE` | `/run/compass/server-ca.crt` | S2 |
  | `COMPASS_GATEWAY_BIND` | `0.0.0.0:4000` | existing |
  | `COMPASS_GATEWAY_DRAIN_MS` | `20000` | existing |
  | `COMPASS_GATEWAY_ENROLL_TOKEN_FILE` | optional | E2/E3, later |

- **Door listen host.** Both host mappings deliver to the host's IPv4
  `127.0.0.1`. The door binds through `Stack.listenTCP` (`stack.go`), which
  calls `net.ResolveTCPAddr` and then `net.ListenTCP`. S2 accepts only hosts
  that accept on `127.0.0.1`:
  - `127.0.0.1`;
  - `localhost`, which resolves IPv4 first (`forResolve` in Go's
    `net/ipsock.go`: "IPv4 is preferred, unless addr contains an IPv6
    literal");
  - `0.0.0.0`, `::` and an empty host (`:port`), which bind a wildcard.

  Any other host, including `::1`, `127.0.0.2` and every DNS name, errors and
  names `--gateway-external`.
- **Order.** Each line is linear:
  - compass: S0 → S1 → S2 → P1;
  - fork: #146 → F1 → F2 → F3.

  I1 follows F3's merge to `rigel-release`. P1 also waits on I1 and RIG-5204.
  No Pulumi state is involved.

## Plan

The Plan is the long section, because it carries the exact interfaces each
lane builds against.

### S0 — Pin the verify message

Lane: compass-server (owns `go/server`). Add a test in
`go/server/gateway_auth_test.go` that asserts code `Unauthenticated` and
message `invalid agent token`, and names the gateway as consumer.

### F1 — Verifier and shared transport (fork #150)

Lane: compass-gateway. Depends on #146.

```ts
// packages/ai/src/compass/wire.ts
export function normalizeCompassBaseUrl(baseUrl: string): string; // replaces assertSecureBaseUrl
export function fetchCompassRpcResponse(opts: { baseUrl: string; service: string; method: string; token: string; fetch: typeof fetch; body: unknown; signal: AbortSignal }): Promise<{ response: Response; text: string }>;
// packages/ai/src/compass/agent-verifier.ts
export interface CompassAgentVerifierOptions { baseUrl: string; token: string; fetch?: typeof fetch; timeoutMs?: number }
export function createCompassAgentVerifier(opts: CompassAgentVerifierOptions): AuthGatewayAuthorizer;
export function probeCompassDoor(opts: CompassAgentVerifierOptions): Promise<void>;
// credential-store.ts: #request uses both helpers.
```

Tests cover each mapping row above, abort, the probe, and that no token
appears in an error.

### F2 — Owner pool

Lane: compass-gateway.

```ts
// packages/ai/src/compass/owner-pool.ts
export interface CompassOwnerEntry { storage: AuthStorage; store: { hasPendingWrites(): boolean } }
export interface CompassOwnerPoolOptions { open: (ownerUserId: string) => Promise<CompassOwnerEntry>; maxOwners?: number; idleMs?: number; minIdleMs?: number; sweepIntervalMs?: number; now?: () => number }
export class CompassOwnerPool {
	get(ownerUserId: string): Promise<AuthStorage>;
	withRequestLease(run: () => Promise<Response>): Promise<Response>;
	// F2 narrows AuthGatewayRouteOptions.routeScope to Response to match.
	sweep(): void;
	close(): void;
}
export function openCompassOwner(opts: CompassAuthCredentialStoreOptions): Promise<CompassOwnerEntry>;
```

Required tests:

- **Two-owner isolation.** Each owner gets the same instance on repeat calls,
  and the two owners get different instances. Each selects only its own
  credential.
- **Lease.** A stream held past both idle limits, with the cap exceeded,
  survives. The lease is released, and the owner becomes evictable, when the
  body ends, when it errors, and when it is cancelled. #151 already has these
  cases.
- **Parked write.** An idle owner with a parked write survives the sweep. The
  next live-token request commits it, using a real store over a fake door.
- **Basics:** single-flight open, retried after a failure, and closed by
  `close()`.

### F3 — Compass boot

Lane: compass-gateway.

```ts
// packages/coding-agent/src/cli/gateway-boot.ts
export interface GatewayBootConfig { serviceTokenFile: string; serverUrl: string; caFile?: string; bind: string; drainMs: number; enrollTokenFile?: string }
export function getGatewayBootConfig(env: Readonly<Record<string, string | undefined>>): GatewayBootConfig;
export function readTokenFile(path: string, envName: string): Promise<string>; // replaces readGatewayToken
export function bundledGatewayModels(): Map<string, Model<Api>>;
export function runGatewayBoot(env?: Readonly<Record<string, string | undefined>>): Promise<void>;
```

Order: config → token files and CA → `installGlobalProxyFetch()` →
`probeCompassDoor` → `startAuthGateway(...)` → `registerGatewayShutdown(handle, drainMs, () => pool.close())`.
The required `storage` option gets a placeholder `AuthStorage` over a store
that throws on every call.

Tests:

- each fail-before-bind rule;
- the model map holds no bare ids;
- an end-to-end boot against a fake Server: good token → 200, bad token →
  401, Server `internal` → 503, bare id → 404, missing provider → 401.

### S1 — Service bearer ensure

Lane: compass-server (owns `go/internal/stack/adapters`; reviews S1, S2, P1).

```go
func TokenStatus(ctx context.Context, r TokenHashResolver, token string, want store.Subject) (TokenState, store.Subject, error) // runnerhub; RunnerTokenStatus wraps it
type ServiceTokenEnsurer interface{ EnsureServiceToken(ctx context.Context, path, serviceID string) error } // stack.Deps.ServiceTokens
func (e *TokenEnsurer) EnsureServiceToken(ctx context.Context, path, serviceID string) error // adapters
```

`EnsureServiceToken` creates the token's directory at 0700 before its first
write. Tests:

- each Approach rule;
- the file is removed when the commit fails;
- mode 0600;
- **fresh state:** with no `<state>/gateway/`, the call creates it at 0700
  and mints the token;
- the runner cases stay green.

### S2 — Gateway spec, network and order

Lane: compass-server. Depends on S1.

```go
const GatewayServiceTokenMountPath, GatewayCAMountPath = "/run/compass/service.token", "/run/compass/server-ca.crt"
const gatewayServerHost, gatewayHostLoopbackAddr = "host.containers.internal", "169.254.1.2"
type GatewayContainerSpec struct { /* existing */ ServerURL string; NetworkArgs []string; CAFile string }
func gatewayContainerSpec(cfg Config, listenAddr string, cert CertResult) (GatewayContainerSpec, error)
// adapters/cert.go
var certHosts = []string{"127.0.0.1", "::1", "localhost", "host.containers.internal"}
func (e *CertEnsurer) needsRotation(certPath, keyPath string, now time.Time) bool // also true when a certHosts SAN is missing
```

- `TokenFile` becomes `<state>/gateway/service.token`, mounted at
  `GatewayServiceTokenMountPath`. `GatewayTokenMountPath` and the
  `COMPASS_GATEWAY_TOKEN_FILE` env are removed.
- The spec sets the env table and `NetworkArgs`
  (`--network pasta:--map-host-loopback,169.254.1.2`).
- Mount labels:
  - the service token uses `:ro,Z`, a private label, like today's
    `spec.TokenFile+":"+stack.GatewayTokenMountPath+":ro,Z"` in
    `gatewayRunArgs`;
  - the CA uses `:ro,z`, a shared label, because the host server reads the
    same `tls.crt`.
- `startGateway` runs `MkdirAll(TokenDir, 0o700)`, removes `gateway.token`,
  then calls `EnsureServiceToken`, then `Start`.
- `drainChildren` order becomes runner, llm-gateway, server. Detached
  `liveTargets` keeps its order, and `drainTargets` still signals the
  consumer tier together.
- `ensureGatewayToken` is deleted.
- The gateway step moves as in the Approach.
- **Network preflight.** Before `EnsureServiceToken`, `startGateway` runs a
  throwaway `podman run --rm` of the gateway image with the S2 `NetworkArgs`
  and `--version`, which tini (the image's `ENTRYPOINT` in `Dockerfile.gateway`)
  answers. This is a new `GatewayContainer.CheckNetwork(ctx, spec) error`.
  On failure, `up` fails with an error that names passt 2024-08-21,
  `--map-host-loopback` and `--gateway-external`, and quotes the first two
  lines of podman's stderr (pasta's `unrecognized option` line; its full
  usage text runs to about 150 lines). The run tests the option itself, so
  it works through the darwin machine too, with no version string to parse.

Tests:

- the argv test pins the service-token mount (`:ro,Z`) and the CA mount
  (`:ro,z`), and finds no `COMPASS_GATEWAY_TOKEN_FILE`;
- `::1`, `127.0.0.2` and `compass.example` error. `127.0.0.1`, `localhost`,
  `0.0.0.0`, `::` and an empty host (`:port`) pass;
- `cert_test.go`: a fresh anchor carries the DNS SAN. An unexpired anchor
  without it rotates once, and the next ensure keeps it;
- harness: on fresh state, server ready → token → container start, and the
  drain stops the gateway before the server.
- a fake podman that rejects `--map-host-loopback` fails `up` with that
  error, before the token is ensured or the container starts;

### I1 — Fork pin, image env and smoke

Lane: compass-gateway.

- Bump `fork-pin.json` from `57f41121ff5e…` to the `rigel-release` commit
  that holds F1–F3.
- Drop the old `ENV` and its allowlist entry.
- `smoke.ts` drops the broker. It runs a fake Server on the host's
  `127.0.0.1` over https, with a throwaway cert for `host.containers.internal`
  (`compass-gen-cert -hosts`) mounted as the CA. It boots with the S2 network
  spec and asserts `/healthz`, `/v1/models` for a good token, 401 for a bad
  token, and a `no-server-env` fault.
- `release.yml` `publish-gateway-image` runs the smoke on
  `runs-on: ubuntu-latest`. Pin it to `runs-on: ubuntu-26.04`, because
  24.04's passt lacks `--map-host-loopback` and `ubuntu-latest` moves to
  26.04 only gradually (Resolved decisions). The alternative is a nix passt
  on PATH. Pinning is recommended.

### P1 — Default image pin

Lane: compass-server. Set `DefaultGatewayImage` to I1's digest. A podman
test boots the bundled gateway and gets 200 on `/v1/models`. Its token comes
from `auth.NewGatewayTokens(st).Mint` directly: only tests call `Mint` until
T5, so this test is not end-to-end proof.

## Tasks

- [ ] S0 — compass-server: pin `invalid agent token`.
- [ ] S1 — compass-server: `TokenStatus`; `EnsureServiceToken` (makes its dir).
- [ ] S2 — compass-server: spec, mounts, `map-host-loopback`, SAN and
      one-time rotation, door-host rule, order.
- [ ] F1 — compass-gateway: `wire.ts` transport, verifier, door probe (#150).
- [ ] F2 — compass-gateway: owner pool and its required tests.
- [ ] F3 — compass-gateway: compass `runGatewayBoot`, `readTokenFile`.
- [ ] I1 — compass-gateway: pin bump, image env, broker-free smoke.
- [ ] P1 — compass-server: pin `DefaultGatewayImage`.
- [ ] Ledger: claim these with `bun tools/dl-claim` when this PR is ready;
      the design pass does not claim them.
  - DL (server): the `llm-gateway` service bearer is
    `<state>/gateway/service.token`. Agents are admitted only through
    `VerifyAgentToken`; the optional enroll token admits only the enroll
    caller, on `/internal/enroll/*`.
  - DL (server): the bundled gateway reaches the Server at
    `host.containers.internal` through `pasta:--map-host-loopback`, and the
    stack anchor carries that DNS SAN (OQ7).
  - DL (server): the boot's catalog per the OQ1 ruling. Claim after OQ1.

## Resolved decisions

**OQ2: drop the inbound shared bearer (Matt, 2026-10-12).** This reverses
the T1 contract agreed in the RIG-2862 comment of 2026-09-29, under which
the boot loaded the token file `/run/compass/gateway.token` as the single
inbound bearer. Agents are now admitted only through `VerifyAgentToken`.

**OQ7: A, `host.containers.internal` on both OSes (Matt, 2026-10-12).**
One network spec, `--network pasta:--map-host-loopback,169.254.1.2`, with the
DNS SAN and the one-time rotation from S2. Option C (bundled gateway
unsupported on darwin) was the fallback. It is moot, because the darwin probe
passed.

Probe evidence. Each probe targets a host listener on `127.0.0.1`.

| Host | Network | Target | Result |
|---|---|---|---|
| Linux, rootless podman 5.8.6 | default | `127.0.0.1` | refused |
| Linux | `pasta:-T,<port>` | `127.0.0.1` | reached; `-p` publishes |
| Linux | `pasta:--map-host-loopback,169.254.1.2` | `host.containers.internal` | reached (cert failed: no SAN); `-p` publishes |
| mattmini, macOS arm64, podman 5.8.6, applehv | default | `host.containers.internal` → `192.168.127.254` | reached |
| mattmini | `pasta:--map-host-loopback,169.254.1.2` | `host.containers.internal` → `192.168.127.254` | reached |
| mattmini | `pasta:-T,51999` | `127.0.0.1` | "Connection reset by peer" |
| mattmini | default, `-p 127.0.0.1:51998:80` | host → container | reached |
| mattmini | `pasta:--map-host-loopback,169.254.1.2`, `-p 127.0.0.1:51998:8080` | host → container, and same container → `host.containers.internal` | both reached |

The Mac listener was `python3 -m http.server 51999 --bind 127.0.0.1`. The
probe machine was removed afterwards.

Host floor evidence. A holds, with a floor:

- `--map-host-loopback` landed in upstream passt on 2024-08-21 (commit
  e813a4df, "conf: Allow address remapped to host to be configured").
- Ubuntu noble (24.04) ships passt `0.0~git20240220`, and its pasta(1) has
  no `--map-host-loopback`. Resolute (26.04, passt 20260120) has it
  (manpages.ubuntu.com `noble`/`resolute` `man1/pasta.1`;
  launchpad.net/ubuntu/+source/passt).
- GitHub `ubuntu-latest` moves to 26.04 gradually, from 2026-10-19 to
  2026-11-19 (github.blog changelog, 2026-09-17).
- This box's podman 5.8.6 bundles `pasta 2026_07_16`, whose `--help` lists
  the option.

## Open Questions

**OQ1 (load-bearing): the catalog under per-owner pools.**

- **A (recommended):** every bundled provider by `provider/id`, with a
  dispatch 401 when a credential is missing.
- **B:** a per-owner `ModelRegistry`, built in the owner pool, behind the
  `resolveModel` widening that the stable-name record already ratified
  (Global Constraints, "Fork changes are seam-shaped"):
  `(modelId, identity) => Promise<Model<Api> | undefined>`. That record keeps
  `listModels?: () => Iterable<Model<Api>>` caller-independent and lists the
  stable-name vocabulary on it. Two variants:
  - **B1 (recommended if B):** the resolver only, pulled forward from
    stable-name P1, with the listing unchanged. No amendment is needed.
    Cost: the seam change in `dispatch.ts`, `server.ts` and seven route
    handlers (Global Constraints), plus one caller-specific non-chat route
    test (embeddings). Dispatch is per owner, but `/v1/models` is not: a
    listed model the owner holds no credential for gets a 404 from the
    resolver.
  - **B2:** B1 plus a caller-aware listing. This adds a second seam and
    changes the `/v1/models` contract that stable-name discovery relies on,
    so it needs a stable-name amendment.
- **C:** a union of live pools. It leaks between owners and is
  nondeterministic.
- **D:** A, limited to the parent's day-1 set. This makes a new provider a
  code change, against RD-6 ("Additional providers are configuration, not
  code").
- **E:** read `GetGatewayModelRegistry` now. This duplicates stable-name P1:
  a sync resolver would need a boot fetch, a poll and a model mapping.

Decision needed: B (Matt leaning) or A; if B, then B1 or B2.

**OQ3 (deferred): verify cache.** Revisit after dev measures the load.

**OQ4 (resolved): the 401 vs 503 split hangs on one message.** No test pinned
it before; S0 does now.

**OQ5 (deferred): enroll token supply.** Nothing reads or writes
`COMPASS_GATEWAY_ENROLL_TOKEN_FILE` today. Owned by E2/E3 of
`compass-gateway-oauth-enrollment`.

**OQ6 (accepted residual): detached down.** `drainTargets` signals the
runner, server and gateway together, so a write-back during the gateway's
drain can be lost.

**OQ8 (deferred to T5): inbound TLS.** The listener is published only on
host loopback (Global Constraints). T5 owns TLS once it binds beyond
loopback (parent OQ-4).
