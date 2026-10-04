-- Directed user approvals form a peering only when both users have a row.
-- The composite FK target. user_accounts holds one row per human user, so the
-- scan and lock are brief; migrations run in a transaction, so CONCURRENTLY is out.
-- squawk-ignore constraint-missing-not-valid,disallowed-unique-constraint
ALTER TABLE user_accounts ADD CONSTRAINT user_accounts_account_tenant_key UNIQUE (account_id, tenant_id);

CREATE TABLE user_peers (
    user_id      TEXT NOT NULL,
    peer_user_id TEXT NOT NULL,
    tenant_id    TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, peer_user_id),
    CHECK (user_id <> peer_user_id),
    FOREIGN KEY (user_id, tenant_id)      REFERENCES user_accounts (account_id, tenant_id) ON DELETE RESTRICT,
    FOREIGN KEY (peer_user_id, tenant_id) REFERENCES user_accounts (account_id, tenant_id) ON DELETE RESTRICT
);
CREATE INDEX user_peers_peer_idx ON user_peers (peer_user_id);

ALTER TABLE user_peers ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_peers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON user_peers
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));

-- Named per table: a schema-wide grant would re-grant privileges earlier
-- migrations deliberately revoked.
GRANT SELECT, INSERT, UPDATE, DELETE ON user_peers TO compass_app, compass_system;
