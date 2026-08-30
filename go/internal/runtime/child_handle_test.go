package runtime

// newChildHandleFuncs (U3): the funcs-backed ChildHandle the microVM GuestExec
// adaptation consumes (U4 wires the real kill/wait pair onto a GuestStream).
// These hermetic unit tests pin the exported Kill/Wait/Terminate surface over a
// kill/wait function pair — no *exec.Cmd, no backend — including that a
// signalled-exit waitFunc returning *ExitStatusError flows through Wait
// unchanged, so the runner's isDeliberateKill can recognize it.

import (
	"errors"
	"syscall"
	"testing"
)

func TestNewChildHandleFuncs_Kill(t *testing.T) {
	killed := false
	h := newChildHandleFuncs(
		func() error { killed = true; return nil },
		func() error { return nil },
	)
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killed {
		t.Fatal("Kill did not invoke killFunc")
	}
}

func TestNewChildHandleFuncs_KillError(t *testing.T) {
	want := errors.New("transport wedged")
	h := newChildHandleFuncs(
		func() error { return want },
		func() error { return nil },
	)
	if err := h.Kill(); !errors.Is(err, want) {
		t.Fatalf("Kill error = %v, want %v", err, want)
	}
}

func TestNewChildHandleFuncs_WaitExitZero(t *testing.T) {
	h := newChildHandleFuncs(
		func() error { return nil },
		func() error { return nil },
	)
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait on exit 0 = %v, want nil", err)
	}
}

func TestNewChildHandleFuncs_WaitSignalledExit(t *testing.T) {
	// A signalled guest exit: waitFunc returns the portable *ExitStatusError,
	// which Wait must surface unchanged so isDeliberateKill recognizes it.
	h := newChildHandleFuncs(
		func() error { return nil },
		func() error { return &ExitStatusError{Signal: syscall.SIGKILL} },
	)
	err := h.Wait()
	var exitStatus *ExitStatusError
	if !errors.As(err, &exitStatus) {
		t.Fatalf("Wait error = %T, want *ExitStatusError", err)
	}
	if exitStatus.Signal != syscall.SIGKILL {
		t.Fatalf("signal = %v, want SIGKILL", exitStatus.Signal)
	}
}

func TestNewChildHandleFuncs_TerminateReturnsWaitError(t *testing.T) {
	// Terminate is Kill then Wait; the wait error (the exit status) is what a
	// caller distinguishing crash-from-teardown needs, so it wins over the kill
	// error.
	killErr := errors.New("signal RPC timed out")
	h := newChildHandleFuncs(
		func() error { return killErr },
		func() error { return &ExitStatusError{Signal: syscall.SIGKILL} },
	)
	err := h.Terminate()
	var exitStatus *ExitStatusError
	if !errors.As(err, &exitStatus) {
		t.Fatalf("Terminate error = %T, want *ExitStatusError (the wait error)", err)
	}
}

func TestNewChildHandleFuncs_TerminateExitZero(t *testing.T) {
	// Clean exit with a kill error: Terminate returns the kill error, since the
	// wait error is nil.
	killErr := errors.New("signal RPC timed out")
	h := newChildHandleFuncs(
		func() error { return killErr },
		func() error { return nil },
	)
	if err := h.Terminate(); !errors.Is(err, killErr) {
		t.Fatalf("Terminate error = %v, want the kill error %v", err, killErr)
	}
}
