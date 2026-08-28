package store

import (
	"context"
	"fmt"
	"time"
)

const (
	bootstrapTenantSlug        = "default"
	bootstrapTenantDisplayName = "Default"
)

// BootstrapTenant ensures the single bootstrap tenant exists and returns its id,
// idempotently — the isolation root the OSS single-tenant deployment stamps
// every account with. Mirrors BootstrapAdmin's unique-violation-means-fetch
// shape: on first boot it mints one tenants row (slug bootstrapTenantSlug); on
// every later boot the insert hits the unique slug and the existing id is
// fetched and returned. Called from Open, so a store is tenant-ready before it
// serves.
func (s *Store) BootstrapTenant(ctx context.Context) (TenantID, error) {
	id := newID()
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		id, bootstrapTenantSlug, bootstrapTenantDisplayName, time.Now().UnixMilli(),
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return s.tenantIDBySlug(ctx, bootstrapTenantSlug)
		}
		return "", fmt.Errorf("store: insert bootstrap tenant: %w", err)
	}
	return TenantID(id), nil
}

// tenantIDBySlug fetches an existing tenant id by slug, backing
// BootstrapTenant's idempotent restart path.
func (s *Store) tenantIDBySlug(ctx context.Context, slug string) (TenantID, error) {
	var id string
	if err := s.pool.QueryRow(ctx, "SELECT id FROM tenants WHERE slug = $1", slug).Scan(&id); err != nil {
		return "", fmt.Errorf("store: resolve tenant by slug: %w", err)
	}
	return TenantID(id), nil
}

// resolveTenant returns the tenant to stamp a write with: the tenant set on the
// context by the auth layer if present, else the bootstrap tenant (the OSS
// single-tenant degenerate path). Mirrors comms.actorFromContext's
// set-or-bootstrap-fallback: no `if multiTenant` fork — a single-tenant
// deployment simply always falls through to the bootstrap tenant.
func (s *Store) resolveTenant(ctx context.Context) TenantID {
	if t, ok := TenantFromContext(ctx); ok && t != "" {
		return t
	}
	return s.bootstrapTenantID
}
