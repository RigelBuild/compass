-- Delivery-consumer read queries (sqlc adoption T4, RIG-3034). These replace the
-- inline SQL literals in internal/store/delivery_reads.go; the hand-written Store
-- methods keep their signatures, the D1 sweep-set disjunct (kept textually in
-- sync with delivery_cursors.sql UndeliveredMessages/InSweepSet), and the D9
-- error mapping. MessageByID shares the message projection the Go drains via
-- messageFromParts.

-- name: SubscribedAgents :many
SELECT aa.account_id
FROM channel_members cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
JOIN channels ch ON ch.id = cm.channel_id
WHERE cm.channel_id = $1
  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
  AND cm.account_id <> $2
ORDER BY aa.account_id;

-- name: ChannelAgentMembers :many
SELECT aa.account_id
FROM channel_members cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
WHERE cm.channel_id = $1
  AND cm.account_id <> $2
ORDER BY aa.account_id;

-- name: IsAgentAccount :one
SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE account_id = $1);

-- name: MessageByID :one
SELECT id, topic_id, author_account_id, at_unix_ms, blocks
FROM messages
WHERE id = $1;

-- name: MessageChannel :one
SELECT t.channel_id FROM messages m JOIN topics t ON t.id = m.topic_id WHERE m.id = $1;

-- name: TopicChannelNames :one
SELECT t.name AS topic_name, c.name AS channel_name FROM topics t JOIN channels c ON c.id = t.channel_id WHERE t.id = $1;
-- name: SweepChannels :many
SELECT cm.channel_id
FROM channel_members cm
JOIN agent_accounts aa ON aa.account_id = cm.account_id
JOIN channels ch ON ch.id = cm.channel_id
WHERE cm.account_id = $1
  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
ORDER BY cm.channel_id;
