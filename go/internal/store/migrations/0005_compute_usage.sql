-- 0005_compute_usage: the append-only compute interval event log. Each
-- session binding is one billable interval; starts and ends commit with its row.

CREATE TABLE compute_usage_events (
    tenant_id        TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    id               TEXT        NOT NULL,
    interval_id      TEXT        NOT NULL,
    kind             TEXT        NOT NULL CHECK (kind IN ('start', 'end')),
    occurred_at      TIMESTAMPTZ NOT NULL,
    agent_account_id TEXT        NOT NULL,
    owner_user_id    TEXT        NOT NULL,
    session_id       TEXT        NOT NULL,
    runner_id        TEXT        NOT NULL,
    estimated        BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, interval_id, kind)
);

CREATE INDEX compute_usage_events_occurred_at_idx ON compute_usage_events (tenant_id, occurred_at);

-- Keep the default so older servers can insert bindings during a rolling deploy.
-- squawk-ignore adding-field-with-default
ALTER TABLE session_bindings ADD COLUMN usage_interval_id TEXT NOT NULL DEFAULT gen_random_uuid()::TEXT;

GRANT SELECT, INSERT ON compute_usage_events TO compass_app, compass_system;

SET LOCAL ROLE compass_system;

INSERT INTO compute_usage_events (
    tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
    owner_user_id, session_id, runner_id, estimated
)
-- updated_at marks the last re-point, the best start this table still holds.
SELECT b.tenant_id, gen_random_uuid()::TEXT AS id, b.usage_interval_id, 'start' AS kind, b.updated_at AS occurred_at,
       b.agent_account_id, a.owner_user_id, b.session_id, b.runner_id, TRUE AS estimated
  FROM session_bindings AS b
  JOIN agent_accounts AS a
    ON a.account_id = b.agent_account_id
   AND a.tenant_id = b.tenant_id;

RESET ROLE;

DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY['compute_usage_events'];
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
