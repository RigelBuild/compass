-- Scope grants are managed for user accounts; agents inherit their owner's rows.
-- name: GrantForgeScope :exec
INSERT INTO account_forge_scopes (account_id, forge_provider, forge_host, repo)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

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
