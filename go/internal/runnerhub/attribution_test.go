//go:build unix

package runnerhub

// DL-167 attribution: an AgentSessionStatus the hub publishes at deliverSession
// carries the session's agent_account_id, joined from the hub's own live
// binding, for as long as that binding stands — including the terminal STOPPED
// status, which resolves its account before Stop's unbindSession drops it. A
// status published for a session with no live binding carries no account (the
// stated residual gap). White-box (package runnerhub) so the test drives the
// unexported binding lifecycle and the deliverSession arm directly and asserts
// on the published status through the fake lifecycle sink.

import (
	"context"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
)

// TestDeliverSessionStampsAgentAccountFromBinding pins the DL-167 join: a
// lifecycle status published while the session's hub binding is live carries the
// bound agent_account_id, and this holds for the terminal STOPPED status (still
// bound at publish, since Stop's unbindSession has not run). An unbound session's
// status carries no account.
//
// Mutation: dropping the accountForSession join in deliverSession (publishing
// {SessionId, State} with no account, the pre-DL-167 shape) reddens every
// "carries acct-agent" assertion; the scan the reject-on-live check reads would
// then have no field to match on.
func TestDeliverSessionStampsAgentAccountFromBinding(t *testing.T) {
	hub, life, _ := newHub()
	bindSession(hub, "sess-1") // binds sess-1 -> testAgentAccount

	// A live-session transition carries the bound account.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 1, SessionID: "sess-1",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("Deliver(session READY) = %v, want nil", err)
	}
	// The terminal STOPPED status, still published while the binding stands
	// (deliverSession runs before any unbindSession) — the case DL-167 says bites
	// hardest, so it must still carry the account.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 2, SessionID: "sess-1",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED),
	}); err != nil {
		t.Fatalf("Deliver(session STOPPED) = %v, want nil", err)
	}

	got := life.snapshot()
	if len(got) != 2 {
		t.Fatalf("published statuses = %d, want 2", len(got))
	}
	for _, st := range got {
		if st.GetAgentAccountId() != string(testAgentAccount) {
			t.Fatalf("status %+v agent_account_id = %q, want %q (the live binding's account, incl. the terminal STOPPED status)",
				st, st.GetAgentAccountId(), testAgentAccount)
		}
	}

	// A session the hub never bound resolves to no account — the residual gap.
	if err := hub.Deliver(context.Background(), RunnerEvent{
		RunnerSeq: 3, SessionID: "never-bound",
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("Deliver(unbound READY) = %v, want nil", err)
	}
	got = life.snapshot()
	last := got[len(got)-1]
	if last.GetSessionId() != "never-bound" || last.GetAgentAccountId() != "" {
		t.Fatalf("unbound status = %+v, want {never-bound, no account} (the stated residual gap)", last)
	}
}
