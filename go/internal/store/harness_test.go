//go:build pgtest

// The store package's thin adapter over the shared real-Postgres harness
// (internal/pgtest). pgtest owns the container orchestration + schema reset and
// hands back a DSN, skipping (not failing) when no runtime is usable; the
// wrappers here open a store.Store against that DSN. A store of record is only
// proven against the database it targets, so there is no mock (design.md:1188).
//
// Build-tagged `pgtest` so it is not part of the default `go test` gate; the
// moon test lane opts in where a runtime is available (T1 test cycle). Set
// COMPASS_TEST_DATABASE_DSN to point every test at an already-running Postgres
// instead of starting a container (the CI-service path, design.md:1188).

package store

import (
	"context"
	"testing"

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
