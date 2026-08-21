//go:build unix

package board

// Integration seam between the RunnerHub and the real Bridge board — the central
// design claim: a session lifecycle frame delivered through the REAL hub is
// BOTH recorded into the board (queryable by GetAgentStatus's Snapshot) AND
// fanned onto SubscribeEvents, off one source of truth. The board imports
// runnerhub here (runnerhub does not import board, so there is no cycle) and
// wires itself in as the hub's LifecycleSink — the structural contract serve.go
// relies on.
//
// The negative (UNSPECIFIED skip) half is proven deterministically with a real
// sentinel frame, mirroring projection_test.go's sentinel idiom — never a timer.

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
)

// noopTailSink is a do-nothing SessionTailSink: a session frame is also a trace
// frame, so the hub relays it here; the seam under test does not assert on it.
type noopTailSink struct{}

func (noopTailSink) RelaySessionFrame(_ string, _ *compassv1internal.SessionFrame) {}

// stateFrame wraps a SessionFrame carrying only a lifecycle transition, mirroring
// runnerhub/helpers_test.go's sessionStateFrame (package-private there).
func stateFrame(state compassv1.AgentSessionState) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{State: state},
		},
	}
}

// TestDeliverStateFrameRecordsAndFans pins the seam: a WORKING session frame
// through the real hub records s1/WORKING into the board AND fans the same
// AgentSessionStatus onto the bus — the two surfaces must agree. A regression
// that wired the hub to a fan-out-only sink (the busLifecycleSink it replaces,
// which left the board empty) would leave Snapshot("s1") empty; one that recorded but
// stopped fanning would starve the bus subscription. Each half reddens
// independently.
func TestDeliverStateFrameRecordsAndFans(t *testing.T) {
	brd, bus := newBoard(t)
	hub := runnerhub.NewHub(brd, noopTailSink{}, nil, nil)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	if err := hub.Deliver(context.Background(), runnerhub.RunnerEvent{
		RunnerSeq: 1,
		SessionID: "s1",
		Frame:     stateFrame(working),
	}); err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}

	// Fan-out surface: the bus subscriber saw s1/WORKING.
	got := recvStatus(t, sub.Live)
	if got.GetSessionId() != "s1" || got.GetState() != working {
		t.Errorf("bus event = %s/%v, want s1/WORKING (hub must fan onto SubscribeEvents)", got.GetSessionId(), got.GetState())
	}

	// Record surface: the board holds s1/WORKING, queryable by id.
	snap := brd.Snapshot("s1")
	if len(snap) != 1 || snap[0].GetState() != working {
		t.Errorf("Snapshot(s1) = %v, want single s1/WORKING (hub must record into the board)", snap)
	}
}

// TestDeliverUnspecifiedFrameNeitherRecordsNorFans pins the UNSPECIFIED skip end
// to end through the hub: a trace-only session frame (state UNSPECIFIED) neither
// records a board row nor fans a bus event. The no-record half is Snapshot("s1")
// empty; the no-fan half is proven deterministically — a real sentinel frame
// delivered after must be the FIRST bus event, so a bogus UNSPECIFIED fan-out
// would surface as a wrong first event, not a race. A regression that synthesized
// an AgentSessionStatus for an UNSPECIFIED frame reddens both halves.
func TestDeliverUnspecifiedFrameNeitherRecordsNorFans(t *testing.T) {
	brd, bus := newBoard(t)
	hub := runnerhub.NewHub(brd, noopTailSink{}, nil, nil)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	if err := hub.Deliver(context.Background(), runnerhub.RunnerEvent{
		RunnerSeq: 1,
		SessionID: "s1",
		Frame:     stateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED),
	}); err != nil {
		t.Fatalf("Deliver(UNSPECIFIED) = %v, want nil", err)
	}

	if snap := brd.Snapshot("s1"); len(snap) != 0 {
		t.Errorf("Snapshot(s1) = %v after UNSPECIFIED frame, want empty (nothing recorded)", idsOf(snap))
	}

	// Sentinel: a real WORKING frame must be the first bus event.
	if err := hub.Deliver(context.Background(), runnerhub.RunnerEvent{
		RunnerSeq: 2,
		SessionID: "sentinel",
		Frame:     stateFrame(working),
	}); err != nil {
		t.Fatalf("Deliver(sentinel) = %v, want nil", err)
	}
	if got := recvStatus(t, sub.Live); got.GetSessionId() != "sentinel" {
		t.Errorf("first bus event = %s, want sentinel (UNSPECIFIED frame must not fan)", got.GetSessionId())
	}
}
