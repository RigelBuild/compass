# Network door request limits

Tracker: RIG-5243

## Open Questions

1. **Webhook bucket (load-bearing).** A rejected delivery may be lost (GitHub
   does not redeliver; Linear's retry may fail `CheckTimestamp`, unverified).
   64 slow senders fill the group cap under any option.
   - **A (recommended):** charge on failure; signed deliveries never drain
     it, garbage stops before buffering.
   - **B:** caps and the 10 s deadline; garbage costs a 1 MiB read and an
     HMAC each.
   - **C:** charge every request; a GitHub burst past 500 loses events.
2. **Health over budget (load-bearing).**
   - **A:** reject; a flood via a shared LB or tunnel fails probes.
   - **B (recommended):** serve the cached status; bounds `Hub.mu`
     contention, never fails a probe.
   - **C:** always cache; no limiter or client counts.
3. **Pre-auth cost (load-bearing).** `receiveUnaryRequest` runs before
   `untyped`, so a tokenless caller can make the door buffer 128 MiB, or drip
   a body to the exempt Runner paths.
   - **A (recommended):** a new issue. A header-shape guard passes `Bearer x`;
     the fix verifies the token before decode and keeps the deadline until
     the Runner authenticates.
   - **B:** add the header-shape guard here as L2b.
4. **Trusted-proxy model (load-bearing).**
   - **A (recommended):** CIDRs and the right-to-left walk.
   - **B:** A plus RFC 7239 `Forwarded`.
   - **C:** PROXY protocol v2, the only fix for an L4 passthrough proxy.
5. **Default budgets (load-bearing).**
   - **A (recommended):** constants plus the kill switch.
   - **B:** a flag per group.
   - **C:** shadow mode first (kill switch default-on for one release).
6. **IPv6 width (non-load-bearing).** Rotating within a /48 lands on the
   shared bucket, which keeps the limit bounded; a wider key or /48 bucket is
   an optional later optimization.

## Problem / Intent

Three anonymous network-door routes lack limits and counters: the signed
GitHub and Linear webhooks, and `GET /l/session/{id}`. This record adds both,
plus a group for the RIG-5191 health routes (anonymous per RIG-5230 OQ-3 A).

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
`withBodyReadDeadline`. `wrap` sets the group's `readTimeout` with
`http.NewResponseController(w).SetReadDeadline`; its writer keeps `Unwrap`
and `Flush` for the webhooks' early ack.

**Client key.** The TCP peer. From a trusted proxy, split all XFF lines in
wire order on commas, trim spaces and tabs, and walk right to left. Parse
each entry with `netip.ParseAddr`, then unmap and strip its zone, as for the
peer, before the trust check. Skip trusted entries; the first untrusted one
is the key, and entries left of it are never parsed. A malformed entry or an
all-trusted chain keys the last trusted hop. Keys mask to /32 or /64.

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

Under OQ 1 A, each unsettled webhook request holds a token on its bucket:
admission needs `TokensAt(now()) >= 1 + held`. Webhook reads stop at 10 s,
GitHub's timeout; the group cap bounds buffered bodies to 64 MiB. The link
cap holds clicks to 4 of the store's 10 connections.

**Overflow.** Rate or per-client cap: 429. Group cap: 503. Both set
`Retry-After`. Health over budget gets a 1 s cached status (OQ 2).

**Memory.** Each group's mutex guards a
`simplelru.LRU[netip.Prefix, *rate.Limiter]` of 16,384 entries and per-key
in-flight counts, deleted at zero. A miss on a full table probes the 8 oldest
entries, moving each ineligible one to the front (`Get`), and replaces the
first full (`TokensAt(now()) >= burst`) and idle one with a new, full bucket.
Otherwise it spends from the shared bucket. Pinning the table costs
`rate × maxClients` requests a second (819,200 on webhook).

**Connections.** `MaxHeaderBytes` is `64 << 10`, which also sets Go's HTTP/2
header-list limit; `MaxConcurrentStreams` stays 250.

**Counters** (`otel.Meter(doorInstrumentationScope)`, nil on error as in
`NewConsumer`): `compass.door.requests` with outcomes `ok`, `client_error`,
`server_error`, `rate_limited`, `client_concurrency`, `group_concurrency`,
`cached`, and `would_*` forms when disabled; `compass.door.inflight`,
`.clients`, `.evictions`; and `.untrusted_xff` for XFF from an untrusted
loopback or private peer, warned once.

**Flags.** `fs.String` with a `firstNonEmpty` env fallback.

| Flag | Env | Default |
|---|---|---|
| `--trusted-proxy` (CIDR list) | `COMPASS_TRUSTED_PROXIES` | none; `/0` rejected |
| `--disable-anon-route-limits` (`ParseBool`) | `COMPASS_DISABLE_ANON_ROUTE_LIMITS` | `false` |

Disabled limits still count; budgets are constants. Authenticated routes are
out of scope: per-IP limits would throttle NAT users and the Runner.

## Alternatives considered

- `go-chi/httprate` (293 importers): unbounded maps, no concurrency cap.
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
  4. `TestDoorLimitsClientKey` (table): a /64 is one key; `client, proxy`
     keys the client; mapped or zoned peer and XFF values are canonicalized
     before the trust check; a two-line chain gives its first untrusted
     entry despite malformed text to its left; a walked malformed entry or
     all-trusted chain gives the last trusted hop; untrusted loopback XFF is
     ignored and counted.
  5. `TestDoorLimitsBounded` (`maxClients` 2, every burst 10,
     `chargeOnFailure`, clock frozen): with both entries drained, 100 rotating
     peers get exactly 10 admissions, then 429, table unchanged; a shared
     refill admits a new peer. In a fresh table, with full buckets, A admitted
     first and in flight, then B served and idle, a new peer evicts B, not A.
  6. `TestDoorLimitsMetrics`: a `ManualReader` sees every outcome, `would_*`
     forms included, and each other instrument; no point has another attribute.
  7. `TestDoorLimitsWriter`: a body still arriving at `readTimeout` fails; a
     flush reaches the client before the handler returns.

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
     gets 429; a bearer `GetServerInfo` from the same peer passes; burst
     bad-signature GitHub POSTs make the peer's next Linear POST 429; with
     `DisableAnonRouteLimits`, link burst+1 passes and counts
     `would_rate_limited`.
  2. `TestNetworkDoorHeaderBound` (TLS): HTTP/1.1 gets 431 at 128 KiB and
     passes 32 KiB. HTTP/2 sends one HEADERS frame via `x/net/http2`'s
     `Framer` and `hpack.NewEncoder` (Huffman-coded `a` runs), each fragment
     under Go's 131,712-byte cutoff (twice the 65,856-byte list limit; GOAWAY
     `PROTOCOL_ERROR` past it). 32 fields of 4 KiB (82,288 bytes encoded) get
     431; one 128 KiB field (81,946) exceeds HPACK's string limit: GOAWAY
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
    overflows to the cache.
- **Red → green:**
  1. `TestCachedHealth`: the inner checker runs once per `ttl`; a drain shows
     at once.
  2. `TestNetworkDoorHealthOverflow`: request burst+1 succeeds on both views
     and counts as `cached`.

### L4 — Operator docs

- **Interfaces:** `## Network door limits` in `docs/self-host.md`: groups,
  flags, counters; tunnels need `--trusted-proxy 127.0.0.1/32,::1/128`; lost
  GitHub events are redelivered from Recent Deliveries.
- **Red → green:** `rumdl check` passes.

## Tasks

- [ ] L1 — limiter core.
- [ ] L2 — wiring, header bound, flags, spec.
- [ ] L3 — health group (blocked on RIG-5191 H1).
- [ ] L4 — `docs/self-host.md`.
