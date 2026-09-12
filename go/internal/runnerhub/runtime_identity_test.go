//go:build unix

package runnerhub

// The enrollment-carried runtime tier + egress posture reach BOTH production
// render paths. A Runner declares its tier and posture ONCE at enrollment; the
// hub stamps them onto every session status it publishes. These tests drive a
// session lifecycle frame through the REAL hub wired to the REAL Bridge board
// (board.Projection as the LifecycleSink), so one delivery exercises the two
// surfaces off one source of truth:
//   - the UI's SubscribeEvents fan-out (bus.Subscribe -> Live), and
//   - the CLI's GetAgentStatus snapshot (board.Snapshot).
// White-box (package runnerhub) so the test drives the unexported enroll/bind
// lifecycle directly. Sleep-free: the delivery records+fans synchronously under
// the projection's lock, so every assertion reads a settled fact.

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/store"
)

// newHubWithBoard wires the real Bridge board as the hub's LifecycleSink over a
// fresh bus, so a delivered lifecycle frame both fans onto SubscribeEvents and
// records into the snapshot. The bus is closed at test end.
func newHubOverBoard(t *testing.T) (*Hub, *board.Projection, *events.Bus[*compassv1.SubscribeEventsResponse]) {
	t.Helper()
	bus := events.NewBus[*compassv1.SubscribeEventsResponse]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	return NewHub(brd, &fakeTailSink{}, nil, discardLogger()), brd, bus
}

// recvSessionStatus reads one live bus event and returns its AgentSessionStatus,
// failing fast on an early close or a stall.
func recvSessionStatus(t *testing.T, ch <-chan events.Stamped[*compassv1.SubscribeEventsResponse]) *compassv1.AgentSessionStatus {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("live channel closed before an event arrived")
		}
		got := e.Payload.GetAgentSessionStatus()
		if got == nil {
			t.Fatalf("live event carried a non-AgentSessionStatus payload: %v", e.Payload)
		}
		return got
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a live event")
		return nil
	}
}

// identitySessionID is the one session these tests drive; the tier/posture
// stamp is Runner-wide, so a second session would assert nothing new.
const identitySessionID = "sess-1"

// deliverWorking pushes a WORKING lifecycle frame for the test session through
// the hub.
func deliverWorking(t *testing.T, hub *Hub, seq uint64) {
	t.Helper()
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: seq, SessionID: identitySessionID,
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING),
	}); err != nil {
		t.Fatalf("Deliver(WORKING) = %v, want nil", err)
	}
}

// TestEnrolledTierAndPostureReachBothRenderPaths pins the central fix: a Runner
// enrolled as HOST/UNENFORCED has both facts stamped onto a session's status on
// BOTH surfaces — the SubscribeEvents fan-out (the UI path) and the board
// snapshot (the CLI's GetAgentStatus path) — off one delivery.
//
// Negative control: reverting deliverSession to publish {SessionId, State,
// AgentAccountId} with no tier/posture (or reverting runnerRuntimeIdentity to
// return UNSPECIFIED) reddens every assertion below — observed
// "bus event runtime_tier = RUNTIME_TIER_UNSPECIFIED, want RUNTIME_TIER_HOST".
func TestEnrolledTierAndPostureReachBothRenderPaths(t *testing.T) {
	hub, brd, bus := newHubOverBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"},
		compassv1.RuntimeTier_RUNTIME_TIER_HOST, compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED)
	bindSession(hub, identitySessionID) // bind AFTER enroll (enroll clears bindings)

	deliverWorking(t, hub, 1)

	// UI path: the SubscribeEvents fan-out carries the enrolled tier + posture.
	got := recvSessionStatus(t, sub.Live)
	if got.GetRuntimeTier() != compassv1.RuntimeTier_RUNTIME_TIER_HOST {
		t.Errorf("bus event runtime_tier = %v, want RUNTIME_TIER_HOST", got.GetRuntimeTier())
	}
	if got.GetEgressPosture() != compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED {
		t.Errorf("bus event egress_posture = %v, want EGRESS_POSTURE_UNENFORCED", got.GetEgressPosture())
	}

	// CLI path: the board snapshot (GetAgentStatus) carries them too.
	snap := brd.Snapshot(identitySessionID)
	if len(snap) != 1 {
		t.Fatalf("Snapshot(sess-1) = %d entries, want 1", len(snap))
	}
	if snap[0].GetRuntimeTier() != compassv1.RuntimeTier_RUNTIME_TIER_HOST {
		t.Errorf("snapshot runtime_tier = %v, want RUNTIME_TIER_HOST", snap[0].GetRuntimeTier())
	}
	if snap[0].GetEgressPosture() != compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED {
		t.Errorf("snapshot egress_posture = %v, want EGRESS_POSTURE_UNENFORCED", snap[0].GetEgressPosture())
	}
}

// TestReEnrollUpdatesStampedTierAndPosture pins the reattach invariant: a status
// published after a Runner re-enrolls with DIFFERENT values must reflect the
// NEWLY enrolled Runner's tier + posture, never the previous Runner's. A
// re-enroll clears bindings, so the session is re-bound before the second frame.
//
// Negative control: making runnerRuntimeIdentity read a cached first-enroll value
// (or dropping the h.mu read so it races the reattach) reddens the assertion —
// observed "runtime_tier = RUNTIME_TIER_HOST, want RUNTIME_TIER_PODMAN".
func TestReEnrollUpdatesStampedTierAndPosture(t *testing.T) {
	hub, brd, _ := newHubOverBoard(t)

	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"},
		compassv1.RuntimeTier_RUNTIME_TIER_HOST, compassv1.EgressPosture_EGRESS_POSTURE_UNENFORCED)
	bindSession(hub, identitySessionID)
	deliverWorking(t, hub, 1)

	// The Runner reconnects declaring a different tier + posture.
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"},
		compassv1.RuntimeTier_RUNTIME_TIER_PODMAN, compassv1.EgressPosture_EGRESS_POSTURE_ARMED)
	bindSession(hub, identitySessionID)
	deliverWorking(t, hub, 2)

	snap := brd.Snapshot(identitySessionID)
	if len(snap) != 1 {
		t.Fatalf("Snapshot(sess-1) = %d entries, want 1", len(snap))
	}
	if snap[0].GetRuntimeTier() != compassv1.RuntimeTier_RUNTIME_TIER_PODMAN {
		t.Errorf("runtime_tier = %v, want RUNTIME_TIER_PODMAN (the re-enrolled Runner's value, not the stale HOST)", snap[0].GetRuntimeTier())
	}
	if snap[0].GetEgressPosture() != compassv1.EgressPosture_EGRESS_POSTURE_ARMED {
		t.Errorf("egress_posture = %v, want EGRESS_POSTURE_ARMED (the re-enrolled Runner's value, not the stale UNENFORCED)", snap[0].GetEgressPosture())
	}
}

// TestEnrollWithNoRuntimeIdentityYieldsUnspecified pins the fail-honest default:
// a Runner that declares nothing (zero values) yields UNSPECIFIED on both fields
// — never a plausible default like PODMAN or ARMED. A security surface that
// guesses is worse than one that says it does not know.
//
// Negative control: defaulting runnerRuntimeIdentity to PODMAN/ARMED when the
// enrolled values are zero reddens both assertions — observed "runtime_tier =
// RUNTIME_TIER_PODMAN, want RUNTIME_TIER_UNSPECIFIED".
func TestEnrollWithNoRuntimeIdentityYieldsUnspecified(t *testing.T) {
	hub, brd, _ := newHubOverBoard(t)

	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"},
		compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindSession(hub, identitySessionID)
	deliverWorking(t, hub, 1)

	snap := brd.Snapshot(identitySessionID)
	if len(snap) != 1 {
		t.Fatalf("Snapshot(sess-1) = %d entries, want 1", len(snap))
	}
	if snap[0].GetRuntimeTier() != compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED {
		t.Errorf("runtime_tier = %v, want RUNTIME_TIER_UNSPECIFIED (never a guessed default)", snap[0].GetRuntimeTier())
	}
	if snap[0].GetEgressPosture() != compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED {
		t.Errorf("egress_posture = %v, want EGRESS_POSTURE_UNSPECIFIED (never a guessed default)", snap[0].GetEgressPosture())
	}
}
