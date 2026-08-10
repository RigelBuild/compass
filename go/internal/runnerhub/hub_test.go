//go:build unix

package runnerhub

// Deliver classification + the OQ6 rows the hub owns. Every test names the
// observable contract a plausible regression would break: a session frame's
// DISCONNECTED state must reach the lifecycle sink verbatim (not collapse to
// ERRORED); a session frame reaches the tail+lifecycle; an unknown frame must be
// counted, never dropped or errored; and a Runner-sequence gap must be flagged
// (in-transit loss the board surfaces).

import (
	"context"
	"testing"
	"time"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// OQ6 row 1: a session frame carrying DISCONNECTED extracts to an
// AgentSessionStatus with state DISCONNECTED on the lifecycle sink — NOT ERRORED.
// A bug that mapped a Runner disconnect straight to ERRORED (skipping the bounded
// reattach window) would redden the state assertion.
func TestDeliverSessionDisconnectedIsNotErrored(t *testing.T) {
	hub, life, tail := newHub()

	err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-1",
		Frame:     sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED),
	})
	if err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}

	statuses := life.snapshot()
	if len(statuses) != 1 {
		t.Fatalf("lifecycle sink saw %d statuses, want 1", len(statuses))
	}
	if got := statuses[0].GetState(); got != compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED {
		t.Fatalf("lifecycle state = %v, want DISCONNECTED (a disconnect must not collapse to ERRORED)", got)
	}
	if got := statuses[0].GetSessionId(); got != "sess-1" {
		t.Fatalf("lifecycle session id = %q, want sess-1", got)
	}
	// The frame still reaches the tail (a lifecycle frame is also a trace frame).
	if got := len(tail.snapshot()); got != 1 {
		t.Fatalf("tail sink saw %d frames, want 1", got)
	}
}

// A session frame with no transition (state UNSPECIFIED) reaches the tail but
// publishes NO lifecycle status — UNSPECIFIED means "trace only". A bug that
// published a bogus UNSPECIFIED status onto SubscribeEvents would redden the
// zero-status assertion.
func TestDeliverSessionTraceOnlyPublishesNoLifecycle(t *testing.T) {
	hub, life, tail := newHub()

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-1",
		Frame:     sessionTraceFrame("trace-body"),
	}); err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}

	if got := len(life.snapshot()); got != 0 {
		t.Fatalf("lifecycle sink saw %d statuses, want 0 (UNSPECIFIED is trace-only)", got)
	}
	frames := tail.snapshot()
	if len(frames) != 1 {
		t.Fatalf("tail sink saw %d frames, want 1", len(frames))
	}
	if got := frames[0].frame.GetTypedEvent().GetAssistantText().GetText(); got != "trace-body" {
		t.Fatalf("tail frame event = %q, want the relayed trace body", got)
	}
}

// An unset/unrecognized oneof variant is the "unknown frame": counted, not
// dropped, and NOT an error. A bug that errored (tearing down the relay) or
// silently dropped it (losing a contract-skew signal) would redden this.
func TestDeliverUnknownFrameIsCountedNotDroppedNotError(t *testing.T) {
	hub, life, tail := newHub()

	// An AgentFrame with no oneof variant set.
	if got := hub.UnknownFrames(); got != 0 {
		t.Fatalf("initial UnknownFrames = %d, want 0", got)
	}
	for i := uint64(1); i <= 3; i++ {
		if err := hub.Deliver(context.Background(), RunnerEvent{
			RunnerSeq: i,
			SessionID: "sess-1",
			Frame:     &compassv1internal.AgentFrame{},
		}); err != nil {
			t.Fatalf("Deliver(unknown) = %v, want nil (an unknown frame is not an error)", err)
		}
	}

	if got := hub.UnknownFrames(); got != 3 {
		t.Fatalf("UnknownFrames = %d, want 3 (each unknown frame counted, none dropped)", got)
	}
	// It reached no sink — it is neither conversation nor session.
	if got := len(life.snapshot()) + len(tail.snapshot()); got != 0 {
		t.Fatalf("an unknown frame reached a sink (%d total); it must be counted only", got)
	}
}

// A nil Frame is also an unknown frame (the GetFrame() oneof is nil), counted
// and not a panic — the relay must survive a malformed PublishEvents message.
func TestDeliverNilFrameIsUnknownNotPanic(t *testing.T) {
	hub := newHubOnly()
	if err := hub.Deliver(context.Background(), RunnerEvent{RunnerSeq: 1, SessionID: "s", Frame: nil}); err != nil {
		t.Fatalf("Deliver(nil frame) = %v, want nil", err)
	}
	if got := hub.UnknownFrames(); got != 1 {
		t.Fatalf("UnknownFrames = %d, want 1 (a nil frame is an unknown frame)", got)
	}
}

// OQ6 row 6: Runner-sequence gap detection. seq 1 then 3 flags a gap (2 lost in
// transit); a contiguous 1,2,3 does not. A bug in the > lastSeq+1 comparison
// (off-by-one either way) is caught by running both sequences.
func TestDeliverSequenceGapDetection(t *testing.T) {
	t.Run("gap flags", func(t *testing.T) {
		hub := newHubOnly()
		deliverSeq(t, hub, 1)
		if hub.SeenGap() {
			t.Fatal("SeenGap true after the first event; the baseline must not flag")
		}
		deliverSeq(t, hub, 3) // 2 skipped
		if !hub.SeenGap() {
			t.Fatal("SeenGap false after seq 1 then 3; a skipped seq must flag a gap")
		}
	})

	t.Run("contiguous does not flag", func(t *testing.T) {
		hub := newHubOnly()
		deliverSeq(t, hub, 1)
		deliverSeq(t, hub, 2)
		deliverSeq(t, hub, 3)
		if hub.SeenGap() {
			t.Fatal("SeenGap true on a contiguous 1,2,3; a gap-free stream must not flag")
		}
	})
}

// deliverSeq delivers one trace frame at seq n (a frame that touches no sink of
// interest, so the test isolates the sequence bookkeeping).
func deliverSeq(t *testing.T, hub *Hub, n uint64) {
	t.Helper()
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: n,
		SessionID: "seq-sess",
		Frame:     sessionTraceFrame(""),
	}); err != nil {
		t.Fatalf("Deliver(seq %d) = %v, want nil", n, err)
	}
}

// OQ6 row 4: a duplicate enrollment re-attaches the same single Runner rather
// than registering a second. The first enroll returns reattached=false, the
// second true, and the router is replaced (a fresh Sessions stream). A bug that
// registered a second Runner would return false twice.
func TestEnrollDuplicateReattaches(t *testing.T) {
	hub := newHubOnly()
	subj := store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}

	if reattached := hub.enroll("runner-1", subj); reattached {
		t.Fatal("first enroll reattached = true, want false (fresh registration)")
	}
	if reattached := hub.enroll("runner-1", subj); !reattached {
		t.Fatal("second enroll reattached = false, want true (single-Runner MVP re-attaches)")
	}
	// A router is resolvable after enrollment (a session command has a Runner to
	// serve it).
	if _, _, err := hub.routerFor("any"); err != nil {
		t.Fatalf("routerFor after enroll = %v, want a live router", err)
	}
}

// TestRunnerReadyHookFiresOnEachStreamAttach pins the SEA-1820 seam: a hook wired
// via SetRunnerReadyHook is invoked once per fireRunnerReady (the Sessions
// handler calls it each time a Runner's command stream attaches), on its own
// goroutine so a blocking seed cannot wedge the handler's receive loop. The
// first-launch supervisor seed hangs off this — Provision/Start need a Runner
// whose command stream can serve them, and that stream attaches only AFTER
// enroll returns, so firing on enroll would race the attach. Re-firing on a
// reconnect is by design; the seed itself is idempotent.
//
// Mutation: not firing (dropping the go hook() call in fireRunnerReady) hangs
// both receives and the test fails on the deadline.
func TestRunnerReadyHookFiresOnEachStreamAttach(t *testing.T) {
	hub := newHubOnly()

	fired := make(chan struct{}, 2)
	hub.SetRunnerReadyHook(func() { fired <- struct{}{} })

	hub.fireRunnerReady() // first stream attach
	hub.fireRunnerReady() // reconnect re-attach

	for i := range 2 {
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Fatalf("runner-ready hook fired %d times, want 2 (once per stream attach)", i)
		}
	}

	// A hub with no hook wired must fire without panicking (nil-safe).
	newHubOnly().fireRunnerReady()
}
