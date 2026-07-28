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
//
// WHY BOTH DELIVERY PATHS ARE PINNED SEPARATELY. forwardComms drains
// sub.Replay (a snapshot taken at Subscribe()) and then tails sub.Live. Both
// carry the same event shapes, so a test asserting only that a MessagePosted
// arrived passes identically whether live delivery works or the subscriber is
// merely draining history. Correlating on a minted id does NOT close that gap
// here: the ring snapshot is taken under the same lock that registers the live
// subscriber, so a concurrently-posted message legitimately arrives by EITHER
// path. A minted id proves the event is this test's; it does not name the
// mechanism that carried it.
//
// So the discrimination was measured, not argued — each path deleted in turn,
// against the real handler and a real Postgres:
//
//	kill the live tail (return before the sub.Live loop)  -> 6 red
//	  TestCreateChannelEmitsChannelChanged
//	  TestPostMessageWriteThrough
//	  TestRespondToAskHappyPathEmitsMessageUpdated
//	  TestSubscribeCommsPostDeliversMessagePosted
//	  TestSubscribeCommsPrivateMessageLeakBlockedLiveTail
//	  TestPostMessageIdempotentRetrySuppressesDuplicatePublish
//
//	force sub.Replay empty                                -> 9 red
//	  TestForwardCommsDeliversVisibleEvent
//	  TestForwardCommsFailsClosedOnStoreFault
//	  TestForwardCommsSkipsNonVisibleEventAndContinues
//	  TestSubscribeCommsPrivateMessageLeakBlocked
//	  TestSubscribeCommsChannelChangedSharedVisibleToNonMember
//	  TestSubscribeCommsAccountChangedDirectoryScoping
//	  TestSubscribeCommsRemovedMemberGetsFinalChannelChanged
//	  TestSubscribeCommsChannelGroupChangedScoping
//	  TestPostMessageIdempotentRetrySuppressesDuplicatePublish
//
// Written out in full rather than as counts or a glob: a name can be checked
// against the file, "all three" cannot. TestForwardComms* in fact matches FOUR
// tests — TestForwardCommsCleanEndOnCancellation stays green under both
// mutations, since it asserts a clean end rather than a delivery — so the glob
// would have overstated the replay set by one.
//
// Near-disjoint, with exactly one deliberate overlap: the idempotent-retry test
// spans both because it asserts a SET ("exactly one MessagePosted across a post
// and its retry"), reading the first event off the live tail and then bounding
// the total by draining the replay to a canary. Every other test names one path
// only. Neither path can be deleted without a test noticing, and the two failure
// sets barely intersect — that is the property, and it is what a passing suite
// alone does not establish.
//
// The canary is what makes a completeness claim possible at all. mkCanary +
// drainReplayAsActor (visibility_filter_test.go) post a globally-visible event
// AFTER the mutation under test and drain until it arrives, bounding the
// complete set the channel emitted. An assertion that stops at the first match
// can observe neither an absence nor a duplicate; "exactly one" is unprovable
// without a terminator.
//
// Re-run both mutations if this harness is refactored: the numbers above are a
// measurement, and a refactor that collapses the two paths would leave them
// silently wrong. In subscribe.go's forwardComms, one at a time:
//
//	live tail: insert `return nil` immediately before the `for { select {`
//	           that reads sub.Live
//	replay:    change `range sub.Replay` to range over an empty slice
//
// then `go test -tags pgtest ./internal/comms/` — the WHOLE package, never a
// -run filter. The first attempt at this measurement used
// -run 'TestSubscribeComms|TestPostMessage', a pattern that cannot match
// TestCreateChannelEmitsChannelChanged or TestRespondToAskHappyPath...; it
// reported 4 red where the truth is 6. A filter narrower than the claim is
// structurally incapable of falsifying it.

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
