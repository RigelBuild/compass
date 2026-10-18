//go:build unix

package microvm

// The host-side exec layer over the U1 GuestControl client: a GuestExec
// wrapping the Connect client with a one-shot Exec and a streaming ExecStream
// that turns the bidi frame protocol into live io.Pipe stdio plus a kill/wait
// handle (design §(c)).

// Package boundary: this produces plain structs mirroring the proto, not
// runtime types — runtime imports microvm, so microvm importing runtime would
// cycle. The runtime-side adaptation (spec->ExecCall, kill/wait pair) is U4's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// sigKill is the signal reported for a deliberate teardown (a ctx-cancelled
// stream with no exit frame): guestd SIGKILLs the child bound to the broken
// stream, so the host reports SIGKILL, which the runtime waitFunc maps to the
// portable deliberate-kill error.
const sigKill = syscall.SIGKILL

// killSignalTimeout bounds Kill's Signal RPC so a wedged transport cannot stall
// teardown. Stop escalates to VMM kill; the demux goroutine still releases Wait.
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

// bidiStream is the subset of the generated client stream used by the exec pumps.
// A narrow interface keeps the cancellation path testable with a blocking fake.
type bidiStream interface {
	Send(req *compassv1.ExecStreamRequest) error
	Receive() (*compassv1.ExecStreamResponse, error)
	CloseSend() error
	Close() error
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
	resp, err := g.client.Exec(ctx, req)
	if err != nil {
		if timeout > 0 && isDeadline(err) {
			return ExecResult{}, &TimeoutError{Timeout: timeout}
		}
		return ExecResult{}, fmt.Errorf("guest exec: %w", err)
	}
	return ExecResult{
		Stdout:   resp.GetStdout(),
		Stderr:   resp.GetStderr(),
		ExitCode: int(resp.GetExitCode()),
	}, nil
}

// isDeadline reports whether err is a host-side deadline: a wrapped
// context.DeadlineExceeded or a Connect deadline-exceeded code.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		connect.CodeOf(err) == connect.CodeDeadlineExceeded
}

// ExecStream opens the bidi stream, sends the StartExec frame, and awaits
// ExecStarted before returning. An unstartable program instead returns an
// exited GuestStream with a stderr diagnostic and terminal 126/127 status.
// It returns a GuestStream whose io.Pipe stdio is pumped by a per-exec
// goroutine: stdin is framed as stdin frames and stdout/stderr are demuxed
// onto the read pipes, which close when ExecExit arrives.
func (g *GuestExec) ExecStream(ctx context.Context, call StreamCall) (_ *GuestStream, retErr error) {
	var stream bidiStream
	if g.openStream != nil {
		stream = g.openStream(ctx)
	} else {
		clientStream, err := g.openBidi(ctx)
		if err != nil {
			return nil, err
		}
		stream = clientStream
	}
	// Until the pumps take ownership, an early-error return must close the stream.
	defer func() {
		if retErr != nil {
			_ = stream.CloseSend()
			_ = stream.Close()
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
	if started.GetExecId() == "" {
		return nil, errors.New("guest exec stream: ExecStarted has empty exec_id")
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	gs := &GuestStream{
		ctx:     otelx.WithoutSpan(ctx), // stream-lifetime: the caller's span ends at Start
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

// openBidi opens the generated GuestControl ExecStream client stream.
func (g *GuestExec) openBidi(
	ctx context.Context,
) (compassv1internalconnect.GuestControlExecStreamClientStream, error) {
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
	_, err := s.client.Signal(ctx, &compassv1.SignalRequest{
		ExecId: s.execID,
		Signal: int32(sig), //nolint:gosec // G115: sig is a small signal number (syscall.Signal), never overflows int32
	})
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

// pumpStdin frames data from the caller's Stdin pipe. EOF sends stdin_close
// and closes the request direction; Kill uses the separate Signal RPC.
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
				return
			}
		}
		if readErr != nil {
			// EOF or another read error ends the child's stdin.
			if sendErr := s.stream.Send(&compassv1.ExecStreamRequest{
				Frame: &compassv1.ExecStreamRequest_StdinClose{StdinClose: &compassv1.StdinClose{}},
			}); sendErr != nil {
				return
			}
			// The response pump remains open to read stdout and the exit frame.
			_ = s.stream.CloseSend()
			return
		}
	}
}

// pumpResponses demuxes stdout/stderr frames onto the read pipes and records
// the exit status or stream failure before unblocking Wait.
func (s *GuestStream) pumpResponses() {
	defer close(s.done)
	defer s.reapStdinPump()
	for {
		resp, err := s.stream.Receive()
		if err != nil {
			// Stream ended without a terminal exit frame: EOF is clean, anything
			// else is a broken stream. A ctx cancel is a deliberate teardown that
			// SIGKILLs the guest child, so report SIGKILL for the runtime
			// waitFunc; a non-cancel break is reported as a non-zero code.
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
			if frame == nil {
				continue
			}
			if _, werr := s.stdoutW.Write(frame.Stdout); werr != nil {
				s.closePipes(werr)
				return
			}
		case *compassv1.ExecStreamResponse_Stderr:
			if frame == nil {
				continue
			}
			if _, werr := s.stderrW.Write(frame.Stderr); werr != nil {
				s.closePipes(werr)
				return
			}
		case *compassv1.ExecStreamResponse_Exit:
			if frame == nil {
				continue
			}
			// A malformed Exit frame with a nil Exit body still terminates: the
			// generated GetExitCode()/GetSignal() getters are nil-safe (return 0),
			// matching the pre-guard terminate-on-Exit behavior.
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

// watchCancel closes the client stream to interrupt a blocked Receive.
func (s *GuestStream) watchCancel() {
	select {
	case <-s.ctx.Done():
		// Close interrupts any blocked Receive and cancellation tears down the stream.
		_ = s.stream.Close()
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

// reapStdinPump unblocks the stdin pump and closes the response stream after
// pumpResponses has stopped receiving.
func (s *GuestStream) reapStdinPump() {
	_ = s.stdinR.CloseWithError(io.ErrClosedPipe) // read end is no longer used
	_ = s.stream.Close()                          // terminal stream cannot be reused
}
