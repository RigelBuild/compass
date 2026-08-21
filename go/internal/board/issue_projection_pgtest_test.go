//go:build pgtest && unix

package board

// pgtest suite for the IssueProjection: the live issue=16 fan-out, the
// read-back-reflects-committed-state property, the sorted-clone Snapshot, and
// the durable Rehydrate-from-Postgres recovery. Every case opens its own real
// store (pgtest.RequireDSN + store.Open) — a projection over the store of record
// is only proven against the database it targets (design.md:1188). SKIPs (never
// fails) when no container runtime and no DSN are available.
//
// Every live-channel wait is event-gated with a deadline as a safety net, never
// as a synchronization device (no sleep); negative assertions publish a real
// sentinel after the ignored input and assert the sentinel arrives first.

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// issueTestTimeout bounds every live-channel wait so a broken fan-out fails fast
// instead of hanging the suite. It is a deadline, not a sleep.
const issueTestTimeout = 5 * time.Second

// openIssueStore opens a real Store against a fresh, migrated database and
// returns it with its DSN, so a durability test can open a second store against
// the same DSN. Registers Close on cleanup. SKIPs when no runtime is available.
func openIssueStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(context.Background(), dsn) // test root context
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st, dsn
}

// reopenIssueStore opens an additional Store against an already-migrated dsn
// WITHOUT resetting the schema — the restart-durability path, which must read
// back exactly what a prior store committed.
func reopenIssueStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsn) // test root context
	if err != nil {
		t.Fatalf("store reopen: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// newIssueBoard builds an IssueProjection over a fresh bus and store, closing
// the bus at test end.
func newIssueBoard(t *testing.T) (*IssueProjection, *events.Bus[busPayload], *store.Store) {
	t.Helper()
	st, _ := openIssueStore(t)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	return NewIssueProjection(bus, st), bus, st
}

// recvIssue reads one live event and returns its Issue payload, failing the test
// if the channel closed early or nothing arrived within the deadline.
func recvIssue(t *testing.T, ch <-chan events.Stamped[busPayload]) *compassv1.Issue {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("live channel closed before an event arrived")
		}
		got := e.Payload.GetIssue()
		if got == nil {
			t.Fatalf("live event carried a non-Issue payload: %v", e.Payload)
		}
		return got
	case <-time.After(issueTestTimeout):
		t.Fatal("timed out waiting for a live event")
		return nil
	}
}

// canonicalIssue is a valid ingested wire Issue for a fresh coordinate.
func canonicalIssue(number uint32) *compassv1.Issue {
	return &compassv1.Issue{
		Forge: &compassv1.ForgeRef{
			Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
			Host:     "github.com",
		},
		Repo:         "RigelBuild/compass",
		Number:       number,
		Title:        "a bug",
		Body:         "it broke",
		ForgeState:   "open",
		Url:          "https://github.com/RigelBuild/compass/issues/1",
		ForgeAccount: "octocat",
		Labels:       []string{"bug"},
	}
}

// TestPublishFansIssue16 pins the live fan-out: a subscriber registered before
// the publish receives the upsert as the SubscribeEventsResponse_Issue variant
// (issue=16), carrying the committed issue, and the store row exists at the
// returned id.
//
// RED-first: before PublishIssueUpdate/issueToProto/protoToForgeFields existed,
// the package did not compile (no IssueProjection type / method) — the test
// could not build, let alone pass; the fan-out arrived on no bus.
func TestPublishFansIssue16(t *testing.T) {
	ctx := context.Background() // test root context
	p, bus, st := newIssueBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	if err := p.PublishIssueUpdate(ctx, canonicalIssue(1)); err != nil {
		t.Fatalf("PublishIssueUpdate: %v", err)
	}

	got := recvIssue(t, sub.Live)
	if got.GetRepo() != "RigelBuild/compass" || got.GetNumber() != 1 {
		t.Errorf("fanned issue = %s#%d, want RigelBuild/compass#1", got.GetRepo(), got.GetNumber())
	}
	if got.GetForge().GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB {
		t.Errorf("fanned provider = %v, want GITHUB", got.GetForge().GetProvider())
	}
	if got.GetId() == "" {
		t.Fatal("fanned issue has empty id")
	}
	// The store row exists at the fanned id (committed truth, not cache-only).
	if _, err := st.GetIssue(ctx, got.GetId()); err != nil {
		t.Errorf("GetIssue(%q): %v", got.GetId(), err)
	}
}

// TestPublishReflectsCommittedState pins the read-back property: upsert an issue,
// have the store set a human lifecycle state, then re-publish the SAME coordinate
// with new forge fields — the fanned Issue reflects the committed state
// (IN_PROGRESS), proving the forge re-poll did not clobber it (the 3a no-clobber
// property, now visible on the wire).
//
// RED-first: before the GetIssue read-back step existed, PublishIssueUpdate would
// have fanned the caller's proto (State=UNSPECIFIED, 0), never the committed
// IN_PROGRESS — the assertion below would read UNSPECIFIED.
func TestPublishReflectsCommittedState(t *testing.T) {
	ctx := context.Background() // test root context
	p, bus, st := newIssueBoard(t)

	// First upsert + a human-set state directly through the store.
	id, err := st.UpsertIssueForgeFields(ctx, protoToForgeFields(canonicalIssue(2)))
	if err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	if err := st.SetIssueState(ctx, id, store.IssueStateInProgress); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// Re-poll the SAME coordinate with a changed forge field (title). The upsert
	// must not clobber the human-set state; the read-back sees IN_PROGRESS.
	repoll := canonicalIssue(2)
	repoll.Title = "a bug (updated title)"
	if err := p.PublishIssueUpdate(ctx, repoll); err != nil {
		t.Fatalf("PublishIssueUpdate: %v", err)
	}

	got := recvIssue(t, sub.Live)
	if got.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
		t.Errorf("fanned state = %v, want IN_PROGRESS (read-back, not clobbered)", got.GetState())
	}
	if got.GetTitle() != "a bug (updated title)" {
		t.Errorf("fanned title = %q, want the re-polled title", got.GetTitle())
	}
}

// TestSnapshotReturnsBoard pins Snapshot: publish 3 issues -> Snapshot returns 3,
// sorted by id, each a distinct clone the caller owns.
func TestSnapshotReturnsBoard(t *testing.T) {
	ctx := context.Background() // test root context
	p, _, _ := newIssueBoard(t)

	for _, n := range []uint32{10, 11, 12} {
		if err := p.PublishIssueUpdate(ctx, canonicalIssue(n)); err != nil {
			t.Fatalf("PublishIssueUpdate(%d): %v", n, err)
		}
	}

	snap := p.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i-1].GetId() >= snap[i].GetId() {
			t.Errorf("Snapshot not sorted by id: %q >= %q", snap[i-1].GetId(), snap[i].GetId())
		}
	}
	// Each entry is a distinct DEEP clone: mutating a scalar, a sub-message, or
	// the label slice on a returned Issue must not touch the cache (a second
	// Snapshot returns the unmutated values). The sub-message + slice mutations
	// are the load-bearing half — a shallow copy would share those pointers.
	snap[0].Title = "MUTATED"
	snap[0].GetForge().Host = "evil.example"
	snap[0].Labels[0] = "mutated-label"
	again := p.Snapshot()
	if again[0].GetTitle() == "MUTATED" {
		t.Error("Snapshot shared a scalar: title mutation leaked into the cache")
	}
	if again[0].GetForge().GetHost() == "evil.example" {
		t.Error("Snapshot shared the Forge sub-message: host mutation leaked into the cache")
	}
	if again[0].GetLabels()[0] == "mutated-label" {
		t.Error("Snapshot shared the Labels backing array: label mutation leaked into the cache")
	}
}

// TestRehydrateLoadsFromPostgres is THE durability test: upsert 2 issues through
// store 1, build a FRESH IssueProjection over a NEW bus against the SAME dsn,
// Rehydrate -> Snapshot returns both (the projection recovers durable board state
// on restart, DL-019).
//
// RED-first: before Rehydrate existed, a fresh projection's Snapshot was empty —
// the durable rows in Postgres never reached the in-memory map; this test read 0,
// not 2.
func TestRehydrateLoadsFromPostgres(t *testing.T) {
	ctx := context.Background() // test root context
	st1, dsn := openIssueStore(t)

	for _, n := range []uint32{20, 21} {
		if _, err := st1.UpsertIssueForgeFields(ctx, protoToForgeFields(canonicalIssue(n))); err != nil {
			t.Fatalf("seed upsert(%d): %v", n, err)
		}
	}

	// A fresh projection over a NEW bus + a SECOND store against the same dsn —
	// the restart. It must recover the board from Postgres alone.
	st2 := reopenIssueStore(t, dsn)
	bus2 := events.NewBus[busPayload]()
	t.Cleanup(bus2.Close)
	p := NewIssueProjection(bus2, st2)

	if err := p.Rehydrate(ctx); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}

	snap := p.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot after Rehydrate len = %d, want 2", len(snap))
	}
	nums := map[uint32]bool{}
	for _, iss := range snap {
		nums[iss.GetNumber()] = true
	}
	if !nums[20] || !nums[21] {
		t.Errorf("rehydrated numbers = %v, want {20,21}", nums)
	}
}

// TestRehydrateEmptyBoard pins that Rehydrate on an empty issues table yields an
// empty Snapshot and no error.
func TestRehydrateEmptyBoard(t *testing.T) {
	ctx := context.Background() // test root context
	p, _, _ := newIssueBoard(t)

	if err := p.Rehydrate(ctx); err != nil {
		t.Fatalf("Rehydrate on empty board: %v", err)
	}
	if snap := p.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot on empty board = %v, want empty", snap)
	}
}

// TestPublishInvalidIssueErrorsWithoutRecordingOrFanning pins the error-path
// contract: an issue with an empty forge coordinate is rejected by the store
// (ErrInvalidArgument), and PublishIssueUpdate surfaces that wrapped error
// WITHOUT recording it in the map or fanning an event. The negative fan-out is
// proven with a trailing sentinel: a valid publish after the rejected one must
// be the FIRST event a pre-registered subscriber sees.
func TestPublishInvalidIssueErrorsWithoutRecordingOrFanning(t *testing.T) {
	ctx := context.Background() // test root context
	p, bus, _ := newIssueBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// Empty coordinate -> the store rejects it; the error must propagate.
	if err := p.PublishIssueUpdate(ctx, &compassv1.Issue{}); err == nil {
		t.Fatal("PublishIssueUpdate on an empty coordinate returned nil, want a store error")
	}
	// The rejected upsert recorded nothing.
	if snap := p.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot after a rejected publish = %v, want empty", snap)
	}

	// Trailing sentinel: a valid publish after the rejected one must be the
	// first event on the wire — proving the rejected publish fanned nothing.
	if err := p.PublishIssueUpdate(ctx, canonicalIssue(99)); err != nil {
		t.Fatalf("sentinel PublishIssueUpdate: %v", err)
	}
	got := recvIssue(t, sub.Live)
	if got.GetNumber() != 99 {
		t.Errorf("first fanned event = #%d, want the sentinel #99 (rejected publish must not have fanned)", got.GetNumber())
	}
}
