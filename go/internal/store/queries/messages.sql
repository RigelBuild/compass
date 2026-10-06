-- Message-domain queries (sqlc adoption T4, RIG-3034). These replace the inline
-- SQL literals in internal/store/messages.go; the hand-written Store methods keep
-- their exact signatures, their tx orchestration (AppendMessage/AnswerAsk begin
-- and commit their own txns), the ON CONFLICT idempotency signalling
-- (errMessageInsertConflict), the JSONB block (de)serialization, and the D9
-- not-found/forbidden error mapping — all hand-written around these generated
-- calls. Every message read shares the id/topic_id/author_account_id/author_handle/
-- at_unix_ms/blocks/turn_sequence projection so Go maps each row through messageFromParts.

-- name: GetChannelPostPolicy :one
SELECT post_policy, COALESCE(owner_account_id, '') AS owner_account_id, name
FROM channels WHERE id = $1;

-- name: InsertMessage :one
INSERT INTO messages (id, topic_id, author_account_id, at_unix_ms, blocks, text_content, client_request_id, turn_sequence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (author_account_id, client_request_id) WHERE client_request_id <> ''
DO NOTHING
RETURNING id, at_unix_ms, seq,
          COALESCE((SELECT (CASE WHEN author_handles.owner_user_id IS NULL THEN author_handles.handle WHEN owner_handles.handle IS NULL THEN '' ELSE owner_handles.handle || '/' || author_handles.handle END)::text FROM account_handles AS author_handles LEFT JOIN account_handles AS owner_handles ON owner_handles.account_id = author_handles.owner_user_id WHERE author_handles.account_id = $3), '')::text AS author_handle;

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
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $4
      AND EXISTS (
          SELECT 1 FROM messages m
          JOIN topics t ON t.id = m.topic_id
          JOIN channels c ON c.id = t.channel_id
          WHERE m.id = $3 AND c.membership_mode = 1
      )
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
)
UPDATE messages m
SET blocks = $1, text_content = $2
FROM topics t
WHERE m.id = $3
  AND t.id = m.topic_id
  AND m.author_account_id = $4
  AND (
    EXISTS (
        SELECT 1 FROM channel_members cm
        WHERE cm.channel_id = t.channel_id AND cm.account_id = $4
    )
    OR (
        EXISTS (SELECT 1 FROM channels WHERE id = t.channel_id AND membership_mode = 1)
        AND EXISTS (
            SELECT 1 FROM channels c
            WHERE c.id = t.channel_id AND (
                c.parent_agent_id IN (SELECT ch.account_id FROM chain ch)
                OR $4 = (SELECT aa.owner_user_id FROM agent_accounts aa
                         WHERE aa.account_id = c.parent_agent_id)
            )
        )
    )
  )
RETURNING m.id, m.topic_id, m.author_account_id,
          COALESCE((SELECT (CASE WHEN author_handles.owner_user_id IS NULL THEN author_handles.handle WHEN owner_handles.handle IS NULL THEN '' ELSE owner_handles.handle || '/' || author_handles.handle END)::text FROM account_handles AS author_handles LEFT JOIN account_handles AS owner_handles ON owner_handles.account_id = author_handles.owner_user_id WHERE author_handles.account_id = $4), '')::text AS author_handle,
          m.at_unix_ms, m.blocks, m.turn_sequence;

-- name: GetMessageBlocksAsAuthor :one
SELECT m.blocks FROM messages m
JOIN topics t ON t.id = m.topic_id
WHERE m.id = $1
  AND m.author_account_id = $2
  AND EXISTS (
    SELECT 1 FROM channel_members cm
    WHERE cm.channel_id = t.channel_id AND cm.account_id = $2
  );

-- name: GetPageCursorSeq :one
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
      AND EXISTS (SELECT 1 FROM channels WHERE id = $3 AND membership_mode = 1)
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
)
SELECT m.seq FROM messages m
JOIN topics t ON t.id = m.topic_id
WHERE m.id = $2 AND t.channel_id = $3
  AND (
    EXISTS (
        SELECT 1 FROM channel_members cm
        WHERE cm.channel_id = t.channel_id AND cm.account_id = $1
    )
    OR (
        EXISTS (SELECT 1 FROM channels WHERE id = t.channel_id AND membership_mode = 1)
        AND EXISTS (
            SELECT 1 FROM channels c
            WHERE c.id = t.channel_id AND (
                c.parent_agent_id IN (SELECT ch.account_id FROM chain ch)
                OR $1 = (SELECT aa.owner_user_id FROM agent_accounts aa
                         WHERE aa.account_id = c.parent_agent_id)
            )
        )
    )
  );

-- name: ListMessages :many
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
      AND EXISTS (SELECT 1 FROM channels WHERE id = $2 AND membership_mode = 1)
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
)
SELECT m.id, m.topic_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence
FROM messages m
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
JOIN topics t ON t.id = m.topic_id
WHERE t.channel_id = $2 AND ($3 = 0 OR m.seq < $3) AND ($5 = 0 OR m.seq <= $5)
  AND ($6 = '' OR m.topic_id = $6)
  AND (
    EXISTS (
        SELECT 1 FROM channel_members cm
        WHERE cm.channel_id = t.channel_id AND cm.account_id = $1
    )
    OR (
        EXISTS (SELECT 1 FROM channels WHERE id = t.channel_id AND membership_mode = 1)
        AND EXISTS (
            SELECT 1 FROM channels c
            WHERE c.id = t.channel_id AND (
                c.parent_agent_id IN (SELECT ch.account_id FROM chain ch)
                OR $1 = (SELECT aa.owner_user_id FROM agent_accounts aa
                         WHERE aa.account_id = c.parent_agent_id)
            )
        )
    )
  )
ORDER BY m.seq DESC
LIMIT $4;
-- name: SearchMessages :many
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
      AND EXISTS (
          SELECT 1 FROM channels c
          WHERE c.membership_mode = 1 AND ($3 = '' OR c.id = $3)
      )
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
)
SELECT m.id, m.topic_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, t.channel_id, m.turn_sequence
FROM messages m
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
JOIN topics t ON t.id = m.topic_id
WHERE m.search_tsv @@ websearch_to_tsquery('english', $2)
  AND ($3 = '' OR t.channel_id = $3)
  AND ($5 = 0 OR m.seq <= $5)
  AND (
    EXISTS (
        SELECT 1 FROM channel_members cm
        WHERE cm.channel_id = t.channel_id AND cm.account_id = $1
    )
    OR (
        EXISTS (SELECT 1 FROM channels WHERE id = t.channel_id AND membership_mode = 1)
        AND EXISTS (
            SELECT 1 FROM channels c
            WHERE c.id = t.channel_id AND (
                c.parent_agent_id IN (SELECT ch.account_id FROM chain ch)
                OR $1 = (SELECT aa.owner_user_id FROM agent_accounts aa
                         WHERE aa.account_id = c.parent_agent_id)
            )
        )
    )
  )
ORDER BY ts_rank(m.search_tsv, websearch_to_tsquery('english', $2)) DESC, m.seq DESC
LIMIT $4;

-- name: FindAskMessage :many
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = $1
      AND EXISTS (SELECT 1 FROM channels WHERE membership_mode = 1)
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
)
SELECT m.id, m.topic_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence
FROM messages m
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
JOIN topics t ON t.id = m.topic_id
WHERE m.blocks @> $2::jsonb
  AND (
    EXISTS (
        SELECT 1 FROM channel_members cm
        WHERE cm.channel_id = t.channel_id AND cm.account_id = $1
    )
    OR (
        EXISTS (SELECT 1 FROM channels WHERE id = t.channel_id AND membership_mode = 1)
        AND EXISTS (
            SELECT 1 FROM channels c
            WHERE c.id = t.channel_id AND (
                c.parent_agent_id IN (SELECT ch.account_id FROM chain ch)
                OR $1 = (SELECT aa.owner_user_id FROM agent_accounts aa
                         WHERE aa.account_id = c.parent_agent_id)
            )
        )
    )
  )
FOR UPDATE OF m;

-- name: GetMessageByRequestID :many
SELECT m.id, m.topic_id, m.author_account_id, (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '') WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle, m.at_unix_ms, m.blocks, m.turn_sequence
FROM messages m
LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
WHERE m.author_account_id = $1 AND m.client_request_id = $2;
