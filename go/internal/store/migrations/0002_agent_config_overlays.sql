-- Tenant, user and role config overlays: current-only whole bundles. A NULL owner
-- is the tenant layer; a non-NULL owner scopes the user and role layers.
CREATE TABLE agent_config_overlays (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
                  REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_user_id TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    role          TEXT NOT NULL DEFAULT '',
    version       TEXT NOT NULL,
    bundle        BYTEA NOT NULL,
    declared_by   TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_config_overlays_role_owner CHECK (role = '' OR owner_user_id IS NOT NULL)
);
CREATE UNIQUE INDEX agent_config_overlays_tenant_key
    ON agent_config_overlays (tenant_id) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX agent_config_overlays_owner_key
    ON agent_config_overlays (tenant_id, owner_user_id, role) WHERE owner_user_id IS NOT NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON agent_config_overlays TO compass_app, compass_system;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE agent_config_overlays_id_seq TO compass_app, compass_system;

ALTER TABLE agent_config_overlays ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_config_overlays FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_config_overlays
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));

CREATE TRIGGER set_updated_at BEFORE UPDATE ON agent_config_overlays
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
