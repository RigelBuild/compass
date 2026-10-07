-- Close intervals only after the binding and matching end event are absent.
-- The tenant id comes from each start because this sweep has no tenant GUC.
-- name: CloseOrphanedComputeIntervals :execrows
WITH orphaned AS (
    SELECT starts.tenant_id, starts.interval_id, starts.agent_account_id,
           starts.owner_user_id, starts.session_id, starts.runner_id
      FROM compute_usage_events AS starts
     WHERE starts.kind = 'start'
       AND NOT EXISTS (
           SELECT 1
             FROM compute_usage_events AS ends
            WHERE ends.tenant_id = starts.tenant_id
              AND ends.interval_id = starts.interval_id
              AND ends.kind = 'end'
       )
       AND NOT EXISTS (
           SELECT 1
             FROM session_bindings AS bindings
            WHERE bindings.tenant_id = starts.tenant_id
              AND bindings.usage_interval_id = starts.interval_id
       )
)
INSERT INTO compute_usage_events (
    tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
    owner_user_id, session_id, runner_id, estimated
)
SELECT orphaned.tenant_id, gen_random_uuid()::text AS id, orphaned.interval_id,
       'end' AS kind, now() AS occurred_at, orphaned.agent_account_id,
       orphaned.owner_user_id, orphaned.session_id, orphaned.runner_id,
       true AS estimated
  FROM orphaned
 ORDER BY orphaned.tenant_id, orphaned.interval_id
ON CONFLICT DO NOTHING;

-- Compute-usage rollups are derived from closed start/end event pairs.
-- Each query runs under the tenant role and transaction scope supplied by Store.

-- LockComputeUsage serializes a tenant rebuild with end-event rollup triggers.
-- name: LockComputeUsage :exec
SELECT pg_advisory_xact_lock(hashtext('compute_usage:' || current_setting('compass.tenant_id', TRUE)));

-- LockComputeUsageTenant takes the same lock for a system-role caller.
-- The key must stay byte-identical to LockComputeUsage and the end trigger.
-- name: LockComputeUsageTenant :exec
SELECT pg_advisory_xact_lock(hashtext('compute_usage:' || @tenant_id::text));

-- ComputeUsageSeries sums matching rows at one pre-aggregated granularity.
-- name: ComputeUsageSeries :many
SELECT r.bucket_start::timestamptz AS bucket_start,
       sum(r.active_ms)::bigint AS active_ms,
       sum(r.intervals)::bigint AS intervals
  FROM (SELECT rollups.bucket_start, rollups.agent_account_id, rollups.active_ms, rollups.intervals
          FROM compute_usage_rollups_hourly AS rollups
         WHERE @granularity::integer = 1
        UNION ALL
        SELECT rollups.bucket_start, rollups.agent_account_id, rollups.active_ms, rollups.intervals
          FROM compute_usage_rollups_daily AS rollups
         WHERE @granularity::integer = 2) AS r
 WHERE r.bucket_start >= @start_at::timestamptz
   AND r.bucket_start < @end_at::timestamptz
   AND (coalesce(cardinality(@agent_account_ids::text[]), 0) = 0
        OR r.agent_account_id = ANY (@agent_account_ids::text[]))
 GROUP BY r.bucket_start
 ORDER BY r.bucket_start;

-- ComputeUsagePruneHorizon pins the horizon while the rebuild replaces newer rows.
-- name: ComputeUsagePruneHorizon :one
SELECT horizon.horizon FROM compute_usage_prune_horizon AS horizon FOR SHARE;

-- DeleteComputeUsageRollupsFrom clears rows the rebuild can reconstruct.
-- name: DeleteComputeUsageRollupsFrom :exec
WITH hourly AS (
    DELETE FROM compute_usage_rollups_hourly AS rollups
     WHERE rollups.bucket_start >= @horizon::timestamptz
)
DELETE FROM compute_usage_rollups_daily AS rollups
 WHERE rollups.bucket_start >= @horizon::timestamptz;

-- RollUpComputeUsageFrom rebuilds only buckets at or after the horizon, which are
-- the rows it deleted; older buckets are frozen. The sentinel skips clipping.
-- name: RollUpComputeUsageFrom :exec
WITH intervals AS (
    SELECT starts.tenant_id, starts.interval_id, starts.occurred_at AS start_at,
           ends.occurred_at AS end_at, starts.owner_user_id, starts.agent_account_id,
           floor(extract(epoch FROM starts.occurred_at) * 1000)::bigint AS start_ms,
           floor(extract(epoch FROM ends.occurred_at) * 1000)::bigint AS end_ms
      FROM compute_usage_events AS starts
      JOIN compute_usage_events AS ends
        ON ends.tenant_id = starts.tenant_id
       AND ends.interval_id = starts.interval_id
       AND ends.kind = 'end'
     WHERE starts.kind = 'start'
       AND ends.occurred_at >= @horizon::timestamptz
), bounds AS (
    SELECT intervals.*,
           CASE WHEN @horizon::timestamptz = '-infinity'::timestamptz
                THEN date_trunc('hour', intervals.start_at, 'UTC')
                ELSE greatest(date_trunc('hour', intervals.start_at, 'UTC'),
                              date_trunc('hour', @horizon::timestamptz, 'UTC'))
            END AS first_hour,
           greatest(date_trunc('hour', intervals.start_at, 'UTC'),
                    date_trunc('hour', intervals.end_at - interval '1 millisecond', 'UTC')) AS last_hour,
           CASE WHEN @horizon::timestamptz = '-infinity'::timestamptz
                THEN date_trunc('day', intervals.start_at, 'UTC')
                ELSE greatest(date_trunc('day', intervals.start_at, 'UTC'),
                              date_trunc('day', @horizon::timestamptz, 'UTC'))
            END AS first_day,
           greatest(date_trunc('day', intervals.start_at, 'UTC'),
                    date_trunc('day', intervals.end_at - interval '1 millisecond', 'UTC')) AS last_day
      FROM intervals
), hourly AS (
    INSERT INTO compute_usage_rollups_hourly (
        bucket_start, owner_user_id, agent_account_id, active_ms, intervals
    )
    SELECT buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id,
           sum(greatest(0, least(bounds.end_ms, epoch.bucket_ms + 3600000) -
                           greatest(bounds.start_ms, epoch.bucket_ms)))::bigint,
           sum(CASE WHEN buckets.bucket_start = date_trunc('hour', bounds.start_at, 'UTC')
                     AND bounds.start_at >= @horizon::timestamptz
                    THEN 1 ELSE 0 END)::bigint
      FROM bounds
      CROSS JOIN LATERAL generate_series(
          bounds.first_hour,
          bounds.last_hour,
          interval '1 hour'
      ) AS buckets(bucket_start)
      CROSS JOIN LATERAL (
          SELECT floor(extract(epoch FROM buckets.bucket_start) * 1000)::bigint AS bucket_ms
      ) AS epoch
     GROUP BY buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id
), daily AS (
    INSERT INTO compute_usage_rollups_daily (
        bucket_start, owner_user_id, agent_account_id, active_ms, intervals
    )
    SELECT buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id,
           sum(greatest(0, least(bounds.end_ms, epoch.bucket_ms + 86400000) -
                           greatest(bounds.start_ms, epoch.bucket_ms)))::bigint,
           sum(CASE WHEN buckets.bucket_start = date_trunc('day', bounds.start_at, 'UTC')
                     AND bounds.start_at >= @horizon::timestamptz
                    THEN 1 ELSE 0 END)::bigint
      FROM bounds
      CROSS JOIN LATERAL generate_series(
          bounds.first_day,
          bounds.last_day,
          interval '24 hours'
      ) AS buckets(bucket_start)
      CROSS JOIN LATERAL (
          SELECT floor(extract(epoch FROM buckets.bucket_start) * 1000)::bigint AS bucket_ms
      ) AS epoch
     GROUP BY buckets.bucket_start, bounds.owner_user_id, bounds.agent_account_id
)
SELECT 1;

-- AdvanceComputeUsagePruneHorizon moves the single global horizon forward.
-- name: AdvanceComputeUsagePruneHorizon :execrows
UPDATE compute_usage_prune_horizon AS state
 SET horizon = GREATEST(state.horizon, @cutoff::timestamptz);

-- DeleteComputeUsageIntervalsBefore removes both events only when the end is old.
-- It runs as the system role, so the tenant predicate is the only tenant scope.
-- name: DeleteComputeUsageIntervalsBefore :execrows
WITH closed AS (
    SELECT ends.tenant_id, ends.interval_id
      FROM compute_usage_events AS ends
     WHERE ends.tenant_id = @tenant_id::text
       AND ends.kind = 'end'
       AND ends.occurred_at < @cutoff::timestamptz
       AND EXISTS (
           SELECT 1
             FROM compute_usage_events AS starts
            WHERE starts.tenant_id = ends.tenant_id
              AND starts.interval_id = ends.interval_id
              AND starts.kind = 'start'
       )
)
DELETE FROM compute_usage_events AS events
 USING closed
 WHERE events.tenant_id = @tenant_id::text
   AND events.tenant_id = closed.tenant_id
   AND events.interval_id = closed.interval_id;
