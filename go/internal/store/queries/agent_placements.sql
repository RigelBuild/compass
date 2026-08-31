-- Agent-placement queries (sqlc adoption T5, RIG-3034). These replace the inline
-- SQL literals in internal/store/agent_placements.go; the hand-written Store
-- methods keep their signatures and map the placement rows into the
-- AgentPlacement domain struct (AccountID newtype done inline in the Go).

-- name: RecordAgentPlacement :exec
INSERT INTO agent_placements (agent_account_id, runner_id, container_name)
VALUES ($1, $2, $3)
ON CONFLICT (agent_account_id) DO UPDATE
   SET runner_id      = EXCLUDED.runner_id,
       container_name = EXCLUDED.container_name,
       updated_at     = now();

-- name: AgentForContainer :one
SELECT agent_account_id FROM agent_placements WHERE container_name = $1;

-- name: ListAgentPlacementsForRunner :many
SELECT agent_account_id, runner_id, container_name
  FROM agent_placements
 WHERE runner_id = $1
 ORDER BY agent_account_id;

-- name: DeleteAgentPlacement :exec
DELETE FROM agent_placements WHERE container_name = $1;

-- name: PlacementForAgent :one
SELECT runner_id, container_name FROM agent_placements WHERE agent_account_id = $1;
