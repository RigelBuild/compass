//go:build unix

package delivery

// The fan-out consumer's acceptance cases (SEA-1569 T3, design.md:744-761),
// RED-first. Each drives the consumer through the real events bus + hand-written
// fakes and gates on the recorder's observed dispatches — never a sleep, never a
// retry (rule://no-retries). context.Background() is the test root
// (rule://go-thread-context exemption for _test.go); it is threaded into Run and
// never re-rooted below.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// startConsumer runs c.Run in the background on a cancelable child of the test
// root and registers cancellation + drain on cleanup, so every test ends the
// loop deterministically. Returns the bus the test publishes onto.
func startConsumer(t *testing.T, c *Consumer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx) // Run returns nil on ctx cancel; the error is asserted elsewhere
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// Case 1: a MessagePosted on a subscribed channel dispatches exactly one deliver
// per live subscribed agent session, author excluded, ascending-seq order per
// session.
func TestPostedDispatchesOnePerLiveSubscriber(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB, author}
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "hello")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (one per live subscriber, author excluded)", len(got))
	}
	sessions := map[string]bool{}
	for _, d := range got {
		if d.messageID != "m1" {
			t.Errorf("dispatched message id = %q, want m1", d.messageID)
		}
		sessions[d.sessionID] = true
	}
	if !sessions["sess-a"] || !sessions["sess-b"] {
		t.Fatalf("dispatched to sessions %v, want both sess-a and sess-b", sessions)
	}
}

// Case 1 (ordering half): two posts on one channel to one recipient dispatch in
// ascending post order (the control lane preserves send order; the per-session
// gate preserves it under concurrency).
func TestPostedDispatchesAscendingPerSession(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "first")))
	c.bus.Publish(postedResponse(wireText("m2", author, "second")))
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if got[0].messageID != "m1" || got[1].messageID != "m2" {
		t.Fatalf("dispatch order = [%s, %s], want [m1, m2]", got[0].messageID, got[1].messageID)
	}
}

// Case 2: an unsubscribed non-home member gets nothing — it is absent from the
// resolved subscriber set, so no deliver is dispatched to it.
func TestUnsubscribedMemberGetsNothing(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const subscribed, unsubscribed store.AccountID = "agent-sub", "agent-unsub"

	// Only the subscribed agent is in the resolved set (the SQL disjunct excludes
	// the unsubscribed non-home member; the fake models the resolved result).
	reads.subscribers[ch] = []store.AccountID{subscribed}
	res.bind(subscribed, "sess-sub")
	res.bind(unsubscribed, "sess-unsub") // live, but not a subscriber
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "hello")))
	disp.waitForDispatches(t, 1)

	for _, d := range disp.snapshot() {
		if d.sessionID == "sess-unsub" {
			t.Fatalf("dispatched to the unsubscribed member session %q, want nothing", d.sessionID)
		}
	}
}

// Case 8: a human-authored message delivers at POST (settled at post; does not
// stream), immediately, without any settle edge.
func TestHumanAuthoredDeliversAtPost(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const human store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.agents[human] = false // human author
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", human, "hi")))
	disp.waitForDispatches(t, 1)

	if got := disp.snapshot(); got[0].messageID != "m1" || got[0].sessionID != "sess-a" {
		t.Fatalf("human-authored dispatch = %+v, want {sess-a, m1}", got[0])
	}
}

// Case 9: an agent-authored message delivers ONLY after the author's turn-settle
// (WORKING->READY), carrying the SETTLED block set (re-read from the store at the
// settle edge), and NOT at post.
func TestAgentAuthoredHeldUntilSettle(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient, authorAgent}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	// The store holds the SETTLED blocks the settle edge re-reads.
	reads.seedMessage(textMessage("m1", authorAgent, "settled body"))
	startConsumer(t, c)

	// Post while the author streams: HELD, nothing dispatched yet.
	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "initial body")))
	c.waitHeld(t, "sess-author", 1)
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatched %d before settle, want 0 (held)", len(got))
	}

	// Author settles WORKING->READY: fire the held deliver from settled blocks.
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if got[0].sessionID != "sess-recip" || got[0].messageID != "m1" {
		t.Fatalf("post-settle dispatch = %+v, want {sess-recip, m1}", got[0])
	}
}

// Case 10: an agent-authored message held at the author's WORKING state whose
// author then emits an ERRORED frame is delivered to a LIVE recipient from stored
// blocks, without waiting for the recipient to reconnect.
func TestAgentAuthoredFiredOnTerminalFrame(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	reads.seedMessage(textMessage("m1", authorAgent, "stored body"))
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "initial body")))
	c.waitHeld(t, "sess-author", 1)

	// Author dies with an ERRORED terminal frame: fire the held set from stored.
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)
	disp.waitForDispatches(t, 1)

	if got := disp.snapshot(); got[0].sessionID != "sess-recip" || got[0].messageID != "m1" {
		t.Fatalf("terminal-frame dispatch = %+v, want {sess-recip, m1}", got[0])
	}
}

// Case 11: an agent-authored message held whose author dies with NO terminal
// frame is NOT force-delivered by an author trigger — no settle edge ever fires,
// so nothing is dispatched and the held entry stays for the reconnect sweep and
// the hub's next-enroll reap ("no-loss, not no-leak").
func TestAgentAuthoredNoFrameNotForceDelivered(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	reads.seedMessage(textMessage("m1", authorAgent, "stored body"))
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "initial body")))
	c.waitHeld(t, "sess-author", 1)

	// A no-frame death is a DISCONNECTED edge (the bounded-reattach window), which
	// must NOT fire held delivers (design.md:314-315). Deliver it and assert the
	// held entry survives and nothing was dispatched.
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED)
	// A READY settle for a DIFFERENT session drains the settle queue, giving a
	// deterministic barrier that the DISCONNECTED edge was processed-and-ignored
	// without firing sess-author's held set.
	c.OnSessionSettled("sess-other", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	c.waitSettleDrained(t)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatched %d on a no-frame death, want 0 (recipient sweeps instead)", len(got))
	}
	if !c.isHeld("sess-author", "m1") {
		t.Fatal("held entry for sess-author reaped by a no-frame death, want it retained for the sweep")
	}
}

// Case 12: an agent-authored message whose author has NO live session delivers
// immediately from stored blocks — there is no live turn to wait on.
func TestAgentAuthoredNoLiveAuthorDeliversNow(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	// Author agent is NOT bound to a live session (already stopped at post), so
	// the deliver re-reads the settled blocks from the store (FIX 3).
	reads.seedMessage(textMessage("m1", authorAgent, "stored body"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "stored body")))
	disp.waitForDispatches(t, 1)

	if got := disp.snapshot(); got[0].sessionID != "sess-recip" || got[0].messageID != "m1" {
		t.Fatalf("no-live-author dispatch = %+v, want {sess-recip, m1}", got[0])
	}
}

// Case 6: live events during a sweep queue BEHIND it (the per-session dispatch
// gate). A sweep for sess-recip holds the session gate for its whole ordered
// re-dispatch; a live deliver for the SAME session published mid-sweep must not
// dispatch until the sweep releases the gate, and then in order after it.
func TestLiveEventsQueueBehindSweep(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	res.bind(recipient, "sess-recip")
	// The sweep owes one message to the recipient.
	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {textMessage("swept-1", author, "owed")},
	}

	// beforeGate signals when the live deliver reaches the session gate, so the
	// test knows deterministically that the live deliver is queued (blocked on the
	// gate the sweep holds) before it asserts nothing dispatched — no sleep.
	atGate := make(chan struct{}, 1)
	c.beforeGate = func(sessionID string) {
		if sessionID == "sess-recip" {
			atGate <- struct{}{}
		}
	}
	startConsumer(t, c)

	// Arm the first dispatch (the sweep's re-dispatch) to block while holding the
	// session gate, so a concurrent live deliver for the same session must queue
	// behind it.
	disp.armFirstBlock()
	go c.sweepSession(context.Background(), recipient, "sess-recip")
	<-disp.enteredFirst // the sweep dispatch is in-flight, holding the gate

	// A live deliver for the SAME session, published now, reaches the gate and
	// blocks there (the sweep holds it). Wait for it to reach the gate, then
	// assert nothing has dispatched — it is provably queued, not dropped.
	c.bus.Publish(postedResponse(wireText("live-1", author, "live")))
	<-atGate
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("recorded %d dispatches while the sweep holds the gate, want 0 (live deliver must queue behind)", len(got))
	}

	// Release the sweep; the swept deliver records first (it held the gate), then
	// the live deliver drains behind it, in order.
	close(disp.releaseFirst)
	disp.waitForDispatches(t, 2)
	got := disp.snapshot()
	if got[0].messageID != "swept-1" {
		t.Fatalf("first dispatch = %q, want swept-1 (the sweep drains before the queued live deliver)", got[0].messageID)
	}
	if got[1].messageID != "live-1" {
		t.Fatalf("second dispatch = %q, want live-1 (queued behind the sweep)", got[1].messageID)
	}
}

// Case 7: a bus-lag resync triggers the sweep, NOT a loss. When the consumer's
// live channel overruns (lagged), it redelivers every owed message to every live
// session rather than dropping the missed events. Driven deterministically: block
// the consumer inside its first dispatch, overrun its live buffer, release, and
// assert the owed message reaches its recipient via the sweep.
func TestBusLagTriggersSweepNotLoss(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const liveAgent store.AccountID = "agent-live"
	const sweptAgent store.AccountID = "agent-swept"

	reads.subscribers[ch] = []store.AccountID{liveAgent}
	res.bind(liveAgent, "sess-live")
	res.bind(sweptAgent, "sess-swept")
	// The swept agent is owed a message it can ONLY receive via the sweep (it is
	// not a live-channel subscriber), so its arrival proves the lag path swept.
	reads.owed[sweptAgent] = map[store.ChannelID][]store.Message{
		ch: {textMessage("swept-only", author, "owed")},
	}
	// Block the first live dispatch so the consumer stalls and its live buffer
	// overruns while we publish past the ring window.
	disp.armFirstBlock()
	startConsumer(t, c)

	// First event: consumed off Live, then stalls in dispatch (armed).
	c.bus.Publish(postedResponse(wireText("m0", author, "first")))
	<-disp.enteredFirst

	// Overrun the consumer's live buffer (capacity 1024) so its channel closes
	// lagged. Publish comfortably past it.
	for range 1100 {
		c.bus.Publish(postedResponse(wireText("flood", author, "x")))
	}

	// Release the stalled dispatch. The consumer drains its buffer, then reads the
	// lagged-closed channel and runs the sweep.
	close(disp.releaseFirst)

	// The swept-only message reaching sess-swept proves lag -> sweep, not loss.
	if !disp.waitForMessage(t, "swept-only") {
		t.Fatal("owed message never redelivered after bus lag: the lag path lost it instead of sweeping")
	}
}

// Case 4: a refused dispatch leaves the cursor UNADVANCED. The consumer never
// advances a cursor on send (the cursor advances only on delivery_ack, in the
// hub's ack arm); a synchronous refusal is swallowed as "no live session, fall
// to the sweep". This test asserts the refusal is non-fatal (the consumer keeps
// running and delivers the next message), so a refused deliver is a no-op on the
// dispatch side — the cursor it never touched stays where the sweep can redeliver
// from. The cursor-advance itself is proven in the store/hub ack tests.
func TestRefusedDispatchIsNonFatalNoAdvance(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const refused, ok store.AccountID = "agent-refused", "agent-ok"

	reads.subscribers[ch] = []store.AccountID{refused, ok}
	res.bind(refused, "sess-refused")
	res.bind(ok, "sess-ok")
	// The refused recipient's session synchronously refuses (no live stream edge).
	disp.refuse["sess-refused"] = errNoStream
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "hello")))
	// The OK recipient still gets its deliver — the refusal did not wedge the
	// consumer, and the refused deliver recorded nothing (no cursor advance path).
	disp.waitForDispatches(t, 1)
	for _, d := range disp.snapshot() {
		if d.sessionID == "sess-refused" {
			t.Fatalf("a refused dispatch was recorded as delivered: %+v", d)
		}
	}
}

// FIX 1 (SEA-1569 T3 review): a bus-lag overrun must RE-SUBSCRIBE and keep
// delivering, not terminate the singleton consumer goroutine. design.md:227-231
// rules a resync "a latency blip, never a loss" and says the consumer "treats a
// resync exactly as SubscribeComms clients do" — a client re-subscribes and
// continues. The pre-fix code ran the sweep then `return nil`, so after one
// transient overrun NO message was ever delivered live again for the server's
// whole life. This drives an overrun (mirroring Case 7), then publishes a NEW
// message AFTER the sweep and asserts it is delivered live — proof the loop is
// still running and still fanning out.
func TestBusLagResubscribesAndKeepsDelivering(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const liveAgent store.AccountID = "agent-live"
	const sweptAgent store.AccountID = "agent-swept"

	reads.subscribers[ch] = []store.AccountID{liveAgent}
	res.bind(liveAgent, "sess-live")
	res.bind(sweptAgent, "sess-swept")
	// The swept agent is owed a message only the sweep can deliver, so its
	// arrival proves the overrun swept before the loop resumed.
	reads.owed[sweptAgent] = map[store.ChannelID][]store.Message{
		ch: {textMessage("swept-only", author, "owed")},
	}

	// A deterministic signal that the lag branch re-subscribed: without it a
	// post-sweep publish could race into the (deliberately un-drained) replay
	// snapshot rather than the fresh live tail. Under the pre-fix code the branch
	// returns instead of re-subscribing, so this never fires — the RED.
	resubscribed := make(chan struct{})
	c.afterResubscribe = func() { close(resubscribed) }

	disp.armFirstBlock()
	startConsumer(t, c)

	// First event consumed off Live, then stalls in the armed dispatch.
	c.bus.Publish(postedResponse(wireText("m0", author, "first")))
	<-disp.enteredFirst

	// Overrun the live buffer (capacity 1024) so the channel closes lagged.
	for range 1100 {
		c.bus.Publish(postedResponse(wireText("flood", author, "x")))
	}
	// Release the stall: the consumer drains its buffer, reads the lagged-closed
	// channel, sweeps, then re-subscribes and continues.
	close(disp.releaseFirst)

	if !disp.waitForMessage(t, "swept-only") {
		t.Fatal("owed message never redelivered after bus lag: the lag path lost it instead of sweeping")
	}
	select {
	case <-resubscribed:
	case <-time.After(testTimeout):
		t.Fatal("consumer never re-subscribed after the lag overrun: the loop returned instead of continuing (permanent live-delivery death)")
	}

	// A NEW message published after the sweep+resubscribe must be delivered live —
	// proof the singleton loop is still running and still fanning out.
	c.bus.Publish(postedResponse(wireText("post-sweep-1", author, "alive")))
	if !disp.waitForMessage(t, "post-sweep-1") {
		t.Fatal("post-sweep message never delivered: the consumer stopped after the overrun instead of re-subscribing and continuing")
	}
}

// FIX A (SEA-1569 T3 round-2 review): a bus-lag resync must SUBSCRIBE before it
// sweeps, or a message committed+published in the window between the sweep's
// owed-read and the fresh Subscribe's lock-acquire is delivered to no one until
// the recipient reconnects. The post path commits the store row before
// publishing MessagePosted (comms.go:270-271), so such a window message M is
// absent from the already-read owed set AND, under a sweep-first order, only in
// the fresh Replay (deliberately not drained) and NOT on a Live that predates
// its registration (events.go:216-219) — a live-delivery seam.
//
// This drives the overrun exactly as TestBusLagResubscribesAndKeepsDelivering,
// but blocks the consumer INSIDE the resync sweep (via a beforeUndelivered seam
// on the reads fake), publishes M during that block, and asserts M reaches a
// live subscriber with an EMPTY owed set — so M can ONLY have arrived via live
// delivery, never the sweep. With the subscribe-first order M lands on the fresh
// Live and is delivered after the sweep drains; with the pre-fix sweep-first
// order M is published before the fresh Subscribe, lands in the skipped Replay,
// and never reaches the window agent (the RED).
func TestBusLagResubscribeDeliversWindowMessageLive(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const windowAgent store.AccountID = "agent-window"

	// windowAgent is the sole live subscriber of chan-1 and is owed NOTHING, so
	// the resync sweep dispatches it nothing — a clean discriminator: the window
	// message can reach sess-window ONLY via live delivery, never the sweep.
	reads.subscribers[ch] = []store.AccountID{windowAgent}
	res.bind(windowAgent, "sess-window")

	// Block the resync sweep mid-flight so the test can publish M while the
	// consumer is between subscribe and the tail resume. On the FIRST owed-read
	// (any agent), signal entry then block until released.
	enteredSweep := make(chan struct{})
	releaseSweep := make(chan struct{})
	var once sync.Once
	reads.beforeUndelivered = func(store.AccountID) {
		once.Do(func() {
			close(enteredSweep)
			<-releaseSweep
		})
	}

	disp.armFirstBlock()
	startConsumer(t, c)

	// First event consumed off Live, then stalls in the armed dispatch.
	c.bus.Publish(postedResponse(wireText("m0", author, "first")))
	<-disp.enteredFirst

	// Overrun the live buffer (capacity 1024) so the channel closes lagged.
	for range 1100 {
		c.bus.Publish(postedResponse(wireText("flood", author, "x")))
	}
	// The window publish must happen once the consumer is inside the resync sweep,
	// but the recorder channel has to keep draining meanwhile (the flood fills it
	// during the Live-buffer drain), so run the coordination in a goroutine and
	// let waitForMessage below drain the recorder.
	go func() {
		<-enteredSweep
		// The consumer is inside the resync sweep. With the subscribe-first order
		// the fresh subscription is already live, so M lands on the new Live; with
		// the pre-fix sweep-first order the subscribe has not happened yet, so M
		// falls into the (skipped) fresh Replay and is never delivered live.
		c.bus.Publish(postedResponse(wireText("window-m", author, "in the window")))
		close(releaseSweep)
	}()

	// Release the stall: the consumer drains its buffer, reads the lagged-closed
	// channel, and runs the resync (subscribe + sweep), blocking in the sweep's
	// beforeUndelivered until the goroutine above releases it.
	close(disp.releaseFirst)

	// M must be delivered live to sess-window. Its owed set is empty, so a sweep
	// could never have carried it — arrival proves subscribe-first closed the seam.
	if !disp.waitForMessage(t, "window-m") {
		t.Fatal("window message never delivered live to sess-window: the resync swept before it subscribed, so M fell into the skipped replay and was lost until reconnect")
	}
}

// blockRecord is one observed deliver: the session it targeted plus the delivered
// message's first text block, so a test can assert WHICH block set was sent (not
// just the message id).
type blockRecord struct {
	sessionID string
	messageID string
	firstText string
}

// blockCapturingDispatcher records the delivered block content of every dispatch,
// so a test can distinguish a deliver carrying the STORED blocks from one
// carrying the POSTED (stale) blocks — the shared fakeDispatcher captures only
// the message id, which cannot tell the two apart.
type blockCapturingDispatcher struct {
	mu       sync.Mutex
	calls    []blockRecord
	recorded chan struct{}
}

func newBlockCapturingDispatcher() *blockCapturingDispatcher {
	return &blockCapturingDispatcher{recorded: make(chan struct{}, 1024)}
}

func (d *blockCapturingDispatcher) DispatchControl(_ context.Context, sessionID string, op *compassv1internal.AgentControl) error {
	msg := op.GetDeliver().GetMessage()
	var first string
	if b := msg.GetBlocks(); len(b) > 0 {
		first = b[0].GetText()
	}
	d.mu.Lock()
	d.calls = append(d.calls, blockRecord{sessionID: sessionID, messageID: msg.GetId(), firstText: first})
	d.mu.Unlock()
	d.recorded <- struct{}{}
	return nil
}

func (d *blockCapturingDispatcher) waitFor(t *testing.T, messageID string) blockRecord {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		d.mu.Lock()
		for _, rec := range d.calls {
			if rec.messageID == messageID {
				d.mu.Unlock()
				return rec
			}
		}
		d.mu.Unlock()
		select {
		case <-d.recorded:
		case <-deadline:
			t.Fatalf("waited for a deliver of %q, none recorded", messageID)
		}
	}
}

// FIX 3 (SEA-1569 T3 review): the no-live-author path must deliver from the
// STORED block set, not the posted (possibly partial) wire message. The branch
// comment and design.md:177-178,:306 both specify "from its stored block set";
// the pre-fix code fanned out the raw bus `msg`. This seeds the store with a
// GROWN block set distinct from the posted one and asserts the dispatched deliver
// carries the stored blocks.
func TestAgentAuthoredNoLiveAuthorDeliversStoredBlocks(t *testing.T) {
	disp := newBlockCapturingDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(events.NewBus[*compassv1.SubscribeCommsResponse](), reads, disp, res, discardLogger())

	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	// Author agent is NOT live (already stopped at post). The store holds the
	// SETTLED, grown block set; the posted wire message carries the stale partial.
	reads.seedMessage(textMessage("m1", authorAgent, "stored grown body"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", authorAgent, "posted partial body")))

	rec := disp.waitFor(t, "m1")
	if rec.sessionID != "sess-recip" {
		t.Fatalf("no-live-author deliver session = %q, want sess-recip", rec.sessionID)
	}
	if rec.firstText != "stored grown body" {
		t.Fatalf("no-live-author deliver carried %q, want the STORED blocks %q — the branch fanned out the posted (stale) message instead of re-reading the store",
			rec.firstText, "stored grown body")
	}
}
