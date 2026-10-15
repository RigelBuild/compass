-- Agent status board reads and writes.

-- name: LockBoardAgent :one
-- NO KEY UPDATE serializes board writers without blocking FK inserts on the agent.
SELECT account_id FROM agent_accounts WHERE account_id = $1 FOR NO KEY UPDATE;

-- name: UpsertAgentBoard :exec
INSERT INTO agent_boards
    (agent_account_id, focus_task_key, halt_kind, halt_ref, halt_reason,
     written_turn, written_at_unix_ms)
VALUES ($1, CASE WHEN $2::boolean THEN $3::text ELSE '' END,
        CASE WHEN $4::boolean THEN $5::smallint ELSE 0 END,
        CASE WHEN $4::boolean THEN $6::text ELSE '' END,
        CASE WHEN $4::boolean THEN $7::text ELSE '' END,
        CASE WHEN $9::boolean THEN 0 ELSE $10::bigint END,
        CASE WHEN $9::boolean THEN 0 ELSE $11::bigint END)
ON CONFLICT (agent_account_id) DO UPDATE
   SET focus_task_key = CASE WHEN $2::boolean THEN $3::text ELSE agent_boards.focus_task_key END,
       halt_kind = CASE WHEN $8::boolean THEN 0
                        WHEN $4::boolean THEN $5::smallint
                        ELSE agent_boards.halt_kind END,
       halt_ref = CASE WHEN $8::boolean THEN ''
                       WHEN $4::boolean THEN $6::text
                       ELSE agent_boards.halt_ref END,
       halt_reason = CASE WHEN $8::boolean THEN ''
                          WHEN $4::boolean THEN $7::text
                          ELSE agent_boards.halt_reason END,
       written_turn = CASE WHEN $9::boolean THEN agent_boards.written_turn ELSE $10::bigint END,
       written_at_unix_ms = CASE WHEN $9::boolean THEN agent_boards.written_at_unix_ms ELSE $11::bigint END;

-- name: UpsertAgentBoardTask :exec
INSERT INTO agent_board_tasks
    (agent_account_id, task_key, title, primary_ref, refs, state, note,
     waiting, subagents, position, written_at_unix_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (agent_account_id, task_key) DO UPDATE
   SET title = EXCLUDED.title,
       primary_ref = EXCLUDED.primary_ref,
       refs = EXCLUDED.refs,
       state = EXCLUDED.state,
       note = EXCLUDED.note,
       waiting = EXCLUDED.waiting,
       subagents = EXCLUDED.subagents,
       position = EXCLUDED.position,
       written_at_unix_ms = EXCLUDED.written_at_unix_ms;

-- name: DeleteAgentBoardTasks :exec
DELETE FROM agent_board_tasks
 WHERE agent_account_id = $1 AND task_key = ANY($2::text[]);

-- name: GetAgentBoards :many
SELECT agent_account_id, focus_task_key, halt_kind, halt_ref, halt_reason,
       written_turn, written_at_unix_ms
  FROM agent_boards
 WHERE agent_account_id = ANY($1::text[]);

-- name: GetAgentBoardTasks :many
SELECT agent_account_id, task_key, title, primary_ref, refs, state, note,
       waiting, subagents, position, written_at_unix_ms
  FROM agent_board_tasks
 WHERE agent_account_id = ANY($1::text[])
 ORDER BY position, task_key;

-- name: SearchAgentBoardTasks :many
WITH filtered_tasks AS (
    SELECT t.agent_account_id, t.task_key, t.position
      FROM agent_board_tasks t
      JOIN agent_boards b ON b.agent_account_id = t.agent_account_id
     WHERE (cardinality(@agent_account_ids::text[]) = 0 OR t.agent_account_id = ANY(@agent_account_ids::text[]))
       AND (@query::text = '' OR to_tsvector('english', t.title || ' ' || t.note)
            @@ websearch_to_tsquery('english', @query::text))
       AND (@ref::text = '' OR @ref::text = ANY(t.refs) OR t.primary_ref = @ref::text)
       AND (@waiting_on::smallint = 0 OR EXISTS (
            SELECT 1 FROM jsonb_array_elements(t.waiting) AS w(value)
             WHERE w.value->>'on' = CASE @waiting_on::smallint
                    WHEN 1 THEN 'BOARD_WAITING_ON_OPERATOR'
                    WHEN 2 THEN 'BOARD_WAITING_ON_AGENT'
                    WHEN 3 THEN 'BOARD_WAITING_ON_EXTERNAL'
                    ELSE '' END))
       AND (@state::smallint = 0 OR t.state = @state::smallint)
       AND (@halt::smallint = 0 OR b.halt_kind = @halt::smallint)
),
matching_agents AS (
    SELECT DISTINCT agent_account_id
      FROM filtered_tasks
     ORDER BY agent_account_id
     LIMIT sqlc.arg(agent_limit)
)
SELECT f.agent_account_id, f.task_key
  FROM filtered_tasks f
  JOIN matching_agents a USING (agent_account_id)
 ORDER BY f.agent_account_id, f.position, f.task_key;

-- name: ListAsksByAuthor :many
-- The EXISTS body is ChannelVisibleTo's predicate with the channel spelled
-- t.channel_id, so it is diffable against the other copies.
WITH RECURSIVE ancestry AS (
    SELECT id, parent_group_id, visibility AS min_vis
      FROM channel_groups
    UNION ALL
    SELECT a.id, g.parent_group_id, LEAST(a.min_vis, g.visibility)
      FROM ancestry a
      JOIN channel_groups g ON g.id = a.parent_group_id
),
effective AS (
    SELECT id, MIN(min_vis) AS eff_vis
      FROM ancestry
     GROUP BY id
),
viewer AS (
    SELECT owner_user_id AS uid FROM agent_accounts WHERE account_id = $1
    UNION ALL
    SELECT $1 AS uid
),
asks AS (
    SELECT m.id, m.topic_id, m.seq, m.at_unix_ms, ask.value->'ask' AS a
      FROM messages m
      CROSS JOIN LATERAL jsonb_array_elements(m.blocks) AS ask(value)
     WHERE m.author_account_id = $2
       AND ask.value->>'kind' = 'ask'
),
answered_topics AS (
    SELECT DISTINCT topic_id
      FROM asks
     WHERE COALESCE((a->>'answered')::boolean, FALSE)
),
-- AnswerAsk posts the answer in the ask's topic, after the ask.
candidate_messages AS MATERIALIZED (
    SELECT answer.topic_id, answer.seq, answer.at_unix_ms, answer.blocks
      FROM messages answer
      WHERE answer.topic_id IN (SELECT topic_id FROM answered_topics)
        AND answer.at_unix_ms >= sqlc.arg(answered_since)::bigint
),
answer_candidates AS (
    SELECT answer.topic_id, answer.seq, answer.at_unix_ms,
           answer_block.value->'ask_answer'->'ask'->>'ask_id' AS ask_id
      FROM candidate_messages answer
      CROSS JOIN LATERAL jsonb_array_elements(answer.blocks) AS answer_block(value)
     WHERE answer_block.value->>'kind' = 'ask_answer'
),
answered AS (
    SELECT k.id, k.a->>'ask_id' AS ask_id, min(candidate.at_unix_ms) AS at_unix_ms
      FROM asks k
      JOIN answer_candidates candidate
        ON candidate.topic_id = k.topic_id AND candidate.seq > k.seq
       AND candidate.ask_id = k.a->>'ask_id'
     WHERE COALESCE((k.a->>'answered')::boolean, FALSE)
     GROUP BY k.id, k.a->>'ask_id'
)
SELECT k.id AS message_id, t.channel_id,
       k.at_unix_ms::bigint AS at_unix_ms,
       (k.a->>'ask_id')::text AS ask_id,
       COALESCE(k.a->>'ref', '')::text AS ref,
       COALESCE(NULLIF(k.a->'questions'->0->>'header', ''),
                left(COALESCE(k.a->'questions'->0->>'question', ''), 80))::text AS header,
       COALESCE((k.a->>'answered')::boolean, FALSE)::boolean AS answered
  FROM asks k
  JOIN topics t ON t.id = k.topic_id
  LEFT JOIN answered an ON an.id = k.id AND an.ask_id = k.a->>'ask_id'
 WHERE (COALESCE((k.a->>'answered')::boolean, FALSE) = FALSE
        OR an.at_unix_ms >= sqlc.arg(answered_since)::bigint)
   AND EXISTS (
       SELECT 1
         FROM channels c
        WHERE c.id = t.channel_id AND (
            EXISTS (
                SELECT 1 FROM channel_members cm
                 WHERE cm.channel_id = c.id AND cm.account_id = $1
            )
            OR (
                c.kind = 0 AND c.group_id IS NOT NULL AND EXISTS (
                    SELECT 1 FROM effective e WHERE e.id = c.group_id AND e.eff_vis = 1
                )
            )
            OR (
                c.parent_agent_id IS NOT NULL
                AND (SELECT aa.owner_user_id FROM agent_accounts aa WHERE aa.account_id = c.parent_agent_id)
                    IN (SELECT uid FROM viewer)
            )
        )
   )
 ORDER BY CASE WHEN COALESCE((k.a->>'answered')::boolean, FALSE) THEN 1 ELSE 0 END,
          COALESCE(an.at_unix_ms, k.at_unix_ms) DESC, k.seq DESC
 LIMIT sqlc.arg(row_limit);

-- name: ListOpenAuthoredRefs :many
SELECT a.forge_provider, a.forge_host, a.repo, a.kind, a.number
  FROM forge_authored_artifacts a
 WHERE a.agent_account_id = $1
   AND ((a.kind = 2 AND EXISTS (
            SELECT 1 FROM pull_requests p
             WHERE p.forge_provider = a.forge_provider AND p.forge_host = a.forge_host
               AND p.repo = CASE WHEN a.forge_provider = 1 THEN lower(a.repo) ELSE a.repo END
               AND p.number = a.number AND p.forge_state = 'open'))
        OR (a.kind = 1 AND EXISTS (
            SELECT 1 FROM issues i
             WHERE i.forge_provider = a.forge_provider AND i.forge_host = a.forge_host
               AND (CASE WHEN i.forge_provider = 1 THEN lower(i.repo) ELSE i.repo END) =
                   (CASE WHEN a.forge_provider = 1 THEN lower(a.repo) ELSE a.repo END)
               AND i.number = a.number AND i.forge_state = 'open')))
 ORDER BY a.created_at_unix_ms, a.forge_provider, a.forge_host, a.repo, a.kind, a.number;

-- name: PullRequestsByCoords :many
WITH want AS (
    SELECT unnest(@forge_providers::smallint[]) AS provider,
           unnest(@forge_hosts::text[]) AS host,
           unnest(@forge_repos::text[]) AS repo,
           unnest(@forge_numbers::bigint[]) AS number
)
SELECT p.forge_provider, p.forge_host, p.repo, p.number, p.forge_state,
       p.forge_created_at, p.forge_updated_at, p.pr
  FROM pull_requests p
  JOIN want w ON p.forge_provider = w.provider AND p.forge_host = w.host
             AND p.repo = w.repo
             AND p.number = w.number
 ORDER BY p.forge_provider, p.forge_host, p.repo, p.number;

-- name: IssuesByCoords :many
WITH want AS (
    SELECT unnest(@forge_providers::smallint[]) AS provider,
           unnest(@forge_hosts::text[]) AS host,
           unnest(@forge_repos::text[]) AS repo,
           unnest(@forge_numbers::bigint[]) AS number
)
SELECT i.id, i.forge_provider, i.forge_host, i.repo, i.number,
       i.title, i.body, i.forge_state, i.url, i.forge_account, i.labels, i.agent_handle,
       i.state, i.priority, i.assignee, i.summary, i.branch
  FROM issues i
  JOIN want w ON i.forge_provider = w.provider AND i.forge_host = w.host
             AND (CASE WHEN i.forge_provider = 1 THEN lower(i.repo) ELSE i.repo END) = w.repo
             AND i.number = w.number
 ORDER BY i.forge_provider, i.forge_host, i.repo, i.number;
