//go:build pgtest

package store

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

// TestTokenTenantMigrationBackfillsV1Tokens upgrades a database left at v1 with a
// live token, the shape of every deployed DB, and checks the token still resolves.
func TestTokenTenantMigrationBackfillsV1Tokens(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if _, err := pool.Exec(ctx, migs[0].sql); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	const migrationsDDL = `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`
	if _, err := pool.Exec(ctx, migrationsDDL); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES (1, $1)", migs[0].name); err != nil {
		t.Fatalf("record v1: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ('boot', $1, 'Default', 1)",
		bootstrapTenantSlug,
	); err != nil {
		t.Fatalf("seed bootstrap tenant: %v", err)
	}
	hash := tokenHash("pre-0002-token")
	if _, err := pool.Exec(ctx,
		"INSERT INTO tokens (hash, subject_kind, subject_id) VALUES ($1, $2, 'acct-old')",
		hash[:], int16(SubjectAccount),
	); err != nil {
		t.Fatalf("seed v1 token: %v", err)
	}

	s := reopenStore(t, dsn)
	got, err := s.ResolveTokenHash(ctx, hash)
	if err != nil {
		t.Fatalf("ResolveTokenHash after upgrade: %v", err)
	}
	want := Subject{Kind: SubjectAccount, ID: "acct-old", Tenant: "boot"}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}
