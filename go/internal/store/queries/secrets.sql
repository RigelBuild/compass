-- Secrets-registry queries (sqlc adoption T6, RIG-3034). These back the
-- hand-written Store methods, which keep their signatures, the door-side
-- validation (name grammar, kind/routing, A9 scope shape), the
-- ErrConflict/ErrInvalidArgument/ErrNotFound mapping, and the RowsAffected
-- branch (DeleteSecretDeclaration is :execrows).
--
-- InsertSecret/DeclaredSecrets are the retained value-free path (T5 caller); the
-- scoped, encrypted path is UpsertSecret + SecretRecordsForAgent (A1/A9).

-- InsertSecret writes the value-free declaration at the tenant coordinate
-- (scope_kind 0, scope_id ''); the value columns stay NULL. Retained for the T5
-- SetSecret caller, removed with it in T5.
-- name: InsertSecret :exec
INSERT INTO secrets (name, scope_kind, delivery, kind, provider, host, declared_by)
VALUES ($1, 0, $2, $3, $4, $5, $6);

-- IsUserAccount reports whether an id names a human account — the user-scope
-- (scope_kind 1) referential check the UpsertSecret door runs in lieu of an FK
-- (A9). The agent-scope check reuses IsAgentAccount.
-- name: IsUserAccount :one
SELECT EXISTS (SELECT 1 FROM user_accounts WHERE account_id = $1);

-- UpsertSecret writes declaration+value in one row and, on a re-write of an
-- existing (name, scope_kind, scope_id), rewrites value/nonce/key_version and the
-- routing metadata. updated_at is maintained by the set_updated_at trigger, which
-- fires on the ON CONFLICT DO UPDATE path — never set here.
-- name: UpsertSecret :exec
INSERT INTO secrets (name, scope_kind, scope_id, delivery, kind, provider, host,
                     value_ciphertext, value_nonce, key_version, declared_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (name, scope_kind, scope_id) DO UPDATE SET
    value_ciphertext = EXCLUDED.value_ciphertext,
    value_nonce      = EXCLUDED.value_nonce,
    key_version      = EXCLUDED.key_version,
    delivery         = EXCLUDED.delivery,
    kind             = EXCLUDED.kind,
    provider         = EXCLUDED.provider,
    host             = EXCLUDED.host;

-- DeleteSecret addresses one scope coordinate — a name alone no longer
-- identifies a row (composite PK).
-- name: DeleteSecret :execrows
DELETE FROM secrets WHERE name = $1 AND scope_kind = $2 AND scope_id = $3;

-- DeclaredSecrets is the names-only view the SERVER SpecResolver's declarations
-- interface still consumes (value-free, all scopes).
-- name: DeclaredSecrets :many
SELECT name, delivery, kind, provider, host, declared_by, created_at, updated_at
FROM secrets ORDER BY name;

-- SecretRecordsForAgent collapses the A9 precedence in SQL: DISTINCT ON keeps the
-- first row per name under scope_kind DESC (agent 2 > user 1 > tenant 0), the
-- user tier reached through agent_accounts.owner_user_id. $1 is the calling
-- agent's account id. Ciphertext only — the store never decrypts.
-- name: SecretRecordsForAgent :many
SELECT DISTINCT ON (s.name) s.name, s.scope_kind, s.scope_id, s.delivery, s.kind,
       s.provider, s.host, s.value_ciphertext, s.value_nonce, s.key_version,
       s.declared_by, s.created_at, s.updated_at
  FROM secrets s
  JOIN agent_accounts a ON a.account_id = $1
 WHERE (s.scope_kind = 0 AND s.scope_id = '')
    OR (s.scope_kind = 1 AND s.scope_id = a.owner_user_id)
    OR (s.scope_kind = 2 AND s.scope_id = a.account_id)
 ORDER BY s.name, s.scope_kind DESC;
