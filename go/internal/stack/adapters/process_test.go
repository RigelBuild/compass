//go:build unix

package adapters

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// The re-exec-self pattern: the test binary doubles as the child process. When
// STACK_TEST_HELPER is set, TestMain routes into helperProcess instead of the
// normal test run, so a started child is this same binary behaving as a stub —
// no external binaries, fully deterministic.
const (
	helperEnvVar   = "STACK_TEST_HELPER"
	helperEchoKey  = "STACK_TEST_ECHO"
	helperReadyKey = "STACK_TEST_READY"
	// helperEchoOutKey names a file the trapecho helper writes the OBSERVED
	// helperEchoKey value into on SIGTERM, so a test proves env-threading by
	// positive observation (read the file) rather than by exit code — the drain
	// normalization in Wait now folds any post-SIGTERM exit code to nil, so an
	// exit-code-keyed negative control would silently pass.
	helperEchoOutKey = "STACK_TEST_ECHO_OUT"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnvVar) {
	case "":
		os.Exit(m.Run())
	case "exit0":
		os.Exit(0)
	case "trap":
		// Arm, signal readiness, block until SIGTERM, then exit 0 — a plain
		// graceful-stop stand-in (self-converts SIGTERM to a clean exit, like
		// compass-server/postgres).
		helperTrap()
		os.Exit(0)
	case "trapecho":
		// Graceful-stop stand-in that PROVES env-threading by positive
		// observation: on SIGTERM it writes the value it observed for
		// helperEchoKey to the helperEchoOutKey file, then exits 0. The test
		// reads that file (env-present → "expected", env-absent → empty) rather
		// than keying on the exit code, which Wait now folds to nil after our
		// SIGTERM.
		helperTrap()
		if out := os.Getenv(helperEchoOutKey); out != "" {
			if err := os.WriteFile(out, []byte(os.Getenv(helperEchoKey)), 0o600); err != nil {
				os.Exit(4)
			}
		}
		os.Exit(0)
	case "trapexit1":
		// Drain mode 1: traps SIGTERM but exits NONZERO (mirrors the embedded
		// runner, whose RunSessions returns canceled:EOF on graceful shutdown so
		// the process exits 1). Wait must fold this to nil after our SIGTERM.
		helperTrap()
		os.Exit(1)
	case "notrap":
		// Drain mode 2: signals readiness but does NOT trap SIGTERM, so the
		// default disposition kills it ("signal: terminated") — the exec→handler
		// arm window the runner also has. Wait must fold this to nil after our
		// SIGTERM.
		helperReadyNoTrap()
		select {} // block forever; SIGTERM terminates by default disposition
	case "exitnonzero":
		// Guard: signals readiness then exits NONZERO on its own — the test never
		// Signals it, so Wait must return the exit error unchanged (the drain
		// normalization is post-Signal-only, never a blanket swallow).
		helperReadyNoTrap()
		os.Exit(5)
	case "sleep":
		// Park until killed; a normal SIGTERM (test never sends one here) also
		// exits clean.
		helperTrap()
		os.Exit(0)
	default:
		os.Exit(99)
	}
}

// helperTrap arms the SIGTERM handler, signals readiness by writing the ready
// file (so the parent can gate its signal on the handler being installed — the
// real synchronization point, not a timing guess), then blocks until SIGTERM.
func helperTrap() {
	ch := make(chan os.Signal, 1)
	signalNotifyTerm(ch)
	writeReady()
	<-ch
}

// helperReadyNoTrap signals readiness WITHOUT installing a SIGTERM handler, so
// the child takes SIGTERM's default (terminate) disposition. The ready write is
// the parent's start gate exactly as in helperTrap.
func helperReadyNoTrap() {
	writeReady()
}

// writeReady marks the ready file if the parent supplied one. A write failure
// would strand the parent's gate, so surface it by exiting.
func writeReady() {
	if ready := os.Getenv(helperReadyKey); ready != "" {
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			os.Exit(4)
		}
	}
}

// startHelper starts the test binary as a child in the given helper mode, on a
// PATH where the requested component's binary name resolves to a wrapper that
// re-execs this test binary with the sentinel env set.
func startHelper(t *testing.T, mode string, component stack.Component, extraEnv []string) stack.Process {
	t.Helper()
	dir := t.TempDir()
	binary := mustComponentBinary(t, component)
	writeReexecWrapper(t, dir, binary, mode)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	readyPath := filepath.Join(t.TempDir(), "ready")
	env := append([]string{helperReadyKey + "=" + readyPath}, extraEnv...)

	sup := NewProcessSupervisor()
	proc, err := sup.Start(context.Background(), stack.ProcessSpec{
		Component: component,
		Env:       env,
	})
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	// Gate on the child arming its SIGTERM handler — a bounded poll on the real
	// readiness signal (the file the child writes after signal.Notify), not a
	// blind sleep. Without this the parent could signal before the handler is
	// installed and the child would take the default-terminate disposition.
	waitReady(t, readyPath, proc)
	return proc
}

// waitReady polls for the child's ready file with a bounded deadline, failing
// loudly on timeout. Gated on the event (file present), not the clock.
func waitReady(t *testing.T, readyPath string, proc stack.Process) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_ = proc.Signal(stack.SignalTerm)
	t.Fatalf("child never armed (ready file %s absent within deadline)", readyPath)
}

// writeReexecWrapper writes an executable shell wrapper named binary into dir
// that re-execs the test binary with STACK_TEST_HELPER=mode, forwarding the
// child's env (so spec.Env-threaded values reach the re-exec'd helper).
func writeReexecWrapper(t *testing.T, dir, binary, mode string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable = %v", err)
	}
	script := "#!/bin/sh\nexec env " + helperEnvVar + "=" + mode + " " + self + " \"$@\"\n"
	path := filepath.Join(dir, binary)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) = %v", path, err)
	}
}

func mustComponentBinary(t *testing.T, c stack.Component) string {
	t.Helper()
	name, err := componentBinary(c)
	if err != nil {
		t.Fatalf("componentBinary(%v) = %v", c, err)
	}
	return name
}

func TestComponentBinaryResolution(t *testing.T) {
	cases := []struct {
		name      string
		component stack.Component
		want      string
		wantErr   bool
	}{
		{"postgres", stack.ComponentPostgres, "compass-postgres", false},
		{"server", stack.ComponentServer, "compass-server", false},
		{"runner", stack.ComponentRunner, "compass-runner", false},
		{"unknown", stack.Component(99), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := componentBinary(tc.component)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("componentBinary(%v) err = nil, want error", tc.component)
				}
				return
			}
			if err != nil {
				t.Fatalf("componentBinary(%v) = %v", tc.component, err)
			}
			if got != tc.want {
				t.Fatalf("componentBinary(%v) = %q, want %q", tc.component, got, tc.want)
			}
		})
	}
}

func TestStartUnknownComponent(t *testing.T) {
	sup := NewProcessSupervisor()
	if _, err := sup.Start(context.Background(), stack.ProcessSpec{Component: stack.Component(99)}); err == nil {
		t.Fatal("Start with unknown component err = nil, want error")
	}
}

func TestStartBinaryNotOnPath(t *testing.T) {
	// A PATH with no compass-server on it: LookPath fails and the error must
	// name the component and the binary.
	t.Setenv("PATH", t.TempDir())
	sup := NewProcessSupervisor()
	_, err := sup.Start(context.Background(), stack.ProcessSpec{Component: stack.ComponentServer})
	if err == nil {
		t.Fatal("Start with binary absent from PATH err = nil, want error")
	}
	if !strings.Contains(err.Error(), "compass-server") {
		t.Fatalf("error %q does not name the binary/component", err)
	}
}

func TestLifecycleGracefulStopThreadsEnv(t *testing.T) {
	// Positive observation: the trapecho child writes the helperEchoKey value it
	// OBSERVED to a file on SIGTERM. A nil Wait proves graceful stop; the file
	// holding "expected" proves spec.Env was threaded into the child. (Exit code
	// can't carry this proof anymore — Wait folds any post-SIGTERM exit to nil.)
	echoOut := filepath.Join(t.TempDir(), "echo")
	proc := startHelper(t, "trapecho", stack.ComponentServer,
		[]string{helperEchoKey + "=expected", helperEchoOutKey + "=" + echoOut})
	if err := proc.Signal(stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SignalTerm) = %v", err)
	}
	if err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after SIGTERM = %v, want nil (graceful drain)", err)
	}
	got, err := os.ReadFile(echoOut)
	if err != nil {
		t.Fatalf("reading observed-env file: %v", err)
	}
	if string(got) != "expected" {
		t.Fatalf("child observed STACK_TEST_ECHO=%q, want %q (env not threaded)", got, "expected")
	}
}

func TestLifecycleEnvNotThreadedIsObservablyEmpty(t *testing.T) {
	// Negative control, positive-observation form: without the env entry the
	// child observes an empty value and writes an empty file — proving the
	// assertion above depends on spec.Env. (Keyed on the file, not the exit
	// code, which Wait now normalizes to nil after our SIGTERM.)
	echoOut := filepath.Join(t.TempDir(), "echo")
	proc := startHelper(t, "trapecho", stack.ComponentServer,
		[]string{helperEchoOutKey + "=" + echoOut})
	if err := proc.Signal(stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SignalTerm) = %v", err)
	}
	if err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after SIGTERM = %v, want nil (graceful drain)", err)
	}
	got, err := os.ReadFile(echoOut)
	if err != nil {
		t.Fatalf("reading observed-env file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("child observed STACK_TEST_ECHO=%q, want empty (no env was threaded)", got)
	}
}

// TestWaitNormalizesRawSignalTermDeath is drain mode 2: a child that does NOT
// trap SIGTERM dies by the default disposition ("signal: terminated"). After our
// Signal(SignalTerm), Wait must fold that death to nil — the exec→handler-arm
// window the embedded runner also has must not make a clean drain look failed.
func TestWaitNormalizesRawSignalTermDeath(t *testing.T) {
	proc := startHelper(t, "notrap", stack.ComponentRunner, nil)
	if err := proc.Signal(stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SignalTerm) = %v", err)
	}
	if err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after SIGTERM = %v, want nil (raw signal:terminated is our drain)", err)
	}
}

// TestWaitNormalizesNonzeroExitAfterSignal is drain mode 1: a child that traps
// SIGTERM and exits NONZERO (as the embedded runner does — RunSessions returns
// canceled:EOF, so the process exits 1). After our Signal, Wait must fold the
// nonzero exit to nil. This is the case a literal childExitError mirror would
// miss (that only normalizes a signaled death, not an exit code).
func TestWaitNormalizesNonzeroExitAfterSignal(t *testing.T) {
	proc := startHelper(t, "trapexit1", stack.ComponentRunner, nil)
	if err := proc.Signal(stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SignalTerm) = %v", err)
	}
	if err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after SIGTERM = %v, want nil (nonzero exit after our SIGTERM is a clean drain)", err)
	}
}

// TestWaitSurfacesNonzeroExitWithoutSignal is the narrowing guard: a child that
// exits NONZERO on its own, with NO Signal from us, must surface its exit error
// unchanged. This proves the drain normalization is strictly post-Signal — it
// never becomes a blanket "ignore all exit errors" that would swallow a genuine
// startup failure or crash of a child we did not stop.
func TestWaitSurfacesNonzeroExitWithoutSignal(t *testing.T) {
	proc := startHelper(t, "exitnonzero", stack.ComponentRunner, nil)
	if err := proc.Wait(context.Background()); err == nil {
		t.Fatal("Wait = nil, want non-nil (an unsolicited nonzero exit must surface — normalization is post-Signal only)")
	}
}

func TestWaitHonorsContext(t *testing.T) {
	proc := startHelper(t, "sleep", stack.ComponentServer, nil)
	p, ok := proc.(*process)
	if !ok {
		t.Fatalf("Start returned %T, want *process", proc)
	}
	pid := p.cmd.Process.Pid

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := proc.Wait(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want context.DeadlineExceeded wrapped", err)
	}

	// The child must actually be gone (reaped, not a zombie). After cmd.Wait has
	// returned, signalling the reaped process errors — the deterministic proof
	// it was reaped rather than left running.
	if killErr := syscall.Kill(pid, syscall.Signal(0)); killErr == nil {
		t.Fatalf("child pid %d still signalable after Wait ctx-cancel; expected reaped", pid)
	}
}

func TestUnknownSignal(t *testing.T) {
	proc := startHelper(t, "trap", stack.ComponentServer, []string{helperEchoKey + "=expected"})
	// Clean up the child so the test does not leak it.
	t.Cleanup(func() {
		_ = proc.Signal(stack.SignalTerm)
		_ = proc.Wait(context.Background())
	})
	if err := proc.Signal(stack.ProcessSignal(99)); err == nil {
		t.Fatal("Signal with unknown disposition err = nil, want error")
	}
}

// signalNotifyTerm registers ch for SIGTERM. Kept as a tiny helper so TestMain's
// helper arms read cleanly.
func signalNotifyTerm(ch chan os.Signal) {
	signal.Notify(ch, syscall.SIGTERM)
}
