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

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

func TestSelfExitKeepsErroredSessionReloadable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexit 7\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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
	if err := h.Deliver(ctx, sessionID, &compassv1internal.AgentControl{Control: &compassv1internal.AgentControl_Prompt{Prompt: &compassv1internal.PromptControl{Input: "hi"}}}); err != nil {
		t.Fatalf("Deliver after Reload = %v", err)
	}
	if err := h.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop after Reload = %v", err)
	}
	assertNoTerminalFrame(t, server)
}

func TestSelfExitAllowsResumeStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexit 7\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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
			name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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

func TestReloadStartFailureMarksErroredAndResumeWorks(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "stay-up")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nif [ -e '"+marker+"' ]; then exec sleep 120; fi\nexec sleep 120\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	ctx := context.Background()
	t.Cleanup(func() { h.Close(ctx) })
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "acct-1"})
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
