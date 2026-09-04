-- Delivery-cursor queries (sqlc adoption T4, RIG-3034). These replace the inline
-- SQL literals in internal/store/delivery_cursors.go; the hand-written Store
-- methods keep their signatures, the AckDelivery tx orchestration (the owed-clear
-- FIRST, the commit-if-cleared arm, the contiguous-advance loop in Go), and the
-- D2 seed self-guard/idempotency contract. The two message-fanout reads
-- (OwedMentions, UndeliveredMessages) share the per-channel projection the Go
-- drains with an inline loop calling messageFromParts.

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
SELECT m.id, m.topic_id, t.channel_id, m.author_account_id, m.at_unix_ms, m.blocks
FROM owed_mentions om
JOIN messages m ON m.id = om.message_id
JOIN topics t ON t.id = m.topic_id
WHERE om.agent_account_id = $1
ORDER BY t.channel_id, m.seq ASC;

-- name: ClearOwedMention :execrows
DELETE FROM owed_mentions WHERE agent_account_id = $1 AND message_id = $2;

-- name: CountOwedMentions :one
SELECT COUNT(*) FROM owed_mentions;

-- name: MarkMentionsRouted :exec
UPDATE messages SET mentions_routed_at = $1 WHERE id = $2;

-- name: UnroutedMentionMessages :many
SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks, t.channel_id, m.seq
FROM messages m
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

-- name: SelfAuthoredSeqsAbove :many
SELECT m.seq FROM messages m JOIN topics t ON t.id = m.topic_id
WHERE t.channel_id = $1 AND m.seq > $2 AND m.author_account_id = $3;

-- name: AdvanceDeliveryCursor :exec
UPDATE agent_delivery_cursors
SET acked_seq = $3, above_seqs = $4, acked_at = now()
WHERE agent_account_id = $1 AND channel_id = $2;

-- name: UndeliveredMessages :many
SELECT m.id, m.topic_id, t.channel_id, m.author_account_id, m.at_unix_ms, m.blocks
FROM channel_members cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
JOIN topics t ON t.channel_id = cm.channel_id
JOIN messages m ON m.topic_id = t.id
JOIN channels ch ON ch.id = cm.channel_id
LEFT JOIN agent_delivery_cursors dc
       ON dc.agent_account_id = cm.account_id AND dc.channel_id = cm.channel_id
WHERE cm.account_id = $1
  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
  AND m.author_account_id <> $1
  AND m.seq > COALESCE(
        dc.acked_seq,
        (SELECT COALESCE(MAX(mh.seq), 0) FROM messages mh JOIN topics th ON th.id = mh.topic_id WHERE th.channel_id = cm.channel_id))
  AND m.seq <> ALL(COALESCE(dc.above_seqs, '{}'::BIGINT[]))
ORDER BY t.channel_id, m.seq ASC;

-- name: InSweepSet :one
SELECT EXISTS(
	SELECT 1
	FROM channel_members cm
	JOIN agent_accounts aa ON aa.account_id = cm.account_id
	JOIN channels ch ON ch.id = cm.channel_id
	WHERE cm.account_id = $1
	  AND cm.channel_id = $2
	  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription));
