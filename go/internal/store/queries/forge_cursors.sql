-- Forge repo-subscription / watermark queries (sqlc adoption T6, RIG-3034). These
-- replace the inline SQL literals in internal/store/forge_cursors.go; the
-- hand-written Store methods keep their signatures, the door-side validation
-- (validCoordinate), the ErrNotFound mapping, and the RowsAffected branches
-- (StoreForgeRepoWatermark / SetForgeRepoSubscriptionEnabled are :execrows). The
-- read methods map the generated rows (provider int, nullable swept_updated_at)
-- back to the domain time.Time / ForgeRepoSubscription.

-- name: LoadForgeRepoWatermark :one
SELECT swept_updated_at, list_etag
FROM forge_repo_subscriptions
WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: StoreForgeRepoWatermark :execrows
UPDATE forge_repo_subscriptions
   SET swept_updated_at = $4, list_etag = $5
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: EnsureForgeRepoSubscription :exec
INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo, enabled, merge_queue, merge_queue_branch)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, forge_provider, forge_host, repo) DO NOTHING;

-- name: ListEnabledForgeRepos :many
SELECT repo
FROM forge_repo_subscriptions
WHERE enabled = TRUE
ORDER BY repo ASC;

-- name: IsEnabledForgeRepo :one
SELECT EXISTS (
  SELECT 1 FROM forge_repo_subscriptions
   WHERE repo = $1 AND enabled = TRUE);

-- name: ListEnabledForgeRepoSubscriptions :many
SELECT forge_provider, forge_host, repo, enabled, merge_queue, merge_queue_branch
FROM forge_repo_subscriptions
WHERE forge_provider = $1 AND forge_host = $2 AND enabled = TRUE
ORDER BY repo ASC;

-- name: SetForgeRepoSubscriptionEnabled :execrows
UPDATE forge_repo_subscriptions
   SET enabled = $4
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: LoadPRsBackfilledAt :one
SELECT prs_backfilled_at FROM forge_repo_subscriptions
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: MarkPRsBackfilled :execrows
UPDATE forge_repo_subscriptions SET prs_backfilled_at = $4
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: SetForgeRepoMergeQueue :execrows
UPDATE forge_repo_subscriptions
   SET merge_queue = $4,
       merge_queue_branch = $5
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: ListForgeRepoMergeQueueConfigs :many
SELECT repo, merge_queue, merge_queue_branch
  FROM forge_repo_subscriptions
 WHERE forge_provider = $1 AND forge_host = $2 AND enabled AND merge_queue <> ''
 ORDER BY repo ASC;

-- name: ListOpenPullRequests :many
SELECT number
  FROM pull_requests
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND forge_state = 'open'
 ORDER BY number ASC;

-- name: ClearForgeRepoMergeQueueConfigs :exec
UPDATE forge_repo_subscriptions
   SET merge_queue = '', merge_queue_branch = ''
 WHERE forge_provider = $1 AND forge_host = $2;

-- name: GetForgeRepoMergeQueueConfig :one
SELECT merge_queue, merge_queue_branch
  FROM forge_repo_subscriptions
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;
