//go:build unix

package microvm

// exec.go is the host-side exec layer over the U1 GuestControl client: a
// GuestExec wrapping the GuestControl Connect client (dial.go GuestClient) with
// a one-shot Exec and a streaming ExecStream that turns the bidi frame protocol
// into live io.Pipe stdio plus a kill/wait handle (design §(c), record §Plan
// U3).
//
// Package boundary: this layer produces plain structs mirroring the proto
// (ExecCall/ExecResult/StreamCall/ExitStatus) rather than go/internal/runtime
// types. runtime's MicroVMRuntime (U4) consumes GuestExec, so runtime imports
// microvm; microvm importing runtime would cycle. The runtime-side adaptation
// (spec -> ExecCall, ExitStatus -> ExecOutput/*runtime.ExitStatusError, the
// newChildHandleFuncs kill/wait pair) is U4's, kept out of this package.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// sigKill is the signal reported for a deliberate teardown (a ctx-cancelled
// stream with no exit frame): guestd SIGKILLs the child bound to the broken
// stream, so the host reports SIGKILL, which the runtime waitFunc maps to the
// portable deliberate-kill error.
const sigKill = syscall.SIGKILL

// killSignalTimeout bounds the Signal RPC a Kill issues so a wedged transport
// cannot block the teardown path: podman's Kill is an instantaneous local
// cancel, so the microVM Kill must not stall on the wire. On timeout the error
// is returned to the caller, which ignores it (the VMM-kill escalation in Stop
// is the backstop) — Wait still returns via the demux goroutine, so a Kill RPC
// that never lands does not wedge teardown (design §(c)).
const killSignalTimeout = 5 * time.Second

// stdinChunk is the stdin pump's read buffer size; a larger chunk is more
// frames of the same bytes, so it affects only framing overhead, not
// correctness.
const stdinChunk = 32 * 1024

// TimeoutError is a one-shot exec that overran its per-command wall-clock cap
// and was aborted rather than left to block the caller. It mirrors the
// discipline of runtime.TimeoutError without leaking that type across the
// package boundary; MicroVMRuntime.Exec (U4) translates it to the runtime type.
type TimeoutError struct {
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("guest exec timed out after %ds", int(e.Timeout.Seconds()))
}

// ExecCall is a one-shot exec request, mirroring ExecRequest field-for-field so
// runtime types don't leak into this package.
type ExecCall struct {
	Command []string
	// UID is the exec user; nil uses the session default set by Provision. UID 0
	// is refused guest-side.
	UID *uint32
	// Workdir is the working directory; nil uses the child's default.
	Workdir *string
	// Env is merged over the session base env.
	Env map[string]string
	// Stdin is fed to the child's stdin over the wire — never the argv — so a
	// script body never appears in the guest process list.
	Stdin []byte
	// TimeoutSeconds bounds the command; 0 leaves it to the caller's ctx. It is
	// enforced host-side (a ctx deadline) and mirrored guest-side.
	TimeoutSeconds uint32
}

// ExecResult is a completed one-shot exec, mirroring ExecResponse. A non-zero
// ExitCode is a successful result, not an error.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// StreamCall is a streaming exec request, mirroring StartExec. Unlike ExecCall
// it carries no stdin body or timeout: a streaming exec keeps a live stdin pipe
// for its whole life and is long-lived by design.
type StreamCall struct {
	Command []string
	UID     *uint32
	Workdir *string
	Env     map[string]string
}

// ExitStatus is how a streaming exec ended: a non-zero Signal means the child
// died by signal (e.g. SIGKILL on a Kill), otherwise Code is the exit code.
type ExitStatus struct {
	Code   int
	Signal int
}

// bidiStream is the subset of *connect.BidiStreamForClient the exec pumps use.
// It exists so a hermetic test can inject a stream whose Receive blocks until
// CloseResponse — proving the ctx-cancel watcher forces the receive side down —
// since the concrete connect stream is unfakeable. *connect.BidiStreamForClient
// satisfies it.
type bidiStream interface {
	Send(req *compassv1.ExecStreamRequest) error
	Receive() (*compassv1.ExecStreamResponse, error)
	CloseRequest() error
	CloseResponse() error
}

// GuestExec is the host-side exec layer over a GuestControl client.
type GuestExec struct {
	client compassv1internalconnect.GuestControlClient
	// openStream opens the bidi ExecStream. Nil in production (openBidi falls
	// back to client.ExecStream); a hermetic test injects a fake stream through
	// it to drive the ctx-cancel reap deterministically.
	openStream func(ctx context.Context) bidiStream
}

// NewGuestExec wraps a GuestControl client (dial.go GuestClient) in the exec
// layer.
func NewGuestExec(client compassv1internalconnect.GuestControlClient) *GuestExec {
	return &GuestExec{client: client}
}

// Exec runs one command to completion over the Exec RPC. A non-zero exit is a
// successful ExecResult with a non-zero ExitCode, NEVER an error; a gate-closed
// or uid-0 refusal or a transport failure is an error. A per-command timeout
// (ExecCall.TimeoutSeconds) is enforced host-side as a ctx deadline and mapped
// to a *TimeoutError, mirroring runtime.PodmanCLI's TimeoutError discipline.
func (g *GuestExec) Exec(ctx context.Context, call ExecCall) (ExecResult, error) {
	var timeout time.Duration
	if call.TimeoutSeconds > 0 {
		timeout = time.Duration(call.TimeoutSeconds) * time.Second
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req := &compassv1.ExecRequest{
		Command:        call.Command,
		Uid:            call.UID,
		Workdir:        call.Workdir,
		Env:            call.Env,
		Stdin:          call.Stdin,
		TimeoutSeconds: call.TimeoutSeconds,
	}
	resp, err := g.client.Exec(ctx, connect.NewRequest(req))
	if err != nil {
		if timeout > 0 && isDeadline(err) {
			return ExecResult{}, &TimeoutError{Timeout: timeout}
		}
		return ExecResult{}, fmt.Errorf("guest exec: %w", err)
	}
	return ExecResult{
		Stdout:   resp.Msg.GetStdout(),
		Stderr:   resp.Msg.GetStderr(),
		ExitCode: int(resp.Msg.GetExitCode()),
	}, nil
}

// isDeadline reports whether err is a host-side deadline: a wrapped
// context.DeadlineExceeded or a Connect deadline-exceeded code.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		connect.CodeOf(err) == connect.CodeDeadlineExceeded
}

// ExecStream opens the bidi stream, sends the StartExec frame, and AWAITS the
// ExecStarted frame before returning — so a spawn failure surfaces as the
// returned error rather than a broken pipe on first read. It returns a
// GuestStream whose io.Pipe stdio is pumped by a per-exec goroutine: the Stdin
// pipe is framed as stdin frames (Close -> stdin_close), and stdout/stderr
// response frames are demuxed onto the read pipes, which close when the
// ExecExit frame arrives.
func (g *GuestExec) ExecStream(ctx context.Context, call StreamCall) (_ *GuestStream, retErr error) {
	stream := g.openBidi(ctx)
	// Until the pumps below take ownership of the stream, any early-error return
	// must reap both halves itself — the bidi stream is already open, so a bare
	// `return nil, err` leaks its response-body reader / makeRequest goroutine
	// for a failed spawn. The pumps own the reap once they start (return gs, nil
	// leaves retErr nil, so this is a no-op on the success path).
	defer func() {
		if retErr != nil {
			_ = stream.CloseRequest()
			_ = stream.CloseResponse()
		}
	}()
	start := &compassv1.ExecStreamRequest{
		Frame: &compassv1.ExecStreamRequest_Start{
			Start: &compassv1.StartExec{
				Command: call.Command,
				Uid:     call.UID,
				Workdir: call.Workdir,
				Env:     call.Env,
			},
		},
	}
	if err := stream.Send(start); err != nil {
		return nil, fmt.Errorf("guest exec stream: sending start: %w", err)
	}

	// Await the ExecStarted frame: only then is the spawn known to have
	// succeeded, so a spawn failure is the returned error, not a first-read
	// broken pipe.
	first, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("guest exec stream: awaiting started: %w", err)
	}
	started := first.GetStarted()
	if started == nil {
		return nil, fmt.Errorf("guest exec stream: first frame was not started: %T", first.GetFrame())
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	gs := &GuestStream{
		ctx:     ctx,
		client:  g.client,
		stream:  stream,
		execID:  started.GetExecId(),
		Stdin:   stdinW,
		Stdout:  stdoutR,
		Stderr:  stderrR,
		stdinR:  stdinR,
		stdoutW: stdoutW,
		stderrW: stderrW,
		done:    make(chan struct{}),
	}
	go gs.pumpStdin()
	go gs.pumpResponses()
	go gs.watchCancel()
	return gs, nil
}

// openBidi opens the bidi ExecStream, honoring an injected openStream (tests)
// and otherwise dialing the real GuestControl client.
func (g *GuestExec) openBidi(ctx context.Context) bidiStream {
	if g.openStream != nil {
		return g.openStream(ctx)
	}
	return g.client.ExecStream(ctx)
}

// GuestStream is a live streaming exec: its stdio pipes plus a kill/wait
// surface over the guest child. Stdin/Stdout/Stderr are the caller's ends of
// io.Pipes pumped by pumpStdin/pumpResponses.
type GuestStream struct {
	// ctx is the stream-lifetime context (not a per-request ctx): Kill's Signal
	// RPC deadline derives from it, and its cancellation is the deliberate
	// teardown pumpResponses maps to SIGKILL. The per-call ctx is the wrong
	// lifetime here.
	//nolint:containedctx // stream-lifetime scope for Kill's Signal RPC + teardown detection; a per-request ctx cannot carry it (see field doc)
	ctx    context.Context
	client compassv1internalconnect.GuestControlClient
	stream bidiStream
	execID string

	// Stdin/Stdout/Stderr are the caller's pipe ends.
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser

	// stdinR/stdoutW/stderrW are the pump's ends of the same pipes.
	stdinR  *io.PipeReader
	stdoutW *io.PipeWriter
	stderrW *io.PipeWriter

	// done is closed once pumpResponses observes the exit frame or a stream
	// break; status is valid to read only after done is closed.
	done   chan struct{}
	status ExitStatus
}

// Kill delivers sig to the guest child over the Signal RPC, bounded by
// killSignalTimeout so a wedged transport never blocks the caller past it. The
// exit frame that follows a SIGKILL arrives on the response stream and unblocks
// Wait; a Signal RPC that never lands is not fatal — the response pump still
// unblocks Wait on the eventual stream break, and the VMM-kill escalation is
// the backstop. The returned error is informational (the caller's teardown path
// ignores it).
func (s *GuestStream) Kill(sig int) error {
	ctx, cancel := context.WithTimeout(s.ctx, killSignalTimeout)
	defer cancel()
	_, err := s.client.Signal(ctx, connect.NewRequest(&compassv1.SignalRequest{
		ExecId: s.execID,
		Signal: int32(sig), //nolint:gosec // G115: sig is a small signal number (syscall.Signal), never overflows int32
	}))
	if err != nil {
		return fmt.Errorf("guest exec kill: signal %d: %w", sig, err)
	}
	return nil
}

// Wait blocks until the response pump observes the terminal exit frame (or a
// stream break) and returns how the exec ended. It never blocks past the
// child's exit: a Kill's exit frame, a clean EOF, or a broken stream all close
// done.
func (s *GuestStream) Wait() ExitStatus {
	<-s.done
	return s.status
}

// pumpStdin reads the caller's Stdin pipe and frames it as stdin frames; when
// the caller closes Stdin (pipe EOF) it sends a stdin_close half-close and
// closes the request direction of the stream. Kill rides the separate Signal
// RPC, not this stream, so closing the request side here never races a
// teardown.
func (s *GuestStream) pumpStdin() {
	buf := make([]byte, stdinChunk)
	for {
		n, readErr := s.stdinR.Read(buf)
		if n > 0 {
			frame := make([]byte, n)
			copy(frame, buf[:n])
			if sendErr := s.stream.Send(&compassv1.ExecStreamRequest{
				Frame: &compassv1.ExecStreamRequest_Stdin{Stdin: frame},
			}); sendErr != nil {
				// The stream is gone; the response pump observes the same break
				// and unblocks Wait. Nothing further to do on the stdin side.
				return
			}
		}
		if readErr != nil {
			// EOF is the caller closing Stdin (the common path); any other read
			// error means the pipe was closed with an error. Either way, signal
			// the guest to half-close the child's stdin, then close the request
			// direction.
			if sendErr := s.stream.Send(&compassv1.ExecStreamRequest{
				Frame: &compassv1.ExecStreamRequest_StdinClose{StdinClose: &compassv1.StdinClose{}},
			}); sendErr != nil {
				return
			}
			// Half-close the request side; the response side stays open to carry
			// stdout/stderr and the terminal exit frame. A CloseRequest error is
			// not actionable here — the response pump surfaces any real break.
			_ = s.stream.CloseRequest()
			return
		}
	}
}

// pumpResponses demuxes stdout/stderr frames onto the read pipes and, on the
// terminal exit frame (or a stream break), records the exit status, closes both
// read pipes, and closes done to unblock Wait. Writing to a pipe blocks until
// the caller reads, so a full pipe applies backpressure rather than dropping
// bytes — matching the runner's continuous-drain model.
func (s *GuestStream) pumpResponses() {
	defer close(s.done)
	// On any terminal exit, also reap the stdin pump and free the response
	// half: pumpStdin may be parked in stdinR.Read (a caller that never wrote
	// or closed Stdin), and the muxed response half must not leak per exec.
	// This defer runs before close(s.done), so by the time Wait returns the
	// pump's reader end is closed.
	defer s.reapStdinPump()
	for {
		resp, err := s.stream.Receive()
		if err != nil {
			// Stream ended without a terminal exit frame: EOF is a clean close,
			// anything else (ctx cancel, transport break) is a broken stream. A
			// ctx cancel is a deliberate teardown, which SIGKILLs the guest child
			// (guestd binds the child to the stream ctx), so report SIGKILL so
			// the runtime waitFunc recognizes a deliberate kill; a non-cancel
			// break with no exit frame is reported as a non-zero code.
			if s.ctx.Err() != nil {
				s.status = ExitStatus{Signal: int(sigKill)}
			} else if !errors.Is(err, io.EOF) {
				s.status = ExitStatus{Code: -1}
			}
			s.closePipes(err)
			return
		}
		switch frame := resp.GetFrame().(type) {
		case *compassv1.ExecStreamResponse_Stdout:
			// A write error means the caller's read end is gone; the guest child
			// is still reaped on stream teardown, so stop pumping this pipe.
			if _, werr := s.stdoutW.Write(frame.Stdout); werr != nil {
				s.closePipes(werr)
				return
			}
		case *compassv1.ExecStreamResponse_Stderr:
			if _, werr := s.stderrW.Write(frame.Stderr); werr != nil {
				s.closePipes(werr)
				return
			}
		case *compassv1.ExecStreamResponse_Exit:
			s.status = ExitStatus{
				Code:   int(frame.Exit.GetExitCode()),
				Signal: int(frame.Exit.GetSignal()),
			}
			s.closePipes(io.EOF)
			return
		default:
			// A duplicate started frame or an unknown frame: ignore and keep
			// reading toward the terminal exit frame.
		}
	}
}

// watchCancel forces the receive side down when the stream ctx is cancelled so
// a deliberate teardown reaps promptly. pumpResponses blocks synchronously in
// stream.Receive() and cannot itself select on ctx; the h2c transport's
// propagation of a request-ctx cancel to a blocked Receive is unbounded
// (observed >30s under CI load, and indefinite in-process), so this watcher
// calls CloseResponse — a local response-body close that aborts the blocked
// Receive at once and RST_STREAMs the server, whose bound child guestd then
// SIGKILL-reaps. On a normal terminal exit (done closed first) it returns
// without touching the stream, so it never closes a still-live receive half;
// CloseResponse is idempotent, so racing reapStdinPump's own call is safe.
func (s *GuestStream) watchCancel() {
	select {
	case <-s.ctx.Done():
		_ = s.stream.CloseResponse()
	case <-s.done:
	}
}

// closePipes closes both read-pipe writer ends with cause, so the caller's
// Stdout/Stderr reads observe EOF (or the cause). CloseWithError(io.EOF) yields
// a plain EOF to the reader.
func (s *GuestStream) closePipes(cause error) {
	_ = s.stdoutW.CloseWithError(cause) // signalled to the caller's Stdout reader
	_ = s.stderrW.CloseWithError(cause) // signalled to the caller's Stderr reader
}

// reapStdinPump unblocks and terminates pumpStdin and frees the response half
// on a stream's terminal exit. pumpStdin blocks in stdinR.Read until the caller
// writes or closes Stdin; on a clean child exit (or a break) where the caller
// did neither, nothing else wakes it, so closing the pump's reader end makes
// that Read return and the goroutine exit. It touches only the pipe, never the
// stream send half, so it does not race pumpStdin's own Send/CloseRequest (the
// send half stays single-owner). CloseResponse frees the receive half for
// connection reuse; it is safe here because pumpResponses is the sole Receiver
// and has already stopped. A caller that closed Stdin already drove pumpStdin's
// own CloseRequest, so the extra reader close is a harmless idempotent no-op.
func (s *GuestStream) reapStdinPump() {
	_ = s.stdinR.CloseWithError(io.ErrClosedPipe)
	_ = s.stream.CloseResponse()
}
