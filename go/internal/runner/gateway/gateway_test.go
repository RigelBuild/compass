//go:build unix

package gateway

// Hermetic suite for the Runner->Server forward handler Gateway.Comms (transport
// design T3, SEA-1351). White-box (package gateway) so it drives the handler and
// its two seams directly, sleep-free: the fakes record every fact synchronously,
// so every assertion reads a value the in-memory call already produced.
//
// The load-bearing case is the fail-closed guard: a call arriving before Start
// binds a session (the socket is live from Provision) must be REFUSED
// CodePermissionDenied and must NEVER forward with an empty session id. That
// case (1) is proven fail-first by the mutation recorded in the task report
// (flip `if !ok {` to `if false {` -> the impl forwards an empty session id ->
// the "relay never called" assertion goes RED).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// fakeSessions is a hand-written SessionForContainer. It records the container
// name it was asked to resolve (so a test can prove the handler resolves the
// name fixed at construction, never one read off the request) and returns a
// canned (sessionID, ok).
//
// The recorder is mutex-guarded because the Gateway's handlers run on the
// socket's http.Server goroutines and a test may drive several concurrently.
// The counter was unsynchronized until a concurrent durable-path test called it
// from 24 goroutines; every existing caller happened to be sequential, so the
// race was latent rather than absent.
type fakeSessions struct {
	sessionID string
	ok        bool

	mu      sync.Mutex
	gotName string
	calls   int
}

func (f *fakeSessions) Session(containerName string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotName = containerName
	return f.sessionID, f.ok
}

// snapshot returns the recorded (name, calls) under the lock, for a test
// asserting after its goroutines have joined.
func (f *fakeSessions) snapshot() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotName, f.calls
}

// recordedRelayCall is one observed forward: the ctx the handler propagated and
// the request payload it built.
type recordedRelayCall struct {
	ctx context.Context
	req *compassv1internal.RelayCommsCallRequest
}

// fakeRelay is a hand-written CommsRelay that records every RelayCommsCall it
// receives and returns a canned response or a canned transport error. A test
// asserts the forward through the recorded calls; NEVER forwarding is proven by
// an empty calls slice.
type fakeRelay struct {
	resp *compassv1internal.RelayCommsCallResponse
	err  error

	calls []recordedRelayCall
}

func (f *fakeRelay) RelayCommsCall(
	ctx context.Context, req *connect.Request[compassv1internal.RelayCommsCallRequest],
) (*connect.Response[compassv1internal.RelayCommsCallResponse], error) {
	f.calls = append(f.calls, recordedRelayCall{ctx: ctx, req: req.Msg})
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.resp), nil
}

// testCallID is the agent-minted correlation id these tests send; asserting it
// round-trips proves the Runner forwards the agent's call verbatim.
const testCallID = "tc-1"

// postCall builds a CommsCallRequest carrying a post variant under testCallID —
// the verbatim payload an in-container agent sends. The public-gen
// PostMessageRequest mirrors the idiom in internal/runnerhub/relay_comms_test.go.
func postCall() *compassv1internal.CommsCallRequest {
	return &compassv1internal.CommsCallRequest{
		CallId: testCallID,
		Call: &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{
			Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"},
			Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hi"}}},
		}},
	}
}

// Case 1. SECURITY CORE — no session bound to the container fails closed
// CodePermissionDenied and the relay is NEVER invoked. The socket is live from
// Provision, before Start binds the session; a call in that window must never
// forward with an empty session id nor attribute to any account. Mutation proof
// (task report): flip `if !ok {` to `if false {` -> the handler forwards an
// empty session id -> the len(relay.calls) != 0 / nil-error assertions go RED.
func TestCommsNoSessionFailsClosedPermissionDenied(t *testing.T) {
	sessions := &fakeSessions{ok: false}
	relay := &fakeRelay{}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	resp, err := g.Comms(context.Background(), connect.NewRequest(postCall()))
	if err == nil {
		t.Fatal("Comms with no session bound = nil error, want CodePermissionDenied (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("no-session error code = %v, want PermissionDenied", got)
	}
	if resp != nil {
		t.Fatalf("Comms returned a non-nil response alongside the fail-closed error")
	}
	if len(relay.calls) != 0 {
		t.Fatalf("relay invoked %d times for an unbound container, want 0 (never forward an empty session id)", len(relay.calls))
	}
}

// Case 2. Happy path — the handler resolves the bound session, forwards the
// agent's request VERBATIM under that session id, and returns the unwrapped
// result. Verbatim is pinned by pointer identity (the forwarded .Call is the same
// object the handler received); the returned result is the same object as
// resp.Msg.Result, and its call_id round-trips.
func TestCommsHappyPathForwardsUnderBoundSessionAndReturnsResult(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	wantResult := &compassv1internal.CommsCallResult{
		CallId: "tc-1",
		Result: &compassv1internal.CommsCallResult_Post{Post: &compassv1.PostMessageResponse{
			Message: &compassv1.Message{Id: "m-1"},
		}},
	}
	relay := &fakeRelay{resp: &compassv1internal.RelayCommsCallResponse{Result: wantResult}}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	call := postCall()
	resp, err := g.Comms(context.Background(), connect.NewRequest(call))
	if err != nil {
		t.Fatalf("Comms(happy) = %v, want success", err)
	}
	if len(relay.calls) != 1 {
		t.Fatalf("relay invoked %d times, want exactly 1", len(relay.calls))
	}
	if got := relay.calls[0].req.GetSessionId(); got != "sess-7" {
		t.Fatalf("forwarded session_id = %q, want the bound sess-7", got)
	}
	if relay.calls[0].req.GetCall() != call {
		t.Fatalf("forwarded Call is not the verbatim request the handler received (Runner is a pure forwarder)")
	}
	if resp.Msg != wantResult {
		t.Fatalf("returned result is not the unwrapped resp.Msg.Result")
	}
	if got := resp.Msg.GetCallId(); got != testCallID {
		t.Fatalf("returned result call_id = %q, want the request's %s", got, testCallID)
	}
}

// Case 3. The container the handler resolves is the one fixed at construction,
// never anything read off the request — the socket IS the container's identity.
// Two construction names, each asserted through the session resolver.
func TestCommsResolvesConstructionContainerName(t *testing.T) {
	for _, name := range []string{"cnt-A", "cnt-B"} {
		t.Run(name, func(t *testing.T) {
			sessions := &fakeSessions{sessionID: "sess-7", ok: true}
			relay := &fakeRelay{resp: &compassv1internal.RelayCommsCallResponse{
				Result: &compassv1internal.CommsCallResult{CallId: "tc-1"},
			}}
			g := NewGateway(context.Background(), name, sessions, relay, nil, nil, nil)

			if _, err := g.Comms(context.Background(), connect.NewRequest(postCall())); err != nil {
				t.Fatalf("Comms = %v, want success", err)
			}
			gotName, calls := sessions.snapshot()
			if calls != 1 {
				t.Fatalf("Session() called %d times, want exactly 1", calls)
			}
			if gotName != name {
				t.Fatalf("Session() resolved %q, want the construction name %q (identity is the socket's, never off the request)", gotName, name)
			}
		})
	}
}

// Case 4. An in-band tool failure (non-member channel, bad input) rides back
// INSIDE the result as the CommsCallResult_Error variant, passed through
// untouched — the Go error is nil, so a single failed call never tears the
// transport down.
func TestCommsInBandErrorRidesThroughAsResult(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	errResult := &compassv1internal.CommsCallResult{
		CallId: "tc-1",
		Result: &compassv1internal.CommsCallResult_Error{Error: &compassv1internal.CommsCallError{
			Code:    "not_found",
			Message: "no such channel",
		}},
	}
	relay := &fakeRelay{resp: &compassv1internal.RelayCommsCallResponse{Result: errResult}}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	resp, err := g.Comms(context.Background(), connect.NewRequest(postCall()))
	if err != nil {
		t.Fatalf("Comms with an in-band tool error = Go error %v, want nil (in-band failure is not a transport error)", err)
	}
	if resp.Msg.GetError() == nil {
		t.Fatal("returned result carries no Error variant, want the in-band CommsCallError passed through")
	}
	if got := resp.Msg.GetError().GetCode(); got != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found (passed through untouched)", got)
	}
}

// Case 5. A transport failure from the relay (Server unreachable) propagates as
// a Connect error with its code preserved — never swallowed into a silent
// nil-result success.
func TestCommsTransportFailurePropagatesConnectError(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeRelay{err: connect.NewError(connect.CodeUnavailable, errors.New("server unreachable"))}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	resp, err := g.Comms(context.Background(), connect.NewRequest(postCall()))
	if err == nil {
		t.Fatal("Comms with a relay transport failure = nil error, want the Connect error propagated")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("propagated error code = %v, want Unavailable (preserved, not swallowed)", got)
	}
	if resp != nil {
		t.Fatalf("Comms returned a non-nil response alongside the transport error")
	}
}

// Case 6. The inbound ctx (and its deadline) rides verbatim into the forward.
// A pure assertion: the fake records the ctx it received, and we compare a value
// set on the inbound ctx plus its deadline. No timing, no sleep.
func TestCommsPropagatesContextToRelay(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeRelay{resp: &compassv1internal.RelayCommsCallResponse{
		Result: &compassv1internal.CommsCallResult{CallId: "tc-1"},
	}}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	type ctxKey struct{}
	const want = "inbound-marker"
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), ctxKey{}, want), deadline)
	defer cancel()

	if _, err := g.Comms(ctx, connect.NewRequest(postCall())); err != nil {
		t.Fatalf("Comms = %v, want success", err)
	}
	if len(relay.calls) != 1 {
		t.Fatalf("relay invoked %d times, want exactly 1", len(relay.calls))
	}
	got := relay.calls[0].ctx
	if v, _ := got.Value(ctxKey{}).(string); v != want {
		t.Fatalf("relay ctx value = %q, want the inbound %q (ctx propagated verbatim)", v, want)
	}
	if d, ok := got.Deadline(); !ok || !d.Equal(deadline) {
		t.Fatalf("relay ctx deadline = %v (ok=%v), want the inbound %v (deadline rides ctx into the forward)", d, ok, deadline)
	}
}

// Case 7. SECURITY GUARD — a resolver that hands back an EMPTY session id with
// ok=true is treated as unbound, exactly like the !ok case: the call fails
// closed CodePermissionDenied and the relay is NEVER invoked. The handler
// promises never to relay an empty session id (before this guard, ("", true)
// forwarded an empty session id). Mutation proof: revert the guard to
// `if !ok {` -> ("", true) forwards -> the len(relay.calls) != 0 / nil-error
// assertions go RED.
func TestCommsEmptySessionIDFailsClosedPermissionDenied(t *testing.T) {
	sessions := &fakeSessions{sessionID: "", ok: true}
	relay := &fakeRelay{}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	resp, err := g.Comms(context.Background(), connect.NewRequest(postCall()))
	if err == nil {
		t.Fatal("Comms with an empty session id = nil error, want CodePermissionDenied (empty session is unbound)")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("empty-session error code = %v, want PermissionDenied", got)
	}
	if resp != nil {
		t.Fatalf("Comms returned a non-nil response alongside the fail-closed error")
	}
	if len(relay.calls) != 0 {
		t.Fatalf("relay invoked %d times for an empty session id, want 0 (never forward an empty session id)", len(relay.calls))
	}
}

// Case 8. GUARD — a successful RelayCommsCall whose response carries a nil
// Result (a malformed Server reply with no result message) is surfaced as
// CodeInternal, never a success wrapping a nil result the agent would deref.
// Mutation proof: drop the nil-check (return connect.NewResponse(resp.Msg.GetResult()), nil)
// -> a nil-result response returns success -> the err/code/nil-response
// assertions go RED.
func TestCommsNilRelayResultFailsInternal(t *testing.T) {
	sessions := &fakeSessions{sessionID: "sess-7", ok: true}
	relay := &fakeRelay{resp: &compassv1internal.RelayCommsCallResponse{}}
	g := NewGateway(context.Background(), "cnt-A", sessions, relay, nil, nil, nil)

	resp, err := g.Comms(context.Background(), connect.NewRequest(postCall()))
	if err == nil {
		t.Fatal("Comms with a nil relay result = nil error, want CodeInternal (malformed Server reply)")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("nil-result error code = %v, want Internal", got)
	}
	if resp != nil {
		t.Fatalf("Comms returned a non-nil response alongside the nil-result error")
	}
}
