-- name: InsertPullRequestIfAbsent :exec
-- The create path's write: a webhook-hydrated row already present always wins.
-- Only an enabled repo gets a row, since board ingestion never refreshes others.
INSERT INTO pull_requests
    (forge_provider, forge_host, repo, number, forge_state,
     forge_created_at, forge_updated_at, pr)
SELECT $1, $2, $3, $4, $5, $6, $7, $8
 WHERE EXISTS (
  SELECT 1 FROM forge_repo_subscriptions s
   WHERE s.forge_provider = $1 AND s.forge_host = $2 AND s.enabled
     AND (CASE WHEN $1 = 1 THEN lower(s.repo) ELSE s.repo END) = $3)
ON CONFLICT (tenant_id, forge_provider, forge_host, repo, number) DO NOTHING;

-- name: UpsertPullRequestGuarded :execrows
-- Zero rows affected means the stored row is newer and the write was skipped.
INSERT INTO pull_requests
    (forge_provider, forge_host, repo, number, forge_state,
     forge_created_at, forge_updated_at, pr)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, forge_provider, forge_host, repo, number) DO UPDATE
   SET forge_state = EXCLUDED.forge_state,
       forge_created_at = EXCLUDED.forge_created_at,
       forge_updated_at = EXCLUDED.forge_updated_at,
       pr = EXCLUDED.pr
 WHERE EXCLUDED.forge_updated_at >= pull_requests.forge_updated_at;

-- name: UpsertExplicitPullRequestLink :exec
INSERT INTO pull_request_issue_links
    (pr_forge_provider, pr_forge_host, pr_repo, pr_number,
     issue_forge_provider, issue_forge_host, issue_repo, issue_number, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
ON CONFLICT (tenant_id, pr_forge_provider, pr_forge_host, pr_repo, pr_number,
             issue_forge_provider, issue_forge_host, issue_repo, issue_number)
DO UPDATE SET source = 1;

-- name: InsertClosingRefLink :exec
INSERT INTO pull_request_issue_links
    (pr_forge_provider, pr_forge_host, pr_repo, pr_number,
     issue_forge_provider, issue_forge_host, issue_repo, issue_number, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 2)
ON CONFLICT (tenant_id, pr_forge_provider, pr_forge_host, pr_repo, pr_number,
             issue_forge_provider, issue_forge_host, issue_repo, issue_number)
DO NOTHING;

-- name: DeleteClosingRefLink :exec
DELETE FROM pull_request_issue_links
 WHERE pr_forge_provider = $1 AND pr_forge_host = $2 AND pr_repo = $3 AND pr_number = $4
   AND issue_forge_provider = $5 AND issue_forge_host = $6 AND issue_repo = $7
   AND issue_number = $8 AND source = 2;

-- name: ListPullRequestLinks :many
SELECT issue_forge_provider, issue_forge_host, issue_repo, issue_number, source
  FROM pull_request_issue_links
 WHERE pr_forge_provider = $1 AND pr_forge_host = $2 AND pr_repo = $3 AND pr_number = $4
 ORDER BY issue_forge_provider, issue_forge_host, issue_repo, issue_number;

-- name: PullRequestsForIssues :many
-- Explicit links attach directly; a closing reference attaches only when none
-- of the PR's explicit targets is an issue on the board.
WITH want AS (
    SELECT unnest(@issue_providers::smallint[]) AS provider,
           unnest(@issue_hosts::text[]) AS host,
           unnest(@issue_repos::text[]) AS repo,
           unnest(@issue_numbers::bigint[]) AS number
)
SELECT l.issue_forge_provider, l.issue_forge_host, l.issue_repo, l.issue_number,
       p.forge_provider, p.forge_host, p.repo, p.number, p.forge_state,
       p.forge_created_at, p.forge_updated_at, p.pr
  FROM pull_request_issue_links l
  JOIN want w
    ON l.issue_forge_provider = w.provider AND l.issue_forge_host = w.host
   AND l.issue_repo = w.repo AND l.issue_number = w.number
  JOIN pull_requests p
    ON p.tenant_id = l.tenant_id AND p.forge_provider = l.pr_forge_provider
   AND p.forge_host = l.pr_forge_host AND p.repo = l.pr_repo AND p.number = l.pr_number
 WHERE l.source = 1
    OR NOT EXISTS (
        SELECT 1
          FROM pull_request_issue_links e
          JOIN issues i
            ON i.tenant_id = e.tenant_id AND i.forge_provider = e.issue_forge_provider
           AND i.forge_host = e.issue_forge_host AND i.number = e.issue_number
           -- issues keeps the ingested repo casing; links hold GitHub repos lowercased.
           AND (CASE WHEN i.forge_provider = 1 THEN lower(i.repo) ELSE i.repo END) = e.issue_repo
         WHERE e.source = 1 AND e.tenant_id = l.tenant_id
           AND e.pr_forge_provider = l.pr_forge_provider AND e.pr_forge_host = l.pr_forge_host
           AND e.pr_repo = l.pr_repo AND e.pr_number = l.pr_number)
 ORDER BY p.forge_created_at, p.forge_provider, p.forge_host, p.repo, p.number;

-- name: FallbackIssuesForTarget :many
SELECT DISTINCT c.issue_forge_provider, c.issue_forge_host, c.issue_repo, c.issue_number
  FROM pull_request_issue_links e
  JOIN pull_request_issue_links c
    ON c.tenant_id = e.tenant_id AND c.pr_forge_provider = e.pr_forge_provider
   AND c.pr_forge_host = e.pr_forge_host AND c.pr_repo = e.pr_repo AND c.pr_number = e.pr_number
 WHERE e.source = 1 AND c.source = 2
   AND e.issue_forge_provider = $1 AND e.issue_forge_host = $2
   AND e.issue_repo = $3 AND e.issue_number = $4
 ORDER BY c.issue_forge_provider, c.issue_forge_host, c.issue_repo, c.issue_number;

-- name: PullRequestForgeUpdatedAt :one
SELECT forge_updated_at FROM pull_requests
 WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND number = $4;
