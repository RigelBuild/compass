//go:build unix

package server

// Default-lane coverage for part 4b's connect-time re-snapshot seam: the
// SubscribeEvents snapshot-boundary frame and the ListBoardIssues handler's
// wiring/reachability, driven through a real connect-go client over the shipped
// h2c door (not a direct handler call). The populated-board assertions (a real
// PG-rehydrated Snapshot, boundary-then-tail ordering, union-by-id) live in the
// pgtest lane (service_board_pgtest_test.go) where a store can seed the board;
// here the board is nil (empty), so these cases pin the seam's frame ordering,
// the empty-board boundary, and the handler's nil-guard reachability.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// TestSubscribeEventsSinceZeroSendsBoundaryFirstEvenEmpty pins the boundary
// contract at its hardest case: a since_seq==0 subscribe against an EMPTY bus
// still leads with exactly one snapshot-boundary frame (Seq==0, SnapshotSeq==0,
// no payload) before any tail event. The boundary is also what flushes the
// stream's response headers, so the open is observable without priming an event.
// A regression that gated the boundary on a non-empty ring, or dropped it, would
// leave the handler tailing silently and this first Receive would hang (deadline
// safety net) or deliver a payload frame.
func TestSubscribeEventsSinceZeroSendsBoundaryFirstEvenEmpty(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)

	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 0})

	boundary := recvOne(t, stream)
	if boundary.GetSeq() != 0 {
		t.Fatalf("boundary seq = %d, want 0 (a positional control marker)", boundary.GetSeq())
	}
	if boundary.GetSnapshotSeq() != 0 {
		t.Fatalf("boundary snapshot_seq = %d, want 0 (board unversioned in v1)", boundary.GetSnapshotSeq())
	}
	if boundary.GetPayload() != nil {
		t.Fatalf("boundary payload = %T, want nil (the boundary carries no payload)", boundary.GetPayload())
	}
	if boundary.GetInstanceEpoch() != bus.InstanceEpoch() {
		t.Fatalf("boundary epoch = %d, want %d (the live bus epoch)", boundary.GetInstanceEpoch(), bus.InstanceEpoch())
	}

	// The boundary precedes the tail: a post-subscribe publish arrives next,
	// carrying a positioned seq and a payload — proving the boundary led.
	bus.Publish(statusEvent())
	tail := recvOne(t, stream)
	if tail.GetSeq() != 1 {
		t.Fatalf("first tail msg seq = %d, want 1 (the seq after the empty snapshot)", tail.GetSeq())
	}
	if tail.GetServerStatus() == nil {
		t.Fatalf("first tail payload = %T, want ServerStatus", tail.GetPayload())
	}
}

// TestSubscribeEventsSinceNonZeroSendsNoBoundary pins the guard: a positioned
// (since_seq>0) resume does NOT receive a boundary — it is not a fresh snapshot,
// so the first frame is the replayed tail, not the ordering marker. A regression
// that sent the boundary unconditionally would put a Seq==0 no-payload frame
// ahead of the replay here.
func TestSubscribeEventsSinceNonZeroSendsNoBoundary(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// Two events pre-subscribe so a since_seq==1 cursor has seq 2 to replay.
	bus.Publish(statusEvent())
	bus.Publish(statusEvent())

	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 1, InstanceEpoch: bus.InstanceEpoch()})

	// First frame is the replayed event at seq 2, NOT a boundary: it carries a
	// positioned seq and a real payload.
	first := recvOne(t, stream)
	if first.GetSeq() != 2 {
		t.Fatalf("first msg seq = %d, want 2 (the replayed event, no leading boundary)", first.GetSeq())
	}
	if first.GetServerStatus() == nil {
		t.Fatalf("first msg payload = %T, want ServerStatus (a positioned event, not the boundary)", first.GetPayload())
	}
}

// TestListBoardIssuesReachableEmptyBoard pins the handler's wiring and nil-guard:
// ListBoardIssues is reachable by an authenticated caller (the h2c door mounts no
// admin gate, matching SubscribeEvents' authenticatedOpen classification enforced
// in classify_exhaustive_test), returns a non-error empty response over a nil
// projection rather than the embedded CodeUnimplemented default or a panic. A
// regression that left the handler as the Unimplemented stub, or dropped the nil
// guard, reddens this (CodeUnimplemented / a 500 panic).
func TestListBoardIssuesReachableEmptyBoard(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	resp, err := client.ListBoardIssues(context.Background(),
		connect.NewRequest(&compassv1.ListBoardIssuesRequest{}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			t.Fatalf("ListBoardIssues = CodeUnimplemented, want a real handler (the 4b wiring is missing)")
		}
		t.Fatalf("ListBoardIssues over the open door: %v", err)
	}
	if got := resp.Msg.GetIssues(); len(got) != 0 {
		t.Fatalf("ListBoardIssues on a nil board = %d issues, want 0", len(got))
	}
}

// TestListBoardIssuesIgnoresRequestSnapshotSeqOnNilBoard pins the unversioned-v1
// contract at the wiring level: the handler IGNORES the request snapshot_seq — a
// zero and a non-zero snapshot_seq both return the same (here empty) board. The
// populated-board form of this property is proven in the pgtest lane; this pins
// that the request field never gates the handler.
func TestListBoardIssuesIgnoresRequestSnapshotSeqOnNilBoard(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	zero, err := client.ListBoardIssues(context.Background(),
		connect.NewRequest(&compassv1.ListBoardIssuesRequest{SnapshotSeq: 0}))
	if err != nil {
		t.Fatalf("ListBoardIssues(snapshot_seq=0): %v", err)
	}
	nonZero, err := client.ListBoardIssues(context.Background(),
		connect.NewRequest(&compassv1.ListBoardIssuesRequest{SnapshotSeq: 42}))
	if err != nil {
		t.Fatalf("ListBoardIssues(snapshot_seq=42): %v", err)
	}
	if len(zero.Msg.GetIssues()) != len(nonZero.Msg.GetIssues()) {
		t.Fatalf("board differed by request snapshot_seq: seq0 = %d issues, seq42 = %d issues; want identical (field ignored)",
			len(zero.Msg.GetIssues()), len(nonZero.Msg.GetIssues()))
	}
}
