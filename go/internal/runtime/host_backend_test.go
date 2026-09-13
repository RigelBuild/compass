package runtime

// host_backend_test.go exercises HostRuntime against real short-lived host
// processes — no mocks of the OS. Every test uses t.TempDir() for state and
// gates on process state (a pid file, an exit) rather than a fixed sleep, so it
// runs on any Linux box with no container engine present and does not flake.

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newHostRuntime builds a HostRuntime rooted at a temp dir.
func newHostRuntime(t *testing.T) *HostRuntime {
	t.Helper()
	return NewHostRuntime(t.TempDir())
}

// createStarted creates and starts a handle named name, returning its id.
func createStarted(t *testing.T, h *HostRuntime, name string) WorkloadID {
	t.Helper()
	id, err := h.Create(t.Context(), WorkloadSpec{Name: name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id
}

// TestHostLifecycle drives the full Create → Start → Exec → Stop → Remove →
// Exists path, asserting each transition's observable result.
func TestHostLifecycle(t *testing.T) {
	h := newHostRuntime(t)
	id, err := h.Create(t.Context(), WorkloadSpec{Name: "agent-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	exists, err := h.Exists(t.Context(), "agent-1")
	if err != nil || !exists {
		t.Fatalf("Exists after Create = (%v, %v), want (true, nil)", exists, err)
	}

	// Exec before Start must fail: the handle is not started.
	if _, execErr := h.Exec(t.Context(), id, NewExecSpec("true")); execErr == nil {
		t.Fatal("Exec before Start = nil error, want not-started error")
	}

	if startErr := h.Start(t.Context(), id); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}

	out, err := h.Exec(t.Context(), id, NewExecSpec("sh", "-c", "echo hi"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 || strings.TrimSpace(out.Stdout) != "hi" {
		t.Fatalf("Exec out = %+v, want exit 0 stdout %q", out, "hi")
	}

	if stopErr := h.Stop(t.Context(), id, time.Second); stopErr != nil {
		t.Fatalf("Stop (no live process) = %v, want nil", stopErr)
	}

	if rmErr := h.Remove(t.Context(), id); rmErr != nil {
		t.Fatalf("Remove: %v", rmErr)
	}
	exists, err = h.Exists(t.Context(), "agent-1")
	if err != nil || exists {
		t.Fatalf("Exists after Remove = (%v, %v), want (false, nil)", exists, err)
	}
}

// TestHostCreateRefusesDuplicateName: a second Create of a live name is refused
// rather than silently clobbering the first handle.
func TestHostCreateRefusesDuplicateName(t *testing.T) {
	h := newHostRuntime(t)
	if _, err := h.Create(t.Context(), WorkloadSpec{Name: "dup"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := h.Create(t.Context(), WorkloadSpec{Name: "dup"}); err == nil {
		t.Fatal("second Create of duplicate name = nil, want an error")
	}
}

// TestHostExecNonZeroExitIsNotError is the contract's sharpest edge: a command
// that exits non-zero is a SUCCESSFUL call returning the exit code, never a Go
// error.
func TestHostExecNonZeroExitIsNotError(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-x")
	out, err := h.Exec(t.Context(), id, NewExecSpec("sh", "-c", "exit 3"))
	if err != nil {
		t.Fatalf("Exec exit-3 err = %v, want nil (non-zero exit is not an error)", err)
	}
	if out.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", out.ExitCode)
	}
}

// TestHostExecLeakedChildKeepsExitStatus: a command that SUCCEEDS but leaves a
// background child holding the output pipe must still report success. Go's
// WaitDelay fires on the orphan's inherited pipe, not on the command. A
// non-zero exit is already an *exec.ExitError and takes an earlier branch, so
// exit 0 is the only case that reaches the WaitDelay path — and the case where
// a completed run's verdict and output would otherwise be thrown away.
func TestHostExecLeakedChildKeepsExitStatus(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-leak")
	// The shell exits at once; the backgrounded child keeps stdout open well
	// past the 10s WaitDelay.
	script := sleepBin + " 30 & echo parent-done"
	out, err := h.Exec(t.Context(), id, NewExecSpec("sh", "-c", script))
	if err != nil {
		t.Fatalf("Exec with leaked child err = %v, want nil", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (the command succeeded)", out.ExitCode)
	}
	if !strings.Contains(out.Stdout, "parent-done") {
		t.Fatalf("Stdout = %q, want it to retain the completed command's output", out.Stdout)
	}
}

// TestHostExecHonorsEnvWorkdirStdin: the child inherits the Runner's
// environment, ExecSpec.Env overrides it per key, and the child runs in its
// workdir and reads its stdin.
func TestHostExecHonorsEnvWorkdirStdin(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-e")
	dir := t.TempDir()

	spec := ExecSpec{
		Command: []string{"sh", "-c", "printf '%s\\n' \"$FOO\"; pwd; cat"},
		Env:     map[string]string{"FOO": "bar"},
		Workdir: &dir,
	}
	spec = spec.WithStdin("stdin-payload")

	out, err := h.Exec(t.Context(), id, spec)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// pwd may resolve symlinks (e.g. /tmp → /private/tmp), so compare the
	// resolved forms.
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	lines := strings.SplitN(out.Stdout, "\n", 2)
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want two lines", out.Stdout)
	}
	if !strings.HasPrefix(lines[0], "bar") {
		t.Fatalf("env not honored: stdout %q, want FOO=bar prefix", out.Stdout)
	}
	gotDir, payload, _ := strings.Cut(lines[1], "\n")
	if gotDir != wantDir {
		t.Fatalf("workdir = %q, want %q", gotDir, wantDir)
	}
	if payload != "stdin-payload" {
		t.Fatalf("stdin = %q, want %q", payload, "stdin-payload")
	}
}

// TestHostExecInheritsRunnerEnv: an unqualified command resolves because the
// child inherited the Runner's PATH. With no inheritance the host tier has no
// image to supply one, so this exits 127 (command not found) instead.
func TestHostExecInheritsRunnerEnv(t *testing.T) {
	if os.Getenv("PATH") == "" {
		t.Skip("Runner has no PATH to inherit")
	}
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-inherit")
	out, err := h.Exec(t.Context(), id, NewExecSpec("sh", "-c", "sleep 0 && echo resolved"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d (stderr %q), want 0: an unqualified command must resolve via the inherited PATH", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "resolved") {
		t.Fatalf("Stdout = %q, want \"resolved\"", out.Stdout)
	}
}

// TestHostExecTimeout: a command that overruns the per-command cap is killed
// and surfaced as a TimeoutError, not a ctx error. A short WithTimeout makes
// the internal deadline fire deterministically against a command that would
// otherwise run far longer.
func TestHostExecTimeout(t *testing.T) {
	h := newHostRuntime(t).WithTimeout(50 * time.Millisecond)
	id := createStarted(t, h, "agent-t")
	_, err := h.Exec(t.Context(), id, NewExecSpec("sleep", "30"))
	if _, ok := errors.AsType[*TimeoutError](err); !ok {
		t.Fatalf("Exec err = %v, want *TimeoutError", err)
	}
}

// TestHostExecStreamingStreamsAndStops spawns a long-lived process, reads its
// streamed stdout, then Stop terminates it. Gates on the streamed marker line,
// never a fixed sleep.
func TestHostExecStreamingStreamsAndStops(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-s")

	// Emit a marker, then loop forever. The marker proves the stream is live
	// before we stop it.
	spec := NewStreamingExecSpec("sh", "-c", "echo started; while true; do sleep 1; done")
	se, err := h.ExecStreaming(t.Context(), id, spec)
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}

	if line := readLine(t, se.IO.Stdout); strings.TrimSpace(line) != "started" {
		t.Fatalf("first stdout line = %q, want %q", line, "started")
	}

	if stopErr := h.Stop(t.Context(), id, 5*time.Second); stopErr != nil {
		t.Fatalf("Stop: %v", stopErr)
	}
	// After Stop the process is gone: Wait returns (a signalled exit is a
	// non-nil error, which is expected).
	_ = se.Process.Wait()
}

// TestHostStopEscalatesToSIGKILL: a process that traps and ignores SIGTERM is
// still stopped, because Stop escalates to SIGKILL after the timeout.
func TestHostStopEscalatesToSIGKILL(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-k")

	// Trap SIGTERM (ignore it), announce readiness, then sleep forever. Only
	// SIGKILL can stop it.
	script := "trap '' TERM; echo ready; while true; do sleep 1; done"
	se, err := h.ExecStreaming(t.Context(), id, NewStreamingExecSpec("sh", "-c", script))
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}
	if line := readLine(t, se.IO.Stdout); strings.TrimSpace(line) != "ready" {
		t.Fatalf("first stdout line = %q, want %q", line, "ready")
	}

	start := time.Now()
	if stopErr := h.Stop(t.Context(), id, 500*time.Millisecond); stopErr != nil {
		t.Fatalf("Stop: %v", stopErr)
	}
	// Stop returned only after the SIGKILL escalation, so it waited at least the
	// grace window; the process is now reaped.
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("Stop returned in %s, want >= the ~500ms grace before SIGKILL", elapsed)
	}
	waitErr := se.Process.Wait()
	if waitErr == nil {
		t.Fatal("Wait after SIGKILL = nil, want a signalled-exit error")
	}
}

// TestHostAsUser: nil and the Runner's own euid are accepted; any other uid is
// rejected with UnsupportedUserError.
func TestHostAsUser(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-u")
	euid := strconv.Itoa(os.Geteuid())

	if _, err := h.Exec(t.Context(), id, NewExecSpec("true").AsUser(euid)); err != nil {
		t.Fatalf("Exec AsUser(euid) = %v, want nil", err)
	}

	other := strconv.Itoa(os.Geteuid() + 1)
	_, err := h.Exec(t.Context(), id, NewExecSpec("true").AsUser(other))
	var unsupported *UnsupportedUserError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Exec AsUser(other) err = %v, want *UnsupportedUserError", err)
	}

	// Streaming honors the same rule.
	_, err = h.ExecStreaming(t.Context(), id, NewStreamingExecSpec("sleep", "1").AsUser(other))
	if !errors.As(err, &unsupported) {
		t.Fatalf("ExecStreaming AsUser(other) err = %v, want *UnsupportedUserError", err)
	}
}

// TestHostMountLabelAndResize: MountLabel is empty, Resize is the typed
// unsupported error.
func TestHostMountLabelAndResize(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-d")

	label, err := h.MountLabel(t.Context(), id)
	if err != nil || label != "" {
		t.Fatalf("MountLabel = (%q, %v), want (\"\", nil)", label, err)
	}

	if resizeErr := h.Resize(t.Context(), id, ResourceLimits{CPUShares: 512}); !errors.Is(resizeErr, ErrResizeUnsupportedOnHost) {
		t.Fatalf("Resize err = %v, want ErrResizeUnsupportedOnHost", resizeErr)
	}
}

// TestHostRemoveDeletesStateDirNotSiblings: Remove deletes the handle's state
// dir but never touches a sibling file outside it — the tier's permanent-writes
// posture.
func TestHostRemoveDeletesStateDirNotSiblings(t *testing.T) {
	h := newHostRuntime(t)
	id, err := h.Create(t.Context(), WorkloadSpec{Name: "agent-r"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A sibling file under the shared state root, outside any handle's dir.
	sibling := filepath.Join(h.stateRoot, "sibling.txt")
	if writeErr := os.WriteFile(sibling, []byte("keep me"), 0o600); writeErr != nil {
		t.Fatalf("writing sibling: %v", writeErr)
	}

	stateDir := h.handles[id].stateDir
	if _, statErr := os.Stat(stateDir); statErr != nil {
		t.Fatalf("state dir not created: %v", statErr)
	}

	if rmErr := h.Remove(t.Context(), id); rmErr != nil {
		t.Fatalf("Remove: %v", rmErr)
	}
	if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("state dir still present after Remove: stat err = %v", statErr)
	}
	if _, statErr := os.Stat(sibling); statErr != nil {
		t.Fatalf("sibling file removed or unreadable after Remove: %v", statErr)
	}
}

// TestHostRemoveKillsLiveProcess: Remove force-kills a still-running streaming
// child before deleting the state dir.
func TestHostRemoveKillsLiveProcess(t *testing.T) {
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-rk")
	se, err := h.ExecStreaming(t.Context(), id, NewStreamingExecSpec("sh", "-c", "echo up; while true; do sleep 1; done"))
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}
	if line := readLine(t, se.IO.Stdout); strings.TrimSpace(line) != "up" {
		t.Fatalf("first stdout line = %q, want %q", line, "up")
	}

	if rmErr := h.Remove(t.Context(), id); rmErr != nil {
		t.Fatalf("Remove: %v", rmErr)
	}
	// The child was SIGKILLed by Remove; Wait now returns the signalled exit.
	waitErr := se.Process.Wait()
	if _, ok := errors.AsType[*exec.ExitError](waitErr); !ok {
		t.Fatalf("Wait after Remove = %v, want an *exec.ExitError from the killed child", waitErr)
	}
}

// readLine reads a single newline-terminated line from r, failing the test if
// none arrives within a generous deadline. Gating on the line — not a sleep —
// is what keeps the streaming tests non-flaky.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 0, 64)
		one := make([]byte, 1)
		for {
			n, err := r.Read(one)
			if n > 0 {
				if one[0] == '\n' {
					ch <- result{line: string(buf)}
					return
				}
				buf = append(buf, one[0])
			}
			if err != nil {
				ch <- result{line: string(buf), err: err}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil && res.line == "" {
			t.Fatalf("readLine: %v", res.err)
		}
		return res.line
	case <-time.After(10 * time.Second):
		t.Fatal("readLine: no line within 10s")
		return ""
	}
}

// TestHostRemoveConcurrentWithExecStreaming drives Remove against a concurrent
// ExecStreaming on the same handle. handle.proc is written under the mutex, so
// reading it unlocked is a data race. This guards that race only — it is
// meaningful under -race and asserts nothing about which of the two wins.
func TestHostRemoveConcurrentWithExecStreaming(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}
	for range 40 {
		h := newHostRuntime(t)
		id := createStarted(t, h, "agent-race")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			stream, execErr := h.ExecStreaming(t.Context(), id, NewStreamingExecSpec(sleepBin, "300"))
			if execErr == nil {
				_ = stream.Process.Kill()
				_ = stream.Process.Wait()
			}
		}()
		go func() {
			defer wg.Done()
			_ = h.Remove(t.Context(), id)
		}()
		wg.Wait()
	}
}

// TestHostExecStreamingRemovedDuringSpawn pins the removal-during-spawn
// window. startedHandle releases the lock before the spawn, so a concurrent
// Remove can delete the handle and wipe the state dir while the child is
// already running and not yet recorded — after which nothing can reach it by
// id. The seam forces that interleaving instead of racing for it.
func TestHostExecStreamingRemovedDuringSpawn(t *testing.T) {
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}
	h := newHostRuntime(t)
	id := createStarted(t, h, "agent-removed")
	h.afterSpawn = func() {
		if removeErr := h.Remove(t.Context(), id); removeErr != nil {
			t.Errorf("Remove during spawn: %v", removeErr)
		}
	}

	stream, err := h.ExecStreaming(t.Context(), id, NewStreamingExecSpec(sleepBin, "300"))
	if err == nil {
		_ = stream.Process.Kill()
		_ = stream.Process.Wait()
		t.Fatal("ExecStreaming returned a live stream for a workload removed mid-spawn; the child would outlive every way of reaching it")
	}
	if stream != nil {
		t.Fatalf("ExecStreaming returned a stream alongside err = %v", err)
	}
	if !strings.Contains(err.Error(), "removed during spawn") {
		t.Fatalf("err = %v, want the removed-during-spawn refusal", err)
	}
}
