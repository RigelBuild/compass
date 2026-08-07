# Compass forge-poll driver (SEA-1810)

Status: Draft
Lane: compass-server
Tracker: SEA

OQ-A is RESOLVED — Matt ruled **Option 2** (2026-08-07): this driver is built
ON the DL-053 forge-subscription machinery from day one, with the board as a
distinguished subscriber. Freeze now waits on OQ-C and OQ-D (see Open
Questions) — two shape calls inside the Option-2 build. This record designs
the running driver that periodically drives the existing greenfield
`go/internal/ingest` library against a real GitHub forge, so board issues
ingested from the tracker appear live in the Compass server projection — and
it instantiates the DL-053 fetch-cursor model (migration + store + poll
driver) as it does so. Design layer only — no implementation code ships with
this record.

## Problem / Intent

Board issues ingested from the external tracker (GitHub) must appear live in
the Compass server projection. Today nothing drives ingestion: `server/serve.go`
wires no poller and imports neither `internal/ingest` nor `internal/forge`
(verified this run — the only mentions of a poller in `go/server` are forward
references, e.g. `server/board.go:40` "the PR-B poll driver … add producers
without changing the executor signature", and the pgtest comment
`server/service_board_pgtest_test.go:14` "exactly as part 3's poller would").
The `internal/ingest` library (`ingest.go:35-65`) is complete and tested but is
exercised only by its own tests and the board pgtest suite.

This slice adds the running driver as **the first DL-053 forge subscriber**:
the DL-053 conditional-poll loop (durable Postgres FETCH cursor → conditional
GET → sink → advance), started at serve boot under the serve errgroup, with
the board as its distinguished, always-on local subscriber and the first real
(non-fake) forge read client behind it. The DL-053 forge-subscription tables
do not exist yet (migrations in `go/internal/store/migrations/` stop at
`0014_channel_policy.sql`; a repo-wide search for `forge_subscriptions` /
`forge_artifact_cursors` under `go/` returns nothing — the existing
`0006_delivery_cursors.sql` table is `agent_delivery_cursors`, the comms
agent-notification fan-out, "how far an agent has confirmed delivery on a
channel", a different subsystem entirely). This slice therefore builds the
fetch half of that machinery: the migration, the cursor store, and the poll
driver that PR-C (tracker-status ingestion, DL-129) and the later
agent-notification slice both ride — one fetch path, no convergence refactor
ever needed.

## Global Constraints

- **Toolchain:** module `github.com/sealedsecurity/compass/go`, go 1.25.0
  (`go/go.mod`).
- **DL-053 (FROZEN, Matt 2026-07-27, `DECISIONS.md:76`) — the target model,
  not an option:** "Forge subscriptions are Server-side Postgres rows with a
  per-artifact FETCH cursor (advanced on any 200) split from a per-subscriber
  DELIVERY cursor (advanced only on that subscriber's own successful notify),
  change-detected by conditional polling in v1 (webhooks are an additive
  accelerator), delivered by account on the existing `Sessions` →
  `AgentGateway.Control` push path." Full text + DDL:
  `compass-server-ownership-layer/design.md:942-1019` (Decision 5). This
  record consumes that model — it never amends it. The genuine forks in
  applying it to a repo-LIST board subscriber are PARKED for Matt as OQ-C (how
  the board is represented as a subscriber) and OQ-D (whether the two spec'd
  tables land in this migration at all, and if so which provider domain their
  CHECK admits). What is NOT a fork: the repo-LIST fetch-cursor CLASS
  (`forge_list_cursors`, Approach (a)) is author-adopted as the only coherent
  shape for a LIST-granularity ETag — invariant under both OQ-C options, so
  ratifying either ratifies the class — and the coordinate alignment to the
  tree's 0013 convention (SMALLINT provider + `forge_host`) is the mechanical
  application of an existing standard. The two spec'd tables are labeled
  `0004_forge` in that (older) record; the tree's migrations reach `0014`, so
  if they land they land here as part of `0015_forge_subscriptions.sql`.
- **DL-129 (FROZEN, Matt 2026-08-04, `DECISIONS.md:173`):** tracker native
  status "is ingested into the DL-070 server projection through the reverse
  `TrackerStatusMapping` (DL-053 poll; echo-suppressed in tracker-status
  space, tracker-sourced transitions never mirror back, stale polls dropped
  by a recency guard)". That names THIS driver as the poll PR-C rides:
  tracker status is a field on the same `/issues` rows this loop fetches
  (`forge.Issue.State`, `provider.go:45-46` — "the forge's truth
  (\"open\" | \"closed\")"), so PR-C MUST NOT re-fetch the endpoint (see
  Approach → the one-fetch-path guarantee).
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
- **Idempotency (load-bearing, now expressed over the DURABLE cursor):** a
  failed or partial poll is safe to retry — "partial progress is fine — a
  re-poll is idempotent on the coordinate" (`ingest.go:48-52`; the coordinate
  is the store upsert key, `store/migrations/0013_issues.sql:57-59`). The
  invariant the durable FETCH cursor must preserve: **a stored list-page ETag
  attests that every row of that page's content was durably sunk when the
  ETag was stored.** Concretely: the driver advances a page's cursor row ONLY
  AFTER every issue on that page has sunk through
  `IssueProjection.PublishIssueUpdate` (a durable upsert,
  `issue_projection.go:68-74`). A mid-page sink failure leaves the old ETag in
  place, so the next tick's conditional GET carries the OLD ETag, the changed
  content returns `200`, and the page re-sinks — no issue is ever stranded
  behind an advanced cursor. This is the same invariant DL-053's
  "advances unconditionally on any 200" expresses for per-artifact rows,
  where the fetched state persists as `snapshot` **in the same row as the
  ETag** (`compass-server-ownership-layer/design.md:1016`): never advance a
  fetch cursor past state you have not durably captured. Here the durable
  capture is the projection upsert itself, so the advance follows the sink;
  the advance is still NEVER conditioned on any subscriber's wire delivery
  (the board has none — see Approach), which is the coupling DL-053 forbids.
- **No projection change:** `board.IssueProjection.PublishIssueUpdate(ctx,
  *compassv1.Issue) error` (`board/issue_projection.go:68`) ALREADY satisfies
  `ingest.issueSink` (`ingest.go:31-33`). This slice adds zero code to
  `internal/board`.
- **`ingest` imports no store:** the package doc holds that "it imports NO
  store. The proto->store mapping edge is part 4 (the sink's real
  implementation)" (`ingest.go:7-8`). The driver's durable-cursor dependency
  therefore enters `ingest` as a package-local structural interface
  (`CursorStore`, T3), with the `*store.Store` adapter living in `server/`
  (T4) — the same narrow-seam pattern as `forgeReader`/`issueSink`
  (`ingest.go:20-33`).
- **Non-goals:** the tracker-status ingestion itself (PR-C — the reverse
  `TrackerStatusMapping`, echo-suppression, recency guard; a separate slice
  that consumes the seam this record makes explicit); the tracker-status
  WRITE-back / outbound mirror (T3-b); the agent-notification DELIVERY half
  of DL-053 (per-agent `forge_subscriptions` rows, `DetectChanges`,
  per-artifact comment/check ETags, the `Sessions` → `AgentGateway.Control`
  push) — that slice adds subscribers and wire delivery ON the tables and
  driver built here, it does not rebuild the fetch path. This record is the
  BOARD-ingestion read poll plus the DL-053 fetch machinery it stands on.

## Approach

Four pieces, one per task: a concrete GitHub read client in `internal/forge`
(T1, carried over from the pre-ruling cut nearly intact), the DL-053 cursor
migration + store layer (T2), the conditional-poll driver in `internal/ingest`
(T3), and boot wiring + config surface in `server/serve.go` +
`cmd/compass-server/main.go` (T4).

### The impedance Option 2 must design around: per-artifact vs repo-LIST

DL-053's subscription model is **per-artifact**: "An agent subscribes to a
*forge artifact*: an issue or a PR, named by `(provider, repo, kind, number)`"
(`compass-server-ownership-layer/design.md:944-945`), and the FETCH cursor
table is keyed the same way (`:1007-1019`, `PRIMARY KEY (provider, repo, kind,
number)`). SEA-1810's board ingestion is **per-repo LIST**: fetch every issue
in a repo in one paginated `/issues?state=all` walk (`Ingester.Ingest`
"fetches every issue for repo", `ingest.go:48`) and sink each. A list-page
ETag is a caching fact about a URL (repo + filter + page), not about any one
artifact — it cannot be stored per-artifact without inventing data. Three
framings were weighed:

- **(a) The board is a distinguished repo-scoped subscriber — ADOPTED for the
  fetch cursor.** The repo LIST is its own fetch-cursor class: a new
  `forge_list_cursors` table keyed `(provider, host, repo, page)` holding the
  per-page ETag + Link-chain fact (`has_next`), sibling to (not a widening of)
  the DL-053 per-artifact `forge_artifact_cursors`. It is the same
  fetch-cursor IDEA — a durable conditional-GET cache, advanced with the
  durably captured state, shared by every consumer of the fetch — applied at
  the LIST granularity. The board's DELIVERY cursor is degenerate: the board
  is one always-on, in-process consumer whose "notify" is the synchronous
  projection sink inside the poll pass, so sunk-ness is already encoded by
  the page-cursor advance (Global Constraints → Idempotency) and no
  `delivered_revision` row is needed — exactly the per-subscriber cursor
  DL-053 splits out for subscribers whose notify can fail independently of
  the fetch, which the board's cannot (blessing this reading is OQ-C).
- **(b) The board subscribes to each issue individually, seeded by a repo
  enumeration — REJECTED.** N `forge_subscriptions` rows per repo, and the
  enumeration that seeds them IS a repo LIST fetch — so the LIST (and its
  paging/ETag problem) remains, plus a per-issue row-churn loop on top. It
  removes nothing and adds a reconcile.
- **(c) The LIST fetch feeds per-artifact state as it enumerates — ADOPTED as
  the sharing seam.** Every `/issues` LIST row carries the full issue payload
  including its forge state (`forge.Issue.State`, `provider.go:45-46`), so
  the one LIST fetch can feed N consumers: the board sink today; PR-C's
  reverse-status mapping tomorrow (same pass, second sink — it consumes the
  rows this fetch already returns, never a second request, though it reads
  fields beyond `State`: DL-129's recency guard needs the row's `updated_at`,
  which T1 therefore parses into `forge.Issue` from day one, see the
  one-fetch-path guarantee); and, when the agent-notification slice lands,
  LIST-fed upserts of `forge_artifact_cursors.revision`/`snapshot`/`polled_at`
  for subscribed issues (their per-issue `etag` column stays `''` — it belongs
  to the single-issue endpoint the LIST never hits; comments/checks ETags
  likewise arrive only with that slice's own endpoints). The driver exposes
  this as a seam (T3: the per-pass raw-issue batch flows through one
  `Ingester.IngestIssues` call a second sink can be composed onto), and this
  record keeps every downstream writer OUT of its build.

**The one-fetch-path guarantee (the property Matt's ruling bought), stated
explicitly:** PR-C never issues a request against `/repos/{owner}/{repo}/issues`.
The endpoint has exactly one caller — this driver's T1 client — and exactly one
cursor state — `forge_list_cursors`. PR-C lands as an additional consumer of
the same pass (the reverse `TrackerStatusMapping` applied to the same
`[]forge.Issue` batch the board sink receives), so DL-129's "DL-053 poll" IS
this driver, the shared rate budget is spent once, and no convergence refactor
is ever needed. What PR-C adds on the fetch side: no request, no endpoint, no
cursor state — only, at most, additional fields parsed off rows this fetch
already returns (the `updated_at` its recency guard needs is parsed here in
T1; `state_reason` similarly, if the reopen mapping wants it). "One fetch
path" is exact; "adds literally nothing" would not be, so the claim is stated
as the former.

### The DL-053 tables (`0015_forge_subscriptions.sql`)

This record's DESIGN lands all three tables in one migration — the two
DL-053-spec'd tables plus the repo-LIST cursor class — adapted from the
ownership-layer DDL (`compass-server-ownership-layer/design.md:976-1019`, there
labeled "`0004_forge`"; the label predates the current migration count, so it
lands as `0015`). Two calls in that adaptation are author-decided (mechanical
application of an existing tree standard); one is a genuine fork PARKED for
Matt as OQ-D.

Author-decided:

- **Coordinate alignment:** the spec'd DDL keys on `provider TEXT` and omits
  the forge host; the tree's frozen issue coordinate is `(forge_provider
  SMALLINT CHECK (forge_provider IN (1, 2, 3)), forge_host, repo, number)`
  (`0013_issues.sql:29-35` — "forge coordinate: the idempotency key"). The
  cursor and subscription keys adopt the 0013 convention (SMALLINT provider
  enum + `forge_host TEXT NOT NULL`) so cursor rows join the `issues`
  coordinate without casts and GHES hosts never collide on `repo` alone.
- **The new LIST-cursor class:** `forge_list_cursors` (below) is additive —
  neither spec'd table changes shape, and it lands regardless of OQ-D (the
  driver rides it directly).

PARKED for Matt (OQ-D):

- **Whether the two spec'd tables land in THIS slice at all.** Nothing this
  slice EXECUTES touches `forge_subscriptions` or `forge_artifact_cursors`: the
  driver rides `forge_list_cursors` only, the T2 store methods cover only the
  list cursor, and every downstream writer is out of this build. So they land
  now purely as anticipatory schema. The design's default (what the DDL below
  assumes) is land-now — one forge migration, the shape frozen + pgtest-covered
  before its writers exist. The counter-position (defer each spec'd table to
  the slice that first writes it) is real: an EMPTY table is the cheapest thing
  to `ALTER`, so freezing its shape early buys little over landing it beside
  its first writer with more information, and landing it now FORCES the OQ-D
  provider-domain call (below) before PR-C or the agent-notification slice
  exist to inform it. This is OQ-D's first sub-fork.
- **If they land now: which provider domain their CHECK admits.** DL-053's
  spec'd DDL keys `provider TEXT -- "github" | "linear"`
  (`compass-server-ownership-layer/design.md:981`) — Linear is a first-class
  provider of subscription/cursor rows in the frozen spec. Adopting the 0013
  coordinate's `SMALLINT CHECK (... IN (1, 2, 3))` (GitHub/GitLab/Forgejo,
  `0013_issues.sql:33`) consciously NARROWS that domain: it drops `linear`
  until the `ForgeProvider` enum and the CHECK grow. This is not "no semantic
  change" — DL-129 routes tracker-status ingestion through "the DL-053 poll"
  (`DECISIONS.md:173`) and the tracker for MVP is GitHub, so the narrowing is
  likely harmless FOR NOW, but a future Linear tracker needs the enum/CHECK
  extended (cheap while the tables are empty). OQ-D's second sub-fork; moot if
  the first resolves to defer.

If OQ-D resolves to defer, this migration lands `forge_list_cursors` alone and
the two spec'd tables ride their first writers; the rest of this record is
unchanged (the driver never touched them). The DDL below is the driver's own
working table either way.

The driver's own working table:

```sql
-- FETCH cursor for a repo-level issue LIST walk: one row per (coordinate,
-- page). The repo-LIST analogue of forge_artifact_cursors — a durable
-- conditional-GET cache, one writer (the poll driver), N read-side consumers.
-- etag advances ONLY after every row of that page's content is durably sunk
-- (the projection upsert is the snapshot store — see the record's Idempotency
-- constraint). has_next persists the Link-chain fact so a 304 (which need not
-- re-send Link) can keep walking a multi-page repo. NOTE the one gap of the
-- stored-Link walk: content that enters the repo sorted BEYOND the last stored
-- page (an issue transferred/imported with an old created_at, under GitHub's
-- default created-desc order) perturbs no stored page — every page 304s and
-- the last row's has_next=false stops the walk, so the new content is missed
-- until unrelated page-1 activity forces a 200. The driver closes this with a
-- bounded unconditional probe of last_page+1 every Nth all-304 walk (see the
-- poll driver) — no schema change. advanced_at records the last content
-- advance (an etag-storing 200+sink), NOT the last poll: an all-304 tick reads
-- the page but rewrites no row, so the column deliberately names "last change".
CREATE TABLE forge_list_cursors (
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3)),
    forge_host     TEXT     NOT NULL,
    repo           TEXT     NOT NULL,
    page           INTEGER  NOT NULL CHECK (page >= 1),
    etag           TEXT     NOT NULL DEFAULT '',
    has_next       BOOLEAN  NOT NULL DEFAULT FALSE,
    advanced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (forge_provider, forge_host, repo, page)
);
```

What the durable cursor buys over the pre-ruling in-memory ETag map: no full
`state=all` re-fetch storm on server restart (an unchanged repo is an all-304
pass from the first post-boot tick — the projection is already durable and
`Rehydrate`-seeded, `serve.go:259-263`, so a 304 page needs no re-sink);
a durable substrate a future multi-replica server COULD share (a precondition,
not a sufficiency — the projection sink assumes single-threaded ingestion per
coordinate, `issue_projection.go:62-67`, so multi-replica would still need
per-coordinate serialization / leader election this slice does not design);
and the "shared fetch primitive" PR-C rides is a real table, not an aspiration
inside one process's heap.

### The GitHub read client (`forge.GitHub`, new file `go/internal/forge/github.go`) — T1, survives the ruling

The pre-ruling T1 survives nearly intact — the client is the shared fetch half
either way. The ONE structural change the ruling forces: the client becomes
**stateless about cursors**. The pre-ruling cut kept a per-URL in-memory
ETag + parsed-response + Link-chain cache inside the client; under Option 2
the ETag lives in `forge_list_cursors` and the DRIVER owns it (it must — only
the driver can see the sink outcome the advance is gated on, which is exactly
why an in-client cursor was already flagged as structurally unable to be
correct: "the client's sole seam is `ListIssues([]Issue, error)`",
`provider.go:37-53` carries no timestamp and no sink signal). So the client
gains a page-level conditional-fetch primitive that takes the caller's ETag
and reports `NotModified` without caching anything, and the whole-walk
`ListIssues` remains as an unconditional convenience built on it (it is also
the read half of the eventual `forge.Provider`, `provider.go:195-207`; no
`var _ Provider` assertion — read half only).

A hand-rolled `net/http` client (OQ-6). Behavior, in order of the request
lifecycle:

1. **Auth.** Every request carries `Authorization: Bearer <token>` where the
   token comes from `TokenSource.Token(ctx)` — resolved per
   request batch, not captured at construction, so a rotated `server_only`
   secret takes effect without a restart (composes with DL-052; see T4 for the
   resolve wiring).
2. **Conditional requests (ETag/304) — the steady-state reducer.** The caller
   passes the page's stored ETag; the client sends `If-None-Match` and maps a
   `304 Not Modified` to `ListPage{NotModified: true}` — no body, no re-parse,
   nothing cached client-side. Per current GitHub docs a 304 is not charged
   against the core rate limit. **That fact is load-bearing for the budget
   math and MUST be reverified against current GitHub REST docs at T1** (the
   rate-limit docs have been revised before). On a no-change tick every page
   of a quiet repo costs zero rate budget and sinks nothing (the projection
   already holds the rows durably).
3. **Rate budget.** After every response the client records
   `x-ratelimit-remaining` and `x-ratelimit-reset`. Before issuing a request,
   if `remaining <= reserve` (default reserve 10), it fails fast with a typed
   budget error (`forge.ErrBudgetExhausted`, exact shape in T1 Interfaces)
   rather than burning the tail of the budget; the driver skips that cycle and
   retries next tick (safe: re-poll is idempotent, `ingest.go:51-52`). Budget
   exhaustion is an EXPECTED skip, not a failure — the driver detects
   `errors.Is(err, forge.ErrBudgetExhausted)` and logs it at `slog.Warn`, NOT
   the `slog.Error` path a real provider/sink error takes, so a rate-limited
   window is not per-tick Error noise. **403 disambiguation (a single rule —
   GitHub overloads 403 across rate-limit, bad-credentials, and
   repo-permission causes):** a `403`/`429` carrying `retry-after` OR a zeroed
   `x-ratelimit-remaining` → `forge.ErrBudgetExhausted` (Warn, budget-skip, NO
   token re-resolve); a `401`, or a `403` whose body `message` is a
   bad-credentials/permission error with NO rate-limit headers →
   `*forge.StatusError` AND, before returning, the client calls
   `Invalidate()` on its `TokenSource` (Interfaces below) so the NEXT batch
   re-resolves — the T4 TTL cache drops its value on `Invalidate`; the seam is
   the client's, since the client is the only component that observes the auth
   failure (the T4 secret-resolve bullet wires it); any other non-2xx →
   `*forge.StatusError` only
   (step 7). The discriminator is the presence of `retry-after` / a zeroed
   `x-ratelimit-remaining`, nothing else — so a dead token is never mistaken
   for a rate-limit skip (which would retry forever with the dead token,
   never re-resolving) and a rate-limit 403 never becomes per-tick Error spam.
   The client never sleeps holding the caller's poll slot — backoff is the
   driver's ticker, not an in-client `time.Sleep`. (Under sustained pressure a
   skipped tick can defer ingestion up to a full reset window (~60 min);
   acceptable given idempotency, and ETag/304 keeps steady-state spend near
   zero.)
4. **Pagination.** GitHub's list-issues endpoint pages; the client requests
   `per_page=100`. `ListIssuesPage` reports whether an RFC-5988 `Link:
   rel="next"` exists (`HasNext`) — the DRIVER walks pages so it can sink and
   advance the cursor per page; `ListIssues` follows the chain itself and
   concatenates (the `Ingester` contract is "fetches every issue for repo",
   `ingest.go:48`).
5. **Filter mapping.** `forge.IssueFilter{State, Labels}`
   (`provider.go:182-188`) maps to `state=`/`labels=` query params. An empty
   `State` — which is what the board path always sends — maps to
   **`state=all`**, this provider's documented default (`provider.go:183-184`
   delegates the empty case to "the provider's default"). Rationale: the
   board needs closed tracker issues to flow so their `ForgeState` stays fresh
   on the row; an open-only default would silently freeze closed issues at
   their last-seen forge state. (The reopen→un-archive LIFECYCLE transition
   rides tracker-STATUS ingestion — PR-C, DL-129 — not this slice, which
   writes `ForgeState` but never `State`; `state=all` here only keeps
   `ForgeState` current on closed rows. Under Option 2 PR-C applies that
   mapping to THIS driver's rows — the same `state=all` fetch is its input.)
6. **PR exclusion.** GitHub's issues API returns pull requests as issues
   (rows carrying a `pull_request` key); the client drops them — the board's
   PR surface is a separate lane, and `forge.Issue` (`provider.go:37-53`) is
   issue-shaped only.
7. **Error mapping.** Any non-2xx/non-304 response that step 3's 403
   disambiguation does NOT classify as a budget skip becomes
   `&forge.StatusError{Status: resp.StatusCode, Message: <GitHub message
   field>}` (`provider.go:216-227`), wrapped so `errors.As` finds it. Bodies
   are returned RAW — the owner-header strip belongs to ingestion
   (`provider.go:42-44`, `ingest.go:67-74`), never the provider.

### The conditional-poll driver (`ingest.Driver`, new file `go/internal/ingest/driver.go`) — the DL-053 poll

The DL-053 poll loop, specialized to its first artifact class (the repo LIST)
and its first subscriber (the board). It lives in the `ingest` package (it
composes the package's own `Ingester`; a new package would be a second
convention for no gain) and consumes the durable cursor through the
package-local `CursorStore` structural interface (Global Constraints → ingest
imports no store).

- `Run(ctx)` performs one immediate pass at start (so a freshly booted demo
  server shows the board without waiting a full interval), then ticks on
  `time.Ticker(interval)`.
- Each pass iterates the configured repos sequentially — one forge, one
  budget; parallel per-repo fetches would just race the same rate limit.
- **Per-repo pass = the DL-053 read-modify-advance cycle, page-wise:**

  1. Read the repo's stored page cursors (`CursorStore.ListCursor` → page →
     `{etag, hasNext}`).
  2. From page 1: `ListIssuesPage(ctx, repo, forge.IssueFilter{}, page,
     storedETag)`.
     - **304** → nothing changed on that page and its content is already
       durably sunk (the Idempotency invariant: a stored ETag attests the
       sink); continue to the next page if the STORED row says `has_next`
       (a 304 need not re-send `Link`, so the chain fact is read from the
       cursor row, not the response).
     - **200** → `Ingester.IngestIssues(ctx, repo, page.Issues)` (translate +
       sink each, stop at first sink error — the existing per-issue pipeline,
       `ingest.go:58-63`, factored to take a caller-fetched batch); ONLY on
       full-page sink success, `UpsertListCursorPage` with the new ETag +
       `HasNext`; continue while `HasNext`.
     - **Budget error** (`errors.Is(err, forge.ErrBudgetExhausted)`) → Warn,
       abandon this repo's pass; already-advanced pages stay advanced (their
       content is sunk — safe).
     - **Any other error** (provider or sink) → `slog.Error`, abandon this
       repo's pass WITHOUT advancing the failing page; next tick re-fetches
       it (the old ETag no longer matches the changed content → 200 →
       re-sink; idempotent on the coordinate).
  3. After a clean full walk, `PruneListCursorPages(repo, lastPage)` — a repo
     that shrank drops its stale tail rows.
  4. **Boundary probe (closes the stored-`has_next` gap, M1):** the stored-Link
     walk in step 2 stops at the last stored page, so content that enters
     sorted BEYOND that boundary without perturbing any stored page (a
     transfer/import with an old `created_at`, under GitHub's default
     `created`-desc order) is invisible to a pure 304 walk until unrelated
     page-1 activity forces a 200. To bound the staleness, when a walk served
     ALL 304s (no page advanced) the driver unconditionally fetches
     `lastPage+1` once every Nth such consecutive all-304 walk (default N = 60,
     ~hourly at the 1m interval; the counter is per-repo, reset by any 200).
     A never-stored page is already an `etag=''` unconditional GET, so the
     mechanism exists — a `HasNext`/non-empty result there resumes the normal
     walk and re-anchors the tail; an empty 200 costs one request per hour on a
     quiet repo. This is a documented probe rule, not a fork.

- **A failed poll is logged, never fatal.** `Run` never returns a non-nil
  error — its sole exit is ctx cancellation, returning `nil` so the errgroup
  drain (see wiring) treats shutdown as clean.
- **Graceful shutdown:** `<-ctx.Done()` between passes and via the ctx
  threaded through fetch and sink mid-pass (both `ListIssuesPage` and
  `PublishIssueUpdate` take ctx).
- **The board's delivery half is degenerate, by design:** DL-053 splits the
  DELIVERY cursor per subscriber because "advancement conditional on a
  successful notify" over a shared cursor produces deterministic duplicates
  across subscribers with different liveness
  (`compass-server-ownership-layer/design.md:949-961`). The board is one
  in-process subscriber whose notify IS the synchronous sink inside the pass —
  there is no wire delivery to fail independently of the fetch, so its
  delivery state is fully encoded by the page-cursor advance and it holds no
  `forge_subscriptions` row (OQ-C). Wire subscribers (agents) arrive with the
  notification slice and bring their own `delivered_revision` rows, exactly
  per spec.
- **The PR-C seam (downstream consumer, OUT of this build):** the per-pass
  raw batch (`[]forge.Issue` per 200 page) flows through one
  `Ingester.IngestIssues` call. PR-C composes its reverse
  `TrackerStatusMapping` sink onto that same batch (and its per-artifact
  `forge_artifact_cursors.revision`/`snapshot`/`polled_at` upserts, LIST-fed —
  no per-issue GET), in its own slice. Nothing in this record's build writes
  `forge_subscriptions` or `forge_artifact_cursors`.
- **Observability:** one `slog.Info` per completed repo pass with fields
  `repo`, `issues` (count sunk), `pages`, `not_modified` (bool: whole walk
  served 304s), `dur`, `ratelimit_remaining`; one `slog.Warn` per
  budget-skipped pass (an expected condition, never `Error`); one
  `slog.Error` only per genuinely failed pass (provider/sink error) with
  `repo`, `err`. Boot logs one `slog.Info` with `repos`, `interval`. No
  per-issue logging — a 500-issue repo must not emit 500 lines per minute.

### Boot wiring + config (`server/serve.go`, `cmd/compass-server/main.go`)

Mirrors the two established precedents exactly:

- **Config surface:** `server.ServeConfig` (`serve.go:56-89`) gains an
  optional `Forge ForgeConfig` field, all-optional exactly like `S3`
  (`serve.go:71-75`): absent repos → no driver, today's behavior, zero new
  requirements on existing deployments (the 0015 tables exist but sit empty —
  a migration is not a behavior change). The CLI (`main.go`) maps flags with
  env fallback mirroring the `COMPASS_S3_*` precedence (`main.go:127-138`):
  `--forge-repos` / `$COMPASS_FORGE_REPOS` (comma-separated `owner/name`),
  `--forge-poll-interval` / `$COMPASS_FORGE_POLL_INTERVAL` (default `1m`),
  `--forge-secret` / `$COMPASS_FORGE_SECRET` (the declared secret NAME,
  default `GITHUB_FORGE_TOKEN`; the VALUE never crosses a flag), plus
  `--forge-host` / `$COMPASS_FORGE_HOST` (default `github.com`, for GHES
  later; the API base URL derives from it). The configured repo list IS the
  board's subscription set under this record's OQ-C recommendation —
  declarative, reconciled implicitly every pass (a removed repo simply stops
  being polled; its cursor rows are inert and its issues remain in the
  projection).
- **Secret resolve:** the driver's `TokenSource` closes over the one
  `secrets.SpecResolver` built at `serve.go:287`, calls
  `Resolve(ctx, "forge poll")` (`secrets/resolver.go:135`), and selects the
  configured name from the returned `[]ResolvedSecret` — the same
  single-resolve-surface DL-052 mandates; the row is declared `server_only`
  so it never reaches a container (mechanism per the ownership-layer T5,
  `compass-server-ownership-layer/design.md:1991-1995`). **Resolve is not
  cheap** — `resolver.go:135-165` reads the entire declared-secret registry
  from the store, writes a manifest temp file, and drives a full secretspec
  provider `Load` (potentially an external provider call) per invocation — so
  the `TokenSource` implementation caches the resolved value behind a short TTL
  (default `5m`); its `Invalidate()` (the T1 seam) drops the cached value so
  the next `Token` call re-resolves, and the client calls it on a
  `401`/bad-creds-`403` (Approach step 3). So a resolve happens on TTL expiry
  OR on an auth failure, NOT once per poll pass: a token that rotates rarely
  does not tax the store and secrets provider every minute, while a rotation
  still takes effect within the TTL (or immediately on the next auth failure).
  A startup resolve is attempted once when forge config is present, and its two
  failure modes get distinct error text so a crash-loop is diagnosable: an
  UNDECLARED/misnamed secret (the configured name is absent from the resolved
  set) is a permanent misconfiguration — `"forge secret %q not declared"`; a
  resolve that ERRORS at boot (secrets provider unreachable, store read
  failing) is transient — `"forge secret resolve failed at startup: %w"`. Both
  fail fast, but at `serve.go:287` the resolver is built AFTER the UDS listener
  binds, so the validation follows the listener-cleanup path the `Rehydrate`
  failure already uses (`serve.go:260-263`), returning the error from `Serve`
  rather than a bare `main.go` exit. A resolve that breaks at RUNTIME (provider
  outage, a declaration deleted post-boot) surfaces per-pass as an auth error
  → `slog.Error` + retry next tick (idempotent); to keep a permanently broken
  secret from degrading to silent forever-Error, the driver emits a distinct
  `slog.Error` "forge token unresolvable" the first time a resolve fails and
  on recovery — a minimal health surface without per-tick spam.
- **Cursor-store adapter:** `serve.go` builds the small adapter satisfying
  `ingest.CursorStore` over the already-open `*store.Store` (`st`), binding
  the driver's `(provider, host)` so the ingest-side interface stays
  repo-keyed. This is the store seam the ingest package's no-store rule
  requires (Global Constraints), following the established
  break-the-cycle/adapter convention of `wireHubServiceCycles`
  (`serve.go:298-301`).
- **Lifecycle:** the driver runs as one more `g.Go` member of the existing
  serve errgroup (`serve.go:327-339`) — `g.Go(func() error { return
  driver.Run(gctx) })` — so it inherits exactly the scoped lifecycle the
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
- **(b) A distinct, simpler board-ingestion loop with an in-memory ETag cache
  (the pre-ruling Option 1 / Option 1.5) — CLOSED by Matt's OQ-A ruling,
  2026-08-07.** Option 1 left the DL-053 convergence asserted-not-designed
  (a per-LIST-PAGE in-memory ETag is not a per-artifact cursor behind a store
  seam — a later merge would be a fetch-layer rewrite) and left the PR-C
  double-fetch of `/issues` for a future decision; Option 1.5 made the cursor
  durable but still deferred the subscriber model. Matt ruled Option 2: build
  on the DL-053 machinery from day one so PR-C lands on one poll path and no
  convergence refactor is ever needed. This record is that build. See Open
  Questions → OQ-A (resolved).
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
- **(e) Per-repo goroutines in the driver — rejected.** One forge, one rate
  budget; sequential per-repo fetches serialize naturally against
  `x-ratelimit-*` and keep the driver a trivially testable single loop.
- **(f) `since`/`updated` incremental sync — still deferred, but its correct
  home now EXISTS.** A `since` watermark would shrink a CHANGED tick (ETag/304
  only makes the no-change tick free; a changed page pays a full paid refetch
  because GitHub's default `created`-desc order shifts page boundaries on
  insert). The pre-ruling record could not adopt it: an in-client watermark
  cannot observe the sink outcome, so advancing on fetch success strands
  un-sunk issues on a mid-pass sink failure (Global Constraints →
  Idempotency). Under Option 2 the driver owns a DURABLE, sink-gated cursor —
  a `since` watermark advanced only on full-walk success is now a small
  additive column on `forge_list_cursors`, correct by the same invariant. It
  is still NOT in this slice (v1 cost is bounded and budget-gated without
  it); it is recorded as the first cheap optimization the durable cursor
  unlocks, adoptable without design change.
- **(g) Widening `forge_subscriptions` to hold the board as a row (nullable
  `agent_account_id` + a subscriber-kind discriminator) — recommended
  against; carried as the OQ-C alternative.** It would make "the board is a
  subscriber" literal at the price of amending DL-053's spec'd shape
  (`agent_account_id TEXT NOT NULL REFERENCES agent_accounts`,
  `compass-server-ownership-layer/design.md:980`) and forcing every future
  subscription query through a kind filter — for a subscriber that needs
  neither `delivered_revision` (its delivery is the in-pass sink) nor
  Subscribe/Unsubscribe dynamics (its set is operator config). See OQ-C.

## Plan

### T1 — `forge.GitHub`: the hand-rolled net/http GitHub read client

New file `go/internal/forge/github.go` (+ `github_test.go`). Carried over from
the pre-ruling cut with ONE structural change: the client is stateless about
cursors — the page-level primitive takes the caller's ETag and reports
`NotModified`; no in-memory ETag/response/Link cache exists. Implements the
request lifecycle from the Approach: bearer auth via `TokenSource`,
conditional GET per page, `x-ratelimit-*` budget gate with a typed budget
error + the 403 disambiguation rule, `per_page=100` + `Link`-header `HasNext`
detection, `IssueFilter` → query-param mapping (empty `State` → `state=all`),
`pull_request`-row exclusion, non-2xx → `*forge.StatusError`. Raw bodies pass
through untouched (`provider.go:42-44`). No `Provider` interface assertion —
read half only. One additive field: T1 widens `forge.Issue`
(`provider.go:37-53`) with `UpdatedAt time.Time`, parsed from the LIST row's
`updated_at`, so PR-C's DL-129 recency guard reads it off this same fetch
(Approach → one-fetch-path guarantee); the board sink ignores it.

Interfaces:

```go
// TokenSource yields the current forge token and lets the client drop a cached
// value when it observes an auth failure, so the next batch re-resolves. Token
// is called per fetch batch (not per request) so a rotated server_only secret
// takes effect without restart; the client calls Invalidate on a 401 /
// bad-creds-403 (Approach step 3). The T4 implementation is a TTL cache over
// the SpecResolver whose Invalidate drops the cached value (a bare func would
// have no seam to observe the auth failure through).
type TokenSource interface {
    Token(ctx context.Context) (string, error)
    Invalidate()
}

// GitHubConfig configures a GitHub read client.
type GitHubConfig struct {
    Host   string       // "github.com" or a GHES host; API base derives from it
    Token  TokenSource  // required
    Client *http.Client // nil -> a default client with a sane timeout
}

func NewGitHub(cfg GitHubConfig) *GitHub

// ListPage is one conditional page fetch result. On NotModified, Issues is
// nil and ETag/HasNext are zero — the caller's stored cursor row remains the
// truth for both.
type ListPage struct {
    Issues      []Issue
    ETag        string // the response ETag to store on sink success
    HasNext     bool   // an RFC-5988 Link rel="next" was present
    NotModified bool   // 304: content unchanged vs the etag argument
}

// ListIssuesPage fetches one page (1-based) conditionally: etag == "" is an
// unconditional fetch. The client holds NO cursor state — the caller owns it.
func (g *GitHub) ListIssuesPage(ctx context.Context, repo string, f IssueFilter, page int, etag string) (ListPage, error)

// ListIssues is the unconditional full walk (concatenated pages), built on
// ListIssuesPage. Satisfies ingest.forgeReader (ingest.go:23-25) structurally
// and is the read half of the eventual forge.Provider (provider.go:200).
func (g *GitHub) ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error)

// ErrBudgetExhausted is returned (wrapped) when x-ratelimit-remaining is at or
// under the reserve; the caller skips the cycle and retries next tick.
var ErrBudgetExhausted = errors.New("forge: rate budget exhausted")
```

Consumes: `forge.IssueFilter` (`provider.go:182-188`), `forge.Issue`
(`provider.go:37-53`, widened here with `UpdatedAt`), `forge.StatusError`
(`provider.go:216-227`),
`net/http`, `encoding/json` (stdlib only — OQ-6).

Test cycle (unit, stubbed `http.RoundTripper`; red-first per repo convention):

1. 200 with issues JSON → parsed `[]forge.Issue` field-by-field (incl. raw
   body untouched, labels, state, ForgeAccount, and `UpdatedAt` parsed from
   the row's `updated_at`).
2. `ListIssuesPage` with a non-empty `etag` sends `If-None-Match`; a scripted
   304 returns `NotModified: true` with nil Issues and no body parse; an
   empty `etag` sends NO `If-None-Match` header.
3. `x-ratelimit-remaining: 0` on response N → call N+1 fails
   `errors.Is(..., ErrBudgetExhausted)` WITHOUT issuing a request (assert via
   RoundTripper call count); after scripted reset, calls resume.
4. `HasNext` is true exactly when a `Link: rel="next"` header is present;
   `per_page=100` and the requested `page` land on the wire; `ListIssues`
   concatenates a scripted two-page walk in order.
5. Empty `IssueFilter.State` → `state=all` on the wire; labels join correctly.
6. A `pull_request`-keyed row is dropped.
7. 404/500, and a 403 WITHOUT rate-limit headers (a bad-credentials/permission
   `message`) → `errors.As(..., *forge.StatusError)` with matching Status and
   the GitHub `message` field, AND the client calls `Invalidate()` on a fake
   `TokenSource` exactly once (the 401/bad-creds-403 discriminator of Approach
   step 3 — asserted on the ABSENCE of `retry-after`/zeroed
   `x-ratelimit-remaining`, so a rate-limit 403 does NOT call `Invalidate`).
8. Token from `TokenSource` lands as `Authorization: Bearer …`; a TokenSource
   error propagates without a request.
9. `403`/`429` with `retry-after` (or a zeroed `x-ratelimit-remaining` on the
   prior response) → next call fails `errors.Is(..., ErrBudgetExhausted)`
   without issuing a request (RoundTripper call count asserted).
10. Absent/malformed `x-ratelimit-*` headers do not wedge the gate — the
    client proceeds (treats unknown budget as available) rather than
    permanently skipping.
11. ctx cancellation mid-`ListIssues`-walk (cancel after page 1) stops
    promptly and returns `ctx.Err()`, not a partial concatenation.
12. Reverify against current GitHub REST docs that a 304 is not charged
    against the core rate limit; record the doc URL + date in the client's
    doc comment (the budget math depends on it).

### T2 — migration `0015_forge_subscriptions.sql` + the cursor store layer

New migration `go/internal/store/migrations/0015_forge_subscriptions.sql` and
new file `go/internal/store/forge_cursors.go` (+
`forge_cursors_pgtest_test.go`, build-tag gated like the sibling
`*_pgtest_test.go` suites). The migration creates the repo-LIST cursor class
`forge_list_cursors` (DDL in the Approach) unconditionally, and — if OQ-D
resolves land-now — the two DL-053-spec'd tables (`forge_subscriptions`,
`forge_artifact_cursors`, adapted from
`compass-server-ownership-layer/design.md:976-1019`, renumbered from that
record's stale `0004_forge` label, keys aligned to the 0013 issue-coordinate
convention: SMALLINT provider enum + `forge_host` in every key; the
provider-domain CHECK is OQ-D's second sub-fork). Store methods cover ONLY the
list cursor in this slice — `forge_subscriptions` / `forge_artifact_cursors`
get their store surface with their writers (the agent-notification slice /
PR-C), whenever they land.

Interfaces:

```go
// package store

// ForgeListPageCursor is one durable page row of a repo's issue-LIST fetch
// cursor (the DL-053 FETCH-cursor model at repo-LIST granularity). ETag ""
// means never fetched (an unconditional GET).
type ForgeListPageCursor struct {
    Provider ForgeProvider // GITHUB(1)/GITLAB(2)/FORGEJO(3); never 0
    Host     string
    Repo     string
    Page     int32 // 1-based
    ETag     string
    HasNext  bool
}

// ForgeListCursor reads every stored page row for the repo, ascending page.
// No rows is a nil slice, not an error (a never-polled repo).
func (s *Store) ForgeListCursor(ctx context.Context, provider ForgeProvider, host, repo string) ([]ForgeListPageCursor, error)

// UpsertForgeListCursorPage inserts-or-updates one page row (touching
// advanced_at — the last CONTENT advance, since this is called only after a
// 200+sink; an all-304 tick rewrites no row). Called by the driver ONLY after
// the page's content durably sank
// — the advance-attests-sink invariant lives in the caller; the store method
// is a plain upsert. Zero/empty coordinate fields -> ErrInvalidArgument.
func (s *Store) UpsertForgeListCursorPage(ctx context.Context, cur ForgeListPageCursor) error

// PruneForgeListCursorPages deletes the repo's page rows with page > maxPage
// (a repo whose walk shrank). maxPage < 1 -> ErrInvalidArgument.
func (s *Store) PruneForgeListCursorPages(ctx context.Context, provider ForgeProvider, host, repo string, maxPage int32) error
```

Consumes: the migration runner + `pgtest` harness the sibling suites use
(`store/issues_pgtest_test.go` et al.), `ErrInvalidArgument` (the store's
existing sentinel, `issues.go:103-108` pattern).

Test cycle (pgtest, isolated schema per case like `issues_pgtest_test.go`):

1. Migration applies cleanly from empty AND from a 0014 database (the
   sequential-migration harness proves both); `forge_list_cursors` exists with
   its PK/CHECKs (provider CHECK rejects 0, page CHECK rejects 0), plus the two
   spec'd tables if OQ-D resolves land-now.
2. `ForgeListCursor` on a never-polled repo → nil, no error.
3. Upsert page 1 then re-upsert with a new ETag → one row, new ETag,
   `advanced_at` advanced; rows come back ascending by page.
4. Two hosts / two providers with the same `repo` string do not collide (the
   OQ-D coordinate rationale, proven).
5. `PruneForgeListCursorPages(maxPage=2)` on a 4-page repo leaves pages 1-2;
   pruning a never-polled repo is a no-op.
6. Invalid input: zero provider / empty host / empty repo / page 0 →
   `ErrInvalidArgument` on each method.
7. **(Only if OQ-D resolves land-now)** `forge_subscriptions` FK: inserting a
   row with an unknown `agent_account_id` fails (RESTRICT, per the DL-053 DDL
   and the 0006-established convention `0006_delivery_cursors.sql:6-9`) — the
   table is writer-less this slice but its shape is proven here. If OQ-D
   resolves defer, this case drops and test 1 asserts only `forge_list_cursors`
   exists.

### T3 — `ingest.Driver`: the DL-053 conditional-poll driver

New file `go/internal/ingest/driver.go` (+ `driver_test.go`), plus one
refactor inside `ingest.go`: extract the translate+sink loop of `Ingest`
(`ingest.go:58-63`) into `IngestIssues(ctx, repo, raws)` so the driver can
feed caller-fetched page batches through the existing per-issue pipeline;
`Ingest` becomes `ListIssues` + `IngestIssues` (behavior unchanged — the
existing `ingest_test.go` suite is the regression net). Pass algorithm,
page-wise cursor advance, budget/error classification, logging per the
Approach; `nil` return on ctx cancellation.

Interfaces:

```go
// package ingest

// IngestIssues translates + sinks a caller-fetched batch of raw issues for
// repo: the loop body of Ingest, exposed so the DL-053 driver (which fetches
// page-wise to gate cursor advance on sink success) reuses the one pipeline.
// Stops and returns the first sink error (wrapped); partial progress is fine —
// a re-poll is idempotent on the coordinate.
func (in *Ingester) IngestIssues(ctx context.Context, repo string, raws []forge.Issue) error

// ListPageCursor is the driver's view of one durable page-cursor row.
type ListPageCursor struct {
    Page    int
    ETag    string
    HasNext bool
}

// CursorStore is the durable FETCH-cursor surface the driver needs — a narrow,
// repo-keyed structural seam (the server wiring adapts *store.Store and binds
// provider+host), keeping this package's no-store property (ingest.go:7-8).
type CursorStore interface {
    ListCursor(ctx context.Context, repo string) ([]ListPageCursor, error)
    UpsertListCursorPage(ctx context.Context, repo string, cur ListPageCursor) error
    PruneListCursorPages(ctx context.Context, repo string, maxPage int) error
}

// pageLister is the fetch seam: the page-level conditional read of the forge
// client (satisfied by *forge.GitHub structurally; faked in tests).
type pageLister interface {
    ListIssuesPage(ctx context.Context, repo string, f forge.IssueFilter, page int, etag string) (forge.ListPage, error)
}

// DriverConfig configures the board-ingestion poll driver.
type DriverConfig struct {
    Repos    []string      // "owner/name", non-empty (validated by the caller)
    Interval time.Duration // > 0; the caller defaults it
    Log      *slog.Logger  // nil -> slog.Default()
}

func NewDriver(client pageLister, ing *Ingester, cursors CursorStore, cfg DriverConfig) *Driver

// Run polls until ctx is cancelled, then returns nil (clean shutdown).
// Per-repo errors are logged and retried next tick, never returned — a
// re-poll is idempotent on the coordinate (ingest.go:48-52).
func (d *Driver) Run(ctx context.Context) error
```

Consumes: `Ingester` (`ingest.go:35-46`), `forge.ListPage` /
`forge.ErrBudgetExhausted` (T1), `time.Ticker`, `log/slog`.

Test cycle (unit, fake `pageLister` + fake `CursorStore` + the package's
existing `recordingSink` pattern, `ingest_test.go:13-27`):

1. `Run` performs an immediate pass for every repo before the first tick
   (short interval, assert call order/coverage).
2. ctx cancel BETWEEN ticks → `Run` returns `nil` promptly, and ctx cancel
   DURING a pass also returns `nil` promptly — deadline-gated, never a sleep
   (the pgtest convention, `service_board_pgtest_test.go:15`). Assert `Run`
   does NOT return while ctx is live (it has no non-cancel exit).
3. Conditional walk: stored cursors `{1: e1 hasNext, 2: e2}` → page 1 fetched
   with `etag=e1`; a scripted 304 on page 1 still walks to page 2 (the
   stored `HasNext`, not the response, carries the chain); an all-304 pass
   sinks nothing and upserts nothing.
4. **Cursor advance gated on sink success (defends the Idempotency
   invariant):** a 200 page of N issues whose sink fails at issue k →
   `UpsertListCursorPage` is NOT called for that page and the pass aborts for
   that repo; next tick re-fetches the page with the OLD etag and every issue
   k..N sinks — none stranded. On full-page sink success the upsert carries
   the response ETag + HasNext.
5. Prune: a walk that ends at page 2 after cursors held 4 pages calls
   `PruneListCursorPages(repo, 2)`; an aborted (error/budget) pass never
   prunes.
6. A scripted failure on repo A does not stop repo B in the same pass, and
   repo A is retried on the next tick.
7. Budget skip: `ErrBudgetExhausted` from the fetch → Warn (not Error), pass
   abandoned, pages already advanced this pass stay advanced.
8. Interval respected: N ticks in a bounded window drive exactly N+1 passes
   (the +1 is the immediate pass); no busy-loop.
9. Log fields: capture a `slog` test handler and assert a happy pass logs at
   Info with `repo`/`issues`/`pages`/`dur`, a budget-skip logs at Warn, and a
   genuine error logs at Error with `err`.
10. `IngestIssues` refactor regression: `Ingest` still fetches-then-sinks
    identically (existing `ingest_test.go` cases stay green unmodified), and
    `IngestIssues` on a batch stops at the first sink error with the same
    wrapped error shape (`ingest.go:60-62`).

### T4 — serve boot wiring + config surface + secret resolve

Touches `server/serve.go`, `cmd/compass-server/main.go` (edits only, no new
files beyond tests). `ServeConfig.Forge` all-optional like `S3`; CLI
flags/env per the Approach; fail-fast on forge-config-present but
secret-unresolvable; the `ingest.CursorStore` adapter over the open
`*store.Store`; driver under the existing errgroup.

Interfaces:

```go
// server.ForgeConfig configures the board-ingestion poll driver (SEA-1810).
// All-optional: empty Repos leaves the driver off (today's behavior).
type ForgeConfig struct {
    Host         string        // default "github.com"
    Repos        []string      // "owner/name"; empty -> no driver
    SecretName   string        // declared server_only secret name; default "GITHUB_FORGE_TOKEN"
    PollInterval time.Duration // default time.Minute
}
// ServeConfig gains: Forge ForgeConfig

// unexported in server/: the ingest.CursorStore adapter binding the driver's
// forge coordinate half, so the ingest-side seam stays repo-keyed.
type forgeCursorStore struct {
    st       *store.Store
    provider store.ForgeProvider
    host     string
}
// satisfies ingest.CursorStore: ListCursor / UpsertListCursorPage /
// PruneListCursorPages, each delegating to the T2 store methods with the
// bound (provider, host).
```

Consumes: `secrets.SpecResolver.Resolve(ctx, reason) ([]ResolvedSecret, error)`
(`secrets/resolver.go:135`), `secrets.ResolvedSecret{Name, Value, …}`
(`secrets/secrets.go:132-135`; Value redacted under all fmt verbs — safe to
thread), the `board.NewIssueProjection` instance from `serve.go:259`,
`ingest.NewIngester` (`ingest.go:44`), `forge.NewGitHub` (T1), the T2 store
methods, `ingest.NewDriver`/`Run` (T3), the errgroup at `serve.go:327`,
`compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB`
(`gen/compass/v1/compass.pb.go:449`).

Produces: a `TokenSource` closure over the resolver that re-resolves +
selects `SecretName` on TTL expiry or auth failure (not per pass; see the
Secret-resolve bullet); the cursor-store adapter; boot-time validation error
text for missing-secret / bad-repo-format / non-positive interval.

Test cycle:

1. Unit: CLI flag/env precedence for the four forge knobs mirrors the S3
   pattern (table test like `resolveNetworkDoor`'s, `main.go:184-212`);
   repo-format validation (`owner/name`) rejects garbage at startup.
2. Unit: empty `Forge.Repos` → `Serve` builds no driver (assert via config
   plumbing seam, no behavioral change to existing serve tests — the whole
   existing suite is the regression net; the 0015 migration alone must not
   change any existing test's outcome).
3. pgtest (build-tag gated like the board suite): a `Serve`-assembled
   pipeline over a fake pager scripted with one issue page sinks to the real
   projection, lands in `ListBoardIssues`, AND leaves a
   `forge_list_cursors` page-1 row carrying the scripted ETag — the
   end-to-end boot-wiring + durable-cursor proof without touching live
   GitHub.
4. pgtest (restart-resync proof — the durable cursor's headline buy): after
   test 3, rebuild the pipeline over the SAME schema (a "restart") with the
   pager scripting a 304 for the stored ETag; assert the pass issues no
   unconditional fetch, sinks nothing, and the projection still serves the
   issue (Rehydrate covers the read side, `serve.go:259-263`).
5. pgtest (the no-clobber contract's runtime proof — the test MUST be able to
   go RED): (1) ingest/create the issue via the projection, (2) set a
   non-default lifecycle `State` through the part-5 write path
   (`store.SetIssueState`, `issues.go:140`), (3) re-ingest a forge UPDATE for
   the same coordinate (forge_state open→closed) via the fake pager, (4)
   assert `ForgeState` is updated AND `State` is unchanged from the human-set
   value (`issue_projection.go:208`, `protoToForgeFields` writes `ForgeState`
   but never `State`). A regression adding `state = EXCLUDED.state` to the
   `UpsertIssueForgeFields` SET clause (`issues.go:121-125`) MUST turn this
   red — the as-worded "State left untouched" over a zero-value row would stay
   green vacuously, so the human-set baseline in step 2 is load-bearing.
6. Unit: the T4 `TokenSource` re-resolves after its TTL, and calling its
   `Invalidate()` drops the cached value so the next `Token` re-resolves and
   the changed resolver value is used on the next fetch — the design's stated
   reason for a TTL-cache `TokenSource` with an invalidation seam rather than a
   captured token or a bare func.
7. Startup failure, two distinct texts: (a) forge config present + secret NAME
   absent from the resolved set → `Serve` returns `"forge secret %q not
   declared"`; (b) the resolve call itself ERRORS at boot → `Serve` returns
   `"forge secret resolve failed at startup: %w"`. Both follow the
   listener-cleanup path the `Rehydrate` failure uses (`serve.go:260-263`),
   since the resolver is built after the UDS listener binds (`serve.go:287`);
   assert the two error strings are distinguishable so a permanent misconfig is
   not confused with a transient provider outage.

## Tasks

- [ ] **T1** — `forge.GitHub` read client (`github.go` + unit tests over a
  stubbed RoundTripper: 200/parse incl. `UpdatedAt`, conditional
  `ListIssuesPage`/304, budget gate + 403 disambiguation + `TokenSource.
  Invalidate` on bad-creds, `HasNext`/pagination, filter mapping incl.
  `state=all` default, PR exclusion, StatusError mapping, bearer auth,
  304-uncharged doc reverify).
- [ ] **T2** — migration `0015_forge_subscriptions.sql` (`forge_list_cursors`
  always; `forge_subscriptions` + `forge_artifact_cursors` per DL-053,
  coordinate-aligned, IF OQ-D resolves land-now) + `store/forge_cursors.go`
  list-cursor methods; pgtest-covered.
- [ ] **T3** — `ingest.Driver` conditional-poll driver (`driver.go` + the
  `IngestIssues` extraction in `ingest.go`; unit tests: immediate pass, ctx
  cancel → nil, conditional walk over stored cursors, sink-gated cursor
  advance, prune, per-repo error isolation, budget-skip classification,
  interval discipline, slog fields).
- [ ] **T4** — serve boot wiring: `ServeConfig.Forge`, CLI flags/env, DL-052
  secret resolve via the existing `SpecResolver`, the `ingest.CursorStore`
  adapter over `*store.Store`, errgroup membership, fail-fast validation;
  unit + pgtest end-to-end incl. the durable-cursor restart proof and the
  no-clobber proof.

## Open Questions

### OQ-A — RESOLVED (Matt, 2026-08-07): Option 2

Matt ruled **Option 2**: this driver is built ON the DL-053 subscription
machinery from day one — the board as a distinguished DL-053 subscriber over
the per-artifact FETCH-cursor model's durable Postgres cursors — so
tracker-status ingestion (PR-C, per DL-129) lands on one poll path, no
convergence refactor is ever needed, and the PR-C `/issues` double-fetch never
arises. The T1 GitHub read client survives the ruling nearly intact (it is the
shared fetch half either way; its one structural change — cursor state moves
out of the client into `forge_list_cursors` — is the Approach's stateless-client
bullet). The superseded Option 1 / Option 1.5 framings and their tradeoffs are
retained in Alternatives (b) for the reasoning trail; the fork is closed and
is not relitigated here. The follow-on shape calls INSIDE Option 2 are OQ-C
and OQ-D below.

### OQ-C (LOAD-BEARING — blocks the freeze): how the board is represented as a subscriber

DL-053's subscription row is per-artifact and agent-owned
(`agent_account_id TEXT NOT NULL REFERENCES agent_accounts`,
`compass-server-ownership-layer/design.md:980`); the board subscribes to whole
repos and is not an agent. Two representations:

- **Config-declared (RECOMMENDED, and what this record designs):** the
  board's subscription set IS `ForgeConfig.Repos` — operator config, no
  `forge_subscriptions` rows, no delivery cursor (the board's notify is the
  synchronous in-pass projection sink, so sunk-ness is already encoded by the
  sink-gated `forge_list_cursors` advance; a `delivered_revision` for it
  would duplicate what the `issues` table already records). The repo-LIST
  fetch state lives in the new sibling table `forge_list_cursors` (Approach);
  DL-053's two spec'd tables land untouched in shape and gain their writers
  in the later slices. Cost: when agent subscriptions arrive, the poll driver
  enumerates targets from two sources (config repos for the LIST class,
  subscription rows for the per-artifact class) — acceptable because the two
  classes hit different endpoints with different cursor tables either way,
  and both share the one client, budget gate, and pass scheduler.
- **A widened subscription row (Alternatives (g)):** nullable
  `agent_account_id` + a subscriber-kind discriminator + a repo-level
  artifact kind, making the board a literal `forge_subscriptions` row. Buys a
  single target-enumeration query; costs an amendment to DL-053's spec'd
  shape and a kind-filter on every future subscription query, for a
  subscriber with no delivery semantics and no Subscribe/Unsubscribe
  dynamics.

**Recommendation: config-declared.** It consumes DL-053 without amending it
(the brief the ruling set), and the one-fetch-path property PR-C needs holds
identically under both (it hangs on `forge_list_cursors` + the shared pass,
not on how the board's membership is stored). PARKED for Matt; batched by the
main agent.

### OQ-D (LOAD-BEARING — blocks the freeze): whether (and how) the two spec'd DL-053 tables land in this slice

The coordinate alignment itself — SMALLINT provider enum + `forge_host` in
every key, matching the tree's frozen 0013 issue coordinate
(`0013_issues.sql:29-35`) rather than the ownership-layer DDL's `provider
TEXT` + host-less key (`compass-server-ownership-layer/design.md:976-1019`) —
is NOT the fork: it is the mechanical application of the tree's existing
standard, author-decided (Approach → author-decided), because diverging would
fork the coordinate vocabulary inside one schema and force casts/joins between
cursor and issue rows. Two things about it ARE genuine forks:

**D1 — land the two spec'd tables now, or defer them to their first writers.**
Nothing this slice EXECUTES touches `forge_subscriptions` or
`forge_artifact_cursors`: the driver rides `forge_list_cursors` only (Approach
→ the DL-053 tables). So they land now purely as anticipatory schema.

- **Land-now (what this record designs):** one forge migration; the DL-053
  shape is frozen and pgtest-covered (T2 test 7) before its writers exist,
  matching the ruling's "build on the DL-053 machinery from day one" read as
  including the schema. Cost: it forces the D2 provider-domain call now.
- **Defer:** `0015` lands `forge_list_cursors` alone; each spec'd table rides
  the slice that first writes it (PR-C / agent-notification) with more
  information. An EMPTY table is the cheapest thing to `ALTER`, so freezing its
  shape early buys little, and deferring dodges D2 entirely until a writer
  needs it. The driver is byte-identical either way (it never touches those
  tables). Cost: a second forge migration two slices out.

**Recommendation: land-now** — it is the most direct reading of the day-one
ruling and one migration is tidier, but the buy is genuinely small, so this is
a real call for Matt, not an author default dressed as one.

**D2 — IF land-now, which provider domain the CHECK admits (moot if D1 =
defer).** DL-053's spec'd DDL keys `provider TEXT -- "github" | "linear"`
(`compass-server-ownership-layer/design.md:981`) — Linear is a first-class
provider of subscription/cursor rows in the frozen spec. Adopting the 0013
coordinate's `SMALLINT CHECK (forge_provider IN (1, 2, 3))`
(GitHub/GitLab/Forgejo, `0013_issues.sql:33`) consciously NARROWS that domain:
it drops `linear` until the `ForgeProvider` enum and the CHECK grow. This is
NOT "no semantic change" (the pre-fold text's claim, now corrected). DL-129
routes tracker-status ingestion through "the DL-053 poll" (`DECISIONS.md:173`)
and the MVP tracker is GitHub, so the narrowing is likely harmless for now —
but it is a real domain decision, cheap to make correctly while the tables are
empty and expensive to discover wrong once a Linear tracker arrives.

- **Narrow to {github, gitlab, forgejo} (RECOMMENDED):** one coordinate
  vocabulary across `issues` and the forge tables; a Linear tracker later
  extends the enum + CHECK (a one-line migration on empty-or-small tables).
- **Widen the CHECK to include a `linear` enum value now:** honors the DL-053
  spec's stated domain literally, at the cost of introducing an enum value
  with no producer this slice or the next.

PARKED for Matt (both sub-forks); batched by the main agent. If D1 = defer,
D2 does not need ruling now.

### OQ-B (non-load-bearing deferral): live-demo content

Whether the dogfood demo shows LIVE GitHub issues — which would make this
driver demo-critical and re-sequence its priority — is a later Matt scope
call per the SEA-1810 issue body's own escalation trip-wire. Recorded as an
explicitly non-load-bearing deferral: the design is correct regardless of the
ruling; only scheduling changes.
