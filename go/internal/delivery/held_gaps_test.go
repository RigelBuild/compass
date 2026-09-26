//go:build unix

package delivery

// The fabric callback holds concurrently with Run's settle drain, and the start
// sweep runs while an author streams. These cases pin both held-message gaps.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// newHeldGapConsumer wires a live agent author and a live subscribed recipient
// over a block-capturing dispatcher, with the consumer clock pinned to settleAt.
// It does not start the consumer, so a test can install read hooks first.
func newHeldGapConsumer(t *testing.T, settleAt time.Time) (*Consumer, *blockCapturingDispatcher, *fakeReads) {
	t.Helper()
	disp := newBlockCapturingDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(reads, disp, res, newFakeFabric(), discardLogger())
	c.now = func() time.Time { return settleAt }
	reads.subscribers["chan-1"] = []store.AccountID{"agent-recip"}
	reads.agents["agent-author"] = true
	res.bind("agent-author", "sess-author")
	res.bind("agent-recip", "sess-recip")
	return c, disp, reads
}

// heldGapFixture is newHeldGapConsumer, started and subscribed.
func heldGapFixture(t *testing.T, settleAt time.Time) (*Consumer, *blockCapturingDispatcher, *fakeReads) {
	t.Helper()
	c, disp, reads := newHeldGapConsumer(t, settleAt)
	startConsumer(t, c)
	fakeFabricOf(c).waitSubscribed(t)
	return c, disp, reads
}

// messageAt builds an author message stamped at the given commit time.
func messageAt(id, body string, at time.Time) store.Message {
	m := textMessage(id, "agent-author", body)
	m.At = at
	return m
}

// A settle drained before the callback holds a message from that same turn must
// not strand it until the next settle: the hold sees the settle and fires. A
// commit stamped at the settle ms is still that turn, and a terminal settle
// counts like READY.
func TestHoldAfterSettleFiresAtOnce(t *testing.T) {
	settleAt := time.UnixMilli(2_000_000)
	cases := []struct {
		name  string
		at    time.Time
		state compassv1.AgentSessionState
	}{
		{"before settle", settleAt.Add(-time.Second), compassv1.AgentSessionState_AGENT_SESSION_STATE_READY},
		{"at settle", settleAt, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY},
		{"stopped", settleAt.Add(-time.Second), compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, disp, reads := heldGapFixture(t, settleAt)

			c.OnSessionSettled("sess-author", tc.state)
			c.waitSettleDrained(t)

			postMessage(t, c, reads, messageAt("m1", "settled body", tc.at))
			rec := disp.waitFor(t, "m1")
			if rec.sessionID != "sess-recip" || rec.firstText != "settled body" {
				t.Fatalf("deliver = %+v, want {sess-recip, m1, settled body}", rec)
			}
			fakeFabricOf(c).waitAcked(t, "m1")
			if n := disp.countFor("m1"); n != 1 {
				t.Fatalf("m1 dispatched %d times, want exactly 1", n)
			}
			if c.isHeld("sess-author", "m1") {
				t.Fatal("m1 still held after its fire")
			}
		})
	}
}

// A post whose settle already landed must not overtake an earlier message of
// the same author that fireHeld is still sending. The loop is parked inside
// fireHeld's re-read of m1 while m2 arrives, so the order is event-gated.
func TestFireNowKeepsPostOrderBehindHeld(t *testing.T) {
	for _, state := range []compassv1.AgentSessionState{
		compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED,
	} {
		t.Run(state.String(), func(t *testing.T) {
			c, disp, reads := newHeldGapConsumer(t, time.UnixMilli(200))
			entered := make(chan struct{})
			release := make(chan struct{})
			var armed atomic.Bool
			reads.beforeMessageByID = func(id string) {
				if id == "m1" && armed.CompareAndSwap(true, false) {
					close(entered)
					<-release
				}
			}
			startConsumer(t, c)
			// Also runs before startConsumer's cleanup, so a failed assert never
			// leaves the loop parked in the hook.
			unpark := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unpark)
			fakeFabricOf(c).waitSubscribed(t)

			postMessage(t, c, reads, messageAt("m1", "m1 partial", time.UnixMilli(100)))
			c.waitHeld(t, "sess-author", 1)
			reads.seedMessage(messageAt("m1", "m1 settled", time.UnixMilli(100)))
			armed.Store(true)

			c.OnSessionSettled("sess-author", state)
			select {
			case <-entered:
			case <-time.After(testTimeout):
				t.Fatal("fireHeld never re-read m1")
			}
			postMessage(t, c, reads, messageAt("m2", "m2 body", time.UnixMilli(150)))
			fakeFabricOf(c).waitAcked(t, "m2")
			unpark()

			disp.waitFor(t, "m2")
			disp.waitFor(t, "m1")
			got := disp.records()
			if len(got) != 2 ||
				got[0] != (blockRecord{sessionID: "sess-recip", messageID: "m1", firstText: "m1 settled"}) ||
				got[1] != (blockRecord{sessionID: "sess-recip", messageID: "m2", firstText: "m2 body"}) {
				t.Fatalf("delivers = %+v, want [m1 settled, m2 body] to sess-recip, each once", got)
			}
		})
	}
}

// Control: a message committed after the recorded settle belongs to a later
// turn, so it is held until the next settle.
func TestHoldAfterSettleKeepsLaterMessageHeld(t *testing.T) {
	settleAt := time.UnixMilli(2_000_000)
	c, disp, reads := heldGapFixture(t, settleAt)

	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	c.waitSettleDrained(t)

	postMessage(t, c, reads, messageAt("m2", "next turn", settleAt.Add(time.Second)))
	c.waitHeld(t, "sess-author", 1)
	if n := disp.countFor("m2"); n != 0 {
		t.Fatalf("m2 dispatched %d times before the next settle, want 0", n)
	}

	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	disp.waitFor(t, "m2")
	if n := disp.countFor("m2"); n != 1 {
		t.Fatalf("m2 dispatched %d times, want exactly 1", n)
	}
}

// The recovery pass keeps a live session's settle time at any age, since a
// backlog replayed after an outage still needs it, and drops a dead session's.
func TestRecoveryPrunesSettleTimesByLiveness(t *testing.T) {
	c, _, res, _ := newTestConsumer(t)
	now := time.UnixMilli(2_000_000)
	c.now = func() time.Time { return now }
	res.bind("agent-live", "sess-live")

	c.OnSessionSettled("sess-live", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	c.OnSessionSettled("sess-dead", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	now = now.Add(recoveryFloorInterval + time.Hour)
	if !c.hasLastSettle("sess-live") || !c.hasLastSettle("sess-dead") {
		t.Fatal("precondition: both settles should be recorded")
	}

	c.requestRecovery()
	c.drainRecovery(t.Context())
	if !c.hasLastSettle("sess-live") {
		t.Fatal("an old settle time of a live session was pruned")
	}
	if c.hasLastSettle("sess-dead") {
		t.Fatal("the settle time of a non-live session survived recovery")
	}
}

// A recipient that starts while its author streams must not get the held
// message from partial blocks: dedup would drop the settled deliver. The settle
// then sends it once, carrying the settled blocks.
func TestStartSweepSkipsHeldMessage(t *testing.T) {
	disp := newBlockCapturingDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(reads, disp, res, newFakeFabric(), discardLogger())
	const ch store.ChannelID = "chan-1"
	const recipient store.AccountID = "agent-recip"

	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.agents["agent-author"] = true
	res.bind("agent-author", "sess-author")
	startConsumer(t, c)
	fakeFabricOf(c).waitSubscribed(t)

	postMessage(t, c, reads, textMessage("m1", "agent-author", "partial body"))
	c.waitHeld(t, "sess-author", 1)
	// The plain message follows m1 in the owed order, so its dispatch proves the
	// sweep already passed m1.
	reads.mu.Lock()
	reads.owed[recipient] = map[store.ChannelID][]store.Message{ch: {
		textMessage("m1", "agent-author", "partial body"),
		textMessage("plain", "human-1", "plain body"),
	}}
	reads.mu.Unlock()

	res.bind(recipient, "sess-recip")
	c.OnSessionStarted("sess-recip", recipient)
	disp.waitFor(t, "plain")
	if n := disp.countFor("m1"); n != 0 {
		t.Fatalf("start sweep sent held m1 %d times, want 0 (fireHeld owns it)", n)
	}

	reads.seedMessage(textMessage("m1", "agent-author", "settled body"))
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec := disp.waitFor(t, "m1")
	if rec.sessionID != "sess-recip" || rec.firstText != "settled body" {
		t.Fatalf("settled deliver = %+v, want {sess-recip, m1, settled body}", rec)
	}
	if n := disp.countFor("m1"); n != 1 {
		t.Fatalf("m1 dispatched %d times, want exactly 1", n)
	}
}

// deadAuthorFixture holds m1 under an author session that is not live, as a
// reap race or a no-frame death leaves it, and owes it to agent-recip.
func deadAuthorFixture(t *testing.T) (*Consumer, *blockCapturingDispatcher, *fakeResolver, *fakeReads) {
	t.Helper()
	disp := newBlockCapturingDispatcher()
	res := newFakeResolver()
	reads := newFakeReads()
	c := NewConsumer(reads, disp, res, newFakeFabric(), discardLogger())
	c.hold(t.Context(), "sess-dead", "m1", 0)
	reads.owed["agent-recip"] = map[store.ChannelID][]store.Message{"chan-1": {
		textMessage("m1", "agent-author", "stored body"),
		textMessage("plain", "human-1", "plain body"),
	}}
	return c, disp, res, reads
}

// No settle ever fires a dead author's held entry, so the start sweep must
// deliver it rather than skip it.
func TestStartSweepDeliversHeldForDeadAuthor(t *testing.T) {
	c, disp, res, _ := deadAuthorFixture(t)
	startConsumer(t, c)
	fakeFabricOf(c).waitSubscribed(t)

	res.bind("agent-recip", "sess-recip")
	c.OnSessionStarted("sess-recip", "agent-recip")
	disp.waitFor(t, "plain") // the sweep passed m1
	if n := disp.countFor("m1"); n != 1 {
		t.Fatalf("start sweep sent m1 %d times, want 1 (its author is dead)", n)
	}
}

// The recovery sweep must also deliver a held entry stranded under a dead author.
func TestRecoverySweepDeliversHeldForDeadAuthor(t *testing.T) {
	c, disp, res, reads := deadAuthorFixture(t)
	tick := make(chan time.Time)
	c.newFloorTicker = func() (<-chan time.Time, func()) { return tick, func() {} }
	res.bind("agent-recip", "sess-recip")
	startConsumer(t, c)
	fakeFabricOf(c).waitSubscribed(t)

	passes := reads.unroutedCallCount()
	select {
	case tick <- time.Now():
	case <-time.After(testTimeout):
		t.Fatal("consumer loop never read the floor tick")
	}
	reads.waitUnroutedCalls(t, passes+1)
	if n := disp.countFor("m1"); n != 1 {
		t.Fatalf("recovery sweep sent m1 %d times, want 1 (its author is dead)", n)
	}
}
