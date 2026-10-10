-- Scope grants are managed for user accounts and agent-specific repositories.
-- name: GrantForgeScope :execrows
-- The SELECT runs under RLS, so a user from another tenant inserts nothing.
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
SELECT u.account_id, sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo)
FROM user_accounts AS u
WHERE u.account_id = sqlc.arg(account_id)
ON CONFLICT DO NOTHING;

-- name: GrantAgentForgeScope :execrows
-- Server-written workstream row; the SELECT runs under RLS.
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
SELECT a.account_id, sqlc.arg(forge_provider), sqlc.arg(forge_host), sqlc.arg(repo)
FROM agent_accounts AS a
WHERE a.account_id = sqlc.arg(account_id)
ON CONFLICT DO NOTHING;

-- name: RevokeAgentForgeScope :execrows
-- Agent rows only: a user id deletes nothing, so a user grant is never removed here.
DELETE FROM account_forge_scopes
WHERE account_id IN (SELECT a.account_id FROM agent_accounts AS a WHERE a.account_id = $1)
  AND forge_provider = $2 AND forge_host = $3 AND repo = $4;

-- name: ListAgentForgeScopeRepos :many
-- An account's own rows only; the owner's grants are not included.
SELECT scope.repo
FROM account_forge_scopes AS scope
WHERE scope.account_id = $1
  AND scope.forge_provider = $2
  AND scope.forge_host = $3
ORDER BY scope.repo;

-- name: CopyAgentForgeScopes :exec
-- A new child starts with its parent agent's own rows; runs under RLS.
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
SELECT sqlc.arg(child_id), scope.forge_provider, scope.forge_host, scope.repo
FROM account_forge_scopes AS scope
WHERE scope.account_id = sqlc.arg(parent_id)
ON CONFLICT DO NOTHING;

-- name: ForgeScopeUserExists :one
SELECT EXISTS (SELECT 1 FROM user_accounts WHERE account_id = $1);

-- name: RevokeForgeScope :exec
DELETE FROM account_forge_scopes
WHERE account_id = $1 AND forge_provider = $2 AND forge_host = $3 AND repo = $4;

-- name: HasForgeScope :one
SELECT EXISTS (
    SELECT 1
    FROM account_forge_scopes AS scope
    LEFT JOIN agent_accounts AS agent ON agent.account_id = $1
    WHERE scope.account_id IN ($1, agent.owner_user_id)
      AND scope.forge_provider = $2
      AND scope.forge_host = $3
      AND scope.repo IN ($4, '*')
);

-- name: ListForgeScopeRepos :many
SELECT DISTINCT scope.repo
FROM account_forge_scopes AS scope
LEFT JOIN agent_accounts AS agent ON agent.account_id = $1
WHERE scope.account_id IN ($1, agent.owner_user_id)
  AND scope.forge_provider = $2
  AND scope.forge_host = $3
ORDER BY scope.repo;
