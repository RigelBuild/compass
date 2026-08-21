//go:build unix

package server

// End-to-end coverage for GetAgentStatus served off the Bridge board projection,
// driven through a real connect-go client over the shipped h2c door (not a direct
// handler call). Every case pins that the handler wires req.session_id →
// board.Snapshot → resp.Statuses correctly, and that the terminal-filter /
// keep-by-id contract survives the RPC boundary. Board transitions are driven by
// calling brd.PublishSessionStatus directly — the board is the writer the
// RunnerHub feeds; the RPC is the reader.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/board"
)

// newAgentStatusClient stands up a service backed by a fresh board over a fresh
// bus, mounts it on the h2c door, and returns a connect client plus the board so
// the test drives transitions as the writer. The bus is closed at test end.
func newAgentStatusClient(t *testing.T) (compassv1connect.CompassServiceClient, *board.Projection) {
	t.Helper()
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	svc := newService("test", bus, nil, nil, brd, nil, nil)
	url := newH2CTestServer(t, svc)
	return newH2CClient(t, url), brd
}

// pub drives one board transition as the writer the RunnerHub feeds.
func pub(brd *board.Projection, id string, state compassv1.AgentSessionState) {
	brd.PublishSessionStatus(&compassv1.AgentSessionStatus{SessionId: id, State: state})
}

// TestGetAgentStatusByIDLive pins that a session_id-scoped request returns
// exactly that live session with its state, end to end through the client. A
// regression that ignored req.session_id (returning all) or dropped the state
// off the wire response reddens this.
func TestGetAgentStatusByIDLive(t *testing.T) {
	client, brd := newAgentStatusClient(t)
	pub(brd, "s1", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	pub(brd, "s2", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)

	resp, err := client.GetAgentStatus(context.Background(),
		connect.NewRequest(&compassv1.GetAgentStatusRequest{SessionId: "s1"}))
	if err != nil {
		t.Fatalf("GetAgentStatus(s1) = %v", err)
	}
	got := resp.Msg.GetStatuses()
	if len(got) != 1 || got[0].GetSessionId() != "s1" ||
		got[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		t.Errorf("GetAgentStatus(s1) statuses = %v, want single s1/WORKING", got)
	}
}

// TestGetAgentStatusByIDTerminal pins Matt's ruling end to end: a terminal
// (STOPPED) session is still returned when queried by id — a caller polling a
// known id learns the final state. A regression that evicted terminal sessions
// or filtered them even on a by-id query reddens this (empty result).
func TestGetAgentStatusByIDTerminal(t *testing.T) {
	client, brd := newAgentStatusClient(t)
	pub(brd, "s1", compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED)

	resp, err := client.GetAgentStatus(context.Background(),
		connect.NewRequest(&compassv1.GetAgentStatusRequest{SessionId: "s1"}))
	if err != nil {
		t.Fatalf("GetAgentStatus(s1) = %v", err)
	}
	got := resp.Msg.GetStatuses()
	if len(got) != 1 || got[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED {
		t.Errorf("GetAgentStatus(s1 terminal) statuses = %v, want single s1/STOPPED (terminal queryable by id)", got)
	}
}

// TestGetAgentStatusAllLiveSorted pins the unfiltered contract end to end: an
// empty session_id returns every LIVE session sorted, with terminals excluded.
// A regression that dropped the terminal filter (leaking STOPPED/ERRORED into
// the listing) or lost the sort reddens this.
func TestGetAgentStatusAllLiveSorted(t *testing.T) {
	client, brd := newAgentStatusClient(t)
	pub(brd, "s3", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)      // live
	pub(brd, "s1", compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED) // live
	pub(brd, "s2", compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED)      // terminal
	pub(brd, "s4", compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)      // terminal

	resp, err := client.GetAgentStatus(context.Background(),
		connect.NewRequest(&compassv1.GetAgentStatusRequest{}))
	if err != nil {
		t.Fatalf("GetAgentStatus(all) = %v", err)
	}
	got := resp.Msg.GetStatuses()
	wantIDs := []string{"s1", "s3"}
	if len(got) != len(wantIDs) || got[0].GetSessionId() != wantIDs[0] || got[1].GetSessionId() != wantIDs[1] {
		ids := make([]string, 0, len(got))
		for _, s := range got {
			ids = append(ids, s.GetSessionId())
		}
		t.Fatalf("GetAgentStatus(all) ids = %v, want %v (live only, sorted)", ids, wantIDs)
	}
}

// TestGetAgentStatusUnseenEmpty pins that a request for an id the board never saw
// returns an empty Statuses slice — not a nil deref, not a fall-through to the
// all-sessions listing. A regression that treated "unknown id" like "no filter"
// would return the whole board here.
func TestGetAgentStatusUnseenEmpty(t *testing.T) {
	client, brd := newAgentStatusClient(t)
	pub(brd, "s1", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)

	resp, err := client.GetAgentStatus(context.Background(),
		connect.NewRequest(&compassv1.GetAgentStatusRequest{SessionId: "nope"}))
	if err != nil {
		t.Fatalf("GetAgentStatus(nope) = %v", err)
	}
	if got := resp.Msg.GetStatuses(); len(got) != 0 {
		t.Errorf("GetAgentStatus(unseen) statuses = %v, want empty", got)
	}
}

// TestGetAgentStatusNilBoardNoPanic pins the no-board serving path: a service
// built with a nil board answers an empty response instead of panicking on the
// nil Snapshot call. A regression that dropped the nil guard would crash the
// handler (500 / panic), reddening this as an RPC error.
func TestGetAgentStatusNilBoardNoPanic(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("test", bus, nil, nil, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	resp, err := client.GetAgentStatus(context.Background(),
		connect.NewRequest(&compassv1.GetAgentStatusRequest{SessionId: "s1"}))
	if err != nil {
		t.Fatalf("GetAgentStatus(nil board) = %v, want empty response no error", err)
	}
	if got := resp.Msg.GetStatuses(); len(got) != 0 {
		t.Errorf("GetAgentStatus(nil board) statuses = %v, want empty", got)
	}
}
