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

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// commandRouter correlates outbound session commands with the results the Runner
// returns on its request stream. One router per attached Runner.
type commandRouter struct {
	// send pushes a command onto the Runner's Sessions response stream. Set when
	// a Runner's Sessions stream is live; nil before it opens. Guarded by mu.
	send func(*compassv1internal.SessionsResponse) error

	mu sync.Mutex
	// inflight maps a request id to the pending call awaiting its result. A
	// retry with a live request id joins the existing call rather than issuing a
	// second command (idempotency).
	inflight map[string]*pendingCall

	// deliverRefusals is the set of request ids for send-only DELIVER dispatches
	// (send1) still awaiting a possible async refusal. A successful deliver
	// returns NO synchronous result and rides a later AgentFrame.delivery_ack, so
	// send1 registers no pendingCall and does not block (SEA-1569 §5). A REFUSAL
	// does ride the Sessions request stream as a RunnerError result correlated by
	// request id, which complete() would otherwise drop as "unknown". This set
	// makes such a refusal OBSERVABLE (logged + counted) instead of silently
	// dropped; the entry is cleared when its refusal lands or is never read (a
	// successful deliver acks elsewhere, so the entry lingers harmlessly until
	// the router is discarded on Runner detach). Guarded by mu.
	deliverRefusals map[string]struct{}
	// refusedDelivers counts observed deliver refusals (RESOURCE_EXHAUSTED and
	// any other RunnerError landing on a send1 id) — the diagnostic that a
	// refusal was seen and the cursor left unadvanced for the D2 sweep to
	// redeliver.
	refusedDelivers atomic.Uint64
	// log carries the refusal diagnostic. Set by the hub after construction
	// (enroll); nil falls back to slog.Default at use so a bare newCommandRouter
	// (tests) still logs safely.
	log *slog.Logger

	// sendMu serializes concurrent calls into the live stream's Send. connect's
	// server-side BidiStream.Send is not safe for concurrent use, and multiple
	// client RPCs dispatch onto the one shared stream at once; a dedicated lock
	// (never held with mu) keeps map bookkeeping off the Send critical section.
	sendMu sync.Mutex
}

// pendingCall is one outstanding command awaiting its result. done closes when
// the result lands; result/err carry it. Multiple retriers of the same request
// id all wait on the same pendingCall, so they observe one identical outcome.
type pendingCall struct {
	done   chan struct{}
	result *compassv1internal.SessionsRequest
	err    error
}

func newCommandRouter() *commandRouter {
	return &commandRouter{
		inflight:        map[string]*pendingCall{},
		deliverRefusals: map[string]struct{}{},
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
// the Runner opens its Sessions stream; detached (send=nil) when it closes.
func (r *commandRouter) attach(send func(*compassv1internal.SessionsResponse) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.send = send
}

// detach clears the send function and fails every in-flight call — the Runner's
// Sessions stream dropped, so no pending command can complete. Callers observe
// the error and the session-disconnect path (OQ6) takes over.
func (r *commandRouter) detach(cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.send = nil
	for id, call := range r.inflight {
		call.err = cause
		close(call.done)
		delete(r.inflight, id)
	}
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
	if r.send == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("no live runner sessions stream for command %q", id)
	}
	call := &pendingCall{done: make(chan struct{})}
	r.inflight[id] = call
	send := r.send
	r.mu.Unlock()

	r.sendMu.Lock()
	err := send(cmd)
	r.sendMu.Unlock()
	if err != nil {
		// The push failed; drop the registration so a later retry can re-issue.
		r.mu.Lock()
		delete(r.inflight, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("pushing session command %q to runner: %w", id, err)
	}
	return waitCall(ctx, call)
}

// push sends cmd down the live Sessions stream WITHOUT registering a pendingCall
// or waiting for a result — the fire-and-forget counterpart to dispatch, for a
// signal-only Server->Runner push (SecretsVersion) that has no result variant on
// the request stream. A nil send (no attached Runner) is a no-op success: the
// signal is best-effort, and the Runner re-fetches its secret set on reconnect
// regardless, so a session whose stream is momentarily detached loses nothing
// permanent. It takes sendMu (never mu while sending) exactly as dispatch does,
// because connect's server-side BidiStream.Send is not safe for concurrent use.
func (r *commandRouter) push(cmd *compassv1internal.SessionsResponse) error {
	r.mu.Lock()
	send := r.send
	r.mu.Unlock()
	if send == nil {
		return nil
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return send(cmd)
}

// send1 pushes a single send-only DELIVER command down the live Sessions stream
// WITHOUT registering a blocking pendingCall — the crux of SEA-1569 §5. A
// successful deliver returns NO synchronous result (success rides a later
// AgentFrame.delivery_ack), so reusing dispatch — which registers an inflight
// call and blocks on waitCall for a result that never comes — would hang until
// ctx timeout on EVERY successful delivery. send1 pushes and returns nil the
// instant the push succeeds.
//
// It DOES register a lightweight refusal-only entry for cmd's request id: a
// refused deliver rides the Sessions request stream as a RunnerError result
// (SessionsRequest.error, correlated by request id, §5), which complete() would
// otherwise drop as "unknown". The entry makes such a refusal observable
// (logged + counted) rather than silently lost; it is NOT a pendingCall and no
// caller ever blocks on it. A nil send (no attached Runner / detached stream)
// returns an error so the consumer treats the id as "no live session" and falls
// to the D2 sweep, mirroring dispatch's no-live-stream error.
func (r *commandRouter) send1(cmd *compassv1internal.SessionsResponse) error {
	id := cmd.GetRequestId()
	if id == "" {
		return errors.New("deliver command requires a request id")
	}
	r.mu.Lock()
	if r.send == nil {
		r.mu.Unlock()
		return fmt.Errorf("no live runner sessions stream for deliver %q", id)
	}
	send := r.send
	r.deliverRefusals[id] = struct{}{}
	r.mu.Unlock()

	r.sendMu.Lock()
	err := send(cmd)
	r.sendMu.Unlock()
	if err != nil {
		// The push failed: drop the refusal registration (no command reached the
		// Runner, so no refusal can arrive for it) and report the failure so the
		// consumer falls to the sweep. The cursor was never advanced on send, so
		// there is nothing to roll back.
		r.mu.Lock()
		delete(r.deliverRefusals, id)
		r.mu.Unlock()
		return fmt.Errorf("pushing deliver %q to runner: %w", id, err)
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
//     refusal is observable and not dropped (SEA-1569 §5). The cursor was never
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
	_, isDeliver := r.deliverRefusals[id]
	if isDeliver {
		delete(r.deliverRefusals, id)
	}
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
