-- Agent-activity queries (sqlc adoption T5, RIG-3034). These replace the inline
-- SQL literals in internal/store/agent_activity.go; the hand-written Store
-- methods keep their signatures and map the ActivityFor rows into the
-- AgentActivity domain struct (agentActivityFromRow-equivalent, done inline in
-- the Go — absent-from-table means absent-from-map).

-- name: SetActivity :exec
INSERT INTO agent_activity (agent_account_id, activity, activity_at_unix_ms)
VALUES ($1, $2, $3)
ON CONFLICT (agent_account_id)
DO UPDATE SET activity = EXCLUDED.activity,
              activity_at_unix_ms = EXCLUDED.activity_at_unix_ms;

-- name: ActivityFor :many
SELECT agent_account_id, activity, activity_at_unix_ms
FROM agent_activity
WHERE agent_account_id = ANY($1::text[]);
