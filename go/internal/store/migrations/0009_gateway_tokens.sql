-- Per-agent LLM gateway bearer tokens. Only the SHA-256 of each token is stored,
-- so a row is never a usable credential.
--
-- RLS-exempt like tokens: the gateway verifies a token before any tenant is
-- known, so the row carries its own tenant and owner instead.
CREATE TABLE gateway_tokens (
    hash             BYTEA       PRIMARY KEY,
    agent_account_id TEXT        NOT NULL,
    -- Copied from the agent at mint; the composite FK keeps the pair real.
    owner_user_id    TEXT        NOT NULL,
    tenant_id        TEXT        NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at       TIMESTAMPTZ,
    -- RESTRICT pins revoked rows too: deleting an agent must purge its
    -- gateway_tokens first, and changing its owner fails closed.
    FOREIGN KEY (agent_account_id, owner_user_id)
        REFERENCES agent_accounts (account_id, owner_user_id) ON DELETE RESTRICT
);

-- At most one live token per agent, so a rotation that skipped the revoke fails.
CREATE UNIQUE INDEX gateway_tokens_live_agent_idx ON gateway_tokens (agent_account_id)
    WHERE revoked_at IS NULL;

-- 0001's schema-wide grant covered only the tables it created.
GRANT SELECT, INSERT, UPDATE, DELETE ON gateway_tokens TO compass_app, compass_system;
