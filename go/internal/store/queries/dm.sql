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
INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, visibility, namespace_owner_id)
VALUES ($1, $2, NULL, $3, $4,
        COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $3), $3));

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

-- AccountDMChannelsForRename reads same-owner DMs by actual agent-party handles.
-- An agent subject is a party directly. A user subject is a party only in an
-- owner-agent DM with exactly one agent party; user members of agent-agent DMs
-- are pulled-in owners and do not affect the deterministic pair name.
--
-- name: AccountDMChannelsForRename :many
SELECT c.id, other.account_id, other_handle.handle
FROM channels c
JOIN channel_groups cg ON cg.id = c.group_id AND cg.name = '__dm__'
    AND cg.parent_group_id IS NULL AND cg.visibility = 0
JOIN channel_members subject ON subject.channel_id = c.id AND subject.account_id = $1
LEFT JOIN agent_accounts subject_party ON subject_party.account_id = subject.account_id
JOIN channel_members other ON other.channel_id = c.id AND other.account_id <> $1
LEFT JOIN agent_accounts other_party ON other_party.account_id = other.account_id
JOIN account_handles other_handle ON other_handle.account_id = other.account_id
JOIN account_handles subject_handle ON subject_handle.account_id = subject.account_id
WHERE c.kind = 1
  AND cg.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)
  AND (
      (
          subject_party.account_id IS NULL
          AND other_party.account_id IS NOT NULL
          AND NOT EXISTS (
              SELECT 1
              FROM channel_members party_member
              JOIN agent_accounts party ON party.account_id = party_member.account_id
              WHERE party_member.channel_id = c.id
                AND party_member.account_id <> other.account_id
          )
      )
      OR (
          subject_party.account_id IS NOT NULL
          AND (
              (
                  other_party.account_id IS NOT NULL
                  AND NOT EXISTS (
                      SELECT 1
                      FROM channel_members party_member
                      JOIN agent_accounts party ON party.account_id = party_member.account_id
                      WHERE party_member.channel_id = c.id
                        AND party_member.account_id NOT IN (subject.account_id, other.account_id)
                  )
              )
              OR (
                  other.account_id = subject_party.owner_user_id
                  AND NOT EXISTS (
                      SELECT 1
                      FROM channel_members party_member
                      JOIN agent_accounts party ON party.account_id = party_member.account_id
                      WHERE party_member.channel_id = c.id
                        AND party_member.account_id <> subject.account_id
                  )
              )
          )
      )
  )
  AND c.name IN (
      'dm:' || subject_handle.handle || ':' || other_handle.handle,
      'dm:' || other_handle.handle || ':' || subject_handle.handle
  )
ORDER BY c.id, other.account_id;

-- name: RenameDMChannel :execrows
UPDATE channels SET name = $2
WHERE id = $1 AND kind = 1 AND name = $3;

-- name: LockOwnerDM :exec
SELECT pg_advisory_xact_lock(hashtext('dm:' || $1));

-- name: ReassertDMMandatory :exec
UPDATE channels SET mandatory_subscription = TRUE WHERE id = $1 AND mandatory_subscription = FALSE;

-- name: GetGroupNameVisibility :one
-- Feeds isReservedGroupTx: the reserved-group discriminator (top-level AND a reserved name AND
-- VisibilityOwner) the CreateChannel and CreateChannelGroup create-guards key on.
SELECT name, visibility, (parent_group_id IS NULL)::boolean AS top_level FROM channel_groups WHERE id = $1;
