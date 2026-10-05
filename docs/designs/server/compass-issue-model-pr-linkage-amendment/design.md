# Compass issue model — amendment: link pull requests to issues

Tracker: RIG-4034

> **Extends `compass-issue-model` (frozen).** That record defines `Issue.prs`
> as "every PR opened for this issue", in discovery order, newest last, but never
> says how a PR is linked or which feed finds it. This amendment settles both.
> All other rules stand: ingestion still never moves canonical `state`,
> `priority` or `assignee`.

Ledger: this PR appends DL-409, DL-410 and DL-411 to `docs/designs/DECISIONS.md`. It supersedes no row.

## Problem / Intent

Nothing fills `Issue.prs` today:

- `CreatePullRequestRequest` (`agent_gateway.proto`) has no issue field, and agent sessions carry no task issue.
- `boardRelevant` (`go/internal/ingest/board_webhook.go`) drops every non-`ISSUE` event.
- `ListUpdatedIssues` drops the PR rows GitHub interleaves into `/issues`.
- `issueToProto` (`go/internal/board/issue_projection.go`) leaves `prs` nil.

Matt ruled option C. Use the issue link the agent gives at create time. Otherwise use GitHub's `closingIssuesReferences`. PRs arrive through the existing board feeds.

## Approach

### 1. The explicit link on the wire

```protobuf
message CreatePullRequestRequest {
  // ... fields 1-6 unchanged ...
  // The issue this PR works on. Unset = no explicit link; the server falls
  // back to the forge's closing references.
  PullRequestIssueLink issue = 7;
}

message PullRequestIssueLink {
  compass.v1.ForgeRef forge = 1;  // unset = the PR's forge
  string repo = 2;                // owner/name; the team key on Linear
  uint64 number = 3;
}
```

A full coordinate lets a GitHub PR name an issue in another repo or on Linear.

**The create path checks the link's shape but never reads the issue.** A tracker
outage must not fail a PR create. `createPullRequest` rejects the call with an
`invalid_argument` `ForgeCallError` in three cases:

- `number` is 0;
- `forge` names a provider that `forgeProviderRegistry.resolve` cannot resolve, which also fills an empty host;
- `repo` is empty while `forge` differs from the PR's forge.

An empty `repo` on the same forge means the PR's repo. GitHub repos are lowercased with the `normalizeBoardRepo` rule, on the PR side and the issue side alike, so the board join cannot miss on case.

The agent tool `create_pull_request` (`packages/compass-agent/src/forge.ts`)
gains an optional `issue: { forge_provider?, forge_host?, repo?, number }`, with
the same names `forgeSelector` uses. The agent role prompt gains one line: when
you open a PR for an issue, pass that issue in `issue`.

### 2. Storage

Two tenant tables go in the next migration. Both follow `0003_token_usage.sql`:

- a `tenant_id` that defaults from `compass.tenant_id` and references `tenants`;
- ENABLE and FORCE RLS with the fail-closed policy;
- the explicit `GRANT … TO compass_app, compass_system`.

```sql
CREATE TABLE pull_requests (
    forge_provider   SMALLINT    NOT NULL CHECK (forge_provider IN (1, 2, 3)),
    forge_host       TEXT        NOT NULL,
    repo             TEXT        NOT NULL,
    number           BIGINT      NOT NULL,
    forge_state      TEXT        NOT NULL,
    forge_created_at TIMESTAMPTZ NOT NULL,  -- prs order key
    forge_updated_at TIMESTAMPTZ NOT NULL,  -- recency guard
    pr               JSONB       NOT NULL,  -- protojson compass.v1.PullRequest
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id        TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
                                 REFERENCES tenants (id),
    PRIMARY KEY (tenant_id, forge_provider, forge_host, repo, number)
);

CREATE TABLE pull_request_issue_links (
    pr_forge_provider    SMALLINT    NOT NULL,
    pr_forge_host        TEXT        NOT NULL,
    pr_repo              TEXT        NOT NULL,
    pr_number            BIGINT      NOT NULL,
    issue_forge_provider SMALLINT    NOT NULL CHECK (issue_forge_provider IN (1, 2, 3, 4)),
    issue_forge_host     TEXT        NOT NULL,
    issue_repo           TEXT        NOT NULL,
    issue_number         BIGINT      NOT NULL,
    source               SMALLINT    NOT NULL CHECK (source IN (1, 2)),  -- 1 explicit, 2 closing_ref
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id            TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
                                     REFERENCES tenants (id),
    PRIMARY KEY (tenant_id, pr_forge_provider, pr_forge_host, pr_repo, pr_number,
                 issue_forge_provider, issue_forge_host, issue_repo, issue_number)
);

CREATE INDEX pull_request_issue_links_issue_idx ON pull_request_issue_links
    (tenant_id, issue_forge_provider, issue_forge_host, issue_repo, issue_number);
```

`pull_requests` joins `updated_at_tables`. The links table has no foreign keys: the
issue may be on Linear or not ingested yet.

**Why links get their own table.** DL-055 makes `forge_authored_artifacts` "an
ownership index, never a mirror of forge content"; human PRs have no authored
row; one PR can close several issues.

**Why the PR is stored whole.** The projection attaches whole `PullRequest`
values with nested reviews and threads. Scalar columns hold only what the store
reads: `forge_updated_at` for the `UpsertIssueForgeFields`-style recency guard
and `forge_created_at` for order. JSONB follows `forge_artifact_cursors.snapshot`.
Under DL-314, `Rehydrate` rebuilds `prs` from Postgres with no forge calls.

Link rows are never garbage-collected. A link whose PR is never ingested, for
example because its repo is not enabled, never renders, because there is no PR
row to attach. These orphans are accepted.

### 3. Precedence

- **An explicit link wins.** At create, after the forge succeeds, `forgeService`
  writes the DL-055 row, the `pull_requests` row (from the `forge.PullRequest`
  the create returned, through `translatePR`) and the explicit link in **one
  transaction**. The same transaction deletes any `closing_ref` rows for that PR,
  because the `opened` webhook can race ahead of it. It then publishes through
  `IssueProjection`. The PR is on its issue at once, not after the webhook.
- **A retried create** (the DL-206 memo hit) rewrites the link idempotently
  through the primary key, so a link lost to a failed first attempt is restored.
- **Otherwise, every `closingIssuesReferences` entry is a link**, and the set is
  recomputed on each hydrate. A reference removed from the PR body is unlinked.
- **Ingestion never removes an explicit link.**
- **A link that cannot resolve falls back when read.** If none of a PR's explicit
  targets is on the board, the projection attaches the PR through its stored
  closing references instead. A typo then costs the explicit link, not the PR's
  place on the board. To make this possible, closing references are stored even
  when an explicit link exists, but with `source = closing_ref` they are used only
  in this case.

GitHub reads closing keywords only on PRs that target the default branch, so in a
stack only the bottom PR gets fallback links.

### 4. Discovery

**Hydrate.** `pullReadQuery` (`go/internal/forge/github_graphql.go`) gains
`closingIssuesReferences(first: 25) { nodes { number repository { nameWithOwner } } }`
on its first page, so the closing references cost no extra round trip.
`forge.PullRequest` gains `CreatedAt`, `UpdatedAt` and `ClosingRefs []IssueRef`,
where `IssueRef` is a new forge-layer `{ Repo string; Number uint64 }`. The cap of
25 is logged when reached.

**Webhook.** `boardRelevant` admits `PULL_REQUEST` events for `OPENED`, `STATE`
and `UPDATE`. `boardCoord` gains a kind, and the PR arm does three things:

1. Gate the repo on `forge_repo_subscriptions`, the same gate issues use.
2. Call `GetPullRequest`.
3. Sink the result through a new `prSink.PublishPullRequestUpdate`.

**Reconciler.** There is no second list walk. `ListUpdatedIssues` already pages
`/issues?state=all&sort=updated` and sees PR rows in the same order under the
same watermark. Instead of dropping them, it returns them as PR coordinates with
their `updated_at`. The sweep hydrates a PR row only when its `updated_at` is
newer than the stored `forge_updated_at`. The watermark rule is unchanged.

**Cold start.** The listing is free, but hydrating every historical PR is not.
With no watermark, the sweep hydrates PR rows that are open or were updated in
the last 30 days, so recent merged PRs reach Done issues.

**Rate cost.** A hydrate costs four paginated REST reads (detail, reviews,
check-runs, status) plus GraphQL pages. The `updated_at` gate stops the sweep
re-hydrating what the webhook handled. PR hydrates share the drain queue and the
`ErrBudgetExhausted` pause with issues.

### 5. Projection

```go
// prSink is the ingest-side seam; it names forge types only (ingest imports no store).
type prSink interface {
    PublishPullRequestUpdate(ctx context.Context, pr *compassv1.PullRequest, closingRefs []forge.IssueRef) error
}
```

`IssueProjection.PublishPullRequestUpdate` does the following:

1. Normalizes the coordinates.
2. Upserts the PR and replaces its `closing_ref` set in one transaction, with the recency guard.
3. Collects the **union of the old and new** linked issues.
4. For each linked issue on the board, loads the ordered `prs` outside the lock.
5. Under `p.mu`, replaces only `Prs` on a clone of the cached issue and publishes it as the existing `issue = 16` event. It never rebuilds the whole issue from a re-read row, so a concurrent state change is not rolled back.

**Every wire build keeps `prs`.** `RecordAndPublish` copies `Prs` from the cached
issue. `PublishIssueUpdate`, `Rehydrate`, `IssueToProto` (the `SetIssueState`
response) and `SearchIssues` load `prs` from the store, and `Rehydrate` and
`SearchIssues` do it in one bulk query.

**Order** is the PR's `forge_created_at`, then provider, host, repo and number.
That is "newest last", it is stable across restart, and it never compares bare
PR numbers across repos, which the parent record forbids.

**Issue not on the board.** The link row waits. When `PublishIssueUpdate` later
upserts that issue, it loads the issue's `prs` by the link index.

**Tenancy.** Like `issues` and `forge_authored_artifacts`, every writer runs
under the bootstrap tenant today: the runner door, the webhook drain and the
reconciler set none. A pgtest asserts that the link, PR and issue share a tenant.
Scoping forge relay calls per tenant must move all three writers together.

## Alternatives considered

- **Columns on `forge_authored_artifacts`**: see §2.
- **An opaque BYTEA blob**: no recency guard, unreadable in SQL.
- **A separate `/pulls` walk and watermark**: `/issues` already returns PR rows in order.
- **Re-fetch PRs on `Rehydrate`**: rate cost on every start, and the board depends on the forge.
- **Tracker-validate the explicit link at create**: an outage would fail PR creation.

## Plan

### Global Constraints

- New tables follow `0003_token_usage.sql`: the tenant default, ENABLE and FORCE RLS, the fail-closed policy, and an explicit GRANT. They go in the next migration file on main at build time, appended after whatever the migration collapse leaves. Never edit an existing migration.
- `updated_at` is maintained only by the `updated_at_tables` trigger.
- Go: no `context.Background()` outside `main` and tests, and no `time.Sleep` in tests. The `ingest` package imports no store.
- GitHub repos are lowercased at every store boundary.

### T1 — Proto, migration and store

- Interfaces:
  - Add `PullRequestIssueLink` and `CreatePullRequestRequest.issue = 7`.
  - `type ForgeCoord struct { Provider ForgeProvider; Host, Repo string; Number uint64 }`.
  - `func (s *Store) CreatePullRequestWithLink(ctx, a AuthoredArtifact, pr PullRequestRow, issue *ForgeCoord) error` writes the DL-055 row, the PR and the explicit link, and deletes `closing_ref` rows, in one transaction.
  - `func (s *Store) UpsertPullRequest(ctx, pr PullRequestRow, closingRefs []ForgeCoord) (affected []ForgeCoord, err error)` returns the union of the old and new linked issues.
  - `func (s *Store) PullRequestsForIssues(ctx, issues []ForgeCoord) (map[ForgeCoord][]PullRequestRow, error)`.
- Tests (pgtest):
  - an explicit link survives an upsert;
  - an unresolvable explicit link falls back;
  - removed closing refs are returned in `affected`;
  - an older `forge_updated_at` is skipped;
  - order follows `forge_created_at`;
  - RLS rejects a foreign tenant;
  - mixed-case repos join.

### T2 — Create path and agent tool

- `createPullRequest` checks the shape of `issue`, then calls `CreatePullRequestWithLink` instead of `record` on the PR arm, and publishes. `forgeStore` and its fake gain the method.
- `forge.ts` gains the `issue` parameter, and the role prompt gains its line.
- Tests:
  - a create with a link shows the PR on the issue at once;
  - each shape error is rejected;
  - a memo hit restores the link.

### T3 — Forge reads

- `pullReadQuery` reads closing references.
- `forge.PullRequest` gains `CreatedAt`, `UpdatedAt` and `ClosingRefs`.
- `ListUpdatedIssues` returns PR rows as `UpdatedRow{Kind, Number, UpdatedAt, State}`.
- Tests (httptest): decoding, a GraphQL error read as an error, and PR rows returned in order.

### T4 — Projection

- Add `PublishPullRequestUpdate`. Load `prs` in `PublishIssueUpdate`, `Rehydrate`, `IssueToProto` and `SearchIssues`. Make `RecordAndPublish` keep `Prs`.
- Tests:
  - a state change keeps `prs`;
  - an issue that lost a closing ref is republished without the PR;
  - a PR that arrives before its issue attaches later;
  - order survives `Rehydrate`.

### T5 — Ingest admission

- `boardRelevant` admits PR events. Add a PR arm to `hydrateAndSink`. The reconciler hydrates PR rows behind the `updated_at` gate and the cold-start window.
- Tests:
  - a PR webhook reaches the projection;
  - an unchanged PR row is not re-hydrated;
  - on cold start, a closed PR older than 30 days is skipped.

Order: T1 first. Then T2, T3 and T4 in parallel. T5 last.

## Tasks

- [ ] T1 — Proto, migration and store
- [ ] T2 — Create-path link and agent tool
- [ ] T3 — Closing refs, PR timestamps and PR rows from the issue walk
- [ ] T4 — Projection attach, fan-out, rehydrate and every wire build
- [ ] T5 — Webhook PR arm and reconciler PR hydrate

## Open Questions

- **Load-bearing (RIG-4604): how fresh must PR review and CI state be on the board?**
  `boardRelevant` drops `REVIEW`, `COMMENT` and `CHECKS` events, and
  `gitHubStateOrUpdateKind` drops `synchronize` and `ready_for_review`. So a PR's
  reviews, checks and draft flag refresh only on a title or state edit, or when a
  sweep sees a newer `updated_at`.
  - **A.** Admit `REVIEW` and `CHECKS` PR events, using the existing per-coordinate coalescing. The board stays fresh at about one hydrate per review or check suite.
  - **B.** Keep `OPENED`, `STATE` and `UPDATE` only, and accept the staleness.
  - Recommendation: **A**, as T6 after T5. Otherwise the stored reviews and checks go stale.
