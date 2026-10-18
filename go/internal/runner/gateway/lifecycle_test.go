//go:build unix

package gateway

// Hermetic Gateway.Lifecycle tests cover forwarding and reject calls before Start
// binds a session.

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// recordedLifecycleCall is one observed forward: the ctx the handler propagated
// and the request payload it built.
type recordedLifecycleCall struct {
	ctx context.Context
	req *compassv1internal.RelayLifecycleCallRequest
}

// fakeLifecycleRelay is a hand-written LifecycleRelay that records every
// RelayLifecycleCall it receives and returns a canned response or a canned
// transport error. A test asserts the forward through the recorded calls; NEVER
// forwarding is proven by an empty calls slice. The lifecycle twin of fakeRelay.
type fakeLifecycleRelay struct {
	resp *compassv1internal.RelayLifecycleCallResponse
	err  error

	calls []recordedLifecycleCall
}

func (f *fakeLifecycleRelay) RelayLifecycleCall(
	ctx context.Context, req *compassv1internal.RelayLifecycleCallRequest,
) (*compassv1internal.RelayLifecycleCallResponse, error) {
	f.calls = append(f.calls, recordedLifecycleCall{ctx: ctx, req: req})
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// spawnCall builds a LifecycleCallRequest carrying a spawn variant under
// testCallID — the verbatim payload an in-container agent sends.
func spawnCall() *compassv1internal.LifecycleCallRequest {
	return &compassv1internal.LifecycleCallRequest{
		CallId: testCallID,
		Call: &compassv1internal.LifecycleCallRequest_Spawn{Spawn: &compassv1internal.SpawnPeerRequest{
			Handle: "peer-1",
		}},
	}
}

// No session bound to the container fails closed with FailedPrecondition and
// never invokes the relay.
func TestLifecycleNoSessionFailsClosedFailedPrecondition(t *testing.T) {
	sessions := &fakeSessions{ok: false}
	relay := &fakeLifecycleRelay{}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Lifecycle: relay})

	resp, err := g.Lifecycle(context.Background(), spawnCall())
	if err == nil {
		t.Fatal("Lifecycle with no session bound = nil error, want CodeFailedPrecondition")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("no-session error code = %v, want FailedPrecondition", got)
	}
	if resp != nil {
		t.Fatalf("no-session response = %+v, want nil", resp)
	}
	if len(relay.calls) != 0 {
		t.Fatalf("relay forwarded %d calls with no session bound, want 0 (never forward an empty session id)", len(relay.calls))
	}
}

// An empty session id is also unbound and must not reach the relay.
func TestLifecycleEmptySessionIDFailsClosedFailedPrecondition(t *testing.T) {
	sessions := &fakeSessions{sessionID: "", ok: true}
	relay := &fakeLifecycleRelay{}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Lifecycle: relay})

	resp, err := g.Lifecycle(context.Background(), spawnCall())
	if err == nil {
		t.Fatal("Lifecycle with an empty session id = nil error, want CodeFailedPrecondition")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("empty-session error code = %v, want FailedPrecondition", got)
	}
	if resp != nil {
		t.Fatalf("Lifecycle returned a non-nil response alongside the fail-closed error")
	}
	if len(relay.calls) != 0 {
		t.Fatalf("relay forwarded %d calls for an empty session id, want 0 (never forward an empty session id)", len(relay.calls))
	}
}

// Case 2. HAPPY PATH — a lifecycle call arriving while a session is bound
// forwards to the Server carrying the EXACT session id the Runner structurally
// owns (never one read off the request) and the agent's call verbatim, and the
// Server's result flows back. Forwarding the exact bound session id IS the
// attribution proof at the Runner seam (OQ-2): the Runner asserts no account.
func TestLifecycleHappyPathForwardsUnderBoundSessionAndReturnsResult(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	wantResult := &compassv1internal.LifecycleCallResult{
		CallId: testCallID,
		Result: &compassv1internal.LifecycleCallResult_Spawn{Spawn: &compassv1internal.SpawnPeerResponse{
			AgentAccountId: "acct-9",
			ContainerName:  "peer-1-cnt",
			SessionId:      "peer-sess",
		}},
	}
	relay := &fakeLifecycleRelay{resp: &compassv1internal.RelayLifecycleCallResponse{Result: wantResult}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Lifecycle: relay})

	resp, err := g.Lifecycle(context.Background(), spawnCall())
	if err != nil {
		t.Fatalf("Lifecycle = %v, want success", err)
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
	if got.GetCall().GetSpawn().GetHandle() != "peer-1" {
		t.Fatalf("forwarded spawn handle = %q, want peer-1 (verbatim payload)", got.GetCall().GetSpawn().GetHandle())
	}
	if resp.GetSpawn().GetAgentAccountId() != "acct-9" {
		t.Fatalf("returned result = %+v, want the Server's spawn result", resp)
	}
}

// The handler resolves the container name fixed at CONSTRUCTION, never one read
// off the request — two construction names, each asserted through the session
// resolver. A bug reading the name from elsewhere would resolve the wrong
// container's session.
func TestLifecycleResolvesConstructionContainerName(t *testing.T) {
	for _, name := range []string{"cnt-A", "cnt-B"} {
		t.Run(name, func(t *testing.T) {
			sessions := &fakeSessions{sessionID: "sess-7", ok: true}
			relay := &fakeLifecycleRelay{resp: &compassv1internal.RelayLifecycleCallResponse{
				Result: &compassv1internal.LifecycleCallResult{CallId: testCallID},
			}}
			g := NewGateway(context.Background(), name, Deps{Sessions: sessions, Lifecycle: relay})

			if _, err := g.Lifecycle(context.Background(), spawnCall()); err != nil {
				t.Fatalf("Lifecycle = %v, want success", err)
			}
			if gotName, _ := sessions.snapshot(); gotName != name {
				t.Fatalf("resolved container %q, want the construction name %q", gotName, name)
			}
		})
	}
}

// A transport failure on the Runner->Server leg surfaces as a Connect error
// (the agent renders it in-band), never a success wrapping a nil result.
func TestLifecycleTransportFailurePropagatesConnectError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeLifecycleRelay{err: connect.NewError(connect.CodeUnavailable, "server unreachable")}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Lifecycle: relay})

	resp, err := g.Lifecycle(context.Background(), spawnCall())
	if err == nil {
		t.Fatal("Lifecycle with a failing relay = nil error, want the transport error propagated")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("propagated error code = %v, want Unavailable", got)
	}
	if resp != nil {
		t.Fatalf("transport-failure response = %+v, want nil", resp)
	}
}

// A well-formed RelayLifecycleCallResponse always carries a result; a nil result
// is a malformed Server reply surfaced as CodeInternal, never a success wrapping
// a nil Msg the agent would deref.
func TestLifecycleNilResultIsInternalError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeLifecycleRelay{resp: &compassv1internal.RelayLifecycleCallResponse{}}
	g := NewGateway(context.Background(), "cnt-A", Deps{Sessions: sessions, Lifecycle: relay})

	resp, err := g.Lifecycle(context.Background(), spawnCall())
	if err == nil {
		t.Fatal("Lifecycle with a nil relay result = nil error, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("nil-result error code = %v, want Internal", got)
	}
	if resp != nil {
		t.Fatalf("nil-result response = %+v, want nil", resp)
	}
}
