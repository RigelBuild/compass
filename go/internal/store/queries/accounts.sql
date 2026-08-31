-- Account-domain queries (sqlc adoption T2, RIG-3034). These replace the inline
-- SQL literals that lived in internal/store/accounts.go; the hand-written Store
-- methods keep their exact signatures and wrap these generated calls, mapping the
-- generated row structs back to the domain Account (see accountFromRow, and
-- accountFromRow via treeAccountText for the tree rows, in the Go).
--
-- The account projection (a.id … sy.account_id) is repeated per query because
-- sqlc has no query-fragment composition. It is column-identical to the former
-- scanAccount projection (accounts LEFT JOIN user_accounts LEFT JOIN
-- agent_accounts LEFT JOIN system_accounts), with the two `role` columns aliased
-- (user_role / agent_role) so the generated row fields do not collide. The
-- account-visibility predicate (formerly the accountVisibleFromWhere Go const) is
-- likewise inlined into each read that needs it; the four copies MUST stay
-- textually identical so the roster clip cannot drift from the ListAccounts read.

-- name: InsertAccount :exec
INSERT INTO accounts (id, handle, display_name, tenant_id)
VALUES ($1, $2, $3, $4);

-- name: InsertUserAccount :exec
INSERT INTO user_accounts (account_id, role)
VALUES ($1, $2);

-- name: InsertSystemAccount :exec
INSERT INTO system_accounts (account_id)
VALUES ($1);

-- name: InsertAgentAccount :exec
INSERT INTO agent_accounts (account_id, owner_user_id, home_channel_id, persona, role, parent_agent_id)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''));

-- name: InsertAccountHandle :exec
INSERT INTO account_handles (account_id, handle, owner_user_id)
VALUES ($1, $2, NULLIF($3, ''));

-- name: InsertHomeChannel :exec
INSERT INTO channels (id, name, group_id, kind)
VALUES ($1, $2, NULL, $3);

-- name: SeedHomeChannelMembers :exec
INSERT INTO channel_members (channel_id, account_id, subscribed)
VALUES ($1, $2, FALSE), ($1, $3, TRUE);

-- name: EnsureChannelMember :exec
INSERT INTO channel_members (channel_id, account_id, subscribed)
VALUES ($1, $2, FALSE)
ON CONFLICT (channel_id, account_id) DO NOTHING;

-- name: CountRootAgents :one
SELECT COUNT(*)
FROM agent_accounts
WHERE owner_user_id = $1 AND parent_agent_id IS NULL;

-- name: GetAccount :one
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM accounts a
LEFT JOIN user_accounts u ON u.account_id = a.id
LEFT JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE a.id = $1;

-- name: GetAccountByGlobalHandle :one
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM account_handles ah
JOIN accounts a ON a.id = ah.account_id
LEFT JOIN user_accounts u ON u.account_id = a.id
LEFT JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE ah.owner_user_id IS NULL AND ah.handle = $1;

-- name: GetAccountByOwnerHandle :one
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM account_handles ah
JOIN accounts a ON a.id = ah.account_id
LEFT JOIN user_accounts u ON u.account_id = a.id
LEFT JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE ah.owner_user_id = $1 AND ah.handle = $2;

-- name: GetAgentOwner :one
SELECT owner_user_id
FROM agent_accounts
WHERE account_id = $1;

-- name: ResolveOwner :one
SELECT COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $1), $1)::text AS owner;

-- name: AcquireOwnerTreeLock :exec
SELECT pg_advisory_xact_lock(hashtext($1));

-- name: GetAgentParent :one
SELECT parent_agent_id
FROM agent_accounts
WHERE account_id = $1;

-- name: UpdateAgentParent :exec
UPDATE agent_accounts
SET parent_agent_id = NULLIF($2, '')
WHERE account_id = $1;

-- name: GetGlobalHandleID :one
SELECT ah.account_id
FROM account_handles ah
WHERE ah.owner_user_id IS NULL AND ah.handle = $1
  AND NOT EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = ah.account_id);

-- name: GetVisibleGlobalHandleID :one
SELECT ah.account_id
FROM account_handles ah
WHERE ah.owner_user_id IS NULL AND ah.handle = $2
  AND NOT EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = ah.account_id)
  AND EXISTS (
      SELECT 1
      FROM accounts a
      LEFT JOIN user_accounts u ON u.account_id = a.id
      LEFT JOIN agent_accounts ag ON ag.account_id = a.id
      LEFT JOIN system_accounts sy ON sy.account_id = a.id
      WHERE (
              a.id = $1
           OR u.account_id IS NOT NULL
           OR ag.owner_user_id = $1
           OR EXISTS (
               SELECT 1
               FROM channel_members cm_self
               JOIN channel_members cm_them ON cm_them.channel_id = cm_self.channel_id
               WHERE cm_self.account_id = $1 AND cm_them.account_id = a.id
           )
            )
        AND a.id = ah.account_id
  );

-- name: GetVisibleAgentHandleID :one
SELECT ah.account_id
FROM account_handles ah
WHERE ah.owner_user_id = $2 AND ah.handle = $3
  AND EXISTS (
      SELECT 1
      FROM accounts a
      LEFT JOIN user_accounts u ON u.account_id = a.id
      LEFT JOIN agent_accounts ag ON ag.account_id = a.id
      LEFT JOIN system_accounts sy ON sy.account_id = a.id
      WHERE (
              a.id = $1
           OR u.account_id IS NOT NULL
           OR ag.owner_user_id = $1
           OR EXISTS (
               SELECT 1
               FROM channel_members cm_self
               JOIN channel_members cm_them ON cm_them.channel_id = cm_self.channel_id
               WHERE cm_self.account_id = $1 AND cm_them.account_id = a.id
           )
            )
        AND a.id = ah.account_id
  );

-- name: ListVisibleAccounts :many
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM accounts a
LEFT JOIN user_accounts u ON u.account_id = a.id
LEFT JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE (
        a.id = $1
     OR u.account_id IS NOT NULL
     OR ag.owner_user_id = $1
     OR EXISTS (
         SELECT 1
         FROM channel_members cm_self
         JOIN channel_members cm_them ON cm_them.channel_id = cm_self.channel_id
         WHERE cm_self.account_id = $1 AND cm_them.account_id = a.id
     )
      )
ORDER BY a.handle;

-- name: AccountVisibleTo :one
SELECT EXISTS (
    SELECT 1
    FROM accounts a
    LEFT JOIN user_accounts u ON u.account_id = a.id
    LEFT JOIN agent_accounts ag ON ag.account_id = a.id
    LEFT JOIN system_accounts sy ON sy.account_id = a.id
    WHERE (
            a.id = $1
         OR u.account_id IS NOT NULL
         OR ag.owner_user_id = $1
         OR EXISTS (
             SELECT 1
             FROM channel_members cm_self
             JOIN channel_members cm_them ON cm_them.channel_id = cm_self.channel_id
             WHERE cm_self.account_id = $1 AND cm_them.account_id = a.id
         )
          )
      AND a.id = $2
);
