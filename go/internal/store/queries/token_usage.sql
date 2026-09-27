-- Token-usage queries (Plane A). The tenant tx sets the GUC that RLS reads and
-- that tenant_id defaults to, so no statement here names a tenant.

-- LockTokenUsage serializes one tenant's appends and rebuilds. A rebuild beside
-- an append would count the append's events twice or lose them.
-- name: LockTokenUsage :exec
SELECT pg_advisory_xact_lock(hashtext('token_usage:' || current_setting('compass.tenant_id', TRUE)));

-- AppendTokenUsageEvents inserts a batch and adds only the events it inserted
-- to both rollups, so a replayed id counts once. The arrays hold one element
-- per event, and no id repeats.
-- name: AppendTokenUsageEvents :exec
WITH inserted AS (
    INSERT INTO token_usage_events (
        id, occurred_at, agent_account_id, owner_user_id, session_id, request_id, provider,
        model, credential_id, input_tokens, output_tokens, cache_read_tokens,
        cache_write_tokens, total_tokens, cost_micro_usd, rate_version, outcome)
    SELECT unnest(@ids::text[]), unnest(@occurred_at::timestamptz[]),
           unnest(@agent_account_ids::text[]), unnest(@owner_user_ids::text[]),
           unnest(@session_ids::text[]), unnest(@request_ids::text[]),
           unnest(@providers::text[]), unnest(@models::text[]),
           unnest(@credential_ids::text[]), unnest(@input_tokens::bigint[]),
           unnest(@output_tokens::bigint[]), unnest(@cache_read_tokens::bigint[]),
           unnest(@cache_write_tokens::bigint[]), unnest(@total_tokens::bigint[]),
           unnest(@cost_micro_usd::bigint[]), unnest(@rate_versions::text[]),
           unnest(@outcomes::text[])
    ON CONFLICT (tenant_id, id) DO NOTHING
    RETURNING occurred_at, owner_user_id, agent_account_id, provider, model, input_tokens,
              output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd
), hourly AS (
    INSERT INTO token_usage_rollups_hourly (
        bucket_start, owner_user_id, agent_account_id, provider, model, input_tokens,
        output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd)
    SELECT date_trunc('hour', occurred_at, 'UTC'), owner_user_id, agent_account_id, provider,
           model, sum(input_tokens), sum(output_tokens), sum(cache_read_tokens),
           sum(cache_write_tokens), sum(total_tokens), sum(cost_micro_usd)
      FROM inserted
     GROUP BY 1, 2, 3, 4, 5
    ON CONFLICT (tenant_id, bucket_start, owner_user_id, agent_account_id, provider, model)
    DO UPDATE SET
        input_tokens       = token_usage_rollups_hourly.input_tokens + EXCLUDED.input_tokens,
        output_tokens      = token_usage_rollups_hourly.output_tokens + EXCLUDED.output_tokens,
        cache_read_tokens  = token_usage_rollups_hourly.cache_read_tokens + EXCLUDED.cache_read_tokens,
        cache_write_tokens = token_usage_rollups_hourly.cache_write_tokens + EXCLUDED.cache_write_tokens,
        total_tokens       = token_usage_rollups_hourly.total_tokens + EXCLUDED.total_tokens,
        cost_micro_usd     = token_usage_rollups_hourly.cost_micro_usd + EXCLUDED.cost_micro_usd
)
INSERT INTO token_usage_rollups_daily (
    bucket_start, owner_user_id, agent_account_id, provider, model, input_tokens,
    output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd)
SELECT date_trunc('day', occurred_at, 'UTC'), owner_user_id, agent_account_id, provider,
       model, sum(input_tokens), sum(output_tokens), sum(cache_read_tokens),
       sum(cache_write_tokens), sum(total_tokens), sum(cost_micro_usd)
  FROM inserted
 GROUP BY 1, 2, 3, 4, 5
ON CONFLICT (tenant_id, bucket_start, owner_user_id, agent_account_id, provider, model)
DO UPDATE SET
    input_tokens       = token_usage_rollups_daily.input_tokens + EXCLUDED.input_tokens,
    output_tokens      = token_usage_rollups_daily.output_tokens + EXCLUDED.output_tokens,
    cache_read_tokens  = token_usage_rollups_daily.cache_read_tokens + EXCLUDED.cache_read_tokens,
    cache_write_tokens = token_usage_rollups_daily.cache_write_tokens + EXCLUDED.cache_write_tokens,
    total_tokens       = token_usage_rollups_daily.total_tokens + EXCLUDED.total_tokens,
    cost_micro_usd     = token_usage_rollups_daily.cost_micro_usd + EXCLUDED.cost_micro_usd;

-- TokenUsageSeries sums one granularity's rollup rows per bucket. granularity
-- is the usage.Granularity value: 1 is hourly, 2 is daily. An empty agent list
-- or provider means no filter.
-- name: TokenUsageSeries :many
SELECT r.bucket_start::timestamptz          AS bucket_start,
       sum(r.input_tokens)::bigint          AS input_tokens,
       sum(r.output_tokens)::bigint         AS output_tokens,
       sum(r.cache_read_tokens)::bigint     AS cache_read_tokens,
       sum(r.cache_write_tokens)::bigint    AS cache_write_tokens,
       sum(r.total_tokens)::bigint          AS total_tokens,
       sum(r.cost_micro_usd)::bigint        AS cost_micro_usd
  FROM (SELECT bucket_start, agent_account_id, provider, input_tokens, output_tokens,
               cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd
          FROM token_usage_rollups_hourly
         WHERE @granularity::integer = 1
        UNION ALL
        SELECT bucket_start, agent_account_id, provider, input_tokens, output_tokens,
               cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd
          FROM token_usage_rollups_daily
         WHERE @granularity::integer = 2) AS r
 WHERE r.bucket_start >= @start_at::timestamptz
   AND r.bucket_start < @end_at::timestamptz
   AND (coalesce(cardinality(@agent_account_ids::text[]), 0) = 0
        OR r.agent_account_id = ANY (@agent_account_ids::text[]))
   AND (@provider::text = '' OR r.provider = @provider::text)
 GROUP BY r.bucket_start
 ORDER BY r.bucket_start;

-- TokenUsagePruneHorizon reads the prune horizon and holds it until the tx
-- ends, so a prune cannot delete the events a rebuild is about to count.
-- name: TokenUsagePruneHorizon :one
SELECT horizon FROM token_usage_prune_horizon FOR SHARE;

-- DeleteTokenUsageRollupsFrom drops both rollups from the horizon on. Older
-- rollups can hold pruned events, so they stay.
-- name: DeleteTokenUsageRollupsFrom :exec
WITH hourly AS (
    DELETE FROM token_usage_rollups_hourly WHERE bucket_start >= @horizon::timestamptz
)
DELETE FROM token_usage_rollups_daily WHERE bucket_start >= @horizon::timestamptz;

-- RollUpTokenUsageFrom rebuilds both rollups from the events at or after the
-- horizon.
-- name: RollUpTokenUsageFrom :exec
WITH hourly AS (
    INSERT INTO token_usage_rollups_hourly (
        bucket_start, owner_user_id, agent_account_id, provider, model, input_tokens,
        output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd)
    SELECT date_trunc('hour', occurred_at, 'UTC'), owner_user_id, agent_account_id, provider,
           model, sum(input_tokens), sum(output_tokens), sum(cache_read_tokens),
           sum(cache_write_tokens), sum(total_tokens), sum(cost_micro_usd)
      FROM token_usage_events
     WHERE occurred_at >= @horizon::timestamptz
     GROUP BY 1, 2, 3, 4, 5
)
INSERT INTO token_usage_rollups_daily (
    bucket_start, owner_user_id, agent_account_id, provider, model, input_tokens,
    output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost_micro_usd)
SELECT date_trunc('day', occurred_at, 'UTC'), owner_user_id, agent_account_id, provider,
       model, sum(input_tokens), sum(output_tokens), sum(cache_read_tokens),
       sum(cache_write_tokens), sum(total_tokens), sum(cost_micro_usd)
  FROM token_usage_events
 WHERE occurred_at >= @horizon::timestamptz
 GROUP BY 1, 2, 3, 4, 5;

-- AdvanceTokenUsagePruneHorizon commits before the prune deletes anything, and
-- waits for a rebuild that holds the old horizon. It only moves forward.
-- name: AdvanceTokenUsagePruneHorizon :exec
UPDATE token_usage_prune_horizon SET horizon = GREATEST(horizon, @cutoff::timestamptz);

-- DeleteTokenUsageEventsBefore deletes old events of the tx's tenant only,
-- because row-level security scopes it. The prune runs it once per tenant.
-- name: DeleteTokenUsageEventsBefore :execrows
DELETE FROM token_usage_events WHERE occurred_at < @cutoff::timestamptz;
