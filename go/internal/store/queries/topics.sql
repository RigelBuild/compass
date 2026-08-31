-- Topic-domain queries (sqlc adoption T4, RIG-3034). These replace the inline
-- SQL literals in internal/store/topics.go; the hand-written Store methods keep
-- their signatures, the UpdateTopic tx orchestration, the rename/merge resolution
-- loop, and the D9 not-found/forbidden error mapping. The topic projection
-- (id, channel_id, name, created_by_account_id, created_at_unix_ms, archived,
-- last_seq) matches the former scanTopics order so the Go maps each row to Topic.

-- name: ListTopics :many
SELECT id, channel_id, name, created_by_account_id, created_at_unix_ms, archived, last_seq
FROM topics
WHERE channel_id = $1 AND ($2 OR NOT archived)
ORDER BY last_seq DESC, created_at_unix_ms DESC, id;

-- name: ResolveTopicForUpdate :one
SELECT t.channel_id FROM topics t
JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
WHERE t.id = $2
FOR UPDATE OF t;

-- name: SetTopicArchived :exec
UPDATE topics SET archived = $2 WHERE id = $1;

-- name: GetTopic :one
SELECT id, channel_id, name, created_by_account_id, created_at_unix_ms, archived, last_seq
FROM topics WHERE id = $1;

-- name: ResolveTopicRenameTarget :one
SELECT id FROM topics
WHERE channel_id = $1 AND lower(name) = lower($2) AND id <> $3
FOR UPDATE;

-- name: RenameTopic :exec
UPDATE topics SET name = $2 WHERE id = $1;

-- name: MoveMessagesToTopic :exec
UPDATE messages SET topic_id = $1 WHERE topic_id = $2;

-- name: MergeTopicLastSeq :exec
UPDATE topics dst SET last_seq = GREATEST(dst.last_seq, src.last_seq)
FROM topics src WHERE dst.id = $1 AND src.id = $2;

-- name: DeleteTopic :exec
DELETE FROM topics WHERE id = $1;
