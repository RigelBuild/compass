-- Durable foreign-rebase notices. The request key identifies one moved foreign head.

-- name: RecordForgeNotice :one
INSERT INTO forge_notices (id, tenant_id, from_account_id, to_account_id, request_key, text)
VALUES (
	    sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(from_account_id),
	    sqlc.arg(to_account_id), sqlc.arg(request_key), sqlc.arg(text)
)
ON CONFLICT (tenant_id, request_key) DO NOTHING
RETURNING id::text AS id, tenant_id, from_account_id, to_account_id, request_key, text,
          created_at, delivered_at, attempts;

-- name: ForgeNoticeByRequestKey :one
SELECT id::text AS id, tenant_id, from_account_id, to_account_id, request_key, text,
       created_at, delivered_at, attempts
FROM forge_notices
WHERE tenant_id = sqlc.arg(tenant_id) AND request_key = sqlc.arg(request_key);

-- name: MarkForgeNoticeDelivered :execrows
UPDATE forge_notices
SET delivered_at = COALESCE(delivered_at, now())
WHERE id = sqlc.arg(id);

-- name: UndeliveredForgeNotices :many
-- Runs under the system role so one pass can enumerate notices across tenants.
SELECT id::text AS id, tenant_id, from_account_id, to_account_id, request_key, text,
       created_at, delivered_at, attempts
FROM forge_notices
WHERE delivered_at IS NULL
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(query_limit);

-- name: BumpForgeNoticeAttempt :execrows
UPDATE forge_notices
SET attempts = attempts + 1
WHERE id = sqlc.arg(id) AND delivered_at IS NULL;
