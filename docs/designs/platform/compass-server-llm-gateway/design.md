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

Make the Compass Server the single LLM egress point for every agent
(RIG-1715): agents never hold upstream provider credentials; a Server-side
gateway holds them all and routes each model call across the user's account
pool with the subscription-limit-maximizing logic the OMP auth-gateway runs
today. The gateway is also the source of the Plane-A token-usage/spend data
that #656 T1 (store), T2 (read gRPC), and T3 (in-app charts) consume — this
record specifies that data contract. Matt's ruling frames the load-bearing
fork: *"survey if we should use the omp gateway or just go straight to a Go
impl directly on the server — less overhead, more scalable, not maintaining a
separate runtime etc on the Server. Porting the logic shouldn't be too hard i
think, and easy to run any tests they have against the current one, and then
also write our own tests to prove the same functionality."*

## Approach

### Recommendation: Go-native gateway, in-process in compass-server (option B)

Skip the "embed the TS gateway first" phase from the original RIG-1715 body
and go straight to a Go implementation living inside `compass-server`,
porting the OMP routing algorithm (characterized below) and proving parity
with a three-layer test strategy. Rationale:

1. **No second runtime in the shipped stack.** The stack supervisor today
   spawns exactly three children — `ComponentPostgres`, `ComponentServer`,
   `ComponentRunner` (`go/internal/stack/deps.go:82-89`) — all Go binaries
   plus postgres. The OMP gateway is a Bun HTTP server
   (`startAuthGateway` calls `Bun.serve`,
   `forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:750-755`), so
   embedding it means shipping and supervising a Bun runtime in every
   self-host install — against the S4 anti-standup-pain posture and the
   all-Go direction (RIG-1719: "The more of the stack that is
   Go/Server-side, the more a Go brain fits") and the move off LiteLLM's
   separate-runtime model (RIG-1716: "implement the MCP gateway ourselves in
   Go ... Composes with the all-Go-stack direction (RIG-1719) and the Go
   LLM-gateway follow-up (RIG-1715 phase 2)").
2. **Credentials and usage events belong to the Server's store.** The
   compass secrets registry already models provider credentials
   (`SECRET_KIND_PROVIDER = 2;  // LLM provider credential; carries provider
   id`, `proto/compass/v1/compass.proto:169`; routing enum
   `go/internal/secrets/secrets.go:54-57`). An in-process Go gateway reads
   them through the existing store package and writes token-usage events
   through the same store — no cross-runtime IPC, no second credential
   database (the TS gateway keeps its own SQLite/auth-broker store,
   `AuthStorage.create` opens `SqliteAuthCredentialStore`,
   `forks/oh-my-pi/packages/ai/src/auth-storage.ts:1329-1332`).
3. **The agent→gateway auth model needs Server identity anyway.** Requests
   must map an agent token to an account pool; compass-server already owns
   agent identity and the agent tree (`agent_accounts.parent_agent_id` ...
   "the fleet's reporting spine", `go/internal/store/migrations/0001_init.sql:56-66`).
   The TS gateway authenticates with a flat static bearer set
   (`isAuthorized(req, tokens)` over `Set<string>(opts.bearerTokens)`,
   `server.ts:752,772`) and has no per-caller identity beyond a peer address
   — tenanting it would mean forking it anyway.
4. **The port is bounded because Compass's ingress is narrower than OMP's.**
   The TS gateway's bulk is wire-format translation for foreign SDKs plus a
   long tail of providers: `server.ts` is 836 lines, but the machinery it
   leans on — `auth-storage.ts` at 7,936 lines, ~17 per-provider usage
   fetchers under `src/usage/`, and per-provider OAuth refresh under
   `src/registry/oauth/` — is the real mass. Compass day-1 needs only the
   providers its agents actually use (see the provider matrix, T2 below);
   the ranking core itself is small and precisely specified (below).

**Cost of B, stated honestly:** the Go gateway forks from a living upstream.
The OMP fork keeps accreting provider quirks (e.g. the Codex sampling-control
strip, `server.ts:132-145`; Claude-Code OAuth system-prompt shaping noted at
`server.ts:79-82`), and a port stops inheriting them. Mitigation: pin parity
to the fork at the port commit, scope day-1 providers narrowly, and accept
that compass-server owns its gateway from then on — which is exactly the
RIG-1715 phase-2 end state; option B just skips paying for phase 1 twice.

### The routing algorithm to port (the valuable part)

This is the precise characterization Matt asked for ("document the OMP
gateway's current routing before the rewrite so the Go version matches",
RIG-1715 body). All citations are the parity spec for the Go port.

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

**Parity-critical ranking details** (in the source, load-bearing for the
port — a Go version built against the summary above would diverge on each):

- **Unmeasured usage normalizes to 0.5, not 0.** A candidate with no usable
  usage report is treated as half-used, not empty (`#normalizeUsageFraction`,
  `auth-storage.ts:4262-4268`) — an unmeasured account must not falsely rank
  as most-drainable.
- **`remainingMs` is clamped to the window's own `durationMs`**
  (`auth-storage.ts:4283-4285`) in addition to the one-minute floor: a
  just-reset window cannot claim more remaining time than the window is long.
- **openai-codex re-checks blocked credentials live during ranking.** Blocked
  codex candidates get a live usage re-fetch inside selection and can unblock
  mid-rank (`#rankOAuthSelections`, `auth-storage.ts:4382-4394`) — I/O on the
  selection path, and a day-1 provider. Its nondeterminism lands on the
  layer-3 black-box suite, not the golden fixtures (see Parity-test strategy).

**Account pools** (option A's multi-tenancy seam, for reference): a
`ReadonlyMap<provider, ReadonlySet<identityKey>>` filter over the broker
snapshot — an OAuth credential is visible only if its identity is in the
pool (`isCredentialInAccountPool`,
`forks/oh-my-pi/packages/ai/src/auth-broker/remote-store.ts:41-51`). The Go
design replaces this file/env-configured filter
(`loadAuthBrokerAccountPool`, `auth-broker/discover.ts:119-167`) with
store-backed per-user pools (below).

**Not ported** (deliberate scope cuts, each with why):

- The three foreign-SDK wire formats stay, but the **pi-native fast path**
  (`/v1/pi/stream`, `server.ts:530-543`) is not ported — it exists to skip
  translation for OMP-internal callers speaking pi-ai's canonical `Context`;
  Compass agents run OMP configured for provider-format endpoints, and a Go
  gateway has no pi-ai internals to shortcut into.
- The broker/remote-store layer (`auth-broker/`): the Go gateway's
  credential source is the compass store, not an external broker.
- The prompt-cache-key derivation IS ported (`deriveSessionId`) — it is
  load-bearing for both prefix caching and credential stickiness.

### Gateway architecture on the Server

**Placement: an in-process component of `compass-server`** — a new
`go/internal/llmgateway` package started by the serve loop, not a fourth
supervised child. compass-server is "a thin wrapper over server.Serve"
(`go/cmd/compass-server/main.go:3-7`); the gateway is one more listener the
serve loop owns and drains. In-process because the gateway must read
credentials and write usage events through the store package, and a separate
binary would need its own DB pool, its own TLS story, and a fourth teardown
entry in the stack. The isolation tradeoff is real, not zero: in-process the
gateway shares compass-server's fate *downward* (no server, no agents — true
of a child too), but a supervised child would contain the *upward* blast
radius an in-process gateway cannot — a gateway resource pathology (leaked
SSE streams, a runaway usage-fetch fan-out, DB-pool exhaustion) degrades the
compass.v1 control door instead of restarting in isolation. In-process is
chosen anyway because the decisive invariant is that upstream provider
credentials never cross a process boundary, and the blast radius is bounded
by posture rather than a process wall: the gateway runs on a reserved
DB-connection sub-pool (never the control plane's), caps concurrent upstream
streams per owner, and bounds the usage-fetch fan-out (OQ-6). The
supervised-child variant is weighed as B' in `## Alternatives considered`,
and promoting to it later is a contained refactor (OQ-7).

**Listener**: a dedicated HTTP listener (default loopback+LAN-reachable TCP,
configurable bind), NOT routes on the compass.v1 Connect door. Two reasons:
(1) callers are LLM SDKs inside agent containers speaking
OpenAI/Anthropic-format HTTP + SSE, not compass.v1 clients — the compass.v1
contract stays the UI↔server door ("the single, owned door between any UI
and the Compass server", `proto/compass/v1/compass.proto:1-4`); (2) SSE
streaming with provider-format error envelopes is idiomatic plain
`net/http`, and the gateway's availability must not couple to Connect
middleware. Routes mirror the TS gateway (`server.ts:769-806`): `/healthz`,
`POST /v1/chat/completions`, `POST /v1/messages`, `POST /v1/responses`,
`GET /v1/models`, `GET /v1/usage`, `GET /v1/credentials/check`.

**Agent→gateway auth**: per-agent bearer tokens minted by the Server. The
Runner already materializes per-agent secrets into containers over the
stdin-exec channel, including the provider seed
(`ProviderSeed` → 0600 `$HOME/.compass/auth-seed.json`,
`go/internal/runtime/secrets_materialize.go:72-77`). Under this design the
materialized artifact changes meaning: instead of raw provider keys, the
seed carries ONE credential — the gateway base URL + the agent's gateway
token (delivered as env: `COMPASS_LLM_GATEWAY_URL`,
`COMPASS_LLM_GATEWAY_TOKEN`); OMP-in-container points its provider base URLs
at the gateway. The token maps server-side to
`agent_account_id → owner_user_id` (both already on `agent_accounts`,
`0001_init.sql:74-80`), which selects the account pool and stamps usage
attribution. Tokens are random 256-bit values stored hashed, revocable, and
rotated on session re-placement.

**Credential storage and rotation**: the gateway is the only holder of
upstream provider credentials. Source of truth is the existing secrets
registry (`SECRET_KIND_PROVIDER` rows carry a provider id,
`go/internal/store/secrets.go:120-125`), extended with OAuth-shaped
credential payloads (access/refresh/expiry) alongside today's api_key —
the TS seed comment already anticipates this ("an OAuth extension is
additive", `secrets_materialize.go:55-58`). OAuth refresh runs gateway-side
(single-flight per credential, mirroring
`#oauthRefreshInFlight`, `auth-storage.ts:1283-1284`); refreshed tokens are
written back to the store. Raw provider credentials STOP being materialized
into agent containers once the gateway ships (the flag-day is T6).

**Account pools: per-user, day-1.** A pool is "the provider credentials
owned by user U", keyed by the agent's `owner_user_id`. Self-host is
single-team, so per-user pools degrade gracefully to "the operator's
accounts"; the managed plane's per-org pools are the same query keyed by org
— the pool-resolution seam (`PoolResolver` in T3) is where the managed plane
swaps in org scoping. This mirrors the OMP account-pool filter semantics
(provider → allowed identity set) but sourced from the store instead of a
JSON file.

### The Plane-A data contract (what #656 T1/T2/T3 build against)

This section IS the seam. #656 already fixes the frame: "the OMP gateway
**to be bundled into the Server** records per-account/org **token usage +
spend** (the display quantity)"
(`compass-observability-architecture/design.md:61-64`); "Plane A (gateway →
Postgres) is authoritative for usage/spend"
(`design.md:445-451`). This record commits the concrete shapes.

**1. The token-usage event (gateway → store, append-only).** One event per
completed upstream model call (including failed/aborted calls with partial
usage, flagged). Shape (Go struct = the write contract **#656 T1** stores and
**this record's T5** emits; field set derived from pi-ai's canonical `Usage` —
`input/output/cacheRead/cacheWrite/totalTokens` +
`cost{input,output,cacheRead,cacheWrite,total}`,
`forks/oh-my-pi/packages/catalog/src/types.ts:95-149`):

```go
// package llmusage — the write contract #656 T1 stores; this record's T5 emits it.
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
append-only, idempotent on `ID`, batched (the gateway buffers and flushes on
a short interval; loss window ≤ flush interval is acceptable for a display
quantity per #656 D5 — this is explicitly NOT the billing-grade
compute-usage event, which is runtime-sourced and out of this record's
scope, `design.md:552-567`).

Cost is integer micro-USD (no floats in the store), computed at write time
from a Go pricing table keyed by (provider, model, `RateVersion`) that retains
**per-component rates** (input/output/cache-read/cache-write) — the port of
the catalog's per-model cost rates (this record's **T2b** carries the table;
staleness handled by recording the rate-version used).

The canonical `Usage` also reports orchestration tokens (billed but outside
the conversation prompt/cache buckets, `catalog/src/types.ts:106-113`),
reasoning tokens, and server-tool counts. These are deliberately NOT carried
as separate `TokenUsageEvent` fields (a display quantity, not a billing-grade
breakdown); the T5 mapping folds orchestration + reasoning tokens into
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
record's T5 + `UsageService`).** New RPCs on a new
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
  // served from the gateway's cached provider usage reports (the Go port of
  // GET /v1/usage, server.ts:708-715).
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
live provider-side snapshot (gateway cache, 5-min TTL semantics mirroring
`server.ts:702-707`), series is our own event-log history.

### Cost attribution per agent / tree node (RIG-1713 composition)

Every event carries `AgentAccountID`; the agent tree is the store's
`agent_accounts.parent_agent_id` spine (`0001_init.sql:56-66`). Subtree
rollups are a read-time recursive-CTE aggregation exposed via
`include_subtree` — attribution is stored flat (leaf-level), never
denormalized up the tree, so reparenting (`store.ReparentAgent`) never
rewrites the event log. Semantics to ratify: because the CTE walks the
*current* parent edges at read time, a subtree query reflects the tree
as-of-now, not as-of-event — reparenting a child retroactively moves its
historical cost under the new parent in subtree rollups (leaf attribution is
unchanged). This as-of-now choice is the simple, denormalization-free
default; the as-of-event alternative (freezing each event's ancestry) is
weighed in OQ-9. RIG-1713's message-cost estimator gains a real price
source: per-agent recent $/token from the rollups replaces guesswork
(RIG-1713 asks "how to price an idle subscriber's re-load"; the estimate
stays that record's scope — this record only guarantees the queryable
per-agent cost series it needs).

### Parity-test strategy (Matt's explicit ask)

Three layers, matching the ruling ("run any tests they have against the
current one, and then also write our own tests"):

1. **Characterize the reference.** The OMP fork already carries the spec
   suite: 14 `auth-gateway-*.test.ts` files under
   `forks/oh-my-pi/packages/ai/test/` (wire formats, caching keys,
   cross-protocol caching, error classification, model list, response
   headers) plus `forks/oh-my-pi/packages/coding-agent/test/auth-gateway-account-pool.test.ts`
   (pool filtering). Run them against the fork at the pin commit; their
   assertions are the behavioral spec.
2. **Language-neutral golden fixtures.** Extract the routing-decision cases
   into JSON fixtures: (candidate set with usage windows/blocks) → expected
   ranking order, and (error input) → expected action
   (refresh-same/switch-sibling/block-until). A tiny TS harness generates
   expected outputs from the fork's comparator; the Go suite replays the
   same fixtures against the port. This pins the comparator semantics
   (hot-window guard, required-drain ordering, blocked-until tiebreaks)
   byte-for-byte without cross-language test infrastructure.
3. **Go-native black-box suite.** httptest-driven: fake provider upstreams
   (401, `usage_limit_reached` with `retry-after`, SSE streams), assert the
   a/b/c retry ladder, sticky sessions, sibling switch, SSE passthrough,
   and usage-event emission. Store code follows the DL-174 pyramid
   (in-memory ref + `pgtest`, `compass-test-strategy/design.md:123-128` via
   #656 Global Constraints).

**Fixture coverage boundary.** The golden fixtures (layer 2) pin only the
pure comparator — a total function of candidate set + `now`. Behaviors that
are stateful or do live I/O — round-robin/session ordering, the all-blocked
fallback, the openai-codex live re-check during ranking, and the
usage-fetch-timeout-yields-null path that flips comparator step 5
(`auth-storage.ts:4373-4381`) — cannot be pinned by deterministic fixtures
and are owned by the layer-3 black-box suite. Layer 1 (the TS suite at the
pin) certifies the reference implementation only; it proves nothing about the
Go port.

## Alternatives considered

### A — Embed the OMP TS auth-gateway as a supervised Server component (rejected)

What it was: RIG-1715 phase 1 — add a fourth supervised child (Bun running
`startAuthGateway`) to the stack chain (`go/internal/stack/stack.go:192-256`),
config-bridged from compass-server; known-good and load-tested by weeks of
wave use.

Why it lost:

- **A separate runtime in every install.** Bun becomes a shipped, supervised
  dependency of the all-Go stack — the exact overhead Matt named ("less
  overhead, more scalable, not maintaining a separate runtime etc on the
  Server").
- **Two credential stores.** The TS gateway resolves credentials from its
  own SQLite/auth-broker store (`auth-storage.ts:1329-1332`,
  `auth-broker/remote-store.ts`), while compass already routes provider
  credentials through its secrets registry
  (`proto/compass/v1/compass.proto:168-171`). Embedding means syncing the
  two or bridging every credential write.
- **No per-agent identity.** Its auth is a flat bearer set
  (`server.ts:752,772-775`); Plane-A attribution (per agent/user/org) would
  require forking the gateway anyway — at which point we are maintaining a
  divergent TS fork instead of a Go implementation.
- **Usage events would cross a runtime boundary.** Token-usage recording
  into Postgres from a Bun child means either an ingestion RPC on the
  server or direct DB access from TS — both new seams option B gets for
  free in-process.
- What it kept that B must not lose: battle-tested routing (ported via the
  fixture parity suite) and the wire-format fidelity of the three foreign
  SDK endpoints (covered by the ported `auth-gateway-*` test assertions).

### A' — Embed TS now, port to Go later (the issue's original two-phase plan; rejected)

Pays the full embedding cost (supervision, config bridge, credential sync,
attribution fork) for a component pre-decided to be replaced. The wave
already runs the TS gateway fine OUTSIDE the product; nothing in beta needs
it embedded before the Go gateway lands. Matt's ruling supersedes the
two-phase plan.

### C — Adopt an existing Go LLM proxy (LiteLLM-class, e.g. a Go gateway OSS) (rejected)

The valuable behavior is the subscription-limit-maximizing pool routing
(required-drain ranking, hot-window guard, reset-aware blocking) — no
off-the-shelf proxy has it, and RIG-1716 already sets the posture for the
sibling MCP gateway: "implement ... ourselves in Go, unless research turns
up a clearly better option," with LiteLLM explicitly the thing being moved
off ("buggy and slow (Python)"). An external proxy would still need the
compass store integration (credentials, attribution, usage events), which
is most of the work.

### B' — Go-native gateway as a fourth supervised child (rejected, narrowly)

The same Go port as B, but running as its own supervised process in the stack
chain (`go/internal/stack/deps.go:82-89`) rather than in-process. Unlike A it
shares none of the Bun/second-credential-store costs — the only axis it
differs from B on is process isolation. Why it lost: it reintroduces the
per-binary overhead B avoids (its own DB pool, TLS anchor, config bridge, a
fourth teardown entry) and — decisively — a credential-holding child puts the
provider secrets behind a second process wall the Runner's secrets path would
have to cross, widening the credential surface rather than narrowing it. What
it would buy — upward blast-radius containment (a gateway crash restarts
without dropping the compass.v1 door) — is instead bought by the in-process
bounded-resource posture (reserved DB sub-pool, per-owner stream caps,
bounded usage fan-out). If that posture proves insufficient under load,
promoting the gateway to a supervised child is a contained, reversible
refactor: the package boundary (`go/internal/llmgateway`) is identical either
way. Surfaced to Matt as OQ-7.

## Global Constraints

- **All-Go server** (RIG-1719 direction): no new non-Go runtime in the
  shipped stack; gateway code lives under `go/internal/`.
- **`compass.v1` is the sole UI↔server door** (`compass.proto:1-4`;
  `AGENTS.md:49-58` per #656): the usage read API is a proto change with
  regenerated clients. The gateway's provider-format HTTP listener is
  agent-facing, never UI-facing.
- **Agents never hold upstream provider credentials** once T6 lands; the
  gateway is the only holder (RIG-1715 core invariant).
- **Plane A is authoritative for usage/spend**; token-usage events are the
  display quantity, NOT the billing-grade compute-usage contract
  (#656 D5, `compass-observability-architecture/design.md:445-451,552-567`).
- **Store discipline**: append-only event writes; DDL folds into the
  squashed migration (`0001_init.sql:10-15`); new store code ships an
  in-memory reference + `pgtest` suite (DL-174 pyramid).
- **Money as integer micro-USD** in events, store, and wire — never floats.
- **Parity is pinned** to the OMP fork at the port commit; later fork
  changes are explicitly not auto-inherited.
- Markdownlint-clean record; Conventional Commits;
  `Co-authored-by: Matt Wilkinson <matt@rigel.build>` on the record commit
  (driver-owned).

## Plan

Ordering: T1 → T2/T2b/T3 (parallel) → T4 → T5 → T6; T7 tracks the #656
handoff. T5 additionally depends on #656 T1's `UsageStore` landing (external
compass-server lane — coordinate; this record supplies the event source T1
declared as its prerequisite), so "T4 → T5" is gated on that cross-record
dependency, not just T4.
Day-1 provider set: anthropic (OAuth + api_key), openai/openai-codex
(OAuth + api_key), google (api_key) — the providers the wave's agents use;
everything else is post-parity accretion.

### T1 — Routing-core port + golden fixtures

The pure decision core, no I/O: candidate ranking (comparator semantics per
the Approach characterization), block bookkeeping, required-drain math,
session stickiness, and the a/b/c retry ladder as a state machine.

Interfaces:

- Consumes: golden fixtures generated from the OMP fork's comparator
  (`auth-storage.ts:4269-4343`) by a TS harness added under
  `forks/oh-my-pi/packages/ai/test/` (fixture-emit mode, not shipped).
- Produces: package `go/internal/llmgateway/routing` with
  `func Rank(candidates []Candidate, now time.Time, req Requirement) []Candidate`,
  `func (b *BlockBook) Blocked(credID string, scopes []string, now time.Time) (until time.Time, ok bool)`,
  `func NextAction(err UpstreamError, lastChance bool) Action` (Action ∈
  RefreshSame | SwitchSibling | Fail); fixture replay test
  `routing/fixtures_test.go` green against the same JSON the TS harness
  emits.
- Test cycle: fixture replay + property tests (ranking is total, stable,
  block-respecting).

### T2 — Provider clients (streaming + auth)

Upstream HTTP for the day-1 provider set: request shaping (incl. the
Claude-Code OAuth system-prompt shaping and Codex sampling-strip semantics,
`server.ts:79-90,132-145`), SSE decode/re-encode, error classification
(usage-limit vs auth-failure phrasing per `server.ts:210-215`), token-usage
extraction into the canonical fields (`catalog/src/types.ts:95-149`), and
OAuth refresh (single-flight per credential). No usage-report or pricing
logic — that is T2b. Independent of T2b (different consumers: the request
pipeline vs the ranking/quota path); the two run in parallel.

Interfaces:

- Consumes: T1's `Action`/`Candidate` types; provider wire docs. Ingress is
  assumed provider-native (an agent's anthropic base URL hits `/v1/messages`
  → anthropic upstream) — passthrough + credential shaping, NOT cross-format
  translation; whether cross-format ingress (OpenAI-format → anthropic model)
  is a supported surface is OQ-8, and a "yes" materially enlarges this task.
- Produces: `go/internal/llmgateway/provider` with
  `type Client interface { Stream(ctx, Request, cred Credential) (EventStream, error) }`
  per provider; `func Classify(status int, body string) ErrorClass`;
  `func Refresh(ctx, cred Credential) (Credential, error)`.
- Test cycle: httptest fake upstreams; recorded SSE fixtures; error-class
  unit tests.

### T2b — Usage fetchers + pricing table

Independent of T2's streaming path: per-account provider usage-report
fetchers (anthropic + codex windows) that feed the ranking comparator and
`GetProviderQuota`, plus the (provider, model, rate-version) → per-component
micro-USD pricing table (input/output/cache-read/cache-write rates retained,
not just a total — that is what makes the event's per-component cost
reconstructable, Approach §Plane-A).

Interfaces:

- Consumes: the fork's usage fetchers as reference
  (`forks/oh-my-pi/packages/ai/src/usage/claude.ts`, `usage/openai-codex.ts`);
  the canonical cost rates (`catalog/src/types.ts:95-149`).
- Produces: `FetchUsage(ctx, cred Credential) (*QuotaReport, error)` per
  provider; `func Price(provider, model string, u TokenCounts) (total int64, perComponent ComponentCost, rateVersion string)` — returns the micro-USD total AND the per-component breakdown from the versioned per-component rates.
- Test cycle: httptest fake usage endpoints; pricing-table unit tests.

### T3 — Credential store + pool resolution

Extend the secrets registry for OAuth payloads and wire pool resolution:
per-user pools keyed by `owner_user_id`; refresh write-back; disable/CAS
semantics mirroring `#tryDisableCredentialAtIfMatches`
(`auth-storage.ts:2112-2123`) so a racing rotation is never clobbered.
Extending the credential payload for OAuth (access/refresh/expiry) plausibly
needs a `compass.proto` change — `SetSecretRequest` today carries only
kind + provider (`proto/compass/v1/compass.proto:193-195`) — so this task may
include a proto + regen step, not only Go store code.

Interfaces:

- Consumes: `go/internal/store` secrets tables (`store/secrets.go`);
  `agent_accounts.owner_user_id` (`0001_init.sql:74-80`).
- Produces: `type PoolResolver interface { Pool(ctx, agentAccountID string) ([]Credential, error) }`
  (the managed-plane org-scoping swap seam);
  `type CredentialStore interface { List(ctx, userID, provider string) ([]Credential, error); UpdateOAuth(ctx, id string, tok OAuthToken, expectedVersion int64) error; Disable(ctx, id, cause string, expectedVersion int64) error }`.
- Test cycle: in-memory ref + `pgtest` (DL-174); CAS race tests.

### T4 — Gateway HTTP listener + agent auth

The `net/http` listener: the three provider-format endpoints + `/healthz`,
`/v1/models`, `/v1/usage`, `/v1/credentials/check`; per-agent bearer minting
(random 256-bit, stored hashed, revoked on session end), constant-time
verification (posture of `timingSafeEqual`, `http.ts:58-76`); request
pipeline gluing T1-T3 (session-id derivation port of
`deriveSessionId`, `server.ts:108-127`; abort mirroring; SSE with
anti-buffering headers per `server.ts:515-527`); wired into `server.Serve`'s
lifecycle (start with, drain with).

Interfaces:

- Consumes: T1 routing, T2 provider clients, T2b pricing/usage, T3 pools;
  `server.ServeConfig` (new `LLMGatewayBind` field, flag
  `--llm-gateway-listen`, default off until T6).
- Produces: `func New(cfg Config, deps Deps) *Gateway` with
  `Run(ctx) error` / graceful drain;
  `type TokenMinter interface { Mint(ctx, agentAccountID string) (token string, err error); Verify(ctx, token string) (agentAccountID string, err error) }`.
- Test cycle: black-box httptest suite (the layer-3 parity tests): retry
  ladder, sticky sessions, usage-limit sibling switch, 401 invalidation,
  SSE integrity, auth rejection.

### T5 — Token-usage events + UsageService read path

Emit `TokenUsageEvent` per completed call (batched flush, idempotent on ID)
into T1(#656)'s `UsageStore`; implement `UsageService.GetUsageSeries`
(rollup query + recursive-CTE subtree aggregation over
`agent_accounts.parent_agent_id`) and `GetProviderQuota` (gateway quota
cache, 5-min TTL semantics per `server.ts:702-707`).

Interfaces:

- Consumes: the `TokenUsageEvent` shape (Approach §Plane-A); #656 T1's
  `UsageStore` (owner: compass-server lane — coordinate, this task supplies
  the event source #656 T1 declared as its prerequisite); T2b's `QuotaReport`
  (for `GetProviderQuota`); `proto/compass/v1` + `moon run compass-proto:gen`.
- Produces: `usage.proto` (UsageService as specified in Approach);
  registered Connect handlers with server-side tenant scoping; the gateway's
  `flushUsage(ctx) error` writer.
- Test cycle: pgtest rollup/read tests; proto-regen CI green; subtree
  aggregation correctness against a seeded tree.

### T6 — Agent egress cutover

Flip agents to gateway-only egress: Runner materializes
`COMPASS_LLM_GATEWAY_URL`/`COMPASS_LLM_GATEWAY_TOKEN` instead of raw
provider keys in the auth-seed (`secrets_materialize.go:72-77,158-182`);
OMP-in-container config points provider base URLs at the gateway; raw
`SECRET_KIND_PROVIDER` values stop leaving the Server. Gated on T4+T5 being
proven in a wave dogfood run.

Interfaces:

- Consumes: T4's minted tokens (Runner obtains them via the existing
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
- Test cycle: runner materialization unit tests; end-to-end agent smoke
  (agent completes a model call with NO provider secret in its container).

### T7 — #656 handoff note

One-line dependency flip: #656's "PREREQUISITE ... OMP-gateway-into-Server"
(`compass-observability-architecture/design.md:698-699`) is satisfied by
this record; #656 T1 consumes `TokenUsageEvent`, T2's usage RPCs are
superseded-by/merged-into this record's `UsageService` (single service, no
duplicate proto surface — coordinate at freeze; when #656 is next touched,
annotate its T2 `Produces` line "superseded by RIG-1715 `UsageService`" so the
deferral is visible from #656's side, not only via its PREREQUISITE gate).
T3 (compass-ui) consumes

## Tasks

- [ ] T1 — Routing-core port + golden fixtures (Owner: compass-server) —
      `llmgateway/routing`, fixture parity green.
- [ ] T2 — Provider clients (Owner: compass-server) —
      anthropic/openai-codex/google, SSE streaming, error classes, OAuth refresh.
- [ ] T2b — Usage fetchers + pricing table (Owner: compass-server) —
      per-account quota reports, (provider, model) → micro-USD pricing + rate-version.
- [ ] T3 — Credential store + pool resolution (Owner: compass-server) —
      OAuth payloads in secrets registry, PoolResolver, CAS rotation.
- [ ] T4 — Gateway listener + agent auth (Owner: compass-server) —
      provider-format endpoints, token mint/verify, black-box parity suite.
- [ ] T5 — Usage events + UsageService (Owner: compass-server) —
      TokenUsageEvent writer, GetUsageSeries/GetProviderQuota on compass.v1.
- [ ] T6 — Agent egress cutover (Owner: compass-server + runner) —
      gateway-only creds in containers, reversible flag.
- [ ] T7 — #656 handoff (Owner: compass-obs) — dependency flip + proto
      dedup note.

## Open Questions

- **OQ-1 [load-bearing] — Skip the embed phase entirely?** This record
  recommends going straight to Go (option B), overriding RIG-1715's written
  two-phase plan. Matt's ruling leans this way but frames it as a survey
  question. Recommendation: confirm B; the survey found the embed phase
  buys nothing beta needs (agents already use the external TS gateway
  during the wave) and costs a supervised Bun runtime + credential-store
  bridge that gets thrown away.
- **OQ-2 [load-bearing] — Day-1 provider set.** Designed against: anthropic,
  openai/openai-codex, google. Every additional provider is a client + usage
  fetcher + pricing rows in T2. Recommendation: confirm this set; add
  others post-parity.
- **OQ-3 [load-bearing] — UsageService ownership vs #656 T2.** #656 T2
  sketches `GetUsageSeries(tenant, account?, window, granularity)`
  (`design.md:588-592`); this record commits the concrete proto (adding
  `GetProviderQuota` and subtree rollup). Recommendation: this record's
  `UsageService` IS #656 T2's deliverable (one service, compass-server
  lane); ratify so the two records don't mint duplicate proto surfaces. On
  any conflict between the two records' usage-read text, THIS record's
  `UsageService` shape is authoritative (it carries the concrete proto) and
  #656 T2 defers to it.
- **OQ-4 [non-load-bearing] — Gateway listener transport.** Designed
  against: plain TCP bind reachable from agent containers, bearer-token
  auth, TLS via the stack's existing anchor when bound beyond loopback.
  A Unix-socket-per-runner variant is a later hardening option; the token
  model is transport-independent.
- **OQ-5 [non-load-bearing] — Usage-event flush cadence.** Designed
  against: 10s batched flush (mirrors the broker's observed-usage default,
  `auth-broker/remote-store.ts:237-238`), loss window acceptable for a
  display quantity. Tunable later without contract change (idempotent IDs).
- **OQ-6 [non-load-bearing] — Provider quota-fetch fan-out limits.**
  Designed against: sequential per-credential probes on the diagnosis
  endpoint (the 429-storm rationale, `server.ts:723-727`) and parallel
  usage fetches under timeout on the selection path
  (`auth-storage.ts:4373-4377`) — same posture as the fork; revisit only if
  pool sizes grow past wave scale.
- **OQ-7 [load-bearing] — Gateway placement: in-process vs supervised
  child.** This record recommends in-process (creds never cross a process
  boundary; blast radius bounded by the reserved-sub-pool / stream-cap
  posture). The alternative (B') is a fourth supervised Go child, trading a
  wider credential surface for hard upward blast-radius isolation.
  Recommendation: in-process now, package boundary kept clean so promotion to
  a child is a contained refactor if load demands it.
- **OQ-8 [load-bearing] — Cross-format ingress.** Must the gateway accept one
  provider's wire format and route to a different provider's model
  (OpenAI-format request → anthropic model), as the OMP gateway does? This
  record assumes NO — ingress is provider-native (passthrough + shaping),
  keeping T2 to per-provider clients rather than an N×N translation matrix.
  Recommendation: confirm provider-native-only for day-1; a "yes" is a scoped
  T2 expansion, not a redesign.
- **OQ-9 [load-bearing] — Subtree cost attribution: as-of-now vs
  as-of-event.** Subtree rollups walk the *current* parent tree at read time,
  so reparenting an agent retroactively moves its historical cost under the
  new parent. Recommendation: keep as-of-now (no denormalization, matches how
  the tree is browsed); the as-of-event alternative (stamp each event's
  ancestry at write time) is available if audit needs frozen lineage, at the
  cost of a wider event row and no free reparenting.
