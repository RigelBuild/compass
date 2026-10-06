-- Agent-attached channels: the anchor edge, the membership mode, and per-account
-- subscription overrides for tree-membered channels.

SET LOCAL lock_timeout = '5s';

-- channels is small and the new column is all NULL, so each validating scan is
-- brief and lock_timeout bounds the wait; NOT VALID would only defer that scan.
ALTER TABLE channels
    ADD COLUMN parent_agent_id TEXT
        -- squawk-ignore adding-foreign-key-constraint
        REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    ADD COLUMN membership_mode SMALLINT NOT NULL DEFAULT 0
        CHECK (membership_mode IN (0, 1)),
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT channels_group_xor_agent
        CHECK (group_id IS NULL OR parent_agent_id IS NULL),
    -- squawk-ignore constraint-missing-not-valid
    ADD CONSTRAINT channels_tree_mode_needs_agent
        CHECK (membership_mode = 0 OR parent_agent_id IS NOT NULL);
CREATE INDEX channels_parent_agent_idx ON channels (parent_agent_id);
CREATE UNIQUE INDEX channels_agent_name_key
    ON channels (parent_agent_id, name) WHERE parent_agent_id IS NOT NULL;
CREATE TABLE channel_subscriptions (
    channel_id TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    subscribed BOOLEAN NOT NULL DEFAULT FALSE,
    tenant_id  TEXT NOT NULL
        DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (channel_id, account_id)
);
-- The account-first index serves delivery lookups keyed by account.
CREATE INDEX channel_subscriptions_account_idx
    ON channel_subscriptions (account_id);
-- New tables do not inherit the bootstrap migration's RLS or grants.
ALTER TABLE channel_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON channel_subscriptions
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));
GRANT SELECT, INSERT, UPDATE, DELETE ON channel_subscriptions
    TO compass_app, compass_system;
