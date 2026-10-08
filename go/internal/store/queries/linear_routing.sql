-- Linear routing channel queries: the admin's reserved __linear__ group and the one
-- routing channel inside it, keyed by (group, name) so a planted look-alike is never adopted.

-- name: GetLinearRoutingGroup :one
-- Visibility-discriminated like GetOwnerDMGroup: a planted wider __linear__ group is never adopted.
SELECT id FROM channel_groups
WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3
ORDER BY id
LIMIT 1;

-- name: InsertLinearRoutingGroup :exec
INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility, namespace_owner_id)
VALUES ($1, $2, NULL, $3, $4,
        COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $3), $3));

-- name: GetLinearRoutingChannel :one
SELECT id, kind FROM channels WHERE group_id = $1 AND name = $2;

-- name: InsertLinearRoutingChannel :one
-- ON CONFLICT DO NOTHING keeps a lost race from poisoning the tx; the caller re-selects.
INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription)
VALUES ($1, $2, $3, $4, $5, NULL, $6)
ON CONFLICT (group_id, name) WHERE group_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: LockLinearRouting :exec
SELECT pg_advisory_xact_lock(hashtext('linear-routing:' || $1));

-- name: ReassertLinearRoutingShape :exec
-- The bridge posts as a non-owner, so owner-only or owned drift would refuse every cold delegation.
UPDATE channels SET mandatory_subscription = TRUE, post_policy = $2, owner_account_id = NULL
WHERE id = $1 AND (mandatory_subscription = FALSE OR post_policy <> $2 OR owner_account_id IS NOT NULL);
