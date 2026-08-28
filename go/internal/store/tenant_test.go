//go:build pgtest

package store

// Tenant contracts (RIG-2861 T1): the tenants table migrates onto a fresh and an
// existing database, Open idempotently seeds exactly one bootstrap tenant, every
// account write stamps a tenant_id (the context tenant when set, else the
// bootstrap tenant — the OSS single-tenant degenerate fallback).

import (
	"context"
	"testing"
)

// tenantOf reads an account's stamped tenant_id directly, so a test asserts the
// persisted tenancy rather than trusting the return value.
func tenantOf(t *testing.T, s *Store, id AccountID) string {
	t.Helper()
	var tenantID string
	if err := s.pool.QueryRow(context.Background(),
		"SELECT tenant_id FROM accounts WHERE id = $1", string(id),
	).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant_id of %q: %v", id, err)
	}
	return tenantID
}

// TestBootstrapTenantSeedsOneIdempotently proves single-tenant boot seeds
// exactly one tenant and a re-run (the restart path) finds it rather than
// minting a second — mirroring TestBootstrapAdminIdempotentByHandle.
func TestBootstrapTenantSeedsOneIdempotently(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&count); err != nil {
		t.Fatalf("count tenants after Open: %v", err)
	}
	if count != 1 {
		t.Fatalf("tenants after Open = %d, want exactly one bootstrap tenant", count)
	}

	// A second bootstrap (the restart path) is a no-op find, not a second row:
	// same id, still exactly one tenant.
	again, err := s.BootstrapTenant(ctx)
	if err != nil {
		t.Fatalf("BootstrapTenant(restart): %v", err)
	}
	if again != s.bootstrapTenantID {
		t.Fatalf("restart minted a new tenant %q, want the existing %q", again, s.bootstrapTenantID)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&count); err != nil {
		t.Fatalf("count tenants after restart: %v", err)
	}
	if count != 1 {
		t.Fatalf("tenants after restart = %d, want still exactly one", count)
	}
}

// TestTenantMigrationAppliesOnFreshAndExistingDB proves the tenants schema
// migrates onto a fresh database (newTestStore Opens against a reset DB) and
// that re-Opening the same DSN (the existing-DB restart path) applies cleanly
// and adds no duplicate tenant — BootstrapTenant is idempotent at Open too.
func TestTenantMigrationAppliesOnFreshAndExistingDB(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreDSN(t)

	first := s.bootstrapTenantID
	if first == "" {
		t.Fatalf("fresh Open left bootstrapTenantID empty")
	}

	// Re-Open against the same, already-migrated database: the existing-DB path.
	reopened := reopenStore(t, dsn)
	if reopened.bootstrapTenantID != first {
		t.Fatalf("reopen bootstrapTenantID = %q, want the existing %q", reopened.bootstrapTenantID, first)
	}
	var count int
	if err := reopened.pool.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&count); err != nil {
		t.Fatalf("count tenants after reopen: %v", err)
	}
	if count != 1 {
		t.Fatalf("tenants after reopen = %d, want still exactly one", count)
	}
}

// TestCreateUserStampsTenant proves CreateUser stamps the context tenant when
// one is set (the managed multi-tenant path) and falls back to the bootstrap
// tenant when the context carries none (the OSS single-tenant degenerate path).
func TestCreateUserStampsTenant(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// No tenant in context → the bootstrap tenant is stamped.
	bootstrapUser, err := s.CreateUser(ctx, NewUser{Handle: "bootstrap", DisplayName: "Bootstrap"})
	if err != nil {
		t.Fatalf("CreateUser(no tenant): %v", err)
	}
	if got := tenantOf(t, s, bootstrapUser.ID); got != string(s.bootstrapTenantID) {
		t.Fatalf("no-tenant CreateUser stamped %q, want the bootstrap tenant %q", got, s.bootstrapTenantID)
	}

	// A second tenant row, then CreateUser under its context stamps it.
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		"tenant-other", "other", "Other", int64(1),
	); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	otherTenant := TenantID("tenant-other")

	scopedUser, err := s.CreateUser(WithTenant(ctx, otherTenant), NewUser{Handle: "scoped", DisplayName: "Scoped"})
	if err != nil {
		t.Fatalf("CreateUser(with tenant): %v", err)
	}
	if got := tenantOf(t, s, scopedUser.ID); got != string(otherTenant) {
		t.Fatalf("tenant-context CreateUser stamped %q, want %q", got, otherTenant)
	}
}

// TestCreateAgentStampsTenant proves the agent-insert path also stamps the
// context tenant. CreateAgent inserts through a different (transactional) path
// than CreateUser, so a wrong-tenant stamp there would not be caught by the
// CreateUser test nor by the NOT NULL column — this asserts the persisted
// tenant_id on the agent account directly. The owning user is created under the
// same tenant context so the owner FK resolves within the tenant.
func TestCreateAgentStampsTenant(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.pool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		"tenant-agent", "agent-tenant", "Agent Tenant", int64(1),
	); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	tenant := TenantID("tenant-agent")
	tctx := WithTenant(ctx, tenant)

	owner, err := s.CreateUser(tctx, NewUser{Handle: "agent-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	agent, err := s.CreateAgent(tctx, owner.ID, NewAgent{Handle: "worker", DisplayName: "Worker"})
	if err != nil {
		t.Fatalf("CreateAgent(with tenant): %v", err)
	}
	if got := tenantOf(t, s, agent.ID); got != string(tenant) {
		t.Fatalf("tenant-context CreateAgent stamped %q, want %q", got, tenant)
	}
}
