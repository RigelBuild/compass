//go:build pgtest

package comms

// OpenAgentWorkspace handler contracts after the handle cutover: agent_handle
// resolves at the edge, and every miss is NOT_FOUND naming the submitted handle.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// TestOpenAgentWorkspaceByHandleOpensOwnAgent: a bare handle resolves in the
// caller's own namespace.
func TestOpenAgentWorkspaceByHandleOpensOwnAgent(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "worker")

	resp, err := svc.OpenAgentWorkspace(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.OpenAgentWorkspaceRequest{
		AgentHandle: "worker",
	}))
	if err != nil {
		t.Fatalf("OpenAgentWorkspace: %v", err)
	}
	ws := resp.Msg.GetWorkspace()
	if ws.GetId() == "" {
		t.Fatal("OpenAgentWorkspace returned an empty workspace id")
	}
	if ws.GetAgentAccountId() != string(agent.ID) {
		t.Fatalf("workspace agent = %q, want %q", ws.GetAgentAccountId(), agent.ID)
	}
}

// TestOpenAgentWorkspaceForeignTargetIndistinguishableFromUnknown: a foreign
// agent resolves (not viewer-scoped), so the store's refusal names its account id
// unless the handler re-keys it; it must match an unknown handle, code AND message.
func TestOpenAgentWorkspaceForeignTargetIndistinguishableFromUnknown(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	mustAgent(t, st, owner.ID, "worker")
	stranger := mustUser(t, st, "stranger")

	for _, handle := range []string{"owner/worker", "owner/ghost", "nobody/worker", "worker"} {
		_, err := svc.OpenAgentWorkspace(WithActor(ctx, stranger.ID), connect.NewRequest(&compassv1.OpenAgentWorkspaceRequest{
			AgentHandle: handle,
		}))
		connectNotFoundFor(t, err, handle, "OpenAgentWorkspace("+handle+")")
	}
}
