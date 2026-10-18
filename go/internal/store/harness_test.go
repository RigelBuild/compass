//go:build pgtest

// These adapters open Store against the shared real-Postgres harness; tests skip
// when no runtime is available. The pgtest build tag keeps them out of default
// tests, while COMPASS_TEST_DATABASE_DSN can point to an existing database.

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

// newTestStore returns a Store connected to a fresh, migrated database. It skips
// the test when no DSN and no container runtime are available.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStoreDSN(t)
	return s
}

// newTestStoreDSN is newTestStore that also returns the database DSN, so a test
// can Close the store and re-Open a second one against the same database — the
// restart-durability path (design.md:1189). The returned dsn addresses a
// freshly-migrated, empty database.
func newTestStoreDSN(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	return openStore(t, dsn), dsn
}

// reopenStore opens an additional Store against an existing, already-migrated
// dsn WITHOUT resetting the schema — for the restart-durability test, which must
// read back exactly what a prior store committed. Registers its own cleanup.
func reopenStore(t *testing.T, dsn string) *Store {
	t.Helper()
	return openStore(t, dsn)
}

// openStore opens a Store against dsn (which pgtest has reset to empty), running
// migrations, and registers its Close on cleanup.
func openStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newTracedTestStore is newTestStore with tracer on every pooled connection.
func newTracedTestStore(t *testing.T, tracer pgx.QueryTracer) *Store {
	t.Helper()
	s, err := OpenTraced(context.Background(), pgtest.RequireDSN(t), tracer)
	if err != nil {
		t.Fatalf("store OpenTraced: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}
