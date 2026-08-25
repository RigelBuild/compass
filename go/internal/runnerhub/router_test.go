//go:build unix

package runnerhub

// The commandRouter: OQ6 rows 2/3 (request-id idempotency) plus correlation and
// detach. Every test pins a behavior a plausible bug would break: a retry with a
// live id must JOIN the in-flight call (one send, both callers the same result),
// distinct ids must each send; complete() must deliver by id; detach() must fail
// every in-flight call so the disconnect path takes over.
//
// The concurrent cases run under testing/synctest: synctest.Wait() blocks until
// every dispatch goroutine is DURABLY blocked (registered + waiting on its
// pendingCall), so the test advances on that observed state — never a sleep, and
// never a retry. That makes "the second caller joined before we completed"
// deterministic rather than a scheduling race.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// recordingSend captures every command the router pushes, so a test can assert
// exactly how many commands reached the Runner. Concurrency-safe.
type recordingSend struct {
	mu   sync.Mutex
	sent []*compassv1internal.SessionsResponse
}

func newRecordingSend() *recordingSend {
	return &recordingSend{}
}

func (s *recordingSend) send(cmd *compassv1internal.SessionsResponse) error {
	s.mu.Lock()
	s.sent = append(s.sent, cmd)
	s.mu.Unlock()
	return nil
}

func (s *recordingSend) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *recordingSend) ids() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, c := range s.sent {
		out[c.GetRequestId()] = true
	}
	return out
}

// startCmd builds a Start command carrying the given request id.
func startCmd(id string) *compassv1internal.SessionsResponse {
	return &compassv1internal.SessionsResponse{
		RequestId: id,
		Command:   &compassv1internal.SessionsResponse_Start{Start: &compassv1.StartAgentSessionRequest{}},
	}
}

// startResult builds a successful Start result correlated to request id.
func startResult(id, sessionID string) *compassv1internal.SessionsRequest {
	return &compassv1internal.SessionsRequest{
		RequestId: id,
		Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: sessionID}},
	}
}

// dispatchOutcome is one caller's dispatch result.
type dispatchOutcome struct {
	result *compassv1internal.SessionsRequest
	err    error
}

// OQ6 row 2: two dispatch calls with the SAME request id push ONE command to the
// Runner, and BOTH callers observe the one completed result. A bug that failed
// to dedupe would push twice and could hand back divergent results.
func TestDispatchSameRequestIdSendsOnceBothGetResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newCommandRouter()
		send := newRecordingSend()
		r.attach(send.send)
		defer r.detach(errStreamClosed)

		const id = "req-dup"
		outcomes := make(chan dispatchOutcome, 2)
		dispatch := func() {
			res, err := r.dispatch(context.Background(), startCmd(id))
			outcomes <- dispatchOutcome{result: res, err: err}
		}

		go dispatch()
		// The first caller is now durably blocked in waitCall, and its command
		// has been pushed exactly once.
		synctest.Wait()
		if got := send.count(); got != 1 {
			t.Fatalf("after first dispatch, sends = %d, want 1", got)
		}

		go dispatch()
		// The second caller with the same id is now durably blocked too — it
		// must have JOINED the in-flight call, not pushed a second command.
		synctest.Wait()
		if got := send.count(); got != 1 {
			t.Fatalf("second dispatch of a live id pushed a command (sends = %d, want 1); idempotent join broken", got)
		}

		// Complete the one in-flight call; both callers unblock with it.
		r.complete(startResult(id, "sess-42"))
		synctest.Wait()

		for i := range 2 {
			o := <-outcomes
			if o.err != nil {
				t.Fatalf("caller %d dispatch err = %v, want nil", i, o.err)
			}
			if got := o.result.GetStart().GetSessionId(); got != "sess-42" {
				t.Fatalf("caller %d session id = %q, want sess-42 (both callers see the one result)", i, got)
			}
		}
	})
}

// OQ6 row 3: distinct request ids are genuinely distinct — each pushes its own
// command. The complement to the dedup test: dedup must key on the id, not
// collapse everything.
func TestDispatchDistinctRequestIdsSendTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newCommandRouter()
		send := newRecordingSend()
		r.attach(send.send)
		defer r.detach(errStreamClosed)

		outcomes := make(chan dispatchOutcome, 2)
		go func() {
			res, err := r.dispatch(context.Background(), startCmd("req-a"))
			outcomes <- dispatchOutcome{res, err}
		}()
		go func() {
			res, err := r.dispatch(context.Background(), startCmd("req-b"))
			outcomes <- dispatchOutcome{res, err}
		}()
		synctest.Wait()

		ids := send.ids()
		if !ids["req-a"] || !ids["req-b"] {
			t.Fatalf("pushed ids = %v, want both req-a and req-b", ids)
		}
		if n := send.count(); n != 2 {
			t.Fatalf("router pushed %d commands for two distinct ids, want 2", n)
		}

		r.complete(startResult("req-a", "sess-a"))
		r.complete(startResult("req-b", "sess-b"))
		synctest.Wait()

		got := map[string]string{}
		for range 2 {
			o := <-outcomes
			if o.err != nil {
				t.Fatalf("dispatch err = %v, want nil", o.err)
			}
			got[o.result.GetRequestId()] = o.result.GetStart().GetSessionId()
		}
		if got["req-a"] != "sess-a" || got["req-b"] != "sess-b" {
			t.Fatalf("results = %v, want req-a→sess-a and req-b→sess-b (distinct ids correlate independently)", got)
		}
	})
}

// complete() delivers strictly by request id: a result for id X unblocks the X
// caller and leaves a concurrent Y caller still blocked. A bug that completed the
// wrong call (or any waiting call) would unblock Y prematurely.
func TestCompleteCorrelatesByRequestId(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newCommandRouter()
		send := newRecordingSend()
		r.attach(send.send)
		defer r.detach(errStreamClosed)

		outX := make(chan dispatchOutcome, 1)
		outY := make(chan dispatchOutcome, 1)
		ctxY, cancelY := context.WithCancel(context.Background())

		go func() {
			res, err := r.dispatch(context.Background(), startCmd("X"))
			outX <- dispatchOutcome{res, err}
		}()
		go func() {
			res, err := r.dispatch(ctxY, startCmd("Y"))
			outY <- dispatchOutcome{res, err}
		}()
		synctest.Wait() // both durably blocked, both pushed
		if !send.ids()["X"] || !send.ids()["Y"] {
			t.Fatalf("in-flight ids = %v, want X and Y", send.ids())
		}

		// Complete only X.
		r.complete(startResult("X", "sess-x"))
		synctest.Wait()
		ox := <-outX
		if ox.err != nil || ox.result.GetStart().GetSessionId() != "sess-x" {
			t.Fatalf("X outcome = %+v, want sess-x with no error", ox)
		}

		// Y must NOT have completed: nothing is on its channel.
		select {
		case o := <-outY:
			t.Fatalf("Y completed after only X was completed: %+v (complete mis-correlated)", o)
		default:
		}
		// Proof Y is genuinely still blocked in waitCall: cancelling its ctx is
		// the only thing that ends it, with a cancellation error (never a result).
		cancelY()
		synctest.Wait()
		oy := <-outY
		if !errors.Is(oy.err, context.Canceled) {
			t.Fatalf("Y outcome err = %v, want context.Canceled (Y was never completed, only X)", oy.err)
		}
	})
}

// complete() with an unknown id is ignored (no panic, no spurious wakeup): a
// duplicate or already-reaped result must be a no-op, and a live call is
// unaffected.
func TestCompleteUnknownIdIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newCommandRouter()
		send := newRecordingSend()
		r.attach(send.send)
		defer r.detach(errStreamClosed)

		// No in-flight call; completing an unknown id must not panic.
		r.complete(startResult("ghost", "sess-ghost"))

		out := make(chan dispatchOutcome, 1)
		go func() {
			res, err := r.dispatch(context.Background(), startCmd("real"))
			out <- dispatchOutcome{res, err}
		}()
		synctest.Wait()
		r.complete(startResult("real", "sess-real"))
		synctest.Wait()
		o := <-out
		if o.err != nil || o.result.GetStart().GetSessionId() != "sess-real" {
			t.Fatalf("real outcome = %+v, want sess-real", o)
		}
	})
}

// detach() fails every in-flight call with the cause — the OQ6 disconnect path.
// A bug that left calls hanging on a dropped stream would deadlock the caller;
// this proves each pending dispatch returns the detach cause, and a later
// dispatch finds no live stream.
func TestDetachFailsAllInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newCommandRouter()
		send := newRecordingSend()
		r.attach(send.send)
		defer r.detach(errStreamClosed)

		outcomes := make(chan dispatchOutcome, 3)
		for _, id := range []string{"a", "b", "c"} {
			go func() {
				res, err := r.dispatch(context.Background(), startCmd(id))
				outcomes <- dispatchOutcome{res, err}
			}()
		}
		synctest.Wait() // all three durably blocked in flight
		if n := send.count(); n != 3 {
			t.Fatalf("pushed %d commands, want 3 before detach", n)
		}

		cause := errors.New("runner stream dropped")
		r.detach(cause)
		synctest.Wait()

		for range 3 {
			o := <-outcomes
			if o.result != nil {
				t.Fatalf("a detached call returned a result %+v, want the detach error", o.result)
			}
			if !errors.Is(o.err, cause) {
				t.Fatalf("detached call err = %v, want the detach cause", o.err)
			}
		}

		// After detach the send is cleared: a fresh dispatch reports no live
		// stream rather than pushing into a dead one.
		_, err := r.dispatch(context.Background(), startCmd("post-detach"))
		if err == nil {
			t.Fatal("dispatch after detach = nil error, want a no-live-stream error")
		}
	})
}

// dispatch with no request id is rejected before any send — the id is the
// correlation key, so an empty one is a caller bug, not a silent no-op.
func TestDispatchRequiresRequestId(t *testing.T) {
	r := newCommandRouter()
	send := newRecordingSend()
	r.attach(send.send)
	defer r.detach(errStreamClosed)
	_, err := r.dispatch(context.Background(), startCmd(""))
	if err == nil {
		t.Fatal("dispatch with empty request id = nil error, want a required-id error")
	}
	if send.count() != 0 {
		t.Fatalf("router pushed %d commands for an empty id, want 0", send.count())
	}
}

// deliverRefusals is a bounded LRU, not an unbounded set: send1 registering an id
// on every dispatch cannot grow it without limit as lifetime successful delivers
// accumulate (RIG-1610). send1 deliverRefusalsMax+N distinct ids, then assert the
// set is capped, the earliest id is evicted, and eviction is SAFE — a refusal
// complete() for a still-resident id is still observed (RefusedDelivers
// increments), so bounding never breaks refusal observability for the in-flight
// window.
// RED (pre-fix): size the LRU unbounded — temporarily replace deliverRefusalsMax
// in newCommandRouter's expirable.NewLRU(...) with a huge capacity (e.g. 1<<30,
// models the pre-fix unbounded map: never evicts) — the earliest id stays
// present, so the Contains("req-0")==false eviction assertion goes RED.
func TestSend1DeliverRefusalsBounded(t *testing.T) {
	r := newCommandRouter()
	rec := newRecordingSend()
	r.attach(rec.send)
	defer r.detach(errStreamClosed)

	const overflow = 100
	total := deliverRefusalsMax + overflow
	id := func(i int) string { return fmt.Sprintf("req-%d", i) }
	for i := range total {
		cmd := &compassv1internal.SessionsResponse{
			RequestId: id(i),
			Command: &compassv1internal.SessionsResponse_DeliverControl{
				DeliverControl: &compassv1internal.DispatchControl{SessionId: "sess-1"},
			},
		}
		if err := r.send1(cmd); err != nil {
			t.Fatalf("send1(%q) = %v, want nil", id(i), err)
		}
		// Gate on the sender draining this frame before enqueuing the next: the
		// refusal-set bound (deliverRefusalsMax) is independent of the outbound
		// queue bound (sendQueueCap), so keep the queue depth ~0 to isolate the
		// LRU behavior from a queue overflow.
		waitRecorded(t, rec, i+1)
	}

	// The set is bounded, not grown to total. After adding deliverRefusalsMax+overflow
	// distinct ids the LRU holds exactly deliverRefusalsMax entries — an exact check
	// catches an off-by-one in the cap (e.g. keeping Max+1) that a `> Max` bound misses.
	if got := r.deliverRefusals.Len(); got != deliverRefusalsMax {
		t.Fatalf("deliverRefusals.Len() = %d, want exactly %d (bounded)", got, deliverRefusalsMax)
	}
	// The earliest id (added first, never touched since) is the LRU victim evicted
	// past deliverRefusalsMax.
	if r.deliverRefusals.Contains(id(0)) {
		t.Fatalf("earliest id %q still present; the bounded LRU must have evicted it", id(0))
	}

	// Eviction is SAFE: a refusal for a still-resident recent id is still observed.
	resident := id(total - 1)
	if !r.deliverRefusals.Contains(resident) {
		t.Fatalf("newest id %q evicted; it must stay resident under the bound", resident)
	}
	if got := r.RefusedDelivers(); got != 0 {
		t.Fatalf("RefusedDelivers before any refusal = %d, want 0", got)
	}
	r.complete(&compassv1internal.SessionsRequest{
		RequestId: resident,
		Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
			Code: compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_RESOURCE_EXHAUSTED,
		}},
	})
	if got := r.RefusedDelivers(); got != 1 {
		t.Fatalf("RefusedDelivers after a refusal for a resident id = %d, want 1 (bounding must not break observability)", got)
	}
}
