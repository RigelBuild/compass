//go:build unix

package gateway

// Hermetic suite for the Runner->Server forward handler Gateway.Forge (create/
// comment/get/list an issue or PR, submit a review — Compass forge write path
// T5). The forge twin of the Lifecycle suite: white-box (package gateway),
// sleep-free — the fakes record every fact synchronously, so every assertion
// reads a value the in-memory call already produced.
//
// The load-bearing case is the fail-closed guard: a forge call arriving before
// Start binds a session (the socket is live from Provision) MUST be refused
// CodePermissionDenied and MUST NEVER forward with an empty session id — a forge
// write under an unbound session would be attributed by the Server to no
// account, the exact security hole the seam exists to close.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// recordedForgeCall is one observed forward: the ctx the handler propagated and
// the request payload it built.
type recordedForgeCall struct {
	ctx context.Context
	req *compassv1internal.RelayForgeCallRequest
}

// fakeForgeRelay is a hand-written ForgeRelay that records every RelayForgeCall
// it receives and returns a canned response or a canned transport error. A test
// asserts the forward through the recorded calls; NEVER forwarding is proven by
// an empty calls slice. The forge twin of fakeLifecycleRelay.
type fakeForgeRelay struct {
	resp *compassv1internal.RelayForgeCallResponse
	err  error

	calls []recordedForgeCall
}

func (f *fakeForgeRelay) RelayForgeCall(
	ctx context.Context, req *connect.Request[compassv1internal.RelayForgeCallRequest],
) (*connect.Response[compassv1internal.RelayForgeCallResponse], error) {
	f.calls = append(f.calls, recordedForgeCall{ctx: ctx, req: req.Msg})
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.resp), nil
}

// forgeCreateIssueCall builds a ForgeCallRequest carrying a create_issue variant
// under testCallID — the verbatim payload an in-container agent sends.
func forgeCreateIssueCall() *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		CallId: testCallID,
		Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
			Repo:  "o/r",
			Title: "t",
		}},
	}
}

// Case 1. SECURITY CORE — no session bound to the container fails closed
// CodePermissionDenied and the relay is NEVER invoked. The socket is live from
// Provision, before Start binds the session; a forge call in that window must
// never forward with an empty session id nor attribute to any account. Mutation
// proof: flip `if !ok || sessionID == "" {` to `if false {` -> the handler
// forwards an empty session id -> the len(relay.calls) != 0 / nil-error
// assertions go RED.
func TestForgeNoSessionFailsClosedPermissionDenied(t *testing.T) {
	sessions := &fakeSessions{ok: false}
	relay := &fakeForgeRelay{}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Forge: relay})

	resp, err := g.Forge(context.Background(), connect.NewRequest(forgeCreateIssueCall()))
	if err == nil {
		t.Fatal("Forge with no session bound = nil error, want CodePermissionDenied (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("no-session error code = %v, want PermissionDenied", got)
	}
	if resp != nil {
		t.Fatalf("no-session response = %+v, want nil", resp)
	}
	if len(relay.calls) != 0 {
		t.Fatalf("relay forwarded %d calls with no session bound, want 0 (never forward an empty session id)", len(relay.calls))
	}
}

// Case 1b. SECURITY CORE — a bound session whose id is empty (ok:true,
// sessionID:"") is still unbound for attribution: it fails closed
// CodePermissionDenied and the relay is NEVER invoked. This pins the SECOND
// clause of the `!ok || sessionID == ""` guard, which Case 1 (ok:false) leaves
// unexercised. Mutation proof: drop `|| sessionID == ""` -> an empty session id
// forwards -> the len(relay.calls) != 0 / nil-error assertions go RED.
func TestForgeEmptySessionIDFailsClosedPermissionDenied(t *testing.T) {
	sessions := &fakeSessions{sessionID: "", ok: true}
	relay := &fakeForgeRelay{}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Forge: relay})

	resp, err := g.Forge(context.Background(), connect.NewRequest(forgeCreateIssueCall()))
	if err == nil {
		t.Fatal("Forge with an empty session id = nil error, want CodePermissionDenied (empty session is unbound)")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("empty-session error code = %v, want PermissionDenied", got)
	}
	if resp != nil {
		t.Fatalf("Forge returned a non-nil response alongside the fail-closed error")
	}
	if len(relay.calls) != 0 {
		t.Fatalf("relay forwarded %d calls for an empty session id, want 0 (never forward an empty session id)", len(relay.calls))
	}
}

// Case 2. HAPPY PATH — a forge call arriving while a session is bound forwards to
// the Server carrying the EXACT session id the Runner structurally owns (never
// one read off the request) and the agent's call verbatim, and the Server's
// result flows back. Forwarding the exact bound session id IS the attribution
// proof at the Runner seam: the Runner asserts no account.
func TestForgeHappyPathForwardsUnderBoundSessionAndReturnsResult(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	wantResult := &compassv1internal.ForgeCallResult{
		CallId: testCallID,
		Result: &compassv1internal.ForgeCallResult_IssueComment{IssueComment: &compassv1internal.CommentRef{Url: "https://forge/1"}},
	}
	relay := &fakeForgeRelay{resp: &compassv1internal.RelayForgeCallResponse{Result: wantResult}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Forge: relay})

	resp, err := g.Forge(context.Background(), connect.NewRequest(forgeCreateIssueCall()))
	if err != nil {
		t.Fatalf("Forge = %v, want success", err)
	}
	if len(relay.calls) != 1 {
		t.Fatalf("relay forwarded %d calls, want exactly 1", len(relay.calls))
	}
	got := relay.calls[0].req
	if got.GetSessionId() != "sess-7" {
		t.Fatalf("forwarded session id = %q, want sess-7 (the bound session the Runner owns)", got.GetSessionId())
	}
	if got.GetCall().GetCallId() != testCallID {
		t.Fatalf("forwarded call id = %q, want %q (the agent's call, verbatim)", got.GetCall().GetCallId(), testCallID)
	}
	if got.GetCall().GetCreateIssue().GetRepo() != "o/r" {
		t.Fatalf("forwarded repo = %q, want o/r (verbatim payload)", got.GetCall().GetCreateIssue().GetRepo())
	}
	if resp.Msg.GetIssueComment().GetUrl() != "https://forge/1" {
		t.Fatalf("returned result = %+v, want the Server's forge result", resp.Msg)
	}
}

// A transport failure on the Runner->Server leg surfaces as a Connect error (the
// agent renders it in-band), never a success wrapping a nil result.
func TestForgeTransportFailurePropagatesConnectError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeForgeRelay{err: connect.NewError(connect.CodeUnavailable, errors.New("server unreachable"))}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Forge: relay})

	resp, err := g.Forge(context.Background(), connect.NewRequest(forgeCreateIssueCall()))
	if err == nil {
		t.Fatal("Forge with a failing relay = nil error, want the transport error propagated")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("propagated error code = %v, want Unavailable", got)
	}
	if resp != nil {
		t.Fatalf("transport-failure response = %+v, want nil", resp)
	}
}

// A well-formed RelayForgeCallResponse always carries a result; a nil result is
// a malformed Server reply surfaced as CodeInternal, never a success wrapping a
// nil Msg the agent would deref.
func TestForgeNilResultIsInternalError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeForgeRelay{resp: &compassv1internal.RelayForgeCallResponse{}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Forge: relay})

	resp, err := g.Forge(context.Background(), connect.NewRequest(forgeCreateIssueCall()))
	if err == nil {
		t.Fatal("Forge with a nil relay result = nil error, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("nil-result error code = %v, want Internal", got)
	}
	if resp != nil {
		t.Fatalf("nil-result response = %+v, want nil", resp)
	}
}
