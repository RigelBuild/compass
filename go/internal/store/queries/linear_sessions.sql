-- Linear-agent-session queries (sqlc adoption T6, RIG-3034). These replace the
-- inline SQL literals in internal/store/linear_sessions.go; the hand-written
-- Store methods keep their signatures, the RowsAffected branch (Upsert returns
-- created via :execrows), the textOrNull nullable issue fields, and the
-- ErrNotFound/ErrInvalidArgument mapping. The LinearAgentSession read maps the
-- generated nullable fields back to LinearAgentSessionRow inline.

-- name: UpsertLinearAgentSession :execrows
INSERT INTO linear_agent_sessions
    (linear_session_id, manager_account_id, channel_id, topic_id, linear_issue_id, linear_issue_identifier)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (linear_session_id) DO NOTHING;

-- name: LinearAgentSession :one
SELECT linear_session_id, manager_account_id, channel_id, topic_id, linear_issue_id, linear_issue_identifier, created_at
FROM linear_agent_sessions
WHERE linear_session_id = $1;
