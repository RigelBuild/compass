//go:build unix

package adapters

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// ProcessSupervisor is the real stack.ProcessSupervisor: it launches supervised
// children via os/exec, resolving each Component to its deployment binary on
// PATH. Secrets ride in spec.Env (never Args), so they never reach the process
// table, and every child is placed in its own process group so a stop can target
// the whole group and no orphan escapes.
type ProcessSupervisor struct{}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.ProcessSupervisor = (*ProcessSupervisor)(nil)

// NewProcessSupervisor builds a ProcessSupervisor.
func NewProcessSupervisor() *ProcessSupervisor {
	return &ProcessSupervisor{}
}

// componentBinary resolves a Component to the binary name to look up on PATH.
// The binary location is a deployment concern, so only the name is fixed here;
// exec.LookPath finds it. An unknown component is an error, not a guess.
func componentBinary(c stack.Component) (string, error) {
	switch c {
	case stack.ComponentPostgres:
		return "compass-postgres", nil
	case stack.ComponentServer:
		return "compass-server", nil
	case stack.ComponentRunner:
		return "compass-runner", nil
	default:
		return "", fmt.Errorf("unknown component %d (%s)", int(c), c)
	}
}

// Start resolves spec.Component to a binary on PATH and launches it with
// spec.Args as argv[1:]. spec.Env is appended to the parent environment so the
// child inherits it plus the supplied secrets, which stay out of the arg vector.
// Stdout/stderr are wired to the parent's shared streams. The child is placed in
// its own process group (Setpgid) so Signal can target the group.
func (s *ProcessSupervisor) Start(_ context.Context, spec stack.ProcessSpec) (stack.Process, error) {
	binary, err := componentBinary(spec.Component)
	if err != nil {
		return nil, err
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("resolving %s binary %q on PATH: %w", spec.Component, binary, err)
	}

	cmd := exec.Command(path, spec.Args...) //nolint:gosec // G204: the process-supervisor seam — path is a LookPath-resolved deployment binary and Args are Stack-built, neither user-controlled
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group: Signal/escalation target the group (negative PID), so a
	// child that forks its own workers takes them down too — no orphan escapes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s (%s): %w", spec.Component, path, err)
	}

	return &process{cmd: cmd}, nil
}

// process is a handle to a started child. It holds the *exec.Cmd; the child's
// PID doubles as its process-group ID (set via Setpgid at Start). stopped records
// that a graceful stop was requested via Signal(SignalTerm), so Wait can tell an
// exit we asked for (a drain) from an unsolicited crash.
type process struct {
	cmd *exec.Cmd
	// stopped is set once Signal(SignalTerm) succeeds. The core calls Signal then
	// Wait sequentially on one goroutine (drainChildren: Signal then Wait per
	// child), so correctness needs no synchronization — but it is an atomic.Bool
	// as cheap insurance so a future caller that overlaps Signal with Wait cannot
	// data-race, and -race stays clean regardless of call ordering. The waiter
	// goroutine Wait spawns touches only the done channel, never this field.
	stopped atomic.Bool
}

// Compile-time proof the handle satisfies the core seam.
var _ stack.Process = (*process)(nil)

// Signal requests a stop of the given disposition. SignalTerm sends SIGTERM to
// the child for a graceful exit; any other ProcessSignal is an error rather than
// a silent no-op.
func (p *process) Signal(sig stack.ProcessSignal) error {
	switch sig {
	case stack.SignalTerm:
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("sending SIGTERM to pid %d: %w", p.cmd.Process.Pid, err)
		}
		// Record that we asked this child to stop, so Wait treats its subsequent
		// exit as the drain we requested rather than a crash.
		p.stopped.Store(true)
		return nil
	default:
		return fmt.Errorf("unknown process signal %d", int(sig))
	}
}

// Pid reports the child's PID, which doubles as its process-group ID (set via
// Setpgid at Start). The supervisor persists it as the pgid a cross-process
// down signals; the negative form is the group target.
func (p *process) Pid() int {
	return p.cmd.Process.Pid
}

// Wait blocks until the child exits or ctx is done. On a normal exit: if a
// graceful stop was requested first (Signal(SignalTerm) succeeded), any exit —
// a clean one, a SIGTERM-signaled death, OR a nonzero exit code — is the drain
// we asked for and returns nil; without a prior stop, the child's exit error is
// returned unchanged, so a genuine crash or a startup failure still surfaces. On
// ctx cancellation it escalates to a hard kill of the whole process group
// (SIGKILL to -pgid), reaps the child to avoid a zombie, and returns the ctx
// error wrapped — a ctx-cancel is an abort/timeout, a different path from a
// graceful drain, so it is never normalized. The waiter goroutine always
// completes — it is never leaked — because the escalated SIGKILL forces
// cmd.Wait() to return.
func (p *process) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case err := <-done:
		// A stop we initiated: during drain, drainChildren only ever Waits after
		// it Signaled, so any exit here IS the requested stop — a crash-vs-clean
		// distinction while tearing down has no actionable difference. This is
		// deliberately broader than compass-postgres's childExitError (which
		// normalizes only a SIGTERM-signaled death): the embedded runner exits
		// nonzero on graceful shutdown by library contract (RunSessions returns
		// canceled:EOF, tolerated at server/lifecycle_e2e_pgtest_test.go:806), so
		// a nonzero code after our SIGTERM must read as a clean drain too.
		if err != nil && p.stopped.Load() {
			return nil
		}
		return err
	case <-ctx.Done():
		// If the child already exited and was reaped in the tiny window before we
		// observed cancellation, its pid may be recycled — a group SIGKILL to
		// -pid would then target an unrelated process group. Only escalate the
		// kill when the child has not already returned.
		select {
		case <-done:
		default:
			// Hard-kill the whole group: negative PID targets the process group set
			// up via Setpgid, so forked workers die too. A kill error is not
			// actionable (the child may have exited after our check); the reap below
			// guarantees no zombie regardless, so it is deliberately discarded.
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			// Reap the child so it is not left a zombie; the SIGKILL guarantees the
			// waiter returns promptly.
			<-done
		}
		return fmt.Errorf("waiting for %s: %w", p.cmd.Path, ctx.Err())
	}
}
