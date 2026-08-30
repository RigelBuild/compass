package runtime

import (
	"fmt"
	"syscall"
)

// ExitStatusError is a backend-portable process-exit error the runner's
// isDeliberateKill recognizes, so a remote (microVM) guest exit can be told
// from a crash without fabricating an *exec.ExitError.
//
// The podman backend reports a deliberate SIGKILL teardown as an
// *exec.ExitError whose syscall.WaitStatus is Signaled()+SIGKILL
// (agent_exec.go isDeliberateKill). A remote exec (the microVM GuestExec
// ChildHandle waitFunc) cannot construct an *exec.ExitError: it embeds
// *os.ProcessState, which has unexported fields and no public constructor, so
// a waitFunc reporting a guest child's exit has no way to forge one. This
// exported concrete type is the portable stand-in — a plain (code, signal)
// pair the microVM waitFunc returns and isDeliberateKill matches with
// errors.As, alongside the existing *exec.ExitError branch so the podman
// byte-path is unchanged (OQ-G, design §(e)). It is a concrete struct rather
// than an interface: it is the simplest errors.As target and no caller needs
// the abstraction today.
type ExitStatusError struct {
	// Code is the child's exit code, meaningful when Signal == 0.
	Code int
	// Signal is the terminating signal, non-zero when the child died by signal
	// (e.g. syscall.SIGKILL on a deliberate Kill).
	Signal syscall.Signal
}

// Error describes the exit as either a signal death or a non-zero exit code.
func (e *ExitStatusError) Error() string {
	if e.Signal != 0 {
		return fmt.Sprintf("process terminated by signal %d (%s)", int(e.Signal), e.Signal)
	}
	return fmt.Sprintf("process exited with code %d", e.Code)
}
