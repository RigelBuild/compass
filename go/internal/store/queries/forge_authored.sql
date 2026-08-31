-- Forge-authored-artifact queries (sqlc adoption T6, RIG-3034). These replace the
-- inline SQL literals in internal/store/forge_authored.go; the hand-written Store
-- methods keep their signatures, the door-side validation (valid/validCoordinate),
-- the ErrConflict/ErrInvalidArgument/ErrNotFound mapping via pgErrIs, and the
-- textOrNull client_request_id NULL discipline. The read queries feed
-- authoredArtifactFromRow, which maps the generated row (provider/kind ints,
-- number BIGINT, nullable client_request_id) back to the domain AuthoredArtifact.

-- name: RecordAuthoredArtifact :exec
-- WRITE-ONCE authorship: the DO UPDATE deliberately omits agent_account_id and
-- owner_user_id, so a re-land never rewrites who authored the artifact.
INSERT INTO forge_authored_artifacts
    (forge_provider, forge_host, repo, kind, number,
     agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (forge_provider, forge_host, repo, kind, number) DO UPDATE
   SET session_id         = EXCLUDED.session_id,
       client_request_id  = EXCLUDED.client_request_id,
       created_at_unix_ms = EXCLUDED.created_at_unix_ms;

-- name: AuthoredArtifactByRequestID :one
SELECT forge_provider, forge_host, repo, kind, number,
       agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms
FROM forge_authored_artifacts
WHERE agent_account_id = $1 AND client_request_id = $2;

-- name: AuthoredArtifactByCoordinate :one
SELECT forge_provider, forge_host, repo, kind, number,
       agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms
FROM forge_authored_artifacts
WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5;

-- name: ListAuthoredArtifactsByAgent :many
SELECT forge_provider, forge_host, repo, kind, number,
       agent_account_id, owner_user_id, session_id, client_request_id, created_at_unix_ms
FROM forge_authored_artifacts
WHERE agent_account_id = $1
ORDER BY created_at_unix_ms ASC, forge_provider ASC, forge_host ASC, repo ASC, kind ASC, number ASC;
