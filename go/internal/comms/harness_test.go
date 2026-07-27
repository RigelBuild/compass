//go:build pgtest

// The handler-level integration harness. The CommsService is a thin shell over
// the real Postgres store and the real event bus (no mocks — a store of record
// is only proven against the database it targets, design.md:1188-1190), so these
// tests drive the handler against an actual Postgres and a live events.Bus.
//
// Container orchestration + schema reset live in the shared internal/pgtest
// harness (podman, else docker; SKIP — not fail — when no runtime is usable, so
// the hermetic gate stays green in a container-less sandbox while the assertions
// are real wherever a runtime exists). Set COMPASS_TEST_DATABASE_DSN to target
// an already-running Postgres instead of starting a container.
//
// Build-tagged `pgtest` so it is not part of the default `go test` gate.

package comms

import (
	"context"
	"testing"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// newTestStore returns a store.Store connected to a fresh, migrated database. It
// skips the test when neither a DSN nor a container runtime is available.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := newTestStoreDSN(t)
	return s
}

// newTestStoreDSN is newTestStore that also returns the DSN, so a test can Close
// the store and re-Open a second one against the same database — the
// restart/resync path.
func newTestStoreDSN(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	return openStore(t, dsn), dsn
}

// reopenStore opens an additional store against an existing, already-migrated
// dsn WITHOUT resetting the schema — for the restart path, which reads back what
// a prior store committed.
func reopenStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	return openStore(t, dsn)
}

// openStore opens a store against dsn (which pgtest has reset to empty), running
// migrations, and registers its Close on cleanup.
func openStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newBus constructs a fresh comms event bus, closed on cleanup so any held-open
// SubscribeComms stream ends.
func newBus(t *testing.T) *events.Bus[*compassv1.SubscribeCommsResponse] {
	t.Helper()
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(bus.Close)
	return bus
}
