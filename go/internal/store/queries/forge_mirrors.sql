-- Ask mirroring settings and outbox persistence.

-- name: ForgeMirrorMode :one
SELECT COALESCE((
    SELECT mode FROM forge_mirror_settings
     WHERE forge_provider = sqlc.arg(forge_provider)::smallint AND forge_host = sqlc.arg(forge_host)::text AND repo = sqlc.arg(repo)::text
), 0)::smallint AS mode;

-- name: ListForgeMirrorSettings :many
SELECT s.forge_provider, s.forge_host,
       (CASE WHEN s.forge_provider = 1 THEN lower(btrim(s.repo)) ELSE btrim(s.repo) END)::text AS repo,
       COALESCE(m.mode, 0)::smallint AS mode
  FROM forge_repo_subscriptions s
  LEFT JOIN forge_mirror_settings m
    ON m.forge_provider = s.forge_provider
   AND m.forge_host = s.forge_host
   AND m.repo = CASE WHEN s.forge_provider = 1 THEN lower(btrim(s.repo)) ELSE btrim(s.repo) END
 WHERE s.enabled
 ORDER BY s.forge_provider, s.forge_host, s.repo;

-- name: DeleteForgeMirrorSetting :exec
DELETE FROM forge_mirror_settings
 WHERE forge_provider = sqlc.arg(forge_provider) AND forge_host = sqlc.arg(forge_host) AND repo = sqlc.arg(repo);

-- name: UpsertForgeMirrorSetting :exec
INSERT INTO forge_mirror_settings (forge_provider, forge_host, repo, mode, updated_by)
VALUES (sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo), sqlc.arg(mode), sqlc.arg(updated_by))
ON CONFLICT (tenant_id, forge_provider, forge_host, repo) DO UPDATE
   SET mode = EXCLUDED.mode, updated_by = EXCLUDED.updated_by;

-- name: ClaimForgeMirror :one
INSERT INTO forge_mirrors
    (id, source, ask_id, source_key, actor_account_id, agent_account_id,
     client_request_id, target_ref, body_rev, state, session_id, source_payload)
VALUES (sqlc.arg(id), sqlc.arg(source), sqlc.arg(ask_id), sqlc.arg(source_key), sqlc.arg(actor_account_id),
        sqlc.narg(agent_account_id), sqlc.narg(client_request_id), sqlc.arg(target_ref), sqlc.arg(body_rev), sqlc.arg(state),
        sqlc.arg(session_id), sqlc.narg(source_payload)::jsonb)
ON CONFLICT DO NOTHING
RETURNING id, source, ask_id, source_key, actor_account_id,
          COALESCE(agent_account_id, '')::text AS agent_account_id,
          client_request_id, target_ref, body_rev, posted_rev, state, attempts,
          comment_id, comment_key, comment_url, last_error, first_attempt_at,
          last_attempt_at, client_comment_id::text AS client_comment_id,
          recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code;

-- name: ForgeMirrorByConflictKeys :one
SELECT id, source, ask_id, source_key, actor_account_id,
       COALESCE(agent_account_id, '')::text AS agent_account_id,
       client_request_id, target_ref, body_rev, posted_rev, state, attempts,
       comment_id, comment_key, comment_url, last_error, first_attempt_at,
       last_attempt_at, client_comment_id::text AS client_comment_id,
       recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code,
       CASE WHEN agent_account_id = NULLIF(sqlc.arg(agent_account_id)::text, '') AND client_request_id = sqlc.narg(client_request_id)::text
            THEN 2::smallint
            WHEN ask_id = NULLIF(sqlc.arg(ask_id)::text, '') THEN 1::smallint
            ELSE 3::smallint END AS conflict_kind
  FROM forge_mirrors
 WHERE (sqlc.arg(ask_id)::text <> '' AND ask_id = sqlc.arg(ask_id)::text)
    OR (sqlc.arg(agent_account_id)::text <> '' AND agent_account_id = sqlc.arg(agent_account_id)::text AND client_request_id = sqlc.narg(client_request_id)::text)
    OR (sqlc.arg(source_key)::text <> '' AND source_key = sqlc.arg(source_key)::text AND target_ref = sqlc.arg(target_ref)::text)
 ORDER BY CASE WHEN agent_account_id = NULLIF(sqlc.arg(agent_account_id)::text, '') AND client_request_id = sqlc.narg(client_request_id)::text THEN 1
               WHEN ask_id = NULLIF(sqlc.arg(ask_id)::text, '') THEN 2
               ELSE 3 END
 LIMIT 1;

-- name: BumpForgeMirrorSourceRevision :execrows
UPDATE forge_mirrors
   SET body_rev = LEAST(body_rev::integer + 1, 32767)::smallint, state = 1,
       source_payload = COALESCE(sqlc.narg(source_payload)::jsonb, source_payload),
       session_id = CASE WHEN sqlc.arg(session_id)::text = '' THEN session_id ELSE sqlc.arg(session_id)::text END
 WHERE id = sqlc.arg(id);

-- name: OpenForgeMirrors :many
SELECT id, source, ask_id, source_key, actor_account_id,
       COALESCE(agent_account_id, '')::text AS agent_account_id,
       client_request_id, target_ref, body_rev, posted_rev, state, attempts,
       comment_id, comment_key, comment_url, last_error, first_attempt_at,
       last_attempt_at, client_comment_id::text AS client_comment_id,
       recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code
  FROM forge_mirrors
 WHERE agent_account_id = sqlc.arg(agent_account_id)::text AND (state <> 2 OR recheck_at <= now())
 ORDER BY created_at, id;

-- name: AskForgeMirrors :many
SELECT id, source, ask_id, source_key, actor_account_id,
       COALESCE(agent_account_id, '')::text AS agent_account_id,
       client_request_id, target_ref, body_rev, posted_rev, state, attempts,
       comment_id, comment_key, comment_url, last_error, first_attempt_at,
       last_attempt_at, client_comment_id::text AS client_comment_id,
       recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code
  FROM forge_mirrors WHERE ask_id = ANY(sqlc.arg(ask_ids)::text[]) ORDER BY created_at, id;

-- name: LeaseForgeMirror :one
UPDATE forge_mirrors
   SET lease_until = now() + sqlc.arg(ttl)::interval, lease_token = sqlc.arg(lease_token)
 WHERE id = sqlc.arg(id) AND (state <> 2 OR recheck_at <= now())
   AND (lease_until IS NULL OR lease_until < now())
RETURNING id, source, ask_id, source_key, actor_account_id,
          COALESCE(agent_account_id, '')::text AS agent_account_id,
          client_request_id, target_ref, body_rev, posted_rev, state, attempts,
          comment_id, comment_key, comment_url, last_error, first_attempt_at,
          last_attempt_at, client_comment_id::text AS client_comment_id,
          recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code;

-- name: RenewForgeMirror :execrows
UPDATE forge_mirrors SET lease_until = now() + sqlc.arg(ttl)::interval
 WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token) AND lease_until > now();

-- name: BeginForgeMirrorAttempt :one
UPDATE forge_mirrors
   SET first_attempt_at = COALESCE(first_attempt_at, now()),
       last_attempt_at = now(), attempts = attempts + 1
 WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token) AND lease_until > now()
RETURNING id, source, ask_id, source_key, actor_account_id,
          COALESCE(agent_account_id, '')::text AS agent_account_id,
          client_request_id, target_ref, body_rev, posted_rev, state, attempts,
          comment_id, comment_key, comment_url, last_error, first_attempt_at,
          last_attempt_at, client_comment_id::text AS client_comment_id,
          recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code;

-- name: MarkForgeMirror :execrows
UPDATE forge_mirrors
   SET posted_rev = sqlc.arg(posted_rev),
       state = CASE WHEN sqlc.arg(state)::smallint = 2 AND body_rev > sqlc.arg(posted_rev)::smallint THEN 1::smallint ELSE sqlc.arg(state)::smallint END,
       comment_id = CASE WHEN sqlc.arg(comment_id)::bigint = 0 THEN comment_id ELSE sqlc.arg(comment_id) END,
       comment_key = CASE WHEN sqlc.arg(comment_key)::text = '' THEN comment_key ELSE sqlc.arg(comment_key) END,
       comment_url = CASE WHEN sqlc.arg(comment_url)::text = '' THEN comment_url ELSE sqlc.arg(comment_url) END,
       last_error = sqlc.arg(last_error),
       last_error_code = sqlc.arg(last_error_code),
       recheck_at = CASE WHEN sqlc.arg(recheck)::boolean THEN now() + interval '10 minutes'
                         WHEN sqlc.arg(clear_recheck)::boolean THEN NULL
                         ELSE recheck_at END,
       lease_until = NULL, lease_token = ''
 WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token) AND lease_until > now();

-- name: ForgeMirrorByID :one
SELECT id, source, ask_id, source_key, actor_account_id,
       COALESCE(agent_account_id, '')::text AS agent_account_id,
       client_request_id, target_ref, body_rev, posted_rev, state, attempts,
       comment_id, comment_key, comment_url, last_error, first_attempt_at,
       last_attempt_at, client_comment_id::text AS client_comment_id,
       recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code
  FROM forge_mirrors WHERE id = sqlc.arg(id);

-- name: ForgeMirrorByRequest :one
SELECT id, source, ask_id, source_key, actor_account_id,
       COALESCE(agent_account_id, '')::text AS agent_account_id,
       client_request_id, target_ref, body_rev, posted_rev, state, attempts,
       comment_id, comment_key, comment_url, last_error, first_attempt_at,
       last_attempt_at, client_comment_id::text AS client_comment_id,
       recheck_at, lease_until, lease_token, updated_at, session_id, source_payload, last_error_code
  FROM forge_mirrors
 WHERE agent_account_id = sqlc.arg(agent_account_id)::text
   AND (client_request_id = sqlc.arg(client_request_id)::text
        OR id = (SELECT mirror_id FROM forge_mirror_requests r
                  WHERE r.agent_account_id = sqlc.arg(agent_account_id)::text
                    AND r.client_request_id = sqlc.arg(client_request_id)::text))
 LIMIT 1;

-- name: RecordForgeMirrorRequest :execrows
INSERT INTO forge_mirror_requests (agent_account_id, client_request_id, mirror_id)
VALUES (sqlc.arg(agent_account_id), sqlc.arg(client_request_id), sqlc.arg(mirror_id))
ON CONFLICT DO NOTHING;

-- name: InsertAskForgeMirror :exec
INSERT INTO forge_mirrors
    (id, source, ask_id, actor_account_id, agent_account_id, target_ref, body_rev, state, session_id)
VALUES (sqlc.arg(id), 1, sqlc.arg(ask_id), sqlc.arg(actor_account_id), sqlc.arg(actor_account_id), sqlc.arg(target_ref), 1, 1, sqlc.arg(session_id));

-- name: BumpAskForgeMirror :exec
UPDATE forge_mirrors SET body_rev = 2, state = 1 WHERE ask_id = sqlc.arg(ask_id);
