-- Agent-forge-subscription / artifact-cursor queries (sqlc adoption T6,
-- RIG-3034). These replace the inline SQL literals in
-- internal/store/forge_subscriptions.go; the hand-written Store methods keep
-- their signatures, the door-side validation (validSubscriptionCoordinate /
-- validCoordinate), the scope normalization, the ErrConflict/ErrInvalidArgument/
-- ErrNotFound mapping via pgErrIs, and the two hand-written tx seams: the
-- DeleteAgentForgeSubscription GC (WithTx) and the ListForgeNotifyTargets row
-- grouping. The read queries feed the ForgeNotifySubscriber / ForgeArtifactCursor
-- / ForgeNotifyTarget mappers, which convert the generated rows (provider/kind
-- ints, BIGINT numbers, LEFT-JOIN-nullable cursor columns) back to the domain
-- types.

-- name: EnsureAgentForgeSubscription :one
-- Idempotent on the UNIQUE coordinate: the no-op DO UPDATE (re-set agent to
-- itself) makes RETURNING fire on conflict so a repeat returns the stored id.
INSERT INTO agent_forge_subscriptions
    (id, agent_account_id, forge_provider, forge_host, repo, kind, number, scope, project)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (agent_account_id, forge_provider, forge_host, repo, kind, number, project) DO UPDATE
   SET agent_account_id = EXCLUDED.agent_account_id
RETURNING id;

-- name: DeleteAgentForgeSubscription :one
-- Scoped to the calling agent (id AND agent). RETURNING the coordinate drives the
-- one-tx GC of the artifact cursor when this was the last subscription.
DELETE FROM agent_forge_subscriptions
 WHERE id = $1 AND agent_account_id = $2
RETURNING forge_provider, forge_host, repo, kind, number;

-- name: GCForgeArtifactCursorIfUnsubscribed :exec
-- Collects the coordinate's cursor IFF no subscription for it remains (the NOT
-- EXISTS guard leaves it in place if any other agent still subscribes).
DELETE FROM forge_artifact_cursors
 WHERE forge_artifact_cursors.forge_provider = $1 AND forge_artifact_cursors.forge_host = $2 AND forge_artifact_cursors.repo = $3 AND forge_artifact_cursors.kind = $4 AND forge_artifact_cursors.number = $5
   AND NOT EXISTS (
       SELECT 1 FROM agent_forge_subscriptions
        WHERE agent_forge_subscriptions.forge_provider = $1 AND agent_forge_subscriptions.forge_host = $2 AND agent_forge_subscriptions.repo = $3 AND agent_forge_subscriptions.kind = $4 AND agent_forge_subscriptions.number = $5
   );

-- name: CountAgentForgeSubscriptionsForArtifact :one
SELECT count(*) FROM agent_forge_subscriptions
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5;

-- name: SubscribersForArtifact :many
-- Exact-artifact subscribers, plus (on an opened event) the container-scope
-- subscribers for the same container/project.
SELECT id, agent_account_id, delivered_revision, project
FROM agent_forge_subscriptions
WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4
  AND (
        (scope = 1 AND number = $5)
     OR ($6::boolean AND scope = 2 AND number = 0 AND project = $7)
  );

-- name: ListForgeNotifyTargets :many
-- The reconcile sweep's work list for one (provider, host): each subscribed
-- coordinate with its LEFT-JOINed shared FETCH cursor (nullable when never
-- observed) and the subscriber rows, container-scope rows collapsed per
-- (repo, kind) to coord_number 0. The Go groups the flat rows into targets.
SELECT s.repo, s.kind,
       (CASE WHEN s.scope = 2 THEN 0 ELSE s.number END)::BIGINT AS coord_number,
       s.id, s.agent_account_id, s.delivered_revision, s.project,
       (c.forge_provider IS NOT NULL)::boolean AS has_cursor,
       c.etag, c.comments_etag, c.checks_etag, c.revision, c.snapshot, c.polled_at
FROM agent_forge_subscriptions s
LEFT JOIN forge_artifact_cursors c
  ON c.forge_provider = s.forge_provider
 AND c.forge_host = s.forge_host
 AND c.repo = s.repo
 AND c.kind = s.kind
 AND c.number = CASE WHEN s.scope = 2 THEN 0 ELSE s.number END
WHERE s.forge_provider = $1 AND s.forge_host = $2
ORDER BY s.repo, s.kind, coord_number;

-- name: UpsertForgeArtifactCursor :exec
INSERT INTO forge_artifact_cursors
    (forge_provider, forge_host, repo, kind, number, etag, comments_etag, checks_etag, revision, snapshot, polled_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (forge_provider, forge_host, repo, kind, number) DO UPDATE
   SET etag = EXCLUDED.etag,
       comments_etag = EXCLUDED.comments_etag,
       checks_etag = EXCLUDED.checks_etag,
       revision = EXCLUDED.revision,
       snapshot = EXCLUDED.snapshot,
       polled_at = EXCLUDED.polled_at;

-- name: LoadForgeArtifactCursor :one
SELECT etag, comments_etag, checks_etag, revision, snapshot, polled_at
FROM forge_artifact_cursors
WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5;

-- name: AdvanceForgeDeliveredRevision :execrows
UPDATE agent_forge_subscriptions
   SET delivered_revision = $3, delivered_at = now()
 WHERE id = $2 AND agent_account_id = $1;
