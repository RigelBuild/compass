//go:build unix

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnertest"
)

func TestSelfExitKeepsErroredSessionReloadable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexit 7\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-dies")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	assertErroredFrame(t, server, sessionID)
	statuses, err := h.Status(ctx, sessionID)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("Status after self-exit = %v, %v; want retained ERRORED session", statuses, err)
	}
	// An ERRORED session has no agent to receive ops; Deliver must refuse so the
	// Server keeps the message owed instead of treating it as dispatched.
	if err := h.Deliver(ctx, sessionID, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "too early"}}}); !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Deliver to ERRORED session = %v, want errSessionUnknown", err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("writing marker: %v", err)
	}
	if err := h.Reload(ctx, sessionID); err != nil {
		t.Fatalf("Reload after self-exit = %v", err)
	}
	statuses, err = h.Status(ctx, sessionID)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("Status after Reload = %v, %v; want READY", statuses, err)
	}
	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	controlStream := assertFirstReplayComplete(t, client)
	if err := h.Deliver(ctx, sessionID, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "hi"}}}); err != nil {
		t.Fatalf("Deliver after Reload = %v", err)
	}
	assertNextPrompt(t, controlStream, "hi")
	if err := h.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop after Reload = %v", err)
	}
	assertNoTerminalFrame(t, server)
}

func assertFirstReplayComplete(t *testing.T, client compassv1internalconnect.AgentGatewayClient) *connect.ServerStreamForClient[compassv1internal.AgentControl] {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	stream, err := client.Control(ctx, connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
	if err != nil {
		t.Fatalf("Control subscription = %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if !stream.Receive() || stream.Msg().GetReplayComplete() == nil {
		t.Fatalf("first control op = %v, err %v; want replay_complete", stream.Msg(), stream.Err())
	}
	return stream
}

func assertNextPrompt(t *testing.T, stream *connect.ServerStreamForClient[compassv1internal.AgentControl], input string) {
	t.Helper()
	if !stream.Receive() {
		t.Fatalf("no prompt reached the agent (stream err %v)", stream.Err())
	}
	if got := stream.Msg().GetPrompt().GetInput(); got != input {
		t.Fatalf("prompt = %q, want %q", got, input)
	}
}

func TestPlainReloadSendsReplayCompleteThenUnackedOps(t *testing.T) {
	server := newCapturePublish()
	h := newTransportFixture(t, server)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-plain-reload")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	// Held by the old process, never acked: a config-driven Reload fires no Server
	// sweep, so the Runner itself must carry it to the new process.
	if err := h.Deliver(ctx, sessionID, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "before-reload"}}}); err != nil {
		t.Fatalf("Deliver before Reload = %v", err)
	}
	if err := h.Reload(ctx, sessionID); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	controlStream := assertFirstReplayComplete(t, client)
	assertNextPrompt(t, controlStream, "before-reload")
	if err := h.Deliver(ctx, sessionID, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "plain-reload"}}}); err != nil {
		t.Fatalf("Deliver after Reload = %v", err)
	}
	assertNextPrompt(t, controlStream, "plain-reload")
	if err := h.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop after Reload = %v", err)
	}
}

// The old process acks Start's replay_complete, as a real agent does, so reload must
// queue a fresh lift for the new process. Without the ack, the retained seq-1 op from
// Start would be redelivered and mask a reload that sends none.
func TestReloadAfterAckedBarrierSendsFreshReplayComplete(t *testing.T) {
	h := newTransportFixture(t, newCapturePublish())
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-acked-reload")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	first := assertFirstReplayComplete(t, client)
	publish := client.Publish(ctx)
	if err := publish.Send(&compassv1internal.PublishFrameRequest{Frame: &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ControlAck{ControlAck: &compassv1internal.ControlAck{AckedSeq: first.Msg().GetControlSeq()}},
	}}); err != nil {
		t.Fatalf("send ack: %v", err)
	}
	// The handler returns only after applying every frame, so the ack is in.
	if _, err := publish.CloseAndReceive(); err != nil {
		t.Fatalf("close ack stream: %v", err)
	}
	if err := h.Reload(ctx, sessionID); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	controlStream := assertFirstReplayComplete(t, client)
	if err := h.Deliver(ctx, sessionID, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "live"}}}); err != nil {
		t.Fatalf("Deliver after Reload = %v", err)
	}
	assertNextPrompt(t, controlStream, "live")
}

func TestSelfExitAllowsResumeStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexit 7\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-dies")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	assertErroredFrame(t, server, sessionID)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("writing marker: %v", err)
	}
	resumed, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name, ResumeSessionId: sessionID}, "transcript", "")
	if err != nil || resumed != sessionID {
		t.Fatalf("resume Start = %q, %v; want %q", resumed, err, sessionID)
	}
	if err := h.Deliver(ctx, resumed, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "hi"}}}); err != nil {
		t.Fatalf("Deliver after resume = %v", err)
	}
	if err := h.Stop(ctx, resumed); err != nil {
		t.Fatalf("Stop resumed = %v", err)
	}
	assertNoTerminalFrame(t, server)
}

// A fresh Start on a container whose session died replaces the ERRORED entry,
// so the container never reports two sessions.
func TestFreshStartReplacesErroredSession(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexit 7\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	dead, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-dies")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	assertErroredFrame(t, server, dead)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("writing marker: %v", err)
	}
	fresh, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-fresh")
	if err != nil {
		t.Fatalf("fresh Start after self-exit = %v", err)
	}
	statuses, err := h.Status(ctx, "")
	if err != nil || len(statuses) != 1 || statuses[0].GetSessionId() != fresh {
		t.Fatalf("Status(all) = %v, %v; want only %q", statuses, err, fresh)
	}
}

func TestSelfExitCodeZeroPublishesErrored(t *testing.T) {
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nexit 0\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-zero")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	assertErroredFrame(t, server, sessionID)
	statuses, err := h.Status(ctx, sessionID)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("Status after exit 0 = %v, %v; want ERRORED", statuses, err)
	}
}

func TestStopReloadRemovePublishNoRunnerTerminal(t *testing.T) {
	for _, op := range []string{"Stop", "Reload", "Remove"} {
		t.Run(op, func(t *testing.T) {
			server := newCapturePublish()
			h := newTransportFixture(t, server)
			ctx := context.Background()
			t.Cleanup(func() { h.Close(ctx) })
			name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
			if err != nil {
				t.Fatalf("Provision = %v", err)
			}
			sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-deliberate")
			if err != nil {
				t.Fatalf("Start = %v", err)
			}
			switch op {
			case "Stop":
				err = h.Stop(ctx, sessionID)
			case "Reload":
				err = h.Reload(ctx, sessionID)
				err = errors.Join(err, h.Stop(ctx, sessionID))
			case "Remove":
				err = h.Remove(ctx, name)
			}
			if err != nil {
				t.Fatalf("%s = %v", op, err)
			}
			h.retireWG.Wait()
			assertNoTerminalFrame(t, server)
		})
	}
}

func TestReloadDuringReaperKeepsNewStream(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	inWait := make(chan struct{})
	releaseWait := make(chan struct{})
	var releaseOnce, enteredOnce sync.Once
	releaseReaper := func() { releaseOnce.Do(func() { close(releaseWait) }) }
	t.Cleanup(releaseReaper)
	h.link.beforeWait = func() {
		enteredOnce.Do(func() {
			close(inWait)
			<-releaseWait
		})
	}
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	initial, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-reaper-race")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	h.mu.Lock()
	old := h.sessions[initial].stream
	h.mu.Unlock()
	if err := old.exec.Process.Kill(); err != nil {
		t.Fatalf("killing old agent: %v", err)
	}
	select {
	case <-inWait:
	case <-timeAfter():
		t.Fatal("reaper did not reach beforeWait")
	}
	reloadErr := make(chan error, 1)
	go func() { reloadErr <- h.Reload(ctx, initial) }()
	deadline := timeAfter()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		h.mu.Lock()
		old := h.sessions[initial].stream
		h.mu.Unlock()
		if old.stopping.Load() {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("Reload did not reach Stop while the reaper was held")
		}
	}
	releaseReaper()
	if err := <-reloadErr; err != nil {
		t.Fatalf("Reload = %v", err)
	}
	statuses, err := h.Status(ctx, initial)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("new stream status = %v, %v; want READY", statuses, err)
	}
	assertNoTerminalFrame(t, server)
	if err := h.Stop(ctx, initial); err != nil {
		t.Fatalf("Stop reloaded stream = %v", err)
	}
}

func TestOldStreamExitAfterReloadDoesNotErrorNewSession(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	// Only the first exit (the killed stream) is held; the reloaded stream never
	// exits during the test, so staleDone joins exactly the stale reaper.
	entered := make(chan struct{})
	release := make(chan struct{})
	staleDone := make(chan struct{})
	var exitCheck sync.Once
	h.afterExitCheck = func() func() {
		done := func() {}
		exitCheck.Do(func() {
			close(entered)
			<-release
			done = func() { close(staleDone) }
		})
		return done
	}
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-old-stream")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	h.mu.Lock()
	old := h.sessions[sessionID].stream
	h.mu.Unlock()
	if err := old.exec.Process.Kill(); err != nil {
		t.Fatalf("killing old agent: %v", err)
	}
	select {
	case <-entered:
	case <-timeAfter():
		t.Fatal("retireOnExit did not reach the post-check hook")
	}
	if err := h.Reload(ctx, sessionID); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	close(release)
	select {
	case <-staleDone:
	case <-timeAfter():
		t.Fatal("stale retireOnExit did not return")
	}
	status, err := h.Status(ctx, sessionID)
	if err != nil || len(status) != 1 || status[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("Status after stale exit = %v, %v; want READY", status, err)
	}
	select {
	case frame := <-server.frames:
		t.Fatalf("stale exit published frame %v, want none", frame)
	default:
	}
	if err := h.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop reloaded stream = %v", err)
	}
}

func TestReloadStartFailureMarksErroredAndResumeWorks(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexec sleep 120\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-reload-fails")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	engine.mu.Lock()
	engine.execErr = errors.New("injected start failure")
	engine.mu.Unlock()
	if err := h.Reload(ctx, sessionID); err == nil {
		t.Fatal("Reload = nil, want injected StartAgent failure")
	}
	statuses, err := h.Status(ctx, sessionID)
	if err != nil || len(statuses) != 1 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("Status after failed Reload = %v, %v; want ERRORED", statuses, err)
	}
	assertErroredFrame(t, server, sessionID)
	engine.mu.Lock()
	engine.execErr = nil
	engine.mu.Unlock()
	resumed, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name, ResumeSessionId: sessionID}, "transcript", "")
	if err != nil || resumed != sessionID {
		t.Fatalf("resume Start = %q, %v; want %q", resumed, err, sessionID)
	}
	if err := h.Stop(ctx, resumed); err != nil {
		t.Fatalf("Stop resumed = %v", err)
	}
	assertNoTerminalFrame(t, server)
}

func assertErroredFrame(t *testing.T, server *capturePublish, sessionID string) {
	t.Helper()
	select {
	case frame := <-server.frames:
		if frame.GetSessionId() != sessionID || frame.GetFrame().GetSession().GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
			t.Fatalf("terminal frame = %v, want ERRORED for %q", frame, sessionID)
		}
	case <-timeAfter():
		t.Fatal("no Runner ERRORED frame after unrequested exit")
	}
}

func assertNoTerminalFrame(t *testing.T, server *capturePublish) {
	t.Helper()
	select {
	case frame := <-server.frames:
		t.Fatalf("unexpected Runner frame: %v", frame)
	default:
	}
}
