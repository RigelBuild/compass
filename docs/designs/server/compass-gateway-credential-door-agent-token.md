# Gateway credential door: agent token as the caller

Tracking: RIG-5080 (decision), RIG-4350 (work; parent RIG-1715). Ruling: Matt,
2026-10-10, on RIG-5080: "We can just modify the gateway to suit our model."
Matt confirmed Mintaka's reading of that ruling on 2026-10-11 (§Open
Questions Q1: A, no door call with no request behind it).

Addendum to the merged record
[`compass-server-llm-gateway`](./compass-server-llm-gateway/design.md)
(§Gateway architecture on the stack, §T2, §T3). It replaces option A of the
RIG-4346 ruling ("A then B") with option B.

## Problem / Intent

The gateway credential door (`service GatewayCredentials` in
`proto/compass/v1/gateway_credentials.proto`) trusts an `agent_account_id`
that the gateway puts in the request. The only proof is the `llm-gateway`
service bearer, so a stolen service token can name any agent and read that
agent's owner's plaintext credentials. This record makes every door call carry
the gateway token of the agent whose request caused it. The Server takes the
agent, owner and tenant from that token's row.

## Approach

**One identity rule.** A door call needs two tokens. The `llm-gateway`
service bearer in `Authorization` proves the caller is the gateway. The new
`agent_token` field says on whose behalf it calls; the door checks it with
`auth.TokenMinter.Verify`. Neither token alone opens the door. The service
bearer stays because an agent container can reach the network door, and an
agent token alone would let an agent read plaintext credentials, which the T5
egress cutover exists to stop.

**Agent → owner → tenant comes from one row.** `gateway_tokens` already stores
`agent_account_id`, `owner_user_id` and `tenant_id` (`0001_init.sql`:
"Gateway tokens are looked up before a tenant is known; store only their
hashes."). The table is outside the RLS `tenant_tables` list and is granted to
`compass_app`, so the lookup needs no system role. Today
`Store.ResolveGatewayToken` drops the tenant:

```go
return GatewayCaller{AgentAccountID: AccountID(row.AgentAccountID), OwnerUserID: AccountID(row.OwnerUserID)}, nil
```

`store.GatewayCaller` gains `TenantID`, and the `ResolveLiveGatewayToken`
query selects `tenant_id` (the DL-381 shape). The door then calls
`store.WithTenant(store.WithoutSystemRole(ctx), caller.TenantID)` and passes
`caller.AgentAccountID` to the unchanged `gatewaycred` calls
(`PoolResolver.Pool`, `CredentialStore.UpdateOAuth`, `CredentialStore.Disable`).
These already check, under RLS, that the credential is in the agent's owner
pool, so a token of owner X cannot touch owner Y's rows.

This deletes the door's system-role lookup, `Store.GatewayAgentTenant`
(`s.q.GatewayAgentTenant(WithSystemRole(ctx), string(agent))`), which is
named in the `WithSystemRole` allowlist comment in
`go/internal/store/tenant_tx.go`. The request path loses one BYPASSRLS
entrypoint (DL-315).

**`agent_account_id` is removed, not deprecated.** Each request message
reserves its number and name and gains
`string agent_token = N [debug_redact = true]`, the form of
`VerifyAgentTokenRequest.token` in `gateway_auth.proto`. No client depends on
the old field:

- the stack pins no gateway image (`DefaultGatewayImage = ""` in
  `go/internal/stack/gateway_image.go`);
- the image lane's fork pin (`tools/gateway-image/fork-pin.json`, commit
  `76015c2d80`) predates the compass credential store (oh-my-pi#115, merge
  `333ed430c9`);
- the buf breaking gate is off before GA (`buf.yaml`: "RE-ADD AT GA / first
  pinned client (tracked: RIG-1951)").

"Ignore, then remove" would keep option A's hole open while no client needs
it.

**The gateway carries the triggering token.** The per-owner `AuthStorage`
stays as it is. `resolveStorage(caller)` still keys on
`AgentCaller.ownerUserId`. What changes is the compass store under it,
`CompassAuthCredentialStore` (`packages/ai/src/compass/credential-store.ts`
in RigelBuild/oh-my-pi). Today it is bound to one agent and one token for its
whole life:

```ts
this.#token = opts.token;
this.#agentAccountId = opts.agentAccountId;
```

`AuthStorage` calls the store with no request context
(`listAuthCredentials`, `updateAuthCredential`, `pollExternalChanges`, …),
and changing those signatures is barred by the parent record ("never the
routing core"). So the token travels in a compass-owned `AsyncLocalStorage`
scope, a pattern the fork already uses
(`credentialRedactionScope = new AsyncLocalStorage<boolean>()` in
`packages/ai/src/providers/transform-messages.ts`). The scope is set at one
seam: a new optional route option that wraps each routed request, including
its `resolveStorage` call. The compass embedder fills the scope with the
request's admitted bearer, and does not wrap when there is none. In
RigelBuild/oh-my-pi, `presentedBearer` (`packages/ai/src/auth-gateway/http.ts`)
reads only `Authorization`, and `startAuthGateway`
(`packages/ai/src/auth-gateway/server.ts`) refuses any other admission:

```ts
// presentedBearer
const token = req.headers
	.get("authorization")
	?.match(/^Bearer\s+(.+)$/i)?.[1]
	.trim();
// startAuthGateway fetch handler
if (!isCallerIdentity(admitted) || (bearer === undefined && admitted !== UNAUTHENTICATED_CALLER)) {
	return withCors(json(401, { error: "unauthorized" }), req);
}
```

The store reads the scope when it starts a door call or queues a write; the
job keeps that token. Rules:

1. **No scope, no door call.** A list or reload with no token in scope is
   skipped; the store keeps its current view. A write with no token in scope
   is parked as `unsynced`, and the next request's poll resends it. No timer
   ever calls the door (Q1, ruled A).
2. **A revoked token parks the write.** Agent tokens rotate on re-placement
   and are revoked on stop (`TokenMinter.Mint` and `Revoke`), so a token can
   die between admission and the door call. The door then answers
   `UNAUTHENTICATED`. Today the store drops the write on that code, which for
   an OAuth write-back loses a refresh token the provider may already have
   rotated. The store adds `unauthenticated` to the codes that keep a write
   `unsynced`, and the next request of any of the owner's agents resends it.
   A bad service bearer also returns `UNAUTHENTICATED`, but then every list
   fails too, so the outage is loud. A list that fails `UNAUTHENTICATED`
   fails its request as today: `open()` throws, and the router answers 503.
3. **Races need no new mechanism.** All of one owner's agents share one
   `AuthStorage` in one process. Its refresh single-flight
   (`#oauthCredentialRefreshInFlight` in `packages/ai/src/auth/refresh.ts`)
   joins concurrent refreshes of one row, and the store's per-row FIFO
   (`#runWrite`) orders its writes. Across processes, `expected_version`
   still decides. The loser gets `ABORTED`, reloads and adopts, as today.

An owner store that holds an unsynced write must not be evicted from the
per-owner LRU. The parent record's stores are "constructed lazily on a
tenant's first request and idle-evicted (`close()`)". After `close()`,
`#request` throws `closed`, which is not a transient code, so the parked
write is lost. Idle eviction hits exactly the owners whose agents stopped,
which is the case that parks a write, so eviction is far more likely than a
restart. A pinned store lives with no time limit until the owner's next
request. The LRU belongs to the compass boot entrypoint (§Global
Constraints).

## Alternatives considered

- **Owner-scoped grant token (RIG-5080 option 1).** The Server mints a
  short-lived owner token at verify time. This adds a new token type with
  issuance and expiry on both sides. Rejected: both answers to Q1 keep "no
  new token type".
- **Keep recent live agent tokens per owner (option 2).** The gateway would
  hold a set of agent tokens in long-lived state, and a refresh would fail
  once none is live. Q1 answer B is a light form of this.
- **Keep option A (option 3).** A stolen service token still reads any pool.
  Rejected.
- **Agent token as the only bearer.** This drops the service bearer. An agent
  could then call the door itself and read plaintext credentials. Rejected.
- **Token in a request header.** A header would need a new redaction rule;
  a `debug_redact` field is already covered by "never log `req.Msg`".
  Rejected.
- **Thread the token through `AuthStorage` calls.** This changes routing-core
  signatures. Rejected by the parent record.
- **Embedder wraps `router.route` itself.** `startAuthGateway`
  (`packages/ai/src/auth-gateway/server.ts`) builds its router privately
  (`const router = createAuthGatewayRouter(opts);`), so the
  embedder would have to copy the admission code. Rejected for the
  `routeScope` option.

## Global Constraints

- **Fork changes stay seam-shaped** (parent record §Global Constraints). The
  fork changes only the route options in `packages/ai/src/auth-gateway/`
  (`dispatch.ts`, `server.ts`) and the compass module
  `packages/ai/src/compass/`. No change to `auth-storage.ts` or
  `packages/ai/src/auth/`.
- **An agent token is never logged.** It rides only in `agent_token` fields
  marked `[debug_redact = true]`. The door handler never logs `req.Msg`. The
  store never puts a token in a log line or an error message.
- **Door errors do not leak which check failed.** An unknown, revoked or
  malformed agent token is `UNAUTHENTICATED` `invalid agent token`, the
  `VerifyAgentToken` message. An out-of-pool credential stays the shared
  `errGatewayCredentialNotFound`. Its comment drops "an unknown agent",
  because that case is now `UNAUTHENTICATED`.
- **Field numbers.** `UpdateCredentialOAuthRequest.agent_token` is 6,
  because RIG-5084 (compass#2130) takes 5 for `clear_fields`.
- **Image pin.** The first fork pin in `tools/gateway-image/fork-pin.json`
  that contains `packages/ai/src/compass` must also contain T2 and T3. Until
  then no running gateway calls the door, so T1 and the fork tasks can land in
  any order.
- **Compass boot entrypoint (outside this record; RIG-2862 and RIG-2863).**
  It builds each owner's `AuthStorage` over a `CompassAuthCredentialStore`
  with no `agentAccountId`. It sets
  `routeScope: (req, run) => { const t = presentedBearer(req); return t ? runWithAgentToken(t, run) : run(); }`.
  Its per-owner LRU must not evict a store while `hasPendingWrites()` is
  true. That entrypoint's tests must prove it: an idle owner with a parked
  write survives the idle sweep, and the next request under a live token
  commits the write. A credential-poll timer is not needed. If one exists,
  rule 1 makes it a no-op.

## Plan

### T1 — Door takes the agent token (compass-server)

Lane: compass-server. One PR: proto, store, door and tests change together,
because removing the field breaks the generated getter the door calls.

- `proto/compass/v1/gateway_credentials.proto`: in each of the three request
  messages, remove `agent_account_id` and add `reserved <n>;` and
  `reserved "agent_account_id";` (the `reserved 2; reserved "initial_prompt";`
  precedent in `compass.proto`). Add `agent_token`. Rewrite the header line
  "the Server resolves it from agent_account_id" and the RPC and field
  comments to name `agent_token` and `UNAUTHENTICATED`. Regenerate with
  `moon run compass-proto:gen`.
- `go/internal/store`: `ResolveLiveGatewayToken` also selects `tenant_id`;
  `GatewayCaller` gains `TenantID`; regenerate with `moon run compass-go:sqlc-gen`.
  Delete `Store.GatewayAgentTenant` (`gateway_agent.go`), the sqlc query
  `GatewayAgentTenant`, `TestGatewayAgentTenantCrossTenantLookup`, and both
  `GatewayAgentTenant` mentions in the `tenant_tx.go` role comments.
- `go/server/gateway_credentials_service.go`: replace `agentTenantResolver`
  with the existing `gatewayTokenVerifier` (`gateway_auth.go`), and
  `resolveTenant` with `resolveCaller`. It keeps the service subject check,
  calls `Verify`, and returns
  `store.WithTenant(store.WithoutSystemRole(ctx), caller.TenantID)` plus
  `caller.AgentAccountID`. It keeps one Info audit line per call (procedure,
  agent, owner, tenant). `newGatewayServices` passes one
  `auth.NewGatewayTokens(st)` to both the credentials and auth services.
- No change to `classify_exhaustive_test.go`. The file stays in
  `ungatedFileDescriptors`.

Interfaces:

```proto
message ListCredentialPoolRequest {
  reserved 1;
  reserved "agent_account_id";
  string provider = 2;
  string agent_token = 3 [debug_redact = true];
}
message UpdateCredentialOAuthRequest {
  reserved 4;
  reserved "agent_account_id";
  string id = 1;
  GatewayOAuthToken token = 2;
  int64 expected_version = 3;
  // clear_fields = 5 (RIG-5084)
  string agent_token = 6 [debug_redact = true];
}
message DisableCredentialRequest {
  reserved 4;
  reserved "agent_account_id";
  string id = 1;
  string cause = 2;
  int64 expected_version = 3;
  string agent_token = 5 [debug_redact = true];
}
```

```go
// go/internal/store/gateway_tokens.go
type GatewayCaller struct {
	AgentAccountID AccountID
	OwnerUserID    AccountID
	TenantID       TenantID
}

// go/server/gateway_credentials_service.go
func newGatewayCredentialsService(tokens gatewayTokenVerifier, creds gatewaycred.CredentialStore, pool gatewaycred.PoolResolver, log *slog.Logger) *gatewayCredentialsService
func (s *gatewayCredentialsService) resolveCaller(ctx context.Context, agentToken, procedure string) (context.Context, store.AccountID, error)
```

Errors: an empty `agent_token` joins the existing required-field
`INVALID_ARGUMENT` check. `auth.ErrGatewayTokenInvalid` maps to
`UNAUTHENTICATED` `invalid agent token`. Any other `Verify` error is logged
and maps to `INTERNAL` `gateway credentials unavailable`. `VerifyAgentToken`'s
response is unchanged: the gateway never sees a tenant.

Test cycle (write first, watch fail):

- `TestGatewayTokenRotateResolveRevoke`: the resolved caller carries the
  agent's tenant.
- `gateway_credentials_service_test.go`: a fake verifier replaces
  `fakeAgentTenantResolver`. An invalid token is `UNAUTHENTICATED` and makes
  no store call. The store call's ctx carries the token's tenant.
- `TestGatewayCredentialsDoorServiceBearerAllowlist` (pgtest), over the real
  mount: a service bearer plus a live agent token of tenant B lists B's owner
  pool. After `TokenMinter.Revoke`, the same call is `UNAUTHENTICATED`. An
  agent token sent as the `Authorization` bearer, with no service bearer, is
  `UNAUTHENTICATED`. Owner X's token on owner Y's credential id is
  `NOT_FOUND`.

### T2 — Route scope seam (compass-gateway)

Lane: compass-gateway, RigelBuild/oh-my-pi, PR off `rigel-release`.

Add one optional route option. `createAuthGatewayRouter` runs its existing
route body, including the `resolveStorage` call, inside `routeScope` when it
is set. The call sits inside the existing `try`, so a throw from the wrapper
still answers 500. Without the option, behavior is unchanged.

A streamed turn outlives `route`. `handleFormatEndpoint`
(`packages/ai/src/auth-gateway/server.ts`) returns
`new Response(sseStream, {`, and `streamSimpleRequest`
(`packages/ai/src/stream.ts`) runs its producer as `void (async () => {`. On
an upstream auth failure, that producer asks the resolver for a new key
(`const nextKey = await resolveNextAuthRetryKey(retryState, apiKeyResolver, failure.error, …)`),
so a refresh write can run after `route` returns. The scope still holds:
"The store is accessible to any asynchronous operations created within the
callback" ([Node.js `AsyncLocalStorage.run`](https://nodejs.org/api/async_context.html)).
A throwaway check under Bun 1.4.2, the fork's `packageManager`, read the
`run` value in such a producer and in a `ReadableStream` `pull`, both after
`Bun.serve`'s `fetch` had returned.

Interfaces:

```ts
// packages/ai/src/auth-gateway/dispatch.ts, AuthGatewayRouteOptions
/** Wraps each routed request, including its `resolveStorage` call. */
routeScope?: <T>(req: Request, run: () => Promise<T>) => Promise<T>;
```

Test cycle: a new `packages/ai/test/auth-gateway-route-scope.test.ts`.

- A value that `routeScope` puts in an `AsyncLocalStorage` is visible inside
  `resolveStorage` and inside the storage call the route makes.
- Streaming: the upstream answers 401 only after `router.route` has returned
  its Response. The auth retry refreshes the OAuth credential, and the
  recording credential store sees the initiating request's value on that
  write.
- A throwing `routeScope` answers 500.

### T3 — Compass store uses the triggering token (compass-gateway)

Lane: compass-gateway, RigelBuild/oh-my-pi, PR off `rigel-release`. It does
not depend on T2's code.

- New `packages/ai/src/compass/agent-token-scope.ts`, exported from
  `packages/ai/src/compass/index.ts`.
- `wire.ts`: the three request types drop `agentAccountId` and gain
  `agentToken: string`.
- `CompassAuthCredentialStore`: remove the `agentAccountId` option and the
  `#agentAccountId` field. Document `token` as the `llm-gateway` service
  bearer. `#request` takes the agent token as an argument and puts it in the
  body. Every door call takes its token from `currentAgentToken()` when it
  starts or is queued, and keeps it with the queued job. The job token is a
  parameter of `#sendOAuthUpdate` and `#disable`. `#adoptOwnCommit`, the
  `ListCredentialPool` call inside `#sendOAuthUpdate`'s retry loop, uses it
  too and never reads the scope:
  - `currentAgentToken()` treats an empty string as no token.
  - `refreshSnapshot()` and `open()` throw when no token is in scope;
    `#maybeBackgroundRefresh()` returns without a call.
  - `#queueOAuthUpdate`, `#queueDisable` and `deleteAuthCredential` with no
    token in scope set `unsynced` and make no call. `#resendUnsynced` resends
    with the current scope's token.
  - In `#runWrite`, `unauthenticated` keeps `unsynced` as a transient code
    does. `#sendOAuthUpdate` keeps recording unknown outcomes only for
    `TRANSIENT_CODES`, because an `unauthenticated` write did not commit.

Interfaces:

```ts
// packages/ai/src/compass/agent-token-scope.ts
export function runWithAgentToken<T>(token: string, run: () => T): T;
export function currentAgentToken(): string | undefined;

// packages/ai/src/compass/credential-store.ts
export interface CompassAuthCredentialStoreOptions {
	baseUrl: string;
	/** The `llm-gateway` service bearer. */
	token: string;
	fetch?: typeof fetch;
	timeoutMs?: number;
	refreshIntervalMs?: number;
}
/** True while a write is queued, in flight, or parked for a resend. */
hasPendingWrites(): boolean;

// packages/ai/src/compass/wire.ts
export interface ListCredentialPoolRequest { agentToken: string; provider?: string; }
export interface UpdateCredentialOAuthRequest { id: string; token: GatewayOAuthTokenRequest; expectedVersion: GatewayInt64; agentToken: string; }
export interface DisableCredentialRequest { id: string; cause: string; expectedVersion: GatewayInt64; agentToken: string; }
```

Test cycle, in `packages/ai/test/compass-credential-store.test.ts`:

- Each door call carries the scope's token and no `agentAccountId`.
- A write queued under token A that runs after A's scope ends still sends A.
- With no scope, `open()` and `refreshSnapshot()` throw and make no fetch,
  and `pollExternalChanges()` makes no fetch. A write makes no fetch, and the
  next poll under token B sends it with B.
- A scope that holds `""` behaves as no scope.
- `unauthenticated` on `UpdateCredentialOAuth` parks the write.
  `hasPendingWrites()` is true. The next poll under another token commits it,
  and `hasPendingWrites()` is false.

## Tasks

- [ ] T1 — compass-server: proto `agent_token` replaces `agent_account_id`
      (reserved); `GatewayCaller.TenantID`; door verifies the token and runs
      under its tenant; delete `GatewayAgentTenant` and its system-role use.
- [ ] T2 — compass-gateway: `AuthGatewayRouteOptions.routeScope` seam.
- [ ] T3 — compass-gateway: `CompassAuthCredentialStore` sends the
      triggering agent token, has no request-less door call, parks writes on
      `unauthenticated`, exposes `hasPendingWrites()`.

## Open Questions

**Q1 (resolved, Matt, 2026-10-11): A.** No door call with no request behind
it. Rule 1, the LRU pin and the parked-write residual stand as written.

Matt asked whether this costs a door call per gateway request. On an open
store it does not. On `rigel-release`, `pollExternalChanges` calls
`#maybeBackgroundRefresh`, which returns early when
`Date.now() - this.#lastRefreshStartMs < this.#refreshIntervalMs` (default
`DEFAULT_REFRESH_INTERVAL_MS = 5_000`), and the reload runs in the
background. Other door calls skip that limit:

- A cold open (`open()` awaits `refreshSnapshot()`) lists once, and the
  request waits for it.
- Each write sends one `UpdateCredentialOAuth` or disable call.
- A write conflict or own-commit adoption adds a list.

This record keeps those paths as they are and adds only the token. Matt chose
to ship this and measure the rate on dev after T3.
