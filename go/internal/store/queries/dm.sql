-- Peer-DM channel queries (sqlc adoption T6, RIG-3034; dm.go was added to the
-- store after the design record froze — the record's "plus any residue"). These
-- replace the inline SQL literals in internal/store/dm.go; the hand-written Store
-- methods keep their signatures and every seam that is NOT a single statement:
-- the per-owner advisory lock (LockDM), the resolution/insert loop, the R3
-- verify-reconcile belt, the transitive-owner membership expansion, and the
-- cursor seeding (seedChannelDeliveryCursors, delivery_cursors.sql). The member
-- INSERTs reuse EnsureChannelMember (accounts.sql) — the statement is identical.

-- name: GetOwnerDMGroup :one
-- Visibility-discriminated get-half: a wider (SHARED) planted __dm__ group must
-- NEVER be adopted, so visibility = $3 (bound to VisibilityOwner) excludes it.
SELECT id FROM channel_groups
WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3;

-- name: InsertOwnerDMGroup :exec
INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility)
VALUES ($1, $2, NULL, $3, $4);

-- name: GetDMChannelByName :one
SELECT id, kind FROM channels WHERE group_id = $1 AND name = $2;

-- name: InsertDMChannel :one
-- Born kind=DM, zero-value policy (OPEN, ownerless) + mandatory; poison-free via
-- ON CONFLICT DO NOTHING on the partial unique index (a concurrent open yields
-- zero rows, never a raised unique-violation).
INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription)
VALUES ($1, $2, $3, $4, $5, NULL, $6)
ON CONFLICT (group_id, name) WHERE group_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: LockOwnerDM :exec
SELECT pg_advisory_xact_lock(hashtext('dm:' || $1));

-- name: ReassertDMMandatory :exec
UPDATE channels SET mandatory_subscription = TRUE WHERE id = $1 AND mandatory_subscription = FALSE;

-- name: GetGroupNameVisibility :one
-- Feeds isReservedDMGroupTx: the reserved-DM-group discriminator (name AND
-- VisibilityOwner) the CreateChannel create-guard keys on.
SELECT name, visibility FROM channel_groups WHERE id = $1;
