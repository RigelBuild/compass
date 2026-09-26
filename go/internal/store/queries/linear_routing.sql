-- Linear routing channel queries: the admin's reserved __linear__ group and the one
-- routing channel inside it, keyed by (group, name) so a planted look-alike is never adopted.

-- name: GetLinearRoutingGroup :one
-- Visibility-discriminated like GetOwnerDMGroup: a planted wider __linear__ group is never adopted.
SELECT id FROM channel_groups
WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3;

-- name: InsertLinearRoutingGroup :exec
INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility)
VALUES ($1, $2, NULL, $3, $4);

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

-- name: ReassertLinearRoutingMandatory :exec
UPDATE channels SET mandatory_subscription = TRUE WHERE id = $1 AND mandatory_subscription = FALSE;
