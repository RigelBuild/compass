//go:build unix

// The session-command router: the Server side of the Sessions bidi stream. A
// client-facing session RPC pushes its command down the Runner's response stream
// and blocks for the matching result, correlated by request id.

// OQ6 idempotency: a retry after a timeout reuses the request id, and the router
// returns the original in-flight/completed result rather than pushing a duplicate
// — so a retried relay-Start creates no duplicate container and no spurious
// ALREADY_RUNNING.
package runnerhub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/hashicorp/golang-lru/v2/expirable"
)

// deliverRefusal keeps the original trace carrier for send-only refusal cleanup.
type deliverRefusal struct {
	sessionID   string
	traceparent string
}

// frameClass tags frames with per-class queue and teardown behavior; all share one FIFO wire.
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

	// mu may be taken while Hub.mu is held (Hub.RunnerEnrolled); never take Hub.mu under it.
	mu sync.Mutex
	// inflight maps a request id to the pending call awaiting its result. A
	// retry with a live request id joins the existing call rather than issuing a
	// second command (idempotency).
	inflight map[string]*pendingCall

	// deliverRefusals: bounded set of request ids for send-only DELIVER dispatches
	// still awaiting a possible async refusal. A success rides a later delivery_ack
	// and registers no pendingCall; a REFUSAL rides the request stream as a
	// RunnerError, which complete() would drop — this set makes it OBSERVABLE.

	// A success's entry is never removed, so an unbounded set would grow with
	// lifetime delivers — the RIG-1610 leak. Bounded LRU; eviction is safe since a
	// real refusal arrives within one round-trip and is present at lookup, while
	// only successes grow old unremoved. Accessed under mu (onEvict=nil).
	deliverRefusals *expirable.LRU[string, deliverRefusal]
	// refusedDelivers counts observed deliver refusals (RESOURCE_EXHAUSTED and
	// any other RunnerError landing on a send1 id) — the diagnostic that a
	// refusal was seen and the cursor left unadvanced for the D2 sweep to
	// redeliver.
	refusedDelivers atomic.Uint64
	// log carries the refusal diagnostic. Set by the hub after construction
	// (enroll); nil falls back to slog.Default at use so a bare newCommandRouter
	// (tests) still logs safely.
	log *slog.Logger
	// onSessionUnknown runs when Runner refuses a deliver because the session is
	// gone, so the hub can drop the stale binding; nil (tests) skips it.
	onSessionUnknown func(sessionID, traceparent string)
}

// pendingCall is one outstanding command awaiting its result. done closes when
// the result lands; result/err carry it. Multiple retriers of the same request
// id all wait on the same pendingCall, so they observe one identical outcome.
type pendingCall struct {
	done   chan struct{}
	result *compassv1internal.SessionsRequest
	err    error
}

// deliverRefusalsMax bounds the refusal registry, which successful sends never
// leave. Size it so every id that can still be refused (one control round-trip)
// stays resident; eviction is safe only for long-past successful sends.
const deliverRefusalsMax = 16384

func newCommandRouter() *commandRouter {
	return &commandRouter{
		inflight: map[string]*pendingCall{},
		// ttl=0: no expiry, a pure size-bounded LRU (deliverRefusalsMax). The set
		// is advisory, so eviction is safe — an evicted id is a long-past
		// successful deliver that never has a refusal to look up.
		deliverRefusals: expirable.NewLRU[string, deliverRefusal](deliverRefusalsMax, nil, 0),
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

// attach creates a fresh sender queue and starts runSender as the attachment's sole stream sender.
func (r *commandRouter) attach(ctx context.Context, send func(*compassv1internal.SessionsResponse) error) {
	s := &senderState{
		queue: make(chan outFrame, sendQueueCap),
		done:  make(chan struct{}),
	}
	r.mu.Lock()
	r.sender = s
	r.mu.Unlock()
	go r.runSender(ctx, s, send)
}

// setSessionUnknown installs the hook complete() fires on a NotFound deliver refusal.
func (r *commandRouter) setSessionUnknown(fn func(sessionID, traceparent string)) {
	r.mu.Lock()
	r.onSessionUnknown = fn
	r.mu.Unlock()
}

// detach closes the queue under mu, fails in-flight calls, then joins the sender outside mu.
// Enqueues also hold mu, so none can race queue closure.
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

// runSender preserves FIFO order and is the only caller of stream Send.
// A send failure ends the stream; detach applies the per-class cleanup to remaining frames.
func (r *commandRouter) runSender(ctx context.Context, s *senderState, send func(*compassv1internal.SessionsResponse) error) {
	defer close(s.done)
	for f := range s.queue {
		if err := send(f.cmd); err != nil {
			r.failFrameOnSendError(ctx, f, err)
			return
		}
	}
}

// failFrameOnSendError completes commands so waiters wake, removes refused-deliver tracking, and logs failures.
// Signals remain best-effort.
func (r *commandRouter) failFrameOnSendError(ctx context.Context, f outFrame, err error) {
	id := f.cmd.GetRequestId()
	// The stream is dying, so drop its cancel; log under the trace that enqueued the frame.
	ctx = otelx.ContextWithTraceparent(context.WithoutCancel(ctx), f.cmd.GetTraceparent())
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
		r.logger().WarnContext(ctx, "pushing deliver to runner failed; cursor left unadvanced for the reconnect sweep",
			"request_id", id, "error", err)
	case frameSignal:
		r.logger().WarnContext(ctx, "pushing signal to runner failed; best-effort, the runner refetches on reconnect",
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

// dispatch waits for the correlated result; retries of an in-flight id share one call.
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
		return nil, fmt.Errorf("%s for command %q", ErrMsgNoSessionsStream, id)
	}
	call := &pendingCall{done: make(chan struct{})}
	r.inflight[id] = call
	// Fail-fast on a full queue: a blocking command must never be silently dropped
	// under a waiting caller. Delete the registration and return immediately, so
	// OQ6 idempotent retry is untouched (a retry with the same id re-issues, or
	// joins a still-queued first attempt's live pendingCall).
	if !r.enqueue(outFrame{cmd: cmd, class: frameCommand}) {
		delete(r.inflight, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("runner send queue full for command %q", id)
	}
	r.mu.Unlock()
	return waitCall(ctx, call)
}

// push sends best-effort signals without registering a pending call; the Runner refetches on reconnect.
// A full queue drops the signal, and Send failures are handled by runSender.
func (r *commandRouter) push(ctx context.Context, cmd *compassv1internal.SessionsResponse) error {
	r.mu.Lock()
	live := r.sender != nil
	enqueued := r.enqueue(outFrame{cmd: cmd, class: frameSignal})
	r.mu.Unlock()
	if live && !enqueued {
		r.logger().WarnContext(ctx, "runner send queue full for signal; dropped, the runner refetches on reconnect")
	}
	return nil
}

// send1 queues a deliver without waiting for a result; successful delivery is acknowledged asynchronously.
// It tracks request ids so refused sends remain observable, but registers no pending call.
// Missing stream or full queue returns an error, leaving the cursor for the later sweep.
func (r *commandRouter) send1(cmd *compassv1internal.SessionsResponse) error {
	id := cmd.GetRequestId()
	if id == "" {
		return errors.New("deliver command requires a request id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sender == nil {
		return fmt.Errorf("%s for deliver %q", ErrMsgNoSessionsStream, id)
	}
	// Add returns whether it evicted an LRU victim (a bool, not an error); not
	// actionable — an evicted id is a long-past successful deliver that will
	// never be looked up for a refusal.
	_ = r.deliverRefusals.Add(id, deliverRefusal{
		sessionID:   cmd.GetDeliverControl().GetSessionId(),
		traceparent: cmd.GetTraceparent(),
	})
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

// waitCall leaves timed-out calls in-flight so retries with the same id can join them.
func waitCall(ctx context.Context, call *pendingCall) (*compassv1internal.SessionsRequest, error) {
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// complete routes results to blocking calls, counts and logs send-only refusals, and ignores unknown ids.
func (r *commandRouter) complete(ctx context.Context, result *compassv1internal.SessionsRequest) {
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
	// A registered id is a send-only deliver awaiting a possible refusal. Its value
	// includes the target session and traceparent; clear it with the lookup.
	refusal, isDeliver := r.deliverRefusals.Peek(id)
	r.deliverRefusals.Remove(id)
	onUnknown := r.onSessionUnknown
	r.mu.Unlock()
	if !isDeliver {
		return
	}
	// A result correlated to a send-only deliver is a refusal — a successful
	// deliver sends no result (it acks via AgentFrame.delivery_ack). Count and log
	// it so the refusal is never silently dropped; the D2 sweep retries its cursor.
	r.refusedDelivers.Add(1)
	log := r.log
	if log == nil {
		log = slog.Default()
	}
	if runnerErr := result.GetError(); runnerErr != nil {
		log.WarnContext(ctx, "deliver refused by runner; cursor left unadvanced for the reconnect sweep",
			"request_id", id, "code", runnerErr.GetCode().String(), "message", runnerErr.GetMessage())
		notFound := runnerErr.GetCode() == compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND
		if notFound && onUnknown != nil && refusal.sessionID != "" {
			onUnknown(refusal.sessionID, refusal.traceparent)
		}
	} else {
		log.WarnContext(ctx, "deliver returned an unexpected non-error result; cursor left unadvanced",
			"request_id", id)
	}
}
