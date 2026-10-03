-- 0004_issues_search: weighted full-text search over the board issue fields.
--
-- Migrations are append-only: 0001_init.sql is frozen, so this file adds the
-- generated search column and index here and grants the helper function to the
-- roles that 0001 created. No new tenant policy is needed; issues already has
-- 0001's forced tenant isolation policy.

-- CREATE OR REPLACE of this function does not recompute stored search_tsv
-- rows, so any change to its output needs an explicit issues backfill.
CREATE FUNCTION compass_labels_text(labels TEXT[]) RETURNS TEXT
    LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
    SET search_path = pg_catalog
    AS $$ SELECT array_to_string(labels, ' ') $$;

GRANT EXECUTE ON FUNCTION compass_labels_text(TEXT[]) TO compass_app, compass_system;

-- The STORED column rewrites issues once under ACCESS EXCLUSIVE. Accepted: a
-- generated column stays current on every upsert with no writer or trigger.
-- squawk-ignore adding-field-with-default
ALTER TABLE issues ADD COLUMN search_tsv TSVECTOR GENERATED ALWAYS AS (
    setweight(to_tsvector('english', title), 'A') ||
    setweight(to_tsvector('english', body), 'B') ||
    setweight(to_tsvector('english', summary), 'B') ||
    setweight(to_tsvector('english', compass_labels_text(labels)), 'C')
) STORED;

CREATE INDEX issues_search_idx ON issues USING gin (search_tsv);
