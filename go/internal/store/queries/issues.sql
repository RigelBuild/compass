-- Issue-domain queries (sqlc adoption T6, RIG-3034). These replace the inline
-- SQL literals in internal/store/issues.go; the hand-written Store methods keep
-- their signatures, the door-side validation, the ErrNotFound/ErrInvalidArgument
-- mapping, and the RowsAffected branch (SetIssueState is :execrows). GetIssue /
-- ListIssues feed issueFromColumns (via issueFromGetRow / issueFromListRow),
-- which maps the generated row (forge_provider/state ints, number BIGINT) back
-- to the domain Issue.

-- name: UpsertIssueForgeFields :one
-- Insert-or-update at the forge coordinate with the OQ-6(a) recency guard; the
-- ON CONFLICT sets ONLY forge columns (never state/machinery), and the CTE's
-- fallback SELECT keeps the returned id stable when the guard skips the UPDATE.
WITH up AS (
     INSERT INTO issues
         (id, forge_provider, forge_host, repo, number,
          title, body, forge_state, url, forge_account, labels, agent_handle,
          forge_updated_at)
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
     ON CONFLICT (tenant_id, forge_provider, forge_host, repo, number) DO UPDATE
        SET title = EXCLUDED.title, body = EXCLUDED.body,
            forge_state = EXCLUDED.forge_state, url = EXCLUDED.url,
            forge_account = EXCLUDED.forge_account, labels = EXCLUDED.labels,
            agent_handle = EXCLUDED.agent_handle,
            forge_updated_at = EXCLUDED.forge_updated_at
      WHERE issues.forge_updated_at IS NULL
         OR EXCLUDED.forge_updated_at IS NULL
         OR EXCLUDED.forge_updated_at >= issues.forge_updated_at
     RETURNING id
 )
 SELECT id FROM up
 UNION ALL
 SELECT id FROM issues
  WHERE NOT EXISTS (SELECT 1 FROM up)
    AND forge_provider = $2 AND forge_host = $3 AND repo = $4 AND number = $5
 LIMIT 1;

-- name: SetIssueState :execrows
UPDATE issues SET state = $2 WHERE id = $1;

-- name: GetIssue :one
SELECT id, forge_provider, forge_host, repo, number,
       title, body, forge_state, url, forge_account, labels, agent_handle,
       state, priority, assignee, summary, branch
FROM issues
WHERE id = $1;

-- name: ListIssues :many
SELECT id, forge_provider, forge_host, repo, number,
       title, body, forge_state, url, forge_account, labels, agent_handle,
       state, priority, assignee, summary, branch
FROM issues
ORDER BY id;
