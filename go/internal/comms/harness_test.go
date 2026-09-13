//go:build pgtest

// The handler-level integration harness. The CommsService is a thin shell over
// the real Postgres store and event bus, so these tests drive the handler
// against an actual Postgres and a live events.Bus. Orchestration lives in
// internal/pgtest (SKIP, not fail, when no runtime); set COMPASS_TEST_DATABASE_DSN.

// Build-tagged `pgtest`, out of the default `go test` gate.

// WHY BOTH DELIVERY PATHS ARE PINNED SEPARATELY. forwardComms drains sub.Replay
// (a snapshot taken at Subscribe()) then tails sub.Live. Both carry the same
// event shapes, and a minted id proves the event is this test's but not which
// path carried it — the ring snapshot races the live subscriber under one lock.

// So the discrimination was measured, not argued: killing the live tail reddens
// 6 tests; forcing sub.Replay empty reddens 9. The two sets are near-disjoint
// with one deliberate overlap (the idempotent-retry test, which asserts a SET),
// so neither path can be deleted without a test noticing.

// The canary makes the "exactly one" completeness claim possible: mkCanary +
// drainReplayAsActor (visibility_filter_test.go) post a globally-visible event
// AFTER the mutation and drain until it arrives, bounding the emitted set. An
// assertion stopping at the first match can observe neither absence nor duplicate.

// Re-run both mutations if this harness is refactored — the counts are a
// measurement. In subscribe.go's forwardComms, one at a time: `return nil`
// before the sub.Live loop, then range sub.Replay over empty. Run the WHOLE
// `go test -tags pgtest ./internal/comms/`, never a -run filter.

package comms

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
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
