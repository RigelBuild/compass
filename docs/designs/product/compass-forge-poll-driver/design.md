# Compass forge-poll driver (SEA-1810)

Status: Draft
Lane: compass-server
Tracker: SEA

Freeze blocked on OQ-A (see Open Questions) — Matt rules the DL-053
relationship before this record freezes. This record designs the running
driver that periodically drives the existing greenfield `go/internal/ingest`
library against a real GitHub forge, so board issues ingested from the tracker
appear live in the Compass server projection. Design layer only — no
implementation code ships with this record.

## Problem / Intent

Board issues ingested from the external tracker (GitHub) must appear live in
the Compass server projection. Today nothing drives ingestion: `server/serve.go`
wires no poller and imports neither `internal/ingest` nor `internal/forge`
(verified this run — the only mentions of a poller in `go/server` are forward
references, e.g. `server/board.go:40` "the PR-B poll driver … add producers
without changing the executor signature", and the pgtest comment
`server/service_board_pgtest_test.go:14` "exactly as part 3's poller would").
The `internal/ingest` library (`ingest.go:35-65`) is complete and tested but is
exercised only by its own tests and the board pgtest suite. This slice adds the
running driver: a ctx-cancellable background poll loop started at serve boot,
plus the first real (non-fake) forge read client behind it.

## Global Constraints

- **Toolchain:** module `github.com/sealedsecurity/compass/go`, go 1.25.0
  (`go/go.mod`).
- **OQ-6 (FROZEN, Matt 2026-07-27):** the GitHub client is hand-rolled over
  `net/http`, **no new dependency** (~300 LOC); go-github is REJECTED — it
  lacks a conditional-request + budget mechanism hook. Conditional requests
  (`If-None-Match`/ETag → treat `304 Not Modified` as no-change) and honoring
  `x-ratelimit-*` response headers as a budget are **mandatory**, not
  optional hardening.
- **DL-052 (Active, `DECISIONS.md:75`):** only the Server holds forge
  credentials, as a `server_only` declared secret filtered out of container
  injection. The driver reads its GitHub token via the Server's secret path
  (the `secrets.SpecResolver` instance built at `server/serve.go:287`) —
  never from an agent/container, never from a flag that would land in `ps`
  output.
- **Error mapping:** every non-2xx forge response maps to
  `*forge.StatusError{Status, Message}` (`forge/provider.go:216-227`), so the
  Service layer can flatten statuses without inspecting the wire.
- **Idempotency:** a failed or partial poll is safe to retry — "partial
  progress is fine — a re-poll is idempotent on the coordinate"
  (`ingest.go:48-52`; the coordinate is the store upsert key,
  `board/issue_projection.go:69-71`).
- **No projection change:** `board.IssueProjection.PublishIssueUpdate(ctx,
  *compassv1.Issue) error` (`board/issue_projection.go:68`) ALREADY satisfies
  `ingest.issueSink` (`ingest.go:31-33`). This slice adds zero code to
  `internal/board`.
- **Non-goals:** the tracker-status WRITE-back / outbound mirror +
  echo-suppress (T3-b / PR-C, a separate slice); the DL-053 agent-notification
  subscription delivery machinery (Sessions → `AgentGateway.Control` push).
  This record is the BOARD-ingestion read poll only.

## Approach

Three pieces, one per task: a concrete GitHub read client in `internal/forge`,
a poll-loop scheduler in `internal/ingest`, and boot wiring + config surface in
`server/serve.go` + `cmd/compass-server/main.go`.

### The GitHub read client (`forge.GitHub`, new file `go/internal/forge/github.go`)

A hand-rolled `net/http` client implementing, at minimum, the read half that
satisfies `ingest.forgeReader`:

```go
ListIssues(ctx context.Context, repo string, f forge.IssueFilter) ([]forge.Issue, error)
```

(`ingest.go:23-25` — the interface is unexported but structural, so any
concrete type with this method passes `ingest.NewIngester`, `ingest.go:44`.)
It lives in `internal/forge` because that package is "the ONLY place a forge
credential is used" (ownership-layer record, quoted at
`compass-server-ownership-layer/design.md:732-734`), and it is the first
increment of the full `forge.Provider` (`provider.go:195-207`) — the write
half arrives with later slices and is out of scope here; until then the type
does NOT claim to implement `Provider` (no `var _ Provider` assertion; only
`ListIssues` is built and only `forgeReader` consumes it).

Behavior, in order of the request lifecycle:

1. **Auth.** Every request carries `Authorization: Bearer <token>` where the
   token comes from a `TokenSource func(ctx) (string, error)` — resolved per
   request batch, not captured at construction, so a rotated `server_only`
   secret takes effect without a restart (composes with DL-052; see T3 for the
   resolve wiring).
2. **Incremental fetch (`since` + conditional requests).** Two composed
   mechanisms keep a steady-state tick cheap. (a) The client requests
   `sort=updated&direction=desc&since=<watermark>`, where `<watermark>` is the
   greatest `updated_at` seen for that repo on the prior successful pass;
   GitHub then returns only issues touched at/after it, so a quiet repo costs
   ~1 request regardless of size. The watermark is in-memory (per Option 1,
   OQ-A); a restart drops it and the next boot pass does one full `state=all`
   resync — bounded and self-healing. (OQ-A Option 1.5 would persist it.)
   (b) On top of `since`, a per-URL in-memory ETag cache (keyed by
   repo+filter+page) sends `If-None-Match`; a `304 Not Modified` returns the
   cached parsed `[]forge.Issue` **and the cached `Link` chain for that key**
   — a 304 need not re-send `Link`, so the next-page URLs MUST be cached
   alongside the ETag or a multi-page repo cannot walk past page 1 — and, per
   current GitHub docs, a 304 is not charged against the core rate limit.
   **That last fact is load-bearing for the budget math below and MUST be
   reverified against current GitHub REST docs at T1** (the rate-limit docs
   have been revised before). `since` is the PRIMARY steady-state reducer
   because ETags alone self-defeat under GitHub's default `created`-desc
   order: one new issue shifts every page boundary and invalidates every
   page's ETag, collapsing the "free 304" path to a full paid refetch of
   `ceil(N/100)` pages; `sort=updated` + `since` sidesteps that. The ETag
   cache is bounded by repos × pages with a fixed size cap (pages grow with
   repo size) — no eviction policy beyond the cap at this scale.
3. **Rate budget.** After every response the client records
   `x-ratelimit-remaining` and `x-ratelimit-reset`. Before issuing a request,
   if `remaining <= reserve` (default reserve 10), it fails fast with a typed
   budget error (`forge.ErrBudgetExhausted`, exact shape in T1 Interfaces)
   rather than burning the tail of the budget; the poller skips that cycle and
   retries next tick (safe: re-poll is idempotent, `ingest.go:51-52`). Budget
   exhaustion is an EXPECTED skip, not a failure — the poller detects
   `errors.Is(err, forge.ErrBudgetExhausted)` and logs it at `slog.Warn`, NOT
   the `slog.Error` path a real provider/sink error takes, so a rate-limited
   window is not per-tick Error noise. A `403`/`429` carrying `retry-after` or
   a zeroed `x-ratelimit-remaining` is treated the same way. The client never
   sleeps holding the caller's poll slot — backoff is the poller's ticker, not
   an in-client `time.Sleep`. (Under sustained pressure a skipped tick can
   defer ingestion up to a full reset window (~60 min); acceptable given
   idempotency, and `since` keeps steady-state spend far under the budget.)
4. **Pagination.** GitHub's list-issues endpoint pages; the client requests
   `per_page=100` and follows RFC-5988 `Link: rel="next"` headers,
   concatenating pages into one `[]forge.Issue` (the `Ingester` contract is
   "fetches every issue for repo", `ingest.go:48`).
5. **Filter mapping.** `forge.IssueFilter{State, Labels}`
   (`provider.go:182-188`) maps to `state=`/`labels=` query params. An empty
   `State` — which is what `Ingester.Ingest` always sends
   (`forge.IssueFilter{}`, `ingest.go:54`) — maps to **`state=all`**, this
   provider's documented default (`provider.go:183-184` delegates the empty
   case to "the provider's default"). Rationale: the board needs closed
   tracker issues to flow so Done/reopen transitions ingest (DL-129: "a user
   reopen un-archives via ingestion", `DECISIONS.md:173`); an open-only
   default would silently freeze closed issues at their last-seen state.
6. **PR exclusion.** GitHub's issues API returns pull requests as issues
   (rows carrying a `pull_request` key); the client drops them — the board's
   PR surface is a separate lane, and `forge.Issue` (`provider.go:37-53`) is
   issue-shaped only.
7. **Error mapping.** Any non-2xx/non-304 response becomes
   `&forge.StatusError{Status: resp.StatusCode, Message: <GitHub message
   field>}` (`provider.go:216-227`), wrapped so `errors.As` finds it. Bodies
   are returned RAW — the owner-header strip belongs to ingestion
   (`provider.go:42-44`, `ingest.go:67-74`), never the provider.

### The poll-loop scheduler (`ingest.Poller`, new file `go/internal/ingest/poller.go`)

A small scheduler in the `ingest` package (it composes the package's own
`Ingester`; a new package would be a second convention for no gain):

- `Run(ctx)` performs one immediate pass at start (so a freshly booted demo
  server shows the board without waiting a full interval), then ticks on
  `time.Ticker(interval)`.
- Each pass iterates the configured repos and calls
  `Ingester.Ingest(ctx, repo)` (`ingest.go:53`) sequentially — one forge, one
  budget; parallel per-repo fetches would just race the same rate limit.
- **A failed poll is logged, never fatal.** `Ingest` stops at the first
  provider/sink error (`ingest.go:50-51`); the poller records it at
  `slog.Error` and moves to the next repo / next tick — the re-poll is
  idempotent on the coordinate (`ingest.go:51-52`). The poller returns a
  non-nil error only never — its sole exit is ctx cancellation, returning
  `nil` so the errgroup drain (see wiring) treats shutdown as clean.
- **Graceful shutdown:** `<-ctx.Done()` between passes and via the ctx
  threaded into `Ingest` mid-pass (both `ListIssues` and
  `PublishIssueUpdate` take ctx, `ingest.go:54,60`).
- **Observability:** one `slog.Info` per completed repo pass with fields
  `repo`, `issues` (count sunk), `not_modified` (bool: whole fetch served
  from cache), `dur`, `ratelimit_remaining`; one `slog.Warn` per
  budget-skipped pass (`errors.Is(err, forge.ErrBudgetExhausted)` — an
  expected condition, never `Error`); one `slog.Error` only per genuinely
  failed pass (provider/sink error) with `repo`, `err`. Boot logs one
  `slog.Info` with `repos`, `interval`. No per-issue logging — a 500-issue
  repo must not emit 500 lines per minute.

### Boot wiring + config (`server/serve.go`, `cmd/compass-server/main.go`)

Mirrors the two established precedents exactly:

- **Config surface:** `server.ServeConfig` (`serve.go:56-89`) gains an
  optional `Forge ForgeConfig` field, all-optional exactly like `S3`
  (`serve.go:71-75`): absent repos → no poller, today's behavior, zero new
  requirements on existing deployments. The CLI (`main.go`) maps flags with
  env fallback mirroring the `COMPASS_S3_*` precedence (`main.go:127-138`):
  `--forge-repos` / `$COMPASS_FORGE_REPOS` (comma-separated `owner/name`),
  `--forge-poll-interval` / `$COMPASS_FORGE_POLL_INTERVAL` (default `1m`),
  `--forge-secret` / `$COMPASS_FORGE_SECRET` (the declared secret NAME,
  default `GITHUB_FORGE_TOKEN`; the VALUE never crosses a flag), plus
  `--forge-host` / `$COMPASS_FORGE_HOST` (default `github.com`, for GHES
  later; the API base URL derives from it).
- **Secret resolve:** the poller's `TokenSource` closes over the one
  `secrets.SpecResolver` built at `serve.go:287`, calls
  `Resolve(ctx, "forge poll")` (`secrets/resolver.go:139`), and selects the
  configured name from the returned `[]ResolvedSecret` — the same
  single-resolve-surface DL-052 mandates; the row is declared `server_only`
  so it never reaches a container (mechanism per the ownership-layer T5,
  `compass-server-ownership-layer/design.md:1991-1995`). **Resolve is not
  cheap** — `resolver.go:139-176` reads the entire declared-secret registry
  from the store, writes a manifest temp file, and drives a full secretspec
  provider `Load` (potentially an external provider call) per invocation — so
  the `TokenSource` caches the resolved value behind a short TTL (default
  `5m`) and re-resolves on expiry OR on a `401`/`403` auth response, NOT once
  per poll pass; a token that rotates rarely does not tax the store and
  secrets provider every minute, while a rotation still takes effect within
  the TTL (or immediately on the next auth failure). A missing/undeclared
  secret with forge config present is a startup error — fail fast, but at
  `serve.go:287` the resolver is built AFTER the UDS listener binds, so the
  validation follows the listener-cleanup path the `Rehydrate` failure
  already uses (`serve.go:260-263`), returning the error from `Serve` rather
  than a bare `main.go` exit. A resolve that breaks at RUNTIME (provider
  outage, a declaration deleted post-boot) surfaces per-pass as an auth error
  → `slog.Error` + retry next tick (idempotent); to keep a permanently broken
  secret from degrading to silent forever-Error, the poller emits a distinct
  `slog.Error` "forge token unresolvable" the first time a resolve fails and
  on recovery — a minimal health surface without per-tick spam.
- **Lifecycle:** the poller runs as one more `g.Go` member of the existing
  serve errgroup (`serve.go:327-339`) — `g.Go(func() error { return
  poller.Run(gctx) })` — so it inherits exactly the scoped lifecycle the
  doors have: cancelled on SIGINT/SIGTERM via the `signal.NotifyContext` ctx
  (`main.go:146-162`), first-error-wins, drained with everything else. No
  bespoke goroutine + done-channel machinery.
- **Ingester assembly:** `ingest.NewIngester(gh, issueBrd,
  &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
  Host: cfg.Forge.Host})` (`ingest.go:44`; enum at
  `gen/compass/v1/compass.pb.go:449`), where `issueBrd` is the existing
  projection instance built at `serve.go:259` — shared writer/reader exactly
  as the board pgtest drives it (`service_board_pgtest_test.go:13-15`).

## Alternatives considered

- **(a) go-github library — REJECTED (OQ-6, frozen Matt 2026-07-27).** It
  would add a dependency for what is ~300 LOC of `net/http`, and its
  high-level API lacks the hook this driver's core mechanism needs:
  first-class conditional-request (`If-None-Match`/304) handling fused with an
  `x-ratelimit-*` budget gate ahead of each call. Hand-rolling makes the
  budget/ETag path the PRIMARY path, not a retrofit around a library's
  transport. Not re-opened here; recorded as a Global Constraint.
- **(b) Fold this poller into the DL-053 subscription poller — the
  load-bearing fork, PARKED as OQ-A.** DL-053 (`DECISIONS.md:76`) freezes a
  Postgres-backed FETCH/DELIVERY-cursor conditional-poll subscription model
  for agent notification delivery, and DL-129 (`DECISIONS.md:173`) names "the
  DL-053 poll" as the path tracker status ingests through. This record
  designs a DISTINCT, simpler board-ingestion loop (Option 1). OQ-A now
  carries THREE readings — Option 1 (distinct, in-memory cursor), Option 1.5
  (distinct, durable Postgres fetch-cursor from day one), Option 2 (build on
  DL-053 machinery) — because tracker status rides the SAME `/issues` rows
  this loop fetches, so PR-C on a separate poll double-fetches one endpoint.
  See OQ-A for the three options, the double-fetch consequence, and the
  recommendation.
- **(c) Webhooks instead of polling — deferred by DL-053 itself**
  ("change-detected by conditional polling in v1 (webhooks are an additive
  accelerator)", `DECISIONS.md:76`). Same posture applies here: a webhook
  receiver is an additive accelerator that sinks through the same
  `Ingester`, never a replacement for the poll.
- **(d) Where the loop lives: a bespoke goroutine in `cmd/compass-server`
  vs. the serve errgroup — chose the errgroup.** `main.go` owns flags and
  signal wiring only (`main.go:41-173`); everything with a lifecycle runs
  under `Serve`'s scoped group (`serve.go:321-339`). A cmd-level goroutine
  would duplicate drain/first-error semantics the group already audits.
- **(e) Per-repo goroutines in the poller — rejected.** One forge, one rate
  budget; sequential per-repo fetches serialize naturally against
  `x-ratelimit-*` and keep the poller a trivially testable single loop.
- **(f) `since`-watermark incremental sync vs page ETags alone — chose
  both, `since` primary.** GitHub's default `created`-desc list order means
  one new issue shifts every page boundary and invalidates every page ETag,
  so ETags alone collapse to a full paid refetch on any churn.
  `sort=updated&since=<watermark>` returns only changed rows and reduces a
  quiet tick to ~1 request independent of repo size; page ETags then ride on
  top for the no-change case. `since` is folded into the request lifecycle
  (Approach step 2), not rejected — the two compose.

## Plan

### T1 — `forge.GitHub`: the hand-rolled net/http GitHub read client

New file `go/internal/forge/github.go` (+ `github_test.go`). Implements the
request lifecycle from the Approach: bearer auth via `TokenSource`,
`sort=updated&since=<watermark>` incremental fetch, ETag conditional requests
with a per-URL response+`Link`-chain cache, `x-ratelimit-*` budget gate with a
typed budget error, `Link`-header pagination at `per_page=100`, `IssueFilter`
→ query-param mapping (empty `State` → `state=all`), `pull_request`-row
exclusion, non-2xx → `*forge.StatusError`. Raw bodies pass through untouched
(`provider.go:42-44`). No `Provider` interface assertion — read half only.

Interfaces:

```go
// TokenSource yields the current forge token; called per fetch pass so a
// rotated server_only secret takes effect without restart.
type TokenSource func(ctx context.Context) (string, error)

// GitHubConfig configures a GitHub read client.
type GitHubConfig struct {
    Host   string      // "github.com" or a GHES host; API base derives from it
    Token  TokenSource // required
    Client *http.Client // nil -> a default client with a sane timeout
}

func NewGitHub(cfg GitHubConfig) *GitHub

// Satisfies ingest.forgeReader (ingest.go:23-25) structurally.
func (g *GitHub) ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error)

// ErrBudgetExhausted is returned (wrapped) when x-ratelimit-remaining is at or
// under the reserve; the caller skips the cycle and retries next tick.
var ErrBudgetExhausted = errors.New("forge: rate budget exhausted")
```

Consumes: `forge.IssueFilter` (`provider.go:182-188`), `forge.Issue`
(`provider.go:37-53`), `forge.StatusError` (`provider.go:216-227`),
`net/http`, `encoding/json` (stdlib only — OQ-6).

Test cycle (unit, stubbed `http.RoundTripper`; red-first per repo convention):

1. 200 with issues JSON → parsed `[]forge.Issue` field-by-field (incl. raw
   body untouched, labels, state, ForgeAccount).
2. Second call sends `If-None-Match` with the first response's ETag; a
   scripted 304 returns the cached slice, zero re-parse.
3. `x-ratelimit-remaining: 0` on response N → call N+1 fails
   `errors.Is(..., ErrBudgetExhausted)` WITHOUT issuing a request (assert via
   RoundTripper call count); after scripted reset, calls resume.
4. Two-page `Link` pagination concatenates; `per_page=100` asserted on the
   request.
5. Empty `IssueFilter.State` → `state=all` on the wire; labels join correctly.
6. A `pull_request`-keyed row is dropped.
7. 403/404/500 → `errors.As(..., *forge.StatusError)` with matching Status and
   the GitHub `message` field.
8. Token from `TokenSource` lands as `Authorization: Bearer …`; a TokenSource
   error propagates without a request.
9. Multi-page 304: a two-page list is fetched, then a scripted 304 on page 1
   returns the cached slice AND walks the cached `Link` chain to page 2's
   cached slice — a repo of >100 issues is served entirely from cache on an
   unchanged pass (the page-chain-caching correctness hole).
10. `403`/`429` with `retry-after` (or a zeroed `x-ratelimit-remaining` on the
    prior response) → next call fails `errors.Is(..., ErrBudgetExhausted)`
    without issuing a request (RoundTripper call count asserted).
11. Absent/malformed `x-ratelimit-*` headers do not wedge the gate — the
    client proceeds (treats unknown budget as available) rather than
    permanently skipping.
12. ctx cancellation mid-pagination (cancel after page 1) stops promptly and
    returns `ctx.Err()`, not a partial concatenation.

### T2 — `ingest.Poller`: the ctx-cancellable poll-loop scheduler

New file `go/internal/ingest/poller.go` (+ `poller_test.go`). Immediate first
pass, then ticker; sequential repos; log-and-continue on per-repo error;
`nil` return on ctx cancellation; slog fields per the Approach.

Interfaces:

```go
// PollerConfig configures the board-ingestion poll loop.
type PollerConfig struct {
    Repos    []string      // "owner/name", non-empty (validated by the caller)
    Interval time.Duration // > 0; the caller defaults it
    Log      *slog.Logger  // nil -> slog.Default()
}

func NewPoller(ing *Ingester, cfg PollerConfig) *Poller

// Run polls until ctx is cancelled, then returns nil (clean shutdown).
// Per-repo errors are logged and retried next tick, never returned — a
// re-poll is idempotent on the coordinate (ingest.go:48-52).
func (p *Poller) Run(ctx context.Context) error
```

Consumes: `Ingester.Ingest(ctx, repo) error` (`ingest.go:53`), `time.Ticker`,
`log/slog`.

Test cycle (unit, fake `Ingester` deps — the package's existing
`recordingSink` pattern, `ingest_test.go:15-25`, plus `forge.FakeProvider`):

1. `Run` performs an immediate pass for every repo before the first tick
   (short interval, assert call order/coverage).
2. ctx cancel BETWEEN ticks → `Run` returns `nil` promptly, and ctx cancel
   DURING a pass (mid-`Ingest`, before all repos are done) also returns `nil`
   promptly — deadline-gated, never a sleep (the pgtest convention,
   `service_board_pgtest_test.go:15`). Assert `Run` does NOT return while ctx
   is live (it has no non-cancel exit).
3. A scripted `Ingest` failure on repo A does not stop repo B in the same
   pass, and repo A is retried on the next tick.
4. Interval respected: N ticks in a bounded window drive exactly N+1 passes
   (the +1 is the immediate pass); no busy-loop.
5. Log fields: capture a `slog` test handler and assert a happy pass logs at
   Info with `repo`/`issues`/`dur`, a budget-skip logs at Warn, and a genuine
   `Ingest` error logs at Error with `err`.

### T3 — serve boot wiring + config surface + secret resolve

Touches `server/serve.go`, `cmd/compass-server/main.go` (edits only, no new
files beyond tests). `ServeConfig.Forge` all-optional like `S3`; CLI
flags/env per the Approach; fail-fast on forge-config-present but
secret-unresolvable; poller under the existing errgroup.

Interfaces:

```go
// server.ForgeConfig configures the board-ingestion poll driver (SEA-1810).
// All-optional: empty Repos leaves the driver off (today's behavior).
type ForgeConfig struct {
    Host         string        // default "github.com"
    Repos        []string      // "owner/name"; empty -> no poller
    SecretName   string        // declared server_only secret name; default "GITHUB_FORGE_TOKEN"
    PollInterval time.Duration // default time.Minute
}
// ServeConfig gains: Forge ForgeConfig
```

Consumes: `secrets.SpecResolver.Resolve(ctx, reason) ([]ResolvedSecret, error)`
(`secrets/resolver.go:139`), `secrets.ResolvedSecret{Name, Value, …}`
(`secrets/secrets.go:128-144`; Value redacted under all fmt verbs — safe to
thread), `board.NewIssueProjection` instance from `serve.go:259`,
`ingest.NewIngester` (`ingest.go:44`), `forge.NewGitHub` (T1),
`ingest.NewPoller`/`Run` (T2), the errgroup at `serve.go:327`,
`compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB`
(`gen/compass/v1/compass.pb.go:449`).

Produces: a `TokenSource` closure over the resolver that re-resolves +
selects `SecretName` on TTL expiry or auth failure (not per pass; see the
Secret-resolve bullet); boot-time validation error text for missing-secret /
bad-repo-format / non-positive interval.

Test cycle:

1. Unit: CLI flag/env precedence for the four forge knobs mirrors the S3
   pattern (table test like `resolveNetworkDoor`'s, `main.go:184-212`);
   repo-format validation (`owner/name`) rejects garbage at startup.
2. Unit: empty `Forge.Repos` → `Serve` builds no poller (assert via config
   plumbing seam, no behavioral change to existing serve tests — the whole
   existing suite is the regression net).
3. pgtest (build-tag gated like the board suite): a `Serve`-assembled
   Ingester over `forge.FakeProvider` scripted with one issue sinks to the
   real projection and lands in `ListBoardIssues` — the end-to-end
   boot-wiring proof without touching live GitHub. (The fake satisfies the
   same structural `forgeReader`, `ingest.go:20-22`.)
4. pgtest: a CLOSED-state issue from the fake sinks with `ForgeState` set and
   the row's `State` left untouched — the no-clobber contract this driver
   leans on (`issue_projection.go:207`, `protoToForgeFields` writes
   `ForgeState` but never `State`), so board ingestion cannot overwrite a
   human-set board column.
5. Unit: the `TokenSource` re-resolves after its TTL and on a scripted auth
   failure, and a changed resolver value is used on the next fetch — the
   design's stated reason for a resolver closure rather than a captured token.
6. Startup failure: forge config present + secret name absent from the
   resolved set → `Serve` returns a descriptive error, following the
   listener-cleanup path the `Rehydrate` failure uses (`serve.go:260-263`),
   since the resolver is built after the UDS listener binds (`serve.go:287`).

## Tasks

- [ ] **T1** — `forge.GitHub` read client (`github.go` + unit tests over a
  stubbed RoundTripper: 200/parse, ETag/304, budget gate, pagination,
  filter mapping incl. `state=all` default, PR exclusion, StatusError
  mapping, bearer auth).
- [ ] **T2** — `ingest.Poller` scheduler (`poller.go` + unit tests: immediate
  pass, ctx cancel → nil, per-repo error isolation + retry, interval
  discipline, slog fields).
- [ ] **T3** — serve boot wiring: `ServeConfig.Forge`, CLI flags/env, DL-052
  secret resolve via the existing `SpecResolver`, errgroup membership,
  fail-fast validation; unit + pgtest end-to-end over `FakeProvider`.

## Open Questions

### OQ-A (LOAD-BEARING — blocks the freeze): relationship to DL-053

The SEA-1810 issue body scopes this slice as the *simple board-ingestion
poll* (ticker → `Ingester.Ingest` → `IssueProjection` sink), explicitly
distinct from the DL-053 agent-notification subscription poller. But DL-053
(`DECISIONS.md:76`) and DL-129 (`DECISIONS.md:173`) freeze a Postgres-backed
FETCH/DELIVERY-cursor conditional-poll subscription model as THE way tracker
status is ingested ("through the reverse `TrackerStatusMapping` (DL-053
poll; echo-suppressed…)"). This slice does NOT ingest tracker STATUS today —
`protoToForgeFields` (`issue_projection.go:208-219`) writes `ForgeState` but
never `State`, and the `:207` guarantee ("the forge-only upsert cannot
clobber a human-set state") means board ingestion and status ingestion are
cleanly separable RIGHT NOW. So Option 1 is not a contradiction of DL-129
today. The fork is about what happens when PR-C (tracker-status ingestion,
DL-129) lands: tracker status is a field on the SAME `/issues` rows this loop
already fetches (`forge.Issue.State`, `provider.go:46-47`), so PR-C on a
separate DL-053 poll would **re-fetch the identical endpoint** — double
rate-budget spend against one shared limit, the exact waste alternative (e)
rejects — UNLESS DL-129/DL-053 is amended so both ingestions share this
loop's fetch. That collision is the sharp edge Matt should rule on. Three
readings:

- **Option 1 (this record's stated assumption — recommended):** the
  board-ingestion poll is a DISTINCT, simpler loop that sinks to the
  projection; it is NOT the DL-053 per-subscriber delivery machinery.
  Per-subscriber DELIVERY cursors and the `Sessions` → `AgentGateway.Control`
  push path are pure overhead for a projection writer with exactly one,
  always-on consumer. **Unpriced cost, now stated:** the convergence with
  DL-053's fetch half is asserted, not designed — and it is not a free seam
  swap. DL-053's FETCH cursor is per-ARTIFACT; this client's cache is a
  per-LIST-PAGE ETag. Different granularities: a list-page ETag cannot become
  a per-artifact cursor by moving it behind a store seam, so a real merge is
  a rewrite of the fetch layer, not a relocation. Plus the PR-C double-fetch
  above is left for a future decision.
- **Option 1.5 (distinct loop, durable cursor from day one — the middle
  path):** keep Option 1's distinct, simple loop and its projection sink, but
  persist the fetch watermark (the `since` timestamp / page ETags) in a small
  Postgres table instead of T1's in-memory map. Cost: one migration + a tiny
  store method (T1 gains a cursor-store dependency; T2/T3 unchanged). Buys:
  no full `state=all` resync on every restart, survives a future multi-replica
  server, and makes the "shared fetch primitive" real rather than aspirational
  — a durable per-repo fetch cursor IS DL-053's FETCH-cursor idea in embryo,
  so a later convergence is an extension, not a rewrite. Still does NOT take
  on per-subscriber delivery. Recommended IF Matt wants the convergence story
  to be load-bearing; declined if in-memory-and-resync is acceptable for v1.
- **Option 2:** build this driver ON the DL-053 subscription machinery from
  day one (the board as a distinguished subscriber), so tracker-status
  ingestion (PR-C, per DL-129) lands on one poll path and no convergence
  refactor is ever needed, AND the double-fetch never arises. Cost: the
  DL-053 Postgres rows/cursors/delivery plumbing does not exist yet — this
  slice would inherit that entire build as a prerequisite, and SEA-1810's
  demo-facing scope (issues visibly on the board) would gate on it.

**Recommendation: Option 1** for fastest demo-facing value, with **Option 1.5
as the hedge** if the convergence with PR-C/DL-053 must be designed now rather
than deferred. If Matt rules Option 2, T2/T3 are re-cut (T1 survives nearly
intact — the client is the shared fetch half either way). The PR-C
double-fetch is the concrete thing each option resolves differently. PARKED
for Matt; the freeze waits on this ruling.

### OQ-B (non-load-bearing deferral): live-demo content

Whether the dogfood demo shows LIVE GitHub issues — which would make this
driver demo-critical and re-sequence its priority — is a later Matt scope
call per the SEA-1810 issue body's own escalation trip-wire. Recorded as an
explicitly non-load-bearing deferral: the design is correct regardless of the
ruling; only scheduling changes.
