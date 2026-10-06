-- The marked reach predicate is one gate shared with delivery_cursors.sql;
-- sql_parity_test.go fails if the copies drift.

-- The five delivery membership sites (SubscribedAgents, ChannelAgentMembers,
-- SweepChannels here; UndeliveredMessages, InSweepSet in delivery_cursors.sql)
-- drive from a participants CTE: stored member rows UNION the TREE-derived set.
-- Channel-keyed sites walk the anchor's subtree; account-keyed ones the chain up.

-- name: SubscribedAgents :many
WITH RECURSIVE subtree AS (
    SELECT c.id AS channel_id, c.parent_agent_id AS account_id
    FROM channels c
    WHERE c.id = $1 AND c.membership_mode = 1
    UNION
    SELECT s.channel_id, a.account_id
    FROM agent_accounts a
    JOIN subtree s ON a.parent_agent_id = s.account_id
), participants AS (
    SELECT cm.channel_id, cm.account_id, cm.subscribed
    FROM channel_members cm
    WHERE cm.channel_id = $1
    UNION ALL
    SELECT s.channel_id, s.account_id, COALESCE(cs.subscribed, FALSE) AS subscribed
    FROM subtree s
    LEFT JOIN channel_subscriptions cs
        ON cs.channel_id = s.channel_id AND cs.account_id = s.account_id
)
SELECT aa.account_id
FROM participants cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
JOIN channels ch ON ch.id = cm.channel_id
WHERE cm.channel_id = $1
  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
  AND cm.account_id <> $2
  AND
-- reach: the author may reach agent aa
(   aa.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $2), $2)
 OR EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = $2)
 OR EXISTS (SELECT 1 FROM user_peers p_out
            JOIN user_peers p_in ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
            WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $2), $2)
              AND p_out.peer_user_id = aa.owner_user_id)
)
ORDER BY aa.account_id;

-- name: ChannelAgentMembers :many
WITH RECURSIVE subtree AS (
    SELECT c.id AS channel_id, c.parent_agent_id AS account_id
    FROM channels c
    WHERE c.id = $1 AND c.membership_mode = 1
    UNION
    SELECT s.channel_id, a.account_id
    FROM agent_accounts a
    JOIN subtree s ON a.parent_agent_id = s.account_id
), participants AS (
    SELECT cm.channel_id, cm.account_id, cm.subscribed
    FROM channel_members cm
    WHERE cm.channel_id = $1
    UNION ALL
    SELECT s.channel_id, s.account_id, COALESCE(cs.subscribed, FALSE) AS subscribed
    FROM subtree s
    LEFT JOIN channel_subscriptions cs
        ON cs.channel_id = s.channel_id AND cs.account_id = s.account_id
)
SELECT aa.account_id, aa.owner_user_id,
       COALESCE(oh.handle, '') AS owner_handle, COALESCE(ah.handle, '') AS handle
FROM participants cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
-- Keep handle joins optional so missing handle rows do not hide @everyone members.
LEFT JOIN account_handles ah ON ah.account_id = aa.account_id
    AND ah.owner_user_id = aa.owner_user_id AND ah.tenant_id = aa.tenant_id
LEFT JOIN account_handles oh ON oh.account_id = aa.owner_user_id
    AND oh.owner_user_id IS NULL AND oh.tenant_id = aa.tenant_id
WHERE cm.channel_id = $1
  AND cm.account_id <> $2
  AND
-- reach: the author may reach agent aa
(   aa.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $2), $2)
 OR EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = $2)
 OR EXISTS (SELECT 1 FROM user_peers p_out
            JOIN user_peers p_in ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
            WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = $2), $2)
              AND p_out.peer_user_id = aa.owner_user_id)
)
ORDER BY aa.account_id;

-- name: IsAgentAccount :one
SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE account_id = $1);

-- name: MessageByID :one
SELECT m.id, m.topic_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence
FROM messages m
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
WHERE m.id = $1;

-- name: MessageChannel :one
SELECT t.channel_id FROM messages m JOIN topics t ON t.id = m.topic_id WHERE m.id = $1;

-- name: TopicChannelNames :one
SELECT t.name AS topic_name, c.name AS channel_name FROM topics t JOIN channels c ON c.id = t.channel_id WHERE t.id = $1;
-- name: SweepChannels :many
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
), participants AS (
    SELECT cm.channel_id, cm.account_id, cm.subscribed
    FROM channel_members cm
    WHERE cm.account_id = $1
    UNION ALL
    SELECT c.id AS channel_id, ch.account_id, COALESCE(cs.subscribed, FALSE) AS subscribed
    FROM channels c
    JOIN chain ch ON ch.account_id = $1
    LEFT JOIN channel_subscriptions cs
        ON cs.channel_id = c.id AND cs.account_id = ch.account_id
    WHERE c.membership_mode = 1
      AND c.parent_agent_id IN (SELECT account_id FROM chain)
)
SELECT cm.channel_id
FROM participants cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
JOIN channels ch ON ch.id = cm.channel_id
WHERE cm.account_id = $1
  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
ORDER BY cm.channel_id;
