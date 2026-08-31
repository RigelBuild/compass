-- Message-domain queries (sqlc adoption T4, RIG-3034). These replace the inline
-- SQL literals in internal/store/messages.go; the hand-written Store methods keep
-- their exact signatures, their tx orchestration (AppendMessage/AnswerAsk begin
-- and commit their own txns), the ON CONFLICT idempotency signalling
-- (errMessageInsertConflict), the JSONB block (de)serialization, and the D9
-- not-found/forbidden error mapping — all hand-written around these generated
-- calls. Every message read shares the id/topic_id/author_account_id/at_unix_ms/
-- blocks projection (the former scanMessages order) so the Go maps each row the
-- same way via messageFromParts.

-- name: GetChannelPostPolicy :one
SELECT post_policy, COALESCE(owner_account_id, '') AS owner_account_id
FROM channels WHERE id = $1;

-- name: InsertMessage :one
INSERT INTO messages (id, topic_id, author_account_id, at_unix_ms, blocks, text_content, client_request_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (author_account_id, client_request_id) WHERE client_request_id <> ''
DO NOTHING
RETURNING id, at_unix_ms, seq;

-- name: UpdateTopicLastSeq :exec
UPDATE topics SET last_seq = GREATEST(last_seq, $2) WHERE id = $1;

-- name: GetTopicChannel :one
SELECT channel_id FROM topics WHERE id = $1;

-- name: InsertTopicIgnore :exec
INSERT INTO topics (id, channel_id, name, created_by_account_id, created_at_unix_ms)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (channel_id, lower(name)) DO NOTHING;

-- name: GetTopicByName :one
SELECT id, archived FROM topics WHERE channel_id = $1 AND lower(name) = lower($2);

-- name: ReviveTopic :exec
UPDATE topics SET archived = FALSE WHERE id = $1;

-- name: MessagesHeadSeq :one
SELECT COALESCE(MAX(seq), 0)::BIGINT AS head FROM messages;

-- name: UpdateMessageBlocks :execrows
UPDATE messages SET blocks = $1, text_content = $2 WHERE id = $3;

-- name: UpdateMessageBlocksAsAuthor :one
UPDATE messages m
SET blocks = $1, text_content = $2
FROM topics t
WHERE m.id = $3
  AND t.id = m.topic_id
  AND m.author_account_id = $4
  AND EXISTS (
    SELECT 1 FROM channel_members cm
    WHERE cm.channel_id = t.channel_id AND cm.account_id = $4
  )
RETURNING m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks;

-- name: GetMessageBlocks :one
SELECT blocks FROM messages WHERE id = $1;

-- name: GetPageCursorSeq :one
SELECT m.seq FROM messages m
JOIN topics t ON t.id = m.topic_id
JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
WHERE m.id = $2 AND t.channel_id = $3;

-- name: ListMessages :many
SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks
FROM messages m
JOIN topics t ON t.id = m.topic_id
JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
WHERE t.channel_id = $2 AND ($3 = 0 OR m.seq < $3) AND ($5 = 0 OR m.seq <= $5)
  AND ($6 = '' OR m.topic_id = $6)
ORDER BY m.seq DESC
LIMIT $4;

-- name: SearchMessages :many
SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks
FROM messages m
JOIN topics t ON t.id = m.topic_id
JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
WHERE m.search_tsv @@ websearch_to_tsquery('english', $2)
  AND ($3 = '' OR t.channel_id = $3)
  AND ($5 = 0 OR m.seq <= $5)
ORDER BY ts_rank(m.search_tsv, websearch_to_tsquery('english', $2)) DESC, m.seq DESC
LIMIT $4;

-- name: FindAskMessage :many
SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks
FROM messages m
JOIN topics t ON t.id = m.topic_id
JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
WHERE m.blocks @> $2::jsonb
FOR UPDATE OF m;

-- name: GetMessageByRequestID :many
SELECT id, topic_id, author_account_id, at_unix_ms, blocks
FROM messages
WHERE author_account_id = $1 AND client_request_id = $2;
