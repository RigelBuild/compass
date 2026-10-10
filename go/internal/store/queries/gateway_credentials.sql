-- name: InsertGatewayCredential :exec
INSERT INTO gateway_credentials (
    id, provider, scope, owner_user_id, kind, version, expires_at_unix_ms,
    value_ciphertext, value_nonce, key_version
) VALUES (
    sqlc.arg(id), sqlc.arg(provider), sqlc.arg(scope), sqlc.narg(owner_user_id),
    sqlc.arg(kind), sqlc.arg(version), sqlc.arg(expires_at_unix_ms),
    sqlc.arg(value_ciphertext), sqlc.arg(value_nonce), sqlc.arg(key_version)
);

-- name: GatewayCredentialOwnerExists :one
SELECT EXISTS (
    SELECT 1 FROM user_accounts WHERE account_id = sqlc.arg(owner_user_id)
);

-- name: ListGatewayCredentials :many
SELECT sqlc.embed(gc)
FROM gateway_credentials gc
WHERE gc.disabled_at IS NULL
  AND (gc.scope = 2 OR gc.owner_user_id = sqlc.arg(owner_user_id))
  AND (sqlc.arg(provider)::text = '' OR gc.provider = sqlc.arg(provider)::text)
  AND (
      NOT sqlc.arg(pool_only)::boolean
      OR gc.scope = 1
      OR NOT EXISTS (
          SELECT 1
          FROM gateway_credentials own
          WHERE own.disabled_at IS NULL
            AND own.scope = 1
            AND own.owner_user_id = sqlc.arg(owner_user_id)
            AND own.provider = gc.provider
      )
  )
ORDER BY gc.provider ASC, gc.created_at ASC, gc.id ASC;

-- name: GetGatewayCredentialForUpdate :one
SELECT id, provider, scope, owner_user_id, kind, version, expires_at_unix_ms,
       value_ciphertext, value_nonce, key_version, disabled_at, disabled_cause,
       created_at, updated_at
FROM gateway_credentials
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: UpdateGatewayOAuthCredential :execrows
UPDATE gateway_credentials
SET value_ciphertext = sqlc.arg(value_ciphertext), value_nonce = sqlc.arg(value_nonce),
    key_version = sqlc.arg(key_version), expires_at_unix_ms = sqlc.arg(expires_at_unix_ms),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND disabled_at IS NULL;

-- name: DisableGatewayCredential :execrows
UPDATE gateway_credentials
SET disabled_at = sqlc.arg(disabled_at), disabled_cause = sqlc.arg(disabled_cause),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND disabled_at IS NULL;

-- name: GatewayAgentTenant :one
SELECT tenant_id
FROM agent_accounts
WHERE account_id = sqlc.arg(account_id);
