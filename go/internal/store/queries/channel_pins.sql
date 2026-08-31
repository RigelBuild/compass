-- Channel-pins (pinned board) queries (sqlc adoption T3, RIG-3034). These
-- replace the inline SQL literals in internal/store/channel_pins.go; the
-- hand-written Store methods and the in-tx FOR UPDATE lock / cap-check control
-- flow stay exactly as they were and wrap these generated calls.

-- name: LockChannelForPins :one
SELECT post_policy, COALESCE(owner_account_id, '') AS owner_account_id
FROM channels WHERE id = $1 FOR UPDATE;

-- name: MessageInChannel :one
SELECT 1 FROM messages m JOIN topics t ON t.id = m.topic_id
WHERE m.id = $1 AND t.channel_id = $2;

-- name: CountChannelPins :one
SELECT count(*) AS count, COALESCE(MAX(position), -1) + 1 AS next_position
FROM channel_pins WHERE channel_id = $1;

-- name: DeleteChannelPin :exec
DELETE FROM channel_pins WHERE channel_id = $1 AND message_id = $2;

-- name: DeleteChannelPinReturningPosition :one
DELETE FROM channel_pins WHERE channel_id = $1 AND message_id = $2 RETURNING position;

-- name: InsertChannelPin :exec
INSERT INTO channel_pins (channel_id, message_id, position, pinned_at_unix_ms, pinned_by_account_id)
VALUES ($1, $2, $3, $4, $5);

-- name: PinnedEntries :many
SELECT message_id, position, pinned_at_unix_ms, pinned_by_account_id
FROM channel_pins WHERE channel_id = $1 ORDER BY position;
