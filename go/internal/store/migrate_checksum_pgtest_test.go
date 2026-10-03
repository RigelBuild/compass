//go:build pgtest

package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

// TestOpenRefusesEditedAppliedMigration simulates a shipped migration edited
// after a database applied it: the recorded checksum no longer matches the
// embedded file, and Open must refuse rather than serve a divergent schema.
func TestOpenRefusesEditedAppliedMigration(t *testing.T) {
	dsn := pgtest.RequireDSN(t)
	applyV1Only(t, dsn)
	execOnDSN(t, dsn, "UPDATE schema_migrations SET checksum = 'edited-since-apply' WHERE version = 1")

	s, err := Open(t.Context(), dsn)
	if err == nil {
		s.Close()
		t.Fatal("Open served a database whose applied v1 differs from the embedded file")
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Open err = %v, want errors.Is(_, ErrSchemaVersion)", err)
	}
}

// TestOpenBaselinesLegacyMigrationRows covers a database migrated before
// checksums existed: its bookkeeping table has no checksum column, so Open adds
// it and records the embedded checksums instead of refusing every deployed box.
func TestOpenBaselinesLegacyMigrationRows(t *testing.T) {
	dsn := pgtest.RequireDSN(t)
	applyV1Only(t, dsn)
	execOnDSN(t, dsn, "ALTER TABLE schema_migrations DROP COLUMN checksum")
	s := openStore(t, dsn)
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	rows, err := s.pool.Query(t.Context(), "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("read checksums: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var v int
		var sum *string
		if err := rows.Scan(&v, &sum); err != nil {
			t.Fatalf("scan checksum: %v", err)
		}
		if want := migs[v-1].checksum; sum == nil || *sum != want {
			t.Errorf("v%d checksum = %v, want %s", v, sum, want)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate checksums: %v", err)
	}
	if n != len(migs) {
		t.Fatalf("schema_migrations rows = %d, want %d", n, len(migs))
	}
}

func execOnDSN(t *testing.T, dsn, sql string) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
