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
	"sync"

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
	return &commandRouter{inflight: map[string]*pendingCall{}}
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
// waiting call, keyed by request id. An unknown id (no matching in-flight call —
// e.g. a duplicate result, or a result for an already-timed-out-and-reaped call)
// is ignored. Called by the Sessions handler for each SessionsRequest result.
func (r *commandRouter) complete(result *compassv1internal.SessionsRequest) {
	id := result.GetRequestId()
	r.mu.Lock()
	call, ok := r.inflight[id]
	if ok {
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	call.result = result
	close(call.done)
}
