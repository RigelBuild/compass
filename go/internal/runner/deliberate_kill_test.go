//go:build unix

package runner

// isDeliberateKill's widened taxonomy (U3b/OQ-G): it accepts both a real
// *exec.ExitError from a SIGKILLed local child (the podman byte-path, unchanged)
// and the portable *runtime.ExitStatusError a remote (microVM) waitFunc
// constructs, and rejects a non-signal exit and an unrelated error. Hermetic:
// the podman-path case SIGKILLs a real short-lived child, the rest are
// constructed errors; no KVM, no backend.

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// sigkilledExitError runs a trivial child and SIGKILLs it, returning the
// *exec.ExitError its Wait yields — the exact shape the podman ChildHandle.Wait
// produces on a deliberate teardown, so the regression guard exercises a real
// wait status rather than a hand-built one.
func sigkilledExitError(t *testing.T) error {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("sigkilling child: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected a non-nil wait error for a SIGKILLed child")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait error is %T, want *exec.ExitError", err)
	}
	return err
}

func TestIsDeliberateKill(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "podman-path *exec.ExitError SIGKILL",
			err:  sigkilledExitError(t),
			want: true,
		},
		{
			name: "portable ExitStatusError SIGKILL",
			err:  &runtime.ExitStatusError{Signal: syscall.SIGKILL},
			want: true,
		},
		{
			name: "portable ExitStatusError SIGTERM is still a deliberate signal",
			err:  &runtime.ExitStatusError{Signal: syscall.SIGTERM},
			want: true,
		},
		{
			name: "portable ExitStatusError non-signal exit is not a kill",
			err:  &runtime.ExitStatusError{Code: 1},
			want: false,
		},
		{
			name: "unrelated error",
			err:  errors.New("connection reset"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDeliberateKill(tt.err); got != tt.want {
				t.Fatalf("isDeliberateKill(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
