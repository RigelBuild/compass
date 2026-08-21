//go:build unix

package runner

// Loop-level tests for the unexported runSessions seam
// (docs/designs/platform/compass-runner-concurrent-dispatch/design.md, Approach
// (a) + Plan T1 tests 4 & 6, T-cap). These drive runSessions directly over a
// scripted in-memory sessionStream so a send failure, a shutdown-cancel join,
// and the Provision-arm concurrency cap can each be forced DETERMINISTICALLY —
// none of which is reproducible over a real bidi wire (a peer-broken Send is not
// deterministically forceable; a cancel-mid-flight races connect's stream close;
// the cap const is unexported Runner-local structure). The production
// RunSessions is a thin wrapper (stream := l.client.Sessions(ctx); return
// runSessions(...)), so the runnerhub wire tests exercise this same runSessions
// over the real wire — the seam is not untested indirection.
//
// Every wait is event-gated on a channel; testTimeout is only a fail-fast
// ceiling, never synchronization (no sleeps, no polling, no retries).

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// scriptStream is a scripted sessionStream: Receive hands out queued commands in
// order then blocks until CloseResponse (the ctx-cancel watcher) or an explicit
// EOF frame; Send is programmable per call. It records every sent result. It is
// the in-memory stand-in for the connect bidi stream that lets a test drive the
// runSessions loop with exact control over Send outcomes and Receive timing.
type scriptStream struct {
	mu sync.Mutex

	recv     chan recvItem                                  // queued Receive results; the loop reads in order
	sendHook func(*compassv1internal.SessionsRequest) error // per-Send override; nil = succeed

	closed    chan struct{} // closed by CloseResponse (the loop's ctx-cancel watcher)
	closeOnce sync.Once
}

type recvItem struct {
	cmd *compassv1internal.SessionsResponse
	err error
}

func newScriptStream() *scriptStream {
	return &scriptStream{recv: make(chan recvItem, 64), closed: make(chan struct{})}
}

func (s *scriptStream) Send(msg *compassv1internal.SessionsRequest) error {
	s.mu.Lock()
	hook := s.sendHook
	s.mu.Unlock()
	if hook != nil {
		return hook(msg)
	}
	return nil
}

func (s *scriptStream) Receive() (*compassv1internal.SessionsResponse, error) {
	select {
	case item := <-s.recv:
		return item.cmd, item.err
	case <-s.closed:
		// CloseResponse ran (the ctx-cancel watcher): surface EOF, the connect
		// stream's natural post-close Receive result.
		return nil, io.EOF
	}
}

func (s *scriptStream) CloseResponse() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// pushCmd queues one command for the loop to Receive.
func (s *scriptStream) pushCmd(cmd *compassv1internal.SessionsResponse) {
	s.recv <- recvItem{cmd: cmd}
}

// A broken Send surfaces as a NON-NIL runSessions error, even when the broken
// stream drives the subsequent Receive to io.EOF (test 6). This pins the
// context.Cause precedence over the unconditional io.EOF nil-arm (Approach (a)):
// a Send failure is off-loop (in a command goroutine), so it cancels the loop
// ctx with a send-failure cause; the classification must return that cause
// rather than reading the EOF the watcher's CloseResponse produces as a clean
// end.
//
// RED (io.EOF arm ahead of the cause check, the naive intermediate): the failed
// Send cancels the ctx, the watcher closes the response side, the next Receive
// returns io.EOF, and the loop returns nil — silently swallowing the send
// failure. GREEN: the cause check runs first and returns the non-nil error.
func TestRunSessionsBrokenSendSurfacesNonNilError(t *testing.T) {
	stream := newScriptStream()
	sendErr := errors.New("stream write failed")
	// The bootstrap Send (no request id) succeeds; the Start result Send fails.
	stream.sendHook = func(msg *compassv1internal.SessionsRequest) error {
		if msg.GetRequestId() == "req-1" {
			return sendErr
		}
		return nil
	}
	host := &fakeSessionHost{sessionID: "sess-1"}
	stream.pushCmd(startCommand("req-1"))

	err := runSessions(context.Background(), stream, host, discardLoggerRunner())
	if err == nil {
		t.Fatal("runSessions returned nil after a Send failure; the broken Send was swallowed (the io.EOF arm must not precede the context.Cause check)")
	}
	if !errors.Is(err, sendErr) {
		t.Fatalf("runSessions error = %v, want it to wrap the send failure %v", err, sendErr)
	}
}

// A clean external ctx cancel returns nil (the context.Canceled cause arm) — the
// counterpart to the send-failure case, proving the classification distinguishes
// a broken stream from an orderly shutdown.
func TestRunSessionsCleanCancelReturnsNil(t *testing.T) {
	stream := newScriptStream()
	host := &fakeSessionHost{sessionID: "sess-1"}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- runSessions(ctx, stream, host, discardLoggerRunner()) }()

	// Cancel with no command in flight and no send failure: a clean shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSessions after clean cancel = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("runSessions did not return after a clean cancel")
	}
}

// runSessions joins every in-flight command goroutine before returning: with a
// Provision parked in the host, cancelling the loop ctx must not let runSessions
// return until the parked command observes ctx.Done and unwinds (test 4, the
// leak-free shutdown join — cancelCause(nil); wg.Wait(); <-configWorkerDone).
//
// RED (per-command goroutine but no wg.Wait, the naive T3 intermediate):
// runSessions returns as soon as its Receive unblocks on cancel, while the
// Provision goroutine is still parked at its exit gate — so runSessions is
// observed to return WHILE the command goroutine is provably still in flight.
// GREEN: the join holds runSessions until the goroutine leaves; runSessions
// cannot return while the gate is held. This mirrors the entered+gate ordering
// of TestCloseJoinsConcurrentTeardowns (host_test.go) — the exit gate makes the
// "still in flight" state a stable, observable fact rather than a raced counter.
func TestRunSessionsShutdownJoinsInFlightCommand(t *testing.T) {
	stream := newScriptStream()
	host := &fakeSessionHost{
		containerName:     "cont-a",
		provisionEntered:  make(chan struct{}, 1),
		provisionRelease:  make(chan struct{}), // never released: only ctx.Done frees it
		provisionExiting:  make(chan struct{}, 1),
		provisionExitGate: make(chan struct{}),
	}
	// Release the exit gate on every path so a failing assertion cannot hang.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(host.provisionExitGate) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- runSessions(ctx, stream, host, discardLoggerRunner()) }()

	// Push a Provision and wait until it is parked in the host.
	stream.pushCmd(provisionCommand("req-1"))
	select {
	case <-host.provisionEntered:
	case <-timeAfter():
		t.Fatal("Provision never reached the host")
	}

	// Cancel: the parked Provision unwinds on ctx.Done and blocks at its exit
	// gate, signalling provisionExiting — a real, stable "still in flight" state.
	cancel()
	select {
	case <-host.provisionExiting:
	case <-timeAfter():
		t.Fatal("the parked Provision never unwound on ctx cancel")
	}

	// Prove the negative — runSessions has NOT returned while the command
	// goroutine is parked at its exit gate holding its wg token — with a bounded
	// BLOCKING observation, not a non-blocking peek. A peek is racy here: the
	// cancel that parks the Provision also drives runSessions' return down a
	// separate, LONGER path (watcher → CloseResponse → Receive EOF → classify →
	// configWorkerDone), so in the RED world (no wg.Wait) provisionExiting — the
	// shorter path — routinely fires before that return lands, and a peek misses
	// it (a false pass). Blocking closes the gap: in RED, done fires within the
	// window and reddens deterministically; in GREEN, done can NEVER fire while
	// the gate is held (runSessions is physically parked at wg.Wait on this very
	// goroutine's token), so the window elapses and the test proceeds. The window
	// is a bounded negative-observation, not synchronization or a timing sleep —
	// GREEN legitimately waits it out; RED never reaches the ceiling.
	select {
	case <-done:
		t.Fatal("runSessions returned while a command goroutine was still in flight at its exit gate; the shutdown join (wg.Wait) is missing")
	case <-time.After(2 * time.Second):
	}

	// Release the goroutine; only now may runSessions return, cleanly (nil).
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSessions after cancel = %v, want nil (clean shutdown)", err)
		}
	case <-timeAfter():
		t.Fatal("runSessions did not return after the in-flight goroutine was released")
	}
}

// The Provision-arm concurrency cap bounds concurrent Provisions to
// provisionConcurrency (T-cap): fanning out more distinct-id Provisions than the
// cap must never run more than the cap simultaneously (the overflow queues on
// the semaphore), while a non-Provision command still runs immediately (the cap
// is on the Provision arm only). Driven over the seam so the test reads the
// unexported cap directly.
//
// RED (no semaphore, pre-T-cap): all fanned-out Provisions park in the host at
// once, so the observed peak concurrency exceeds the cap — this trips the moment
// the (cap+1)-th enters, a fast deterministic fail, never the ceiling. GREEN: at
// most `cap` are ever live together.
func TestRunSessionsProvisionCapBoundsConcurrency(t *testing.T) {
	capN := provisionConcurrency
	fanout := capN + 4

	stream := newScriptStream()
	host := &fakeSessionHost{
		containerName:    "cont",
		provisionEntered: make(chan struct{}, fanout),
		provisionRelease: make(chan struct{}),
		stopEntered:      make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runSessions(ctx, stream, host, discardLoggerRunner()) }()

	for i := range fanout {
		stream.pushCmd(provisionCommand("req-" + string(rune('a'+i))))
	}

	// Gate until `cap` Provisions have entered — the saturating positive gate,
	// fired in both worlds (cap Provisions always run). With the semaphore, the
	// overflow now blocks on it before host.Provision, so no more enter until a
	// slot frees.
	for range capN {
		select {
		case <-host.provisionEntered:
		case <-timeAfter():
			t.Fatalf("fewer than the cap of %d Provisions entered; the fan-out should saturate the arm", capN)
		}
	}

	// A concurrent Stop still runs immediately with the Provision arm saturated —
	// the cap is on the Provision arm ONLY (pure event gate, Stop never touches
	// the semaphore).
	stream.pushCmd(stopCommand("req-stop"))
	select {
	case <-host.stopEntered:
	case <-timeAfter():
		t.Fatal("Stop did not run while Provisions were saturated; the cap must apply to the Provision arm ONLY, never a Stop")
	}

	// Release every Provision and drain the rest: with the cap, a slot frees only
	// after a parked Provision returns, so an overflow acquire strictly follows a
	// release and peak concurrency never exceeds the cap. Without the semaphore,
	// all `fanout` entered before any release, so the peak is `fanout`.
	close(host.provisionRelease)
	for range fanout - capN {
		select {
		case <-host.provisionEntered:
		case <-timeAfter():
			t.Fatal("the overflow Provisions never drained after release")
		}
	}
	if peak := host.peakProvisions(); peak > capN {
		t.Fatalf("peak concurrent Provisions = %d, want <= %d (the Provision-arm semaphore must bound them)", peak, capN)
	}

	cancel()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("runSessions did not return after release + cancel")
	}
}
