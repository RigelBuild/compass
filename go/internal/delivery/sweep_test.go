//go:build unix

package delivery

// SEA-1569 T6 — the reconnect/start redelivery sweep, RED-first. A session-start
// edge (OnSessionStarted, the hub's SessionStartSink hook fired at
// promoteSession) enqueues into the consumer's ctx-rooted loop, which sweeps the
// freshly-live session's owed messages (UndeliveredMessages) and re-dispatches
// them ascending-seq per channel through the recipient's dispatch gate. Each case
// drives the consumer through the real events bus + hand-written fakes and gates
// on the recorder's observed dispatches — never a sleep, never a retry
// (rule://no-retries). context.Background() is the test root
// (rule://go-thread-context exemption for _test.go); it is threaded into Run and
// never re-rooted below.

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Case T6-1: messages posted while NO session was live arrive as delivers on the
// recipient's next start, in ascending seq order per channel, dispatched through
// that session's gate. The recipient is NOT a live-channel subscriber here (it is
// absent from res until the start edge names it), so the owed messages can ONLY
// reach it via the start sweep — their arrival proves the reconnect path.
func TestSessionStartSweepsOwedMessages(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	// Two messages posted on the channel while the recipient had no live session:
	// the cursor owes both, ascending seq per channel.
	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {
			textMessage("owed-1", author, "first"),
			textMessage("owed-2", author, "second"),
		},
	}
	// The session is now live; the start edge names it.
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	disp.waitForDispatches(t, 2)

	got := disp.snapshot()
	if len(got) != 2 {
		t.Fatalf("dispatches = %d, want 2 (both owed messages swept on start)", len(got))
	}
	for _, d := range got {
		if d.sessionID != "sess-recip" {
			t.Fatalf("swept deliver to session %q, want sess-recip", d.sessionID)
		}
	}
	if got[0].messageID != "owed-1" || got[1].messageID != "owed-2" {
		t.Fatalf("sweep order = [%s, %s], want [owed-1, owed-2] (ascending seq per channel)",
			got[0].messageID, got[1].messageID)
	}
}

// Case T6-2: a message already acked is NOT re-swept. The cursor is the source of
// truth: an acked message advances past the cursor (or lands in the above-set),
// so UndeliveredMessages omits it — the sweep never re-issues it. This drives two
// real start edges: the first sweeps the one owed message; between them the owed
// set is emptied (modeling the recipient acking it, cursor advanced); the second
// must dispatch NOTHING more.
//
// The barrier is a SENTINEL start edge, not queue-emptiness. drainStarts pops and
// empties the queue slice under c.mu BEFORE it runs the sweep's store-read +
// gate-held re-dispatch (settle.go:120-123), so waitStartsDrained returns the
// instant the edge is DEQUEUED — before that edge's sweep completes. A snapshot
// gated on it would race a bad re-dispatch. Instead: enqueue a THIRD edge for a
// sentinel session owed exactly one distinct message, then block until that
// message ARRIVES (disp.waitForMessage). Because drainStarts drains FIFO in a
// SINGLE loop goroutine, one sweep at a time, the sentinel's message dispatching
// proves every earlier edge's sweep — including the second sess-recip edge — has
// already run to completion. That arrival is the true post-sweep barrier.
func TestSessionStartDoesNotResweepAckedMessages(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"
	const sentinel store.AccountID = "agent-sentinel"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {textMessage("owed-1", author, "first")},
	}
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	// First start: sweeps the one owed message.
	c.OnSessionStarted("sess-recip", recipient)
	disp.waitForDispatches(t, 1)

	// The recipient acks owed-1; the cursor advances, so it drops out of the owed
	// set (UndeliveredMessages omits it — design.md:360-365).
	reads.mu.Lock()
	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	// Seed the sentinel: a distinct session owed exactly one distinct message. Its
	// arrival is the post-sweep barrier for the second sess-recip edge.
	reads.owed[sentinel] = map[store.ChannelID][]store.Message{
		ch: {textMessage("sentinel-1", author, "barrier")},
	}
	reads.mu.Unlock()
	res.bind(sentinel, "sess-sentinel")

	// Second start: nothing owed now, so nothing re-sweeps. Then the sentinel edge,
	// which drains strictly after it (FIFO, single goroutine).
	c.OnSessionStarted("sess-recip", recipient)
	c.OnSessionStarted("sess-sentinel", sentinel)

	if !disp.waitForMessage(t, "sentinel-1") {
		t.Fatal("sentinel-1 never dispatched (barrier: its sweep drains after the second sess-recip edge)")
	}

	// owed-1 was swept exactly once (never re-swept) and sentinel-1 exactly once.
	var owed1, sent1 int
	for _, d := range disp.snapshot() {
		switch d.messageID {
		case "owed-1":
			owed1++
		case "sentinel-1":
			sent1++
		}
	}
	if owed1 != 1 {
		t.Fatalf("owed-1 dispatched %d times, want 1 (an acked message is not re-swept)", owed1)
	}
	if sent1 != 1 {
		t.Fatalf("sentinel-1 dispatched %d times, want 1 (barrier message)", sent1)
	}
}

// Case T6-3: a live bus event posted mid-sweep queues BEHIND the start sweep and
// lands after it — the per-session dispatch gate serialization (design.md:220-225,
// 828). The start sweep for sess-recip holds the session gate for its whole
// ordered re-dispatch; a live deliver for the SAME session published mid-sweep
// must not dispatch until the sweep releases the gate, and then in order after it.
// Deterministic via the beforeGate seam + the dispatcher's first-call barrier — no
// sleep.
func TestLiveEventQueuesBehindStartSweep(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	// The recipient is a live-channel subscriber (so a live post fans out to it)
	// AND is owed one message the start sweep redelivers.
	reads.subscribers[ch] = []store.AccountID{recipient}
	res.bind(recipient, "sess-recip")
	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {textMessage("swept-1", author, "owed")},
	}
	startConsumer(t, c)

	// Arm the first dispatch (the start sweep's re-dispatch) to block after
	// signaling entry but BEFORE it records — so while it is held, the consumer
	// loop is parked inside the sweep and cannot yet read the live tail.
	disp.armFirstBlock()
	c.OnSessionStarted("sess-recip", recipient)
	<-disp.enteredFirst // the sweep's first re-dispatch is in-flight, loop parked

	// Publish a live deliver for the SAME session while the sweep holds the loop.
	// It buffers on the bus tail: the single-goroutine loop drains the start edge
	// to completion before it ever selects the live event (design.md:220-225 — the
	// sweep runs IN the loop, so live bus events for the session queue behind it;
	// the per-session dispatch gate is the belt-and-suspenders that also holds
	// when a sweep runs off-loop, exercised by TestLiveEventsQueueBehindSweep).
	// Nothing has recorded yet: the armed dispatch blocks before appending.
	c.bus.Publish(postedResponse(wireText("live-1", author, "live")))
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("recorded %d dispatches while the start sweep holds the loop, want 0 (live deliver must queue behind)", len(got))
	}

	// Release the sweep; the swept deliver records first (it held the loop), then
	// the live deliver drains behind it, in order.
	close(disp.releaseFirst)
	disp.waitForDispatches(t, 2)
	got := disp.snapshot()
	if got[0].messageID != "swept-1" {
		t.Fatalf("first dispatch = %q, want swept-1 (the sweep drains before the queued live deliver)", got[0].messageID)
	}
	if got[1].messageID != "live-1" {
		t.Fatalf("second dispatch = %q, want live-1 (queued behind the start sweep)", got[1].messageID)
	}
}

// Case T6-4 (guard): OnSessionStarted with an empty session id or account is a
// no-op — it enqueues no start edge and sweeps nothing, so a promotion that named
// no account (the fail-closed binding path) never triggers a spurious sweep.
func TestSessionStartIgnoresEmptyBinding(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		"chan-1": {textMessage("owed-1", "human-1", "first")},
	}
	startConsumer(t, c)

	c.OnSessionStarted("", recipient)
	c.OnSessionStarted("sess-recip", "")
	// A valid throwaway start with nothing owed gives a drained barrier proving
	// the empty edges were dropped without a sweep.
	c.OnSessionStarted("sess-throwaway", "acct-nothing-owed")
	c.waitStartsDrained(t)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (empty session/account is a no-op)", len(got))
	}
}

// RIG-2486 T1 (sweep coverage): the reconnect/start sweep denormalizes the
// author's handle onto each redelivered deliver op (sweepSession, settle.go:266,
// deliverOp(wire, c.authorHandle(ctx, wire))). Redelivery is exactly where
// from_handle is load-bearing — an idle/reconnecting peer receives the deliver
// via the sweep, not the live fan-out. Seeds the author's account and asserts
// the swept deliver carries its handle. Mirrors
// TestDeliverAndSteerCarryAuthorFromHandle's assertion (mention_test.go:261).
func TestSweepSessionCarriesAuthorFromHandle(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {textMessage("owed-1", author, "first")},
	}
	// The author's account resolves its handle for the denormalized from_handle.
	reads.accounts[author] = store.Account{ID: author, Handle: "matt"}
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 || got[0].kind != opDeliver || got[0].messageID != "owed-1" {
		t.Fatalf("dispatch = %+v, want one deliver of owed-1", got)
	}
	if got[0].fromHandle != "matt" {
		t.Fatalf("from_handle = %q on swept deliver, want matt", got[0].fromHandle)
	}
}
