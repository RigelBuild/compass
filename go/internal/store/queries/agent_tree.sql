-- Agent-tree queries (sqlc adoption T2, RIG-3034). These replace the
-- const-hoisted `agentTreeProjection` + per-caller WHERE composition that lived
-- in internal/store/agent_tree.go and was run through the `queryAgents` helper —
-- exactly the "SQL in a variable" shape the inline-SQL gate cannot grep. Each of
-- the three tree reads is now a full, static, schema-checked query.
--
-- The projection is column-identical to the former agentTreeProjection: the join
-- to agent_accounts is INNER (the tree is agents-only), so every row scans back
-- through the shared accountFromRow mapping (fed by treeAccountText) into a domain Account with its
-- Agent subtype set. The two `role` columns are aliased (user_role / agent_role)
-- so the generated row fields do not collide.

-- name: AgentNeighborhood :many
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM accounts a
LEFT JOIN user_accounts u ON u.account_id = a.id
JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE a.id = $1
   OR a.id = (SELECT parent_agent_id FROM agent_accounts WHERE account_id = $1)
   OR ag.parent_agent_id IS NOT DISTINCT FROM
        (SELECT parent_agent_id FROM agent_accounts WHERE account_id = $1)
   OR ag.parent_agent_id = $1
ORDER BY a.id;

-- name: AgentSubtree :many
WITH RECURSIVE subtree AS (
    SELECT ag0.account_id FROM agent_accounts ag0 WHERE ag0.account_id = $1
    UNION
    SELECT ag.account_id
    FROM agent_accounts ag
    JOIN subtree ON ag.parent_agent_id = subtree.account_id
)
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM accounts a
LEFT JOIN user_accounts u ON u.account_id = a.id
JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE a.id IN (SELECT subtree.account_id FROM subtree)
ORDER BY a.id;

-- name: AgentsByOwner :many
SELECT a.id, a.handle, a.display_name,
       u.role AS user_role,
       ag.owner_user_id, ag.home_channel_id, ag.persona, ag.role AS agent_role, ag.parent_agent_id,
       sy.account_id AS system_account_id
FROM accounts a
LEFT JOIN user_accounts u ON u.account_id = a.id
JOIN agent_accounts ag ON ag.account_id = a.id
LEFT JOIN system_accounts sy ON sy.account_id = a.id
WHERE ag.owner_user_id = $1
ORDER BY a.id;
