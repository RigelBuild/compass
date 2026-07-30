//go:build unix

package runnerhub

// Deliver classification + the OQ6 rows the hub owns. Every test names the
// observable contract a plausible regression would break: a session frame's
// DISCONNECTED state must reach the lifecycle sink verbatim (not collapse to
// ERRORED); a conversation frame must reach ONLY the comms sink and a session
// frame ONLY the tail+lifecycle (mis-routing is silent data loss); an unknown
// frame must be counted, never dropped or errored; and a Runner-sequence gap
// must be flagged (in-transit loss the board surfaces).

import (
	"context"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// OQ6 row 1: a session frame carrying DISCONNECTED extracts to an
// AgentSessionStatus with state DISCONNECTED on the lifecycle sink — NOT ERRORED.
// A bug that mapped a Runner disconnect straight to ERRORED (skipping the bounded
// reattach window) would redden the state assertion.
func TestDeliverSessionDisconnectedIsNotErrored(t *testing.T) {
	hub, _, life, tail := newHub()

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
	hub, _, life, tail := newHub()

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

// Routing: a conversation-posted frame hits ONLY the ConversationSink (as a
// posted message), and NOT the tail or lifecycle. Mis-routing a conversation to
// the tail would silently drop it from the comms store of record. The frame
// reaches the sink under the account the hub resolved the session to — the
// attribution the write-through commits under.
func TestDeliverConversationPostedRoutesToCommsOnly(t *testing.T) {
	hub, conv, life, tail := newHub()
	bindSession(hub, "sess-conv")

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-conv",
		Frame:     convPostedFrame("hello from agent"),
	}); err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}

	calls := conv.snapshot()
	if len(calls) != 1 {
		t.Fatalf("conversation sink saw %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.account != "acct-agent" {
		t.Fatalf("conversation account = %q, want the bound acct-agent (the hub resolves session->account before the sink)", c.account)
	}
	if c.sessionID != "sess-conv" {
		t.Fatalf("conversation session id = %q, want sess-conv", c.sessionID)
	}
	if c.posted == nil {
		t.Fatalf("conversation posted arg was nil; a posted frame must arrive as the posted arg")
	}
	if c.updated != nil {
		t.Fatalf("conversation updated arg was non-nil; a posted frame must leave updated nil")
	}
	if got := firstTextBlock(c.posted.GetMessage()); got != "hello from agent" {
		t.Fatalf("posted message text = %q, want the relayed text", got)
	}
	// The comms surface owns it exclusively.
	if got := len(tail.snapshot()); got != 0 {
		t.Fatalf("tail sink saw %d frames, want 0 (a conversation frame must not reach the tail)", got)
	}
	if got := len(life.snapshot()); got != 0 {
		t.Fatalf("lifecycle sink saw %d statuses, want 0", got)
	}
}

// A conversation-updated frame arrives on the ConversationSink as the UPDATED
// arg (posted nil) — the streaming-turn path — under the same resolved account.
// A bug that swapped the posted and updated args would redden the nil checks.
func TestDeliverConversationUpdatedRoutesAsUpdated(t *testing.T) {
	hub, conv, _, _ := newHub()
	bindSession(hub, "sess-conv")

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-conv",
		Frame:     convUpdatedFrame("streaming turn"),
	}); err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}

	calls := conv.snapshot()
	if len(calls) != 1 {
		t.Fatalf("conversation sink saw %d calls, want 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("conversation account = %q, want the bound acct-agent", calls[0].account)
	}
	if calls[0].updated == nil {
		t.Fatalf("updated arg was nil; an updated frame must arrive as the updated arg")
	}
	if calls[0].posted != nil {
		t.Fatalf("posted arg was non-nil; an updated frame must leave posted nil")
	}
	if got := firstTextBlock(calls[0].updated.GetMessage()); got != "streaming turn" {
		t.Fatalf("updated message text = %q, want the relayed text", got)
	}
}

// Routing: a session frame hits the tail + lifecycle and NOT the ConversationSink.
// The mirror of the conversation-routing test — the two together pin the full
// classification, so no bug can route a session frame into the comms store.
func TestDeliverSessionFrameDoesNotReachConversationSink(t *testing.T) {
	hub, conv, life, tail := newHub()

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-1",
		Frame:     sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("Deliver = %v, want nil", err)
	}

	if got := len(conv.snapshot()); got != 0 {
		t.Fatalf("conversation sink saw %d calls, want 0 (a session frame must not reach comms)", got)
	}
	if got := len(tail.snapshot()); got != 1 {
		t.Fatalf("tail sink saw %d frames, want 1", got)
	}
	if got := len(life.snapshot()); got != 1 {
		t.Fatalf("lifecycle sink saw %d statuses, want 1 (READY is a transition)", got)
	}
}

// An unset/unrecognized oneof variant is the "unknown frame": counted, not
// dropped, and NOT an error. A bug that errored (tearing down the relay) or
// silently dropped it (losing a contract-skew signal) would redden this.
func TestDeliverUnknownFrameIsCountedNotDroppedNotError(t *testing.T) {
	hub, conv, life, tail := newHub()

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
	if got := len(conv.snapshot()) + len(life.snapshot()) + len(tail.snapshot()); got != 0 {
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
