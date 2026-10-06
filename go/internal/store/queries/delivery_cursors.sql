-- Delivery-cursor reads share the author's reach predicate with delivery_reads.sql.
-- A member outside the author's owner or live peering is not delivered.

-- name: SeedDeliveryCursor :exec
INSERT INTO agent_delivery_cursors (agent_account_id, channel_id, acked_seq)
SELECT $1, $2, COALESCE((SELECT MAX(m.seq) FROM messages m JOIN topics t ON t.id = m.topic_id WHERE t.channel_id = $2), 0)
WHERE EXISTS (SELECT 1 FROM agent_accounts WHERE account_id = $1)
ON CONFLICT (agent_account_id, channel_id) DO NOTHING;

-- name: SeedChannelDeliveryCursors :exec
INSERT INTO agent_delivery_cursors (agent_account_id, channel_id, acked_seq)
SELECT cm.account_id, $1,
       COALESCE((SELECT MAX(m.seq) FROM messages m JOIN topics t ON t.id = m.topic_id WHERE t.channel_id = $1), 0)
FROM channel_members cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
WHERE cm.channel_id = $1
ON CONFLICT (agent_account_id, channel_id) DO NOTHING;

-- name: RecordOwedMention :exec
-- Runs under the BYPASSRLS system role (delivery consumer, no tenant GUC), so
-- tenant_id is stamped explicitly from the owning account's FK rather than the
-- column DEFAULT (which would NULL-violate with no GUC). The INSERT..SELECT
-- yields zero rows only if $1 has no accounts row — impossible on the no-loss
-- path: the caller always passes an agent resolved from live channel membership
-- (delivery/dispatch.go), whose accounts row exists. A stray user/unknown id
-- would instead FK-violate the owed_mentions -> agent_accounts FK. The
-- ON CONFLICT DO NOTHING is the intended idempotent re-record (a zero-row result
-- there is the NORMAL replay case, not a drop), so asserting rows-affected here
-- would wrongly fail an idempotent re-fire.
INSERT INTO owed_mentions (agent_account_id, message_id, channel_id, recorded_at_unix_ms, tenant_id)
SELECT $1, $2, $3, $4, a.tenant_id FROM accounts a WHERE a.id = $1
ON CONFLICT (agent_account_id, message_id) DO NOTHING;
-- name: OwedMentions :many
SELECT m.id, m.topic_id, t.channel_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence
FROM owed_mentions om
JOIN messages m ON m.id = om.message_id
JOIN agent_accounts aa ON aa.account_id = om.agent_account_id
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
JOIN topics t ON t.id = m.topic_id
WHERE om.agent_account_id = $1
  AND
-- reach: the author may reach agent aa
(   aa.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = m.author_account_id), m.author_account_id)
 OR EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = m.author_account_id)
 OR EXISTS (SELECT 1 FROM user_peers p_out
            JOIN user_peers p_in ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
            WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = m.author_account_id), m.author_account_id)
              AND p_out.peer_user_id = aa.owner_user_id)
)
ORDER BY t.channel_id, m.seq ASC;

-- name: ClearOwedMention :execrows
DELETE FROM owed_mentions WHERE agent_account_id = $1 AND message_id = $2;

-- name: CountOwedMentions :one
SELECT COUNT(*) FROM owed_mentions;

-- name: MarkMentionsRouted :exec
UPDATE messages SET mentions_routed_at = $1 WHERE id = $2;
-- name: UnroutedMentionMessages :many
SELECT m.id, m.topic_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence, t.channel_id, m.seq
FROM messages m
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
JOIN topics t ON t.id = m.topic_id
WHERE m.mentions_routed_at IS NULL AND m.seq > $1
ORDER BY m.seq ASC
LIMIT $2;

-- name: ResolveAckMessage :one
SELECT m.seq FROM messages m JOIN topics t ON t.id = m.topic_id WHERE m.id = $1 AND t.channel_id = $2;

-- name: LoadDeliveryCursor :one
SELECT acked_seq, above_seqs FROM agent_delivery_cursors
WHERE agent_account_id = $1 AND channel_id = $2
FOR UPDATE;

-- name: SkippableSeqsAbove :many
SELECT m.seq FROM messages m JOIN topics t ON t.id = m.topic_id
JOIN agent_accounts aa ON aa.account_id = sqlc.arg(agent_account_id)
WHERE t.channel_id = $1 AND m.seq > $2
  AND (m.author_account_id = sqlc.arg(agent_account_id) OR NOT
-- reach: the author may reach agent aa
(   aa.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = m.author_account_id), m.author_account_id)
 OR EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = m.author_account_id)
 OR EXISTS (SELECT 1 FROM user_peers p_out
            JOIN user_peers p_in ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
            WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = m.author_account_id), m.author_account_id)
              AND p_out.peer_user_id = aa.owner_user_id)
));

-- name: AdvanceDeliveryCursor :exec
UPDATE agent_delivery_cursors
SET acked_seq = $3, above_seqs = $4, acked_at = now()
WHERE agent_account_id = $1 AND channel_id = $2;
-- name: UndeliveredMessages :many
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
SELECT m.id, m.topic_id, t.channel_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence
FROM participants cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
JOIN topics t ON t.channel_id = cm.channel_id
JOIN messages m ON m.topic_id = t.id
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
JOIN channels ch ON ch.id = cm.channel_id
LEFT JOIN agent_delivery_cursors dc
       ON dc.agent_account_id = cm.account_id AND dc.channel_id = cm.channel_id
WHERE cm.account_id = $1
  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
  AND m.author_account_id <> $1
  AND
-- reach: the author may reach agent aa
(   aa.owner_user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = m.author_account_id), m.author_account_id)
 OR EXISTS (SELECT 1 FROM system_accounts sy WHERE sy.account_id = m.author_account_id)
 OR EXISTS (SELECT 1 FROM user_peers p_out
            JOIN user_peers p_in ON p_in.user_id = p_out.peer_user_id AND p_in.peer_user_id = p_out.user_id
            WHERE p_out.user_id = COALESCE((SELECT owner_user_id FROM agent_accounts WHERE account_id = m.author_account_id), m.author_account_id)
              AND p_out.peer_user_id = aa.owner_user_id)
)
  AND m.seq > COALESCE(
        dc.acked_seq,
        (SELECT COALESCE(MAX(mh.seq), 0) FROM messages mh JOIN topics th ON th.id = mh.topic_id WHERE th.channel_id = cm.channel_id))
  AND m.seq <> ALL(COALESCE(dc.above_seqs, '{}'::BIGINT[]))
ORDER BY t.channel_id, m.seq ASC;

-- name: InSweepSet :one
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
      AND EXISTS (SELECT 1 FROM channels WHERE id = $2 AND membership_mode = 1)
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
      AND c.id = $2
      AND c.parent_agent_id IN (SELECT account_id FROM chain)
)
SELECT EXISTS(
	SELECT 1
	FROM participants cm
	JOIN agent_accounts aa ON aa.account_id = cm.account_id
	JOIN channels ch ON ch.id = cm.channel_id
	WHERE cm.account_id = $1
	  AND cm.channel_id = $2
	  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription));

-- The accounts still owed a mention: a wake that failed before any Runner could
-- serve it is retried for these once one attaches.
-- name: OwedMentionAccounts :many
SELECT DISTINCT agent_account_id FROM owed_mentions ORDER BY agent_account_id;
