//go:build unix

package delivery

// RIG-2490 T3 — the recovery scan wired at both consumer recovery points and the
// mention-routed mark stamped on the live settle path, RED-first. Each case
// drives the consumer through the real events bus + hand-written fakes and gates
// on the observable durable effect (owed row, mark), never a sleep.
// context.Background() is the test root (rule://go-thread-context exemption for
// _test.go); it is threaded into Run via startConsumer and into a direct scan
// call, never re-rooted below.

import (
	"context"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Start-scan leg: a committed-but-unmarked mention whose bus event is ABSENT
// (never replayed or delivered live) is recovered by Run's start scan — the
// offline out-of-sweep-set member gets a durable owed row and the message is
// marked. Without the wiring the owed row never appears (the RED).
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

// Overrun leg: a committed-but-unmarked mention dropped in the bus-lag overrun
// window is recovered by the overrun-branch scan. Driven exactly like the
// bus-lag resync cases (arm the first dispatch, overrun the live buffer,
// release), gating on the afterResubscribe seam — the scan runs before it fires,
// so once resubscribed is closed the scan has completed. Without the wiring the
// owed row never appears (the RED).
func TestOverrunBranchScansMissedMention(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const liveAgent store.AccountID = "agent-live"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{liveAgent}
	res.bind(liveAgent, "sess-live")
	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// m1 is seeded AFTER the start scan has run (below, past <-disp.enteredFirst)
	// so Run's start scan reads an empty unrouted set and cannot recover it —
	// only the overrun-branch scan can. Without that isolation the start scan
	// would grab m1 at loop entry and this test would pass with the overrun-branch
	// scan removed (a false guard).

	resubscribed := make(chan struct{})
	c.afterResubscribe = func() { close(resubscribed) }

	disp.armFirstBlock()
	startConsumer(t, c)

	// First event consumed off Live, then stalls in the armed dispatch. Entry
	// here is strictly after Run's start scan completed, so seeding m1 now hides
	// it from that scan; only the overrun-branch scan can recover it.
	c.bus.Publish(postedResponse(wireText("m0", author, "first")))
	<-disp.enteredFirst
	reads.seedUnrouted(textMessage("m1", author, "@aa ping"), ch, 1)
	// Overrun the live buffer so the channel closes lagged.
	for range busLagFloodCount {
		c.bus.Publish(postedResponse(wireText("flood", author, "x")))
	}
	close(disp.releaseFirst)

	select {
	case <-resubscribed:
	case <-time.After(testTimeout):
		t.Fatal("consumer never re-subscribed after the lag overrun")
	}
	if n := reads.owedCount(agentA); n != 1 {
		t.Fatalf("owed rows for agentA = %d, want 1 (the overrun-branch scan recovers the dropped-window mention)", n)
	}
	if got := reads.markCount("m1"); got != 1 {
		t.Fatalf("marks for m1 = %d, want 1 (the overrun-branch scan marks the recovered message)", got)
	}
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
