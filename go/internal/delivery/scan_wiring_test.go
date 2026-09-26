//go:build unix

package delivery

// RIG-2490 T3 — the recovery scan wired at both consumer recovery points and the
// mention-routed mark stamped on the live settle path, RED-first. Each case
// drives the consumer through the fake fabric + fakes and gates on the
// observable durable effect (owed row, mark), never a sleep.

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Start-scan leg: a committed-but-unmarked mention whose ref was never published
// is recovered by Run's start scan. The offline out-of-sweep-set member gets a
// durable owed row and the message is marked; without the scan the owed row
// never appears (the RED).
func TestStartScanRecoversMissedMention(t *testing.T) {
	c, _, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// agentA offline (never bound) and out of the sweep set (sweepSet unseeded).
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)

	startConsumer(t, c)

	reads.waitForOwed(t, agentA, 1)
	if got := reads.markCount("m1"); got != 1 {
		t.Fatalf("marks for m1 = %d, want 1 (the start scan marks the recovered message)", got)
	}
}

// The start scan finishes before Run subscribes, so no event is handled until
// the committed-but-unmarked set has been recovered.
func TestStartScanCompletesBeforeSubscribe(t *testing.T) {
	c, _, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)
	fab := fakeFabricOf(c)
	marksAtSubscribe := -1
	fab.beforeSubscribe = func() { marksAtSubscribe = reads.markCount("m1") }
	startConsumer(t, c)
	fab.waitSubscribed(t)

	if marksAtSubscribe != 1 {
		t.Fatalf("marks for m1 when Run subscribed = %d, want 1 (the start scan must finish first)", marksAtSubscribe)
	}
}

// A serve shutdown that lands while Run is subscribing is a clean stop, not a
// Run error: the serve group would otherwise report a failed shutdown.
func TestRunReturnsNilWhenCancelledWhileSubscribing(t *testing.T) {
	fab := newFakeFabric()
	c := NewConsumer(newFakeReads(), newFakeDispatcher(), newFakeResolver(), fab, discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	fab.beforeSubscribe = cancel

	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run cancelled while subscribing = %v, want nil", err)
	}
}

// Reconnect leg: a mention committed after Run subscribed, whose publish failed,
// is invisible to the start scan; the scan a fabric reconnect triggers recovers
// it. Without the reconnect hook the owed row never appears (the RED).
func TestReconnectScansMissedMention(t *testing.T) {
	c, _, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	startConsumer(t, c)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)

	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)
	fab.fireReconnect()

	reads.waitForOwed(t, agentA, 1)
}

// Live-path mark: a live settle pass through fanOut marks the message, so a
// subsequent recovery scan finds nothing to re-route for it. The mark is stamped
// before the deliver dispatch, so waitForDispatches guarantees it ran. Without
// the wiring the message is never marked (the RED).
func TestLivePathMarksMentionsRouted(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	res.bind(agentA, "sess-a")
	// The message is committed-but-unmarked (in the unrouted set) until its live
	// settle edge fires fanOut and marks it. Drive fanOut directly to isolate the
	// live-path mark from Run's start scan.
	reads.seedUnrouted(textMessage("m1", author, "hello"), ch, 1)

	c.fanOut(context.Background(), ch, author, wireText("m1", author, "hello"))
	disp.waitForDispatches(t, 1)

	if got := reads.markCount("m1"); got != 1 {
		t.Fatalf("marks for m1 = %d, want 1 (the live settle pass marks the message)", got)
	}
	// A follow-up recovery scan processes nothing for m1: the live mark removed
	// it from the unrouted set, so it is not re-routed or re-marked.
	c.scanMissedMentions(context.Background())
	if got := reads.markCount("m1"); got != 1 {
		t.Fatalf("marks for m1 = %d, want 1 after a follow-up scan (a marked message is not re-processed)", got)
	}
}
