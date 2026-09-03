-- Model-registry queries (RIG-3122 P2). Back the hand-written Store methods in
-- internal/store/model_registry.go, which own the fail-closed payload
-- validation (ValidateModelRegistry), the JSONB marshal/unmarshal, and the
-- ErrVersionConflict/ErrNotFound mapping. The registry is a fleet-wide singleton
-- row (singleton = TRUE) with a monotonic version supplying the CAS substrate:
-- a write only lands if the row still holds the version the caller read.

-- name: CurrentModelRegistry :one
SELECT version, registry FROM model_registry WHERE singleton = TRUE;

-- InsertModelRegistry seeds the FIRST registry (the caller read no row, expected
-- version 0). ON CONFLICT DO NOTHING makes it a CAS: it lands only when the
-- singleton is still absent, so a racing seed loses (zero rows, ErrNoRows via
-- RETURNING) rather than clobbering the winner. The seeded version is 1.
-- name: InsertModelRegistry :one
INSERT INTO model_registry (singleton, version, registry)
VALUES (TRUE, 1, $1)
ON CONFLICT (singleton) DO NOTHING
RETURNING version;

-- UpdateModelRegistry is the compare-and-set write over an existing row: it
-- lands only when the row still holds $2 (the version the caller read), bumping
-- to version + 1 and returning the new version. A stale/racing expected version
-- matches no row (ErrNoRows via RETURNING) — the caller maps that to
-- ErrVersionConflict.
-- name: UpdateModelRegistry :one
UPDATE model_registry
   SET registry = $1, version = version + 1, updated_at = now()
 WHERE singleton = TRUE AND version = $2
RETURNING version;

-- name: DeleteModelRegistry :exec
DELETE FROM model_registry WHERE singleton = TRUE;
