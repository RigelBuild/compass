//go:build unix

// The session-command router: the Server side of the Sessions bidi stream. A
// client-facing session RPC (Start/Stop/Reload/Status on CompassService) routes
// here; the router pushes the command down the Runner's Sessions response stream
// and blocks for the matching result on the Runner's request stream, correlated
// by request id.
//
// OQ6 idempotency (go-toolchain-default.md:1388-1389): a command carries a
// request id; a retry after a timeout reuses that id, and the router returns the
// original in-flight/completed result rather than pushing a duplicate command —
// so a relay-Start retried after a timeout creates no duplicate container and no
// spurious ALREADY_RUNNING.
package runnerhub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/hashicorp/golang-lru/v2/expirable"
)

// frameClass tags an outbound frame with its overflow/teardown contract. The
// three classes share one FIFO — they share the wire, and a command gains
// nothing by overtaking a deliver — but their full-queue and detach-time
// semantics differ. See
// docs/designs/infra/runtime/compass-runnerhub-send-queue/design.md.
type frameClass int

const (
	frameDeliver frameClass = iota // send1: send-only, cursor-backstopped
	frameSignal                    // push: fire-and-forget, best-effort
	frameCommand                   // dispatch: pendingCall-correlated, blocking caller
)

// outFrame is one queued outbound Sessions frame.
type outFrame struct {
	cmd   *compassv1internal.SessionsResponse
	class frameClass
}

// sendQueueCap bounds the per-router outbound queue. A frame is one small proto
// pointer, so memory is negligible; the bound exists to fail fast on a wedged
// stream, not to save memory.
const sendQueueCap = 256

// senderState is one attachment's queue plus its sender goroutine. A fresh
// senderState per attach means a re-attached stream never inherits stale frames
// from a previous attachment.
type senderState struct {
	queue chan outFrame
	done  chan struct{} // closed when the sender goroutine exits
}

// commandRouter correlates outbound session commands with the results the Runner
// returns on its request stream. One router per attached Runner.
type commandRouter struct {
	// sender is the live attachment's outbound queue plus its sender goroutine.
	// Set when a Runner's Sessions stream is live; nil before it opens or after
	// it detaches. Guarded by mu.
	sender *senderState

	mu sync.Mutex
	// inflight maps a request id to the pending call awaiting its result. A
	// retry with a live request id joins the existing call rather than issuing a
	// second command (idempotency).
	inflight map[string]*pendingCall

	// deliverRefusals is the bounded set of request ids for send-only DELIVER
	// dispatches (send1) still awaiting a possible async refusal. A successful
	// deliver returns NO synchronous result and rides a later
	// AgentFrame.delivery_ack, so send1 registers no pendingCall and does not
	// block (RIG-1569 §5). A REFUSAL does ride the Sessions request stream as a
	// RunnerError result correlated by request id, which complete() would
	// otherwise drop as "unknown". This set makes such a refusal OBSERVABLE
	// (logged + counted) instead of silently dropped.
	//
	// A successful deliver's entry is never removed by complete() (no refusal ever
	// lands for it), so an unbounded set would grow with total lifetime successful
	// delivers, not the in-flight working set — the RIG-1610 leak. It is a bounded
	// size-capped LRU (deliverRefusalsMax): once past the cap the oldest entries
	// are evicted. Eviction is safe — a refusal arrives within one control
	// round-trip of its send1, so at lookup time a real refusal's id is freshly
	// added and present; the only ids that grow old unremoved are successful
	// delivers, which never have a refusal to look up. The LRU is internally
	// synchronized, but is still accessed under mu here (onEvict=nil, so no
	// callback re-entrancy) to keep the same critical sections. Guarded by mu.
	deliverRefusals *expirable.LRU[string, struct{}]
	// refusedDelivers counts observed deliver refusals (RESOURCE_EXHAUSTED and
	// any other RunnerError landing on a send1 id) — the diagnostic that a
	// refusal was seen and the cursor left unadvanced for the D2 sweep to
	// redeliver.
	refusedDelivers atomic.Uint64
	// log carries the refusal diagnostic. Set by the hub after construction
	// (enroll); nil falls back to slog.Default at use so a bare newCommandRouter
	// (tests) still logs safely.
	log *slog.Logger
}

// pendingCall is one outstanding command awaiting its result. done closes when
// the result lands; result/err carry it. Multiple retriers of the same request
// id all wait on the same pendingCall, so they observe one identical outcome.
type pendingCall struct {
	done   chan struct{}
	result *compassv1internal.SessionsRequest
	err    error
}

// deliverRefusalsMax bounds the send1 refusal registry (deliverRefusals). A
// successful deliver's entry is never removed by complete() — no refusal ever
// lands for it — so an unbounded set would grow one entry per lifetime
// successful deliver for a long-lived Runner, a leak proportional to delivery
// volume (RIG-1610). Sized generously so a normal session's recent send1 ids all
// stay resident (a refusal within one control round-trip is always still
// present); an evicted id is a long-past successful deliver that will never be
// looked up for a refusal, so eviction is safe, never a correctness loss.
const deliverRefusalsMax = 16384

func newCommandRouter() *commandRouter {
	return &commandRouter{
		inflight: map[string]*pendingCall{},
		// ttl=0: no expiry, a pure size-bounded LRU (deliverRefusalsMax). The set
		// is advisory, so eviction is safe — an evicted id is a long-past
		// successful deliver that never has a refusal to look up.
		deliverRefusals: expirable.NewLRU[string, struct{}](deliverRefusalsMax, nil, 0),
	}
}

// RefusedDelivers reports the count of observed deliver refusals — a RunnerError
// result landing on a send1-registered id. The cursor is never advanced on send,
// so a refusal needs no rollback: the D2 sweep redelivers on the recipient's
// next start. Non-zero means a live deliver was refused (typically the session's
// control retention was full); the diagnostic sibling of the hub's frame
// counters.
func (r *commandRouter) RefusedDelivers() uint64 {
	return r.refusedDelivers.Load()
}

// attach binds the router to a live Sessions stream's send function. Called when
// the Runner opens its Sessions stream; detached when it closes. It builds a
// fresh senderState (queue + done) and spawns runSender, the sole caller of send
// for this attachment. See
// docs/designs/infra/runtime/compass-runnerhub-send-queue/design.md.
func (r *commandRouter) attach(send func(*compassv1internal.SessionsResponse) error) {
	s := &senderState{
		queue: make(chan outFrame, sendQueueCap),
		done:  make(chan struct{}),
	}
	r.mu.Lock()
	r.sender = s
	r.mu.Unlock()
	go r.runSender(s, send)
}

// detach tears down the live attachment: it nils and closes the outbound queue
// under mu, fails every in-flight call (the Runner's Sessions stream dropped, so
// no pending command can complete — callers observe the cause and the OQ6
// disconnect path takes over), then joins the sender goroutine on its done
// channel outside mu. Closing the queue is safe: every enqueue is a non-blocking
// send performed under mu after a non-nil sender check, so no send can race the
// close.
func (r *commandRouter) detach(cause error) {
	r.mu.Lock()
	s := r.sender
	r.sender = nil
	if s != nil {
		close(s.queue)
	}
	for id, call := range r.inflight {
		call.err = cause
		close(call.done)
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	if s != nil {
		<-s.done
	}
}

// runSender drains the attachment's queue into the stream in FIFO order. It is
// the ONLY caller of send for this attachment — the single-sender invariant that
// replaces the old sendMu (connect's server-side BidiStream.Send is not safe for
// concurrent use). It exits when the queue is closed (detach) or when a Send
// fails: a failed Send means the stream is dead, and the handler's
// defer router.detach tears down the rest (frames still queued then follow the
// detach semantics — a command's pendingCall is failed by detach, a deliver's
// cursor was never advanced, a signal is best-effort). See
// docs/designs/infra/runtime/compass-runnerhub-send-queue/design.md.
func (r *commandRouter) runSender(s *senderState, send func(*compassv1internal.SessionsResponse) error) {
	defer close(s.done)
	for f := range s.queue {
		if err := send(f.cmd); err != nil {
			r.failFrameOnSendError(f, err)
			return
		}
	}
}

// failFrameOnSendError applies the per-class teardown for the one frame whose
// Send failed. A command's pendingCall is COMPLETED with the error (set err,
// close done, delete from inflight — all under mu and presence-checked, exactly
// as detach and complete finish a waiting call, NOT a delete-only: the caller
// is parked in waitCall since the real Send now happens here, so a delete-only
// would hang it to ctx timeout; the presence check keeps this from
// double-closing against detach's in-flight sweep). A deliver's refusal entry is
// removed and the failure warn-logged (no command reached the Runner, so no
// refusal can arrive). A signal is warn-logged only, best-effort by contract.
func (r *commandRouter) failFrameOnSendError(f outFrame, err error) {
	id := f.cmd.GetRequestId()
	switch f.class {
	case frameCommand:
		r.mu.Lock()
		if call, ok := r.inflight[id]; ok {
			call.err = fmt.Errorf("pushing session command %q to runner: %w", id, err)
			close(call.done)
			delete(r.inflight, id)
		}
		r.mu.Unlock()
	case frameDeliver:
		r.mu.Lock()
		r.deliverRefusals.Remove(id)
		r.mu.Unlock()
		r.logger().Warn("pushing deliver to runner failed; cursor left unadvanced for the reconnect sweep",
			"request_id", id, "error", err)
	case frameSignal:
		r.logger().Warn("pushing signal to runner failed; best-effort, the runner refetches on reconnect",
			"error", err)
	}
}

// enqueue performs the non-blocking outbound send under mu. It returns false when
// there is no live sender OR the queue is full; callers distinguish the two under
// the same mu hold by checking r.sender themselves before calling.
func (r *commandRouter) enqueue(f outFrame) bool {
	if r.sender == nil {
		return false
	}
	select {
	case r.sender.queue <- f:
		return true
	default:
		return false
	}
}

// logger returns the refusal diagnostic logger, falling back to slog.Default so a
// bare newCommandRouter (tests) still logs safely.
func (r *commandRouter) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return slog.Default()
}

// dispatch pushes cmd (already carrying its request id) to the Runner and waits
// for the correlated result or ctx cancellation. A retry with an id already
// in-flight does NOT re-push: it joins the existing call and returns the same
// result (OQ6 idempotency). The command variant is set by the caller; dispatch
// only stamps correlation + waits.
func (r *commandRouter) dispatch(ctx context.Context, cmd *compassv1internal.SessionsResponse) (*compassv1internal.SessionsRequest, error) {
	id := cmd.GetRequestId()
	if id == "" {
		return nil, errors.New("session command requires a request id")
	}

	r.mu.Lock()
	// Idempotent retry: an id already in flight joins the existing call rather
	// than issuing a second command to the Runner.
	if existing, ok := r.inflight[id]; ok {
		r.mu.Unlock()
		return waitCall(ctx, existing)
	}
	if r.sender == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("no live runner sessions stream for command %q", id)
	}
	call := &pendingCall{done: make(chan struct{})}
	r.inflight[id] = call
	// Fail-fast on a full queue: a blocking command must never be silently
	// dropped under a waiting caller. Delete the registration and return an
	// error immediately — the exact shape of the old synchronous push-failure
	// path, so OQ6 idempotent retry is untouched (a retry with the same id
	// re-issues cleanly, a retry racing a still-queued first attempt joins its
	// live pendingCall). The caller-side contract already surfaces a prompt
	// error as CodeUnavailable.
	if !r.enqueue(outFrame{cmd: cmd, class: frameCommand}) {
		delete(r.inflight, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("runner send queue full for command %q", id)
	}
	r.mu.Unlock()
	return waitCall(ctx, call)
}

// push enqueues cmd onto the outbound queue WITHOUT registering a pendingCall or
// waiting for a result — the fire-and-forget counterpart to dispatch, for a
// signal-only Server->Runner push (SecretsVersion/ConfigVersion) that has no
// result variant on the request stream. A nil sender (no attached Runner) is a
// no-op success: the signal is best-effort, and the Runner re-fetches its secret
// set on reconnect regardless, so a session whose stream is momentarily detached
// loses nothing permanent. A full queue is the same best-effort outcome with a
// warn log for observability — the signal is advisory. A real Send error is
// handled asynchronously in runSender.
func (r *commandRouter) push(cmd *compassv1internal.SessionsResponse) error {
	r.mu.Lock()
	live := r.sender != nil
	enqueued := r.enqueue(outFrame{cmd: cmd, class: frameSignal})
	r.mu.Unlock()
	if live && !enqueued {
		r.logger().Warn("runner send queue full for signal; dropped, the runner refetches on reconnect")
	}
	return nil
}

// send1 enqueues a single send-only DELIVER command onto the outbound queue
// WITHOUT registering a blocking pendingCall — the crux of RIG-1569 §5. A
// successful deliver returns NO synchronous result (success rides a later
// AgentFrame.delivery_ack), so reusing dispatch — which registers an inflight
// call and blocks on waitCall for a result that never comes — would hang until
// ctx timeout on EVERY successful delivery. send1 enqueues and returns nil the
// instant the frame is queued; nil now means "queued", no longer "pushed", but
// success still rides the later delivery_ack exactly as before.
//
// It DOES register a lightweight refusal-only entry for cmd's request id: a
// refused deliver rides the Sessions request stream as a RunnerError result
// (SessionsRequest.error, correlated by request id, §5), which complete() would
// otherwise drop as "unknown". The entry makes such a refusal observable
// (logged + counted) rather than silently lost; it is NOT a pendingCall and no
// caller ever blocks on it.
//
// A nil sender (no attached Runner / detached stream) OR a FULL queue returns
// the "no live stream"-class refusal error: the consumer already treats any
// error as "no live session" and leaves the cursor unadvanced for the D2 sweep.
// On a full queue the refusal entry is removed (no frame will reach the Runner,
// so no refusal can arrive for it). See
// docs/designs/infra/runtime/compass-runnerhub-send-queue/design.md.
func (r *commandRouter) send1(cmd *compassv1internal.SessionsResponse) error {
	id := cmd.GetRequestId()
	if id == "" {
		return errors.New("deliver command requires a request id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sender == nil {
		return fmt.Errorf("no live runner sessions stream for deliver %q", id)
	}
	// Add returns whether it evicted an LRU victim (a bool, not an error); not
	// actionable — an evicted id is a long-past successful deliver that will
	// never be looked up for a refusal.
	_ = r.deliverRefusals.Add(id, struct{}{})
	if !r.enqueue(outFrame{cmd: cmd, class: frameDeliver}) {
		// The queue is full: no command will reach the Runner, so no refusal can
		// arrive for it — drop the refusal entry. The cursor was never advanced
		// on send, so there is nothing to roll back; the consumer falls to the
		// sweep on the returned error.
		r.deliverRefusals.Remove(id)
		return fmt.Errorf("runner send queue full for deliver %q", id)
	}
	return nil
}

// waitCall blocks until the call completes or ctx is cancelled. A ctx timeout
// leaves the call in-flight on purpose: a retry with the same id joins it, so a
// timeout-then-retry is idempotent rather than orphaning the original.
func waitCall(ctx context.Context, call *pendingCall) (*compassv1internal.SessionsRequest, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// complete delivers a result the Runner returned on its request stream to the
// waiting call, keyed by request id. Three id classes, in order:
//   - an inflight blocking call (dispatch): hand it the result and wake it.
//   - a send-only deliver awaiting a possible async refusal (send1): the result
//     is a RunnerError refusal — count + log it and clear the entry, so the
//     refusal is observable and not dropped (RIG-1569 §5). The cursor was never
//     advanced on send, so no rollback is needed; the D2 sweep redelivers.
//   - a truly-unknown id (a duplicate result, or a reaped call): ignored, the
//     original contract.
//
// Called by the Sessions handler for each SessionsRequest result.
func (r *commandRouter) complete(result *compassv1internal.SessionsRequest) {
	id := result.GetRequestId()
	r.mu.Lock()
	call, ok := r.inflight[id]
	if ok {
		delete(r.inflight, id)
		r.mu.Unlock()
		call.result = result
		close(call.done)
		return
	}
	// Remove returns whether the id was registered (present); a send-only deliver
	// awaiting a possible refusal. Removing here clears the entry as its refusal
	// lands, in the same critical section the map lookup+delete occupied.
	isDeliver := r.deliverRefusals.Remove(id)
	r.mu.Unlock()
	if !isDeliver {
		return
	}
	// A result correlated to a send-only deliver is a refusal — a successful
	// deliver sends no result (it acks via AgentFrame.delivery_ack). Count and
	// log it so the refusal is never silently dropped. The cursor was not
	// advanced on send, so the D2 sweep redelivers on the recipient's next start.
	r.refusedDelivers.Add(1)
	log := r.log
	if log == nil {
		log = slog.Default()
	}
	if runnerErr := result.GetError(); runnerErr != nil {
		log.Warn("deliver refused by runner; cursor left unadvanced for the reconnect sweep",
			"request_id", id, "code", runnerErr.GetCode().String(), "message", runnerErr.GetMessage())
	} else {
		log.Warn("deliver returned an unexpected non-error result; cursor left unadvanced",
			"request_id", id)
	}
}
