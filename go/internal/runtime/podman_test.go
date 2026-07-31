package runtime

// The podman-argv hermetic suite: the pure argv/relabel assembly that decides
// exactly what the Runner shells out to podman with. No subprocess is spawned —
// these pin the serialized command shape a real bug (a dropped flag, a wrong
// relabel suffix, a reordered env) would silently corrupt.

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMountArgRelabel(t *testing.T) {
	tests := []struct {
		name  string
		mount Mount
		want  string
	}{
		{
			// Read-only mounts (the shared bare-repo cache) get :ro plus the :Z
			// SELinux relabel so they work on an enforcing host.
			name:  "read-only relabels with ro",
			mount: Mount{HostPath: "/tmp/cache", ContainerPath: "/src", ReadOnly: true},
			want:  "/tmp/cache:/src:ro,Z",
		},
		{
			// Read-write mounts still relabel, but never carry :ro.
			name:  "read-write relabels without ro",
			mount: Mount{HostPath: "/tmp/work", ContainerPath: "/work", ReadOnly: false},
			want:  "/tmp/work:/work:Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mountArg(tc.mount); got != tc.want {
				t.Fatalf("mountArg(%+v) = %q, want %q", tc.mount, got, tc.want)
			}
		})
	}
}

func TestExecOutputSuccessTracksExitCode(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		want     bool
	}{
		{"zero exit is success", 0, true},
		{"non-zero exit is failure", 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := ExecOutput{ExitCode: tc.exitCode}
			if got := out.Success(); got != tc.want {
				t.Fatalf("ExecOutput{ExitCode: %d}.Success() = %v, want %v", tc.exitCode, got, tc.want)
			}
		})
	}
}

// The full agent exec the Runner assembles (runner.AgentEnv.execSpec): user,
// workdir, and every env key the agent reads. Env is emitted in sorted key
// order, so the expectation is exact rather than order-tolerant.
func TestExecStreamingArgsAssemblesInteractiveExec(t *testing.T) {
	spec := NewStreamingExecSpec("omp", "acp").AsUser("1000").InDir("/work")
	spec.Env["HOME"] = "/home/agent"
	spec.Env["COMPASS_WORKDIR"] = "/work"
	spec.Env["COMPASS_MODEL"] = "test-model"

	args := execStreamingArgs(ContainerID("ctr123"), spec)

	want := []string{
		"exec", "--interactive",
		"--user", "1000",
		"--workdir", "/work",
		"-e", "COMPASS_MODEL=test-model",
		"-e", "COMPASS_WORKDIR=/work",
		"-e", "HOME=/home/agent",
		"ctr123", "omp", "acp",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("execStreamingArgs = %q, want %q", args, want)
	}
}

func TestExecStreamingArgsMinimalOmitsUserAndWorkdir(t *testing.T) {
	spec := NewStreamingExecSpec("omp", "acp")

	args := execStreamingArgs(ContainerID("c"), spec)

	want := []string{"exec", "--interactive", "c", "omp", "acp"}
	if !slices.Equal(args, want) {
		t.Fatalf("execStreamingArgs = %q, want %q", args, want)
	}
}

// The env-file rides before the -e vars, so an -e key still wins on collision
// (podman applies -e after --env-file). The agent exec always carries one.
func TestExecStreamingArgsCarriesEnvFileBeforeInlineEnv(t *testing.T) {
	spec := NewStreamingExecSpec("omp", "acp").AsUser("1000").InDir("/work").WithEnvFile("/home/agent/.compass/env")
	spec.Env["HOME"] = "/home/agent"

	args := execStreamingArgs(ContainerID("ctr123"), spec)

	want := []string{
		"exec", "--interactive",
		"--user", "1000",
		"--workdir", "/work",
		"--env-file", "/home/agent/.compass/env",
		"-e", "HOME=/home/agent",
		"ctr123", "omp", "acp",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("execStreamingArgs = %q, want %q", args, want)
	}
}

// An empty env-file path (WithEnvFile("")) adds no flag — the no-op guard.
func TestExecStreamingArgsOmitsEmptyEnvFile(t *testing.T) {
	spec := NewStreamingExecSpec("omp").WithEnvFile("")

	args := execStreamingArgs(ContainerID("c"), spec)

	want := []string{"exec", "--interactive", "c", "omp"}
	if !slices.Equal(args, want) {
		t.Fatalf("execStreamingArgs with empty env-file = %q, want %q", args, want)
	}
}

// TestSpawnCaptureWaitDelayBoundsLeakedPipeHang pins the reliability contract on
// the one-shot exec path: spawnCapture must return within a bounded wall-clock
// after its per-command timeout fires, even when a descendant of the killed
// process still holds the captured stdout pipe open. Without a non-zero
// cmd.WaitDelay, cmd.Wait blocks on the stdout copy goroutine until that leaked
// pipe closes — so a wedged podman with a pipe-holding grandchild hangs the
// calling task far past the timeout it was supposed to enforce. With WaitDelay
// set, the pipes are force-closed after the delay and the call returns.
//
// Behavioral (not structural): it drives a real subprocess through Exec →
// spawnCapture and asserts the OBSERVABLE bound — the call returns a
// *TimeoutError within budget — not that any field was assigned. WithProgram
// swaps podman for a shell stub that ignores the podman argv; WithTimeout makes
// the timeout fire fast so the whole test is bounded.
//
// Cost: a green run necessarily waits ~WaitDelay (10s, hardcoded in
// spawnCapture) for the force-close, because the leaked pipe only unblocks the
// copy goroutine when WaitDelay fires. That is the honest floor for exercising
// this contract behaviorally. The stub's orphan sleeps well past that, so a
// regression (WaitDelay dropped) is unmistakable: the call would instead hang
// to the orphan's exit, which the safety deadline below trips.
func TestSpawnCaptureWaitDelayBoundsLeakedPipeHang(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan.pid")

	// A shell stub that (1) forks a child inheriting the captured stdout pipe and
	// outliving the parent (recording its pid so the test reaps it), then (2)
	// hangs the parent so the per-command timeout SIGKILLs it. The podman argv
	// ($@) is ignored on purpose. The 30s orphan sleep exceeds WaitDelay + the
	// safety deadline, so a WaitDelay-less hang is caught, not tolerated.
	stub := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\nexec sleep 30\n"
	prog := filepath.Join(dir, "podman-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	// Reap the orphaned pipe-holder by its recorded pid — never a pattern kill.
	// It closes the stdout pipe, so even a regressed (WaitDelay-less) call left
	// blocked in the goroutine below unwedges and the goroutine exits.
	t.Cleanup(func() { killRecordedPid(t, pidFile) })

	cli := NewPodmanCLI().WithProgram(prog).WithTimeout(200 * time.Millisecond)
	ctx := t.Context()

	// Buffered so a late send from a regressed (still-hung) call never leaks the
	// goroutine once the test has already failed on the safety deadline.
	done := make(chan error, 1)
	go func() {
		_, err := cli.Exec(ctx, ContainerID("c"), NewExecSpec("true"))
		done <- err
	}()

	// safety sits above WaitDelay (~10s, so a healthy call has returned) and below
	// the stub's 30s orphan sleep (so a WaitDelay-less hang trips it).
	const safety = 20 * time.Second
	select {
	case <-time.After(safety):
		t.Fatalf("Exec did not return within %s — spawnCapture blocked on the leaked stdout pipe past its timeout; cmd.WaitDelay is not set", safety)
	case err := <-done:
		var timeout *TimeoutError
		if !errors.As(err, &timeout) {
			t.Fatalf("Exec error = %v (%T), want *TimeoutError", err, err)
		}
	}
}

// killRecordedPid reaps the process whose pid the stub wrote to pidFile — an
// explicit, single-pid kill (never a pattern match) of a process this test
// spawned, so no orphan outlives the test. A missing file or already-exited pid
// is fine: the process may have exited on its own.
func killRecordedPid(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return // stub never got far enough to fork, or file absent — nothing to reap.
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}

// TestChildHandleTerminateKillsAndReaps pins the abandon-path contract on the
// streaming exec: after ChildHandle.Terminate returns, the child process is both
// SIGKILLed AND reaped — no live agent, no zombie, no leaked watch goroutine.
// Terminate bundles cancel() (SIGKILL via the exec's cancellable context, wired
// by ExecStreaming as cmd.Cancel=Process.Kill) with cmd.Wait() (the reap). Go
// has no Drop, so a consumer that drops the handle without this leaks a zombie +
// goroutine until the Runner exits; Terminate is the safe default that prevents
// it.
//
// Behavioral (not structural): it drives a real subprocess through the public
// ExecStreaming → se.Process.Terminate() surface and asserts two OBSERVABLE
// facts — Terminate returns within a WaitDelay-bounded safety deadline, and the
// recorded child pid is gone (kill(pid,0)==ESRCH) once it returns — not that any
// field was assigned. WithProgram swaps podman for a shell stub that ignores the
// podman argv and sleeps far longer than the deadline, so a Terminate that fails
// to kill (child still alive) or fails to reap (child a zombie) is unmistakable.
func TestChildHandleTerminateKillsAndReaps(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// A shell stub that records its own pid then sleeps far past the safety
	// deadline. `exec sleep` makes the sleep the very process cmd spawned, so the
	// recorded pid is exactly the child ExecStreaming's SIGKILL must land on and
	// Wait must reap. The podman argv ($@) is ignored on purpose.
	stub := "#!/bin/sh\necho $$ > " + pidFile + "\nexec sleep 120\n"
	prog := filepath.Join(dir, "podman-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	// Backstop reap by recorded pid — never a pattern kill — so a regressed
	// Terminate that leaves the child alive can't outlive the test. A healthy run
	// has already killed+reaped it, making this a no-op (ESRCH, ignored).
	t.Cleanup(func() { killRecordedPid(t, pidFile) })

	cli := NewPodmanCLI().WithProgram(prog)
	ctx := t.Context()

	se, err := cli.ExecStreaming(ctx, ContainerID("c"), NewStreamingExecSpec("omp", "acp"))
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}

	// Wait for the stub to record the child pid before terminating, so the pid we
	// probe is the process Terminate actually acts on. Gate on the file appearing,
	// never a fixed sleep.
	pid := waitForRecordedPid(t, pidFile)

	// Terminate must return within a WaitDelay-bounded deadline. ExecStreaming
	// sets cmd.WaitDelay=10s, so a healthy Terminate returns effectively at once
	// (the child dies on SIGKILL); the deadline sits above WaitDelay and far
	// below the stub's 120s sleep, so a wedged Terminate — or one that never
	// signals and blocks in Wait until the stub exits on its own — trips it.
	const safety = 15 * time.Second
	done := make(chan error, 1) // buffered so a late send can't leak the goroutine after a failed deadline.
	go func() { done <- se.Process.Terminate() }()
	select {
	case <-time.After(safety):
		t.Fatalf("Terminate did not return within %s — it wedged instead of Kill+Wait", safety)
	case <-done:
		// A kill-signalled child yields a non-nil "signal: killed" error from
		// Wait; that's the expected result of Terminate, not a failure. The
		// contract under test is the process state after it returns.
	}

	// The contract: after Terminate returns the child is dead AND reaped. A live
	// process (Terminate never signalled) or a zombie (signalled but never
	// reaped) both keep kill(pid,0) returning nil; only a fully reaped process is
	// gone (ESRCH). ESRCH is the single condition that holds iff Terminate did
	// both its jobs.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("after Terminate, kill(%d, 0) = %v, want ESRCH — child not killed-and-reaped (still alive, or a zombie)", pid, err)
	}
}

// waitForRecordedPid blocks until the stub writes its pid to pidFile, then
// returns it. It gates on the file's appearance and a fully-written line — no
// fixed sleep — and fails fast under a bounded deadline so a stub that never
// spawns can't hang the test.
func waitForRecordedPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open(pidFile)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		line, err := bufio.NewReader(f).ReadString('\n')
		f.Close()
		if err != nil { // file exists but the pid line isn't fully written yet.
			time.Sleep(5 * time.Millisecond)
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("parsing recorded pid %q: %v", line, err)
		}
		return pid
	}
	t.Fatalf("stub never recorded its pid to %s within deadline", pidFile)
	return 0
}

func TestStopGraceSeconds(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want int64
	}{
		// The only real caller today: a whole-second grace passes through as-is.
		{"whole seconds pass through", 10 * time.Second, 10},
		// A sub-second positive grace must round UP to at least 1s. Truncating
		// toward zero would emit --time 0 — an immediate SIGKILL with no grace.
		{"sub-second rounds up to 1", 500 * time.Millisecond, 1},
		// Fractional seconds ceil, never truncate down to 1.
		{"fractional ceils not truncates", 1500 * time.Millisecond, 2},
		// Zero grace is a valid explicit immediate stop.
		{"zero stays zero", 0, 0},
		// A negative Duration clamps to 0; it must NOT reach podman as a negative
		// --time, which podman reads as an infinite (unbounded) wait.
		{"negative clamps to zero", -1 * time.Second, 0},
		// Any positive Duration, however tiny, rounds up to >=1.
		{"one nanosecond rounds up to 1", 1 * time.Nanosecond, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopGraceSeconds(tc.in); got != tc.want {
				t.Fatalf("stopGraceSeconds(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
