//go:build unix

package delivery

// The fabric callback holds concurrently with Run's settle drain, and the start
// sweep runs while an author streams. These cases pin both held-message gaps.

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
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

			c.OnSessionSettled("sess-author", tc.state, 0)
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

			c.OnSessionSettled("sess-author", state, 0)
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

// The edge a late hold queues must fire only that turn's messages. A message
// of the next turn, still streaming, stays held until its own settle.
func TestFireNowSparesNextTurnMessage(t *testing.T) {
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
	// Also runs before startConsumer's cleanup, so a failed assert never leaves
	// the loop parked in the hook.
	unpark := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unpark)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)

	postMessage(t, c, reads, messageAt("m1", "m1 body", time.UnixMilli(100)))
	c.waitHeld(t, "sess-author", 1)
	armed.Store(true)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	select {
	case <-entered:
	case <-time.After(testTimeout):
		t.Fatal("fireHeld never re-read m1")
	}
	postMessage(t, c, reads, messageAt("m-late", "late body", time.UnixMilli(150)))
	postMessage(t, c, reads, messageAt("m-next", "next partial", time.UnixMilli(300)))
	fab.waitAcked(t, "m-late")
	fab.waitAcked(t, "m-next")
	unpark()

	disp.waitFor(t, "m-late")
	// The loop drains edges in order, so once this one is popped every earlier
	// fire returned.
	c.OnSessionSettled("sess-other", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	c.waitSettleDrained(t)
	got := disp.records()
	if len(got) != 2 || got[0].messageID != "m1" || got[1].messageID != "m-late" {
		t.Fatalf("delivers = %+v, want exactly [m1, m-late]", got)
	}
	if !c.isHeld("sess-author", "m-next") {
		t.Fatal("m-next is no longer held before its own turn settled")
	}

	reads.seedMessage(messageAt("m-next", "next settled", time.UnixMilli(300)))
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	rec := disp.waitFor(t, "m-next")
	if rec.sessionID != "sess-recip" || rec.firstText != "next settled" {
		t.Fatalf("m-next deliver = %+v, want {sess-recip, next settled}", rec)
	}
	if n := disp.countFor("m-next"); n != 1 {
		t.Fatalf("m-next dispatched %d times, want exactly 1", n)
	}
}

// A queued settle for turn N must not fire a held turn N+1 post that raced ahead
// of its drain. The fake store exposes different partial and settled block sets.
// Turn N's queued edge cannot consume a raced-ahead N+1 post or its partial blocks.
func TestQueuedSettleFiresOnlyItsTurn(t *testing.T) {
	c, disp, reads := newHeldGapConsumer(t, time.UnixMilli(500))
	post := func(m store.Message) {
		t.Helper()
		reads.seedMessage(m)
		ref := fabric.EventRef{Tenant: string(testTenant), Kind: fabric.KindMessagePosted, RowID: string(m.ID)}
		if err := c.onEventRef(t.Context(), ref); err != nil {
			t.Fatalf("onEventRef(%s): %v", m.ID, err)
		}
	}

	mN := messageAt("mN", "turn N partial", time.UnixMilli(100))
	mN.TurnSequence = 10
	mNext := messageAt("mNext", "turn N+1 partial", time.UnixMilli(200))
	mNext.TurnSequence = 11
	post(mN)
	post(mNext)
	if !c.isHeld("sess-author", "mN") || !c.isHeld("sess-author", "mNext") {
		t.Fatal("both turn messages must be held before the settle drains")
	}
	reads.seedMessage(messageAt("mN", "turn N settled", time.UnixMilli(100)))
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 10)
	c.drainSettles(t.Context())

	rec := disp.waitFor(t, "mN")
	if rec.firstText != "turn N settled" {
		t.Fatalf("turn N blocks = %q, want settled blocks", rec.firstText)
	}
	if n := disp.countFor("mNext"); n != 0 {
		t.Fatalf("turn N+1 dispatched %d times after turn N settled, want 0", n)
	}
	if !c.isHeld("sess-author", "mNext") {
		t.Fatal("turn N+1 must remain held after turn N settles")
	}

	reads.seedMessage(messageAt("mNext", "turn N+1 settled", time.UnixMilli(200)))
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 11)
	c.drainSettles(t.Context())
	rec = disp.waitFor(t, "mNext")
	if rec.firstText != "turn N+1 settled" {
		t.Fatalf("turn N+1 blocks = %q, want settled blocks", rec.firstText)
	}
	if n := disp.countFor("mNext"); n != 1 {
		t.Fatalf("turn N+1 dispatched %d times, want exactly 1", n)
	}

	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 10)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 11)
	c.drainSettles(t.Context())
	if n := disp.countFor("mN"); n != 1 {
		t.Fatalf("turn N dispatched %d times after repeated edges, want exactly 1", n)
	}
	if n := disp.countFor("mNext"); n != 1 {
		t.Fatalf("turn N+1 dispatched %d times after repeated edges, want exactly 1", n)
	}
}

// Zero sequence preserves fire-all behavior and logs once; late holds compare turns.
func TestSettleSequenceCompatibilityAndLateHold(t *testing.T) {
	t.Run("legacy settle logs once and fires all", func(t *testing.T) {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		disp := newBlockCapturingDispatcher()
		res := newFakeResolver()
		reads := newFakeReads()
		c := NewConsumer(reads, disp, res, newFakeFabric(), logger)
		reads.subscribers["chan-1"] = []store.AccountID{"agent-recip"}
		reads.agents["agent-author"] = true
		res.bind("agent-author", "sess-author")
		res.bind("agent-recip", "sess-recip")
		for _, id := range []string{"m1", "m2"} {
			m := messageAt(id, id+" settled", time.UnixMilli(100))
			m.TurnSequence = 5
			reads.seedMessage(m)
			c.hold(store.WithTenant(t.Context(), testTenant), "sess-author", id, 100, 5)
		}
		c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
		c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
		c.drainSettles(t.Context())
		if got := len(disp.records()); got != 2 {
			t.Fatalf("legacy settle dispatched %d messages, want 2", got)
		}
		if got := strings.Count(logs.String(), "legacy fire-all fallback"); got != 1 {
			t.Fatalf("legacy fallback logs = %d, want exactly 1", got)
		}
	})

	t.Run("numbered late hold ignores settle timestamp", func(t *testing.T) {
		c, disp, reads := newHeldGapConsumer(t, time.UnixMilli(100))
		c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 10)
		c.drainSettles(t.Context())
		m := messageAt("m-late-sequence", "settled blocks", time.UnixMilli(50_000))
		m.TurnSequence = 10
		post := fabric.EventRef{Tenant: string(testTenant), Kind: fabric.KindMessagePosted, RowID: string(m.ID)}
		reads.seedMessage(m)
		if err := c.onEventRef(t.Context(), post); err != nil {
			t.Fatalf("onEventRef(late sequence): %v", err)
		}
		c.drainSettles(t.Context())
		if rec := disp.waitFor(t, "m-late-sequence"); rec.firstText != "settled blocks" {
			t.Fatalf("late hold blocks = %q, want settled blocks", rec.firstText)
		}
		if c.isHeld("sess-author", "m-late-sequence") {
			t.Fatal("late hold remained held after its own numbered settle")
		}
	})
}

func TestNumberedSettleFiresLegacyHeldEntry(t *testing.T) {
	c, disp, reads := newHeldGapConsumer(t, time.UnixMilli(100))
	m := messageAt("m-legacy", "legacy settled", time.UnixMilli(50))
	reads.seedMessage(m)
	c.hold(t.Context(), "sess-author", "m-legacy", 50, 0)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 10)
	c.drainSettles(t.Context())
	if rec := disp.waitFor(t, "m-legacy"); rec.firstText != "legacy settled" {
		t.Fatalf("legacy held blocks = %q, want settled blocks", rec.firstText)
	}
}

// Out-of-order numbered edges retain the highest observed sequence for late posts.
func TestOutOfOrderSettleKeepsHighestSequence(t *testing.T) {
	c, disp, reads := newHeldGapConsumer(t, time.UnixMilli(500))
	message := messageAt("m11", "turn 11 settled", time.UnixMilli(110))
	message.TurnSequence = 11
	reads.seedMessage(message)
	c.hold(t.Context(), "sess-author", "m11", message.At.UnixMilli(), 11)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 11)
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 10)
	c.drainSettles(t.Context())
	if n := disp.countFor("m11"); n != 1 {
		t.Fatalf("turn 11 dispatched %d times after out-of-order settles, want exactly 1", n)
	}

	late := messageAt("m10-late", "late turn 10 settled", time.UnixMilli(20_000))
	late.TurnSequence = 10
	reads.seedMessage(late)
	ref := fabric.EventRef{Tenant: string(testTenant), Kind: fabric.KindMessagePosted, RowID: string(late.ID)}
	if err := c.onEventRef(t.Context(), ref); err != nil {
		t.Fatalf("onEventRef(late turn 10): %v", err)
	}
	c.drainSettles(t.Context())
	if rec := disp.waitFor(t, "m10-late"); rec.firstText != "late turn 10 settled" {
		t.Fatalf("late turn 10 blocks = %q, want settled blocks", rec.firstText)
	}
	if n := disp.countFor("m10-late"); n != 1 {
		t.Fatalf("late turn 10 dispatched %d times, want exactly 1", n)
	}
}

// A numbered post newer than a legacy settle is not inferred from its timestamp.
func TestNumberedLateHoldDoesNotUseLegacyTimestamp(t *testing.T) {
	c, disp, reads := newHeldGapConsumer(t, time.UnixMilli(100))
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	c.drainSettles(t.Context())
	m := messageAt("m-late-legacy", "still streaming", time.UnixMilli(200))
	m.TurnSequence = 10
	reads.seedMessage(m)
	ref := fabric.EventRef{Tenant: string(testTenant), Kind: fabric.KindMessagePosted, RowID: string(m.ID)}
	if err := c.onEventRef(t.Context(), ref); err != nil {
		t.Fatalf("onEventRef(late legacy): %v", err)
	}
	c.drainSettles(t.Context())
	if n := disp.countFor("m-late-legacy"); n != 0 {
		t.Fatalf("numbered late hold dispatched %d times from an earlier legacy timestamp, want 0", n)
	}
	if !c.isHeld("sess-author", "m-late-legacy") {
		t.Fatal("numbered hold must remain held when prior legacy settle predates its commit")
	}
}

// Control: a message committed after the recorded settle belongs to a later
// turn, so it is held until the next settle.
func TestHoldAfterSettleKeepsLaterMessageHeld(t *testing.T) {
	settleAt := time.UnixMilli(2_000_000)
	c, disp, reads := heldGapFixture(t, settleAt)

	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	c.waitSettleDrained(t)

	postMessage(t, c, reads, messageAt("m2", "next turn", settleAt.Add(time.Second)))
	c.waitHeld(t, "sess-author", 1)
	if n := disp.countFor("m2"); n != 0 {
		t.Fatalf("m2 dispatched %d times before the next settle, want 0", n)
	}

	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
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

	c.OnSessionSettled("sess-live", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	c.OnSessionSettled("sess-dead", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
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
	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
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
	c.hold(t.Context(), "sess-dead", "m1", 0, 0)
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
