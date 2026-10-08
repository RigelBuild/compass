-- Per-agent LLM gateway token queries. Rotation and revocation are scoped to the
-- caller's tenant; ResolveLiveGatewayToken runs before any tenant is known.

-- name: LockGatewayTokenAgent :one
SELECT owner_user_id, tenant_id FROM agent_accounts WHERE account_id = $1 FOR NO KEY UPDATE;

-- name: RevokeLiveGatewayToken :execrows
UPDATE gateway_tokens SET revoked_at = now()
WHERE agent_account_id = $1 AND tenant_id = $2 AND revoked_at IS NULL;

-- name: InsertGatewayToken :exec
INSERT INTO gateway_tokens (hash, agent_account_id, owner_user_id, tenant_id) VALUES ($1, $2, $3, $4);

-- name: ResolveLiveGatewayToken :one
SELECT agent_account_id, owner_user_id FROM gateway_tokens
WHERE hash = $1 AND revoked_at IS NULL;
