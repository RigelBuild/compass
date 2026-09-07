-- Server-secrets registry queries (design record T0, mechanism C1/D6). The
-- SERVER-owned half of the names-only secret registry, physically separate from
-- `secrets` so the inject-all container delivery path can never see these rows.
-- The hand-written Store methods keep the door-side validation (name grammar,
-- reserved-prefix guard) and the ErrConflict/ErrInvalidArgument/ErrNotFound
-- mapping, mirroring the `secrets` methods minus delivery/kind/provider/host.
--
-- declared_by is NULLABLE here: a server-provisioned row (the master key) has no
-- human actor, so it is written as NULL rather than attributed to the
-- bootstrap-admin account.

-- name: InsertServerSecret :exec
INSERT INTO server_secrets (name, declared_by)
VALUES ($1, $2);

-- name: DeleteServerSecret :execrows
DELETE FROM server_secrets WHERE name = $1;

-- name: DeclaredServerSecrets :many
SELECT name, declared_by, created_at, updated_at
FROM server_secrets ORDER BY name;
