-- name: AskForForgeMirror :one
WITH RECURSIVE chain AS (
    SELECT aa.account_id, aa.parent_agent_id
    FROM agent_accounts aa
    WHERE aa.account_id = sqlc.arg(account_id)
    UNION
    SELECT a.account_id, a.parent_agent_id
    FROM agent_accounts a
    JOIN chain ch ON a.account_id = ch.parent_agent_id
), participating AS (
    SELECT cm.channel_id FROM channel_members cm WHERE cm.account_id = sqlc.arg(account_id)
    UNION
    SELECT c.id FROM channels c
    WHERE c.membership_mode = 1
      AND (
          c.parent_agent_id IN (SELECT account_id FROM chain)
          OR c.parent_agent_id IN (
              SELECT aa.account_id FROM agent_accounts aa WHERE aa.owner_user_id = sqlc.arg(account_id)
          )
      )
)
SELECT m.id, m.topic_id, m.author_account_id,
       (CASE WHEN ah.owner_user_id IS NULL THEN COALESCE(ah.handle, '')
             WHEN oh.handle IS NULL THEN '' ELSE oh.handle || '/' || ah.handle END)::text AS author_handle,
       m.at_unix_ms, m.blocks, m.turn_sequence, t.channel_id, c.name AS channel_name,
       t.name AS topic_name, COALESCE(answer.author_handle, '')::text AS answered_by
  FROM messages m
  LEFT JOIN account_handles ah ON ah.account_id = m.author_account_id
  LEFT JOIN account_handles oh ON oh.account_id = ah.owner_user_id
  JOIN topics t ON t.id = m.topic_id
  JOIN channels c ON c.id = t.channel_id
  JOIN participating p ON p.channel_id = t.channel_id
  LEFT JOIN LATERAL (
      SELECT (CASE WHEN answer_handle.owner_user_id IS NULL THEN COALESCE(answer_handle.handle, '')
                   WHEN answer_owner.handle IS NULL THEN ''
                   ELSE answer_owner.handle || '/' || answer_handle.handle END)::text AS author_handle
        FROM messages answer_message
        LEFT JOIN account_handles answer_handle ON answer_handle.account_id = answer_message.author_account_id
        LEFT JOIN account_handles answer_owner ON answer_owner.account_id = answer_handle.owner_user_id
       WHERE answer_message.topic_id = m.topic_id
         AND answer_message.blocks @> jsonb_build_array(jsonb_build_object(
             'kind', 'ask_answer',
             'ask_answer', jsonb_build_object('ask', jsonb_build_object('ask_id', sqlc.arg(ask_id)::text))
         ))
       ORDER BY answer_message.seq
       LIMIT 1
  ) answer ON TRUE
 WHERE m.blocks @> jsonb_build_array(jsonb_build_object(
    'kind', 'ask', 'ask', jsonb_build_object('ask_id', sqlc.arg(ask_id)::text)
 ))
 ORDER BY m.seq
 LIMIT 1;
