package runtime

// The one subprocess seam every CLI-driven WorkloadRuntime backend spawns
// through. PodmanCLI and AppleContainerCLI differ only in binary and argv; the
// process handling — timeout, stdin, exit-code mapping, streaming pipes,
// kill-on-abandon — is identical, so each backend embeds cliEngine.

import (
	"bytes"
	"context"
	"errors"
	"os"
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

// stopWithClientScript runs "$@" with stdin relayed by a watcher in the same
// process group. Killing the engine client closes that stdin, and the watcher
// then SIGKILLs the group; a natural exit kills the watcher and keeps the status.
// The shell notices for those kills go to /dev/null; "$@" keeps the real stderr.
const stopWithClientScript = `exec 3>&2; { { sh -c 'echo "$$"; exec cat' && kill -s KILL 0; } | { read -r w; "$@" 2>&3 3>&-; s=$?; kill -s KILL "$w"; exit "$s"; }; } 2>/dev/null` //nolint:gosec // G101: a shell script, not a credential

// sessionSweepScript kills detached exec sessions while preserving PID 1's
// session, which carries the container keep-alive needed for in-place reloads.
// It waits between rounds: a killed process stays visible until the kernel reaps it.
const sessionSweepScript = `IFS=' '
sid_of() {
	stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
	# Fields after the last ") " are kernel-written; comm before it may hold anything.
	set -- ${stat##*) }
	state=$1
	sid=$4
	[ -n "$sid" ]
}
sid_of 1 || { echo "cannot read PID 1 session" >&2; exit 1; }
keep=$sid
sid_of $$ || { echo "cannot read sweep session" >&2; exit 1; }
own=$sid
round=0
while [ "$round" -lt 100 ]; do
	round=$((round + 1))
	live=0
	for d in /proc/[0-9]*; do
		pid=${d#/proc/}
		sid_of "$pid" || continue
		[ "$sid" = "$keep" ] || [ "$sid" = "$own" ] || [ "$state" = Z ] && continue
		# A landed kill on a not-yet-reaped process counts until it is gone or a zombie;
		# another uid's process (EPERM) is outside this sweep.
		if kill -s KILL "$pid" 2>/dev/null; then
			live=$((live + 1))
		fi
	done
	[ "$live" -eq 0 ] && exit 0
	sleep 0.1
done
echo "processes remain outside PID 1 session after 10s" >&2
exit 1`

// stopWithClient wraps command so killing the engine client also kills its
// in-container process group, which the engine leaves running on its own.
// A descendant that called setsid escapes the group kill.
func stopWithClient(command []string) []string {
	return append([]string{"sh", "-c", stopWithClientScript, "sh"}, command...)
}

// spawnStreaming starts `<program> <args>` streaming, returning the live pipes
// plus a kill/wait handle. The process is bound to a cancellable child of ctx:
// its Cancel SIGKILLs the engine client and WaitDelay bounds the reap. The
// in-container process dies with the client only because exec argv builders
// wrap the command in stopWithClient. stdout/stderr are caller-owned os.Pipes,
// so Wait reaps at exit without closing them under output still in the pipe.
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
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return spawnErr(errors.Join(err, stdin.Close()))
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return spawnErr(errors.Join(err, stdin.Close(), closePipes(stdoutR, stdoutW)))
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		// A failed Start closes the exec-owned stdin pipe but never caller files.
		return spawnErr(errors.Join(err, closePipes(stdoutR, stdoutW, stderrR, stderrW)))
	}
	// The child holds its own copies; closing ours lets the reader see EOF once
	// the child and every descendant holding the pipe have exited.
	if err := closePipes(stdoutW, stderrW); err != nil {
		cancel()
		return nil, errors.Join(&SpawnError{Program: e.program, Err: err}, cmd.Wait(), closePipes(stdoutR, stderrR))
	}

	return &StreamingExec{
		IO:      StreamingIO{Stdin: stdin, Stdout: stdoutR, Stderr: stderrR},
		Process: &ChildHandle{cmd: cmd, cancel: cancel},
	}, nil
}
