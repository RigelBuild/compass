package runtime

// clispawn.go is the one subprocess seam every CLI-driven WorkloadRuntime
// backend spawns through. PodmanCLI and AppleContainerCLI differ only in which
// binary they invoke and which argv they assemble; the process handling around
// that — timeout, stdin, exit-code mapping, streaming pipes, kill-on-abandon —
// is identical, so it lives here once and each backend embeds cliEngine.
//
// Embedded (not a named field) so the backends keep referring to program and
// timeout directly and the seam methods are promoted onto them unchanged.

import (
	"bytes"
	"context"
	"errors"
	"os/exec" //nolint:depguard // CLI-engine seam: *exec.Cmd/*exec.ExitError types for the container engine subprocess
	"strings"
	"time"
)

// cliEngine is the shared subprocess seam: a container-engine binary plus the
// per-command wall-clock cap its one-shot invocations run under.
type cliEngine struct {
	program string
	timeout time.Duration
}

// spawnCapture spawns `<program> <args>`, optionally writing stdin, and
// captures output under the command timeout. A spawn failure, a timeout, and a
// captured non-zero exit are all mapped here. summary names the operation for
// error context without leaking the full argv (which may hold env values or a
// token on stdin).
//
// A non-zero exit is returned as (stdout, stderr, code, nil) — the caller
// decides whether that is an error (run) or an expected result (Exec, Exists).
// Only a spawn failure, this call's own timeout, or parent-context cancellation
// is a non-nil error.
func (e cliEngine) spawnCapture(ctx context.Context, summary string, args []string, stdin *string) (stdout, stderr []byte, exitCode int, err error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	//nolint:gosec // G204: this is the container-engine seam — spawning the
	// configured engine binary (e.program) with caller-supplied argv is the
	// module's entire purpose. Host/allowlist inputs are validated upstream
	// (isValidHost) before reaching an argv, and the program is operator-set,
	// not attacker-controlled.
	cmd := exec.CommandContext(cctx, e.program, args...)
	// A killed process that leaked a child still holding the output pipe would
	// keep Run blocked on that pipe indefinitely; WaitDelay bounds that wait so a
	// leaked-pipe hang can't outlive this call's timeout by more than WaitDelay.
	cmd.WaitDelay = 10 * time.Second
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if stdin != nil {
		// A strings.Reader hits EOF when the script is exhausted, so the child
		// never blocks waiting for more input.
		cmd.Stdin = strings.NewReader(*stdin)
	}

	if runErr := cmd.Run(); runErr != nil {
		switch {
		case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
			// This call's own timeout fired (not the parent): the process was
			// killed, so surface a timeout rather than a bogus exit code.
			return nil, nil, 0, &TimeoutError{Summary: summary, Timeout: e.timeout}
		case ctx.Err() != nil:
			// The caller cancelled: propagate the context error.
			return nil, nil, 0, ctx.Err()
		default:
			if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
				// Ran to completion but exited non-zero: not an error here.
				return out.Bytes(), errBuf.Bytes(), exitErr.ExitCode(), nil
			}
			return nil, nil, 0, &SpawnError{Program: e.program, Err: runErr}
		}
	}
	return out.Bytes(), errBuf.Bytes(), 0, nil
}

// run runs `<program> <args>`, requiring a zero exit (a non-zero becomes a
// CommandError). For fire-and-check operations like create/start/stop/remove.
func (e cliEngine) run(ctx context.Context, summary string, args []string) ([]byte, error) {
	stdout, stderr, exitCode, err := e.spawnCapture(ctx, summary, args, nil)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, &CommandError{
			Summary:  summary,
			ExitCode: exitCode,
			Stderr:   strings.TrimSpace(string(stderr)),
		}
	}
	return stdout, nil
}

// spawnStreaming starts `<program> <args>` streaming, returning the live pipes
// plus a kill/wait handle. The process is bound to a cancellable child of ctx:
// its Cancel SIGKILLs the process and WaitDelay bounds the reap, so cancelling
// the parent context or calling ChildHandle.Kill terminates the in-container
// agent even without a Go Drop.
func (e cliEngine) spawnStreaming(ctx context.Context, args []string) (*StreamingExec, error) {
	execCtx, cancel := context.WithCancel(ctx)
	//nolint:gosec // G204: the container-engine seam — see spawnCapture. The
	// engine binary is operator-set and the exec argv is Runner-assembled.
	cmd := exec.CommandContext(execCtx, e.program, args...)
	// A dropped session must kill the exec, or the in-container agent keeps
	// running after the Runner lets go of the handle. No command timeout: a
	// streaming session is long-lived by design.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 10 * time.Second

	spawnErr := func(err error) (*StreamingExec, error) {
		cancel()
		return nil, &SpawnError{Program: e.program, Err: err}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return spawnErr(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return spawnErr(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return spawnErr(err)
	}
	if err := cmd.Start(); err != nil {
		return spawnErr(err)
	}

	return &StreamingExec{
		IO:      StreamingIO{Stdin: stdin, Stdout: stdout, Stderr: stderr},
		Process: &ChildHandle{cmd: cmd, cancel: cancel},
	}, nil
}
