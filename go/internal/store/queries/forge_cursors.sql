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
   SET swept_updated_at = $4, list_etag = $5, updated_at = now()
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;

-- name: EnsureForgeRepoSubscription :exec
INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (forge_provider, forge_host, repo) DO NOTHING;

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
SELECT forge_provider, forge_host, repo, enabled
FROM forge_repo_subscriptions
WHERE forge_provider = $1 AND forge_host = $2 AND enabled = TRUE
ORDER BY repo ASC;

-- name: SetForgeRepoSubscriptionEnabled :execrows
UPDATE forge_repo_subscriptions
   SET enabled = $4, updated_at = now()
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3;
