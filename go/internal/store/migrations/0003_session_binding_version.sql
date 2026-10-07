-- binding_version names one write of a session_bindings row. Every upsert sets a
-- fresh UUID, so a release conditioned on it cannot remove a later re-bind of the
-- same session id. Not xmin: transaction ids wrap around and repeat.
ALTER TABLE session_bindings
    ADD COLUMN binding_version TEXT NOT NULL DEFAULT '';

-- Give rows written before this column a real version too.
UPDATE session_bindings SET binding_version = gen_random_uuid()::TEXT
WHERE binding_version = '';
