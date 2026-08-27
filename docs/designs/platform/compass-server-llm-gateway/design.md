# Compass Server LLM Gateway (RIG-1715)

Status: Draft (freezes on merge). Directory-form record under
`docs/designs/platform/`. Companion / downstream of
`docs/designs/platform/compass-observability-architecture/design.md` (#656),
whose Plane-A tasks T1–T3 name this record as their upstream prerequisite
("bundling the OMP gateway into the Server ... Its design record must land
first", `compass-observability-architecture/design.md:507-509`).

Authorship note: this record is authored by the compass-obs lane per Matt's
ruling (2026-08-27); the driver owns commit/push/PR (commit carries
`Co-authored-by: Matt Wilkinson <matt@rigel.build>`).

## Problem / Intent

Make the Compass Server stack the single LLM egress point for every agent
(RIG-1715): agents never hold upstream provider credentials; a gateway holds
them all and routes each model call across the user's account pool with the
subscription-limit-maximizing logic the OMP auth-gateway runs today. The
gateway is also the source of the Plane-A token-usage/spend data that #656 T1
(store), T2 (read gRPC), and T3 (in-app charts) consume — this record
specifies that data contract. Matt ratified the load-bearing forks
(2026-08-27, PR review): **A2 + B1** — adopt the OMP TS auth-gateway as the
gateway rather than re-implementing it in Go: *"Given we're keeping it
seperate anyway and that it's a pi-native format, the TS one seems to be the
better bet. Maybe a go one later but that's primarily a perf/cost optimization
that costs us a lot given we need to reimplement a bunch of Pi and OMP code in
Go, and then keep that updated with their latest code, which changes very
fast. So OMP gateway for medium term, Go reimpl is a low priority thing i
think, so kind of A1 with a low prio follow up issue. We already run TS/OMP in
the agent itself so not everything is Go anyway and that will remain in TS as
the extensions etc need to stay TS. B - B1. Wasn't aware there was a pi
format, definitely keep that."* This record designs the ADOPTION: the OMP
gateway as a compass service plus the compass adapters around it — not a port.

## Approach

### Recommendation: adopt the OMP TS auth-gateway as a standalone supervised service (A1)

Run the OMP fork's auth-gateway — unmodified in its routing core — as its own
process: a fourth supervised child of the self-host stack (the S4
container-supervision pattern), its own service on the managed plane. Compass
builds ADAPTERS only: a compass-backed `AuthStorage` implementation, a
per-agent bearer→tenant auth adapter, and a token-usage emission hook. The Go
re-implementation is demoted from a planned phase to a **low-priority
follow-up issue** (RIG-2843), which inherits the golden-fixture
parity strategy from this record. Rationale:

1. **Zero re-port cost, and no upstream treadmill.** The gateway's shell is
   small (`server.ts` 836 lines, `auth-gateway/types.ts` 153, `http.ts` 227)
   but the value it fronts is the pi-ai provider layer plus `auth-storage.ts`
   at 7,936 lines — per-provider request shaping (e.g. the Codex
   sampling-control strip, `server.ts:132-145`; Claude-Code OAuth
   system-prompt shaping applied on every translate-path request,
   `server.ts:79-82`), ~17 per-provider usage fetchers, per-provider OAuth
   refresh, and the ranking core. A Go port re-implements that mass and then
   tracks a fast-moving upstream forever; adoption inherits every fix by
   `forks/oh-my-pi` sync. This is Matt's decisive cost argument, verbatim in
   the ruling above.
2. **Full cross-format parity comes free — and it is now a REQUIREMENT.**
   Matt ratified full ingress parity including provider→standard translation
   (OQ-8, decided). The gateway already normalizes all three foreign wire
   formats (OpenAI chat, OpenAI responses, Anthropic messages;
   `handleFormatEndpoint`, `server.ts:346-528`) into the pi-native `Context`
   and re-encodes on egress — the N-format translation matrix that made the
   Go port's T2 balloon under a parity requirement is already written and
   test-covered (14 `auth-gateway-*.test.ts` files under
   `forks/oh-my-pi/packages/ai/test/`). "Free" is scoped to the wire-format
   translation matrix; the compass-adapted request path (per-agent verifier,
   per-tenant store, RPC credential surface) is NEW code this record adds and
   is proven by the adapter tests plus a fork-suite run parameterized over the
   compass injections (Test strategy), not by the inherited suite alone.
3. **The canonical inter-agent format is pi-native `Context` (B1).** pi-ai's
   `Context` is `{ systemPrompt?: string[]; messages: Message[]; tools?: Tool[] }`
   (`forks/oh-my-pi/packages/ai/src/types.ts:1087-1091`; the gateway's parsed
   requests carry it, `auth-gateway/types.ts:109-113`), and the gateway ships
   a pi-native fast path (`POST /v1/pi/stream`, `server.ts:530-543`) that
   skips wire-format round-trips entirely for OMP callers. Compass agents run
   OMP, so the primary agent→gateway path is pi-native; the provider-format
   endpoints remain the external-interop surface for non-OMP callers. A Go
   gateway has no pi-ai internals to serve this format — the old design cut
   `/v1/pi/stream` for exactly that reason; B1 makes that cut untenable.
4. **The gateway is already shaped for standalone adoption.** It is a
   self-contained `Bun.serve` HTTP server (default bind `127.0.0.1:4000`,
   `auth-gateway/types.ts:24`) with credentials and the model registry
   injected at boot (`AuthGatewayBootOptions.storage` / `resolveModel`,
   `server.ts:57-65`; `startAuthGateway`, `server.ts:750-836`) and zero
   compass coupling. The credential seam is exactly where compass plugs in:
   the request path resolves credentials only through the injected storage
   (`storage.getApiKey(model.provider, sessionId, {modelId})`,
   `server.ts:419-422`), and `AuthStorage` is constructed over any
   `AuthCredentialStore` (`auth-storage.ts:1287-1290`) — SQLite
   (`AuthStorage.create`, `auth-storage.ts:1329-1332`) and broker-RPC
   (`RemoteAuthCredentialStore`, `auth-broker/remote-store.ts:241`) backends
   already exist, so a compass-backed store is a third implementation of a
   proven seam, not a fork of the gateway.

**Cost of adoption, stated honestly:** the self-host stack ships and
supervises a Bun runtime — a real exception to the all-Go direction
(RIG-1719) and a fourth supervised child where today there are two Go
binaries plus postgres — three supervised children
(`go/internal/stack/deps.go:82-89`). Matt accepts this
explicitly: OMP already runs in every agent container, "so not everything is
Go anyway and that will remain in TS as the extensions etc need to stay TS."
The Bun runtime is not NEW to the shipped product — the agent image already
carries it — it is new to the *stack supervisor's* child set. The perf/cost
axis (per-request overhead, memory, a second GC) is real and is precisely
what the low-priority Go follow-up exists to reclaim if it ever matters.

### The routing algorithm we adopt (and the parity surface the Go follow-up must preserve)

This is the precise characterization Matt asked for ("document the OMP
gateway's current routing before the rewrite so the Go version matches",
RIG-1715 body). Under A1 it documents what compass adopts as-is — the
behavior contract of the running gateway — and doubles as the parity spec the
Go follow-up issue must preserve if it is ever picked up.

**Request pipeline** (`server.ts:346-528` `handleFormatEndpoint`):

1. Parse the wire request; resolve `model` via the catalog
   (`server.ts:377-380`).
2. Derive a session identity for credential stickiness + prompt caching:
   client `prompt_cache_key` wins, else a deterministic UUID over
   `modelId + systemPrompt + tools + first message` (`deriveSessionId`,
   `server.ts:108-127`; applied at `server.ts:410-411`). The same key is
   forwarded as `promptCacheKey`/`sessionId` (`server.ts:163-168`).
3. Resolve a credential: `storage.getApiKey(model.provider, sessionId, {modelId})`
   (`server.ts:419-422`); 401 with `authentication_error` when none
   (`server.ts:430-436`).
4. Stream with a retry resolver implementing the a/b/c policy
   (`buildGatewayApiKeyResolver`, `server.ts:276-328`): (a) initial → the
   resolved credential; (b) first failure, `!lastChance` → force-refresh the
   SAME session-sticky credential (a peer may have rotated the token);
   (c) `lastChance` → switch credentials via
   `refreshGatewayApiKeyAfterAuthError`.
5. Error-class split on (c) (`server.ts:210-274`): a usage-limit error
   (provider `usage_limit_reached` / `resource_exhausted` phrasing) calls
   `markUsageLimitReached` — block just this credential until its reset,
   honoring `retry-after`/`resets_at` hints, and retry only if a sibling is
   free; a hard auth failure calls `invalidateCredentialMatching` — suspect
   the row so it is not re-picked.

**Credential selection** (`auth-storage.ts`): per-provider candidate lists
with session stickiness, round-robin dispersion, temporary blocks, and
usage-report-driven ranking. State: round-robin index per provider:type,
session→last-credential map, per-credential `blockedUntil`
(`auth-storage.ts:1242-1252`). Selection walks candidates in session/RR order
and returns the first unblocked, falling back to the first in order when all
are blocked (`#selectCredentialByType`, `auth-storage.ts:1890-1918`). When a
ranking strategy exists for the provider, candidates are ranked by live usage
reports fetched in parallel under a timeout — "better to pick a credential
without usage data than to hang the agent" (`auth-storage.ts:4373-4377`).

**The ranking comparator** — the subscription-limit-maximizing core
(`#compareUsageRankedCandidatePriority`, `auth-storage.ts:4292-4334`), in
order:

1. Unblocked before blocked; among blocked, earliest `blockedUntil` first.
2. Plan-priority (openai-codex plan gating) when a plan requirement is set.
3. Priority-boosted accounts first (`strategy.hasPriorityBoost`).
4. **Hot-window guard**: a candidate whose primary (e.g. 5h) window is above
   `PRIMARY_WINDOW_HOT_FRACTION` ranks behind every cool one — "overflow
   lands on the next-most-urgent cool account" (`auth-storage.ts:4308-4313`).
5. Usage-measured candidates outrank unmeasured ones
   (`auth-storage.ts:4314-4320`).
6. **Required-drain, descending** — the algorithm's heart: for each window,
   `requiredDrain = headroomFraction / remainingHours` — "how fast the
   window's remaining quota must be consumed to fully use it before it
   resets ... selection chases quota that is about to be wasted ('use it or
   lose it')" (`#computeWindowRequiredDrain`, `auth-storage.ts:4269-4290`,
   with a one-minute floor on remaining time). Order: secondary (weekly)
   drain desc → secondary used asc → primary (5h) drain desc → primary used
   asc (`auth-storage.ts:4321-4332`), then stable order-position tiebreak
   (`auth-storage.ts:4336-4343`).

**Usage-limit marking** (`markUsageLimitReached`,
`auth-storage.ts:4164-4251`): resolve the session's credential (including
bearer-fingerprint attribution for delayed limit responses after token
rotation, `auth-storage.ts:4180-4192`); compute `blockedUntil` from
`retryAfterMs` or the default 60s backoff, extended to the provider-reported
window reset when a usage report confirms exhaustion
(`auth-storage.ts:4204-4218`); report `switched: true` only if an unblocked
sibling exists, else the earliest sibling `retryAtMs`
(`auth-storage.ts:4236-4250`).

**Parity-critical ranking details** (in the source, load-bearing for the Go
follow-up — a port built against the summary above would diverge on each):

- **Unmeasured usage normalizes to 0.5, not 0.** A candidate with no usable
  usage report is treated as half-used, not empty (`#normalizeUsageFraction`,
  `auth-storage.ts:4261-4267`) — an unmeasured account must not falsely rank
  as most-drainable.
- **`remainingMs` is clamped to the window's own `durationMs`**
  (`auth-storage.ts:4283-4285`) in addition to the one-minute floor: a
  just-reset window cannot claim more remaining time than the window is long.
- **openai-codex re-checks blocked credentials live during ranking.** Blocked
  codex candidates get a live usage re-fetch inside selection and can unblock
  mid-rank (`#rankOAuthSelections`, `auth-storage.ts:4382-4394`) — I/O on the
  selection path, and a day-1 provider.

**Account pools** (the multi-tenancy seam compass re-sources): a
`ReadonlyMap<provider, ReadonlySet<identityKey>>` filter over the credential
snapshot — an OAuth credential is visible only if its identity is in the
pool (`isCredentialInAccountPool`,
`forks/oh-my-pi/packages/ai/src/auth-broker/remote-store.ts:41-51`). Today
the pool is file/env-configured (`loadAuthBrokerAccountPool`,
`auth-broker/discover.ts:119-167`); the compass `AuthStorage` adapter sources
it from the store instead (per-user pools, below). Same filter semantics,
different source.

**Adopted-surface notes** (deltas from the old Go-port scope, each with why):

- **The pi-native fast path (`/v1/pi/stream`, `server.ts:530-543`) is KEPT
  and promoted**: under B1 it is the canonical inter-agent path, not an
  OMP-internal shortcut to cut. The old design dropped it because a Go
  gateway has no pi-ai internals; adoption dissolves that reason.
- **The broker/remote-store layer (`auth-broker/`) is NOT used**: the
  compass gateway's credential source is the compass-backed `AuthStorage`
  adapter (T2 below), not an external broker process. The broker remains
  what wave agents use OUTSIDE the product.
- **`deriveSessionId` is inherited but MUST be tenant-qualified** — it hashes
  only modelId + systemPrompt + tools + first message (`server.ts:108-127`),
  so two agents of different owners with identical prompts derive the SAME id
  and would collide on the session→credential stickiness map and on usage
  attribution. The compass adapter seeds the hash with the resolved
  `agent_account_id` (T3) so the id is unique per tenant; the prefix-caching
  benefit is preserved within a tenant.

### Gateway architecture on the stack

**Placement: a standalone supervised child (self-host) / its own service
(managed)** — NOT a Go package in compass-server, and NOT in-process. This is
entailed by A1, not a separate topology ruling: the adopted gateway is a Bun
server and cannot run in-process in the Go compass-server, so separateness
follows from the language choice. It also matches Matt's intent ("we're
keeping it seperate anyway") and the managed-plane scaling point — a gateway
tier scales independently of the control plane — and self-host mirrors that
shape so the two deployments run the same artifact. The stack supervisor
today spawns three supervised children — postgres plus two Go binaries
(`ComponentPostgres`/`ComponentServer`/`ComponentRunner`,
`go/internal/stack/deps.go:82-89`) via the `spawnChain` cold-start sequence
(`go/internal/stack/stack.go:192-256`); the gateway becomes a fourth child
following the S4 supervised-container pattern already proven for the
container-backed postgres (`PostgresContainer` seam, `deps.go:172-188`,
started by `startPostgresContainer`, `stack.go:295-303`) and adopted by the
Plane-B collector child (RIG-2825): the Bun gateway runs as a supervised
container in all planes (one supervision seam; the bare-Bun-process dev
variant is deferred unless a dev-loop need proves it out), core-built spec,
adapter-run, torn down with the stack. Fault isolation of the control door,
previously the in-process design's conceded tradeoff, now comes for free: a
gateway resource pathology (leaked SSE streams, runaway usage-fetch fan-out)
is contained in the gateway process and cannot degrade the compass.v1 control
door. This is isolation, NOT automatic recovery — the stack supervisor spawns
the chain once and drains on failure (`spawnChain` drains via `drainChildren`,
`stack.go:128-135`), with no child crash-restart loop today; the gateway is
the one child whose death halts all agent model calls, so T1 adds a restart
policy for it (below).

**Listener: the gateway's own, unchanged.** `startAuthGateway` binds its own
HTTP server (`Bun.serve`, `server.ts:750-755`; loopback default
`127.0.0.1:4000`, stack-configured bind for agent-container reachability) and
already serves the full route set (`server.ts:769-806`): `/healthz`,
`GET /v1/usage`, `GET /v1/credentials/check`, `POST /v1/chat/completions`,
`POST /v1/messages`, `POST /v1/responses`, `POST /v1/pi/stream`,
`GET /v1/models`. Nothing lands on the compass.v1 Connect door — that
contract stays the UI↔server door ("the single, owned door between any UI
and the Compass server", `proto/compass/v1/compass.proto:1-4`); callers here
are LLM SDKs and OMP inside agent containers.

**Agent→gateway auth: per-agent bearer→tenant adapter.** Today the gateway
authenticates against a flat static bearer set (`isAuthorized(req, tokens)`
over `Set<string>(opts.bearerTokens)`, `server.ts:752,772`;
constant-time comparison, `http.ts:58-76`) with no per-caller identity. The
compass adapter (fork extension, T3) replaces the flat set with an injected
verifier: per-agent tokens minted by the Server (random 256-bit, stored
hashed, revocable, rotated on session re-placement), verified by the gateway
and resolved to `agent_account_id → owner_user_id` (both already on
`agent_accounts`, `0001_init.sql:74-80`) — the tenant identity that selects
the account pool and stamps usage attribution. This is a small, upstreamable
seam change (an `authorize(req) → identity | null` boot option in place of
`bearerTokens`), not a fork of the routing core. Scoping credential SELECTION
to the caller's pool is the larger change, and the record names its mechanism
rather than folding it into the auth seam: `getApiKey` carries no caller
identity and `AuthStorage` is a process-global singleton over one store with
process-wide selection state (round-robin indices, the session→credential
stickiness map, per-credential backoff, `auth-storage.ts:1235-1266`), so
per-request pool scoping is NOT an injection into that singleton. The compass
integration runs ONE `AuthStorage` per tenant (`owner_user_id`), each over a
pool-scoped store view, keeping selection/backoff/usage-cache state correctly
partitioned. On the managed plane one gateway process fronts many tenants, so
these per-tenant instances live in a bounded LRU pool — constructed lazily on
a tenant's first request and idle-evicted (`close()`) — so live memory and
usage-fetch fan-out scale with ACTIVE tenants, not the total roster: one
gateway process, never one per tenant. The eviction-miss cost is a cold
`AuthStorage` reload for a returning idle tenant, bounded and operational, and
the per-tenant state/fan-out cost is carried in T2/T3. The rejected
alternative — threading identity through the selection core (`getApiKey` →
`#selectCredentialByType`), or equivalently re-keying its process-wide
selection maps by `(tenant, provider)` — is a routing-core signature change
that would break the "never the routing core" fork-sync constraint the whole
standalone-adoption case (A1) rests on, and downgrades tenant isolation from a
structural property to one re-audited on every upstream sync.
Delivery to containers reuses the container seed path
(`ProviderSeed` → 0600 `$HOME/.compass/auth-seed.json`,
`go/internal/runtime/secrets_materialize.go:72-77`), whose payload changes meaning — instead
of raw provider keys it carries ONE credential, the gateway base URL + the
agent's gateway token (env: `COMPASS_LLM_GATEWAY_URL`,
`COMPASS_LLM_GATEWAY_TOKEN`), and OMP-in-container points at the gateway
(pi-native endpoint preferred, provider-format endpoints for foreign SDKs in
the container).

### Credential storage and rotation

The gateway is the only runtime holder of upstream provider credentials at
request time; the compass store is the durable source of truth. That needs a
value-persisting store — and it is a NEW store, NOT an extension of the
SEA-1327 declared-secrets registry, which is names-only by invariant: it
persists a secret's name/delivery/kind/provider but never its value ("Values
live only in the provider and this process's memory during a resolve; they are
never persisted by Compass", `go/internal/secrets/secrets.go:20-22`;
`store/secrets.go:99-113` has no value column). Provider credentials are
different in kind: OAuth access/refresh tokens are refreshed by the gateway on
the hour and MUST survive restarts, so they need a value store compass owns.
This record adds a dedicated `gateway_credentials` store — api_key and
OAuth-shaped payloads (access/refresh/expiry), a monotonic `version` per row
supplying the CAS substrate, and a scope column resolving each row into a
caller's pool: an owner-scoped row keyed by `owner_user_id` (a user's own
credential) or a shared row keyed by the org/company scope (the company key its
users fall back to) —
distinct from the declared-secrets registry, whose never-persist-values
invariant is left intact. The `SECRET_KIND_PROVIDER` registry rows stay as
they are (they NAME which providers a user configured); the value store holds
the credentials the gateway actually routes with. Blast radius, stated: a
compromised gateway (holding one stack token) can read every tenant's provider
credentials — the isolation boundary is the per-tenant pool scoping enforced
server-side on the credential-list surface (below), not the TS process, which
by design holds them all.

The gateway consumes credentials exclusively through its injected
`AuthStorage` (`server.ts:57-59,419-422`), so the compass integration is an
`AuthCredentialStore` implementation (the same seam SQLite and the broker
RPC store implement, `auth-storage.ts:1287-1290,1329-1332`). Matt named both
wirings in the OQ-7 note ("either auths to Postgres itself or RPCs to the
Server to get the creds", RIG-1715 comment); this record recommends RPC and
keeps Postgres-direct as a named alternative — decided-with-rationale, not an
open fork:

- **RECOMMENDED: RPC-to-Server-for-creds.** The gateway's store
  implementation calls a narrow, stack-token-authenticated Server surface
  (list credentials for pool / write back refreshed OAuth tokens / CAS
  disable). Why: the gateway is a compute tier — own process self-host, own
  service managed — and the Server already owns the store; a single
  DB-connection surface (the Server's) keeps Postgres topology, pooling, and
  migration coupling in one place, and the managed plane gets the same
  wiring with zero deltas. Precedent: the gateway already runs this exact
  shape against the auth-broker (`RemoteAuthCredentialStore`,
  `remote-store.ts:241`), so the RPC store is a sibling of existing code,
  not a new pattern.
- **Alternative (self-host-single-node): Postgres-direct.** The
  `AuthCredentialStore` implementation opens the stack's Postgres directly.
  Simpler on a single node (no new Server surface), but it gives the gateway
  its own DB pool and couples a TS process to the Go store's schema — the
  exact coupling the RPC wiring avoids. Kept as the fallback if the RPC
  surface proves heavier than the T2 estimate; the seam is identical either
  way, so switching is contained.

OAuth refresh stays gateway-side, as it already is: single-flight per
credential (`#oauthRefreshInFlight`, `auth-storage.ts:1283-1284`), refreshed
tokens written back through the store implementation with CAS semantics
mirroring `#tryDisableCredentialAtIfMatches` (`auth-storage.ts:2112-2123`) so
a racing rotation is never clobbered. Raw provider credentials STOP being
materialized into agent containers once the gateway ships (the flag-day is
T5).

### Account pools: own credentials, then a shared org key

A pool is the set of provider credentials a request may draw on, resolved from
the caller's tenant identity (`owner_user_id`, `0001_init.sql:74-80`). Two
credential SOURCES compose, in precedence order: (1) the user's OWN credentials
— their personal subscription/OAuth login and their own API keys — preferred so
a user who brought their own provider access uses it; and (2) a SHARED
org/company key the company provides to all its users, used as the fallback
when the caller brought none of their own for that provider. Own-before-shared
(a paid personal subscription is never silently displaced by the company key),
riding OMP's existing within-provider type precedence unchanged — a deliberate
OAuth/login credential wins over a stored API key (`peekApiKey`/`getApiKey`,
`auth-storage.ts:5133-5166`). Subscriptions and API keys are already distinct
credential backends there (a claude.ai OAuth login and an Anthropic API key are
separate rows of different `type`), so own-then-shared is a pool-membership +
ordering rule over that seam, not a new selection mechanism. The gateway's
account-pool filter (`isCredentialInAccountPool`, `remote-store.ts:41-51`)
provides the membership semantics; the compass `AuthStorage` adapter provides
the source — the store, not a JSON file (`discover.ts:119-167` is the wiring
being replaced) — with the shared-vs-own distinction a column on the credential
row resolved server-side into the caller's pool.

Self-host is single-team, so per-user pools degrade gracefully to "the
operator's accounts" plus whatever shared keys the operator configured. The
managed plane needs org grouping, and the schema does NOT model it today —
`owner_user_id` is the only ownership key and no org/tenant/team table exists
(`0001_init.sql`). Org-scoped shared pools are therefore an added entity (an
org id on `user_accounts` and on the shared-credential rows) the managed plane
introduces when it lands, resolved through the SAME T2 pool-resolution seam;
this record does not build it but names it as the seam's one open extension, so
the T2 adapter is designed to take an org key, not only a user key.

**Out of scope here — role/model policy above the pool.** Which MODEL a Compass
agent role uses, and how a stable model name (e.g. `claude-opus-4-8`) maps to a
preferred BACKEND order across the providers a user holds (own subscription,
own API key, then alternates like OpenRouter or Bedrock), is a policy layer
ABOVE this credential substrate — designed separately in RIG-2845 (Compass
Model Roles + stable-name provider routing), which consumes this pool
resolution and the RIG-2562 model evaluations.

### The Plane-A data contract (what #656 T1/T2/T3 build against)

This section IS the seam. #656 already fixes the frame: "the OMP gateway
**to be bundled into the Server** records per-account/org **token usage +
spend** (the display quantity)"
(`compass-observability-architecture/design.md:61-64`); "Plane A (gateway →
Postgres) is authoritative for usage/spend"
(`design.md:445-451`). This record commits the concrete shapes. The shapes
below are UNCHANGED by the A1 ruling — only the emission source moved: the
TS gateway emits each event through a compass emission hook (an injectable
`onUsage` callback in the gateway boot options — a small, upstreamable fork
extension, T4) whose compass implementation batches and posts to a Server
ingestion surface, which writes the store. The old design's in-process Go
gateway wrote the store directly; the contract the store and readers see is
identical.

**1. The token-usage event (gateway → ingestion → store, append-only).** One
event per completed upstream model call (including failed/aborted calls with
partial usage, flagged). Shape (Go struct = the write contract **#656 T1**
stores and **this record's T4** emits; field set derived from pi-ai's
canonical `Usage` — `input/output/cacheRead/cacheWrite/totalTokens` +
`cost{input,output,cacheRead,cacheWrite,total}`,
`forks/oh-my-pi/packages/catalog/src/types.ts:95-149`):

```go
// package llmusage — the write contract #656 T1 stores; this record's T4 emits it.
type TokenUsageEvent struct {
    ID              string // server-assigned UUID
    OccurredAtUnixMs int64
    AgentAccountID  string // the calling agent (attribution spine)
    OwnerUserID     string // the tenant key (per-org on managed)
    SessionID       string // runner session id, when known
    RequestID       string // gateway request UUID
    Provider        string // e.g. "anthropic"
    Model           string // resolved model id
    CredentialID    string // which pool account served it
    InputTokens     int64
    OutputTokens    int64
    CacheReadTokens int64
    CacheWriteTokens int64
    TotalTokens     int64
    CostMicroUSD    int64  // total; computed gateway-side from the pricing table
    RateVersion     string // pricing-table version applied at write time
    Outcome         string // "ok" | "error" | "aborted" (partial usage still recorded)
}
```

Write contract: `AppendTokenUsage(ctx, []TokenUsageEvent) error` —
append-only, idempotent on `ID`, batched: the gateway buffers events in a
BOUNDED in-memory buffer and flushes on a short interval, retrying with
backoff against the ingestion surface (idempotent `ID`s make retry safe).
Because emission now crosses a network hop to the Server, the loss window is
≤ flush interval only while ingestion is reachable; during an outage the
buffer holds up to its cap and drops oldest beyond it, so worst-case loss is
bounded by the buffer cap (T4), not unbounded — acceptable for a display
quantity per #656 D5, which is explicitly NOT the billing-grade compute-usage
event (runtime-sourced, out of this record's scope, `design.md:552-567`).

Cost is integer micro-USD (no floats in the store), computed at write time
from a pricing table keyed by (provider, model, `RateVersion`) that retains
**per-component rates** (input/output/cache-read/cache-write). Under A1 the
per-component rates come free: the gateway's pi-ai catalog already carries
them (`Usage.cost`, `catalog/src/types.ts:95-149`) and the emission hook maps
them; the ingestion side records the `RateVersion` (the fork's catalog
version at emit time) so cost stays auditable against a pinned table.

The canonical `Usage` also reports orchestration tokens (billed but outside
the conversation prompt/cache buckets, `catalog/src/types.ts:106-113`),
reasoning tokens, and server-tool counts. These are deliberately NOT carried
as separate `TokenUsageEvent` fields (a display quantity, not a billing-grade
breakdown); the T4 mapping folds orchestration + reasoning tokens into
`TotalTokens` and `CostMicroUSD` (so the total stays faithful to what the
provider billed) and drops the per-server-tool attribution. Per-component
cost (input/output/cache-read/cache-write) is not stored on the event — only
`CostMicroUSD` total — but stays reconstructable from the per-component token
counts × the versioned per-component rates pinned by `RateVersion`, so the
event row stays narrow without losing auditability (the pricing table is
retained by version, never mutated in place).

**2. Storage (#656 T1's lane, #656 owns the task).** `token_usage_events`
append-only + hourly/daily rollup tables keyed (owner_user_id,
agent_account_id, provider, model, bucket) — rebuildable from the event log,
per #656 T1 ("Postgres rollup tables derived and rebuildable",
`design.md:525-536`), folded into the squashed migration per the store
convention (`0001_init.sql:10-15`). This record hands T1 the event shape
above; T1 owns the DDL, retention, and the `UsageStore` interface
(`design.md:545-551`).

**3. The tenant-scoped read contract (#656 T2's lane; implemented by this
record's T4 + `UsageService`).** New RPCs on a new
`UsageService` in `proto/compass/v1` (schema change + `moon run
compass-proto:gen`, never a raw stub — the compass.v1 discipline):

```proto
service UsageService {
  // Historical token usage/spend series for charts (T3).
  // Tenant scope is enforced server-side from the authenticated caller;
  // never client-supplied trust.
  rpc GetUsageSeries(GetUsageSeriesRequest) returns (GetUsageSeriesResponse);
  // Live provider-quota snapshot for the UsageBar meters:
  // per pool account {provider, plan, used/limit fractions, resets_at},
  // served from the adopted gateway's cached provider usage reports
  // (proxied from its GET /v1/usage, server.ts:708-715).
  rpc GetProviderQuota(GetProviderQuotaRequest) returns (GetProviderQuotaResponse);
}

message GetUsageSeriesRequest {
  int64 start_unix_ms = 1;
  int64 end_unix_ms = 2;
  Granularity granularity = 3;     // HOUR | DAY (fixed enum, no free-form)
  string agent_account_id = 4;     // optional filter
  bool include_subtree = 5;        // roll up the agent's tree descendants
  string provider = 6;             // optional filter
}
message UsageBucket {
  int64 bucket_start_unix_ms = 1;
  int64 input_tokens = 2;
  int64 output_tokens = 3;
  int64 cache_read_tokens = 4;
  int64 cache_write_tokens = 5;
  int64 cost_micro_usd = 6;
}
```

`GetProviderQuota` is what T3 wires the existing `UsageBar` shell to (it
renders "per-account provider/plan/tokensUsed/tokensLimit/resetIn meters"
from stub data today, `design.md:81-88`); `GetUsageSeries` powers the
time-series charts. Two RPCs because they are different data: quota is a
live provider-side snapshot (the gateway's 5-min per-credential TTL cache,
`server.ts:702-707`, surfaced on its `/v1/usage`), series is our own
event-log history. The `UsageService` read path is Go/compass.v1, exactly as
before — the Server proxies quota from the gateway's HTTP endpoint and reads
series from its own store.

### Cost attribution per agent / tree node (RIG-1713 composition)

Every event carries `AgentAccountID`; the agent tree is the store's
`agent_accounts.parent_agent_id` spine (`0001_init.sql:56-66`). Subtree
rollups are a read-time recursive-CTE aggregation exposed via
`include_subtree` — attribution is stored flat (leaf-level), never
denormalized up the tree, so reparenting (`store.ReparentAgent`) never
rewrites the event log. Semantics (DECIDED, OQ-9): because the CTE walks the
*current* parent edges at read time, a subtree query reflects the tree
as-of-now, not as-of-event — reparenting a child retroactively moves its
historical cost under the new parent in subtree rollups (leaf attribution is
unchanged). Matt ratified as-of-now for MVP; the as-of-event alternative
(freezing each event's ancestry at write time) remains available later at
the cost of a wider event row and no free reparenting. RIG-1713's
message-cost estimator gains a real price source: per-agent recent $/token
from the rollups replaces guesswork (RIG-1713 asks "how to price an idle
subscriber's re-load"; the estimate stays that record's scope — this record
only guarantees the queryable per-agent cost series it needs).

### Test strategy

Under adoption the parity question inverts: we do not prove a port matches
the gateway — we RUN the gateway, so its own suite is the spec and the
regression net. Two layers:

1. **The gateway's own suite, in CI at the fork pin.** The OMP fork carries
   14 `auth-gateway-*.test.ts` files under `forks/oh-my-pi/packages/ai/test/`
   (wire formats, caching keys, cross-protocol caching, error
   classification, model list, response headers, pi-native path) plus
   `forks/oh-my-pi/packages/coding-agent/test/auth-gateway-account-pool.test.ts`
   (pool filtering). These run against the exact code compass ships in its
   VANILLA configuration (flat bearer, default store), so they protect the
   routing core, not the compass-adapted request path; a fork sync that breaks
   them blocks the sync.
2. **Compass adapter tests — the code this record actually adds.**
   - The `AuthStorage`/`AuthCredentialStore` adapter (T2): contract tests
     against the same behaviors the SQLite store satisfies (list/refresh
     write-back/CAS disable), run against the RPC implementation with a fake
     Server; Go-side, the ingestion + credential surfaces follow the DL-174
     pyramid (in-memory ref + `pgtest`,
     `compass-test-strategy/design.md:123-128` via #656 Global Constraints).
   - Selection-path RPC (T2): the credential resolver calls the store
     mid-request (`storage.getApiKey` with `forceRefresh` in the a/b/c retry
     resolver, `server.ts:299-311`; codex live usage re-fetch inside ranking,
     `auth-storage.ts:4382-4394`), so under the RPC wiring each selection
     crosses a hop to the Server. Test cycle asserts a latency budget for that
     RPC and falls back to the existing usage-fetch timeout posture
     (`auth-storage.ts:4373-4377`) on breach; a fork-suite subset runs
     parameterized over the compass `authorize()` + RPC-store injections so the
     SHIPPED configuration, not just vanilla, carries a regression net.
   - The bearer→tenant adapter (T3): mint/verify/revoke unit tests
     (constant-time verification posture per `http.ts:58-76`), pool-scoping
     tests (agent A never sees agent B's owner pool).
   - Usage emission (T4): hook-fires-per-call tests in the fork; ingestion
     idempotency (replayed batch, same `ID`s) and rollup/subtree correctness
     via `pgtest`.
   - End-to-end smoke: a supervised-stack boot where an agent container
     completes a model call through the gateway with NO provider secret in
     its container (the T5 acceptance).

The old three-layer Go-parity strategy — golden JSON fixtures generated from
the fork's comparator, a Go fixture-replay suite, and the httptest black-box
ladder — MOVES to the Go follow-up issue verbatim; it is that issue's
acceptance spec, not this record's.

## Alternatives considered

### Go-native gateway, in-process in compass-server (the pre-ratification recommendation; rejected)

What it was: skip embedding, port the routing algorithm (the
characterization above) into a new `go/internal/llmgateway` package inside
compass-server ("a thin wrapper over server.Serve",
`go/cmd/compass-server/main.go:3-7`), prove parity with golden fixtures +
a black-box suite. Its strengths were real: no second supervised runtime, direct
store access for credentials and usage events, and the Server-side identity
the flat-bearer gateway lacks. Matt rejected it on cost: the port
re-implements "a bunch of Pi and OMP code in Go" — not the 836-line shell but
the 7,936-line `auth-storage.ts`, the pi-ai provider layer behind
`streamSimple`, per-provider OAuth and usage fetchers — "and then keep that
updated with their latest code, which changes very fast." The full-parity
requirement (OQ-8, decided) makes it strictly worse: the N-format translation
matrix and provider→standard normalization the old design scoped OUT of its
T2 is now mandatory, and the TS gateway already has it. Each of its strengths
has an adapter-shaped answer under A1 (RPC credential surface, ingestion RPC,
bearer→tenant adapter); none of its costs do.

**Why not Go NOW, honestly:** nothing in the beta needs the perf headroom —
the gateway fronts network calls to LLM providers measured in seconds, and
Bun's overhead is noise against that. The Go re-implementation is a
perf/cost optimization with a large fixed cost and a permanent upstream-chase
tax, which is exactly the profile of a low-priority follow-up, not a
prerequisite. It is tracked as the driver-filed follow-up issue; the routing
characterization and the golden-fixture strategy in this record are its
inherited spec.

### Go-native gateway as a fourth supervised child (folded into the follow-up)

The same Go port, isolated as its own process. Everything said about the
in-process variant's port cost applies unchanged; the only axis it differed
on — process isolation — is now delivered by the adopted TS gateway's child
topology anyway. If the Go follow-up is ever executed, it inherits this
shape (a drop-in replacement child behind the same routes and adapters), so
the variant folds into the follow-up issue rather than standing as a live
alternative.

### Adopt an existing Go LLM proxy (LiteLLM-class, e.g. a Go gateway OSS) (rejected)

The valuable behavior is the subscription-limit-maximizing pool routing
(required-drain ranking, hot-window guard, reset-aware blocking) — no
off-the-shelf proxy has it, and RIG-1716 already sets the posture for the
sibling MCP gateway: "implement ... ourselves in Go, unless research turns
up a clearly better option," with LiteLLM explicitly the thing being moved
off ("buggy and slow (Python)"). An external proxy would still need the
compass store integration (credentials, attribution, usage events), which
is most of the work — and unlike the OMP gateway, it starts from zero on the
routing core.

### Adopt the auth-broker as the credential plane (rejected)

The gateway already speaks `RemoteAuthCredentialStore` to the OMP auth-broker
(`remote-store.ts:241`), which implements snapshot streaming, observed-usage
batching (the 10s default OQ-5 borrows), and pool filtering — the closest
existing system to what T2 builds. Rejected: the broker is a single-operator
credential plane with client-side pool filtering as a routing policy, not a
server-side tenant-isolation boundary (`remote-store.ts:44-46`), and it is
another process to run. Compass instead implements a new `AuthCredentialStore`
over its own multi-tenant, server-scoped store; the broker stays what wave
agents use OUTSIDE the product.

## Global Constraints

- **All-Go server, with ONE ratified exception**: the LLM gateway is the
  adopted OMP TS auth-gateway and stays TS/Bun for the medium term (Matt:
  "We already run TS/OMP in the agent itself so not everything is Go anyway
  and that will remain in TS as the extensions etc need to stay TS"). The
  supervised Bun runtime is a shipped self-host dependency from T1 on. No
  OTHER non-Go runtime enters the stack; compass-side gateway code (ingestion,
  UsageService, token minting, stack supervision) is Go under `go/internal/`.
- **Fork changes are seam-shaped and upstreamable**: compass touches the
  gateway only at injection points (auth verifier, `AuthStorage`
  implementation, usage hook) — never the routing core — so fork syncs stay
  mergeable.
- **`compass.v1` is the sole UI↔server door** (`compass.proto:1-4`;
  `AGENTS.md:49-58` per #656): the usage read API is a proto change with
  regenerated clients. The gateway's HTTP listener is agent-facing, never
  UI-facing.
- **Agents never hold upstream provider credentials** once T5 lands; the
  gateway is the only runtime holder (RIG-1715 core invariant).
- **Canonical inter-agent format is pi-native `Context`** (B1;
  `packages/ai/src/types.ts:1087-1091`): OMP agents use `/v1/pi/stream`;
  provider-format endpoints are the external-interop surface, with full
  cross-format parity (OQ-8, decided) inherited from the gateway.
- **Plane A is authoritative for usage/spend**; token-usage events are the
  display quantity, NOT the billing-grade compute-usage contract
  (#656 D5, `compass-observability-architecture/design.md:445-451,552-567`).
- **Store discipline**: append-only event writes; DDL folds into the
  squashed migration (`0001_init.sql:10-15`); new store code ships an
  in-memory reference + `pgtest` suite (DL-174 pyramid).
- **Money as integer micro-USD** in events, store, and wire — never floats.
- Markdownlint-clean record; Conventional Commits;
  `Co-authored-by: Matt Wilkinson <matt@rigel.build>` on the record commit
  (driver-owned).

## Plan

Ordering: T1 → T2/T3 (parallel) → T4 → T5; T6 tracks the #656 handoff. T4
additionally depends on #656 T1's `UsageStore` landing (external
compass-server lane — coordinate; this record supplies the event source T1
declared as its prerequisite), so "T4 before T5" is gated on that
cross-record dependency, not just this record's own sequence.
Day-1 provider set: anthropic (OAuth + api_key), openai/openai-codex
(OAuth + api_key), google (api_key) — the providers the wave's agents use.
Under A1 this set is INHERITED, not built: the gateway already speaks all of
them; "day-1" scopes which providers get store-registered credentials,
pricing verification, and smoke coverage. Everything else is a
credential-registration away, not a code task.

### T1 — Adopt the gateway as a supervised stack child

Owner: compass-server.

Package the fork's auth-gateway as a stack-supervised child following the S4
container pattern (`PostgresContainer` seam, `deps.go:172-188`;
`startPostgresContainer`, `stack.go:295-303`; the RIG-2825 collector child is
the sibling precedent): a new gateway component in the spawn chain
(`stack.go:192-256`), core-built spec (image/binary, bind, config), adapter-
run, health-gated on `/healthz` (`server.ts:769-771`), drained on down. A
boot entrypoint in the fork wires `startAuthGateway` with the compass
adapters (T2/T3 injections) in place of the SQLite/broker defaults. Managed
plane runs the same artifact as its own service.

Because the gateway is the single egress for every agent's model calls, T1
adds a bounded crash-restart policy for THIS child specifically (restart with
backoff, distinct from the other children's spawn-once posture) plus a boot
guard: the compass entrypoint refuses to start (or binds loopback-only) when
no `authorize()`/bearer verifier is configured, since the vanilla gateway
treats an empty token set as OPEN (`isAuthorized` returns `true` for
`tokens.size === 0`, `http.ts:80-81`). The Bun entrypoint handles SIGTERM by
draining in-flight SSE before exit (today's `close` is a force-stop,
`server.ts:832-834`; long-thinking calls idle up to 255s,
`server.ts:822-826`), so "drained on down" is a real drain, not a hard kill.
The managed-plane DEPLOYMENT (orchestration, scaling, its config surface) is a
separate record; this record guarantees only that the artifact takes ALL
config — creds-store wiring, verifier, usage hook, bind — by injection, so no
code fork is needed between planes.

Interfaces:

- Consumes: `startAuthGateway(opts)` (`server.ts:750-836`); the stack
  supervisor seams (`deps.go:82-89,172-188`); stack config (new gateway
  bind + image/entrypoint fields).
- Produces: a supervised gateway child with readiness gating; a fork boot
  entrypoint (`packages/ai` or a thin compass wrapper package in the fork)
  accepting adapter injections; stack up/down/teardown coverage in the
  harness tests.
- Test cycle: stack harness tests (spawn order, readiness gate, drain
  order); `/healthz` probe test; the fork's own gateway suite green at the
  pin.

### T2 — AuthStorage-over-compass adapter (credentials + pools)

Owner: compass-server.

The compass `AuthCredentialStore` implementation behind the gateway's
injected `AuthStorage` (`server.ts:57-59,418-422`;
`auth-storage.ts:1287-1290`): RECOMMENDED wiring is RPC-to-Server — a
narrow, stack-token-authenticated Server surface for credential list /
OAuth-refresh write-back / CAS disable (semantics mirroring
`#tryDisableCredentialAtIfMatches`, `auth-storage.ts:2112-2123`), with
Postgres-direct kept as the self-host-single-node alternative behind the
same interface. Server-side: add the dedicated `gateway_credentials` value
store (NOT an extension of the names-only secrets registry — see §Credential
storage and rotation) with a monotonic `version` column supplying the CAS
substrate for `UpdateOAuth`/`Disable` and a scope column (owner-scoped by
`owner_user_id` vs org/company-shared), and implement pool resolution that
composes a caller's OWN credentials (their subscription/OAuth + API keys)
with the SHARED org key as an own-before-shared fallback
(`0001_init.sql:74-80`), with the account-pool filter
semantics (`remote-store.ts:41-51`) enforced SERVER-SIDE on the list surface,
so tenant isolation is a Server boundary and not merely a routing policy
inside the TS process. A `compass.proto` change is likely (today
`SetSecretRequest` carries only kind + provider,
`proto/compass/v1/compass.proto:193-195`).

Interfaces:

- Consumes: `go/internal/store` secrets tables (`store/secrets.go`);
  `agent_accounts.owner_user_id` (`0001_init.sql:74-80`); the
  `AuthCredentialStore` contract (`auth-storage.ts:1287-1290`).
- Produces: the TS `CompassAuthCredentialStore` (RPC client) in the fork's
  compass wrapper; Go-side `type CredentialStore interface { List(ctx, userID, provider string) ([]Credential, error); UpdateOAuth(ctx, id string, tok OAuthToken, expectedVersion int64) error; Disable(ctx, id, cause string, expectedVersion int64) error }`
  and `type PoolResolver interface { Pool(ctx, agentAccountID string) ([]Credential, error) }`
  (the managed-plane org-scoping swap seam) behind the RPC surface;
  proto + regen when the OAuth payload extension needs it.
- Test cycle: TS contract tests against a fake Server; Go in-memory ref +
  `pgtest` (DL-174); CAS race tests.

### T3 — Per-agent bearer→tenant auth adapter

Owner: compass-server.

Replace the gateway's flat bearer set (`server.ts:752,772`) with an injected
verifier (fork seam change, upstreamable): Server-minted per-agent tokens
(random 256-bit, stored hashed, revocable, rotated on session re-placement),
resolved per request to `agent_account_id → owner_user_id`, threaded into
pool scoping (T2) and usage attribution (T4). Constant-time verification
posture preserved (`timingSafeEqual`, `http.ts:58-76`).

Interfaces:

- Consumes: the gateway auth path (`isAuthorized`, `http.ts:80-95`,
  `server.ts:772-775`); `agent_accounts` (`0001_init.sql:74-80`).
- Produces: a gateway boot option `authorize(req) → CallerIdentity | null`
  replacing `bearerTokens`; Go-side
  `type TokenMinter interface { Mint(ctx, agentAccountID string) (token string, err error); Verify(ctx, token string) (agentAccountID string, err error) }`
  and the verify RPC/lookup the gateway's verifier calls.
- Test cycle: mint/verify/revoke unit tests; pool-isolation tests (agent A
  cannot reach agent B's pool); 401 behavior parity with the fork suite.

### T4 — TokenUsageEvent emission hook + UsageService

Owner: compass-obs.

Fork side: an injectable `onUsage` hook in the gateway boot options, fired
once per completed upstream call with the canonical `Usage`
(`catalog/src/types.ts:95-149`) + request metadata; the compass
implementation maps it to `TokenUsageEvent` (folding orchestration +
reasoning into totals per the Plane-A contract), buffers in a BOUNDED
in-memory buffer (drop-oldest at cap), retries with backoff, and flushes
batched idempotent-on-ID writes to a Server ingestion surface. Server side:
the ingestion endpoint writing #656 T1's `UsageStore`, and
`UsageService.GetUsageSeries` (rollup query + recursive-CTE subtree
aggregation over `agent_accounts.parent_agent_id`, `0001_init.sql:56-66`) +
`GetProviderQuota` (proxying the gateway's cached usage reports,
`server.ts:702-715`).

Interfaces:

- Consumes: the `TokenUsageEvent` shape (Approach §Plane-A); #656 T1's
  `UsageStore` (owner: compass-server lane — coordinate, this task supplies
  the event source #656 T1 declared as its prerequisite); the gateway's
  `/v1/usage` (`server.ts:708-715`) for quota; `proto/compass/v1` +
  `moon run compass-proto:gen`.
- Produces: the fork's `onUsage` hook + compass emitter (batched
  `flushUsage`); the Server ingestion handler (idempotent on `ID`);
  `usage.proto` (UsageService as specified in Approach); registered Connect
  handlers with server-side tenant scoping.
- Test cycle: hook-fires-per-call tests in the fork; ingestion idempotency
  (replayed batches); pgtest rollup/read tests; proto-regen CI green;
  subtree aggregation correctness against a seeded tree.

### T5 — Agent egress cutover

Owner: compass-server + runner.

Flip agents to gateway-only egress: Runner materializes
`COMPASS_LLM_GATEWAY_URL`/`COMPASS_LLM_GATEWAY_TOKEN` instead of raw
provider keys in the auth-seed (`secrets_materialize.go:72-77,158-182`);
OMP-in-container points at the gateway (pi-native endpoint per B1, provider
base URLs for foreign SDKs); raw `SECRET_KIND_PROVIDER` values stop leaving
the Server. Gated on T1–T4 being proven in a wave dogfood run.

Interfaces:

- Consumes: T3's minted tokens (Runner obtains them via the existing
  Server→Runner secrets-fetch path, `proto/compass/v1/runner.proto:346-348`
  vicinity); the materializer script machinery.
- Produces: updated `ProviderSeedScript`/env materialization; a
  compatibility flag `--llm-gateway-egress={off,dual,required}` so cutover is
  reversible for one release. Mode semantics: `off` = raw provider creds
  materialized as today (gateway idle); `dual` = both the gateway
  URL/token AND raw creds materialized, OMP configured to prefer the gateway
  and fall back to raw creds on gateway error (the safe rollback rung);
  `required` = only the gateway URL/token, no raw creds. Residual-credential
  window: the flag governs new materializations only — a live container keeps
  the auth-seed it was placed with (`secrets_materialize.go:73-77`), so
  entering `required` does NOT retroactively strip creds from running agents;
  `required` forces re-placement of active sessions (rotating their seed) so
  the raw-credential exposure ends at a bounded, operator-visible point rather
  than lingering until each session happens to re-place.
  Dual-mode fallback (prefer gateway, fall back to raw creds on gateway error)
  presumes an OMP-side per-call fallback capability; this record does NOT
  assume it — T5 either grounds it in OMP's provider-config surface (file+line)
  or scopes the fork change that adds it. If OMP provider config is static per
  session, `dual` is placement-time selection, not per-call rollback, and T5
  says so rather than implying a safety it lacks. `required`-mode incident
  posture: with no raw creds anywhere, a gateway outage stops all model calls,
  so `required` ships only alongside the T1 gateway restart policy plus
  agent-side retry/backoff on transient gateway errors; expected recovery is
  the restart (seconds), with flip-back-to-`dual` re-placement (minutes) as the
  slower fallback.
- Test cycle: runner materialization unit tests; end-to-end agent smoke
  (agent completes a model call with NO provider secret in its container).

### T6 — #656 handoff note

Owner: compass-obs.

One-line dependency flip: #656's "PREREQUISITE ... OMP-gateway-into-Server"
(`compass-observability-architecture/design.md:698-699`) is satisfied by
this record; #656 T1 consumes `TokenUsageEvent`, T2's usage RPCs are
superseded-by/merged-into this record's `UsageService` (single service, no
duplicate proto surface — coordinate at freeze; when #656 is next touched,
annotate its T2 `Produces` line "superseded by RIG-1715 `UsageService`" so the
deferral is visible from #656's side, not only via its PREREQUISITE gate);
and #656 T3 (compass-ui) consumes `GetUsageSeries`/`GetProviderQuota` as
specified here.

## Tasks

- [ ] T1 — Adopt gateway as supervised child (Owner: compass-server) —
      S4-pattern component, fork boot entrypoint, health-gated, fork suite
      green at pin.
- [ ] T2 — AuthStorage-over-compass adapter (Owner: compass-server) —
      RPC-to-Server credential store (Postgres-direct alt), OAuth payloads in
      secrets registry, own-then-shared PoolResolver (org-key seam), CAS rotation.
- [ ] T3 — Per-agent bearer→tenant adapter (Owner: compass-server) —
      token mint/verify, flat-bearer replacement, pool isolation.
- [ ] T4 — Usage emission hook + UsageService (Owner: compass-obs) —
      onUsage hook, TokenUsageEvent ingestion,
      GetUsageSeries/GetProviderQuota on compass.v1.
- [ ] T5 — Agent egress cutover (Owner: compass-server + runner) —
      gateway-only creds in containers, reversible flag.
- [ ] T6 — #656 handoff (Owner: compass-obs) — dependency flip + proto
      dedup note.
- [ ] Follow-up issue RIG-2843 (Owner: compass-obs; low priority) —
      Go-native re-port of the LLM gateway as a perf/cost
      optimization; carries the routing characterization + golden-fixture
      parity strategy from this record.

## Resolved decisions

Formerly load-bearing open questions, each ratified by Matt (2026-08-27):

- **RD-1 (was OQ-1) — Gateway implementation: adopt the OMP TS
  auth-gateway (A1's mechanism, A2's phasing).** DECIDED: no Go build now; the
  Go re-implementation
  is a low-priority follow-up issue ("primarily a perf/cost optimization that
  costs us a lot ... So OMP gateway for medium term"). This reverses this
  record's pre-ratification recommendation (Go-native in-process), preserved
  in `## Alternatives considered` with the reversal rationale.
- **RD-2 (was OQ-7) — Topology: own process from day 1.** ENTAILED by A1,
  not a standalone ruling: the adopted TS gateway is a Bun server and cannot
  run in-process in the Go compass-server, so separateness follows from the
  language choice. Matt's "we're keeping it seperate anyway" is consistent
  with this and motivates the managed-plane independent-scaling point, but the
  load-bearing reason the in-process-vs-child fork is dissolved is A1 itself;
  the gateway's native shape is a standalone server (`server.ts:750-836`).
- **RD-3 (was OQ-8) — Cross-format ingress: full parity, REQUIRED.**
  DECIDED: the gateway accepts every wire format it supports today and
  routes across providers, including provider→standard translation.
  Inherited free by adoption (`handleFormatEndpoint`, `server.ts:346-528`);
  the old provider-native-passthrough scoping is void.
- **RD-4 (new, from the ruling) — Canonical inter-agent format: pi-native
  `Context` (B1).** DECIDED: "Wasn't aware there was a pi format, definitely
  keep that." OMP agents use `/v1/pi/stream` (`server.ts:530-543`);
  Anthropic/OpenAI wire formats are the external-interop surface.
- **RD-5 (author-decided with rationale; NOT part of Matt's A2+B1 ruling) —
  Credential model: RPC-to-Server-for-creds, with Postgres-direct as the
  self-host alternative.** Matt named both wirings in the OQ-7 note ("either
  auths to Postgres itself or RPCs to the Server to get the creds", RIG-1715
  comment); this record recommends RPC
  (own-process gateway is a compute tier; the Server owns the store; single
  DB-connection surface; precedent in `RemoteAuthCredentialStore`,
  `remote-store.ts:241`) behind the identical `AuthCredentialStore` seam
  (`auth-storage.ts:1287-1290`), so switching to Postgres-direct is
  contained if the RPC surface proves heavy. OAuth refresh stays
  gateway-side either way (`auth-storage.ts:1283-1284`).
- **RD-6 (was OQ-2) — Day-1 provider set: anthropic, openai/openai-codex,
  google.** DECIDED as the day-1 scope — now inherited rather than built:
  the set governs credential registration, pricing verification, and smoke
  coverage, not client implementations. Additional providers are
  configuration, not code.
- **RD-7 (was OQ-3) — UsageService ownership.** DECIDED ("lgtm"): this
  record's `UsageService` IS #656 T2's deliverable (one service, no duplicate
  proto surface); on any conflict between the two records' usage-read text,
  THIS record's `UsageService` shape is authoritative (it carries the
  concrete proto, vs #656 T2's sketch at `design.md:588-592`) and #656 T2
  defers to it.
- **RD-8 (was OQ-9) — Subtree cost attribution: as-of-now.** DECIDED for
  MVP: subtree rollups walk the current parent tree at read time; reparenting
  retroactively moves historical cost under the new parent (leaf attribution
  unchanged). As-of-event (stamped ancestry) remains a later option if audit
  needs frozen lineage.

## Open Questions

Non-load-bearing deferrals — each designed-against with a stated default;
none blocks freeze:

- **OQ-4 — Gateway listener transport.** Designed against: plain TCP bind
  reachable from agent containers (the gateway's own listener, default
  `127.0.0.1:4000`, `auth-gateway/types.ts:24`, stack-configured beyond
  loopback), bearer-token auth, TLS via the stack's existing anchor when
  bound beyond loopback. A Unix-socket-per-runner variant is a later
  hardening option; the token model is transport-independent.
- **OQ-5 — Usage-event flush cadence.** Designed against: 10s batched flush
  (the broker's observed-usage default, `auth-broker/remote-store.ts:237-238`
  — under A1 this is the gateway family's own convention, not a mirrored
  constant) into a BOUNDED in-memory buffer (default ~10k events / 5 min,
  drop-oldest at cap) with retry-with-backoff against ingestion; worst-case
  loss is the buffer cap during an ingestion outage, acceptable for a display
  quantity. Tunable later without contract change (idempotent IDs).
- **OQ-6 — Provider quota-fetch fan-out limits.** Designed against: the
  adopted gateway's own posture, unchanged — sequential per-credential probes
  on the diagnosis endpoint (the 429-storm rationale, `server.ts:723-727`)
  and parallel usage fetches under timeout on the selection path
  (`auth-storage.ts:4373-4377`); revisit only if pool sizes grow past wave
  scale.
