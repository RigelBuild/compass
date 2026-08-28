# Compass board ingestion: forge poll → webhooks (App-only credential cutover)

Status: Draft
Lane: compass-forge
Tracker: RIG-2883

## Problem / Intent

Matt's decision (2026-08-27, already made — this record documents it, not
re-opens it): *"should we switch the poll over to webhooks? i think yes, it's a
lot better for our API rate limits? let's write that up."* The board-ingestion
forge poll (`ingest.Driver`, DL-161/162/163) is the LAST standing poll loop and
the LAST read-path static-PAT consumer in the tree; this record switches it to
the webhook-driven model DL-264 already proved on the agent-notification lane
(webhook hot path + bounded reconcile backstop), and completes the GitHub
App-only credential cutover Matt directed post-#679.

There are two forge read paths on `main`, and only one still polls:

1. **Agent-notification lane — already webhook-only (DL-264).**
   `go/server/github_webhook.go` mounts-to-be `POST /webhooks/github`
   (`github_webhook.go:24`: `githubWebhookPath = "/webhooks/github"`), the
   DL-254 shape — *"verify signature -> ack 200 fast -> enqueue async"*
   (`github_webhook.go:4-5`) — feeding `ForgeEventSink`
   (`github_webhook.go:47-51`) → `ingest.NotifyRouter.Route`
   (`notify_router.go:150`). Its reliability backstop is a bounded sweep, not
   a poll: `NotifyReconciler` — *"It runs one immediate sweep at startup
   (healing the downtime window) then on a ticker at the Backstop cadence
   (default 30 min)"* (`notify_reconcile.go:4-6`). This lane does not change;
   it is the proven model being extended.

2. **Board-ingestion lane — still a poll. This is what switches.**
   `go/server/serve.go` `buildForgeDriver` (`serve.go:800-841`) assembles
   `ingest.Driver`, whose `Run` — *"performs an immediate pass, then a pass on
   every Interval tick"* (`driver.go:95`, loop at `driver.go:99-112`) — walks
   each enabled repo's issue pages via the `pageLister` seam
   (`driver.go:35-37`: `ListIssuesPage(ctx, repo, f, page, etag)`), sinks
   pages through `Ingester.IngestIssues` (`ingest.go:61-75`), which lands each
   issue on `IssueProjection.PublishIssueUpdate` (`issue_projection.go:68`).
   Targets are `forge_repo_subscriptions` rows read `WHERE enabled` each pass
   (`driver.go:39-41`: *"the driver reads them from PollStore.ListEnabledRepos
   at the top of every pass"*, DL-162). Its credential is the static PAT:
   `tok := newForgeTokenSource(resolver, fc.SecretName)` (`serve.go:830`),
   default secret name `GITHUB_FORGE_TOKEN` (`serve.go:149`).

**The board projection is fed ONLY by the poll today** (verified this run):
the only non-test caller of `PublishIssueUpdate` is the ingest pipeline —
`in.sink.PublishIssueUpdate(ctx, issue)` at `ingest.go:70`; every other match
is a test or the projection's own internals (grep over `go/`). Webhook `issues`
/ `pull_request` events already reach the tree (`ParseGitHubEvent`,
`githubapp_webhook.go:104-132`) but feed only the NotifyRouter — never the
board.

**Why this composes with the App-only cutover.** Matt directed (post-#679):
remove the static-PAT fallback; the GitHub App is the only GitHub credential.
The App credential source is already merged: `NewAppTokenSource` — *"RS256 App
JWT (~10 min) -> POST /app/installations/{id}/access_tokens -> cached until ~5
min before the 1 h expiry"* (`githubapp.go:67-72`). Webhooks require the App
(the App owns the webhook URL + secret, DL-264), and retiring the poll
dissolves the last read-path PAT usage (`serve.go:830`) — one coherent
posture: **GitHub board ingestion becomes webhook-driven and
App-credentialed.**

## Approach

Extend the DL-264 lane shape to the board projection: the one GitHub webhook
ingress fans accepted events out to a second consumer — a board ingest arm —
while a bounded per-repo reconcile sweep (startup + slow ticker, the
`NotifyReconciler` pattern) replaces the standing poll as the reliability and
cold-start/backfill path. The poll driver and its config/flags retire; its
`forge_list_cursors` page-cursor table loses its only writer now and its
definition is dropped from `0001_init.sql` directly (T4; OQ-3 ruled — Compass
is pre-live, the init migration is edited in place); the `forge_repo_subscriptions`
target table survives unchanged as the webhook lane's subscribed-repo set
(DL-162's table-is-authoritative model is transport-independent). The shared
GitHub client moves onto `NewAppTokenSource`, completing the App-only
credential cutover.

Load-bearing pieces, in the vocabulary already on `main`:

- **One ingress, two consumers.** `NewGitHubWebhookHandler` takes a single
  `ForgeEventSink` (`github_webhook.go:110-113`) whose `Enqueue` *"MUST NOT
  block"* (`github_webhook.go:48-50`). The board arm is a second sink behind a
  fan-out: the wiring composes a `fanoutSink` that hands each accepted event to
  every registered sink — the new board ingest queue today, plus the notify
  arm's async enqueue once it exists (`NotifyRouter.Route` is synchronous with
  DB + network I/O, `notify_router.go:150-178`, so it cannot sit behind
  `Enqueue` directly — see OQ-7, resolved: this lane owns both sides). No
  handler changes; the verify/ack/dedup/oversize properties
  (`github_webhook.go:22-42`) are inherited wholesale.
- **Same canonical pipeline, same sink.** The webhook arm MUST run the exact
  poll-path normalization — `translateOne`: `StripOwner` → `TranslateIssue` →
  clone-stamp `ForgeRef` + `Repo` (`ingest.go:82-99`) — before
  `PublishIssueUpdate`, or board attribution breaks. `Ingester.IngestIssues`
  (`ingest.go:61-75`) is transport-agnostic (it takes caller-fetched
  `[]forge.Issue`); the board arm reuses it verbatim.
- **Hydration on event.** The webhook parse today extracts only
  `Number/HTMLURL/State` from an issue payload (`whIssue`,
  `githubapp_webhook.go:52-57`) — not the `Title/Body/State/URL/ForgeAccount/
  Labels` that `TranslateIssue` maps (`translate.go:44-55`). The board arm
  therefore hydrates: on each board-relevant event it conditionally re-reads
  the issue via `GetIssueConditional` (`notify_reader.go:129`) and sinks the
  fresh `forge.Issue`. Ruled (OQ-4): hydrate, with per-coordinate coalescing
  at the T1 drain; the payload-carried alternative is rejected.
- **Reconcile backstop = catch-up primitive.** A `BoardReconciler` sibling of
  `NotifyReconciler` (startup sweep + slow ticker + pacing +
  `ErrBudgetExhausted` abort, `notify_reconcile.go:96-113,115-119`) walks each
  enabled repo's issues **newest-updated-first** with a per-repo updated-at
  watermark, sinking rows through `IngestIssues` until it reaches rows older
  than the watermark. Cold start (zero watermark) degrades to one full walk —
  exactly one poll pass — then the webhook carries the hot path. The existing
  `ListNewArtifacts` (`notify_reader.go:226-231`) is `sinceNumber`-keyed
  (created-order) and misses UPDATES to old issues, so the board needs the
  updated-order variant; it is a small sibling of `ghWalkNewArtifacts`
  (`notify_reader.go:258-262`) keyed on `Issue.UpdatedAt`
  (`provider.go:54-57`), reusing `getJSONCond`'s conditional page-1 ETag.
- **Gate and warn like the notify lane.** The board webhook lane runs iff App
  config is present (W4's hard-off posture, agent-notification design
  `## Resolved decisions`: *"No App configured → NO GitHub notifications; boot
  Warn when subscriptions exist"*). The `warnDisabledForgePolling` idiom
  (`serve.go:1057-1067`) is repurposed: Warn when enabled
  `forge_repo_subscriptions` rows exist but the App lane is off.

**Sequencing (owned here):** the notify lane's server boot wiring
(`buildForgeNotifyLane`, agent-notification design §T7) is NOT yet on `main` —
grep for `buildForgeNotifyLane|forgeNotifyLane|ForgeAppConfig` in `go/server`
returns nothing; `NewGitHubWebhookHandler` is exercised only by tests
(`github_webhook_test.go:49`). This is not a cross-lane risk: this lane
(compass-forge) owns BOTH the board slice and the notify T7, so the shared
ingress is built once by its one owner — see OQ-7 (resolved).

### Alternatives considered

- **Keep the poll as a fallback (hybrid).** Rejected. It contradicts Matt's
  stated rationale (rate-limit budget: a standing 1-min-cadence page walk per
  repo, `serve.go:159` `defaultForgePollInterval = time.Minute`, is exactly
  the spend being eliminated), and it contradicts the W4 precedent — Matt's
  ruling on the notify lane was *"ONE webhook path only — he will not support
  a dual in-server transport"* (agent-notification design §Resolved
  decisions, W4). A dual transport also doubles the invariant surface: two
  writers into `PublishIssueUpdate` with different cursor models
  (`forge_list_cursors` page ETags vs. event-driven) would need cross-path
  ordering reasoning the single-writer model gets for free
  (`issue_projection.go:62-67` documents the single-threaded-ingestion
  assumption). The reconcile backstop already covers every failure class the
  fallback poll would (missed delivery, downtime, cold start) at a bounded,
  tens-of-minutes cadence.
- **Webhook-only, no reconcile backstop.** Rejected. Webhook delivery is
  at-most-once from the server's perspective (downtime, LRU-deduped
  redeliveries, App misconfiguration windows); DL-264 explicitly pairs the
  hot path with *"a bounded reconciliation catch-up"* (`DECISIONS.md:99`).
  The board additionally needs cold-start state a webhook can never supply.
- **Reuse `NotifyReconciler` itself for the board.** Rejected as a merge of
  concerns: it is coordinate-keyed over `NotifyStore.ListNotifyTargets`
  (per-artifact cursors + subscribers, `notify_reconcile.go:6-7`), while the
  board is repo-LIST-keyed with no per-subscriber delivery state (DL-161:
  *"the board holds no per-subscriber DELIVERY cursor"*, `DECISIONS.md:96`).
  The board reconciler reuses the PATTERN (startup sweep + ticker + pacing +
  budget abort) and the `getJSONCond` machinery, not the type.
- **GitHub's native `since=` list parameter (server-side updated-after
  filter).** Rejected. `GET /repos/{repo}/issues?since=` filters
  updated-after on the server, which would remove the client-side stop rule
  entirely and shrink hot-repo payloads — but a since-parameterized URL
  changes every sweep, which kills the stable page-1 URL whose ETag makes
  the steady-state sweep a free 304 (*"a 304 on an authorized request is NOT
  charged"*, `notify_reader.go:12-13`). The client-side walk keeps one
  stable page-1 URL per repo; that trade is why it wins.

## Plan

### Global Constraints

1. **Toolchain:** Go 1.26.6 (`tools/toolchain/versions/go.nix:10`
   `{ version = "1.26.6"; }`); module `github.com/RigelBuild/compass/go` with
   `go 1.25.0` directive (`go/go.mod:15`, the pin-minus-one floor policy,
   `go.mod:10-12`); server-side tests build with `-tags unix`
   (`issue_projection.go:1` `//go:build unix`).
2. **Owner-strip / translate invariant:** every issue reaching
   `PublishIssueUpdate` MUST pass the one pipeline — `StripOwner` →
   `TranslateIssue` → ForgeRef+Repo stamp (`ingest.go:82-99`). No second
   normalization path; the webhook arm reuses `Ingester.IngestIssues`
   (`ingest.go:61-75`), never re-implements it.
3. **App-required-for-webhooks:** the board webhook lane runs iff GitHub App
   config is present (`NewAppTokenSource` preconditions,
   `githubapp.go:72-81`); no App → board ingestion hard-off with a boot Warn
   (the `warnDisabledForgePolling` idiom, `serve.go:1057-1067`). No PAT
   fallback anywhere on the read path (Matt's App-only directive).
4. **Pre-live single init migration:** Compass is pre-live; `0001_init.sql`
   (the only file in `go/internal/store/migrations/`) is edited in place, not
   migrated over — a squashed single init migration is the house convention
   until first production data. Every schema change in this record lands by
   editing `0001_init.sql` (T4). If a parallel PR lands a `0002`, condense
   back into `0001` (Matt's ruling, 2026-08-27).
5. **Ingress invariants inherited, not re-implemented:** signature verify,
   fast-ack, delivery-id LRU dedup, 1 MiB body cap all live in
   `github_webhook.go:22-42,110-125` and are shared with the notify lane.
6. **Non-blocking sink:** any sink handed to the ingress obeys
   `ForgeEventSink.Enqueue` — *"MUST NOT block"* (`github_webhook.go:48-50`).
7. **Idempotency on the coordinate — but NOT monotonicity:** re-sinking an
   issue is safe — `UpsertIssueForgeFields`'s coordinate is the idempotency
   key (`issue_projection.go:69-71`). Idempotency does NOT order concurrent
   writers, though: the projection assumes single-threaded ingestion per
   coordinate (*"a per-coordinate guard would be needed if concurrent
   same-coordinate ingestion is ever introduced"*,
   `issue_projection.go:62-67`), the T1 drain goroutine serializes only the
   WEBHOOK arm — the T3 reconciler is a second concurrent writer — and
   `UpsertIssueForgeFields` is unconditional last-write-wins with no
   `updated_at` comparison (`issues.go:103-134`; the `ON CONFLICT ... DO
   UPDATE SET title = EXCLUDED.title, ...` at `issues.go:122-126` compares
   nothing). This design does NOT hold the single-writer assumption; the
   ruled fix is the OQ-6(a) store-level recency guard, threaded end-to-end
   in T4a (firm).
8. **Repo case normalization at the event boundary:** every GitHub event
   repo passes through `normalizeGitHubRepo` (`serve.go:1091-1101`; export
   an equivalent if needed) before the gate, the hydrate, and the sink.
   `ParseGitHubEvent` sets `Repo` from the case-PRESERVED payload
   `repository.full_name` (`githubapp_webhook.go:74`: `FullName string`
   with tag `json:"full_name"`; `githubapp_webhook.go:113`:
   `Repo: wh.Repository.FullName`), while subscription rows and board
   coordinates
   are LOWERCASED (*"the repo string is lowercased at the seed/upsert
   boundary"*, `0001_init.sql:612-614`). One normalization point, the same
   rule the seed path enforces — a raw event repo would silently miss the
   `IsEnabledRepo` row (per-repo event drop) and, if the gate loosened,
   mint a duplicate board coordinate.

### T1 — board webhook arm: fan-out sink + hydrating ingest queue

A second consumer behind the one GitHub ingress. A `fanoutSink` feeds a new
`BoardEventSink` (and later the notify arm's async enqueue — OQ-7); the board
sink filters to board-relevant GitHub events (`Change ∈ {OPENED, STATE,
UPDATE} ∧ Kind == ISSUE` — PR events dropped in v1, the board projection is
issues-only, `issue_projection.go:17-21`; COMMENT events dropped too:
`issue_comment` also parses to `Kind ISSUE` with `Change COMMENT` for non-PR
carriers (`githubapp_webhook.go:196-210`), the board projects no comments,
and comments are the high-rate human activity — admitting them would burn one
hydrate GET per comment and erode OQ-4's cost bound), enqueues onto a bounded
channel, and a single drain goroutine hydrates each coordinate via a
conditional GET and sinks through the shared `Ingester`.

- Package: `go/internal/ingest` (new file `board_webhook.go`).
- Interfaces:

  ```go
  // issueHydrator is the conditional point-read seam (satisfied structurally
  // by *forge.GitHub via GetIssueConditional, notify_reader.go:129).
  type issueHydrator interface {
      GetIssueConditional(ctx context.Context, repo string, number uint64, etag string) (forge.ConditionalResult[forge.Issue], error)
  }

  // BoardWebhookArm consumes board-relevant forge events from the webhook
  // ingress and sinks hydrated issues through the shared Ingester pipeline.
  type BoardWebhookArm struct { /* queue, hydrator, *Ingester, targets, log */ }

  func NewBoardWebhookArm(h issueHydrator, ing *Ingester, targets TargetChecker, cfg BoardArmConfig) *BoardWebhookArm
  // Enqueue satisfies server.ForgeEventSink's contract (non-blocking: a full
  // queue drops the event with a counter+Warn — the reconciler heals it).
  func (a *BoardWebhookArm) Enqueue(ctx context.Context, ev forge.ForgeEvent)
  // Run drains the queue until ctx cancel; returns nil on cancel (driver.go:95-99 idiom).
  func (a *BoardWebhookArm) Run(ctx context.Context) error

  // TargetChecker gates events to subscribed repos (forge_repo_subscriptions
  // WHERE enabled) — the DL-162 target model, point-checked per event.
  type TargetChecker interface {
      IsEnabledRepo(ctx context.Context, repo string) (bool, error)
  }
  ```

- Behavior: at `Enqueue`, filter `Change ∈ {OPENED, STATE, UPDATE} ∧ Kind ==
  ISSUE` + channel try-send — nothing else (the non-blocking contract). On
  drain, COALESCE per coordinate first (OQ-4's batching lever): drain every
  queued event for the same `(repo, number)` before hydrating, so an edit
  storm of N rapid events on one issue costs ONE GET, not N — the event only
  proves "changed", so one fresh read serves the whole burst. Then normalize
  the event repo (constraint 8) → `IsEnabledRepo` gate →
  `GetIssueConditional(repo, number, "")` (unconditional: the event proves
  change; no stored per-issue ETag in v1) → on success `IngestIssues(ctx,
  repo, []forge.Issue{res.V})`. Per-event errors log-and-continue
  (`driver.go:96-98` idiom); `ErrBudgetExhausted` pauses the drain until the
  gate reopens.
- Parse-table coupling (documented, not changed): the filter consumes
  `ParseGitHubEvent`'s output, so the board lane inherits the NOTIFY lane's
  action table by construction — `gitHubStateOrUpdateKind` maps
  opened/closed/reopened/edited/labeled/unlabeled and drops everything else
  (`githubapp_webhook.go:174-185`). A future notify-motivated change to that
  table silently changes board coverage; any edit there now has two
  consumers.
- Observability: a sustained queue-full (e.g. the drain paused on
  `ErrBudgetExhausted` while events keep arriving) silently degrades the hot
  path to a de-facto 30-min poll (the reconciler heal ceiling). Export a
  queue-full/drop metric with an alerting threshold — a counter+Warn alone
  is not enough to notice the degradation.
- Test cycle (red first): fake hydrator + recording sink (the
  `recordingSink` pattern, `ingest_test.go:13-24`): event → hydrated issue
  reaches `PublishIssueUpdate` with stripped body + stamped ForgeRef;
  non-enabled repo dropped; PR-kind event dropped; COMMENT-change event
  dropped (no hydrate GET spent); mixed-case `full_name` normalized before
  gate/sink — the lowercased subscription row matches and no duplicate
  coordinate is minted; full queue drops without blocking Enqueue; hydrate
  error retries next event, never crashes; a burst of N events on one
  coordinate coalesces to one hydrate GET (the coalesce lever observable at
  the fake hydrator's call count).

### T2 — updated-order conditional list: `ListUpdatedIssues`

The reconcile/backfill read primitive. `ListNewArtifacts` is created-order and
`sinceNumber`-keyed (`notify_reader.go:226-231`) so it cannot see updates to
existing issues; the board needs an updated-order walk.

- Package: `go/internal/forge` (extend the GitHub arm beside
  `ghWalkNewArtifacts`).
- Interfaces:

  ```go
  // ListUpdatedIssues walks /repos/{repo}/issues?state=all&sort=updated&
  // direction=desc (page 1 conditioned on etag; 304 => NotModified),
  // collecting issue rows (PR rows dropped by the pull_request marker,
  // notify_reader.go:238-243) until a page's oldest updated_at is strictly
  // < since, or no rel="next" remains. Rows with updated_at == since are
  // RE-fetched: GitHub updated_at is second-granularity, so a <= stop would
  // permanently exclude an issue updated in the same second as the stored
  // watermark after the sweep read it; the duplicates are free by
  // coordinate idempotency. A zero since walks ALL pages (cold start).
  func (g *GitHub) ListUpdatedIssues(ctx context.Context, repo string, since time.Time, etag string) (ConditionalResult[[]Issue], error)
  ```

- `Issue.UpdatedAt` is already parsed off the list row (`provider.go:54-57`);
  reuses `getJSONCond` (`notify_reader.go:84`) for the conditional page-1 +
  Link-chain walk.
- Test cycle (red first): httptest fixtures — multi-page walk stops strictly
  below `since` (a row with `updated_at == since` is re-included); page-1
  304 short-circuits; zero `since` walks to the last
  page; PR rows filtered; ETag returned for re-store; budget error propagates.

### T3 — board reconciler: startup sweep + slow ticker + watermark

The reliability backstop and the cold-start/backfill path — the
`NotifyReconciler` PATTERN (`notify_reconcile.go:3-15`) at repo-LIST
granularity.

- Package: `go/internal/ingest` (new file `board_reconcile.go`).
- Interfaces:

  ```go
  // BoardStore is the durable target + watermark seam (server adapts
  // *store.Store, the forgePollStore pattern).
  type BoardStore interface {
      ListEnabledRepos(ctx context.Context) ([]string, error)
      // LoadRepoWatermark returns the repo's updated-at watermark + list ETag
      // (zero values when never swept).
      LoadRepoWatermark(ctx context.Context, repo string) (time.Time, string, error)
      // StoreRepoWatermark persists watermark + ETag AFTER the repo's rows sank
      // (advance-after-sink, the DL-161 idempotency invariant).
      StoreRepoWatermark(ctx context.Context, repo string, mark time.Time, etag string) error
  }

  type updatedLister interface {
      ListUpdatedIssues(ctx context.Context, repo string, since time.Time, etag string) (forge.ConditionalResult[[]forge.Issue], error)
  }

  type BoardReconcileConfig struct {
      Backstop time.Duration // default 30 * time.Minute (defaultBackstop, notify_reconcile.go:33)
      Pace     time.Duration // default 200ms inter-repo (defaultPace, notify_reconcile.go:40); <0 disables
      Log      *slog.Logger
  }

  func NewBoardReconciler(l updatedLister, ing *Ingester, st BoardStore, cfg BoardReconcileConfig) *BoardReconciler
  // Run: one immediate sweep, then a sweep per tick, nil on ctx cancel
  // (notify_reconcile.go:96-99 idiom). Per-repo errors isolated;
  // ErrBudgetExhausted aborts the sweep, resumed next tick.
  func (rc *BoardReconciler) Run(ctx context.Context) error
  ```

- Sweep semantics: per enabled repo, `ListUpdatedIssues(repo, watermark,
  etag)`; NotModified → done (ETag carried); else sink rows through
  `IngestIssues`, then store `max(UpdatedAt)` + fresh ETag. Zero/absent
  watermark (fresh deployment, an App REINSTALL, or a newly enabled repo) =
  one full walk — the cold-start AND reinstall-backfill answer. This is the
  industry-standard webhook-App pattern (REST-list backfill on install +
  webhooks to stay in sync; GitHub's own best practice is to subscribe to
  webhook events instead of polling): a reinstall's absent watermark row
  auto-triggers the full re-walk on the next sweep, so NO separate backfill
  mechanism exists or is needed (OQ-1, Matt's follow-up, answered there).
- Clock skew is a non-issue: the watermark compares GitHub's own
  `updated_at` against GitHub's own `updated_at` (`Issue.UpdatedAt` is
  *"parsed from the LIST row's updated_at"*, `provider.go:54-56`) — no
  server clock ever enters the comparison.
- Poisoned-row livelock (bound it): whole-repo advance-after-sink plus
  `IngestIssues`' stop-on-first-error (*"It stops and returns the first sink
  error"*, `ingest.go:64-66`) means one persistently-rejected row (e.g. a
  store validation error) pins the watermark, and every sweep re-walks a
  monotonically growing window — a quiet livelock with no error surfaced.
  Isolate per-row sink errors inside the sweep (skip-and-count the poison
  row, sink the rest, advance the watermark past the healthy rows) or
  equivalent — the re-walk window MUST NOT grow without bound on one bad
  row.
- Test cycle (red first): `synctest`-driven (the
  `notify_reconcile_test.go:387-390` pattern): startup sweep fires
  immediately; ticker sweeps at Backstop; watermark advances only after sink
  success (sink failure → same watermark → rows re-listed next sweep); 304
  costs no sink; a persistently-failing row does NOT pin the watermark
  forever (poison-row isolation — the re-walk window stays bounded); budget
  abort resumes next tick; ctx cancel returns nil.

### T4 — store: watermark persistence + `forge_list_cursors` retirement

- Package: `go/internal/store` (schema edited in `0001_init.sql`).
- Schema (EDIT `0001_init.sql` IN PLACE — constraint 4, the pre-live
  convention; Matt: *"0001. we aren't live yet for compass, can just edit
  0001."* The prior 0002/0003 split and its rolling-deploy-crash rationale
  are retired — moot with no live deployment):
  - DROP the `forge_list_cursors` table definition outright
    (`0001_init.sql:675`) — dead once the poll retires; its only writer is
    the driver's page-cursor path.
  - ADD `swept_updated_at TIMESTAMPTZ` and `list_etag TEXT NOT NULL DEFAULT
    ''` columns to the `forge_repo_subscriptions` table definition
    (`0001_init.sql:616`).
  - ADD the `forge_updated_at TIMESTAMPTZ` column to the `issues` table
    definition (`0001_init.sql:571`) — the OQ-6(a) recency-guard column, now
    UNCONDITIONAL. The column is INERT until the write path populates it:
    the bare column plus a `>=` guard is a silent no-op, because
    `Issue.UpdatedAt` reaches no writer today — the canonical proto `Issue`
    has no `updated_at` slot (`compass.proto:825-854`, fields 1-18),
    `TranslateIssue` does not map it (`translate.go:44-55`),
    `protoToForgeFields` does not carry it (`issue_projection.go:208-224`),
    `IssueForgeFields` has no such field (`issues.go:78-90`), and
    `UpsertIssueForgeFields`'s INSERT column list never sets it
    (`issues.go:118-121`). So the column and the T4a threading land
    together — T4a is a firm task, not a contingency.
- **T4a — recency-guard write-path threading (FIRM — OQ-6 ruled (a)).** The
  full chain the guard needs, or it never fires: (1) add `updated_at` to the
  canonical proto `Issue` (next free field number) + regen Go/TS; (2) map it
  in `TranslateIssue` (`translate.go:44-55`) from `forge.Issue.UpdatedAt`
  (`provider.go:54-57`); (3) carry it in `protoToForgeFields`
  (`issue_projection.go:208-224`) and add it to `IssueForgeFields`
  (`issues.go:78-90`); (4) include it in `UpsertIssueForgeFields`'s INSERT
  column list (`issues.go:118-121`); (5) make the `ON CONFLICT DO UPDATE`
  conditional — `WHERE issues.forge_updated_at IS NULL OR
  EXCLUDED.forge_updated_at IS NULL OR EXCLUDED.forge_updated_at >=
  issues.forge_updated_at` (the NULL arms let rows written before the
  threading and any not-yet-threaded writer still update, so the guard is
  additive, never a regression). Test cycle: two interleaved sinks of one
  coordinate (stale-then-fresh AND fresh-then-stale) — the fresher
  `updated_at` wins both orders; a NULL-either-side write still applies.
- Re-enable semantics (correct as-is, stated to save the derivation): a repo
  disabled then re-enabled keeps its stale watermark — the next sweep walks
  from that watermark and back-fills the disabled-window gap. No reset
  needed.
- Interfaces:

  ```go
  // On *store.Store (forge_cursors.go, replacing the three page-cursor methods
  // the retired driver consumed via ingest.PollStore, driver.go:19-31):
  func (s *Store) LoadForgeRepoWatermark(ctx context.Context, provider ForgeProvider, host, repo string) (time.Time, string, error)
  func (s *Store) StoreForgeRepoWatermark(ctx context.Context, provider ForgeProvider, host, repo string, mark time.Time, etag string) error
  // ListEnabledForgeRepos + IsEnabledForgeRepo (point variant) survive/are added
  // on the existing forge_repo_subscriptions surface (forge_cursors.go:8-12).
  ```

- Deletes: the `forge_list_cursors` read/upsert/prune store methods and their
  pgtest coverage (`forge_cursors_pgtest_test.go:5-9`).
- Test cycle: pgtest — watermark round-trip, zero-value on never-swept,
  coordinate isolation across (provider, host); the edited `0001_init.sql`
  applies cleanly on a fresh database (no `forge_list_cursors`; the three
  new columns present).

### T5 — serve wiring: retire the poll, mount the board arm, App-only credential

- Package: `go/server` (+ `go/cmd/compass-server`).
- Retire: `buildForgeDriver` (`serve.go:800-841`), `ingest.Driver` +
  `PollStore` + `pageLister` + `DriverConfig` (`driver.go` entire),
  `ForgeConfig.Poll`/`PollInterval` (`serve.go:130,143`),
  `forgePollingEnabled` (`serve.go:170-173`), flags `--forge-poll` /
  `--forge-poll-interval` + `$COMPASS_FORGE_POLL(_INTERVAL)`
  (`main.go:358-365`), and `newForgeTokenSource(resolver, fc.SecretName)`
  for the read path (`serve.go:830`). `--forge-repos` and
  `--forge-host` survive (the seed + host model is transport-independent,
  DL-162; seed reconcile `serve.go:820-825` unchanged).
- Add:

  ```go
  // buildBoardIngestLane assembles: App TokenSource (NewAppTokenSource,
  // githubapp.go:72) -> shared forge.GitHub client -> Ingester (existing
  // ctor, serve.go:832) -> BoardWebhookArm (T1) + BoardReconciler (T3) +
  // store adapters (T4). Returns the two Runs for the serve errgroup and the
  // board sink to compose into the ingress fan-out.
  func buildBoardIngestLane(ctx context.Context, cfg ServeConfig, st *store.Store, issueBrd *board.IssueProjection, resolver secrets.Resolver, log *slog.Logger) (*boardIngestLane, error)

  // fanoutSink fans one accepted webhook event to every registered sink
  // (notify enqueue + board enqueue); satisfies server.ForgeEventSink.
  type fanoutSink struct{ sinks []ForgeEventSink }
  func (f *fanoutSink) Enqueue(ctx context.Context, ev forge.ForgeEvent)
  ```

- Gate: lane built iff App config present (constraint 3); otherwise boot Warn
  when enabled `forge_repo_subscriptions` rows exist (repurposed
  `warnDisabledForgePolling`, `serve.go:1057-1067`, renamed
  `warnDisabledBoardIngestion`).
- Ownership (OQ-7 ruled: single shared mount, THIS lane owns it): nothing
  exists yet to compose with — no `buildForgeNotifyLane` / `ForgeAppConfig`
  / notify enqueue is on `main` (verified: grep in `go/server` returns only
  tests), and there is no "NotifyRouter enqueue" to fan into —
  `NotifyRouter.Route` is SYNCHRONOUS with DB + network I/O
  (`notify_router.go:150-178`: cursor load, checks roll-up fetch), so it
  cannot sit behind `ForgeEventSink.Enqueue`'s MUST-NOT-BLOCK contract; the
  async wrapper is the notify design's unbuilt T7 — owned by THIS SAME lane
  (compass-forge owns both the board slice and the notify T7). The shared
  GitHub ingress — the `POST /webhooks/github` mount, the `ForgeAppConfig`
  block, and the `fanoutSink` type + its registration seam — is built ONCE,
  owned by this lane, and both consumers register on it. Sequencing is one
  owner ordering its own two slices, not a cross-lane contract: whichever of
  {this board slice, the notify T7} lands its wiring first builds the mount +
  fanout (board-first mounts `fanoutSink{board}`, a single-element
  fan-out), and the other composes in through the one-line registration
  seam.
- Test cycle: server pgtest (`-tags unix`) — signed fake `issues` webhook
  through the REAL ingress (the `forge_webhook_fakes_test.go:8-11` pattern) →
  board projection observes the hydrated issue on its bus; App-config-absent
  boot leaves the lane off + Warns with enabled rows; flag-removal compile
  coverage in `main_forge_test.go`.

### T6 — docs + runbook delta

- Update the poll-driver record's status header (superseded-by pointer), the
  App setup runbook (board events now require the `issues` webhook
  subscription on the App — `ParseGitHubEvent` already routes the `issues`
  event type, `githubapp_webhook.go:116-118`), and flag/env documentation for
  the removed `--forge-poll` / `--forge-poll-interval`.
- Interfaces: none (docs only).
- Test cycle: none (docs); link-check if the docs CI has one.

## Tasks

- [ ] T1 — board webhook arm: `fanoutSink`-fed hydrating ingest queue in
      `go/internal/ingest`, reusing `Ingester.IngestIssues`
- [ ] T2 — `(*forge.GitHub).ListUpdatedIssues` updated-order conditional walk
- [ ] T3 — `BoardReconciler`: startup sweep + 30-min ticker + watermark
      advance-after-sink
- [ ] T4 — store: edit `0001_init.sql` in place (pre-live) — DROP
      `forge_list_cursors`, ADD `swept_updated_at` + `list_etag` to
      `forge_repo_subscriptions`, ADD `issues.forge_updated_at`; watermark
      store methods replace the page-cursor methods
- [ ] T4a — recency-guard write-path threading (proto `updated_at` + regen →
      `TranslateIssue` → `protoToForgeFields`/`IssueForgeFields` → conditional
      `UpsertIssueForgeFields`) — FIRM (OQ-6 ruled (a))
- [ ] T5 — serve wiring: retire `ingest.Driver` + poll flags + read-path PAT;
      mount board arm behind the shared ingress; App-only credential
- [ ] T6 — docs + runbook delta (superseded poll record, App webhook events,
      removed flags)

## Resolved decisions (Matt, 2026-08-27)

Matt ruled all seven forks at the freeze gate (PR #695). Each entry leads
with the decision; rejected options survive as a short tail for future
readers.

- **OQ-1 — the poll driver RETIRES.** `ingest.Driver` is deleted entirely
  (T5). The standing loop is exactly the rate spend Matt is eliminating; the
  reconciler's zero-watermark sweep IS a full resync (T3), reachable
  operationally by clearing a repo's watermark row — an escape hatch with no
  second transport, consistent with W4's "ONE webhook path only"; and
  dead-but-compiled poll code invites drift against the single-writer
  assumption (`issue_projection.go:62-67`). **Matt's follow-up ("for initial
  setup of Compass, if you reinstall the App etc how do we backfill
  issues/prs etc? … what do other apps that use webhooks with github/linear
  do?"), answered:** the industry-standard pattern for a webhook
  GitHub/Linear App is REST-list backfill on install + webhooks to stay in
  sync (GitHub's own best-practice guidance: subscribe to webhook events
  instead of polling to stay within the API rate limit; integration guides
  paginate existing issues/PRs with the installation token on install).
  This design ALREADY implements exactly that: the T3 zero-watermark sweep
  IS the backfill — a fresh install, a REINSTALL, or a newly enabled repo
  has no watermark row, so the first sweep walks the repo's full
  updated-order issue list once (identical cost to one legacy poll pass),
  seeds the board, and webhooks carry the hot path from there. Reinstall
  backfill is therefore automatic — the reinstall's zero/absent watermark
  triggers a full re-walk on the next sweep; no separate backfill mechanism
  exists or is needed. Known limitation (pre-existing, unchanged): NO
  transport in this design removes a board row — GitHub
  `deleted`/`transferred` actions are dropped by the parse table
  (`gitHubStateOrUpdateKind` maps only opened/closed/reopened/edited/
  labeled/unlabeled, `githubapp_webhook.go:174-185`) and a deleted issue
  vanishes from every list, so a deleted/transferred issue — including one
  deleted during a downtime window — persists on the board until manually
  removed. The poll had this gap identically (upsert-only sink). (Rejected:
  keep the driver compiled as an opt-in emergency full-resync path.)
- **OQ-2 — the T3 watermark sweep IS the cold-start / downtime / backfill
  primitive.** Decided along the recommendation, reinforced by OQ-1's
  ruling: startup sweep + 30-min ticker, `ListUpdatedIssues(repo, watermark,
  etag)`; zero watermark = one full walk. Cost bound: steady-state is one
  conditional page-1 GET per enabled repo per 30 min (304 ≈ free — *"a 304
  on an authorized request is NOT charged"*, `notify_reader.go:12-13`); cold
  start is one full list walk per repo, identical to a single legacy poll
  pass. (Rejected: `ListNewArtifacts` alone — it is created-order
  (`sinceNumber`) and misses updates to existing issues
  (`notify_reader.go:226-231`), which the board must observe.)
- **OQ-3 — `forge_list_cursors` is dropped by EDITING `0001_init.sql`
  directly** (DL-163). Matt: *"0001. we aren't live yet for compass, can
  just edit 0001. if some PR made a 0002 can condense again."* Compass is
  pre-live and `0001_init.sql` is the ONLY migration in
  `go/internal/store/migrations/`, so every schema change in this record —
  the `forge_list_cursors` DROP and the three new columns — lands by
  editing `0001_init.sql` in place (T4, constraint 4). This OVERRIDES the
  record's prior split-0002/0003 recommendation; the rolling-deploy-crash
  rationale for the split is moot with no live deployment. (Rejected:
  additive `0002` + post-soak `0003` DROP; DROP bundled into a new `0002`.)
- **OQ-4 — HYDRATE, with batching.** One conditional GET per accepted board
  event (T1's `GetIssueConditional`). Rationale: (a) no widening of
  `ForgeEvent`, which the notify lane co-owns; (b) the GET returns coherent,
  current state even for stale/re-ordered deliveries, where trusting a
  payload snapshot can regress a newer title/state; (c) cost is bounded by
  real human edit rate on subscribed repos — orders of magnitude below the
  retired 1-min page walk. **Matt's follow-up ("can we batch together to
  minimize api rate limit cost?"), answered — YES, two levers, both in the
  design:** (1) the T1 drain COALESCES per coordinate: N rapid events on
  one issue (edit storms, label churn) collapse to ONE hydrate GET, not N;
  (2) the T3 reconcile sweep batches by construction — it hydrates via one
  updated-order LIST call per repo (`ListUpdatedIssues`, T2), never
  per-issue GETs, so the backstop path is inherently batched. Honest limit:
  the per-event hot-path hydrate stays one conditional GET per DISTINCT
  coordinate — GitHub REST has no issue-batch-GET endpoint, and the GitHub
  arm is REST throughout (`getJSONCond`, `notify_reader.go:84`; its only
  GraphQL reference is the deliberately-unbuilt review-thread read,
  `github.go:577`). If per-coordinate coalescing proves insufficient under
  real load, a GraphQL multi-issue batch fetch is a documented FUTURE
  optimization, not v1. Cost caveat (unchanged): the T1 hydrate
  `GetIssueConditional(repo, number, "")` is a CHARGED, unconditional read
  every time (fine at human edit rate); OQ-2's "304 ≈ free" framing applies
  to the reconcile sweep's page-1 conditional GET only, NOT to this hydrate
  — the two must not be conflated. (Rejected: widen `whIssue` + `ForgeEvent`
  to carry the full payload — saves the GET but couples the two lanes'
  event currency and imports the ordering hazard.)
- **OQ-5 — poll config RETIRES; `ReconcileBackstop` added.** Decided along
  the recommendation, consistent with OQ-1's retire: retire `Poll` +
  `PollInterval` (+ flags `--forge-poll`, `--forge-poll-interval`, envs
  `$COMPASS_FORGE_POLL(_INTERVAL)`, `main.go:358-365`); keep `Host` and
  `SeedRepos`/`--forge-repos` (the DL-162 seed model survives — a webhook
  lane still needs the subscribed-repo set), and
  `ReviewerSecretName`/`SecretName` only as far as the WRITE path still
  consumes them (the agent forge-write gate, `serve.go:134-141`, is
  explicitly out of scope here; the App-only cutover for WRITES is the
  notify lane's W1 posture and its own slice). Add `ReconcileBackstop
  time.Duration` (default 30 min).
- **OQ-6 — option (a): store-level recency guard. T4a is FIRM.** Carry
  `Issue.UpdatedAt` into `IssueForgeFields` and make the `ON CONFLICT DO
  UPDATE` conditional on `EXCLUDED.forge_updated_at >=
  issues.forge_updated_at` — the DL-129 recency-guard precedent already in
  the tree (`provider.go:55-56` cites it for PR-C). Writer-count-agnostic:
  it handles interleaving at the store, has no cold-start queue-flood, and
  future-proofs any later writer — the minimal correct answer to the
  violated single-writer assumption (the T1 drain and the T3 reconciler are
  two concurrent writers into `PublishIssueUpdate`,
  `issue_projection.go:62-67`, and `UpsertIssueForgeFields` compares
  nothing, `issues.go:122-126` — a sweep carrying a stale list-row snapshot
  could otherwise overwrite a fresher webhook-hydrated write for up to 30
  min). The guard fires only if `Issue.UpdatedAt` is threaded end-to-end —
  the full T4a chain (proto `updated_at` + regen → `TranslateIssue` →
  `protoToForgeFields`/`IssueForgeFields` → conditional
  `UpsertIssueForgeFields`); a bare `forge_updated_at` column with no
  writer is a silent NULL no-op, so the column (T4) and the threading (T4a)
  land together. (Rejected: (b) funnel the reconciler through the T1 drain
  queue — restores single-writer with no store change, but a zero-watermark
  cold-start full walk floods the bounded queue; (c) per-coordinate mutex
  in `IssueProjection` — serializes commit+record but does NOT fix
  stale-fetch-wins, insufficient alone.)
- **OQ-7 — SINGLE SHARED MOUNT, owned by THIS lane.** Matt: *"shared, you
  are owning both sides anyway?"* — correct: compass-forge owns both the
  board slice AND the notify lane's T7. The shared GitHub ingress — the
  `POST /webhooks/github` mount, the `ForgeAppConfig` block, and the
  `fanoutSink` that fans accepted events to the board arm and the notify
  arm — is built ONCE, owned by this lane, and both consumers register on
  it. The notify arm's async enqueue wrapper is still unbuilt T7 work
  (`NotifyRouter.Route` is synchronous with DB + network I/O,
  `notify_router.go:150-178`), so whichever of the lane's own two slices
  {this board slice, the notify T7} lands its wiring first builds the mount +
  fanout and the other composes in — one owner sequencing its own two
  slices, NOT a cross-lane landing-order contract. (Rejected: the
  landing-order contract between two independent lanes — moot with one
  owner; extracting the mount into a slice-zero task both lanes depend on —
  coordination weight with no remaining race to remove.)

## Ledger-impact

Proposed delta for `docs/designs/DECISIONS.md` (executed by the spawning
agent, NOT this record):

- **DL-053 (`DECISIONS.md:81`) — AMEND, transport clause only.** The row's
  *"change-detected by conditional polling in v1 (webhooks are an additive
  accelerator)"* clause is inverted for BOTH lanes now: webhooks are the
  primary transport; conditional reads survive only inside bounded reconcile
  sweeps. The FETCH/DELIVERY cursor split, Postgres subscription rows, and
  the delivery path are unchanged — amend, do not supersede.
- **DL-161 (`DECISIONS.md:96`) — SUPERSEDED.** The board-ingestion poll
  driver and its repo-LIST page-cursor model retire; replaced by the webhook
  arm + watermark reconciler (new DL below).
- **DL-162 (`DECISIONS.md:97`) — AMEND.** The `forge_repo_subscriptions`
  table-as-target model and `--forge-repos` seed survive verbatim as the
  webhook lane's subscribed-repo set; the `--forge-poll` flag clause retires.
- **DL-163 (`DECISIONS.md:98`) — AMEND.** `forge_list_cursors` loses its
  only writer and is dropped by editing `0001_init.sql` in place (OQ-3
  ruled: Compass is pre-live; the init migration is edited directly, never
  post-soak-migrated). The other three tables are unchanged.
- **DL-264 (`DECISIONS.md:99`) — UNCHANGED, cited as precedent.** Its
  webhook-only + bounded-reconcile model is extended to the board lane.
- **NEW DL — landed as DL-279 (`DECISIONS.md:103`); its ledger row's
  "`forge_list_cursors` dropped post-soak" clause needs the OQ-3 wording fix
  (dropped by editing `0001_init.sql`, pre-live) — coordinator-owned.
  Proposed statement:** "Board ingestion is WEBHOOK-DRIVEN: the
  GitHub App webhook ingress (DL-264's `POST /webhooks/github`) fans accepted
  `issues` events to a board ingest arm that hydrates each coordinate via a
  conditional GET and sinks through the one StripOwner→TranslateIssue→stamp
  pipeline into `IssueProjection.PublishIssueUpdate`; reliability and
  cold-start are a bounded per-repo updated-order reconcile sweep (startup +
  30-min ticker, per-repo updated-at watermark advanced only after sink) —
  no standing poll loop exists anywhere, and the GitHub App is the ONLY
  GitHub read credential (the static read-path PAT is retired). Known
  limitation, pre-existing from the poll era: no transport removes a board
  row — `deleted`/`transferred` actions are dropped at parse and a deleted
  issue vanishes from every list, so a forge-deleted issue persists on the
  board until manually removed."
