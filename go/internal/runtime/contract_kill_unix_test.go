//go:build unix

package runtime

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

// A podman-shaped teardown that dies by a signal other than SIGKILL is not a
// deliberate kill in production (isDeliberateKill), so the contract row must
// not accept it either.
func TestDeliberateKillPodmanBranchRequiresSIGKILL(t *testing.T) {
	signalled := func(sig syscall.Signal) error {
		cmd := exec.CommandContext(context.Background(), "sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Process.Signal(sig); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		return cmd.Wait()
	}
	caps := backendCaps{name: "podman"}
	if !deliberateKill(signalled(syscall.SIGKILL), caps) {
		t.Fatal("SIGKILL death not classified as a deliberate kill")
	}
	if deliberateKill(signalled(syscall.SIGTERM), caps) {
		t.Fatal("SIGTERM death classified as a deliberate kill; production isDeliberateKill requires SIGKILL")
	}
}

// killedBySIGKILL matches production isDeliberateKill's podman branch.
func killedBySIGKILL(e *exec.ExitError) bool {
	ws, ok := e.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}
