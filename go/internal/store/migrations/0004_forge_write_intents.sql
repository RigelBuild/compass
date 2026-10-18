-- A write intent is one create attempt. Failed attempts may retry with the same
-- client key; every other state is write-once, and PR head ownership is unique
-- while pending or complete.
CREATE TABLE forge_write_intents (
    id                TEXT        NOT NULL,
    forge_provider    SMALLINT    NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host        TEXT        NOT NULL,
    repo              TEXT        NOT NULL,
    kind              SMALLINT    NOT NULL CHECK (kind IN (1, 2, 3, 4)),
    parent_number     BIGINT      NOT NULL DEFAULT 0,
    head_ref          TEXT        NOT NULL DEFAULT '',
    agent_account_id  TEXT        NOT NULL,
    owner_user_id     TEXT        NOT NULL,
    session_id        TEXT        NOT NULL,
    client_request_id TEXT        NOT NULL CHECK (client_request_id <> ''),
    forge_key         TEXT        NOT NULL DEFAULT '',
    state             TEXT        NOT NULL CHECK (state IN ('pending', 'done', 'failed', 'lost')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id         TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (agent_account_id, owner_user_id)
        REFERENCES agent_accounts (account_id, owner_user_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX forge_write_intents_request_key_idx
    ON forge_write_intents (tenant_id, agent_account_id, client_request_id)
    WHERE state <> 'failed';

CREATE UNIQUE INDEX forge_write_intents_pr_head_idx
    ON forge_write_intents (tenant_id, forge_provider, forge_host, repo, head_ref)
    WHERE kind = 2 AND state IN ('pending', 'done');

CREATE INDEX forge_write_intents_pending_idx
    ON forge_write_intents (created_at, id)
    WHERE state = 'pending';

GRANT SELECT, INSERT, UPDATE, DELETE ON forge_write_intents TO compass_app, compass_system;

ALTER TABLE forge_write_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE forge_write_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON forge_write_intents
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));
