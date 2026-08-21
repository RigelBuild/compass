//go:build unix

package runnerhub

// The agent-forge Server leg (Compass forge write path T5, fail-closed authz —
// the load-bearing security leg, exactly as RelayBoardCall / RelayLifecycleCall /
// RelayCommsCall). Every test here defends one invariant of the
// session->account resolution + RelayForgeCall handler: the Runner asserts no
// account, so the SERVER's binding is the sole authority for whose account a
// relayed forge call runs under. A regression that let an unbound, stopped, or
// reconnect-dropped session resolve to ANY account — or delegated under the
// wrong account, or turned a tool failure into a transport teardown — must
// redden a test below.
//
// White-box (package runnerhub) so the tests drive the unexported binding
// lifecycle and the resolution edge directly, asserting the account attribution
// through the fake ForgeCaller. Sleep-free: the hub calls the caller inline, so
// every assertion reads a synchronously-recorded fact.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// forgeCall records one ForgeCaller invocation: the account the hub attributed
// it to, the session id it passed through, and the request forwarded. A test
// asserts the hub delegated under the RESOLVED caller account (never the
// Runner's, never admin) and threaded the session id.
type forgeCall struct {
	account   store.AccountID
	sessionID string
	call      *compassv1internal.ForgeCallRequest
}

// fakeForgeCaller is a hand-written ForgeCaller mirroring fakeBoardCaller: it
// records every call (account + session id + request) so a test asserts the hub
// attributed to the bound account and forwarded the exact request, and returns a
// configurable canned result or error so a test drives both the success and the
// in-band tool-error path without a real service. Concurrency-safe for parity
// with the real caller, though the hub calls it inline.
type fakeForgeCaller struct {
	mu    sync.Mutex
	calls []forgeCall

	result *compassv1internal.ForgeCallResult
	err    error
}

func (f *fakeForgeCaller) ExecuteForgeCallAsAccount(_ context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest) (*compassv1internal.ForgeCallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, forgeCall{account: caller, sessionID: sessionID, call: call})
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeForgeCaller) snapshot() []forgeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgeCall(nil), f.calls...)
}

// newHubWithForge builds a hub whose ForgeCaller is the returned fake (wired
// post-construction via SetForgeCaller, the real wiring path), so a
// RelayForgeCall test drives the resolve->attribute->delegate path and asserts
// on the caller account the fake was called with. Like newHubOnly otherwise.
func newHubWithForge() (*Hub, *fakeForgeCaller) {
	fake := &fakeForgeCaller{}
	hub := newHubOnly()
	hub.SetForgeCaller(fake)
	return hub, fake
}

// relayCreateIssue builds a RelayForgeCallRequest carrying a create_issue
// variant under callID.
func relayCreateIssue(sessionID, callID string, req *compassv1internal.CreateIssueRequest) *compassv1internal.RelayForgeCallRequest {
	return &compassv1internal.RelayForgeCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.ForgeCallRequest{
			CallId: callID,
			Call:   &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: req},
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
func TestRelayForgeCallUnboundSessionFailsClosedNotFound(t *testing.T) {
	hub, fake := newHubWithForge()

	_, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("never-bound", "fc-1", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
	if err == nil {
		t.Fatal("RelayForgeCall for an unbound session = nil error, want CodeNotFound (fail closed)")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unbound-session error code = %v, want NotFound", got)
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("caller was invoked %d times for an unbound session, want 0 (no delegation attempt)", len(calls))
	}
}

// 2. A hub with no ForgeCaller wired fails RelayForgeCall closed with
// CodeUnavailable — the forge write leg is not mounted, never a silent success.
// This is checked BEFORE session resolution, so even a bound session gets
// Unavailable on a caller-less hub.
//
// Mutation: reordering the checks so resolution runs first would change the code
// to NotFound for an unbound+nil case — the second sub-assertion (unbound session
// + nil caller still Unavailable) reddens that reorder.
func TestRelayForgeCallNilCallerIsUnavailableBeforeResolution(t *testing.T) {
	t.Run("bound session still Unavailable", func(t *testing.T) {
		hub := newHubOnly()  // no ForgeCaller wired
		bindLiveSession(hub) // a live binding exists, proving the nil guard precedes resolution

		_, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("sess-1", "fc-2", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
		if err == nil {
			t.Fatal("RelayForgeCall on a caller-less hub = nil error, want CodeUnavailable")
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("nil-caller (bound session) error code = %v, want Unavailable", got)
		}
	})
	t.Run("unbound session still Unavailable (nil-check precedes resolution)", func(t *testing.T) {
		hub := newHubOnly() // no ForgeCaller wired, no binding

		_, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("never-bound", "fc-2b", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
		if err == nil {
			t.Fatal("RelayForgeCall on a caller-less hub (unbound) = nil error, want CodeUnavailable")
		}
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("nil-caller (unbound session) error code = %v, want Unavailable, not NotFound (proves nil-check precedes resolution)", got)
		}
	})
}

// 3. THE core authz test: the RESOLVED caller account (the hub's own binding)
// reaches the caller — never a request field, never a literal admin id — AND the
// session id is threaded through so the chokepoint can stamp the owner header. A
// call for the bound session_id delegates under acct-agent, the account the hub
// bound, with the bound session id.
//
// Mutation: passing a request field or a literal admin id instead of the
// resolved account reddens the account assertion; dropping the session-id
// passthrough reddens the sessionID assertion.
func TestRelayForgeCallDelegatesUnderResolvedCallerAccount(t *testing.T) {
	hub, fake := newHubWithForge()
	fake.result = &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_IssueComment{IssueComment: &compassv1internal.CommentRef{}},
	}
	bindLiveSession(hub) // sess-1 -> acct-agent

	_, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("sess-1", "fc-3", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
	if err != nil {
		t.Fatalf("RelayForgeCall(create_issue) = %v, want success", err)
	}
	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("caller invoked %d times, want exactly 1", len(calls))
	}
	if calls[0].account != "acct-agent" {
		t.Fatalf("caller attributed to %q, want the bound caller account acct-agent (never request-asserted, never admin)", calls[0].account)
	}
	if calls[0].sessionID != "sess-1" {
		t.Fatalf("caller received session id %q, want the bound sess-1 (threaded for the owner-header stamp)", calls[0].sessionID)
	}
	if calls[0].call.GetCreateIssue().GetRepo() != "o/r" {
		t.Fatalf("caller received repo %q, want the request's o/r", calls[0].call.GetCreateIssue().GetRepo())
	}
}

// 4. A caller (tool-level) error surfaces IN-BAND in a SUCCESSFUL (nil-err)
// response as the ForgeCallError variant — the agent renders it and the
// transport survives. Only a resolution miss / no-caller is a Connect error.
//
// Mutation: returning the caller error as a Connect error (instead of in-band)
// reddens the err==nil assertion.
func TestRelayForgeCallToolErrorIsInBandNotStreamError(t *testing.T) {
	hub, fake := newHubWithForge()
	fake.err = connect.NewError(connect.CodeNotFound, errors.New("repo \"o/x\" does not exist"))
	bindLiveSession(hub)

	resp, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("sess-1", "fc-4", &compassv1internal.CreateIssueRequest{Repo: "o/x", Title: "t"}))
	if err != nil {
		t.Fatalf("RelayForgeCall with a tool error returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band ForgeCallError, want the tool failure rendered in-band")
	}
	if toolErr.GetCode() != "not_found" {
		t.Fatalf("in-band error code = %q, want not_found", toolErr.GetCode())
	}
	if toolErr.GetMessage() != "not_found: repo \"o/x\" does not exist" {
		t.Fatalf("in-band error message = %q, want the caller's rendered error", toolErr.GetMessage())
	}
	// The call_id still round-trips on the error variant so the agent correlates
	// the failed call.
	if got := resp.GetResult().GetCallId(); got != "fc-4" {
		t.Fatalf("in-band error call_id = %q, want fc-4", got)
	}
}

// 5. On a successful call the minted call_id is echoed onto the result so the
// agent correlates its call, and the caller's result rides through.
func TestRelayForgeCallEchoesCallIDOnSuccess(t *testing.T) {
	hub, fake := newHubWithForge()
	fake.result = &compassv1internal.ForgeCallResult{
		Result: &compassv1internal.ForgeCallResult_IssueComment{IssueComment: &compassv1internal.CommentRef{}},
	}
	bindLiveSession(hub)

	resp, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("sess-1", "fc-5", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
	if err != nil {
		t.Fatalf("RelayForgeCall(create_issue) = %v, want success", err)
	}
	if got := resp.GetResult().GetCallId(); got != "fc-5" {
		t.Fatalf("response call_id = %q, want the request's fc-5", got)
	}
	if resp.GetResult() != fake.result {
		t.Fatal("response result is not the caller's result")
	}
}

// 6. A ForgeCaller that returns (nil, nil) — a nil result on the nil-error arm —
// must NOT nil-deref on the resolution edge: the malformed reply is surfaced
// in-band as CodeInternal with the call_id echoed, not a panic. The sibling
// legs get this immunity for free from their internal executor; the forge leg
// calls the external ForgeCaller directly, so the guard is explicit.
func TestRelayForgeCallNilResultIsInternalErrorInBand(t *testing.T) {
	hub, fake := newHubWithForge()
	fake.result = nil // (nil result, nil error) — a malformed caller reply
	fake.err = nil
	bindLiveSession(hub)

	resp, err := hub.RelayForgeCall(context.Background(), relayCreateIssue("sess-1", "fc-6", &compassv1internal.CreateIssueRequest{Repo: "o/r", Title: "t"}))
	if err != nil {
		t.Fatalf("RelayForgeCall with a nil-result caller returned a Go error %v, want nil (in-band render)", err)
	}
	toolErr := resp.GetResult().GetError()
	if toolErr == nil {
		t.Fatal("response has no in-band ForgeCallError, want the malformed nil result rendered in-band")
	}
	if toolErr.GetCode() != "internal" {
		t.Fatalf("in-band error code = %q, want internal", toolErr.GetCode())
	}
	if got := resp.GetResult().GetCallId(); got != "fc-6" {
		t.Fatalf("in-band error call_id = %q, want fc-6", got)
	}
}
