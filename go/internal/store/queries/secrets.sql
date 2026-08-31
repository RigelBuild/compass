-- Secrets-registry queries (sqlc adoption T6, RIG-3034). These replace the inline
-- SQL literals in internal/store/secrets.go; the hand-written Store methods keep
-- their signatures, the door-side validation (name grammar, kind routing), the
-- ErrConflict/ErrInvalidArgument/ErrNotFound mapping, and the RowsAffected branch
-- (DeleteSecretDeclaration is :execrows). DeclaredSecrets maps the generated row
-- back to the domain SecretDeclaration (delivery/kind ints -> named types).

-- name: InsertSecret :exec
INSERT INTO secrets (name, delivery, kind, provider, host, declared_by)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: DeleteSecret :execrows
DELETE FROM secrets WHERE name = $1;

-- name: DeclaredSecrets :many
SELECT name, delivery, kind, provider, host, declared_by, created_at, updated_at
FROM secrets ORDER BY name;
