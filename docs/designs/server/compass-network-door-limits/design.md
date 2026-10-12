# Network door request limits

Tracker: RIG-5243

## Open Questions

1. **Webhook bucket (load-bearing).** A rejected delivery may be lost: GitHub
   does not redeliver, and Linear's 1-minute retry may fail the 60 s
   `CheckTimestamp` (unverified). Under any option, 64 slow senders fill the
   group cap.
   - **A (recommended):** charge on failure. Signed deliveries never drain the
     bucket; garbage is stopped before buffering.
   - **B:** caps and the 10 s deadline only; each garbage request costs a
     1 MiB read and an HMAC.
   - **C:** charge every request; a GitHub burst over 500 loses events.
2. **Health over budget (load-bearing).**
   - **A:** reject. A flood via a shared LB or tunnel fails the operator's
     probe.
   - **B (recommended):** serve the cached status, bounding `Hub.mu`
     contention without failing a probe.
   - **C:** always cache, no limiter or per-client counts.
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
   - **C:** shadow mode first (kill switch default-on for one release).
6. **IPv6 width (non-load-bearing).** A /48 holds 65,536 /64s, so rotation
   lands on the shared bucket. If `compass.door.clients` sits at its cap,
   widen the key or add a /48 bucket.

## Problem / Intent

Three anonymous network-door routes have no limits or counters: the signed
GitHub and Linear webhooks, and `GET /l/session/{id}`, which reads the store
per click. This record adds both, and a group for the RIG-5191 health routes
(anonymous per RIG-5230 OQ-3 A).

## Global Constraints

- `golang.org/x/time v0.15.0` and, for the L2 test, `golang.org/x/net v0.60.0`
  become direct; `go.sum` and `vendorHash` are unchanged.
- Only anonymous routes are limited; `MaxHeaderBytes` alone is door-wide.
- `X-Forwarded-For` (XFF) is read only from a `--trusted-proxy` peer. A zero
  `ServeConfig` enforces limits and trusts no proxy.
- Metric attributes: `compass.door.group` and `compass.door.outcome` only.
- Tasks are red→green. The PR adds a decision file or `Ledger-impact:`.

## Approach

**Wrapping.** `buildNetworkServer` mounts each anonymous route as
`netMux.Handle(p, limits.wrap(group, h, overflow))` inside
`withBodyReadDeadline`, before the body read. `wrap` swaps the door's 30 s
deadline for the group's `readTimeout` via
`http.NewResponseController(w).SetReadDeadline`. Its status writer keeps
`Unwrap` and `Flush` for the webhooks' early ack.

**Client key.** The TCP peer, unmapped and zone-stripped. From a trusted
proxy, join all XFF lines in wire order and walk right to left with
`netip.ParseAddr`, skipping trusted entries; the first untrusted one is the
key; entries left of it are never parsed. A malformed entry or an all-trusted
chain keys the last trusted hop. Keys mask to /32 or /64.

**Algorithm.** A `golang.org/x/time/rate` bucket per client, behind
per-client and group in-flight caps; `webhook` covers both paths. Each
group's shared row serves keys the table cannot hold (Memory), same caps.

| Bucket | Rate, burst | Per client | Group cap | Token spent |
|---|---|---|---|---|
| `webhook` | 50/s, 500 | 16 | 64 | on handler 4xx |
| `webhook` shared | 50/s, 500 | same | same | on handler 4xx |
| `link` | 1/s, 20 | 2 | 4 | always |
| `link` shared | 1/s, 20 | same | same | always |
| `health` (L3) | 5/s, 20 | 4 | 32 | always |
| `health` shared | 5/s, 20 | same | same | always |

Under OQ 1 A, each unsettled webhook request holds a token on its own or the
shared bucket: admission needs `TokensAt(now()) >= 1 + held`, so concurrent
requests cannot share the last token. Webhook reads stop at 10 s, GitHub's
timeout; the group cap bounds buffered bodies to 64 MiB. The
link cap holds clicks to 4 of the store's 10 connections.

**Overflow.** Rate or per-client cap: 429. Group cap: 503. Both set
`Retry-After`. Health over budget gets a 1 s cached status (OQ 2).

**Memory.** Each group's mutex guards a
`simplelru.LRU[netip.Prefix, *rate.Limiter]` of 16,384 entries and per-key
in-flight counts, deleted at zero. A miss on a full table probes the 8 oldest
entries, moving each ineligible one to the front (`Get`). The first full
(`TokensAt(now()) >= burst`) and idle one is replaced by a new bucket, which
loses nothing as `rate.NewLimiter` starts full. Otherwise it spends from the
shared bucket. Pinning the table costs `rate × maxClients` requests a second
(819,200 on webhook).

**Connections.** `MaxHeaderBytes` is `64 << 10`, which also sets Go's HTTP/2
header-list limit; `MaxConcurrentStreams` stays 250.

**Counters** (`otel.Meter(doorInstrumentationScope)`, nil on error as in
`NewConsumer`): `compass.door.requests` with outcomes `ok`, `client_error`,
`server_error`, `rate_limited`, `client_concurrency`, `group_concurrency`,
`cached`, and `would_*` forms when disabled; `compass.door.inflight`,
`compass.door.clients`, `compass.door.evictions`; and
`compass.door.untrusted_xff` for XFF from an untrusted loopback or private
peer, which warns once.

**Flags.** `fs.String` with a `firstNonEmpty` env fallback, like
`--forge-disable-scope-enforcement`.

| Flag | Env | Default |
|---|---|---|
| `--trusted-proxy` (CIDR list) | `COMPASS_TRUSTED_PROXIES` | none; `/0` rejected |
| `--disable-anon-route-limits` (`ParseBool`) | `COMPASS_DISABLE_ANON_ROUTE_LIMITS` | `false` |

Disabled limits still count; budgets are constants.

**Authenticated routes are out of scope:** per-IP limits there would throttle
users behind NAT, and the Runner.

## Alternatives considered

- `go-chi/httprate` (293 importers): unbounded maps, no concurrency cap,
  spoofable header keys.
- `ulule/limiter/v3` (341 importers): stale since 2023.
- A per-IP connection cap: door-wide.

`x/time/rate` (16,473 importers) is already a module dependency.

## Plan

### L1 — Limiter core

- **Interfaces:**

  ```go
  // go/server/door_limits.go
  const doorInstrumentationScope = "github.com/RigelBuild/compass/go/server"
  const evictProbe = 8
  type doorGroup string // groupWebhook "webhook", groupLink "link"
  type groupBudget struct {
      rate, sharedRate                                    rate.Limit
      burst, sharedBurst, perClient, inflight, maxClients int
      readTimeout     time.Duration // 0 keeps networkBodyReadTimeout
      chargeOnFailure bool
  }
  var defaultBudgets map[doorGroup]groupBudget // the Approach table; webhook: 10s, true
  func newDoorLimits(enforce bool, trusted []netip.Prefix, budgets map[doorGroup]groupBudget, now func() time.Time, log *slog.Logger) *doorLimits
  func (l *doorLimits) clientKey(r *http.Request) netip.Prefix
  func (l *doorLimits) wrap(g doorGroup, next, overflow http.Handler) http.Handler // nil overflow rejects
  ```

  `admit`, under the group mutex, checks the group cap, the client cap, then
  `AllowN(now(), 1)` on the key's own or the shared bucket (the hold check
  under `chargeOnFailure`). `settle`, deferred, spends after a 4xx and frees
  the slot.
- **Red → green** (`door_limits_test.go`, `now` injected):
  1. `TestDoorLimitsRate`: link request burst+1 gets 429 with `Retry-After`;
     another peer passes; a refill readmits the first.
  2. `TestDoorLimitsChargeOnFailure`: burst webhook 400s make the next request
     429; 2×burst 200s pass; with one token left, a concurrent second gets 429.
  3. `TestDoorLimitsConcurrency`: perClient+1 gets 429, the group cap 503; an
     overflow handler serves both; a panic frees the slot.
  4. `TestDoorLimitsClientKey` (table): a /64 is one key, from the peer or
     XFF; v4-mapped is unmapped, a zone stripped; a chain across two XFF
     lines gives its first untrusted entry despite malformed text to its
     left; a walked malformed entry or all-trusted chain gives the last
     trusted hop; untrusted loopback XFF is ignored and counted.
  5. `TestDoorLimitsBounded` (`maxClients` 2, both drained, clock frozen): 100
     rotating peers get exactly the shared burst, then 429, and the table is
     unchanged; after a shared refill a new peer passes; a refilled entry is
     evicted for a new peer with a full bucket, an in-flight one never.
  6. `TestDoorLimitsMetrics`: a `ManualReader` sees every outcome, `would_*`
     forms included, and each other instrument; no point has another attribute.
  7. `TestDoorLimitsWriter` (`httptest`): a body still arriving at
     `readTimeout` fails; a flush reaches the client before the handler
     returns.

### L2 — Door wiring, flags, spec

- **Interfaces:**
  - Add `ServeConfig.TrustedProxies []netip.Prefix` and
    `ServeConfig.DisableAnonRouteLimits bool`.
  - `buildNetworkServer` (same signature) wraps both webhooks and the link,
    nil overflow, with
    `newDoorLimits(!cfg.DisableAnonRouteLimits, cfg.TrustedProxies, defaultBudgets, time.Now, slog.Default())`.
  - Add `networkMaxHeaderBytes = 64 << 10` and
    `parseTrustedProxies(raw string) ([]netip.Prefix, error)`.
  - Fix `githubDeliveryLRUSize`'s redelivery claim; point `bodyDeadlineExempt`
    at OQ 3.
  - Amend `docs/specs/product/compass.md` §The network door.
- **Red → green:**
  1. `TestNetworkDoorLimitsAnonymousRoutes` (pgtest): link request burst+1
     gets 429; a bearer `GetServerInfo` from the same peer passes.
  2. `TestNetworkDoorHeaderBound` (TLS): HTTP/1.1 gets 431 at 128 KiB and
     passes 32 KiB. Go's client refuses over-limit lists, so HTTP/2 uses
     `x/net/http2`'s `Framer`: one HEADERS frame of 32 4 KiB fields gets 431;
     one 128 KiB field exceeds HPACK's string limit and gets GOAWAY
     `COMPRESSION_ERROR`.
  3. `TestBuildServeConfigDoorLimits`: flags and env vars parse; `/0`, a bad
     CIDR and `maybe` fail.

### L3 — Health group (after RIG-5191 H1)

- **Interfaces:**
  - `groupHealth`: the table rows, `maxClients: 16384`.
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

- [ ] L1 — limiter core.
- [ ] L2 — wiring, header bound, flags, spec.
- [ ] L3 — health group (blocked on RIG-5191 H1).
- [ ] L4 — `docs/self-host.md`.
