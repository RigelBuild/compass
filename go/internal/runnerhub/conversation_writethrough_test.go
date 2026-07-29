//go:build unix

package runnerhub

// Deliver's conversation arms after the T3 write-through landed (SEA-1364): the
// session->account resolution that precedes the sink, and the refusal posture
// that keeps a rejected frame from tearing down the Runner's PublishEvents
// stream. Two invariants, each with a plausible regression:
//
//   - FAIL CLOSED. An unbound session must NOT reach the sink at all. Before
//     T3 the sink was a log line, so an unresolved session cost nothing; now the
//     sink COMMITS, and a frame that arrived with no resolvable account must
//     never be attributed to a default, a stale binding, or the bootstrap admin.
//     A regression that resolved to "" and let the call through would push the
//     decision down to the comms layer's errNoActor guard — which would catch
//     it, but one layer too late and only by luck.
//
//   - REFUSALS ARE NON-FATAL. A frame the comms layer refuses (cross-account,
//     revoked member, malformed ask, empty message id) is ONE bad frame, not a
//     broken transport. It must be dropped and logged, and the stream must go on
//     to commit the next valid frame — the same posture countUnknown takes for an
//     unrecognized variant (hub.go). Only a genuine store/transaction failure
//     tears the stream down, because that one is not frame-specific and retrying
//     the relay is the right answer.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// THE fail-closed test. A conversation frame for a session the hub never bound
// (never provisioned, already stopped, or dropped by a Runner reconnect) reaches
// NO sink and is refused. The sink seeing zero calls is the load-bearing
// assertion: it proves the refusal happens at the resolution site, before any
// commit is attempted, so no default identity can be substituted downstream.
//
// Mutation: resolve to a zero AccountID and call the sink anyway → the sink
// records a call and this reddens. Mutation: fall back to any default account →
// same, plus the account assertion in the happy-path test would drift.
func TestDeliverConversationUnboundSessionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame *compassv1internal.AgentFrame
	}{
		{"posted", convPostedFrame("never committed")},
		{"updated", convUpdatedFrame("never committed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, conv, life, tail := newHub()
			// Deliberately NO bindSession: this id resolves to nothing.

			err := hub.Deliver(context.Background(), RunnerEvent{
				RunnerSeq: 1,
				SessionID: "never-bound",
				Frame:     tc.frame,
			})
			if err != nil {
				t.Fatalf("Deliver(unbound session) = %v, want nil (a refusal is a non-fatal drop, not a stream teardown)", err)
			}

			if got := len(conv.snapshot()); got != 0 {
				t.Fatalf("conversation sink saw %d calls for an unbound session, want 0 — an unresolvable frame must never reach the write-through", got)
			}
			if got := len(life.snapshot()) + len(tail.snapshot()); got != 0 {
				t.Fatalf("an unbound conversation frame reached %d other sink calls, want 0", got)
			}
			if got := hub.RefusedFrames(); got != 1 {
				t.Fatalf("RefusedFrames = %d, want 1 (a refusal is counted, never silently swallowed)", got)
			}
		})
	}
}

// A frame whose session WAS bound but whose commit the comms layer refuses is
// also non-fatal: Deliver returns nil, the refusal is counted, and — the point
// of the test — the SAME hub then commits a following valid frame. Without the
// survival half, a bug that returned the refusal as a stream error would still
// pass a "returns nil" assertion if it only broke on the next frame.
//
// Each subtest names one refusal class the comms layer produces. They are
// indistinguishable to the hub (it sees a Connect error and drops), which is
// exactly why the hub tests them as a class rather than re-deriving comms' authz.
func TestDeliverConversationRefusalIsNonFatalAndStreamSurvives(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"cross-account update", connect.NewError(connect.CodeNotFound, errors.New("message not found"))},
		{"revoked member update", connect.NewError(connect.CodeNotFound, errors.New("message not found"))},
		{"malformed ask", connect.NewError(connect.CodeInvalidArgument, errors.New("ask has no ask_id"))},
		{"empty message id", connect.NewError(connect.CodeInvalidArgument, errors.New("message id is required"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, conv, _, _ := newHub()
			bindSession(hub, "sess-1")
			conv.err = tc.err

			if err := hub.Deliver(context.Background(), RunnerEvent{
				RunnerSeq: 1,
				SessionID: "sess-1",
				Frame:     convUpdatedFrame("refused"),
			}); err != nil {
				t.Fatalf("Deliver(refused frame) = %v, want nil (one bad frame must not tear down the Runner stream)", err)
			}
			if got := hub.RefusedFrames(); got != 1 {
				t.Fatalf("RefusedFrames = %d, want 1 (a refusal is observable, never silently swallowed)", got)
			}

			// THE survival assertion: the stream goes on to commit the next
			// valid frame on the same hub.
			conv.err = nil
			if err := hub.Deliver(context.Background(), RunnerEvent{
				RunnerSeq: 2,
				SessionID: "sess-1",
				Frame:     convPostedFrame("the next good frame"),
			}); err != nil {
				t.Fatalf("Deliver(following valid frame) = %v, want nil — the stream must survive a refusal", err)
			}
			calls := conv.snapshot()
			if len(calls) != 2 {
				t.Fatalf("conversation sink saw %d calls, want 2 (the refused attempt and the following commit)", len(calls))
			}
			if got := firstTextBlock(calls[1].posted.GetMessage()); got != "the next good frame" {
				t.Fatalf("the frame after the refusal carried %q, want the following valid body", got)
			}
			if got := hub.RefusedFrames(); got != 1 {
				t.Fatalf("RefusedFrames = %d after a successful commit, want still 1", got)
			}
		})
	}
}

// The one error class that IS fatal: a genuine store/transaction failure is not
// frame-specific, so it ends the stream and the Runner retries the relay rather
// than losing the frame. This is the counterweight that keeps the non-fatal
// posture above from degenerating into "swallow everything" — a bug that
// dropped store faults too would make an outage look like a quiet stream.
func TestDeliverConversationStoreFailureTearsDownTheStream(t *testing.T) {
	hub, conv, _, _ := newHub()
	bindSession(hub, "sess-1")
	conv.err = connect.NewError(connect.CodeInternal, errors.New("commit tx: connection reset"))

	err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1,
		SessionID: "sess-1",
		Frame:     convPostedFrame("lost to an outage"),
	})
	if err == nil {
		t.Fatal("Deliver(store failure) = nil, want an error (a store fault must end the stream so the Runner retries)")
	}
	if got := hub.RefusedFrames(); got != 0 {
		t.Fatalf("RefusedFrames = %d, want 0 (a store fault is a teardown, not a per-frame refusal)", got)
	}
}

// The reconnect consequence, pinned deliberately rather than left to be
// rediscovered as a bug: enroll() clears ALL session bindings, so a frame
// in-flight across a Runner reconnect resolves to nothing and fails closed. That
// is the RULED behavior (a restarted Runner can re-mint a still-bound session id;
// clearing prevents attributing the new session's words to the old account). The
// fix is Runner-side resume, NOT a fallback here — a test that asserted the frame
// still committed after a reconnect would be asserting the security hole.
func TestDeliverConversationAfterRunnerReconnectFailsClosed(t *testing.T) {
	hub, conv, _, _ := newHub()
	bindSession(hub, "sess-1")

	// Positive control: while the binding is live the frame commits, so the
	// refusal below is the reconnect and nothing else.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1", Frame: convPostedFrame("before the reconnect"),
	}); err != nil {
		t.Fatalf("Deliver before the reconnect = %v, want nil", err)
	}
	if got := len(conv.snapshot()); got != 1 {
		t.Fatalf("conversation sink saw %d calls before the reconnect, want 1", got)
	}

	// The Runner reconnects: enroll drops every binding (OQ-2, ratified).
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})

	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1", Frame: convPostedFrame("after the reconnect"),
	}); err != nil {
		t.Fatalf("Deliver after the reconnect = %v, want nil (fail closed is a drop, not a teardown)", err)
	}
	if got := len(conv.snapshot()); got != 1 {
		t.Fatalf("conversation sink saw %d calls after the reconnect, want still 1 — a re-minted session id must NOT inherit the old account", got)
	}
	if got := hub.RefusedFrames(); got != 1 {
		t.Fatalf("RefusedFrames = %d, want 1", got)
	}
}
