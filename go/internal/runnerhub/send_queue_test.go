//go:build unix

package runnerhub

// The per-router bounded send queue: latency isolation (a wedged Send no longer
// blocks another router or the calling path), per-class full-queue policy
// (deliver = synchronous refusal, signal = best-effort drop, command =
// fail-fast), FIFO order preserved to the wire, and the leak-free
// attach-spawns / detach-joins lifecycle with its per-class teardown of
// queued-but-unsent frames. See
// docs/designs/infra/runtime/compass-runnerhub-send-queue/design.md.
//
// Every case is channel-gated — no sleeps, no retries. A parked Send blocks on
// an explicit release channel the test controls, so "the caller returned while
// the stream is wedged" is observed on a done channel, not inferred from
// elapsed time. All run under -race.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// parkedSend is a stream send that blocks on release until the test unparks it,
// then returns retErr for every subsequent (and the released) call. It records
// the order in which frames reached the wire. The single-sender invariant means
// exactly one goroutine ever calls send, so no internal lock is needed for the
// order slice — but a lock keeps it race-detector clean against the test
// goroutine reading sentOrder.
type parkedSend struct {
	entered chan struct{} // one token per send call that has begun blocking
	release chan struct{} // closed by the test to unblock every parked send
	retErr  error         // returned by send once released (nil = success)
	mu      chanGuard
	order   []string
}

// chanGuard is a tiny mutex wrapper kept separate so the struct literal reads
// clearly; it exists only to make the order slice race-clean.
type chanGuard struct{ ch chan struct{} }

func newChanGuard() chanGuard { return chanGuard{ch: make(chan struct{}, 1)} }
func (g chanGuard) lock()     { g.ch <- struct{}{} }
func (g chanGuard) unlock()   { <-g.ch }

func newParkedSend(retErr error) *parkedSend {
	return &parkedSend{
		entered: make(chan struct{}, 1024),
		release: make(chan struct{}),
		retErr:  retErr,
		mu:      newChanGuard(),
	}
}

func (p *parkedSend) send(cmd *compassv1internal.SessionsResponse) error {
	p.entered <- struct{}{}
	<-p.release
	p.mu.lock()
	p.order = append(p.order, cmd.GetRequestId())
	p.mu.unlock()
	return p.retErr
}

func (p *parkedSend) sentOrder() []string {
	p.mu.lock()
	defer p.mu.unlock()
	out := make([]string, len(p.order))
	copy(out, p.order)
	return out
}

// deliverCmd builds a send-only DELIVER command carrying the given request id.
func deliverCmd(id string) *compassv1internal.SessionsResponse {
	return &compassv1internal.SessionsResponse{
		RequestId: id,
		Command: &compassv1internal.SessionsResponse_DeliverControl{
			DeliverControl: &compassv1internal.DispatchControl{SessionId: "sess-1"},
		},
	}
}

// signalCmd builds a fire-and-forget SIGNAL command (SecretsVersion) carrying
// the given request id for ordering assertions.
func signalCmd(id string) *compassv1internal.SessionsResponse {
	return &compassv1internal.SessionsResponse{
		RequestId: id,
		Command: &compassv1internal.SessionsResponse_SecretsVersion{
			SecretsVersion: &compassv1internal.SecretsVersion{SessionId: "sess-1"},
		},
	}
}

// Test 1 — a wedged Send on router A does not block router B. This is the
// baseline regression fence: the whole point of the per-router queue is that
// one Runner's stall is isolated. A parks on its first frame; B's send1 must
// still complete promptly.
func TestWedgedRouterDoesNotBlockAnother(t *testing.T) {
	a := newCommandRouter()
	pa := newParkedSend(nil)
	a.attach(pa.send)
	t.Cleanup(func() { close(pa.release); a.detach(errStreamClosed) })

	b := newCommandRouter()
	rec := newRecordingSend()
	b.attach(rec.send)
	t.Cleanup(func() { b.detach(errStreamClosed) })

	// Wedge A: enqueue a deliver, then wait for the sender goroutine to be parked
	// inside Send.
	if err := a.send1(deliverCmd("a-1")); err != nil {
		t.Fatalf("A send1 = %v, want nil (queued)", err)
	}
	<-pa.entered

	// B is unaffected: its send1 returns nil and the frame reaches B's wire.
	done := make(chan error, 1)
	go func() { done <- b.send1(deliverCmd("b-1")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("B send1 while A is wedged = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("B send1 blocked while A's Send is wedged — routers are not isolated")
	}
}

// Test 2 — a wedged Send does not block the calling path. A's sender is parked;
// send1 and push must return promptly (the frame is queued, not pushed), and
// dispatch must return control to waitCall (observable via ctx-cancel unblocking
// it) rather than parking inside the Send.
func TestWedgedSendDoesNotBlockCaller(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(nil)
	r.attach(p.send)
	t.Cleanup(func() { close(p.release); r.detach(errStreamClosed) })

	// Wedge the sender on a first frame.
	if err := r.send1(deliverCmd("wedge")); err != nil {
		t.Fatalf("wedging send1 = %v, want nil", err)
	}
	<-p.entered

	// send1 returns promptly (queued behind the wedged frame).
	done := make(chan error, 1)
	go func() { done <- r.send1(deliverCmd("d-1")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send1 behind a wedged Send = %v, want nil (queued)", err)
		}
	case <-timeAfter():
		t.Fatal("send1 blocked behind a wedged Send — it must enqueue and return")
	}

	// push returns promptly.
	pushDone := make(chan error, 1)
	go func() { pushDone <- r.push(signalCmd("s-1")) }()
	select {
	case err := <-pushDone:
		if err != nil {
			t.Fatalf("push behind a wedged Send = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("push blocked behind a wedged Send — it must enqueue and return")
	}

	// dispatch returns control to waitCall: it is not parked in the Send, so a
	// ctx cancel (never a result — the frame never reaches the wire) unblocks it.
	ctx, cancel := context.WithCancel(context.Background())
	dispDone := make(chan error, 1)
	go func() {
		_, err := r.dispatch(ctx, startCmd("c-1"))
		dispDone <- err
	}()
	cancel()
	select {
	case err := <-dispDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dispatch behind a wedged Send err = %v, want context.Canceled (it waited in waitCall, not in Send)", err)
		}
	case <-timeAfter():
		t.Fatal("dispatch blocked inside the Send — it must enqueue and wait in waitCall")
	}
}

// Test 3 — full queue, deliver class: an overflowing send1 returns a non-nil
// refusal error promptly and its deliverRefusals entry is removed, so a later
// complete for that id is a no-op unknown (never counted a refusal).
func TestFullQueueDeliverRefuses(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(nil)
	r.attach(p.send)
	t.Cleanup(func() { close(p.release); r.detach(errStreamClosed) })

	// Wedge the sender, then fill the queue to capacity. The first frame is the
	// one the sender pulled and parked on; sendQueueCap more fill the buffer.
	if err := r.send1(deliverCmd("wedge")); err != nil {
		t.Fatalf("wedging send1 = %v, want nil", err)
	}
	<-p.entered
	for i := range sendQueueCap {
		if err := r.send1(deliverCmd(fmt.Sprintf("fill-%d", i))); err != nil {
			t.Fatalf("fill send1 #%d = %v, want nil (still room)", i, err)
		}
	}

	// The next deliver overflows: a prompt non-nil refusal.
	const overflowID = "overflow"
	done := make(chan error, 1)
	go func() { done <- r.send1(deliverCmd(overflowID)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send1 on a full queue = nil, want a refusal error so the consumer falls to the sweep")
		}
	case <-timeAfter():
		t.Fatal("send1 on a full queue blocked — it must refuse synchronously")
	}

	// The overflow id's refusal entry was removed: a later result for it is a
	// no-op unknown, never counted a refusal.
	if r.deliverRefusals.Contains(overflowID) {
		t.Fatal("overflowing deliver left its refusal entry registered; it must be removed")
	}
	r.complete(&compassv1internal.SessionsRequest{
		RequestId: overflowID,
		Result:    &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{}},
	})
	if got := r.RefusedDelivers(); got != 0 {
		t.Fatalf("RefusedDelivers after a result for a removed overflow id = %d, want 0 (no-op unknown)", got)
	}
}

// Test 4 — full queue, command class: dispatch returns a non-nil error promptly
// (not hanging to ctx timeout) and the id is NOT left registered, so a retry
// with the same id re-issues rather than joining a phantom call.
func TestFullQueueCommandFailsFast(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(nil)
	r.attach(p.send)
	t.Cleanup(func() { close(p.release); r.detach(errStreamClosed) })

	if err := r.send1(deliverCmd("wedge")); err != nil {
		t.Fatalf("wedging send1 = %v, want nil", err)
	}
	<-p.entered
	for i := range sendQueueCap {
		if err := r.send1(deliverCmd(fmt.Sprintf("fill-%d", i))); err != nil {
			t.Fatalf("fill send1 #%d = %v, want nil", i, err)
		}
	}

	// dispatch overflows: a prompt error with a still-live ctx (proving it did
	// not hang to a ctx timeout).
	const cmdID = "cmd-overflow"
	done := make(chan error, 1)
	go func() {
		_, err := r.dispatch(context.Background(), startCmd(cmdID))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dispatch on a full queue = nil, want a fail-fast error")
		}
	case <-timeAfter():
		t.Fatal("dispatch on a full queue blocked — it must fail fast, not wait for a result")
	}

	// The id is not left registered: a retry finds no phantom call. Assert
	// directly on the inflight map under mu (a live registration would make the
	// retry join a call that will never complete).
	r.mu.Lock()
	_, stillRegistered := r.inflight[cmdID]
	r.mu.Unlock()
	if stillRegistered {
		t.Fatal("a full-queue dispatch left its id registered; a retry would join a phantom call")
	}
}

// Test 5 — full queue, signal class: push returns nil (best-effort) and does not
// block.
func TestFullQueueSignalDropsBestEffort(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(nil)
	r.attach(p.send)
	t.Cleanup(func() { close(p.release); r.detach(errStreamClosed) })

	if err := r.send1(deliverCmd("wedge")); err != nil {
		t.Fatalf("wedging send1 = %v, want nil", err)
	}
	<-p.entered
	for i := range sendQueueCap {
		if err := r.send1(deliverCmd(fmt.Sprintf("fill-%d", i))); err != nil {
			t.Fatalf("fill send1 #%d = %v, want nil", i, err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- r.push(signalCmd("sig-overflow")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("push on a full queue = %v, want nil (best-effort drop)", err)
		}
	case <-timeAfter():
		t.Fatal("push on a full queue blocked — best-effort must return promptly")
	}
}

// Test 6 — FIFO order preserved to the wire. Enqueue an interleaved sequence of
// delivers, commands, and signals while the sender is parked, then unpark and
// assert the recording send observed exactly the enqueue order.
func TestFIFOOrderToWire(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(nil)
	r.attach(p.send)

	// The sender pulls the first frame and parks; that frame is first on the
	// wire. Enqueue an interleaved sequence.
	want := []string{"d-0", "c-1", "s-2", "c-3", "d-4", "s-5", "d-6"}
	enqueue := func(id string) {
		var err error
		switch id[0] {
		case 'd':
			err = r.send1(deliverCmd(id))
		case 's':
			err = r.push(signalCmd(id))
		case 'c':
			// A command registers a pendingCall and blocks in waitCall; enqueue
			// it from a goroutine so this loop keeps the enqueue order.
			go func() { _, _ = r.dispatch(context.Background(), startCmd(id)) }()
			return
		}
		if err != nil {
			t.Errorf("enqueue %q = %v, want nil", id, err)
		}
	}

	// Enqueue the first frame and wait for the sender to park on it, so the
	// remaining enqueues all queue behind a known head.
	enqueue(want[0])
	<-p.entered
	for _, id := range want[1:] {
		enqueue(id)
		// For a command, wait until its pendingCall is registered so the enqueue
		// order into the channel is deterministic.
		if id[0] == 'c' {
			waitInflight(t, r, id)
		}
	}

	// Unpark: every queued frame drains in FIFO order.
	close(p.release)
	waitOrder(t, p, want)

	r.detach(errStreamClosed)
}

// waitInflight blocks until id is registered in the router's inflight map, so a
// command enqueue is observed durable before the next enqueue. Channel-gated via
// a bounded spin on the mutex-protected map, never a sleep.
func waitInflight(t *testing.T, r *commandRouter, id string) {
	t.Helper()
	deadline := timeAfter()
	for {
		r.mu.Lock()
		_, ok := r.inflight[id]
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("command %q never registered inflight", id)
		default:
			runtime.Gosched()
		}
	}
}

// waitOrder blocks until the parked send has observed exactly want (in order) or
// the deadline fires. Gated on the send's recorded order, never a sleep.
func waitOrder(t *testing.T, p *parkedSend, want []string) {
	t.Helper()
	deadline := timeAfter()
	for {
		got := p.sentOrder()
		if len(got) >= len(want) {
			if len(got) != len(want) {
				t.Fatalf("wire saw %d frames, want %d: %v", len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("wire order = %v, want %v (FIFO not preserved)", got, want)
				}
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("wire saw %v, want %v (incomplete drain)", got, want)
		default:
			runtime.Gosched()
		}
	}
}

// Test 7 — leak-free sender join on detach. The send is parked mid-frame; a real
// teardown unblocks it with an error and detach runs. The sender goroutine must
// exit (its done channel observable) and every queued command's pendingCall must
// be failed with the detach cause.
func TestDetachJoinsSenderAndFailsQueuedCommands(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(errors.New("stream torn down"))
	r.attach(p.send)

	// Queue a command behind the wedged head, wait for it to register.
	const wedgeID = "wedge"
	const cmdID = "queued-cmd"
	if err := r.send1(deliverCmd(wedgeID)); err != nil {
		t.Fatalf("wedging send1 = %v, want nil", err)
	}
	<-p.entered
	cmdErr := make(chan error, 1)
	go func() {
		_, err := r.dispatch(context.Background(), startCmd(cmdID))
		cmdErr <- err
	}()
	waitInflight(t, r, cmdID)

	// Capture the sender state to observe its done channel, then tear down as a
	// real stream teardown does: unblock the parked Send (with an error) and
	// detach.
	r.mu.Lock()
	s := r.sender
	r.mu.Unlock()

	cause := errors.New("runner stream dropped")
	close(p.release)
	r.detach(cause)

	// The sender goroutine exited: done is closed (detach joined it, but assert
	// the channel is observably closed).
	select {
	case <-s.done:
	case <-timeAfter():
		t.Fatal("sender goroutine did not exit after detach — leak")
	}

	// The queued command's pendingCall was failed with the detach cause.
	select {
	case err := <-cmdErr:
		if !errors.Is(err, cause) {
			t.Fatalf("queued command err = %v, want the detach cause %v", err, cause)
		}
	case <-timeAfter():
		t.Fatal("queued command never failed on detach — its caller is leaked")
	}
}

// Test 8 — a queued deliver at detach leaves recovery to the sweep. Queue a
// deliver behind a wedged head, detach before it sends; no panic, the deliver
// never reaches the wire, and complete for its id is a no-op (the cursor was
// never advanced, so the D2 sweep is the recovery).
func TestQueuedDeliverAtDetachNeverSends(t *testing.T) {
	r := newCommandRouter()
	p := newParkedSend(errors.New("stream torn down"))
	r.attach(p.send)

	const wedgeID = "wedge"
	const deliverID = "queued-deliver"
	if err := r.send1(deliverCmd(wedgeID)); err != nil {
		t.Fatalf("wedging send1 = %v, want nil", err)
	}
	<-p.entered
	if err := r.send1(deliverCmd(deliverID)); err != nil {
		t.Fatalf("queued send1 = %v, want nil", err)
	}

	// Tear down: the queued deliver is discarded, not sent.
	close(p.release)
	r.detach(errStreamClosed)

	order := p.sentOrder()
	for _, id := range order {
		if id == deliverID {
			t.Fatalf("queued deliver %q reached the wire after detach; it must be discarded", deliverID)
		}
	}

	// A stray complete for the queued deliver's id must be a harmless no-op: no
	// result can arrive for a frame that never left, so at most it is an
	// observable-refusal miss, never a panic. detach does NOT clean deliverRefusals
	// entries (per design (e)); the entry is left to LRU eviction, so it may or may
	// not still be resident — the contract asserted here is only non-panic.
	r.complete(&compassv1internal.SessionsRequest{
		RequestId: deliverID,
		Result:    &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{}},
	})
}

// Test 9 — Send-failure per-class teardown. The Send fails on the first frame; a
// command frame's parked dispatch caller must UNBLOCK with the error (its
// waitCall returns the Send error, not merely that the id left inflight), a
// deliver frame's refusal entry must be removed, and the sender must exit.
func TestSendFailurePerClassTeardown(t *testing.T) {
	t.Run("command frame caller unblocks with the send error", func(t *testing.T) {
		r := newCommandRouter()
		sendErr := errors.New("send failed")
		p := newParkedSend(sendErr)
		r.attach(p.send)
		t.Cleanup(func() { r.detach(errStreamClosed) })

		const cmdID = "cmd-1"
		dispDone := make(chan error, 1)
		go func() {
			_, err := r.dispatch(context.Background(), startCmd(cmdID))
			dispDone <- err
		}()
		waitInflight(t, r, cmdID)
		<-p.entered // the sender pulled the command and parked in Send

		// Release: the Send returns the error, and runSender completes the
		// pendingCall with it.
		close(p.release)
		select {
		case err := <-dispDone:
			if err == nil || !errors.Is(err, sendErr) {
				t.Fatalf("dispatch caller err = %v, want the send error %v", err, sendErr)
			}
		case <-timeAfter():
			t.Fatal("dispatch caller never unblocked on a Send failure — a delete-only would hang it")
		}
	})

	t.Run("deliver frame refusal entry removed and sender exits", func(t *testing.T) {
		r := newCommandRouter()
		sendErr := errors.New("send failed")
		p := newParkedSend(sendErr)
		r.attach(p.send)
		t.Cleanup(func() { r.detach(errStreamClosed) })

		const deliverID = "deliver-1"
		if err := r.send1(deliverCmd(deliverID)); err != nil {
			t.Fatalf("send1 = %v, want nil (queued)", err)
		}
		r.mu.Lock()
		s := r.sender
		r.mu.Unlock()
		<-p.entered // sender parked on the deliver

		close(p.release)

		// The sender exits on the Send error.
		select {
		case <-s.done:
		case <-timeAfter():
			t.Fatal("sender did not exit after a Send failure")
		}
		// The deliver's refusal entry was removed (no frame reached the Runner).
		r.mu.Lock()
		resident := r.deliverRefusals.Contains(deliverID)
		r.mu.Unlock()
		if resident {
			t.Fatal("a Send-failed deliver left its refusal entry registered; it must be removed")
		}
	})
}
