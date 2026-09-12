-- Forge state-transition memo queries (compass-forge-state-transition §Actor
-- attribution). The write chokepoint upserts one memo per forge coordinate
-- AFTER a successful agent-driven transition; the notify lane consumes it on
-- match to attribute the echoed STATE event to the acting agent. The
-- hand-written Store methods keep the door-side validation (validCoordinate),
-- the state-domain guard, and the ErrInvalidArgument mapping.

-- name: RecordStateTransition :exec
-- Latest transition wins: a re-transition of the same coordinate re-lands on the
-- PK and RESETS consumed_at to NULL, so the newest transition is attributable
-- even when the previous one was already consumed.
INSERT INTO forge_state_transitions
    (forge_provider, forge_host, repo, kind, number, state, agent_account_id, written_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, forge_provider, forge_host, repo, kind, number) DO UPDATE
   SET state            = EXCLUDED.state,
       agent_account_id = EXCLUDED.agent_account_id,
       written_at       = EXCLUDED.written_at,
       consumed_at      = NULL;

-- name: ConsumeStateTransition :one
-- Single-statement clear-and-return: the row is claimed and its actor returned
-- in ONE UPDATE, so a memo attributes at most one event and a concurrent second
-- reader matches nothing (consumed_at is no longer NULL). A state mismatch or a
-- memo written before the freshness bound matches nothing either — no actor,
-- which is the correct answer for every human/external transition.
UPDATE forge_state_transitions
   SET consumed_at = now()
 WHERE forge_provider = $1
   AND forge_host = $2
   AND repo = $3
   AND kind = $4
   AND number = $5
   AND state = $6
   AND consumed_at IS NULL
   AND written_at >= $7
RETURNING agent_account_id;
