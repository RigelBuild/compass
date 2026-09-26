//go:build unix

package delivery

// The fabric callback holds concurrently with Run's settle drain, and the start
// sweep runs while an author streams. These cases pin both held-message gaps.

import (
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// heldGapFixture wires a live agent author and a live subscribed recipient over
// a block-capturing dispatcher, with the consumer clock pinned to settleAt.
func heldGapFixture(t *testing.T, settleAt time.Time) (*Consumer, *blockCapturingDispatcher, *fakeReads) {
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
// not strand it until the next settle: the hold sees the settle and fires.
func TestHoldAfterSettleFiresAtOnce(t *testing.T) {
	settleAt := time.UnixMilli(2_000_000)
	c, disp, reads := heldGapFixture(t, settleAt)

	c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	c.waitSettleDrained(t)

	postMessage(t, c, reads, messageAt("m1", "settled body", settleAt.Add(-time.Second)))
	rec := disp.waitFor(t, "m1")
	if rec.sessionID != "sess-recip" || rec.firstText != "settled body" {
		t.Fatalf("deliver = %+v, want {sess-recip, m1, settled body}", rec)
	}
	fakeFabricOf(c).waitAcked(t, "m1")
	if n := disp.countFor("m1"); n != 1 {
		t.Fatalf("m1 dispatched %d times, want exactly 1", n)
	}
	if c.isHeld("sess-author", "m1") {
		t.Fatal("m1 was held after the settle already happened")
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

// The recovery pass drops settle times older than the floor interval and keeps
// fresh ones, so the map stays bounded for authors that never settle again.
func TestRecoveryPrunesStaleSettleTimes(t *testing.T) {
	c, _, _, _ := newTestConsumer(t) //nolint:dogsled // only the consumer's clock and settle map are exercised.
	now := time.UnixMilli(2_000_000)
	c.now = func() time.Time { return now }

	c.OnSessionSettled("sess-old", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	now = now.Add(recoveryFloorInterval + time.Millisecond)
	c.OnSessionSettled("sess-fresh", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	if !c.hasLastSettle("sess-old") || !c.hasLastSettle("sess-fresh") {
		t.Fatal("precondition: both settles should be recorded")
	}

	c.requestRecovery()
	c.drainRecovery(t.Context())
	if c.hasLastSettle("sess-old") {
		t.Fatal("settle time older than the floor interval survived recovery")
	}
	if !c.hasLastSettle("sess-fresh") {
		t.Fatal("fresh settle time was pruned")
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
