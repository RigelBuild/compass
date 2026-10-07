-- account_forge_scopes is an owner-managed allowlist for forge write coordinates.
CREATE TABLE account_forge_scopes (
    tenant_id     TEXT     NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    account_id    TEXT     NOT NULL REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host    TEXT     NOT NULL,
    repo          TEXT     NOT NULL CHECK (repo <> ''),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, account_id, forge_provider, forge_host, repo)
);

CREATE INDEX account_forge_scopes_coordinate_idx
    ON account_forge_scopes (tenant_id, forge_provider, forge_host, account_id);

GRANT SELECT, INSERT, DELETE ON account_forge_scopes TO compass_app, compass_system;

DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY['account_forge_scopes'];
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
