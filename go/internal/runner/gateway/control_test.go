package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// Runner control producer tests: Control stream, ControlSender, retention +
// redelivery.
//
// The record's Red→green clause names seven behaviors; each has a test below.
// They exercise the seam only — the Runner-side callers that DECIDE what to
// send are out of scope.

const testSession = "sess-c3"

// newTestProducer is newControlProducer with the fixture session already
// bound, standing in for the Start that binds it in production. Bind and
// Retire are the whole lifetime of a session's control state, so a producer
// that was never told to bind knows no sessions at all and every path
// correctly refuses — which would make almost every test below a test of the
// refusal rather than of the behavior it names.
//
// A test that is ABOUT the unbound case constructs the producer directly.
func newTestProducer() *controlProducer {
	p := newControlProducer()
	p.Bind(testSession)
	return p
}

// controlStream is the minimal server-stream sink the handler drains into.
// Each Send is recorded so ordering and seq stamping are assertable.
type controlStream struct {
	sent chan *compassv1internal.AgentControl
}

func newControlStream() *controlStream {
	return &controlStream{sent: make(chan *compassv1internal.AgentControl, 64)}
}

func (s *controlStream) Send(op *compassv1internal.AgentControl) error {
	s.sent <- op
	return nil
}

// recv waits for one op, failing the test rather than hanging forever.
func (s *controlStream) recv(t *testing.T) *compassv1internal.AgentControl {
	t.Helper()
	select {
	case op := <-s.sent:
		return op
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a control op on the stream")
		return nil
	}
}

// none asserts no op arrives within a short settle window.
func (s *controlStream) none(t *testing.T, why string) {
	t.Helper()
	select {
	case op := <-s.sent:
		t.Fatalf("%s: unexpected op control_seq=%d", why, op.GetControlSeq())
	case <-time.After(150 * time.Millisecond):
	}
}

// blockingStream wedges inside its first Send until released, modelling a
// stalled peer connection — the state a takeover exists to recover from.
// Holding the drainer inside Send is what lets a test take over while the
// drainer is mid-batch rather than parked.
type blockingStream struct {
	enter   chan struct{}
	release <-chan struct{}
	sends   chan struct{}
	once    sync.Once
}

func newBlockingStream(release <-chan struct{}) *blockingStream {
	return &blockingStream{
		enter:   make(chan struct{}, 1),
		release: release,
		sends:   make(chan struct{}, 64),
	}
}

// Send blocks on the FIRST call only; later calls (the ones a correct drainer
// must never make once displaced) return immediately and are counted.
func (s *blockingStream) Send(*compassv1internal.AgentControl) error {
	s.sends <- struct{}{}
	s.once.Do(func() {
		s.enter <- struct{}{}
		<-s.release
	})
	return nil
}

// entered waits until the drainer is blocked inside its first Send.
func (s *blockingStream) entered(t *testing.T) {
	t.Helper()
	select {
	case <-s.enter:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stream to block inside Send")
	}
}

// sendsBeyond reports how many ops the stream received beyond the first want.
//
// The caller must have already witnessed the drainer's RETURN. Once that
// goroutine is gone nothing can add to the count, so this reads a settled
// total: a drainer that wrongly finished its batch has necessarily written
// those ops before exiting, and they are sitting in the buffer here.
func (s *blockingStream) sendsBeyond(t *testing.T, want int) int {
	t.Helper()
	extra := 0
	for {
		select {
		case <-s.sends:
			extra++
		default:
			if extra < want {
				t.Fatalf("stream received %d ops, want at least %d", extra, want)
			}
			return extra - want
		}
	}
}

// awaitReturn waits for a drainer goroutine to exit, failing loudly rather
// than hanging forever on one that never does.
func awaitReturn(t *testing.T, done <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", why)
	}
}

// eventually polls cond until it holds, failing loudly with the named
// condition on a real timeout. For the cases with no event to gate on — a
// bounded, self-describing wait rather than a blind fixed sleep. The timeout
// is deliberately generous: it exists to turn a hang into a named failure, not
// to measure how quickly the condition arrives on a loaded runner.
func eventually(t *testing.T, why string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatalf("timed out waiting for %s", why)
		}
	}
}

func prompt(text string) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Prompt{
			Prompt: &compassv1internal.PromptControl{Input: text},
		},
	}
}

// TestControlSendStampsSeqAndDelivers is the keystone: an open subscription
// receives a Send-ed op, stamped with a Runner-assigned control_seq.
func TestControlSendStampsSeqAndDelivers(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	if err := p.Send(testSession, prompt("hello")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := stream.recv(t)
	if got.GetControlSeq() == 0 {
		t.Errorf("control_seq = 0, want a Runner-assigned nonzero seq")
	}
	if got.GetPrompt().GetInput() != "hello" {
		t.Errorf("prompt input = %q, want %q", got.GetPrompt().GetInput(), "hello")
	}
}

// TestControlPreservesSendOrder — ops arrive in Send order with monotonically
// increasing seqs. Wire order == apply order is the contract.
func TestControlPreservesSendOrder(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	for _, text := range []string{"one", "two", "three"} {
		if err := p.Send(testSession, prompt(text)); err != nil {
			t.Fatalf("Send(%q): %v", text, err)
		}
	}

	var lastSeq uint64
	for _, want := range []string{"one", "two", "three"} {
		got := stream.recv(t)
		if got.GetPrompt().GetInput() != want {
			t.Fatalf("out of order: got %q, want %q", got.GetPrompt().GetInput(), want)
		}
		if got.GetControlSeq() <= lastSeq {
			t.Fatalf("control_seq not monotonic: %d after %d", got.GetControlSeq(), lastSeq)
		}
		lastSeq = got.GetControlSeq()
	}
}

// TestControlRejectsEmptyVariants pins the must-not-send rule. The four
// undefined-payload variants (SteerControl, DeliverControl, TranscriptReplay, ConfigControl) are
// empty shells on the wire; sending one is a must-not, enforced at the seam
// with CodeInvalidArgument rather than left for the agent to count as unmapped.
func TestControlRejectsEmptyVariants(t *testing.T) {
	empties := map[string]*compassv1internal.AgentControl{
		"steer": {Control: &compassv1internal.AgentControl_Steer{
			Steer: &compassv1internal.SteerControl{}}},
		"deliver": {Control: &compassv1internal.AgentControl_Deliver{
			Deliver: &compassv1internal.DeliverControl{}}},
		"replay": {Control: &compassv1internal.AgentControl_Replay{
			Replay: &compassv1internal.TranscriptReplay{}}},
		"config": {Control: &compassv1internal.AgentControl_Config{
			Config: &compassv1internal.ConfigControl{}}},
	}

	for name, op := range empties {
		t.Run(name, func(t *testing.T) {
			p := newTestProducer()
			stream := newControlStream()
			stop := p.subscribe(t, stream)
			defer stop()

			err := p.Send(testSession, op)
			if err == nil {
				t.Fatalf("Send(empty %s) = nil, want CodeInvalidArgument", name)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("Send(empty %s) code = %v, want %v",
					name, connect.CodeOf(err), connect.CodeInvalidArgument)
			}
			stream.none(t, "rejected op must not reach the stream")
		})
	}

	// The representable variants must still pass — a rejection rule that
	// rejects everything is not a rejection rule.
	t.Run("representable_pass", func(t *testing.T) {
		p := newTestProducer()
		stream := newControlStream()
		stop := p.subscribe(t, stream)
		defer stop()

		for _, op := range []*compassv1internal.AgentControl{
			prompt("live"),
			{Control: &compassv1internal.AgentControl_AskAnswer{
				AskAnswer: &compassv1internal.AskAnswerControl{AskId: "ask-1"}}},
			{Control: &compassv1internal.AgentControl_ReplayComplete{
				ReplayComplete: &compassv1internal.ReplayComplete{}}},
		} {
			if err := p.Send(testSession, op); err != nil {
				t.Fatalf("Send(representable) = %v, want nil", err)
			}
			_ = stream.recv(t)
		}
	})
}

// TestControlTakeoverTransfersUnackedOps pins subscription takeover. A second
// Control subscription cancels the first AND inherits every op the first never
// acked, so a container replacement loses nothing.
func TestControlTakeoverTransfersUnackedOps(t *testing.T) {
	p := newTestProducer()

	first := newControlStream()
	stopFirst := p.subscribe(t, first)
	defer stopFirst()

	if err := p.Send(testSession, prompt("before-takeover")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	carried := first.recv(t) // delivered, but never acked

	second := newControlStream()
	stopSecond := p.subscribe(t, second)
	defer stopSecond()

	// The stale subscription is displaced: a takeover advances the generation,
	// which is how the first drainer learns it has been replaced.
	if gen := p.subscriptionGeneration(testSession); gen != 2 {
		t.Errorf("subscription generation = %d after takeover, want 2 (first displaced)", gen)
	}

	// Unacked ops transfer to the replacement.
	got := second.recv(t)
	if got.GetControlSeq() != carried.GetControlSeq() {
		t.Errorf("transferred seq = %d, want the unacked %d",
			got.GetControlSeq(), carried.GetControlSeq())
	}

	// And subsequent ops go to the new subscription.
	if err := p.Send(testSession, prompt("after-takeover")); err != nil {
		t.Fatalf("Send after takeover: %v", err)
	}
	if next := second.recv(t); next.GetPrompt().GetInput() != "after-takeover" {
		t.Errorf("post-takeover op = %q, want %q", next.GetPrompt().GetInput(), "after-takeover")
	}
}

// TestControlDisplacedDrainerAbandonsBatch — a drainer displaced by a takeover
// must stop MID-BATCH, not finish writing the ops it had already collected.
//
// A takeover does not cancel the old stream's context (the Runner cannot reach
// into a peer's connection), so the stale drainer is still live inside its send
// loop. It collected its batch under the lock before the takeover, so without a
// per-op re-check it would keep writing ops to a stream the Runner has already
// replaced — the same ops the replacement is re-sending from the ack cursor.
// That is a duplicate delivery to a connection the Runner believes is retired.
//
// The wedged-in-Send state modelled here is exactly the one a takeover exists
// to recover from, which is what makes the ordering reachable rather than
// theoretical.
func TestControlDisplacedDrainerAbandonsBatch(t *testing.T) {
	p := newTestProducer()

	// The first stream wedges inside its first Send until released.
	release := make(chan struct{})
	first := newBlockingStream(release)
	firstDone, stopFirst := p.subscribeDone(t, first)
	defer stopFirst()

	// Three ops, so the stale drainer holds a multi-op batch: it is wedged
	// writing #1 with #2 and #3 still queued behind it in the same batch.
	for _, text := range []string{"op1", "op2", "op3"} {
		if err := p.Send(testSession, prompt(text)); err != nil {
			t.Fatalf("Send(%q): %v", text, err)
		}
	}
	first.entered(t) // wedged inside Send for op1

	// Take over while it is wedged.
	second := newControlStream()
	stopSecond := p.subscribe(t, second)
	defer stopSecond()

	// Release the wedged Send. The drainer is now displaced and must abandon
	// ops 2 and 3 rather than writing them to the retired stream.
	close(release)

	// Its return is the completion signal: displaced or not, the drainer exits
	// (a wrongly-continuing one finishes the batch first, then parks on the
	// wake channel the takeover closed and retires at the loop top). Once it is
	// gone the send count can no longer change, so what the stream holds now is
	// the whole truth about what it wrote.
	awaitReturn(t, firstDone, "the displaced drainer to return")

	if n := first.sendsBeyond(t, 1); n != 0 {
		t.Errorf("displaced drainer sent %d more op(s) after being replaced, want 0"+
			" — it must re-check its generation per op, not finish its batch", n)
	}

	// The replacement gets all three from the ack cursor, exactly once each.
	for i, want := range []string{"op1", "op2", "op3"} {
		if got := second.recv(t).GetPrompt().GetInput(); got != want {
			t.Fatalf("replacement op %d = %q, want %q", i+1, got, want)
		}
	}
}

// TestControlRedeliversPastAckCursor — retention semantics. Ops past the
// ControlAck cursor are re-sent on a new subscription; ops at or below it are
// not; and an op named in applied_above is dropped even though it sits past
// the cursor.
func TestControlRedeliversPastAckCursor(t *testing.T) {
	p := newTestProducer()
	first := newControlStream()
	stopFirst := p.subscribe(t, first)

	texts := []string{"op1", "op2", "op3", "op4"}
	seqs := make([]uint64, 0, len(texts))
	for _, text := range texts {
		if err := p.Send(testSession, prompt(text)); err != nil {
			t.Fatalf("Send(%q): %v", text, err)
		}
		seqs = append(seqs, first.recv(t).GetControlSeq())
	}

	// Contiguously applied through op2; op4 applied out of order above it.
	p.AckControl(testSession, seqs[1], []uint64{seqs[3]})
	stopFirst()

	second := newControlStream()
	stopSecond := p.subscribe(t, second)
	defer stopSecond()

	// Only op3 survives retention: op1/op2 are at-or-below the cursor, op4 is
	// individually acked above it.
	got := second.recv(t)
	if got.GetControlSeq() != seqs[2] {
		t.Errorf("redelivered seq = %d, want only the unapplied %d", got.GetControlSeq(), seqs[2])
	}
	second.none(t, "acked ops must not be redelivered")
}

// TestControlReplayBarrierHoldsLiveOps pins the barrier. Live ops queued
// during replay are held until ReleaseReplayBarrier (driven by a
// ReplayCompleteAck routed from the Publish stream).
func TestControlReplayBarrierHoldsLiveOps(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	p.HoldForReplay(testSession)

	if err := p.Send(testSession, prompt("live-during-replay")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stream.none(t, "live op must be held behind the replay barrier")

	p.ReleaseReplayBarrier(testSession)

	if got := stream.recv(t); got.GetPrompt().GetInput() != "live-during-replay" {
		t.Errorf("released op = %q, want %q", got.GetPrompt().GetInput(), "live-during-replay")
	}
}

// TestControlAckBeyondSentDoesNotWedgeSession — an ack the Runner never issued
// a seq for must not advance the cursor past what was actually sent.
//
// The ack arrives from the AGENT, which runs in the container and is the
// untrusted side of this seam. The cursor is what a new subscription starts
// draining from, so a cursor pushed past nextSeq makes every future op invisible
// to every future subscriber: the session wedges permanently and silently, with
// no error surfaced to the Runner or the caller. Clamping keeps the cursor a
// statement about ops that exist.
func TestControlAckBeyondSentDoesNotWedgeSession(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	if err := p.Send(testSession, prompt("first")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stream.recv(t) // seq 1, the only op ever issued

	// The agent acks a seq that was never assigned.
	p.AckControl(testSession, 100, nil)

	if err := p.Send(testSession, prompt("after-bogus-ack")); err != nil {
		t.Fatalf("Send after out-of-range ack: %v", err)
	}

	// Both the live subscription and a fresh one must still see it.
	if got := stream.recv(t); got.GetPrompt().GetInput() != "after-bogus-ack" {
		t.Errorf("live subscription got %q, want %q",
			got.GetPrompt().GetInput(), "after-bogus-ack")
	}

	next := newControlStream()
	stopNext := p.subscribe(t, next)
	defer stopNext()
	if got := next.recv(t); got.GetPrompt().GetInput() != "after-bogus-ack" {
		t.Errorf("new subscription got %q, want the unacked %q",
			got.GetPrompt().GetInput(), "after-bogus-ack")
	}
}

// TestControlSendNoAgent — ErrNoAgent when no subscription is live and the
// caller opts out of retention.
func TestControlSendNoAgent(t *testing.T) {
	p := newTestProducer()

	err := p.SendIfLive(testSession, prompt("nobody-home"))
	if !errors.Is(err, ErrNoAgent) {
		t.Errorf("SendIfLive with no subscription = %v, want ErrNoAgent", err)
	}
}

// subscribe opens a Control subscription in the background and returns a stop
// func. Test helper only — mirrors what the connect handler does per stream.
//
// It waits for THIS subscription's generation, not merely for "some
// subscription exists": on a takeover the previous generation is already
// nonzero, so a bare nonzero check returns before the new drainer has bound
// and the test races the goroutine it just started.
func (p *controlProducer) subscribe(t *testing.T, s *controlStream) func() {
	t.Helper()
	return p.subscribeSink(t, s)
}

// subscribeSink is subscribe for any sink — used by tests that need a stream
// which blocks inside Send rather than recording into a buffer. It is
// subscribeDone for the callers that do not need to witness the drainer's
// return, so both paths bind through one registration wait.
func (p *controlProducer) subscribeSink(t *testing.T, s controlSink) func() {
	t.Helper()
	_, stop := p.subscribeDone(t, s)
	return stop
}

// TestControlSendClonesCallerMessage pins that the producer owns what it
// retains. Send stamps a Runner-assigned control_seq; if it stamped the
// CALLER'S message the caller would see its own op mutated, and a caller that
// reuses one pointer for two Sends would put two retention entries behind a
// single message carrying only the last seq — two ops on the wire under one
// seq, which the agent's seq-dedup then collapses, losing an op.
func TestControlSendClonesCallerMessage(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	// One pointer, sent twice — the shape a lifecycle caller reusing a buffer
	// produces.
	op := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Prompt{
			Prompt: &compassv1internal.PromptControl{Input: "reused"},
		},
	}
	if err := p.Send(testSession, op); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := p.Send(testSession, op); err != nil {
		t.Fatalf("second Send: %v", err)
	}

	first, second := stream.recv(t), stream.recv(t)
	if first.GetControlSeq() == second.GetControlSeq() {
		t.Fatalf("both ops carry control_seq=%d: the caller's message was stamped in place, not cloned",
			first.GetControlSeq())
	}
	if first.GetControlSeq() != 1 || second.GetControlSeq() != 2 {
		t.Fatalf("want ascending seqs 1,2; got %d,%d", first.GetControlSeq(), second.GetControlSeq())
	}
	if op.GetControlSeq() != 0 {
		t.Fatalf("caller's message was mutated: control_seq=%d, want 0 (untouched)", op.GetControlSeq())
	}
}

// TestControlTwoStaleDrainersDoNotStealWake is the N=2 case the single-stale
// test cannot reach. The bind-time signal posts exactly ONE token, so it can
// retire at most one lingering drainer. A second stale drainer that parks while
// the live drainer is still busy inside Send lands AHEAD of it in the wake
// channel's receive queue; the next token goes to the stale drainer, which sees
// its generation changed and returns without re-signalling. The wake
// evaporates, the live drainer stays parked, and the op sits in retention
// undelivered — an agent that silently stops receiving control ops.
func TestControlTwoStaleDrainersDoNotStealWake(t *testing.T) {
	p := newTestProducer()

	// The park ORDER is this test's whole premise, so it must be an observed
	// event, not an elapsed interval. onCycle fires on the drainer's own
	// goroutine immediately before it parks, so counting invocations tracks
	// drainers arriving at the park boundary. Installed before any
	// subscription binds, and through setOnCycle so the write is under the
	// same lock the drainer's capture takes.
	var (
		parkMu   sync.Mutex
		preParks int
	)
	p.setOnCycle(func(int) {
		parkMu.Lock()
		preParks++
		parkMu.Unlock()
	})
	parksSoFar := func() int {
		parkMu.Lock()
		defer parkMu.Unlock()
		return preParks
	}
	// awaitPark waits for one more drainer to reach the boundary than `was`.
	// Sound only because the test keeps every OTHER drainer either wedged
	// inside Send or already returned at each call site, so the increment can
	// only be the one drainer named.
	awaitPark := func(was int, who string) {
		t.Helper()
		eventually(t, who, func() bool { return parksSoFar() > was })
	}

	// gen1 wedges inside Send — a stalled peer, the case a takeover exists to
	// recover from — so it survives the takeover instead of retiring. While
	// wedged it cannot reach the park boundary, so it adds no counter noise.
	release1 := make(chan struct{})
	wedged := newBlockingStream(release1)
	stop1 := p.subscribeSink(t, wedged)
	defer stop1()
	if err := p.Send(testSession, promptOp("wedge")); err != nil {
		t.Fatalf("Send to wedge gen1: %v", err)
	}
	wedged.entered(t)

	// gen2 parks, and is retired by gen3's bind-time token — spending the one
	// token that could otherwise have retired gen1.
	parked := newControlStream()
	gen2Done, stop2 := p.subscribeDone(t, parked)
	defer stop2()

	// gen3 is the live subscription, on a gated sink so the test can hold it
	// INSIDE Send at the moment gen1 parks.
	live := newGatedStream()
	stop3 := p.subscribeSink(t, live)
	// Order matters: release the gate FIRST, so cancelling the subscription can
	// retire a drainer that would otherwise be parked inside Send.
	defer func() {
		live.close()
		stop3()
	}()

	// gen2's retirement is the step this test's premise spends the single
	// bind-time token on, so wait for its drainer to actually return. That both
	// asserts the step and silences it: a returned drainer can no longer reach
	// the park boundary, so every later increment belongs to gen1 or gen3.
	awaitReturn(t, gen2Done, "gen2 to be retired by gen3's bind-time token")

	// gen3 binds at the ack cursor, so it is owed every retained op: "wedge"
	// (never acked by gen1) and then "hold-live". Let the first through and
	// hold it inside the second, so it is BUSY at the moment gen1 parks.
	if err := p.Send(testSession, promptOp("hold-live")); err != nil {
		t.Fatalf("Send to occupy gen3: %v", err)
	}
	live.entered(t)
	live.release(t)
	if in := live.recv(t).GetPrompt().GetInput(); in != "wedge" {
		t.Fatalf("gen3's first owed op = %q, want \"wedge\"", in)
	}
	live.entered(t)

	// gen3 is busy inside Send, so releasing gen1 now makes it reach the park
	// boundary FIRST — at the head of the wake channel's receive queue, ahead
	// of the live drainer. Waiting for its pre-park signal is what fixes that
	// order; without it the ordering is a timing guess and the test misses the
	// lost-wakeup bug it names whenever gen1 happens to be scheduled late.
	before1 := parksSoFar()
	close(release1)
	awaitPark(before1, "the stale drainer to reach its park")

	// gen3 finishes "hold-live", finds nothing new, and parks SECOND.
	before3 := parksSoFar()
	live.release(t)
	if in := live.recv(t).GetPrompt().GetInput(); in != "hold-live" {
		t.Fatalf("gen3's second owed op = %q, want \"hold-live\"", in)
	}
	awaitPark(before3, "the live drainer to reach its park")

	// One token, two parked drainers, stale at the head of the queue. If the
	// stale drainer absorbs it, this op never reaches the live subscription.
	if err := p.Send(testSession, promptOp("after-takeover")); err != nil {
		t.Fatalf("Send after takeover: %v", err)
	}
	live.entered(t)
	live.release(t)
	if in := live.recv(t).GetPrompt().GetInput(); in != "after-takeover" {
		t.Fatalf("live subscription got %q, want \"after-takeover\"", in)
	}
}

// TestControlReplayCompletePassesBarrier pins the barrier against the record:
// "Replay frames first; live ops held until the agent's replay ack arrives."
// The release is driven by the agent's ReplayCompleteAck, and the agent emits
// that only after RECEIVING replay_complete — so holding replay_complete behind
// the barrier holds the one op whose ack is the only thing that can release it,
// and the session's control stream is dead for good.
func TestControlReplayCompletePassesBarrier(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	p.HoldForReplay(testSession)

	// A live op is correctly held.
	if err := p.Send(testSession, promptOp("live")); err != nil {
		t.Fatalf("Send live op: %v", err)
	}

	// replay_complete is replay-path traffic: it must reach the agent so the
	// ack can come back.
	rc := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_ReplayComplete{
			ReplayComplete: &compassv1internal.ReplayComplete{},
		},
	}
	if err := p.Send(testSession, rc); err != nil {
		t.Fatalf("Send replay_complete: %v", err)
	}

	got := stream.recv(t)
	if got.GetReplayComplete() == nil {
		t.Fatalf("first delivered op is %T, want replay_complete: the barrier is holding its own release",
			got.GetControl())
	}

	// The live op stays held until the ack releases it, and then arrives —
	// asserted positively (ordering), not by a settle window.
	p.ReleaseReplayBarrier(testSession)
	if in := stream.recv(t).GetPrompt().GetInput(); in != "live" {
		t.Fatalf("after release got %q, want the held live op", in)
	}
}

// TestControlSendIfLiveAfterSubscriptionEnds pins ErrNoAgent to LIVENESS, not
// to "a subscription has ever existed". The generation counter only ever
// increases, so a liveness check written against it stops working the moment
// the first agent disconnects — which is precisely when a caller that would
// rather fail than queue wants to fail fast.
func TestControlSendIfLiveAfterSubscriptionEnds(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	stop() // agent disconnects

	err := p.SendIfLive(testSession, promptOp("no-agent"))
	if !errors.Is(err, ErrNoAgent) {
		t.Fatalf("SendIfLive after the subscription ended = %v, want ErrNoAgent", err)
	}
}

// The other half of the same gate, which no test reached while the method was
// named for retention: when a subscription IS live, SendIfLive retains exactly
// as Send does. The name says liveness and only liveness — the agent was
// listening, so the op was genuinely handed over, and at-least-once still owes
// it redelivery if that agent dies before acking.
//
// RED if the gate is ever "fixed" into a retention opt-out: the op would be
// dropped instead of redelivered here.
func TestControlSendIfLiveRetainsWhileLive(t *testing.T) {
	p := newTestProducer()
	first := newControlStream()
	stopFirst := p.subscribe(t, first)

	if err := p.SendIfLive(testSession, promptOp("handed-over")); err != nil {
		t.Fatalf("SendIfLive with a live subscription = %v, want nil", err)
	}
	// Delivery gates the disconnect: the op is on the wire, unacked.
	if in := first.recv(t).GetPrompt().GetInput(); in != "handed-over" {
		t.Fatalf("delivered %q, want the op just sent", in)
	}
	stopFirst() // the agent dies without acking

	second := newControlStream()
	stopSecond := p.subscribe(t, second)
	defer stopSecond()

	if in := second.recv(t).GetPrompt().GetInput(); in != "handed-over" {
		t.Errorf("a new subscription got %q, want the unacked op redelivered: "+
			"SendIfLive gates on liveness, it does not opt out of retention", in)
	}
}

// TestControlRetainsWithoutSubscription is the retention clause the suite never
// executed: Send succeeds with NO agent bound, and the ops are delivered, in
// order, to a subscription that arrives later. This is why Send and
// SendIfLive are two methods.
func TestControlRetainsWithoutSubscription(t *testing.T) {
	p := newTestProducer()

	if err := p.Send(testSession, promptOp("first")); err != nil {
		t.Fatalf("Send with no subscription: %v", err)
	}
	if err := p.Send(testSession, promptOp("second")); err != nil {
		t.Fatalf("Send with no subscription: %v", err)
	}

	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	for i, want := range []string{"first", "second"} {
		got := stream.recv(t)
		if got.GetPrompt().GetInput() != want {
			t.Fatalf("op %d = %q, want %q", i, got.GetPrompt().GetInput(), want)
		}
		if got.GetControlSeq() != uint64(i+1) {
			t.Fatalf("op %d control_seq = %d, want %d", i, got.GetControlSeq(), i+1)
		}
	}
}

// promptOp is a representable control op with an identifiable payload.
func promptOp(input string) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Prompt{
			Prompt: &compassv1internal.PromptControl{Input: input},
		},
	}
}

// gatedStream releases one Send at a time, so a test can hold a drainer BUSY at
// a chosen moment — the only way to control which drainer parks first, and
// therefore its position in the wake channel's receive queue.
type gatedStream struct {
	entry  chan struct{}
	gate   chan struct{}
	sent   chan *compassv1internal.AgentControl
	closed chan struct{}
	once   sync.Once
}

// errGatedStreamClosed ends a drainer parked in a gated Send at teardown.
var errGatedStreamClosed = errors.New("gated stream closed")

func newGatedStream() *gatedStream {
	return &gatedStream{
		entry:  make(chan struct{}, 64),
		gate:   make(chan struct{}, 64),
		sent:   make(chan *compassv1internal.AgentControl, 64),
		closed: make(chan struct{}),
	}
}

func (s *gatedStream) Send(op *compassv1internal.AgentControl) error {
	s.entry <- struct{}{}
	select {
	case <-s.gate:
	case <-s.closed:
		// Teardown: never hold a drainer that the test is done with, or the
		// subscription's cleanup blocks forever on a Send nothing will release.
		return errGatedStreamClosed
	}
	s.sent <- op
	return nil
}

// close releases every current and future Send, so cancelling the subscription
// can actually retire its drainer.
func (s *gatedStream) close() {
	s.once.Do(func() { close(s.closed) })
}

// entered waits until the drainer is blocked inside a Send.
func (s *gatedStream) entered(t *testing.T) {
	t.Helper()
	select {
	case <-s.entry:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the gated stream to block inside Send")
	}
}

// release lets one blocked Send complete.
func (s *gatedStream) release(t *testing.T) {
	t.Helper()
	select {
	case s.gate <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out releasing a gated Send")
	}
}

// recv waits for one op to complete its Send.
func (s *gatedStream) recv(t *testing.T) *compassv1internal.AgentControl {
	t.Helper()
	select {
	case op := <-s.sent:
		return op
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a control op on the gated stream")
		return nil
	}
}

// TestControlRetentionCapRejectsSend pins the bound on retention. An agent that
// never acks would otherwise grow the Runner's memory without limit, so past
// the cap Send FAILS rather than silently evicting: an evicted op was already
// reported "queued until acked", and dropping it would break that promise with
// nobody told. ResourceExhausted tells the caller to back off instead.
func TestControlRetentionCapRejectsSend(t *testing.T) {
	p := newTestProducer()

	// No subscription: nothing drains, so every op stays retained.
	for i := range maxRetainedOps {
		if err := p.Send(testSession, promptOp("fill")); err != nil {
			t.Fatalf("Send %d of %d: %v", i+1, maxRetainedOps, err)
		}
	}

	err := p.Send(testSession, promptOp("overflow"))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("Send past the cap = %v (code %v), want CodeResourceExhausted",
			err, connect.CodeOf(err))
	}

	// The rejected op must not have consumed a seq or entered retention —
	// a rejected Send is a no-op, not a half-applied one.
	s, ok := p.existingSession(testSession)
	if !ok {
		t.Fatal("the fixture bound this session, so it must resolve")
	}
	s.mu.Lock()
	gotSeq, gotLen := s.nextSeq, len(s.ops)
	s.mu.Unlock()
	if gotSeq != uint64(maxRetainedOps) {
		t.Fatalf("nextSeq = %d after a rejected Send, want %d", gotSeq, maxRetainedOps)
	}
	if gotLen != maxRetainedOps {
		t.Fatalf("retention holds %d ops after a rejected Send, want %d", gotLen, maxRetainedOps)
	}

	// Acking frees room, so the cap is backpressure and not a permanent wedge.
	p.AckControl(testSession, uint64(maxRetainedOps), nil)
	if err := p.Send(testSession, promptOp("after-ack")); err != nil {
		t.Fatalf("Send after the ack freed room: %v", err)
	}
}

// TestControlReplayCompleteSurvivesRetentionCap pins the one interaction that
// turns the retention cap from backpressure into a permanent wedge.
//
// errRetentionFull documents the cap as "backpressure, not a wedge: an ack
// frees room immediately". That promise assumes an ack CAN arrive. Behind a
// raised replay barrier it cannot: live ops retain but are never drained, so
// the agent holds nothing to ack. Retention therefore fills to the cap with the
// agent having received ZERO ops, and the cap then rejects the one op whose
// delivery is the only thing that can lift the barrier — replay_complete. The
// agent never receives it, never emits ReplayCompleteAck, nothing ever calls
// ReleaseReplayBarrier, and no ack can ever free room. The session's control
// stream is dead, permanently and silently, with no error left anywhere.
//
// replayPath ops already bypass the BARRIER for exactly this reason; they must
// bypass the CAP for the same one. The carve-out is for the release path only,
// so this asserts both halves: replay_complete gets through, and live traffic
// keeps getting rejected. A "fix" that simply removed the cap would satisfy the
// first and fail the second.
func TestControlReplayCompleteSurvivesRetentionCap(t *testing.T) {
	p := newTestProducer()
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	p.HoldForReplay(testSession)

	// Fill retention to the cap with live ops. The barrier holds every one of
	// them, so nothing drains and the agent receives none — which is precisely
	// why no ack can arrive to free room.
	for i := range maxRetainedOps {
		if err := p.Send(testSession, promptOp("held-live")); err != nil {
			t.Fatalf("Send live op %d of %d: %v", i+1, maxRetainedOps, err)
		}
	}

	// The cap still binds live traffic — backpressure must survive the fix.
	if err := p.Send(testSession, promptOp("overflow")); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("live Send past the cap = %v (code %v), want CodeResourceExhausted"+
			" — the cap must keep rejecting live traffic",
			err, connect.CodeOf(err))
	}

	// The release path must stay reachable. replay_complete is the op whose ack
	// lifts the barrier, so a cap that rejects it is holding its own release.
	rc := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_ReplayComplete{
			ReplayComplete: &compassv1internal.ReplayComplete{},
		},
	}
	if err := p.Send(testSession, rc); err != nil {
		t.Fatalf("Send(replay_complete) at the retention cap = %v, want nil"+
			" — the cap is rejecting the barrier's own release, and with the agent"+
			" holding zero ops no ack can ever free room: the session is wedged for good", err)
	}

	// Rejecting live traffic is still the rule after the carve-out fired: the
	// exemption is for the replay path, not a removal of the cap.
	if err := p.Send(testSession, promptOp("overflow-again")); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("live Send after the replay-path carve-out = %v (code %v), want CodeResourceExhausted"+
			" — the carve-out must exempt the replay path, not disable backpressure",
			err, connect.CodeOf(err))
	}

	// End to end: the agent actually RECEIVES replay_complete, which is what
	// makes the ack — and therefore the release — possible at all.
	got := stream.recv(t)
	if got.GetReplayComplete() == nil {
		t.Fatalf("first delivered op is %T, want replay_complete: the cap is holding the barrier's release",
			got.GetControl())
	}

	// So the ack releases the barrier and the retained live ops finally drain,
	// in seq order, with nothing lost and nothing duplicated.
	p.ReleaseReplayBarrier(testSession)
	for i := range maxRetainedOps {
		op := stream.recv(t)
		if op.GetPrompt() == nil {
			t.Fatalf("drained op %d is %T, want a held live prompt", i+1, op.GetControl())
		}
		if op.GetControlSeq() != uint64(i+1) {
			t.Fatalf("drained op %d has control_seq %d, want %d", i+1, op.GetControlSeq(), i+1)
		}
	}
	stream.none(t, "every retained op must drain exactly once after the barrier lifts")
}

// TestControlAckOnlyRouterRefusesSubscription pins Gateway.Control's lane check.
// A Gateway carries the ack-only no-op router from construction until the real
// producer is injected (NewGateway's default; Serve replaces it), so a Control
// subscription arriving in that window reaches a router that can route acks but
// carries no drain. Accepting it would hand the agent a stream nothing can ever
// write to — a subscription that looks live and is silently dead — so the
// handler refuses honestly: CodeUnimplemented, errNoControlLane.
//
// The session must be BOUND first. The session check runs before the lane check,
// so an unbound container returns CodePermissionDenied and never reaches the
// branch under test — a green here with no session bound would prove nothing.
//
// The sentinel is asserted alongside the code because the code alone is not
// specific: the generated UnimplementedAgentGatewayHandler that Gateway embeds
// also returns CodeUnimplemented, so a code-only assertion would stay green
// against a Gateway whose Control was never wired at all.
func TestControlAckOnlyRouterRefusesSubscription(t *testing.T) {
	// No SetControlRouter: the Gateway keeps the noopControlRouter default, which
	// routes acks and carries no drain. The nil stream is load-bearing — the
	// refusal must fire before anything is written, so a handler that touched the
	// stream first would panic here rather than pass.
	g := NewGateway(context.Background(), "cnt-A", boundSessions(), nil, nil)

	err := g.Control(context.Background(),
		connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}), nil)
	if err == nil {
		t.Fatal("Control against an ack-only router = nil, want CodeUnimplemented (a stream nothing can write to must be refused, not accepted)")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("ack-only refusal code = %v, want %v", got, connect.CodeUnimplemented)
	}
	if !errors.Is(err, errNoControlLane) {
		t.Fatalf("ack-only refusal = %v, want it to wrap errNoControlLane", err)
	}
}

// sessionCount reports how many sessions the producer is holding state for.
// The map IS the contract under test here: Retire exists because `sessions`
// was create-only, so a Runner reusing one container across Stop/Start — a
// fresh session id per cycle — accumulated one controlSession per cycle for
// the life of the process, each pinning up to maxRetainedOps retained ops.
// Nothing on the public surface can observe an entry that is merely leaked, so
// this counts it directly.
func (p *controlProducer) sessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

// nullSink is a subscription that can never block or fill: it accepts every op
// and keeps none. Used by the concurrency stress below, where a bounded
// recording buffer would wedge the drainer rather than exercise the seam.
type nullSink struct{}

func (nullSink) Send(*compassv1internal.AgentControl) error { return nil }

// subscribeDone binds a subscription like subscribeSink but hands back the
// drainer's done channel alongside the stop func.
//
// Retire's contract includes "the live drainer RETURNS", and the stop func
// cannot witness that: it cancels the context, which unparks the drainer all
// by itself, so a test built on stop() alone stays green against a Retire that
// never touched the wake channel. Waiting on done with Retire as the only
// stimulus is what makes the assertion real.
func (p *controlProducer) subscribeDone(t *testing.T, sink controlSink) (<-chan struct{}, func()) {
	t.Helper()
	want := p.subscriptionGeneration(testSession) + 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.serve(ctx, testSession, sink)
	}()
	eventually(t, fmt.Sprintf("subscription generation %d to register", want), func() bool {
		return p.subscriptionGeneration(testSession) >= want
	})
	return done, func() {
		cancel()
		<-done
	}
}

// TestControlRetireReclaimsSessionState is the load-bearing one: Retire must
// actually drop the session, not merely quiesce it.
//
// The defect was unbounded map growth, so the entry going away is asserted
// directly. The seq restart is the half that survives a partial fix: Retire
// also clears `ops`, so a Retire that quiesced the session but left the entry
// behind would still deliver nothing stale — yet nextSeq would carry over and
// the reused id would keep counting from the retired session's high-water
// mark. A fresh session id (which is what the Runner mints each Stop/Start
// cycle) starting at control_seq 1 is the observable proof the state is gone.
func TestControlRetireReclaimsSessionState(t *testing.T) {
	p := newTestProducer()

	if err := p.Send(testSession, promptOp("before-retire")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := p.sessionCount(); got != 1 {
		t.Fatalf("sessions after Send = %d, want 1 (the fixture must actually create state to reclaim)", got)
	}

	p.Retire(testSession)

	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after Retire = %d, want 0: the retired session's state is still pinned, which is the unbounded growth Retire exists to stop", got)
	}

	// A reused id is a fresh Start, so the lifecycle binds it again.
	p.Bind(testSession)
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()
	if err := p.Send(testSession, promptOp("after-retire")); err != nil {
		t.Fatalf("Send after Retire: %v", err)
	}

	got := stream.recv(t)
	if input := got.GetPrompt().GetInput(); input != "after-retire" {
		t.Fatalf("first op after Retire = %q, want %q: the retired session's retained ops must not be redelivered", input, "after-retire")
	}
	if got.GetControlSeq() != 1 {
		t.Fatalf("control_seq after Retire = %d, want 1: the reused id must start a NEW session, not resume the retired one's seq space", got.GetControlSeq())
	}
	stream.none(t, "nothing from the retired session may survive into its replacement")
}

// TestControlRetireIsIdempotent pins Retire to Stop's semantics: retiring an
// unknown or already-retired id is a no-op, not a panic and not a resurrection.
//
// The lifecycle calls this on teardown paths that can run twice (an explicit
// Stop racing a container exit), and the second call arrives with the wake
// channel already closed — a close of it again would take the Runner down.
// "Unknown id creates nothing" is asserted too: Retire must reclaim an entry,
// never mint one for a bogus id.
func TestControlRetireIsIdempotent(t *testing.T) {
	p := newControlProducer() // asserts a count of 0, so nothing may be bound yet

	p.Retire("never-seen")
	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after retiring an unknown id = %d, want 0: Retire must not create the state it is meant to drop", got)
	}

	p.Bind(testSession) // the lifecycle's Start
	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()
	if err := p.Send(testSession, promptOp("live")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stream.recv(t)

	// Twice, with a subscription live at the first call: the second must not
	// re-close the wake channel the first one closed.
	p.Retire(testSession)
	p.Retire(testSession)

	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after a double Retire = %d, want 0", got)
	}
	// A twice-retired id is freshly usable — the next Start binds it again and
	// the double Retire left nothing behind to obstruct that.
	p.Bind(testSession)
	if err := p.Send(testSession, promptOp("reused")); err != nil {
		t.Fatalf("Send on a twice-retired id = %v, want the id to be freshly usable", err)
	}
}

// TestControlRetireRetiresLiveDrainer pins the teardown half of the contract:
// retiring a session with a subscription bound must unpark that drainer and
// let it RETURN, then leave the id immediately re-usable.
//
// A drainer parked on its wake channel has no other way to learn its session is
// gone — the Runner cannot cancel a peer's stream context — so a Retire that
// dropped the map entry without closing the channel would strand the goroutine
// for the life of the process, which is the same leak in a different shape.
// The reverse mistake is just as bad: waking it without advancing the
// generation leaves it looping against a closed channel, writing ops to a
// stream whose session no longer exists.
func TestControlRetireRetiresLiveDrainer(t *testing.T) {
	p := newTestProducer()
	retired := newControlStream()
	done, stop := p.subscribeDone(t, retired)
	defer stop()

	if err := p.Send(testSession, promptOp("pre")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if input := retired.recv(t).GetPrompt().GetInput(); input != "pre" {
		t.Fatalf("pre-Retire op = %q, want %q", input, "pre")
	}

	p.Retire(testSession)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainer did not return within 2s of Retire: closing the wake channel is the only thing that can unpark it, so it is stranded for the process lifetime")
	}

	// Rebinding must not trip over the channel Retire closed, and the ops of
	// the new session must reach the new stream only. The reused id is a fresh
	// Start, so the lifecycle binds it again.
	p.Bind(testSession)
	replacement := newControlStream()
	stopReplacement := p.subscribe(t, replacement)
	defer stopReplacement()
	if err := p.Send(testSession, promptOp("post")); err != nil {
		t.Fatalf("Send after Retire: %v", err)
	}
	if input := replacement.recv(t).GetPrompt().GetInput(); input != "post" {
		t.Fatalf("replacement subscription got %q, want %q", input, "post")
	}
	retired.none(t, "the retired subscription must receive nothing after Retire")
}

// TestControlRetireRacesSendWithoutPanicking covers the window Retire's
// `s.wake = nil` exists to close.
//
// send() resolves the session under the producer lock and only then takes the
// session lock, so a Send can hold a pointer to a session Retire is
// concurrently tearing down: it blocks on s.mu, Retire closes wake and
// releases, and the Send proceeds into signalLocked. Without the nil-out that
// is a send on a closed channel — a panic that kills the Runner, not one
// control op — and it is reachable exactly when the lifecycle is doing its
// normal thing, retiring a session while the session's own goroutines are
// still writing to it. Rounds rebind a subscription each time because the
// window only exists while a wake channel is installed.
func TestControlRetireRacesSendWithoutPanicking(t *testing.T) {
	const (
		rounds  = 30
		senders = 4
		perSend = 25
	)
	p := newTestProducer()

	for range rounds {
		p.Bind(testSession) // each round is a fresh Start
		stop := p.subscribeSink(t, nullSink{})
		var wg sync.WaitGroup
		for range senders {
			wg.Go(func() {
				for range perSend {
					_ = p.Send(testSession, promptOp("racing"))
				}
			})
		}
		p.Retire(testSession)
		wg.Wait()
		stop()
		p.Retire(testSession)
	}

	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after the final Retire = %d, want 0", got)
	}
}

// A watermark JUMP must not strand entries below the new mark. The drain loop
// advances `sent` to the ack cursor when the untrusted agent acks a seq it
// never received; every entry `above` already recorded at or below that cursor
// is then unreachable, because the contiguous walk only ever steps upward from
// sent+1. Those entries are delivered ops the agent can never ask for again,
// so retaining them is pure leak — and the guard that triggers the jump does
// not re-fire once the watermark catches the cursor, so nothing later clears
// them either.
//
// `above` is drainer-local, so the strand reaches no delivery assertion: a
// test watching only the stream passes either way. controlSession publishes
// the set's size for that reason — this pins the helper, and
// TestControlAckJumpDropsStrandedSeqs below pins the drain loop calling it.
//
// RED against absorbContiguous: 1, 2 and 3 sit at or below the new watermark
// of 5 and survive forever.
func TestAbsorbJumpDropsStrandedEntries(t *testing.T) {
	above := map[uint64]struct{}{1: {}, 2: {}, 3: {}, 6: {}, 7: {}, 9: {}}

	// Jump the watermark to 5: 1/2/3 fall at or below it, and 6/7 are the
	// contiguous run above it the walk must still absorb.
	sent := absorbJump(5, above)

	if sent != 7 {
		t.Errorf("watermark = %d, want 7 (the contiguous run 6,7 absorbed)", sent)
	}
	// Only 9 survives: it is genuinely out of order, past the gap at 8, so a
	// later delivery can still fill it.
	if len(above) != 1 {
		t.Errorf("above = %v, want only {9}: entries at or below the jump are stranded and must be dropped", above)
	}
	if _, ok := above[9]; !ok {
		t.Errorf("above = %v, want 9 retained — it sits past the gap at 8 and may still be filled", above)
	}
}

// The asymmetry is deliberate, so it is pinned too: absorbContiguous must NOT
// sweep. It runs once per delivered op on the steady-state path, where `from`
// is contiguous by construction and nothing can be stranded; folding the
// O(len(above)) sweep in would make delivery O(n) per op to fix a case that
// path cannot produce. A "simplification" that collapses the two into one
// sweeping helper reddens this.
func TestAbsorbContiguousDoesNotSweep(t *testing.T) {
	// 2 sits below `from` — unreachable, but not this function's job to drop.
	above := map[uint64]struct{}{2: {}, 5: {}}

	sent := absorbContiguous(4, above)

	if sent != 5 {
		t.Errorf("watermark = %d, want 5 (the contiguous step to 5)", sent)
	}
	if _, ok := above[2]; !ok {
		t.Errorf("above = %v, want 2 retained: the contiguous path must stay O(1) in the set size", above)
	}
}

// The drain loop's JUMP site must call absorbJump. The helper being correct is
// not enough: it is a plain function, and Go neither fails the build nor lints
// on one that is defined, documented and never called — which is exactly the
// state this fix was found in. So this pins the CALLSITE, driving a real
// watermark jump through the drain and reading the set the drainer actually
// holds.
//
// RED with absorbContiguous at the jump site: the stranded seq stays in the
// drainer's set for the life of the subscription, so every later cycle reports
// it and the last observation is nonzero.
func TestControlAckJumpDropsStrandedSeqs(t *testing.T) {
	p := newTestProducer()

	// Installed before the subscription binds, which is the only safe point:
	// the hook runs on the drainer goroutine, so the mutex guards the handoff
	// of what it saw back to this one.
	var (
		aboveMu   sync.Mutex
		lastAbove int
	)
	p.setOnCycle(func(aboveLen int) {
		aboveMu.Lock()
		lastAbove = aboveLen
		aboveMu.Unlock()
	})

	stream := newControlStream()
	stop := p.subscribe(t, stream)
	defer stop()

	// The barrier is what produces an out-of-order delivery: live ops are held
	// while a replay-path op behind them drains, so the replay seq is recorded
	// ABOVE a watermark still sitting at 0 — the entry a jump then strands.
	p.HoldForReplay(testSession)
	for range 3 {
		if err := p.Send(testSession, promptOp("live")); err != nil {
			t.Fatalf("Send live: %v", err)
		}
	}
	rc := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_ReplayComplete{
			ReplayComplete: &compassv1internal.ReplayComplete{},
		},
	}
	if err := p.Send(testSession, rc); err != nil {
		t.Fatalf("Send replay_complete: %v", err)
	}
	// Receiving it IS the gate: the drainer has recorded seq 4 above the
	// watermark by the time this returns, so nothing below waits on a clock.
	if got := stream.recv(t).GetControlSeq(); got != 4 {
		t.Fatalf("drained seq = %d, want the replay op at 4 through the barrier", got)
	}

	// The agent acks through 4, including the three live ops it never
	// received — the untrusted case AckControl documents. That prunes them, so
	// the gap at 1 can never be filled by a delivery and the watermark can
	// only advance by the jump, stranding the recorded 4 beneath it.
	p.AckControl(testSession, 4, nil)
	p.ReleaseReplayBarrier(testSession)

	// Drive one more op through. Its delivery gates the drain: the loop ran
	// its jump and sent before recv returns.
	if err := p.Send(testSession, promptOp("after-jump")); err != nil {
		t.Fatalf("Send after jump: %v", err)
	}
	if got := stream.recv(t).GetControlSeq(); got != 5 {
		t.Fatalf("drained seq = %d, want 5 delivered after the jump", got)
	}
	// This settles the drain: no further op may issue, so the drainer has
	// reached its park — and the hook fires immediately before that park, so
	// the observation read below is the quiesced set, not a mid-batch
	// transient.
	stream.none(t, "no op may be redelivered after the watermark jump")

	aboveMu.Lock()
	got := lastAbove
	aboveMu.Unlock()
	if got != 0 {
		t.Errorf("drainer still holds %d out-of-order seq(s) after the watermark jumped past them; "+
			"they are unreachable by the upward walk and the jump guard does not re-fire, so this is the unbounded leak", got)
	}
}

// TestControlServeRefusesRetiredSessionInResolveWindow covers the
// serve()-vs-Retire() TOCTOU (SEA-1550): serve resolves the session under p.mu
// and releases it, then takes s.mu ~13 lines later to bind. A full Retire that
// lands in that window deletes the session, marks it dead, and closes its wake.
// Without a re-check, serve then binds the DETACHED session: its own s.sub++
// outruns Retire's bump so the drain loop's `s.sub != mine` exit never fires,
// and it parks on a fresh wake nothing will ever close — the drainer and its
// session state strand for the process lifetime (a live goroutine leak once
// in-process reattach lets Retire run without killing the agent connection).
//
// The afterResolve seam makes the interleave deterministic: it fires on serve's
// own goroutine after the resolve and before the bind, so the Retire it runs is
// GUARANTEED to sit in the window rather than being timing-dependent. No sleeps,
// no retries — the seam IS the synchronization.
func TestControlServeRefusesRetiredSessionInResolveWindow(t *testing.T) {
	p := newTestProducer()

	// Fire a full Retire in serve's resolve->bind window, exactly once.
	var once sync.Once
	p.setAfterResolve(func() {
		once.Do(func() { p.Retire(testSession) })
	})

	stream := newControlStream()
	served := make(chan error, 1)
	go func() { served <- p.serve(t.Context(), testSession, stream) }()

	// serve must REFUSE the bind and return promptly: the session was retired
	// in the window, so there is nothing live to serve. A drainer that instead
	// bound the detached session would park forever on a wake Retire already
	// closed-and-nilled, so this never returns — the leak.
	select {
	case err := <-served:
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("serve on a session retired in the resolve window = %v (code %v), want CodeNotFound: a session torn down before the bind must be refused, not bound", err, connect.CodeOf(err))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s: it bound a session Retire had already detached and parked on a wake nothing will ever close — the stranded drainer SEA-1550 describes")
	}

	// No detached session may survive the retirement: binding on the torn-down
	// object would have left live=true state behind that nothing retires again.
	if got := p.sessionCount(); got != 0 {
		t.Fatalf("sessions after a Retire in the serve window = %d, want 0: serve revived a session Retire had deleted", got)
	}
	stream.none(t, "a refused subscription must receive nothing")
}
