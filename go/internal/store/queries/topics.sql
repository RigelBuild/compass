-- Topic-domain queries (sqlc adoption T4, RIG-3034). These replace the inline
-- SQL literals in internal/store/topics.go; the hand-written Store methods keep
-- their signatures, the UpdateTopic tx orchestration, the rename/merge resolution
-- loop, and the D9 not-found/forbidden error mapping. The topic projection
-- (id, channel_id, name, created_by_account_id, created_at_unix_ms, archived,
-- last_seq) matches the former scanTopics order so the Go maps each row to Topic.

-- Participant-channel copies: every `chain` + `participating` CTE in this
-- file and topics.sql MUST stay identical and equal to ChannelParticipant
-- (authz.sql). It is participation, not channel visibility: never widen it to
-- the owner-set visibility predicate. A future ACL conjunct goes in each copy.

-- name: ListTopics :many
SELECT id, channel_id, name, created_by_account_id, created_at_unix_ms, archived, last_seq, tenant_id
FROM topics
WHERE channel_id = $1 AND ($2 OR NOT archived)
ORDER BY last_seq DESC, created_at_unix_ms DESC, id;

-- name: ResolveTopicForUpdate :one
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
), participating AS (
    SELECT cm.channel_id FROM channel_members cm WHERE cm.account_id = $1
    UNION
    SELECT c.id FROM channels c
    WHERE c.membership_mode = 1
      AND (
          c.parent_agent_id IN (SELECT account_id FROM chain)
          OR c.parent_agent_id IN (SELECT aa.account_id FROM agent_accounts aa WHERE aa.owner_user_id = $1)
      )
)
SELECT t.channel_id FROM topics t
JOIN participating p ON p.channel_id = t.channel_id
WHERE t.id = $2
FOR UPDATE OF t;

-- name: SetTopicArchived :exec
UPDATE topics SET archived = $2 WHERE id = $1;

-- name: GetTopic :one
SELECT id, channel_id, name, created_by_account_id, created_at_unix_ms, archived, last_seq, tenant_id
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
