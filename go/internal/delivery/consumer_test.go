//go:build unix

package delivery

// The fan-out consumer's acceptance cases (RIG-1569 T3), RED-first. Each drives
// the consumer through the fake fabric + hand-written fakes and gates on the
// recorder's observed dispatches — never a sleep, never a retry (rule://no-retries).

import (
	"context"
	"sync"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// startConsumer runs c.Run in the background on a cancelable child of the test
// root and registers cancellation + drain on cleanup, so every test ends the
// loop deterministically and a Run error fails the test.
func startConsumer(t *testing.T, c *Consumer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("consumer Run: %v", err)
		}
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

	postMessage(t, c, reads, textMessage("m1", author, "hello"))
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

	postMessage(t, c, reads, textMessage("m1", author, "first"))
	postMessage(t, c, reads, textMessage("m2", author, "second"))
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

	postMessage(t, c, reads, textMessage("m1", author, "hello"))
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

	postMessage(t, c, reads, textMessage("m1", human, "hi"))
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
	startConsumer(t, c)

	// Post while the author streams: HELD, nothing dispatched yet.
	postMessage(t, c, reads, textMessage("m1", authorAgent, "initial body"))
	c.waitHeld(t, "sess-author", 1)
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatched %d before settle, want 0 (held)", len(got))
	}

	// The author's turn grows the stored blocks before it settles.
	reads.seedMessage(textMessage("m1", authorAgent, "settled body"))

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
	startConsumer(t, c)

	postMessage(t, c, reads, textMessage("m1", authorAgent, "initial body"))
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
// so nothing is dispatched and the held entry stays until the hub's next-enroll
// reap, after which the recovery sweep delivers it ("no-loss, not no-leak").
func TestAgentAuthoredNoFrameNotForceDelivered(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	postMessage(t, c, reads, textMessage("m1", authorAgent, "initial body"))
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

	postMessage(t, c, reads, textMessage("m1", authorAgent, "stored body"))
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
	go c.sweepSession(context.Background(), recipient, "sess-recip", nil)
	<-disp.enteredFirst // the sweep dispatch is in-flight, holding the gate

	// A live deliver for the SAME session, published now, reaches the gate and
	// blocks there (the sweep holds it). Wait for it to reach the gate, then
	// assert nothing has dispatched — it is provably queued, not dropped.
	postMessage(t, c, reads, textMessage("live-1", author, "live"))
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

// Case 7: a message whose fabric publish failed is owed by the cursor but never
// published. A fabric reconnect runs the recovery pass, which delivers it to a
// live recipient that never restarts; only the sweep can reach it.
func TestReconnectSweepsPublishFailedPlainDeliver(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	res.bind(recipient, "sess-recip")
	startConsumer(t, c)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)

	reads.mu.Lock()
	reads.owed[recipient] = map[store.ChannelID][]store.Message{ch: {textMessage("unpublished", author, "plain")}}
	reads.mu.Unlock()
	fab.fireReconnect()

	if !disp.waitForMessage(t, "unpublished") {
		t.Fatal("publish-failed plain message never delivered after a fabric reconnect")
	}
}

// Case 7b: while NATS stays up nothing reconnects, so the floor tick is what
// bounds how long a publish-failed plain message waits for a live recipient.
func TestFloorTickSweepsPublishFailedPlainDeliver(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	tick := make(chan time.Time)
	c.newFloorTicker = func() (<-chan time.Time, func()) { return tick, func() {} }
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)
	fakeFabricOf(c).waitSubscribed(t)

	reads.mu.Lock()
	reads.owed[recipient] = map[store.ChannelID][]store.Message{ch: {textMessage("unpublished", author, "plain")}}
	reads.mu.Unlock()
	select {
	case tick <- time.Now():
	case <-time.After(testTimeout):
		t.Fatal("consumer loop never read the floor tick")
	}

	if !disp.waitForMessage(t, "unpublished") {
		t.Fatal("publish-failed plain message never delivered after a floor tick")
	}
}

// A recovery pass must not sweep a message held for a still-streaming author:
// the partial blocks would land first and message_id dedup would drop the
// settled deliver. The settle then sends it once, carrying the settled blocks.
func TestRecoverySweepSkipsHeldMessage(t *testing.T) {
	disp := newBlockCapturingDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(reads, disp, res, newFakeFabric(), discardLogger())
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const human store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	tick := make(chan time.Time)
	c.newFloorTicker = func() (<-chan time.Time, func()) { return tick, func() {} }
	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)

	postMessage(t, c, reads, textMessage("m1", authorAgent, "partial body"))
	c.waitHeld(t, "sess-author", 1)
	// The cursor owes the recipient both the held message and a plain one.
	reads.mu.Lock()
	reads.owed[recipient] = map[store.ChannelID][]store.Message{ch: {
		textMessage("m1", authorAgent, "partial body"),
		textMessage("plain", human, "plain body"),
	}}
	reads.mu.Unlock()

	// Each pass ends with the mention scan's batch read, which gates the sweep.
	passes := reads.unroutedCallCount()
	select {
	case tick <- time.Now():
	case <-time.After(testTimeout):
		t.Fatal("consumer loop never read the floor tick")
	}
	reads.waitUnroutedCalls(t, passes+1)
	disp.waitFor(t, "plain") // positive control: a non-held owed message is swept
	fab.fireReconnect()
	reads.waitUnroutedCalls(t, passes+2)
	if n := disp.countFor("m1"); n != 0 {
		t.Fatalf("recovery swept held m1 %d times, want 0 (fireHeld owns it)", n)
	}

	reads.seedMessage(textMessage("m1", authorAgent, "settled body"))
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec := disp.waitFor(t, "m1")
	if rec.sessionID != "sess-recip" || rec.firstText != "settled body" {
		t.Fatalf("settled deliver = %+v, want {sess-recip, m1, settled body}", rec)
	}
	if n := disp.countFor("m1"); n != 1 {
		t.Fatalf("m1 dispatched %d times, want exactly 1", n)
	}
}

// OQ-1: a ref whose row is missing is logged and acked, since redelivery cannot
// make the row appear; the refs behind it still deliver.
func TestMissingRowRefIsAckedAndConsumerContinues(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	publishPosted(t, c, "ghost") // never committed: MessageByID is ErrNotFound
	fakeFabricOf(c).waitAcked(t, "ghost")
	postMessage(t, c, reads, textMessage("m1", author, "hello"))
	if !disp.waitForMessage(t, "m1") {
		t.Fatal("m1 never delivered after a missing-row ref")
	}
	for _, d := range disp.snapshot() {
		if d.messageID == "ghost" {
			t.Fatalf("dispatched the missing-row ref: %+v", d)
		}
	}
}

// OQ-3 part 1: the per-event re-read runs under the ref's tenant, never the
// system role, and the held deliver re-reads under the tenant captured at hold.
func TestEventReadsRunUnderRefTenant(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"
	const tenant store.TenantID = "tenant-b"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	reads.seedMessage(textMessage("m1", authorAgent, "body"))
	startConsumer(t, c)

	publishRef(t, context.Background(), c, tenant, "m1")
	c.waitHeld(t, "sess-author", 1)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	disp.waitForDispatches(t, 1)

	scopes := reads.readScopes("m1")
	if len(scopes) != 2 {
		t.Fatalf("MessageByID(m1) calls = %d, want 2 (the post re-read and the settle re-read)", len(scopes))
	}
	for i, s := range scopes {
		if s.tenant != tenant || s.systemRole {
			t.Fatalf("MessageByID(m1) call %d ran under %+v, want tenant %q without the system role", i, s, tenant)
		}
	}
}

// OQ-2: fabric callbacks hold while the loop drains settles, concurrently. Under
// -race this proves the held registry is synchronized, and each held message
// still fires exactly once.
func TestConcurrentEventRefAndSettleDrainFireEachHeldOnce(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"
	const n = 50

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	res.bind(authorAgent, "sess-author")
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	settling := make(chan struct{})
	go func() {
		defer close(settling)
		for range n {
			c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
		}
	}()
	for i := range n {
		postMessage(t, c, reads, textMessage("m"+itoa(i), authorAgent, "body"))
	}
	<-settling
	fab := fakeFabricOf(c)
	for i := range n {
		fab.waitAcked(t, "m"+itoa(i))
	}
	// Every hold has landed; this settle fires whatever the racing ones missed.
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	disp.waitForDispatches(t, n)
	// The loop drains edges in order, so once this one is popped every earlier fire returned.
	c.OnSessionSettled("sess-other", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	c.waitSettleDrained(t)

	fired := map[string]int{}
	for _, d := range disp.snapshot() {
		fired[d.messageID]++
	}
	for i := range n {
		if id := "m" + itoa(i); fired[id] != 1 {
			t.Fatalf("dispatches of %s = %d, want exactly 1", id, fired[id])
		}
	}
}

// Case 4: a refused dispatch leaves the cursor UNADVANCED. The consumer never
// advances a cursor on send (that happens only on delivery_ack in the hub); a
// synchronous refusal is swallowed as "no live session, fall to the sweep". Asserts
// the refusal is non-fatal; the cursor-advance is proven in the store/hub ack tests.
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

	postMessage(t, c, reads, textMessage("m1", author, "hello"))
	// The OK recipient still gets its deliver — the refusal did not wedge the
	// consumer, and the refused deliver recorded nothing (no cursor advance path).
	disp.waitForDispatches(t, 1)
	for _, d := range disp.snapshot() {
		if d.sessionID == "sess-refused" {
			t.Fatalf("a refused dispatch was recorded as delivered: %+v", d)
		}
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
	signalObserved(d.recorded)
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

// countFor reports how many delivers of messageID were recorded.
func (d *blockCapturingDispatcher) countFor(messageID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, rec := range d.calls {
		if rec.messageID == messageID {
			n++
		}
	}
	return n
}

// The no-live-author path delivers the STORED block set. The ref carries no
// blocks, so the deliver must carry what the store holds.
func TestAgentAuthoredNoLiveAuthorDeliversStoredBlocks(t *testing.T) {
	disp := newBlockCapturingDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(reads, disp, res, newFakeFabric(), discardLogger())

	const ch store.ChannelID = "chan-1"
	const authorAgent store.AccountID = "agent-author"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents[authorAgent] = true
	// Author agent is NOT live (already stopped at post); the store holds the
	// SETTLED, grown block set.
	reads.seedMessage(textMessage("m1", authorAgent, "stored grown body"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	publishPosted(t, c, "m1")

	rec := disp.waitFor(t, "m1")
	if rec.sessionID != "sess-recip" {
		t.Fatalf("no-live-author deliver session = %q, want sess-recip", rec.sessionID)
	}
	if rec.firstText != "stored grown body" {
		t.Fatalf("no-live-author deliver carried %q, want the STORED blocks %q", rec.firstText, "stored grown body")
	}
}

// TestConsumerNilAgentWakerConstructsAndRuns pins the nil-safe wake seam
// (RIG-1641 T3): a Consumer built with no AgentWaker wired — the default, since
// T3 adds no production caller — constructs and runs its loop exactly as
// before, routing a posted message to a live subscriber. The wake seam is
// defined and wired at assembly but dormant until T2/T4 add the routing caller;
// this proves its mere presence changes nothing when unset.
//
// Mutation: a non-nil-safe waker field (e.g. an unconditional
// c.agentWaker.WakeAgent) would nil-panic the loop here, reddening the delivery.
func TestConsumerNilAgentWakerConstructsAndRuns(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t) // no SetAgentWaker: agentWaker stays nil
	if c.agentWaker != nil {
		t.Fatal("precondition: a freshly-built consumer has no AgentWaker wired")
	}
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA, author}
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	postMessage(t, c, reads, textMessage("m1", author, "hello"))
	disp.waitForDispatches(t, 1)

	if got := disp.snapshot(); len(got) != 1 || got[0].sessionID != "sess-a" {
		t.Fatalf("nil-waker consumer dispatches = %v, want one deliver to sess-a (a nil waker must not disturb routing)", got)
	}
}
