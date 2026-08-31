-- Agent config-bundle queries (sqlc adoption T5, RIG-3034). These replace the
-- inline SQL literals in internal/store/agent_config.go; the hand-written Store
-- methods keep their signatures and own the bundle validation/hash and the
-- tar-walk member inventory (Go-side, over the returned BYTEA). The config
-- bundle is a fleet-wide singleton row (singleton = TRUE).

-- name: PutAgentConfig :exec
INSERT INTO agent_config_bundle (singleton, version, bundle)
VALUES (TRUE, $1, $2)
ON CONFLICT (singleton)
DO UPDATE SET version = EXCLUDED.version, bundle = EXCLUDED.bundle, updated_at = now();

-- name: CurrentAgentConfig :one
SELECT version, bundle FROM agent_config_bundle WHERE singleton = TRUE;

-- name: DeleteAgentConfig :exec
DELETE FROM agent_config_bundle WHERE singleton = TRUE;
