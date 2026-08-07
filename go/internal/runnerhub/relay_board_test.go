//go:build unix

package runnerhub

// The agent-board Server leg (agent primary lifecycle T3-a, fail-closed authz —
// the load-bearing security leg, exactly as RelayLifecycleCall / RelayCommsCall).
// Every test here defends one invariant of the session->account resolution +
// RelayBoardCall handler: the Runner asserts no account, so the SERVER's binding
// is the sole authority for whose account a relayed board write runs under. A
// regression that let an unbound, stopped, or reconnect-dropped session resolve
// to ANY account — or delegated under the wrong account, or turned a tool
// failure into a transport teardown — must redden a test below.
//
// White-box (package runnerhub) so the tests drive the unexported binding
// lifecycle and the resolution edge directly, asserting the account attribution
// through the fake BoardCaller. Sleep-free: the hub calls the caller inline, so
// every assertion reads a synchronously-recorded fact.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// relaySetIssueState builds a RelayBoardCallRequest carrying a set-issue-state
// variant under callID.
func relaySetIssueState(sessionID, callID string, req *compassv1internal.SetIssueStateRequest) *compassv1internal.RelayBoardCallRequest {
	return &compassv1internal.RelayBoardCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.BoardCallRequest{
			CallId: callID,
			Call:   &compassv1internal.BoardCallRequest_SetIssueState{SetIssueState: req},
		},
	}
}

// 1. An unbound session fails closed CodeNotFound and NEVER reaches the caller —
// no delegation is attempted for a session the hub has no binding for. This is
// the core fail-closed guard: a session_id on the wire selects an account, it
// never carries one, so an id the hub never bound resolves to nothing.
//
// Mutation: hardcode accountForSession to return a fixed account (ok=true) and
// this test fails twice over — the error becomes nil and the caller records a
// call.
func TestRelayBoardCallUnboundSessionFailsClosedNotFound(t *testing.T) {
	hub, fake := newHubWithBoard()

	_, err := hub.RelayBoardCall(context.Background(), relaySetIssueState("never-bound", "bc-1", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}))
	if err == nil {
		t.Fatal("RelayBoardCall for an unbound session = nil error, want CodeNotFound (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unbound-session error code = %v, want NotFound", got)
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("caller was invoked %d times for an unbound session, want 0 (no delegation attempt)", len(calls))
	}
}

// 2. A hub with no BoardCaller wired fails RelayBoardCall closed with
// CodeUnavailable — the board write leg is not mounted, never a silent success.
// This is checked BEFORE session resolution, so even a bound session gets
// Unavailable on a caller-less hub.
//
// Mutation: reordering the checks so resolution runs first would change the code
// to NotFound for an unbound+nil case — the second sub-assertion (unbound session
// + nil caller still Unavailable) reddens that reorder.
func TestRelayBoardCallNilCallerIsUnavailableBeforeResolution(t *testing.T) {
	t.Run("bound session still Unavailable", func(t *testing.T) {
		hub := newHubOnly()  // no BoardCaller wired
		bindLiveSession(hub) // a live binding exists, proving the nil guard precedes resolution

		_, err := hub.RelayBoardCall(context.Background(), relaySetIssueState("sess-1", "bc-2", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}))
		if err == nil {
			t.Fatal("RelayBoardCall on a caller-less hub = nil error, want CodeUnavailable")
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("nil-caller (bound session) error code = %v, want Unavailable", got)
		}
	})
	t.Run("unbound session still Unavailable (nil-check precedes resolution)", func(t *testing.T) {
		hub := newHubOnly() // no BoardCaller wired, no binding

		_, err := hub.RelayBoardCall(context.Background(), relaySetIssueState("never-bound", "bc-2b", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}))
		if err == nil {
			t.Fatal("RelayBoardCall on a caller-less hub (unbound) = nil error, want CodeUnavailable")
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("nil-caller (unbound session) error code = %v, want Unavailable, not NotFound (proves nil-check precedes resolution)", got)
		}
	})
}

// 3. THE core authz test: the RESOLVED caller account (the hub's own binding)
// reaches the caller — never a request field, never a literal admin id. A write
// for the bound session_id delegates under acct-agent, the account the hub bound.
//
// Mutation: passing a request field or a literal admin id instead of the
// resolved account reddens the account assertion.
func TestRelayBoardCallDelegatesUnderResolvedCallerAccount(t *testing.T) {
	hub, fake := newHubWithBoard()
	fake.resp = &compassv1internal.SetIssueStateResponse{Issue: &compassv1.Issue{Id: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}}
	bindLiveSession(hub) // sess-1 -> acct-agent

	_, err := hub.RelayBoardCall(context.Background(), relaySetIssueState("sess-1", "bc-3", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_TODO}))
	if err != nil {
		t.Fatalf("RelayBoardCall(set_issue_state) = %v, want success", err)
	}
	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want exactly 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("caller attributed to %q, want the bound caller account acct-agent (never request-asserted, never admin)", calls[0].account)
	}
	if calls[0].setIssueState.GetIssueId() != "iss-1" {
		t.Fatalf("caller received issue_id %q, want the request's iss-1", calls[0].setIssueState.GetIssueId())
	}
}

// 4. A caller (tool-level) error surfaces IN-BAND in a SUCCESSFUL (nil-err)
// response as the BoardCallError variant — the agent renders it and the
// transport survives. Only a resolution miss / no-caller is a Connect error.
//
// Mutation: returning the caller error as a Connect error (instead of in-band)
// reddens the err==nil assertion.
func TestRelayBoardCallToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, fake := newHubWithBoard()
	fake.err = connect.NewError(connect.CodeNotFound, errors.New("issue \"iss-x\" does not exist"))
	bindLiveSession(hub)

	resp, err := hub.RelayBoardCall(context.Background(), relaySetIssueState("sess-1", "bc-4", &compassv1internal.SetIssueStateRequest{IssueId: "iss-x", State: compassv1.IssueState_ISSUE_STATE_DONE}))
	if err != nil {
		t.Fatalf("RelayBoardCall with a tool error returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band BoardCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found", toolErr.GetCode())
	}
	if toolErr.GetMessage() != "not_found: issue \"iss-x\" does not exist" {
		t.Fatalf("in-band error message = %q, want the caller's rendered error", toolErr.GetMessage())
	}
	// The call_id still round-trips on the error variant so the agent correlates
	// the failed call.
	if got := resp.GetResult().GetCallId(); got != "bc-4" {
		t.Fatalf("in-band error call_id = %q, want bc-4", got)
	}
}

// 5. On a successful write the minted call_id is echoed onto the result so the
// agent correlates its call, and the caller's response rides through on the
// set_issue_state variant.
func TestRelayBoardCallEchoesCallIDOnSuccess(t *testing.T) {
	hub, fake := newHubWithBoard()
	fake.resp = &compassv1internal.SetIssueStateResponse{Issue: &compassv1.Issue{Id: "iss-1", State: compassv1.IssueState_ISSUE_STATE_IN_PROGRESS}}
	bindLiveSession(hub)

	resp, err := hub.RelayBoardCall(context.Background(), relaySetIssueState("sess-1", "bc-5", &compassv1internal.SetIssueStateRequest{IssueId: "iss-1", State: compassv1.IssueState_ISSUE_STATE_IN_PROGRESS}))
	if err != nil {
		t.Fatalf("RelayBoardCall(set_issue_state) = %v, want success", err)
	}
	if got := resp.GetResult().GetCallId(); got != "bc-5" {
		t.Fatalf("response call_id = %q, want the request's bc-5", got)
	}
	if resp.GetResult().GetSetIssueState() != fake.resp {
		t.Fatal("response set_issue_state result is not the caller's response")
	}
}

// 6. A call whose set_issue_state oneof is unset is an invalid request. The
// dispatch's default arm returns a Connect CodeInvalidArgument error, which
// RelayBoardCall renders IN-BAND (a non-resolution error is always in-band) — so
// the Go error is nil, result.error.code == "invalid_argument", and the caller
// is NEVER invoked (a malformed call reaches no execution method).
//
// Mutation: returning a Connect error for the unset oneof (instead of in-band)
// reddens the err==nil assertion; invoking a caller method for it reddens the
// len==0 assertion.
func TestRelayBoardCallUnsetOneofIsInBandInvalidArgument(t *testing.T) {
	hub, fake := newHubWithBoard()
	bindLiveSession(hub)

	resp, err := hub.RelayBoardCall(context.Background(), &compassv1internal.RelayBoardCallRequest{
		SessionId: "sess-1",
		Call:      &compassv1internal.BoardCallRequest{CallId: "bc-6"}, // no set_issue_state variant set
	})
	if err != nil {
		t.Fatalf("RelayBoardCall with an unset oneof returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band BoardCallError, want the malformed call rendered in-band")
	}
	if toolErr.GetCode() != "invalid_argument" {
		t.Fatalf("in-band error code = %q, want invalid_argument", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "bc-6" {
		t.Fatalf("in-band error call_id = %q, want bc-6", got)
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("caller invoked %d times for a malformed call, want 0", len(calls))
	}
}
