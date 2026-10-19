-- Durable create attempts. Failed attempts may reuse a request key; pending,
-- done, and lost attempts retain it as the idempotency result.

-- name: RecordForgeWriteIntent :one
INSERT INTO forge_write_intents (
    id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
    agent_account_id, owner_user_id, session_id, client_request_id,
    issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number, state
)
VALUES (
    sqlc.arg(id), sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo),
    sqlc.arg(kind), sqlc.arg(parent_number), sqlc.arg(head_ref), sqlc.narg(base_ref)::text,
    sqlc.arg(agent_account_id), sqlc.arg(owner_user_id), sqlc.arg(session_id),
    sqlc.arg(client_request_id), sqlc.narg(issue_link_forge_provider)::smallint,
    sqlc.narg(issue_link_forge_host)::text, sqlc.narg(issue_link_repo)::text,
    sqlc.narg(issue_link_number)::bigint, 'pending'
)
ON CONFLICT (tenant_id, agent_account_id, client_request_id)
    WHERE state <> 'failed'
DO UPDATE SET id = forge_write_intents.id
RETURNING id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
          agent_account_id, owner_user_id, session_id, client_request_id,
          issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number,
          forge_key, forge_url, state, created_at, tenant_id;

-- name: PRWriteIntentByHead :one
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number,
       forge_key, forge_url, state, created_at, tenant_id
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
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number,
       forge_key, forge_url, state, created_at, tenant_id
FROM forge_write_intents
WHERE id = sqlc.arg(id);

-- name: HoldForgeWriteIntent :one
INSERT INTO forge_write_intents (
    id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
    agent_account_id, owner_user_id, session_id, client_request_id, state
)
VALUES (
    sqlc.arg(id), sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo),
    sqlc.arg(kind), sqlc.arg(parent_number), sqlc.arg(head_ref), sqlc.narg(base_ref)::text,
    sqlc.arg(agent_account_id), sqlc.arg(owner_user_id), sqlc.arg(session_id),
    sqlc.arg(client_request_id), 'held'
)
ON CONFLICT (tenant_id, agent_account_id, client_request_id)
    WHERE state <> 'failed'
DO UPDATE SET id = forge_write_intents.id
RETURNING id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
          agent_account_id, owner_user_id, session_id, client_request_id,
          issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number,
          forge_key, forge_url, state, created_at, tenant_id;

-- name: PromoteForgeWriteIntent :execrows
UPDATE forge_write_intents
SET state = 'pending', created_at = now(),
    base_ref = COALESCE(sqlc.narg(base_ref)::text, base_ref),
    issue_link_forge_provider = COALESCE(sqlc.narg(issue_link_forge_provider)::smallint, issue_link_forge_provider),
    issue_link_forge_host = COALESCE(sqlc.narg(issue_link_forge_host)::text, issue_link_forge_host),
    issue_link_repo = COALESCE(sqlc.narg(issue_link_repo)::text, issue_link_repo),
    issue_link_number = COALESCE(sqlc.narg(issue_link_number)::bigint, issue_link_number)
WHERE id = sqlc.arg(id) AND state = 'held';

-- name: LockWriteIntent :one
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number,
       forge_key, forge_url, state, created_at, tenant_id
FROM forge_write_intents
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: CompleteForgeWriteIntent :execrows
UPDATE forge_write_intents
SET state = 'done', forge_key = sqlc.arg(forge_key), forge_url = sqlc.arg(forge_url)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: CompletePullRequestWriteIntent :execrows
WITH intent AS (
    SELECT tenant_id, forge_provider, forge_host, repo, agent_account_id,
           owner_user_id, session_id, issue_link_forge_provider,
           issue_link_forge_host, issue_link_repo, issue_link_number
    FROM forge_write_intents
    WHERE id = sqlc.arg(id) AND state = 'pending' AND kind = 2
    FOR UPDATE
), artifact AS (
    SELECT sqlc.arg(number)::bigint AS number
    WHERE EXISTS (SELECT 1 FROM intent)
), owned AS (
    INSERT INTO forge_authored_artifacts (
        forge_provider, forge_host, repo, kind, number, agent_account_id,
        owner_user_id, session_id, client_request_id, created_at_unix_ms
    )
    SELECT i.forge_provider, i.forge_host, i.repo, 2, a.number,
           i.agent_account_id, i.owner_user_id, i.session_id, '',
           sqlc.arg(created_at_unix_ms)::bigint
    FROM intent i CROSS JOIN artifact a
    ON CONFLICT (tenant_id, forge_provider, forge_host, repo, kind, number) DO UPDATE
       SET session_id = EXCLUDED.session_id,
           client_request_id = EXCLUDED.client_request_id,
           created_at_unix_ms = EXCLUDED.created_at_unix_ms
    RETURNING number
), pull_request AS (
    INSERT INTO pull_requests (
        forge_provider, forge_host, repo, number, forge_state,
        forge_created_at, forge_updated_at, pr
    )
    SELECT sqlc.arg(pr_forge_provider)::smallint, sqlc.arg(pr_forge_host)::text,
           sqlc.arg(pr_repo)::text, a.number,
           sqlc.arg(forge_state)::text, sqlc.arg(forge_created_at)::timestamptz,
           sqlc.arg(forge_updated_at)::timestamptz, sqlc.arg(pr)::jsonb
    FROM artifact a
    ON CONFLICT (tenant_id, forge_provider, forge_host, repo, number) DO NOTHING
    RETURNING number
), issue_link AS (
    INSERT INTO pull_request_issue_links (
        pr_forge_provider, pr_forge_host, pr_repo, pr_number,
        issue_forge_provider, issue_forge_host, issue_repo, issue_number, source
    )
    SELECT sqlc.arg(pr_forge_provider)::smallint, sqlc.arg(pr_forge_host)::text,
           sqlc.arg(pr_repo)::text, a.number,
           i.issue_link_forge_provider, i.issue_link_forge_host,
           i.issue_link_repo, i.issue_link_number, 1
    FROM intent i CROSS JOIN artifact a
    WHERE i.issue_link_forge_provider IS NOT NULL
    ON CONFLICT (tenant_id, pr_forge_provider, pr_forge_host, pr_repo, pr_number,
                 issue_forge_provider, issue_forge_host, issue_repo, issue_number)
    DO UPDATE SET source = 1
    RETURNING pr_number
)
UPDATE forge_write_intents
SET state = 'done', forge_key = (SELECT number::text FROM artifact),
    forge_url = sqlc.arg(forge_url)::text
WHERE forge_write_intents.id = sqlc.arg(id) AND forge_write_intents.state = 'pending'
  AND EXISTS (SELECT 1 FROM owned)
  AND (NOT EXISTS (SELECT 1 FROM intent WHERE issue_link_forge_provider IS NOT NULL)
       OR EXISTS (SELECT 1 FROM issue_link));

-- name: PendingForgeWriteIntents :many
SELECT id, forge_provider, forge_host, repo, kind, parent_number, head_ref, base_ref,
       agent_account_id, owner_user_id, session_id, client_request_id,
       issue_link_forge_provider, issue_link_forge_host, issue_link_repo, issue_link_number,
       forge_key, forge_url, state, created_at, tenant_id
FROM forge_write_intents
WHERE state = 'pending' AND created_at < sqlc.arg(older_than)::timestamptz
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(query_limit);

-- name: DeleteExpiredHeldWriteIntents :execrows
-- The outer predicate rechecks state, so a row promoted to pending meanwhile survives.
DELETE FROM forge_write_intents
WHERE state = 'held' AND created_at < sqlc.arg(older_than)::timestamptz
  AND (tenant_id, id) IN (
    SELECT tenant_id, id FROM forge_write_intents
    WHERE state = 'held' AND created_at < sqlc.arg(older_than)::timestamptz
    ORDER BY created_at ASC, id ASC
    LIMIT sqlc.arg(query_limit)
);

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
