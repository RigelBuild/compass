-- 0009_compute_usage_rollups adds derived compute duration buckets and a raw-log horizon.

CREATE TABLE compute_usage_rollups_hourly (
    tenant_id        TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    bucket_start     TIMESTAMPTZ NOT NULL,
    owner_user_id    TEXT        NOT NULL,
    agent_account_id TEXT        NOT NULL,
    active_ms        BIGINT      NOT NULL,
    intervals        BIGINT      NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket_start, owner_user_id, agent_account_id)
);

CREATE TABLE compute_usage_rollups_daily (
    tenant_id        TEXT        NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    bucket_start     TIMESTAMPTZ NOT NULL,
    owner_user_id    TEXT        NOT NULL,
    agent_account_id TEXT        NOT NULL,
    active_ms        BIGINT      NOT NULL,
    intervals        BIGINT      NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bucket_start, owner_user_id, agent_account_id)
);

CREATE TABLE compute_usage_prune_horizon (
    singleton  BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    horizon    TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO compute_usage_prune_horizon (horizon) VALUES ('-infinity');

GRANT DELETE ON compute_usage_events TO compass_system;
GRANT SELECT, INSERT, UPDATE, DELETE
    ON compute_usage_rollups_hourly, compute_usage_rollups_daily,
       compute_usage_prune_horizon
    TO compass_app, compass_system;
REVOKE INSERT, DELETE ON compute_usage_prune_horizon FROM compass_app, compass_system;

DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY[
        'compute_usage_rollups_hourly', 'compute_usage_rollups_daily'
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

DO $$
DECLARE
    t text;
    updated_at_tables text[] := ARRAY[
        'compute_usage_rollups_hourly',
        'compute_usage_rollups_daily',
        'compute_usage_prune_horizon'
    ];
BEGIN
    FOREACH t IN ARRAY updated_at_tables LOOP
        EXECUTE format(
            'CREATE TRIGGER set_updated_at BEFORE UPDATE ON %I
                 FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t);
    END LOOP;
END $$;

-- Keep this trigger unpinned so isolated schemas resolve their own tables.
CREATE FUNCTION roll_up_compute_usage_end() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    started compute_usage_events%ROWTYPE;
    start_ms BIGINT;
    end_ms BIGINT;
    width_ms BIGINT;
    rollup_bucket TIMESTAMPTZ;
    first_bucket TIMESTAMPTZ;
    last_bucket TIMESTAMPTZ;
    bucket_ms BIGINT;
    active_ms BIGINT;
BEGIN
    IF NEW.kind <> 'end' THEN
        RETURN NEW;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtext('compute_usage:' || NEW.tenant_id));
    -- No horizon clipping: an interval open at prune time is in no frozen bucket yet.
    SELECT events.* INTO started
      FROM compute_usage_events AS events
     WHERE events.tenant_id = NEW.tenant_id
       AND events.interval_id = NEW.interval_id
       AND events.kind = 'start';
    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    start_ms := floor(extract(epoch FROM started.occurred_at) * 1000)::bigint;
    end_ms := floor(extract(epoch FROM NEW.occurred_at) * 1000)::bigint;

    FOREACH width_ms IN ARRAY ARRAY[3600000::bigint, 86400000::bigint] LOOP
        IF width_ms = 3600000 THEN
            first_bucket := date_trunc('hour', started.occurred_at, 'UTC');
            last_bucket := greatest(first_bucket, date_trunc('hour', NEW.occurred_at - interval '1 millisecond', 'UTC'));
        ELSE
            first_bucket := date_trunc('day', started.occurred_at, 'UTC');
            last_bucket := greatest(first_bucket, date_trunc('day', NEW.occurred_at - interval '1 millisecond', 'UTC'));
        END IF;

        rollup_bucket := first_bucket;
        WHILE rollup_bucket <= last_bucket LOOP
            bucket_ms := floor(extract(epoch FROM rollup_bucket) * 1000)::bigint;
            active_ms := greatest(0, least(end_ms, bucket_ms + width_ms) - greatest(start_ms, bucket_ms));
            IF width_ms = 3600000 THEN
                INSERT INTO compute_usage_rollups_hourly (
                    tenant_id, bucket_start, owner_user_id, agent_account_id, active_ms, intervals
                ) VALUES (
                    NEW.tenant_id, rollup_bucket, started.owner_user_id, started.agent_account_id,
                    active_ms, CASE WHEN rollup_bucket = first_bucket THEN 1 ELSE 0 END
                )
                ON CONFLICT (tenant_id, bucket_start, owner_user_id, agent_account_id)
                DO UPDATE SET
                    active_ms = compute_usage_rollups_hourly.active_ms + EXCLUDED.active_ms,
                    intervals = compute_usage_rollups_hourly.intervals + EXCLUDED.intervals;
            ELSE
                INSERT INTO compute_usage_rollups_daily (
                    tenant_id, bucket_start, owner_user_id, agent_account_id, active_ms, intervals
                ) VALUES (
                    NEW.tenant_id, rollup_bucket, started.owner_user_id, started.agent_account_id,
                    active_ms, CASE WHEN rollup_bucket = first_bucket THEN 1 ELSE 0 END
                )
                ON CONFLICT (tenant_id, bucket_start, owner_user_id, agent_account_id)
                DO UPDATE SET
                    active_ms = compute_usage_rollups_daily.active_ms + EXCLUDED.active_ms,
                    intervals = compute_usage_rollups_daily.intervals + EXCLUDED.intervals;
            END IF;
            rollup_bucket := rollup_bucket + CASE WHEN width_ms = 3600000 THEN interval '1 hour' ELSE interval '24 hours' END;
        END LOOP;
    END LOOP;
    RETURN NEW;
END $$;

CREATE TRIGGER compute_usage_rollup_after_end
AFTER INSERT ON compute_usage_events
FOR EACH ROW EXECUTE FUNCTION roll_up_compute_usage_end();

-- Seed rollups for closed intervals written before the trigger existed.
SET LOCAL ROLE compass_system;

WITH intervals AS (
    SELECT starts.tenant_id, starts.interval_id, starts.occurred_at AS start_at,
           ends.occurred_at AS end_at, starts.owner_user_id, starts.agent_account_id,
           floor(extract(EPOCH FROM starts.occurred_at) * 1000)::BIGINT AS start_ms,
           floor(extract(EPOCH FROM ends.occurred_at) * 1000)::BIGINT AS end_ms
      FROM compute_usage_events AS starts
      JOIN compute_usage_events AS ends
        ON ends.tenant_id = starts.tenant_id
       AND ends.interval_id = starts.interval_id
       AND ends.kind = 'end'
     WHERE starts.kind = 'start'
),

bounds AS (
    SELECT intervals.*,
           date_trunc('hour', intervals.start_at, 'UTC') AS first_hour,
           greatest(date_trunc('hour', intervals.start_at, 'UTC'),
                    date_trunc('hour', intervals.end_at - INTERVAL '1 millisecond', 'UTC')) AS last_hour,
           date_trunc('day', intervals.start_at, 'UTC') AS first_day,
           greatest(date_trunc('day', intervals.start_at, 'UTC'),
                    date_trunc('day', intervals.end_at - INTERVAL '1 millisecond', 'UTC')) AS last_day
      FROM intervals
),

hourly AS (
    INSERT INTO compute_usage_rollups_hourly (
        tenant_id, bucket_start, owner_user_id, agent_account_id, active_ms, intervals
    )
    SELECT bounds.tenant_id, buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id,
           sum(greatest(0, least(bounds.end_ms, epoch.bucket_ms + 3600000) -
                           greatest(bounds.start_ms, epoch.bucket_ms)))::BIGINT AS active_ms,
           sum(CASE WHEN buckets.bucket_start = bounds.first_hour THEN 1 ELSE 0 END)::BIGINT AS intervals
      FROM bounds
      CROSS JOIN LATERAL generate_series(
          bounds.first_hour,
          bounds.last_hour,
          INTERVAL '1 hour'
      ) AS buckets(bucket_start)
      CROSS JOIN LATERAL (
          SELECT floor(extract(EPOCH FROM buckets.bucket_start) * 1000)::BIGINT AS bucket_ms
      ) AS epoch
     GROUP BY bounds.tenant_id, buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id
    RETURNING 1
),

daily AS (
    INSERT INTO compute_usage_rollups_daily (
        tenant_id, bucket_start, owner_user_id, agent_account_id, active_ms, intervals
    )
    SELECT bounds.tenant_id, buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id,
           sum(greatest(0, least(bounds.end_ms, epoch.bucket_ms + 86400000) -
                           greatest(bounds.start_ms, epoch.bucket_ms)))::BIGINT AS active_ms,
           sum(CASE WHEN buckets.bucket_start = bounds.first_day THEN 1 ELSE 0 END)::BIGINT AS intervals
      FROM bounds
      CROSS JOIN LATERAL generate_series(
          bounds.first_day,
          bounds.last_day,
          INTERVAL '24 hours'
      ) AS buckets(bucket_start)
      CROSS JOIN LATERAL (
          SELECT floor(extract(EPOCH FROM buckets.bucket_start) * 1000)::BIGINT AS bucket_ms
      ) AS epoch
     GROUP BY bounds.tenant_id, buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id
    RETURNING 1
)

SELECT (SELECT count(*) FROM hourly) + (SELECT count(*) FROM daily);

RESET ROLE;
