//go:build pgtest

package store

import (
	"context"
	"testing"
)

// TestBackfillEmptyTenantMigration replays migration 0002 over rows a system-role
// wake stamped with an empty tenant_id. Each row must land in its agent's tenant;
// an empty-tenant binding whose tenant already binds that agent must be dropped.
func TestBackfillEmptyTenantMigration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tenantB := seedTenant(t, s, "tenant-b")
	ctxB := WithTenant(ctx, tenantB)
	lone := seedTenantAgent(t, ctxB, s, "lone")
	twice := seedTenantAgent(t, ctxB, s, "twice")

	// The live state: twice is bound in tenant B and again under ''.
	if _, err := s.RecordSessionBinding(ctxB, "sess-twice-live", twice.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding: %v", err)
	}
	execAsSystem(t, s, "INSERT INTO agent_sessions (session_id, agent_account_id, tenant_id) VALUES ('sess-lone', $1, ''), ('sess-twice-old', $2, '')",
		string(lone.ID), string(twice.ID))
	execAsSystem(t, s, "INSERT INTO session_bindings (agent_account_id, session_id, runner_id, tenant_id, usage_interval_id) VALUES ($1, 'sess-lone', 'runner-1', '', 'iv-lone'), ($2, 'sess-twice-old', 'runner-1', '', 'iv-twice-old')",
		string(lone.ID), string(twice.ID))

	execAsSystem(t, s, migrationSQL(t, 2))

	if n := countAsSystem(t, s, "SELECT count(*) FROM agent_sessions WHERE tenant_id = ''") +
		countAsSystem(t, s, "SELECT count(*) FROM session_bindings WHERE tenant_id = ''"); n != 0 {
		t.Fatalf("%d rows still carry tenant_id ''", n)
	}
	if got, ok, err := s.LatestSessionForAccount(ctxB, lone.ID); err != nil || !ok || got != "sess-lone" {
		t.Fatalf("tenant B session for lone = %q,%v,%v; want sess-lone", got, ok, err)
	}
	if got, err := s.SessionBindingTenant(WithSystemRole(ctx), "sess-lone", "runner-1"); err != nil || got != tenantB {
		t.Fatalf("lone binding tenant = %q,%v; want %q", got, err, tenantB)
	}
	if got, err := s.SessionBindingTenant(WithSystemRole(ctx), "sess-twice-live", "runner-1"); err != nil || got != tenantB {
		t.Fatalf("twice live binding tenant = %q,%v; want %q (no ambiguity)", got, err, tenantB)
	}
	if n := countAsSystem(t, s, "SELECT count(*) FROM session_bindings WHERE session_id = 'sess-twice-old'"); n != 0 {
		t.Fatalf("stale '' binding for twice survived (%d rows)", n)
	}
}

func migrationSQL(t *testing.T, version int) string {
	t.Helper()
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	return migs[version-1].sql
}

func execAsSystem(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+systemRole); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countAsSystem(t *testing.T, s *Store, sql string) int {
	t.Helper()
	var n int
	execTx := func() error {
		ctx := context.Background()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+systemRole); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql).Scan(&n)
	}
	if err := execTx(); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
