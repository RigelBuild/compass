CREATE TABLE forge_notices (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    from_account_id TEXT        NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    to_account_id   TEXT        NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    request_key     TEXT        NOT NULL CHECK (request_key <> ''),
    text            TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    UNIQUE (tenant_id, request_key)
);

CREATE INDEX forge_notices_undelivered_idx
    ON forge_notices (created_at, id)
    WHERE delivered_at IS NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON forge_notices TO compass_app, compass_system;

ALTER TABLE forge_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE forge_notices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON forge_notices
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));
