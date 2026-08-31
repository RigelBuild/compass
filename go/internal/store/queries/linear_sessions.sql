-- Linear-agent-session queries (sqlc adoption T6, RIG-3034). These replace the
-- inline SQL literals in internal/store/linear_sessions.go; the hand-written
-- Store methods keep their signatures, the RowsAffected branch (Upsert returns
-- created via :execrows), the textOrNull linear_issue_id NULL discipline, and the
-- ErrNotFound/ErrInvalidArgument mapping. The LinearAgentSession read maps the
-- generated row (nullable linear_issue_id, created_at timestamp) back to the
-- domain LinearAgentSessionRow inline.

-- name: UpsertLinearAgentSession :execrows
INSERT INTO linear_agent_sessions
    (linear_session_id, manager_account_id, channel_id, topic_id, linear_issue_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (linear_session_id) DO NOTHING;

-- name: LinearAgentSession :one
SELECT linear_session_id, manager_account_id, channel_id, topic_id, linear_issue_id, created_at
FROM linear_agent_sessions
WHERE linear_session_id = $1;
