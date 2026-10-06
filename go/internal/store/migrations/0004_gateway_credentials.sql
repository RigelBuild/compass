-- Credential values stay sealed so raw rows cannot expose provider secrets.
CREATE TABLE gateway_credentials (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    provider           TEXT NOT NULL CHECK (provider <> ''),
    scope              SMALLINT NOT NULL CHECK (scope IN (1, 2)),
    owner_user_id      TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    kind               SMALLINT NOT NULL CHECK (kind IN (1, 2)),
    version            BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    expires_at_unix_ms BIGINT NOT NULL DEFAULT 0,
    value_ciphertext   BYTEA NOT NULL,
    value_nonce        BYTEA NOT NULL,
    key_version        SMALLINT NOT NULL DEFAULT 1,
    disabled_at        TIMESTAMPTZ,
    disabled_cause     TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((scope = 1 AND owner_user_id IS NOT NULL) OR (scope = 2 AND owner_user_id IS NULL))
);

CREATE INDEX gateway_credentials_tenant_provider_idx ON gateway_credentials (tenant_id, provider);
CREATE INDEX gateway_credentials_owner_idx ON gateway_credentials (owner_user_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON gateway_credentials TO compass_app;
-- Schema-wide grants include compass_system; sealed credentials need no system-role path.
REVOKE ALL ON gateway_credentials FROM compass_system;

ALTER TABLE gateway_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE gateway_credentials FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON gateway_credentials
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));

CREATE TRIGGER set_updated_at BEFORE UPDATE ON gateway_credentials
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
