//go:build unix

package runner

// Exit reporting when the agent's pipes outlive it: the reaper reaps first and
// joins the drains with a bound that starts at exit, so a descendant holding a
// pipe cannot delay ERRORED, a live agent's drains are never cut, and output
// still buffered in the pipe at exit reaches the exit record.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

func TestSelfExitIsReportedWhileADescendantHoldsThePipes(t *testing.T) {
	dir := t.TempDir()
	pidFile, release := filepath.Join(dir, "descendant.pid"), filepath.Join(dir, "exit-now")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nsleep 120 &\necho $! > '"+pidFile+
		"'\nwhile [ ! -e '"+release+"' ]; do sleep 0.01; done\nexit 7\n")
	server := newCapturePublish()
	h := newTransportFixtureWithEngine(t, server, engine)
	h.link.drainGrace = 100 * time.Millisecond
	// context.Background() is the test root: t.Context() is already cancelled in cleanup.
	t.Cleanup(func() { h.Close(context.Background()) })
	t.Cleanup(func() { killRecordedDescendant(t, pidFile) })

	ctx := t.Context()
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-leaked-pipe")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	h.mu.Lock()
	stream := h.sessions[sessionID].stream
	h.mu.Unlock()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	assertErroredFrame(t, server, sessionID)
	pid := readRecordedPid(t, pidFile)
	assertAlive(t, pid, "before ERRORED was observed")
	select {
	case <-stream.reaped:
	case <-timeAfter():
		t.Fatal("reaper never finished: the drain join is not bounded from exit")
	}
	assertAlive(t, pid, "before the reaper finished")
}

func TestDrainOutlivesTheGraceWhileTheAgentLives(t *testing.T) {
	release := filepath.Join(t.TempDir(), "write-now")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nwhile [ ! -e '"+release+
		"' ]; do sleep 0.01; done\nhead -c 131072 /dev/zero | tr '\\0' x >&2\necho >&2\necho after-flood >&2\nexit 0\n")
	logs := newCaptureLog()
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))
	const grace = 50 * time.Millisecond
	link.drainGrace = grace
	stream, err := link.StartAgent(t.Context(), "sess-late-flood", runtime.WorkloadID("c1"), engine, testAgentEnv(), logs.logger())
	if err != nil {
		t.Fatalf("StartAgent = %v", err)
	}

	// Elapsed time is the input here: a bound timed from spawn would fire now.
	<-time.After(4 * grace)
	select {
	case <-stream.reaped:
		t.Fatal("reaper finished while the agent was still waiting to write")
	default:
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.reaped:
	case <-timeAfter():
		t.Fatal("agent never exited: its >64KB stderr write blocked on an undrained pipe")
	}
	exit := recvExitRecord(t, logs)
	if got := lastLine(exit.attrs["stderr_tail"]); got != "after-flood" {
		t.Fatalf("stderr_tail ends %q, want the line after the flood", got)
	}
}

// The drain is parked on the agent's first line until the exit is observed, so
// the last lines sit unread in the pipe when the agent is reaped. A reap that
// closes the read end (os/exec's StdoutPipe) drops them.
func TestStderrTailWrittenJustBeforeExitSurvives(t *testing.T) {
	release := filepath.Join(t.TempDir(), "finish")
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\necho first >&2\nwhile [ ! -e '"+release+
		"' ]; do sleep 0.01; done\necho second-to-last >&2\necho final-words >&2\nexit 7\n")
	logs := newCaptureLog()
	held := &holdFirstLine{next: logs, parked: make(chan struct{}), unpark: make(chan struct{})}
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))
	stream, err := link.StartAgent(t.Context(), "sess-last-words", runtime.WorkloadID("c1"), engine, testAgentEnv(), slog.New(held))
	if err != nil {
		t.Fatalf("StartAgent = %v", err)
	}
	select {
	case <-held.parked:
	case <-timeAfter():
		t.Fatal("drain never logged the first stderr line")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.waitDone:
	case <-timeAfter():
		close(held.unpark)
		t.Fatal("agent exit was not observed while its drain was parked")
	}
	close(held.unpark)
	select {
	case <-stream.reaped:
	case <-timeAfter():
		t.Fatal("reaper did not finish after the agent exited")
	}
	exit := recvExitRecord(t, logs)
	if exit.attrs["exit_result"] != "exit status 7" {
		t.Fatalf("exit_result = %q, want exit status 7", exit.attrs["exit_result"])
	}
	if got := exit.attrs["stderr_tail"]; got != "first\nsecond-to-last\nfinal-words" {
		t.Fatalf("stderr_tail = %q: output still in the pipe at exit was lost", got)
	}
}

// holdFirstLine parks the first agent stderr record until unpark closes, then
// forwards every record to next.
type holdFirstLine struct {
	next   slog.Handler
	once   sync.Once
	parked chan struct{}
	unpark chan struct{}
}

func (h *holdFirstLine) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}
func (h *holdFirstLine) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *holdFirstLine) WithGroup(string) slog.Handler      { return h }
func (h *holdFirstLine) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "agent stderr" {
		h.once.Do(func() {
			close(h.parked)
			<-h.unpark
		})
	}
	return h.next.Handle(ctx, r)
}

// recvExitRecord reads captured records until the unexpected-exit record.
func recvExitRecord(t *testing.T, logs *captureLog) logLine {
	t.Helper()
	for {
		if line := logs.recvLine(t); line.msg == "agent exited unexpectedly" {
			return line
		}
	}
}

func lastLine(s string) string { return s[strings.LastIndexByte(s, '\n')+1:] }

func readRecordedPid(t *testing.T, pidFile string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing descendant pid %q: %v", raw, err)
	}
	return pid
}

func assertAlive(t *testing.T, pid int, when string) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("descendant %d gone %s (%v): nothing held the pipes, so the test proved nothing", pid, when, err)
	}
}

// killRecordedDescendant reaps the pipe-holding sleep by its recorded pid, never
// by pattern, so it cannot outlive the test.
func killRecordedDescendant(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Errorf("reading descendant pid: %v", err)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Errorf("parsing descendant pid %q: %v", raw, err)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("killing descendant %d: %v", pid, err)
	}
}
