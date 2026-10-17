package auth

// AdminGate contract tests (RIG-1195 T3a), transcribed from the Rust suite in
// crates/compass-daemon/src/auth.rs: a non-admin or identity-less caller on an
// adminOnly procedure is denied and the handler never runs; the admin passes.

// White-box (package auth) to reach unexported classifyProcedure and check.
// Descriptor exhaustiveness lives in classify_exhaustive_test.go; this file
// spot-checks the interceptor and its procedure classifications.

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1connect "github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/store"
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

func gateServerCall(interceptor connect.ServerInterceptor, ctx context.Context, procedure string, streamType connect.StreamType, next connect.ServerFunc) error {
	return runServerCall(interceptor, ctx, connect.Spec{Procedure: procedure, StreamType: streamType}, &connect.CallInfo{}, next)
}

// TestAdminGateGatesFrozenAdminOnlyRPCs is the gated partition: exactly the five
// privileged CompassService RPCs (agent-session start/stop/reload/inspect + token
// issuance) are admin-gated. For each, the admin passes (nil) and a non-admin is
// denied — so a procedure silently dropping out of the gated set (non-admin
// wrongly admitted) or the gate over-reaching (admin wrongly denied) reddens the
// exact row. Mirrors admin_gate_covers_every_frozen_session_rpc +
// admin_gate_allows_the_admin_on_a_gated_rpc.
func TestAdminGateGatesFrozenAdminOnlyRPCs(t *testing.T) {
	interceptor := NewAdminGate(adminID)

	frozen := []struct {
		name      string
		procedure string
	}{
		{"StartAgentSession", compassv1connect.CompassServiceStartAgentSessionProcedure},
		{"StopAgentSession", compassv1connect.CompassServiceStopAgentSessionProcedure},
		{"ReloadAgentSession", compassv1connect.CompassServiceReloadAgentSessionProcedure},
		{"GetAgentStatus", compassv1connect.CompassServiceGetAgentStatusProcedure},
		{"IssueToken", compassv1connect.CompassServiceIssueTokenProcedure},
		{"RenameAccount", compassv1connect.CompassServiceRenameAccountProcedure},
		{"RevokeToken", compassv1connect.CompassServiceRevokeTokenProcedure},
		{"RemoveAgentWorkspace", compassv1connect.CompassServiceRemoveAgentWorkspaceProcedure},
	}

	for _, tc := range frozen {
		t.Run(tc.name, func(t *testing.T) {
			err := gateServerCall(interceptor, withCaller(context.Background(), adminID), tc.procedure, connect.StreamTypeUnary, recordingSpy(&spyResult{}))
			if err != nil {
				t.Fatalf("%s: the admin must pass the gate, got %v", tc.name, err)
			}
			err = gateServerCall(interceptor, withCaller(context.Background(), nonAdminID), tc.procedure, connect.StreamTypeUnary, recordingSpy(&spyResult{}))
			wantPermissionDenied(t, err, tc.name+" for a non-admin")
		})
	}
}

// TestAdminGateDeniesMissingIdentityOnAdminOnlyRPC verifies a missing caller is
// denied by the server interceptor.
func TestAdminGateDeniesMissingIdentityOnAdminOnlyRPC(t *testing.T) {
	err := gateServerCall(NewAdminGate(adminID), context.Background(), compassv1connect.CompassServiceStartAgentSessionProcedure, connect.StreamTypeUnary, recordingSpy(&spyResult{}))
	wantPermissionDenied(t, err, "an adminOnly RPC with no caller in context")
}

// TestAdminGateAllowsAnyAccountOnOpenRPCs: the connect-time probe, the event
// stream, and every CommsService method are open to any authenticated account —
// a non-admin caller passes the gate on each. Mirrors
// admin_gate_lets_a_non_admin_through_an_open_rpc, widened to the full open set,
// and includes the recut additions CreateChannel + UpdateChannelMembers to pin
// they classify open (not admin-gated).
func TestAdminGateAllowsAnyAccountOnOpenRPCs(t *testing.T) {
	interceptor := NewAdminGate(adminID)
	open := []struct {
		name      string
		procedure string
	}{
		{"GetServerInfo", compassv1connect.CompassServiceGetServerInfoProcedure},
		{"GetRefLinkRules", compassv1connect.CompassServiceGetRefLinkRulesProcedure},
		{"SubscribeEvents", compassv1connect.CompassServiceSubscribeEventsProcedure},
		{"ListBoardIssues", compassv1connect.CompassServiceListBoardIssuesProcedure},
		{"SkipBatchWindow", compassv1connect.CompassServiceSkipBatchWindowProcedure},
		{"CommsCreateUser", compassv1connect.CommsServiceCreateUserProcedure},
		{"CommsCreateChannel", compassv1connect.CommsServiceCreateChannelProcedure},
		{"CommsUpdateChannelMembers", compassv1connect.CommsServiceUpdateChannelMembersProcedure},
		{"CommsReparentAgent", compassv1connect.CommsServiceReparentAgentProcedure},
		{"CommsReparentChannel", compassv1connect.CommsServiceReparentChannelProcedure},
		{"CommsApprovePeer", compassv1connect.CommsServiceApprovePeerProcedure},
		{"CommsRevokePeer", compassv1connect.CommsServiceRevokePeerProcedure},
		{"CommsListPeers", compassv1connect.CommsServiceListPeersProcedure},
		{"CommsPostMessage", compassv1connect.CommsServicePostMessageProcedure},
		{"CommsSubscribeComms", compassv1connect.CommsServiceSubscribeCommsProcedure},
		{"CommsListAgentBoards", compassv1connect.CommsServiceListAgentBoardsProcedure},
	}

	for _, tc := range open {
		t.Run(tc.name, func(t *testing.T) {
			err := gateServerCall(interceptor, withCaller(context.Background(), nonAdminID), tc.procedure, connect.StreamTypeUnary, recordingSpy(&spyResult{}))
			if err != nil {
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
		{"RenameAccount", compassv1connect.CompassServiceRenameAccountProcedure, true, true},
		{"RemoveAgentWorkspace", compassv1connect.CompassServiceRemoveAgentWorkspaceProcedure, true, true},
		{"PutAgentConfig", compassv1connect.CompassServicePutAgentConfigProcedure, true, true},
		{"DeleteAgentConfig", compassv1connect.CompassServiceDeleteAgentConfigProcedure, true, true},
		{"GetRefLinkRules", compassv1connect.CompassServiceGetRefLinkRulesProcedure, false, true},
		{"GetServerInfo", compassv1connect.CompassServiceGetServerInfoProcedure, false, true},
		{"SubscribeEvents", compassv1connect.CompassServiceSubscribeEventsProcedure, false, true},
		{"ListBoardIssues", compassv1connect.CompassServiceListBoardIssuesProcedure, false, true},
		{"SearchIssues", compassv1connect.CompassServiceSearchIssuesProcedure, false, true},
		{"SubscribeAgentSession", compassv1connect.CompassServiceSubscribeAgentSessionProcedure, false, true},
		{"GetAgentConfigInfo", compassv1connect.CompassServiceGetAgentConfigInfoProcedure, false, true},
		{"CommsCreateUser", compassv1connect.CommsServiceCreateUserProcedure, false, true},
		{"CommsCreateChannel", compassv1connect.CommsServiceCreateChannelProcedure, false, true},
		{"CommsUpdateChannelMembers", compassv1connect.CommsServiceUpdateChannelMembersProcedure, false, true},
		{"CommsReparentAgent", compassv1connect.CommsServiceReparentAgentProcedure, false, true},
		{"CommsApprovePeer", compassv1connect.CommsServiceApprovePeerProcedure, false, true},
		{"CommsRevokePeer", compassv1connect.CommsServiceRevokePeerProcedure, false, true},
		{"CommsListPeers", compassv1connect.CommsServiceListPeersProcedure, false, true},
		{"CommsReparentChannel", compassv1connect.CommsServiceReparentChannelProcedure, false, true},
		{"CommsPostMessage", compassv1connect.CommsServicePostMessageProcedure, false, true},
		{"CommsListAgentBoards", compassv1connect.CommsServiceListAgentBoardsProcedure, false, true},
		{"CommsSubscribeComms", compassv1connect.CommsServiceSubscribeCommsProcedure, false, true},
		{"SkipBatchWindow", compassv1connect.CompassServiceSkipBatchWindowProcedure, false, true},
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

// TestAdminGateServerInterceptor exercises the Connect v2 interceptor with both
// unary and server-streaming procedures. A denied call never reaches its handler.
func TestAdminGateServerInterceptor(t *testing.T) {
	tests := []struct {
		name       string
		procedure  string
		caller     store.AccountID
		streamType connect.StreamType
		wantDeny   bool
	}{
		{name: "admin passes admin-only unary", procedure: compassv1connect.CompassServiceStartAgentSessionProcedure, caller: adminID, streamType: connect.StreamTypeUnary},
		{name: "non-admin denied admin-only unary", procedure: compassv1connect.CompassServiceStartAgentSessionProcedure, caller: nonAdminID, streamType: connect.StreamTypeUnary, wantDeny: true},
		{name: "open unary passes non-admin", procedure: compassv1connect.CompassServiceGetServerInfoProcedure, caller: nonAdminID, streamType: connect.StreamTypeUnary},
		{name: "open server stream passes non-admin", procedure: compassv1connect.CompassServiceSubscribeEventsProcedure, caller: nonAdminID, streamType: connect.StreamTypeServer},
		{name: "unknown procedure fails closed", procedure: "/compass.v1.CompassService/Bogus", caller: nonAdminID, streamType: connect.StreamTypeUnary, wantDeny: true},
	}
	t.Run("missing caller is denied", func(t *testing.T) {
		err := gateServerCall(NewAdminGate(adminID), t.Context(), compassv1connect.CompassServiceStartAgentSessionProcedure, connect.StreamTypeUnary, recordingSpy(&spyResult{}))
		wantPermissionDenied(t, err, "admin-only RPC without a caller")
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &spyResult{}
			err := gateServerCall(NewAdminGate(adminID), withCaller(t.Context(), tt.caller), tt.procedure, tt.streamType, recordingSpy(rec))
			if tt.wantDeny {
				wantPermissionDenied(t, err, tt.name)
				if rec.called {
					t.Fatal("handler ran for a denied call")
				}
				return
			}
			if err != nil {
				t.Fatalf("allowed call returned error: %v", err)
			}
			if !rec.called || !rec.hasCaller || rec.caller != tt.caller {
				t.Fatalf("handler result = %+v, want caller %q", rec, tt.caller)
			}
		})
	}
}
