-- Agent-session queries (sqlc adoption T5, RIG-3034). These replace the inline
-- SQL literals that lived in internal/store/agent_sessions.go; the hand-written
-- Store methods keep their exact signatures and wrap these generated calls,
-- mapping AccountID newtypes and the not-found/forbidden merge (D9) by hand. No
-- domain xFromRow mapper — the session reads project scalar columns the methods
-- return directly.

-- name: InsertAgentSession :exec
INSERT INTO agent_sessions (session_id, agent_account_id, recorded_at_unix_ms)
VALUES ($1, $2, $3);

-- name: LatestSessionForAccount :one
SELECT session_id
  FROM agent_sessions
 WHERE agent_account_id = $1
 ORDER BY recorded_at_unix_ms DESC, session_id DESC
 LIMIT 1;

-- name: RequireAgentSessionSubscriber :one
SELECT EXISTS (
         SELECT 1
           FROM agent_sessions se
           JOIN agent_accounts ag ON ag.account_id = se.agent_account_id
           JOIN channel_members cm ON cm.channel_id = ag.home_channel_id
                                  AND cm.account_id = $2
          WHERE se.session_id = $1);
