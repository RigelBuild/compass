# Network door request limits

Tracker: RIG-5243

## Problem / Intent

Three network-door routes take no bearer token and have no limits or
counters: the GitHub and Linear webhooks, which check only a signature, and
`GET /l/session/{id}`, which reads the store per click. This record adds
both. The RIG-5191 health routes, anonymous per RIG-5230 OQ-3 A, are a fourth
group. The `bodyDeadlineExempt` residual and the pre-auth decode cost stay out
of scope (OQ 3).

## Global Constraints

- `golang.org/x/time v0.15.0` becomes direct; `vendorHash` is unchanged.
- Only anonymous routes are limited; `MaxHeaderBytes` alone is door-wide.
- `X-Forwarded-For` (XFF) is read only from a `--trusted-proxy` peer. A zero
  `ServeConfig` enforces limits and trusts no proxy.
- Metric attributes: `compass.door.group` and `compass.door.outcome` only.
- Tasks are red→green. The PR adds a decision file or `Ledger-impact:`.

## Approach

**Wrapping.** `buildNetworkServer` mounts each anonymous route as
`netMux.Handle(p, limits.wrap(group, h, overflow))`, inside
`withBodyReadDeadline` and before the body read. For a group with a
`readTimeout`, `wrap` first calls `http.NewResponseController(w).SetReadDeadline`,
replacing the door's 30 s deadline. Its status writer implements `Unwrap` and
`Flush`: both webhooks assert `w.(http.Flusher)` to ack before they enqueue.

**Client key.** The TCP peer, unmapped: /32 for IPv4, /64 for IPv6. From a
trusted proxy, join every XFF field line in wire order into one chain and walk
it right to left, skipping trusted entries; the first untrusted entry is the
key. A malformed or all-trusted chain keys the peer.

**Algorithm.** A `golang.org/x/time/rate` bucket per client, behind
per-client and group in-flight caps.

| Group | Rate, burst | Per client | Group | Token spent |
| --- | --- | --- | --- | --- |
| `webhook` (both paths) | 50/s, 500 | 16 | 64 | on a handler 4xx |
| `link` | 1/s, 20 | 2 | 4 | always |
| `health` (L3) | 5/s, 20 | 4 | 32 | always |

A rejected webhook may be lost. GitHub's docs say it does not redeliver
failures automatically, which the `githubDeliveryLRUSize` comment contradicts
(L2 rewords it). Linear retries after 1 minute; whether that retry passes
`CheckTimestamp` (`delta <= skew`, 60 s) is unverified.

Under OQ 1 A, each unsettled webhook request holds a token: admission needs
`TokensAt(now()) >= 1 + held`, `held` being the bucket's unsettled requests, so
concurrent requests cannot share the last token. B and C need no hold. The
webhook read deadline is 10 s, GitHub's timeout; the group cap bounds
buffered bodies to 64 MiB. The link cap holds clicks to 4 of the store's 10
connections.

**Overflow.** Rate or per-client cap: 429. Group cap: 503. Both set
`Retry-After`. Health over budget gets a 1 s cached status (OQ 2).

**Memory.** Each group keeps a `simplelru.LRU[netip.Prefix, *rate.Limiter]`
of 16,384 entries under its mutex; in-flight entries are deleted at zero.
When full, it evicts the oldest bucket only if it is full and idle, so a
returning key loses nothing; otherwise the new key spends from one shared
bucket, and key churn gets no fresh bursts.

**Connections.** `MaxHeaderBytes` is `64 << 10`; Go's HTTP/2 server derives
its header-list limit from it. `MaxConcurrentStreams` keeps Go's 250: without
a per-IP connection cap, a stream cap bounds nothing.

**Counters.** Instruments come from `otel.Meter(doorInstrumentationScope)`;
one that fails to build is nil, as in `NewConsumer`.

- `compass.door.requests` outcomes: admitted `ok`, `client_error`,
  `server_error`; limited `rate_limited`, `client_concurrency`,
  `group_concurrency`, `cached`; `would_*` forms when disabled.
- `compass.door.inflight`, `compass.door.clients` and `compass.door.evictions`.
- `compass.door.untrusted_xff` counts XFF from an untrusted loopback or
  private peer, and warns once.

**Flags.** `fs.String` with a `firstNonEmpty` env fallback, like
`--forge-disable-scope-enforcement`.

| Flag | Env | Default |
| --- | --- | --- |
| `--trusted-proxy` (CIDR list) | `COMPASS_TRUSTED_PROXIES` | none; `/0` rejected |
| `--disable-anon-route-limits` (`ParseBool`) | `COMPASS_DISABLE_ANON_ROUTE_LIMITS` | `false` |

Disabled limits still count. Budgets are constants. Health needs no switch,
as its overflow never fails a probe.

**Authenticated routes are out of scope:** per-IP limits there would throttle
users behind NAT, and the Runner.

## Alternatives considered

- `go-chi/httprate` (293 importers): unbounded maps, no concurrency cap, and
  spoofable header keys.
- `ulule/limiter/v3` (341 importers): stale since 2023.
- A per-IP connection cap: it would apply door-wide.

`x/time/rate` has 16,473 importers and is already in the module graph.

## Plan

### L1 — Limiter core

- **Interfaces:**

  ```go
  // go/server/door_limits.go
  const doorInstrumentationScope = "github.com/RigelBuild/compass/go/server"
  type doorGroup string // groupWebhook "webhook", groupLink "link"
  type groupBudget struct {
      rate                                   rate.Limit
      burst, perClient, inflight, maxClients int
      readTimeout     time.Duration // 0 keeps networkBodyReadTimeout
      chargeOnFailure bool
  }
  var defaultBudgets map[doorGroup]groupBudget // the Approach table; webhook: 10s, true
  func newDoorLimits(enforce bool, trusted []netip.Prefix, budgets map[doorGroup]groupBudget, now func() time.Time, log *slog.Logger) *doorLimits
  func (l *doorLimits) clientKey(r *http.Request) netip.Prefix
  func (l *doorLimits) wrap(g doorGroup, next, overflow http.Handler) http.Handler // nil overflow rejects
  ```

  `admit` and `settle` run under the group mutex. `admit` checks the group
  cap, the client cap, then `AllowN(now(), 1)`, or the hold check with
  `chargeOnFailure`. `settle`, from a `defer`, spends with `AllowN` after a
  4xx and frees the slot.
- **Red → green** (`door_limits_test.go`, `now` injected):
  1. `TestDoorLimitsRate`: on link, request burst+1 gets 429 with
     `Retry-After`; another peer passes; a refill readmits the first.
  2. `TestDoorLimitsChargeOnFailure`: on webhook, burst 400s make the next
     request 429; 2×burst 200s all pass; with one token left, a second
     concurrent request gets 429.
  3. `TestDoorLimitsConcurrency`: perClient+1 gets 429 and the group cap gets
     503; an overflow handler serves both; a panic frees the slot.
  4. `TestDoorLimitsClientKey` (table): one /64 shares a key; v4-mapped is
     unmapped; a trusted chain gives its first untrusted entry, across two
     XFF lines; an all-trusted or malformed chain gives the peer; untrusted
     loopback XFF is ignored and counted.
  5. `TestDoorLimitsBounded`: with `maxClients` 2, a third peer evicts a full
     bucket, or spends the shared one when both are drained; 100 rotating
     peers get at most burst plus refill.
  6. `TestDoorLimitsMetrics`: a `ManualReader` sees every outcome, each
     `would_*` form included, and the `inflight`, `clients` and `evictions`
     values.
  7. `TestDoorLimitsWriter` (`httptest`): a body still arriving after
     `readTimeout` fails; a handler's flush reaches the client before it
     returns.

### L2 — Door wiring, flags, spec

- **Interfaces:**
  - Add `ServeConfig.TrustedProxies []netip.Prefix` and
    `ServeConfig.DisableAnonRouteLimits bool`.
  - `buildNetworkServer` keeps its signature. It builds
    `newDoorLimits(!cfg.DisableAnonRouteLimits, cfg.TrustedProxies, defaultBudgets, time.Now, slog.Default())`
    and wraps both webhooks and the link with a nil overflow.
  - Add `networkMaxHeaderBytes = 64 << 10` and
    `parseTrustedProxies(raw string) ([]netip.Prefix, error)`.
  - Reword the `bodyDeadlineExempt` and `githubDeliveryLRUSize` comments.
  - Amend `docs/specs/product/compass.md` §The network door.
- **Red → green:**
  1. `TestNetworkDoorLimitsAnonymousRoutes` (pgtest): link request burst+1
     gets 429, and a bearer `GetServerInfo` from the same peer passes.
  2. `TestNetworkDoorHeaderBound` (TLS): a 128 KiB header gets 431 on
     HTTP/1.1 and HTTP/2, and 32 KiB passes. HTTP/2 hand-encodes HPACK; Go's
     client refuses an over-limit list (`ErrRequestHeaderListSize`).
  3. `TestBuildServeConfigDoorLimits`: the flags and env vars parse, and `/0`,
     a bad CIDR and `maybe` fail.

### L3 — Health group (after RIG-5191 H1)

- **Interfaces:**
  - `groupHealth: {rate: 5, burst: 20, perClient: 4, inflight: 32, maxClients: 16384}`.
  - `func newCachedHealth(inner grpchealth.Checker, draining func() bool, ttl time.Duration, now func() time.Time) grpchealth.Checker`
    caches each known service's status for `ttl`, bypassed while draining.
  - `func healthzHandler(c grpchealth.Checker) http.Handler`.
  - `func (h *serverHealth) mountLimited(mux *http.ServeMux, l *doorLimits)`
    uses cached overflow on the network door.
- **Red → green:**
  1. `TestCachedHealth`: the inner checker runs once per `ttl`, and a drain
     shows at once.
  2. `TestNetworkDoorHealthOverflow`: request burst+1 succeeds on both views
     and counts as `cached`.

### L4 — Operator docs

- **Interfaces:** `## Network door limits` in `docs/self-host.md`: groups,
  flags, counters; tunnels need `--trusted-proxy 127.0.0.1/32,::1/128`; a lost
  GitHub event is redelivered from Recent Deliveries or
  `POST /app/hook/deliveries/{delivery_id}/attempts`.
- **Red → green:** `rumdl check` passes.

## Tasks

- [ ] L1 — `doorLimits`: key, buckets, caps, read deadline, counters.
- [ ] L2 — wiring, header bound, flags, spec.
- [ ] L3 — health group with cached overflow (blocked on RIG-5191 H1).
- [ ] L4 — `docs/self-host.md` section.

## Open Questions

1. **Webhook bucket (load-bearing).** A rejected delivery may be lost; under
   any option, 64 slow senders still fill the group cap.
   - **A (recommended):** charge on failure. Signed deliveries never drain the
     bucket; garbage is stopped before buffering.
   - **B:** no bucket, just caps and the 10 s deadline. Each garbage request
     costs a 1 MiB read and an HMAC.
   - **C:** charge every request. A GitHub burst over 500 loses events.
2. **Health over budget (load-bearing).**
   - **A:** reject. A flood through a shared LB or tunnel fails the
     operator's own probe.
   - **B (recommended):** serve the cached status. It bounds `Hub.mu`
     contention and never fails a probe.
   - **C:** always cache, no limiter; no per-client counts.
3. **Pre-auth cost (load-bearing).** `NewUnaryHandler` calls
   `receiveUnaryRequest` before `untyped(ctx, request)`, so a tokenless caller
   can make the door buffer 128 MiB, or drip a body to the exempt Runner
   paths.
   - **A (recommended):** a new issue. A header-shape guard passes `Bearer x`;
     the real fix verifies the token before decode and keeps the deadline
     until the Runner authenticates.
   - **B:** add the header-shape guard here as L2b.
4. **Trusted-proxy model (load-bearing).**
   - **A (recommended):** CIDRs and the right-to-left walk.
   - **B:** A plus RFC 7239 `Forwarded`.
   - **C:** PROXY protocol v2, the only fix for an L4 passthrough proxy.
5. **Default budgets (load-bearing).**
   - **A (recommended):** constants plus the kill switch.
   - **B:** a flag per group.
   - **C:** shadow mode first (the kill switch on by default for a release).
6. **IPv6 width (non-load-bearing).** A /48 holds 65,536 /64s, four times the
   LRU, so rotation lands on the shared bucket. If `compass.door.clients`
   sits at its cap, widen the key or add a /48 bucket.
