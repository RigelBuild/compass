-- Tenant-bootstrap queries (sqlc adoption T6, RIG-3034). These replace the inline
-- SQL literals in internal/store/tenant.go; the hand-written Store methods keep
-- their signatures and the unique-violation-means-fetch idempotent bootstrap
-- shape (BootstrapTenant falls back to TenantIDBySlug on a duplicate slug).

-- name: InsertTenant :exec
INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4);

-- name: TenantIDBySlug :one
SELECT id FROM tenants WHERE slug = $1;
