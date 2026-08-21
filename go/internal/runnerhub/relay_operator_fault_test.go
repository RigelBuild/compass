//go:build unix

package runnerhub

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// End-to-end proof of the operator-fault seam: a Runner provision that fails on
// an operator-fault configuration error surfaces to the Hub caller as
// connect.CodeFailedPrecondition (never Internal), with both the underlying
// socket diagnostic and the appended operator-fault sentinel phrase intact in
// the error text. This drives the mapping through the REAL router/relay wiring
// rather than the pure runnerErrorToConnect function, so the plumbing that ties
// the Runner's error result to the client-facing Connect code is what is proven.
// See docs/designs/platform/compass-runner-gateway-error-sentinels/design.md.
func TestProvisionRelaySurfacesOperatorFaultAsFailedPrecondition(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}

	// A faithful stand-in for the %w chain the real Runner's err.Error() carries:
	// the socket diagnostic followed by the appended gateway.ErrOperatorConfig
	// sentinel text. The Runner is simulated here, so the wire carries only the
	// string — the gateway package is deliberately not imported.
	const diag = "serving agent socket for container \"cont-op\": agent socket path \"/run/compass/containers/cont-op/agent.sock\" is 120 bytes, over the 108-byte AF_UNIX limit: shorten the Runner's --runtime-dir or the agent account id: operator-fault runner configuration"

	router.attach(func(cmd *compassv1internal.SessionsResponse) error {
		go router.complete(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
				Code:    compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION,
				Message: diag,
			}},
		})
		return nil
	})

	_, _, err = hub.Provision(context.Background(), "req-op", &compassv1.ProvisionAgentWorkspaceRequest{})
	if err == nil {
		t.Fatal("Provision failing on an operator-fault error = nil, want FailedPrecondition")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("Provision error code = %v, want FailedPrecondition (operator fault, not Internal)", got)
	}
	if !strings.Contains(err.Error(), "serving agent socket for container") {
		t.Fatalf("Provision error = %q, want the socket diagnostic to survive the relay", err.Error())
	}
	if !strings.Contains(err.Error(), "operator-fault runner configuration") {
		t.Fatalf("Provision error = %q, want the operator-fault sentinel phrase to survive the relay", err.Error())
	}
}
