//go:build unix

package runnerhub

// The Server-facing command surface: the RunnerError→Connect code mapping and
// the no-Runner→Unavailable path. Every row pins the exact Connect code a client
// sees, so a mis-mapping (an ALREADY_RUNNING surfaced as Internal, a NOT_FOUND
// swallowed) is caught. The end-to-end relay test drives the mapping through the
// real router so the wiring, not just the pure function, is proven.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// runnerErrorToConnect maps each RunnerErrorCode to the exact Connect code the
// client sees. Table-driven over every code, including UNSPECIFIED (the "else"
// arm) which must fall to Internal.
func TestRunnerErrorToConnectCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		code compassv1internal.RunnerErrorCode
		want connect.Code
	}{
		{"already running", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING, connect.CodeAlreadyExists},
		{"not found", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND, connect.CodeNotFound},
		{"failed precondition", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION, connect.CodeFailedPrecondition},
		{"internal", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL, connect.CodeInternal},
		{"unspecified falls to internal", compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_UNSPECIFIED, connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runnerErrorToConnect(&compassv1internal.RunnerError{Code: tc.code, Message: "boom"})
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("runnerErrorToConnect(%v) code = %v, want %v", tc.code, got, tc.want)
			}
			// The Runner's message is surfaced, not swallowed.
			mustContain(t, err.Error(), "boom")
		})
	}
}

// A session RPC with no enrolled Runner is Unavailable — there is no Runner to
// serve it, a transport-class failure, never a per-command error code. Driven
// across every command surface so no method mis-classifies the empty registry.
func TestCommandsNoRunnerIsUnavailable(t *testing.T) {
	hub := newHubOnly() // no enroll → no Runner
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"provision", func() error {
			_, _, err := hub.Provision(ctx, "r1", &compassv1.ProvisionAgentWorkspaceRequest{})
			return err
		}},
		{"start", func() error {
			_, err := hub.Start(ctx, "r1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
			return err
		}},
		{"stop", func() error {
			_, err := hub.Stop(ctx, "r1", &compassv1.StopAgentSessionRequest{SessionId: "s1"})
			return err
		}},
		{"reload", func() error {
			_, err := hub.Reload(ctx, "r1", &compassv1.ReloadAgentSessionRequest{SessionId: "s1"})
			return err
		}},
		{"status", func() error {
			_, err := hub.Status(ctx, "r1", &compassv1.GetAgentStatusRequest{SessionId: "s1"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s with no runner = nil error, want Unavailable", tc.name)
			}
			if got := connect.CodeOf(err); got != connect.CodeUnavailable {
				t.Fatalf("%s with no runner = code %v, want Unavailable", tc.name, got)
			}
		})
	}
}

// End-to-end through the real router: a Start whose Runner returns an
// ALREADY_RUNNING RunnerError surfaces as CodeAlreadyExists at the Hub.Start
// boundary — proving the relay wires the mapping, not just the pure function.
// This is OQ6 row 3's genuine-double half: a real already-live container.
func TestStartRelaySurfacesAlreadyRunningAsAlreadyExists(t *testing.T) {
	hub := newHubOnly()
	// Enroll a Runner and bind a send that answers every command with an
	// ALREADY_RUNNING error result correlated by the pushed request id.
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		// Answer asynchronously the way the real Sessions loop does.
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
				Code:    compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING,
				Message: "session already running on container",
			}},
		})
		return nil
	})

	_, err = hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
	if err == nil {
		t.Fatal("Start against an already-running container = nil, want AlreadyExists")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("Start error code = %v, want AlreadyExists (a genuine double start)", got)
	}
}

// A successful relay returns the typed response, not an error — the happy path
// through the same wiring, so the error tests above are not the only path
// exercised.
func TestStartRelayReturnsSessionIdOnSuccess(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, _ := hub.routerFor("any")
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-ok"}},
		})
		return nil
	})

	resp, err := hub.Start(context.Background(), "req-ok", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
	if err != nil {
		t.Fatalf("Start = %v, want success", err)
	}
	if got := resp.GetSessionId(); got != "sess-ok" {
		t.Fatalf("Start session id = %q, want sess-ok", got)
	}
}

// TestStartEmitsNoInitialSignal: the initial secret materialize is pre-exec on
// the Runner (host.Start, FetchSecretsByContainer), so a bound Start pushes NO
// SecretsVersion frame. Signalling here would drive a redundant second
// materialize over the T6 rotation path — the race Start was conformed away
// from. A non-zero count means the initial-signal path was re-introduced.
func TestStartEmitsNoInitialSignal(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	hub.bindContainer("c1", testAgentAccount)
	router, _, _ := hub.routerFor("any")
	rec := newRecordingSend()
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		_ = rec.send(cmd)
		if cmd.GetStart() != nil {
			go router.complete(&compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-ok"}},
			})
		}
		return nil
	})

	if _, err := hub.Start(context.Background(), "req-1", &compassv1.StartAgentSessionRequest{ContainerName: "c1"}); err != nil {
		t.Fatalf("Start = %v, want success", err)
	}
	if pushed := secretsVersionsPushed(t, rec); len(pushed) != 0 {
		t.Fatalf("bound Start pushed %d SecretsVersion frames, want 0 (initial materialize is pre-exec)", len(pushed))
	}
}

// A successful Remove relay sends a RemoveAgentWorkspace command down the
// Sessions stream and returns the Runner's RemoveAgentWorkspaceResponse — the
// container-teardown counterpart to Provision. This drives the mapping through
// the real router (like the Start-relay happy path) so the wiring, not just the
// pure function, is proven: the command variant the Runner sees is the Remove
// variant, and the typed result flows back.
func TestRemoveRelayReturnsResponseOnSuccess(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, _ := hub.routerFor("any")
	var sawRemove bool
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		if cmd.GetRemove() != nil {
			sawRemove = true
		}
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}},
		})
		return nil
	})

	resp, err := hub.Remove(context.Background(), "req-rm", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: "c1"})
	if err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	if resp == nil {
		t.Fatal("Remove returned a nil response, want the Runner's RemoveAgentWorkspaceResponse")
	}
	if !sawRemove {
		t.Fatal("Runner did not receive a RemoveAgentWorkspace command variant")
	}
}

// Remove clears the container's provisioned account binding — the teardown
// counterpart to Provision's bindContainer. On a Provision->Remove path that
// never reached Start (promoteSession clears it there), a lingering binding would
// keep authorizing a pre-exec FetchSecrets materialize (HasContainerBinding) for
// a container that no longer exists.
//
// Mutation: dropping the unbindContainer call in Remove leaves HasContainerBinding
// true after teardown and reddens this.
func TestRemoveClearsContainerBinding(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	hub.bindContainer("c1", testAgentAccount)
	if !hub.HasContainerBinding("c1") {
		t.Fatal("precondition: container c1 should be bound after bindContainer")
	}
	router, _, _ := hub.routerFor("any")
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result:    &compassv1internal.SessionsRequest_Remove{Remove: &compassv1.RemoveAgentWorkspaceResponse{}},
		})
		return nil
	})

	if _, err := hub.Remove(context.Background(), "req-rm", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: "c1"}); err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	if hub.HasContainerBinding("c1") {
		t.Fatal("container c1 still bound after Remove, want the binding cleared (stale binding authorizes pre-exec secrets materialize)")
	}
}

// attachStatusResponder enrolls a Runner and binds its router to answer every
// GetAgentStatus command with a canned response carrying statuses, correlated by
// the pushed request id — the seam the SessionState fallback tests drive.
func attachStatusResponder(t *testing.T, hub *Hub, statuses []*compassv1.AgentSessionStatus) {
	t.Helper()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Status{
				Status: &compassv1.GetAgentStatusResponse{Statuses: statuses},
			},
		})
		return nil
	})
}

// L2 (SEA-1569 T8 review): SessionState adopts the SOLE status as this session's
// state only when that status carries NO session id (the "Runner answered without
// echoing the id" case). A sole status echoing an EMPTY id resolves ok=true.
func TestSessionStateSoleStatusEmptyIDResolves(t *testing.T) {
	hub := newHubOnly()
	attachStatusResponder(t, hub, []*compassv1.AgentSessionStatus{
		{SessionId: "", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
	})

	state, ok := hub.SessionState(context.Background(), "sess-1")
	if !ok {
		t.Fatal("SessionState with a sole empty-id status = ok=false, want ok=true (adopt the id-less answer)")
	}
	if state != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		t.Fatalf("SessionState = %v, want WORKING (the sole status's state)", state)
	}
}

// L2 (SEA-1569 T8 review): a sole status echoing a NON-EMPTY MISMATCHED id is NOT
// this session's state — a Runner bug echoing a wrong id must not reconstruct a
// wrong presence — so it is unresolved (ok=false → UNSPECIFIED). RED against
// pre-fix: the pre-fix fallback adopted the sole status unconditionally, so this
// returned ok=true with the wrong session's state.
func TestSessionStateSoleStatusMismatchedIDIsUnresolved(t *testing.T) {
	hub := newHubOnly()
	attachStatusResponder(t, hub, []*compassv1.AgentSessionStatus{
		{SessionId: "some-other-session", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
	})

	state, ok := hub.SessionState(context.Background(), "sess-1")
	if ok {
		t.Fatalf("SessionState with a sole MISMATCHED-id status = ok=true (state %v), want ok=false — a wrong-id echo must not resolve this session", state)
	}
	if state != compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		t.Fatalf("SessionState unresolved state = %v, want UNSPECIFIED", state)
	}
}

// L2 companion: a status whose id MATCHES the requested session resolves ok=true
// through the id-match loop (the primary path, unchanged by the fix) — so the
// tightened fallback did not regress the matched case.
func TestSessionStateMatchedIDResolves(t *testing.T) {
	hub := newHubOnly()
	attachStatusResponder(t, hub, []*compassv1.AgentSessionStatus{
		{SessionId: "sess-1", State: compassv1.AgentSessionState_AGENT_SESSION_STATE_READY},
	})

	state, ok := hub.SessionState(context.Background(), "sess-1")
	if !ok || state != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("SessionState(sess-1) = %v ok=%v, want READY ok=true (id-match path)", state, ok)
	}
}
