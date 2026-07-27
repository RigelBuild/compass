package auth

// AdminGate + classifyProcedure + check contract tests (SEA-1195 T3a, the S3
// "admin-guard" gate item), transcribed from the authoritative Rust suite in
// crates/compass-daemon/src/auth.rs (#[cfg(test)] mod tests, the admin-gate
// section: admin_gate_rejects_a_non_admin_on_a_gated_rpc,
// admin_gate_allows_the_admin_on_a_gated_rpc,
// admin_gate_rejects_a_gated_rpc_with_no_identity,
// admin_gate_lets_a_non_admin_through_an_open_rpc,
// admin_gate_covers_every_frozen_session_rpc). The Rust gate is a tower Layer
// that inspects the HTTP method path and short-circuits with permission_denied;
// the Go gate is a connect Interceptor that reads Spec().Procedure and returns a
// connect.CodePermissionDenied error. The observable contract transcribed here
// is: a non-admin (or no) caller on an adminOnly procedure is denied and the
// wrapped handler never runs; the admin passes and the handler runs; every open
// procedure passes for any authenticated account.
//
// Default lane (no store): the gate reads a caller identity set on the context
// (withCaller with a store.AccountID literal) and a procedure path — neither
// needs the Postgres store, so these do not gate on pgtest.
//
// White-box (package auth) so the tests reach the unexported classifyProcedure
// (its (privilege, bool) partition) and check(ctx, procedure) directly, and the
// unexported withCaller to build a caller context. The per-procedure
// classification and denial logic is tested through check()/classifyProcedure —
// cleaner and exact, because connect.Request carries its Procedure in an
// unexported, unsettable field, so a hand-built unary request always presents an
// empty Procedure. That empty path is itself the fail-closed default
// (classifyProcedure("") == (adminOnly{}, false)), which the WrapUnary tests
// exercise through the public interceptor surface; the streaming tests drive real
// named procedures through a fake StreamingHandlerConn, whose interface is public.
//
// Proto-descriptor exhaustiveness (every generated procedure is classified,
// including the additions CreateChannel + UpdateChannelMembers and the dropped
// Share/UnshareAgentWorkspace) is covered separately in classify_exhaustive_test.go;
// this file spot-checks specific known and unknown procedure paths and never
// hardcodes the dropped procedures.

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	compassv1connect "github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// The two caller identities the gate tests compare against. Bare-string
// store.AccountID literals (the id space is a plain string now), distinct so the
// admin/non-admin partition is unambiguous.
const (
	adminID    = store.AccountID("admin-id")
	nonAdminID = store.AccountID("non-admin-id")
)

// wantPermissionDenied asserts the gate rejected with CodePermissionDenied. A nil
// error is CodeUnknown under connect.CodeOf, so this fails on a missing rejection
// too — the assertion cannot pass vacuously.
func wantPermissionDenied(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a permission-denied rejection, got nil error", what)
	}
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
		t.Fatalf("%s: expected CodePermissionDenied, got %v (err: %v)", what, code, err)
	}
}

// fakeStreamConn is a minimal connect.StreamingHandlerConn whose Spec carries a
// caller-supplied procedure path, so a streaming test drives the gate with a real
// named procedure (connect never exposes a way to set the procedure on a unary
// request in a unit test). Receive/Send are inert; the gate only reads Spec().
type fakeStreamConn struct {
	procedure string
	header    http.Header
}

func (c *fakeStreamConn) Spec() connect.Spec {
	return connect.Spec{Procedure: c.procedure, StreamType: connect.StreamTypeServer}
}
func (c *fakeStreamConn) Peer() connect.Peer { return connect.Peer{} }
func (c *fakeStreamConn) Receive(any) error  { return nil }
func (c *fakeStreamConn) RequestHeader() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}
func (c *fakeStreamConn) Send(any) error               { return nil }
func (c *fakeStreamConn) ResponseHeader() http.Header  { return make(http.Header) }
func (c *fakeStreamConn) ResponseTrailer() http.Header { return make(http.Header) }

// recordingStreamHandler is the streaming "next" handler: it records whether it
// ran and the caller it saw, so a denied stream (short-circuited before next) is
// proven by called staying false. Reuses spyResult from interceptor_test.go.
func recordingStreamHandler(rec *spyResult) connect.StreamingHandlerFunc {
	return func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		rec.called = true
		rec.caller, rec.hasCaller = CallerFrom(ctx)
		return nil
	}
}

// TestAdminGateGatesFrozenAdminOnlyRPCs is the frozen partition: exactly the five
// privileged CompassService RPCs (agent-session start/stop/reload/inspect + token
// issuance) are admin-gated. For each, the admin passes (nil) and a non-admin is
// denied — so a procedure silently dropping out of the gated set (non-admin
// wrongly admitted) or the gate over-reaching (admin wrongly denied) reddens the
// exact row. Mirrors admin_gate_covers_every_frozen_session_rpc +
// admin_gate_allows_the_admin_on_a_gated_rpc.
func TestAdminGateGatesFrozenAdminOnlyRPCs(t *testing.T) {
	gate := NewAdminGate(adminID)

	frozen := []struct {
		name      string
		procedure string
	}{
		{"StartAgentSession", compassv1connect.CompassServiceStartAgentSessionProcedure},
		{"StopAgentSession", compassv1connect.CompassServiceStopAgentSessionProcedure},
		{"ReloadAgentSession", compassv1connect.CompassServiceReloadAgentSessionProcedure},
		{"GetAgentStatus", compassv1connect.CompassServiceGetAgentStatusProcedure},
		{"IssueToken", compassv1connect.CompassServiceIssueTokenProcedure},
	}

	for _, tc := range frozen {
		t.Run(tc.name, func(t *testing.T) {
			if err := gate.check(withCaller(context.Background(), adminID), tc.procedure); err != nil {
				t.Fatalf("%s: the admin must pass the gate, got %v", tc.name, err)
			}
			err := gate.check(withCaller(context.Background(), nonAdminID), tc.procedure)
			wantPermissionDenied(t, err, tc.name+" for a non-admin")
		})
	}
}

// TestAdminGateDeniesMissingIdentityOnAdminOnlyRPC: BearerInterceptor injects the
// caller before the gate, but a gated RPC that somehow arrives with no caller in
// context must never be treated as the admin — it is denied. Mirrors
// admin_gate_rejects_a_gated_rpc_with_no_identity.
func TestAdminGateDeniesMissingIdentityOnAdminOnlyRPC(t *testing.T) {
	gate := NewAdminGate(adminID)

	err := gate.check(context.Background(), compassv1connect.CompassServiceStartAgentSessionProcedure)
	wantPermissionDenied(t, err, "an adminOnly RPC with no caller in context")
}

// TestAdminGateAllowsAnyAccountOnOpenRPCs: the connect-time probe, the event
// stream, and every CommsService method are open to any authenticated account —
// a non-admin caller passes the gate on each. Mirrors
// admin_gate_lets_a_non_admin_through_an_open_rpc, widened to the full open set,
// and includes the recut additions CreateChannel + UpdateChannelMembers to pin
// they classify open (not admin-gated).
func TestAdminGateAllowsAnyAccountOnOpenRPCs(t *testing.T) {
	gate := NewAdminGate(adminID)

	open := []struct {
		name      string
		procedure string
	}{
		{"GetServerInfo", compassv1connect.CompassServiceGetServerInfoProcedure},
		{"SubscribeEvents", compassv1connect.CompassServiceSubscribeEventsProcedure},
		{"SubscribeAgentSession", compassv1connect.CompassServiceSubscribeAgentSessionProcedure},
		{"CommsCreateUser", compassv1connect.CommsServiceCreateUserProcedure},
		{"CommsCreateChannel", compassv1connect.CommsServiceCreateChannelProcedure},
		{"CommsUpdateChannelMembers", compassv1connect.CommsServiceUpdateChannelMembersProcedure},
		{"CommsPostMessage", compassv1connect.CommsServicePostMessageProcedure},
		{"CommsSubscribeComms", compassv1connect.CommsServiceSubscribeCommsProcedure},
	}

	for _, tc := range open {
		t.Run(tc.name, func(t *testing.T) {
			if err := gate.check(withCaller(context.Background(), nonAdminID), tc.procedure); err != nil {
				t.Fatalf("%s: an open RPC must pass for a non-admin, got %v", tc.name, err)
			}
		})
	}
}

// TestClassifyProcedureClassifiesKnownAndUnknownProcedures pins the classification
// partition directly: the five privileged procedures classify adminOnly (ok=true),
// the probe/stream and CommsService procedures (including CreateChannel +
// UpdateChannelMembers) classify authenticatedOpen (ok=true), and any unrecognized
// path — a bogus method, a non-proto path, and the empty string a hand-built unary
// request presents — classifies adminOnly with ok=false (fail closed). ok=false
// only on the default arm is the contract: a flipped default (admitting an unknown
// path as open, or reporting it classified) reddens the corresponding row.
func TestClassifyProcedureClassifiesKnownAndUnknownProcedures(t *testing.T) {
	cases := []struct {
		name      string
		procedure string
		wantAdmin bool // true => adminOnly, false => authenticatedOpen
		wantOK    bool // true => a recognized generated procedure
	}{
		{"StartAgentSession", compassv1connect.CompassServiceStartAgentSessionProcedure, true, true},
		{"StopAgentSession", compassv1connect.CompassServiceStopAgentSessionProcedure, true, true},
		{"ReloadAgentSession", compassv1connect.CompassServiceReloadAgentSessionProcedure, true, true},
		{"GetAgentStatus", compassv1connect.CompassServiceGetAgentStatusProcedure, true, true},
		{"IssueToken", compassv1connect.CompassServiceIssueTokenProcedure, true, true},
		{"GetServerInfo", compassv1connect.CompassServiceGetServerInfoProcedure, false, true},
		{"SubscribeEvents", compassv1connect.CompassServiceSubscribeEventsProcedure, false, true},
		{"SubscribeAgentSession", compassv1connect.CompassServiceSubscribeAgentSessionProcedure, false, true},
		{"CommsCreateUser", compassv1connect.CommsServiceCreateUserProcedure, false, true},
		{"CommsCreateChannel", compassv1connect.CommsServiceCreateChannelProcedure, false, true},
		{"CommsUpdateChannelMembers", compassv1connect.CommsServiceUpdateChannelMembersProcedure, false, true},
		{"CommsPostMessage", compassv1connect.CommsServicePostMessageProcedure, false, true},
		{"CommsSubscribeComms", compassv1connect.CommsServiceSubscribeCommsProcedure, false, true},
		{"unknown compass method", "/compass.v1.CompassService/Bogus", true, false},
		{"non-proto path", "/not/a/procedure", true, false},
		{"empty procedure", "", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, ok := classifyProcedure(tc.procedure)
			if ok != tc.wantOK {
				t.Fatalf("classifyProcedure(%q) ok = %v, want %v", tc.procedure, ok, tc.wantOK)
			}
			_, isAdmin := class.(adminOnly)
			if isAdmin != tc.wantAdmin {
				_, isOpen := class.(authenticatedOpen)
				t.Fatalf("classifyProcedure(%q) class isAdmin=%v isOpen=%v, want adminOnly=%v",
					tc.procedure, isAdmin, isOpen, tc.wantAdmin)
			}
		})
	}
}

// TestAdminGateWrapUnaryAllowsAdmin: the unary interceptor invokes the wrapped
// handler when the gate passes. A hand-built connect request presents an empty
// procedure (connect exposes no setter), which classifies adminOnly via the
// fail-closed default — so the admin caller clears it and the handler runs.
func TestAdminGateWrapUnaryAllowsAdmin(t *testing.T) {
	gate := NewAdminGate(adminID)
	rec := &spyResult{}

	wrapped := gate.WrapUnary(recordingSpy(rec))
	_, err := wrapped(withCaller(context.Background(), adminID), bearerRequest(""))
	if err != nil {
		t.Fatalf("the admin must pass the unary gate, got %v", err)
	}
	if !rec.called {
		t.Fatal("the wrapped handler must run when the gate passes")
	}
	if !rec.hasCaller || rec.caller != adminID {
		t.Fatalf("the handler sees the admin caller: got %v hasCaller=%v, want %v", rec.caller, rec.hasCaller, adminID)
	}
}

// TestAdminGateWrapUnaryDeniesNonAdmin: the unary interceptor short-circuits with
// CodePermissionDenied and never calls the wrapped handler when the gate rejects.
// The empty-procedure request classifies adminOnly (fail-closed default), so a
// non-admin caller is denied — this also proves the default admits nothing as
// open through the public interceptor surface. Mirrors
// admin_gate_rejects_a_non_admin_on_a_gated_rpc (handler-never-runs clause).
func TestAdminGateWrapUnaryDeniesNonAdmin(t *testing.T) {
	gate := NewAdminGate(adminID)
	rec := &spyResult{}

	wrapped := gate.WrapUnary(recordingSpy(rec))
	_, err := wrapped(withCaller(context.Background(), nonAdminID), bearerRequest(""))
	wantPermissionDenied(t, err, "a non-admin on the unary gate")
	if rec.called {
		t.Fatal("the wrapped handler must not run when the gate denies")
	}
}

// TestAdminGateWrapStreamingHandlerRunsHandlerForAdmin: the streaming interceptor
// invokes the wrapped handler for the admin on an adminOnly procedure. The fake
// conn carries a real named procedure (StartAgentSession), which connect cannot
// supply on a unary request in a unit test.
func TestAdminGateWrapStreamingHandlerRunsHandlerForAdmin(t *testing.T) {
	gate := NewAdminGate(adminID)
	rec := &spyResult{}

	wrapped := gate.WrapStreamingHandler(recordingStreamHandler(rec))
	conn := &fakeStreamConn{procedure: compassv1connect.CompassServiceStartAgentSessionProcedure}
	err := wrapped(withCaller(context.Background(), adminID), conn)
	if err != nil {
		t.Fatalf("the admin must pass the streaming gate, got %v", err)
	}
	if !rec.called {
		t.Fatal("the streaming handler must run when the gate passes")
	}
	if !rec.hasCaller || rec.caller != adminID {
		t.Fatalf("the streaming handler sees the admin caller: got %v hasCaller=%v, want %v", rec.caller, rec.hasCaller, adminID)
	}
}

// TestAdminGateWrapStreamingHandlerDeniesNonAdmin: the streaming interceptor
// rejects a non-admin on an adminOnly procedure with CodePermissionDenied before
// the handler runs. Mirrors admin_gate_rejects_a_non_admin_on_a_gated_rpc for the
// streaming leg.
func TestAdminGateWrapStreamingHandlerDeniesNonAdmin(t *testing.T) {
	gate := NewAdminGate(adminID)
	rec := &spyResult{}

	wrapped := gate.WrapStreamingHandler(recordingStreamHandler(rec))
	conn := &fakeStreamConn{procedure: compassv1connect.CompassServiceStartAgentSessionProcedure}
	err := wrapped(withCaller(context.Background(), nonAdminID), conn)
	wantPermissionDenied(t, err, "a non-admin on the streaming gate")
	if rec.called {
		t.Fatal("the streaming handler must not run when the gate denies")
	}
}

// TestAdminGateWrapStreamingHandlerRunsHandlerForOpenRPC: an open streaming
// procedure (SubscribeEvents) passes the gate for a non-admin, and the handler
// runs. This is the only integration-shape test that a real open procedure clears
// the wrapper — a unary request cannot carry one (its empty procedure fail-closes).
func TestAdminGateWrapStreamingHandlerRunsHandlerForOpenRPC(t *testing.T) {
	gate := NewAdminGate(adminID)
	rec := &spyResult{}

	wrapped := gate.WrapStreamingHandler(recordingStreamHandler(rec))
	conn := &fakeStreamConn{procedure: compassv1connect.CompassServiceSubscribeEventsProcedure}
	err := wrapped(withCaller(context.Background(), nonAdminID), conn)
	if err != nil {
		t.Fatalf("an open streaming RPC must pass for a non-admin, got %v", err)
	}
	if !rec.called {
		t.Fatal("the streaming handler must run on an open RPC")
	}
}
