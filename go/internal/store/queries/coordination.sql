-- Coordination-store queries (sqlc adoption T3, RIG-3034). These replace the
-- inline SQL literals in internal/store/coordination.go; the hand-written Store
-- methods keep their signatures, the per-owner advisory-lock discipline, the
-- suffix-search resolution loop, and the WithTx seam (which stays hand-written).
-- The member INSERT/DELETE reuse EnsureChannelMember (accounts.sql) and
-- DeleteChannelMember (channels.sql) — the statements are identical.

-- name: GetCoordinationGroup :one
SELECT id FROM channel_groups
WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3;

-- name: InsertCoordinationGroup :exec
INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility)
VALUES ($1, $2, NULL, $3, $4);

-- name: GetCoordinationChannelByName :one
SELECT id, COALESCE(owner_account_id, '') AS owner_account_id
FROM channels WHERE group_id = $1 AND name = $2;

-- name: InsertCoordinationChannel :one
INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)
ON CONFLICT (group_id, name) WHERE group_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: ChannelMemberIDs :many
SELECT account_id FROM channel_members WHERE channel_id = $1;

-- name: CoordinationReports :many
SELECT account_id FROM agent_accounts WHERE parent_agent_id = $1 ORDER BY account_id;

-- name: ResolveCoordinationManager :one
SELECT a.handle, ag.owner_user_id
FROM accounts a
JOIN agent_accounts ag ON ag.account_id = a.id
WHERE a.id = $1;

-- name: LockOwnerCoordination :exec
SELECT pg_advisory_xact_lock(hashtext('coordination:' || $1));
