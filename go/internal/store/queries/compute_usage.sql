-- Close intervals only after both binding deletion and its end event are absent.
-- The tenant id comes from each start because the system role has no tenant GUC.
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
SELECT orphaned.tenant_id, gen_random_uuid()::text AS id, orphaned.interval_id, 'end' AS kind, now() AS occurred_at,
       orphaned.agent_account_id, orphaned.owner_user_id, orphaned.session_id,
       orphaned.runner_id, true AS estimated
  FROM orphaned
ON CONFLICT DO NOTHING;
