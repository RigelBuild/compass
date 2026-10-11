-- Model-registry queries preserve a monotonic singleton version.

-- name: CurrentModelRegistry :one
SELECT version, registry FROM model_registry WHERE singleton = TRUE;

-- name: ModelRegistryVersion :one
SELECT version FROM model_registry WHERE singleton = TRUE;

-- InsertModelRegistry seeds or revives the singleton. A tombstone retains its
-- version, so reseeding after delete increments instead of reusing it.
-- name: InsertModelRegistry :one
INSERT INTO model_registry (singleton, version, registry)
VALUES (TRUE, 1, $1)
ON CONFLICT (singleton) DO UPDATE
   SET registry = EXCLUDED.registry, version = model_registry.version + 1
 WHERE model_registry.registry = 'null'::jsonb
RETURNING version;

-- UpdateModelRegistry applies a compare-and-set only to a live row.
-- name: UpdateModelRegistry :one
UPDATE model_registry
   SET registry = $1, version = version + 1
 WHERE singleton = TRUE AND registry <> 'null'::jsonb AND version = $2
RETURNING version;

-- Delete marks the row as unconfigured while retaining its monotonic version.
-- It matches only the version the orphan check read, so a racing Put conflicts.
-- name: DeleteModelRegistry :execrows
UPDATE model_registry
   SET registry = 'null'::jsonb, version = version + 1
 WHERE singleton = TRUE AND registry <> 'null'::jsonb AND version = $1;
