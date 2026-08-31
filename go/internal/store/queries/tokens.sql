-- Token-domain queries (sqlc adoption T6, RIG-3034). These replace the inline
-- SQL literals in internal/store/tokens.go; the hand-written Store methods keep
-- their signatures, the ErrConflict/ErrNotFound/ErrTokenRevoked mapping, and the
-- RowsAffected branching (RevokeToken is :execrows). ResolveTokenHash maps the
-- generated row (subject_kind/subject_id/revoked) back to the domain Subject.

-- name: InsertTokenHash :exec
INSERT INTO tokens (hash, subject_kind, subject_id) VALUES ($1, $2, $3);

-- name: ResolveTokenHash :one
SELECT subject_kind, subject_id, (revoked_at IS NOT NULL)::boolean AS revoked FROM tokens WHERE hash = $1;

-- name: RevokeToken :execrows
UPDATE tokens SET revoked_at = now() WHERE hash = $1 AND revoked_at IS NULL;

-- name: TokenHashExists :one
SELECT EXISTS (SELECT 1 FROM tokens WHERE hash = $1);
