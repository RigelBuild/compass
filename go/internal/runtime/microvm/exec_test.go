//go:build unix

package microvm

// Hermetic round-trip for the host exec layer (RIG-2588 U3): a fake
// GuestControl server on a unix listener speaking h2c Connect, dialed by a real
// GuestControlClient — no KVM, no vsock muxer. The fake's ExecStream handler is
// scripted per test so the pump's demux, stdin framing, kill/exit unblocking,
// ctx-cancel teardown, and one-shot timeout are each exercised against a real
// bidi stream.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// fakeGuest is a scriptable GuestControl server. Each RPC delegates to a func
// field so a test supplies only the behavior it exercises; an unset field
// embeds UnimplementedGuestControlHandler's CodeUnimplemented.
type fakeGuest struct {
	compassv1internalconnect.UnimplementedGuestControlHandler
	execFn       func(context.Context, *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error)
	execStreamFn func(context.Context, *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error
	signalFn     func(context.Context, *connect.Request[compassv1.SignalRequest]) (*connect.Response[compassv1.SignalResponse], error)
}

func (f *fakeGuest) Exec(ctx context.Context, req *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error) {
	return f.execFn(ctx, req)
}

func (f *fakeGuest) ExecStream(ctx context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
	return f.execStreamFn(ctx, stream)
}

func (f *fakeGuest) Signal(ctx context.Context, req *connect.Request[compassv1.SignalRequest]) (*connect.Response[compassv1.SignalResponse], error) {
	return f.signalFn(ctx, req)
}

// serveFakeGuest binds a unix listener, serves fake over h2c Connect, and
// returns a GuestExec whose client dials that listener. Everything is torn down
// via t.Cleanup.
func serveFakeGuest(t *testing.T, fake *fakeGuest) *GuestExec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guest.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding fake guest: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // listener teardown

	mux := http.NewServeMux()
	mux.Handle(compassv1internalconnect.NewGuestControlHandler(fake))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols}
	t.Cleanup(func() { _ = srv.Close() }) // server teardown
	go func() { _ = srv.Serve(ln) }()     // errors on Close

	clientProtocols := new(http.Protocols)
	clientProtocols.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{
		Transport: &http.Transport{
			Protocols: clientProtocols,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
	client := compassv1internalconnect.NewGuestControlClient(httpClient, "http://guest")
	return NewGuestExec(client)
}

func TestGuestExec_OneShot_NonZeroExitIsSuccess(t *testing.T) {
	ge := serveFakeGuest(t, &fakeGuest{
		execFn: func(_ context.Context, req *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error) {
			// Echo the command back so we also prove the spec mapped through.
			if got := req.Msg.GetCommand(); len(got) != 1 || got[0] != "false" {
				t.Errorf("command = %v, want [false]", got)
			}
			return connect.NewResponse(&compassv1.ExecResponse{
				Stdout:   []byte("out"),
				Stderr:   []byte("err"),
				ExitCode: 3,
			}), nil
		},
	})

	res, err := ge.Exec(context.Background(), ExecCall{Command: []string{"false"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "out" || string(res.Stderr) != "err" {
		t.Fatalf("stdout/stderr = %q/%q, want out/err", res.Stdout, res.Stderr)
	}
}

func TestGuestExec_OneShot_RefusalIsError(t *testing.T) {
	ge := serveFakeGuest(t, &fakeGuest{
		execFn: func(context.Context, *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("uid 0 refused"))
		},
	})

	_, err := ge.Exec(context.Background(), ExecCall{Command: []string{"whoami"}})
	if err == nil {
		t.Fatal("expected an error for a guest-side refusal")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestGuestExec_OneShot_Timeout(t *testing.T) {
	ge := serveFakeGuest(t, &fakeGuest{
		execFn: func(ctx context.Context, _ *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error) {
			<-ctx.Done() // outlive the host deadline
			return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
		},
	})

	_, err := ge.Exec(context.Background(), ExecCall{Command: []string{"sleep"}, TimeoutSeconds: 1})
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v (%T), want *TimeoutError", err, err)
	}
}

// scriptStream is a small helper wiring the guest-side of ExecStream: it sends
// the ExecStarted frame, then runs body with the stream. body returns the exit
// frame to emit (or nil to end without one).
func startStream(t *testing.T, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse], execID string) {
	t.Helper()
	if err := stream.Send(&compassv1.ExecStreamResponse{
		Frame: &compassv1.ExecStreamResponse_Started{Started: &compassv1.ExecStarted{ExecId: execID}},
	}); err != nil {
		t.Errorf("sending started: %v", err)
	}
}

func TestGuestExec_Stream_PumpDemuxAndExit(t *testing.T) {
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(_ context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			// First frame must be the start.
			first, err := stream.Receive()
			if err != nil {
				return err
			}
			if first.GetStart() == nil {
				t.Errorf("first request frame was not start: %T", first.GetFrame())
			}
			startStream(t, stream, "exec-1")
			// Interleave stdout/stderr, then exit.
			_ = stream.Send(&compassv1.ExecStreamResponse{Frame: &compassv1.ExecStreamResponse_Stdout{Stdout: []byte("o1")}})
			_ = stream.Send(&compassv1.ExecStreamResponse{Frame: &compassv1.ExecStreamResponse_Stderr{Stderr: []byte("e1")}})
			_ = stream.Send(&compassv1.ExecStreamResponse{Frame: &compassv1.ExecStreamResponse_Stdout{Stdout: []byte("o2")}})
			return stream.Send(&compassv1.ExecStreamResponse{
				Frame: &compassv1.ExecStreamResponse_Exit{Exit: &compassv1.ExecExit{ExitCode: 0}},
			})
		},
	})

	gs, err := ge.ExecStream(context.Background(), StreamCall{Command: []string{"echo"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	// Close stdin immediately (no input); the pump half-closes.
	if err := gs.Stdin.Close(); err != nil {
		t.Fatalf("closing stdin: %v", err)
	}

	// Both pipes must be drained concurrently: the pump blocks writing a stderr
	// frame until the caller reads it, so a serial read (all stdout, then
	// stderr) would deadlock against an interleaved stream — the same
	// continuous-drain property the runner relies on.
	var stdout, stderr []byte
	var rerr, eerr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); stdout, rerr = io.ReadAll(gs.Stdout) }()
	go func() { defer wg.Done(); stderr, eerr = io.ReadAll(gs.Stderr) }()
	wg.Wait()
	if rerr != nil {
		t.Fatalf("reading stdout: %v", rerr)
	}
	if eerr != nil {
		t.Fatalf("reading stderr: %v", eerr)
	}
	if string(stdout) != "o1o2" {
		t.Fatalf("stdout = %q, want o1o2", stdout)
	}
	if string(stderr) != "e1" {
		t.Fatalf("stderr = %q, want e1", stderr)
	}
	if got := gs.Wait(); got.Code != 0 || got.Signal != 0 {
		t.Fatalf("exit status = %+v, want {Code:0}", got)
	}
}

func TestGuestExec_Stream_StdinFramingAndClose(t *testing.T) {
	gotStdin := make(chan []byte, 1)
	gotClose := make(chan struct{}, 1)
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(_ context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			if _, err := stream.Receive(); err != nil { // start
				return err
			}
			startStream(t, stream, "exec-1")
			var buf []byte
			for {
				req, err := stream.Receive()
				if err != nil {
					return err
				}
				switch f := req.GetFrame().(type) {
				case *compassv1.ExecStreamRequest_Stdin:
					buf = append(buf, f.Stdin...)
				case *compassv1.ExecStreamRequest_StdinClose:
					gotStdin <- buf
					gotClose <- struct{}{}
					return stream.Send(&compassv1.ExecStreamResponse{
						Frame: &compassv1.ExecStreamResponse_Exit{Exit: &compassv1.ExecExit{ExitCode: 0}},
					})
				}
			}
		},
	})

	gs, err := ge.ExecStream(context.Background(), StreamCall{Command: []string{"cat"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if _, err := gs.Stdin.Write([]byte("hello ")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if _, err := gs.Stdin.Write([]byte("world")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := gs.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	select {
	case got := <-gotStdin:
		if string(got) != "hello world" {
			t.Fatalf("guest stdin = %q, want %q", got, "hello world")
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for stdin bytes")
	}
	select {
	case <-gotClose:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for stdin_close frame")
	}
	gs.Wait()
}

func TestGuestExec_Stream_KillIssuesSignalAndUnblocksWait(t *testing.T) {
	var mu sync.Mutex
	var gotSignal int32
	signalled := make(chan struct{})
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(ctx context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			if _, err := stream.Receive(); err != nil { // start
				return err
			}
			startStream(t, stream, "exec-kill")
			// Wait for the kill signal to be observed, then emit a signalled
			// exit frame — the guest's own reap of the SIGKILLed child.
			<-signalled
			return stream.Send(&compassv1.ExecStreamResponse{
				Frame: &compassv1.ExecStreamResponse_Exit{Exit: &compassv1.ExecExit{Signal: 9}},
			})
		},
		signalFn: func(_ context.Context, req *connect.Request[compassv1.SignalRequest]) (*connect.Response[compassv1.SignalResponse], error) {
			mu.Lock()
			gotSignal = req.Msg.GetSignal()
			mu.Unlock()
			if req.Msg.GetExecId() != "exec-kill" {
				t.Errorf("signal exec_id = %q, want exec-kill", req.Msg.GetExecId())
			}
			close(signalled)
			return connect.NewResponse(&compassv1.SignalResponse{}), nil
		},
	})

	gs, err := ge.ExecStream(context.Background(), StreamCall{Command: []string{"sleep"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if err := gs.Kill(9); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	got := gs.Wait()
	if got.Signal != 9 {
		t.Fatalf("exit status = %+v, want Signal 9", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSignal != 9 {
		t.Fatalf("guest saw signal %d, want 9", gotSignal)
	}
}

func TestGuestExec_Stream_CtxCancelBreaksStream(t *testing.T) {
	streamBroke := make(chan struct{})
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(ctx context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			if _, err := stream.Receive(); err != nil { // start
				return err
			}
			startStream(t, stream, "exec-cancel")
			// Block reading; the host cancel breaks the stream and Receive here
			// returns an error the fake observes.
			_, err := stream.Receive()
			close(streamBroke)
			return err
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	gs, err := ge.ExecStream(ctx, StreamCall{Command: []string{"sleep"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	cancel()

	// Wait unblocks on the broken stream and reports SIGKILL (deliberate
	// teardown reaps the guest child).
	got := gs.Wait()
	if got.Signal != int(sigKill) {
		t.Fatalf("exit status = %+v, want Signal SIGKILL after ctx cancel", got)
	}
	select {
	case <-streamBroke:
	case <-time.After(testTimeout):
		t.Fatal("fake server never observed the stream break")
	}
}

func TestGuestExec_Stream_CleanExitReapsStdinPump(t *testing.T) {
	// Regression for the stdin-pump leak: on a clean child exit where the caller
	// never closes Stdin, pumpResponses must still reap pumpStdin (otherwise it
	// is parked forever in stdinR.Read). reapStdinPump closes the pump's reader
	// end via a defer that runs BEFORE close(s.done), so by the time Wait()
	// returns the stdin pipe is closed at its read end — a caller Write then
	// fails with the pipe-closed error. Before the fix the pump held the pipe
	// open and the Write would block/succeed, so this is a true regression gate
	// with no goroutine-count heuristic and no sleep.
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(_ context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			if _, err := stream.Receive(); err != nil { // start
				return err
			}
			startStream(t, stream, "exec-reap")
			// Exit immediately with no output; the caller never touches Stdin.
			return stream.Send(&compassv1.ExecStreamResponse{
				Frame: &compassv1.ExecStreamResponse_Exit{Exit: &compassv1.ExecExit{ExitCode: 0}},
			})
		},
	})

	gs, err := ge.ExecStream(context.Background(), StreamCall{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	// Deliberately do NOT close gs.Stdin: the leak this guards is exactly the
	// caller that leaves Stdin open on a clean exit.
	if got := gs.Wait(); got.Code != 0 || got.Signal != 0 {
		t.Fatalf("exit status = %+v, want {Code:0}", got)
	}
	// After Wait returns, reapStdinPump has closed the pump's read end, so the
	// caller's write end is broken — the deterministic proof the pump unblocked.
	if _, werr := gs.Stdin.Write([]byte("x")); werr == nil {
		t.Fatal("Stdin.Write succeeded after a clean exit; pumpStdin was not reaped (goroutine leak)")
	}
}

func TestGuestExec_Stream_TransportBreakIsFailureExit(t *testing.T) {
	// A non-EOF, non-cancel Receive error (a mid-stream transport/handler break
	// with no exit frame) must classify as a failure exit (Code -1, no signal),
	// NOT a clean exit or a deliberate kill — so isDeliberateKill treats it as a
	// real failure.
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(_ context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			if _, err := stream.Receive(); err != nil { // start
				return err
			}
			startStream(t, stream, "exec-break")
			// Return an error with no exit frame: the client's Receive surfaces a
			// connect error (not io.EOF), and the ctx is never cancelled.
			return errors.New("simulated mid-stream transport break")
		},
	})

	gs, err := ge.ExecStream(context.Background(), StreamCall{Command: []string{"sleep"}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	got := gs.Wait()
	if got.Code != -1 || got.Signal != 0 {
		t.Fatalf("exit status = %+v, want {Code:-1} (transport break is a failure, not a kill or clean exit)", got)
	}
}

func TestGuestExec_Stream_FirstFrameNotStartedIsError(t *testing.T) {
	// A spawn failure surfaces as a non-Started first frame: the guest sends an
	// Exit (or any non-Started) frame instead of ExecStarted. ExecStream must
	// return an error rather than a live GuestStream — and reap both stream
	// halves on that early-error return (the defer'd CloseRequest/CloseResponse),
	// so a failed spawn leaks neither the response reader nor its goroutine.
	ge := serveFakeGuest(t, &fakeGuest{
		execStreamFn: func(_ context.Context, stream *connect.BidiStream[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse]) error {
			if _, err := stream.Receive(); err != nil { // start
				return err
			}
			// Skip the ExecStarted frame the client awaits: send an exit frame
			// first, simulating a guest that failed to spawn the child.
			return stream.Send(&compassv1.ExecStreamResponse{
				Frame: &compassv1.ExecStreamResponse_Exit{Exit: &compassv1.ExecExit{ExitCode: 127}},
			})
		},
	})

	gs, err := ge.ExecStream(context.Background(), StreamCall{Command: []string{"nonexistent"}})
	if err == nil {
		t.Fatalf("ExecStream with a non-started first frame = %+v, nil error; want an error", gs)
	}
	if gs != nil {
		t.Fatalf("ExecStream returned a non-nil stream (%+v) alongside an error", gs)
	}
}
