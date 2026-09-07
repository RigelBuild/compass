-- Session-binding queries (RIG-3108 / RIG-2861 §T4): the durable
-- (session -> agent account, Runner) binding the RunnerHub has so far held only
-- in RAM. The hand-written Store methods in internal/store/session_bindings.go
-- keep their signatures and map these rows into the SessionBinding domain struct
-- (the AccountID newtype is done inline in the Go, as agent_placements does).
--
-- updated_at is NEVER assigned here: the set_updated_at() BEFORE UPDATE trigger
-- (0001_init.sql, RIG-3495) is the one mechanism, and a hand-written
-- `updated_at = now()` is the exact defect that convention removes.

-- name: RecordSessionBinding :exec
INSERT INTO session_bindings (session_id, agent_account_id, runner_id)
VALUES ($1, $2, $3)
ON CONFLICT (session_id) DO UPDATE
   SET agent_account_id = EXCLUDED.agent_account_id,
       runner_id        = EXCLUDED.runner_id;

-- name: SessionBindingAccount :one
SELECT agent_account_id FROM session_bindings WHERE session_id = $1;

-- name: SessionBindingForAccount :one
SELECT session_id FROM session_bindings WHERE agent_account_id = $1;

-- name: DeleteSessionBinding :exec
DELETE FROM session_bindings WHERE session_id = $1;

-- The reconnect sweep. Hub.enroll (internal/runnerhub/hub.go:905-957) clears
-- every binding when a Runner (re-)enrolls: a reconnecting Runner has no live
-- sessions, so a surviving binding would resolve a re-minted session id to a
-- stale account. A durable table does not forget on reconnect, so the sweep must
-- be explicit.
--
-- :many with RETURNING, deliberately NOT :exec. enroll snapshots the bindings
-- BEFORE clearing them (hub.go:912-928) because each cleared binding drives a
-- presence DISCONNECTED edge (RIG-1569 T8) and each cleared session id must be
-- reaped from the delivery held-deliver registry (RIG-1569 T3). A bare DELETE
-- would satisfy the invariant while silently dropping both side-effects, leaving
-- a long-WORKING agent stuck WORKING in the projection forever. RETURNING is
-- what preserves them, so the returned rows are load-bearing, not diagnostic.
-- A DELETE ... RETURNING takes no ORDER BY, so the Store method sorts the
-- returned slice by session id to keep a sweep pass deterministic and diffable.

-- name: DeleteSessionBindingsForRunner :many
DELETE FROM session_bindings
 WHERE runner_id = $1
RETURNING session_id, agent_account_id;
