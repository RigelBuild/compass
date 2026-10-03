//go:build unix

package runner

import (
	"context"
	"errors"
	"os"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnertest"
)

// A re-Provision that fails because the name is still taken must not close the
// running container's socket; a resume on that container must then accept
// Deliver and reach the agent over the socket.
func TestFailedReprovisionKeepsLiveContainerSocket(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	h := newTransportFixtureWithEngine(t, &recordingRelay{}, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })

	req := &compassv1.ProvisionAgentWorkspaceRequest{}
	name, err := h.Provision(ctx, req, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-1")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	path := listenerPath(t, h, name)
	if err := h.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop = %v", err)
	}

	if _, err := h.Provision(ctx, req, "acct-1"); !errors.Is(err, errAlreadyProvisioned) {
		t.Fatalf("re-Provision onto a launched name = %v, want errAlreadyProvisioned", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket of the still-running container after a failed re-Provision: %v, want it kept", err)
	}

	resumed, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name, ResumeSessionId: sessionID}, "transcript", "")
	if err != nil {
		t.Fatalf("resume Start = %v", err)
	}
	t.Cleanup(func() {
		if err := h.Stop(ctx, resumed); err != nil {
			t.Errorf("Stop resumed = %v", err)
		}
	})
	if err := h.Deliver(ctx, resumed, &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "hi"}},
	}); err != nil {
		t.Fatalf("Deliver after resume = %v, want success", err)
	}

	subCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	stream, err := runnertest.DialAgentSocket(t, path).Control(subCtx, connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
	if err != nil {
		t.Fatalf("agent Control over the kept socket = %v", err)
	}
	defer func() {
		if err := stream.Close(); err != nil {
			t.Errorf("closing Control stream: %v", err)
		}
	}()
	if !stream.Receive() {
		t.Fatalf("resumed agent got no op over the socket (stream err %v)", stream.Err())
	}
	if stream.Msg().GetReplayComplete() == nil {
		t.Fatalf("first op = %v, want replay_complete", stream.Msg())
	}
}
