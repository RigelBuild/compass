//go:build pgtest && unix

package server

// pgtest lane for part 4b's re-snapshot seam against a REAL PG-rehydrated board:
// ListBoardIssues over a projection Rehydrated from Postgres, the
// boundary-then-tail ordering a since_seq==0 subscriber observes, and the
// union-by-id self-healing property when a live upsert lands AFTER the boundary.
// A projection over the store of record is only proven against the database it
// targets — every case opens its own isolated-schema store (pgtest.RequireDSN +
// store.Open), SKIPping when no runtime/DSN is available.
//
// The board is driven as the WRITER through IssueProjection.PublishIssueUpdate
// (the ingestion sink), exactly as part 3's poller would; the RPC/stream is the
// reader. Every stream wait is deadline-gated as a safety net, never a sleep.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// boardCanonicalIssue is a valid ingested wire Issue for a fresh forge
// coordinate (distinct issue number). Mirrors the board package's canonicalIssue
// fixture; duplicated here because it is unexported to that package.
func boardCanonicalIssue(number uint32) *compassv1.Issue {
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

// newBoardServiceClient stands up a service over a fresh bus and a real store,
// with an IssueProjection wired in, mounts it on the h2c door, and returns a
// connect client plus the bus and projection so a test drives the board as the
// writer. Bus/store are closed at test end.
func newBoardServiceClient(t *testing.T) (compassv1connect.CompassServiceClient, *events.Bus[busPayload], *board.IssueProjection) {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(context.Background(), dsn) // test root context
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	issueBrd := board.NewIssueProjection(bus, st)
	svc := newService("test", bus, st, nil, nil, issueBrd, nil)
	url := newH2CTestServer(t, svc)
	return newH2CClient(t, url), bus, issueBrd
}

// TestListBoardIssuesReturnsRehydratedBoard is THE durability seam at the RPC:
// two issues committed to Postgres through a first projection, a FRESH
// projection Rehydrated from the SAME store, and ListBoardIssues over that
// projection returns both (sorted by id). A regression that read the empty
// in-memory map instead of the rehydrated one returns 0 here.
func TestListBoardIssuesReturnsRehydratedBoard(t *testing.T) {
	ctx := context.Background() // test root context
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	// Commit two issues through a first projection (the writer path).
	bus1 := events.NewBus[busPayload]()
	t.Cleanup(bus1.Close)
	writer := board.NewIssueProjection(bus1, st)
	if err := writer.PublishIssueUpdate(ctx, boardCanonicalIssue(1)); err != nil {
		t.Fatalf("PublishIssueUpdate(1): %v", err)
	}
	if err := writer.PublishIssueUpdate(ctx, boardCanonicalIssue(2)); err != nil {
		t.Fatalf("PublishIssueUpdate(2): %v", err)
	}

	// A fresh projection over a NEW bus recovers the durable board on Rehydrate.
	bus2 := events.NewBus[busPayload]()
	t.Cleanup(bus2.Close)
	reader := board.NewIssueProjection(bus2, st)
	if err := reader.Rehydrate(ctx); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	svc := newService("test", bus2, st, nil, nil, reader, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	resp, err := client.ListBoardIssues(ctx, connect.NewRequest(&compassv1.ListBoardIssuesRequest{}))
	if err != nil {
		t.Fatalf("ListBoardIssues: %v", err)
	}
	got := resp.Msg.GetIssues()
	if len(got) != 2 {
		t.Fatalf("ListBoardIssues over a rehydrated board = %d issues, want 2", len(got))
	}
	// Sorted by id (Snapshot's determinism contract survives the wire).
	if got[0].GetId() >= got[1].GetId() {
		t.Fatalf("issues not sorted by id: %q then %q", got[0].GetId(), got[1].GetId())
	}
	// Both forge coordinates round-tripped.
	nums := map[uint32]bool{got[0].GetNumber(): true, got[1].GetNumber(): true}
	if !nums[1] || !nums[2] {
		t.Fatalf("rehydrated issue numbers = %v, want {1,2}", nums)
	}
}

// TestSubscribeEventsBoundaryThenLiveUpsertUnionsByID pins the whole seam
// end to end: a since_seq==0 subscriber sees the boundary FIRST, then a live
// upsert published AFTER the boundary arrives on the tail as an issue=16 frame.
// This is the subscribe-first, union-by-id self-heal: the client reads the board
// once (ListBoardIssues) and unions it with this tail, id-keyed, so an upsert in
// the connect window is never lost. A regression that sent the boundary after
// the tail, or dropped the issue fan-out, reddens this.
func TestSubscribeEventsBoundaryThenLiveUpsertUnionsByID(t *testing.T) {
	ctx := context.Background() // test root context
	client, _, issueBrd := newBoardServiceClient(t)

	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 0})

	// Boundary first: Seq==0, SnapshotSeq==0, no payload — before any tail.
	boundary := recvOne(t, stream)
	if boundary.GetSeq() != 0 || boundary.GetSnapshotSeq() != 0 || boundary.GetPayload() != nil {
		t.Fatalf("leading frame = seq %d snapshot_seq %d payload %T, want the boundary (0/0/nil)",
			boundary.GetSeq(), boundary.GetSnapshotSeq(), boundary.GetPayload())
	}

	// A live upsert AFTER the boundary (the subscribe-first window): it fans as an
	// issue=16 tail frame the client unions by id with its ListBoardIssues read.
	if err := issueBrd.PublishIssueUpdate(ctx, boardCanonicalIssue(7)); err != nil {
		t.Fatalf("PublishIssueUpdate(7): %v", err)
	}
	tail := recvOne(t, stream)
	issue := tail.GetIssue()
	if issue == nil {
		t.Fatalf("tail payload = %T, want an Issue (issue=16 fan-out)", tail.GetPayload())
	}
	if issue.GetNumber() != 7 {
		t.Fatalf("tail issue number = %d, want 7 (the live upsert)", issue.GetNumber())
	}
	if issue.GetId() == "" {
		t.Fatal("tail issue has empty id (nothing to union on)")
	}

	// The same upsert is now readable via ListBoardIssues (union-by-id closes to
	// a single entry — the read and the tail agree on the id).
	resp, err := client.ListBoardIssues(ctx, connect.NewRequest(&compassv1.ListBoardIssuesRequest{}))
	if err != nil {
		t.Fatalf("ListBoardIssues after live upsert: %v", err)
	}
	boardIssues := resp.Msg.GetIssues()
	if len(boardIssues) != 1 || boardIssues[0].GetId() != issue.GetId() {
		t.Fatalf("board = %d issues, want the single upserted id %q", len(boardIssues), issue.GetId())
	}
}
