-- 0004_issues_search adds weighted full-text search to the issue board.
-- 0001_init.sql is frozen; this append-only migration grants the helper to both
-- roles and relies on 0001's existing issue tenant-isolation policy.

-- CREATE OR REPLACE does not recompute stored rows; changing output needs a backfill.
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
