-- 0003_token_usage: the Plane-A token-usage store — the raw event log, its
-- hourly and daily rollups, and the global prune horizon.
--
-- Migrations are append-only: 0001_init.sql is frozen, so this file cannot add
-- the new tables to 0001's tenant_tables / updated_at_tables arrays. It repeats
-- those loops' bodies for its own tables instead, with the same policy text,
-- grants, and trigger. 0001 already created compass_app, compass_system, and
-- set_updated_at().

-- token_usage_events: the append-only raw log of upstream model calls the LLM
-- gateway reports, idempotent on the server-assigned id. Retention deletes old
-- rows; the rollups keep their sums. No FK to accounts: a log row outlives its
-- account.
CREATE TABLE token_usage_events (
    tenant_id          TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    id                 TEXT        NOT NULL,
    occurred_at        TIMESTAMPTZ NOT NULL,
    agent_account_id   TEXT        NOT NULL,
    owner_user_id      TEXT        NOT NULL,
    session_id         TEXT        NOT NULL,
    request_id         TEXT        NOT NULL,
    provider           TEXT        NOT NULL,
    model              TEXT        NOT NULL,
    credential_id      TEXT        NOT NULL,
    input_tokens       BIGINT      NOT NULL,
    output_tokens      BIGINT      NOT NULL,
    cache_read_tokens  BIGINT      NOT NULL,
    cache_write_tokens BIGINT      NOT NULL,
    total_tokens       BIGINT      NOT NULL,
    cost_micro_usd     BIGINT      NOT NULL,
    rate_version       TEXT        NOT NULL,
    outcome            TEXT        NOT NULL CHECK (outcome IN ('ok', 'error', 'aborted')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- The rebuild re-rolls one tenant's events from the prune horizon on.
CREATE INDEX token_usage_events_occurred_at_idx ON token_usage_events (tenant_id, occurred_at);

-- token_usage_rollups_hourly / _daily: the per-bucket sums of the events, keyed
-- like the in-memory reference. bucket_start is the UTC-aligned bucket start.
-- Rows outlive the events they sum, so a prune never touches them. bucket_start
-- follows tenant_id in the key because the series read and the rebuild range
-- over it.
CREATE TABLE token_usage_rollups_hourly (
    tenant_id          TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    bucket_start       TIMESTAMPTZ NOT NULL,
    owner_user_id      TEXT        NOT NULL,
    agent_account_id   TEXT        NOT NULL,
    provider           TEXT        NOT NULL,
    model              TEXT        NOT NULL,
    input_tokens       BIGINT      NOT NULL,
    output_tokens      BIGINT      NOT NULL,
    cache_read_tokens  BIGINT      NOT NULL,
    cache_write_tokens BIGINT      NOT NULL,
    total_tokens       BIGINT      NOT NULL,
    cost_micro_usd     BIGINT      NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket_start, owner_user_id, agent_account_id, provider, model)
);

CREATE TABLE token_usage_rollups_daily (
    tenant_id          TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    bucket_start       TIMESTAMPTZ NOT NULL,
    owner_user_id      TEXT        NOT NULL,
    agent_account_id   TEXT        NOT NULL,
    provider           TEXT        NOT NULL,
    model              TEXT        NOT NULL,
    input_tokens       BIGINT      NOT NULL,
    output_tokens      BIGINT      NOT NULL,
    cache_read_tokens  BIGINT      NOT NULL,
    cache_write_tokens BIGINT      NOT NULL,
    total_tokens       BIGINT      NOT NULL,
    cost_micro_usd     BIGINT      NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket_start, owner_user_id, agent_account_id, provider, model)
);

-- token_usage_prune_horizon: the one global row holding the UTC day the latest
-- prune cut at. Rollups before it may count pruned events, so a rebuild keeps
-- them. Not tenant-scoped, because a prune spans every tenant. It only moves
-- forward, and '-infinity' means no prune has run.
CREATE TABLE token_usage_prune_horizon (
    singleton  BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    horizon    TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO token_usage_prune_horizon (horizon) VALUES ('-infinity');

-- The table grant 0001's schema-wide GRANT gives every table it created. Named
-- per table here, because re-running 0001's ALL TABLES grant would hand
-- server_key_state its revoked DELETE back.
GRANT SELECT, INSERT, UPDATE, DELETE
    ON token_usage_events, token_usage_rollups_hourly, token_usage_rollups_daily,
       token_usage_prune_horizon
    TO compass_app, compass_system;

-- token_usage_prune_horizon holds the one row the migration inserts. Re-inserting
-- it at '-infinity' would let a rebuild drop pruned-day rollups; UPDATE stays.
REVOKE INSERT, DELETE ON token_usage_prune_horizon FROM compass_app, compass_system;

-- 0001's tenant_tables loop body, for this migration's tenant-owned tables.
DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY[
        'token_usage_events', 'token_usage_rollups_hourly', 'token_usage_rollups_daily'
    ];
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

-- 0001's updated_at_tables loop body, for this migration's updated_at tables.
DO $$
DECLARE
    t text;
    updated_at_tables text[] := ARRAY[
        'token_usage_rollups_hourly',
        'token_usage_rollups_daily',
        'token_usage_prune_horizon'
    ];
BEGIN
    FOREACH t IN ARRAY updated_at_tables LOOP
        EXECUTE format(
            'CREATE TRIGGER set_updated_at BEFORE UPDATE ON %I
                 FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t);
    END LOOP;
END $$;
