-- Durable create attempts. Failed attempts may reuse a request key; pending,
-- done, and lost attempts retain it as the idempotency result.

-- name: RecordForgeWriteIntent :one
INSERT INTO forge_write_intents (
    id, forge_provider, forge_host, repo, kind, parent_number, head_ref,
    agent_account_id, owner_user_id, session_id, client_request_id, state
)
VALUES (
    sqlc.arg(id), sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo),
    sqlc.arg(kind), sqlc.arg(parent_number), sqlc.arg(head_ref),
    sqlc.arg(agent_account_id), sqlc.arg(owner_user_id), sqlc.arg(session_id),
    sqlc.arg(client_request_id), 'pending'
)
ON CONFLICT (tenant_id, agent_account_id, client_request_id)
    WHERE state <> 'failed'
DO UPDATE SET id = forge_write_intents.id
RETURNING id, forge_provider, forge_host, repo, kind, parent_number, head_ref,
          agent_account_id, owner_user_id, session_id, client_request_id,
          forge_key, state, created_at, tenant_id;


-- name: PRWriteIntentByHead :one
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       forge_key, state, created_at, tenant_id
FROM forge_write_intents
WHERE forge_provider = sqlc.arg(forge_provider)
  AND forge_host = sqlc.arg(forge_host)
  AND repo = sqlc.arg(repo)
  AND head_ref = sqlc.arg(head_ref)
  AND kind = 2 AND state IN ('pending', 'done')
ORDER BY created_at ASC, id ASC
LIMIT 1;

-- name: EndForgeWriteIntent :execrows
UPDATE forge_write_intents
SET state = sqlc.arg(state)
WHERE id = sqlc.arg(id) AND state = 'pending';


-- name: WriteIntentByID :one
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       forge_key, state, created_at, tenant_id
FROM forge_write_intents
WHERE id = sqlc.arg(id);

-- name: LockWriteIntent :one
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       forge_key, state, created_at, tenant_id
FROM forge_write_intents
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: CompleteForgeWriteIntent :execrows
UPDATE forge_write_intents
SET state = 'done', forge_key = sqlc.arg(forge_key)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: PendingForgeWriteIntents :many
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       forge_key, state, created_at, tenant_id
FROM forge_write_intents
WHERE state = 'pending' AND created_at < sqlc.arg(older_than)::timestamptz
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(query_limit);

-- name: AuthoredArtifactByCoordinateFold :one
SELECT forge_provider, forge_host, repo, kind, number,
       agent_account_id, owner_user_id, session_id, client_request_id,
       created_at_unix_ms, tenant_id
FROM forge_authored_artifacts
WHERE forge_provider = sqlc.arg(forge_provider)
  AND forge_host = sqlc.arg(forge_host)
  AND (CASE WHEN sqlc.arg(forge_provider)::smallint = 1 THEN lower(repo) ELSE repo END) = sqlc.arg(repo)
  AND kind = sqlc.arg(kind)
  AND number = sqlc.arg(number)
ORDER BY created_at_unix_ms ASC, repo ASC
LIMIT 1;
