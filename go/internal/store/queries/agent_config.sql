-- Agent config-bundle queries (sqlc adoption T5, RIG-3034). These replace the
-- inline SQL literals in internal/store/agent_config.go; the hand-written Store
-- methods keep their signatures and own the bundle validation/hash and the
-- tar-walk member inventory (Go-side, over the returned BYTEA). The config
-- bundle is a fleet-wide singleton row (singleton = TRUE).

-- name: PutAgentConfig :exec
INSERT INTO agent_config_bundle (singleton, version, bundle)
VALUES (TRUE, $1, $2)
ON CONFLICT (singleton)
DO UPDATE SET version = EXCLUDED.version, bundle = EXCLUDED.bundle;

-- name: CurrentAgentConfig :one
SELECT version, bundle FROM agent_config_bundle WHERE singleton = TRUE;

-- name: DeleteAgentConfig :exec
DELETE FROM agent_config_bundle WHERE singleton = TRUE;

-- name: PutAgentConfigOverlay :exec
INSERT INTO agent_config_overlays (tenant_id, owner_user_id, role, version, bundle, declared_by)
VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6)
ON CONFLICT (tenant_id) WHERE owner_user_id IS NULL
DO UPDATE SET version = EXCLUDED.version, bundle = EXCLUDED.bundle, declared_by = EXCLUDED.declared_by;

-- name: PutOwnedAgentConfigOverlay :exec
INSERT INTO agent_config_overlays (tenant_id, owner_user_id, role, version, bundle, declared_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, owner_user_id, role) WHERE owner_user_id IS NOT NULL
DO UPDATE SET version = EXCLUDED.version, bundle = EXCLUDED.bundle, declared_by = EXCLUDED.declared_by;

-- name: DeleteAgentConfigTenantOverlay :execrows
DELETE FROM agent_config_overlays WHERE tenant_id = $1 AND owner_user_id IS NULL;

-- name: DeleteAgentConfigOwnedOverlay :execrows
DELETE FROM agent_config_overlays WHERE tenant_id = $1 AND owner_user_id = $2 AND role = $3;

-- name: AgentConfigOverlay :one
SELECT version, bundle FROM agent_config_overlays
WHERE tenant_id = $1 AND owner_user_id IS NOT DISTINCT FROM NULLIF($2, '') AND role = $3;

-- name: AgentConfigOverlayLocks :one
SELECT bundle FROM agent_config_overlays
WHERE tenant_id = $1 AND owner_user_id IS NULL AND role = '';

-- name: EffectiveAgentConfigLayers :many
WITH target_agent AS (
    SELECT aa.account_id, aa.owner_user_id, aa.role
    FROM agent_accounts aa
    JOIN accounts a ON a.id = aa.account_id
    WHERE aa.account_id = $1 AND a.tenant_id = $2
), config_rows AS (
    SELECT 1::smallint AS layer, c.version, c.bundle
    FROM target_agent ta CROSS JOIN agent_config_bundle c
    WHERE c.singleton
    UNION ALL
    SELECT CASE WHEN o.owner_user_id IS NULL THEN 2 WHEN o.role = '' THEN 3 ELSE 4 END::smallint AS layer,
           o.version, o.bundle
    FROM target_agent ta
	JOIN agent_config_overlays o ON o.tenant_id = $2
	  AND (o.owner_user_id IS NULL OR (o.owner_user_id = ta.owner_user_id AND (o.role = '' OR (ta.role IN ('supervisor', 'owner', 'manager') AND o.role = ta.role))))
)
SELECT layer, version, bundle FROM config_rows
UNION ALL
SELECT 0::smallint AS layer, ''::text AS version, ''::bytea AS bundle
FROM target_agent
WHERE NOT EXISTS (SELECT 1 FROM config_rows)
ORDER BY layer;

-- name: EffectiveAgentConfigLayerVersions :one
SELECT COALESCE((SELECT version::text FROM agent_config_bundle WHERE singleton), '')::text AS default_version,
	COALESCE((SELECT o.version::text FROM agent_config_overlays o
				 WHERE o.tenant_id = $2 AND o.owner_user_id IS NULL AND o.role = ''), '')::text AS tenant_version,
	COALESCE((SELECT o.version::text FROM agent_config_overlays o
				 WHERE o.tenant_id = $2 AND o.owner_user_id = a.owner_user_id AND o.role = ''), '')::text AS user_version,
	COALESCE((SELECT o.version::text FROM agent_config_overlays o
				 WHERE o.tenant_id = $2 AND o.owner_user_id = a.owner_user_id AND o.role = a.role AND a.role IN ('supervisor', 'owner', 'manager')), '')::text AS role_version
FROM agent_accounts a
JOIN accounts ac ON ac.id = a.account_id
WHERE a.account_id = $1 AND ac.tenant_id = $2;
