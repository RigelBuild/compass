-- Per-account first-run tour progress. The composite key scopes each account's
-- one-way first-run claim to its tenant.
CREATE TABLE account_tour_state (
    tenant_id  TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    account_id TEXT        NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    outcome    TEXT        NOT NULL CHECK (outcome IN ('started', 'dismissed', 'completed')),
    step_id    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, account_id)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON account_tour_state TO compass_app, compass_system;

DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY['account_tour_state'];
BEGIN
    FOREACH t IN ARRAY tenant_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($f$
            CREATE POLICY tenant_isolation ON %I
                USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
                       AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
                WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
                       AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
        $f$, t);
    END LOOP;
END $$;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON account_tour_state
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
