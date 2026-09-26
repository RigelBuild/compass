//go:build unix

package delivery

// RIG-1569 T6 — the reconnect/start redelivery sweep, RED-first. A session-start
// edge enqueues into the consumer's loop, which sweeps the freshly-live session's
// owed messages and re-dispatches them ascending-seq per channel. Each case gates
// on the recorder's observed dispatches — never a sleep, never a retry.

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Case T6-1: messages posted while NO session was live arrive as delivers on the
// recipient's next start, in ascending seq order per channel. The recipient is NOT
// a live-channel subscriber here, so the owed messages can ONLY reach it via the
// start sweep — their arrival proves the reconnect path.
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

// Case T6-2: an already-acked message is NOT re-swept — it advances past the cursor,
// so UndeliveredMessages omits it. Two start edges: the first sweeps the owed
// message, the owed set is emptied (the ack), the second dispatches NOTHING. Barrier
// is a SENTINEL edge whose arrival (FIFO single-loop drain) proves every earlier sweep ran.
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

// Case T6-3: a live event posted mid-sweep queues BEHIND the start sweep — the
// per-session dispatch gate serialization. The start sweep holds the gate for its
// whole ordered re-dispatch; a live deliver for the SAME session must not dispatch
// until the sweep releases the gate, then in order. Deterministic via beforeGate.
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
	// beforeGate signals when the live deliver reaches the session gate, so the
	// no-dispatch check below runs only once that deliver is provably queued.
	atGate := make(chan struct{}, 1)
	c.beforeGate = func(sessionID string) {
		if sessionID == "sess-recip" {
			atGate <- struct{}{}
		}
	}
	startConsumer(t, c)

	// Arm the first dispatch (the start sweep's re-dispatch) to block after
	// signaling entry but BEFORE it records, so the sweep holds the recipient's
	// session gate while it is blocked.
	disp.armFirstBlock()
	c.OnSessionStarted("sess-recip", recipient)
	<-disp.enteredFirst // the sweep's first re-dispatch is in-flight, holding the gate

	// Publish a live deliver for the SAME session while the sweep is in flight.
	// The callback blocks on the session gate the sweep holds; wait until it
	// reaches that gate, then check nothing has recorded.
	postMessage(t, c, reads, textMessage("live-1", author, "live"))
	<-atGate
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("recorded %d dispatches while the start sweep holds the gate, want 0 (live deliver must queue behind)", len(got))
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
// author's handle onto each redelivered deliver op. Redelivery is exactly where
// from_handle is load-bearing — an idle/reconnecting peer receives the deliver via
// the sweep, not the live fan-out. Mirrors TestDeliverAndSteerCarryAuthorFromHandle.
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

// RIG-2956 T0 (sweep coverage): the reconnect/start sweep denormalizes the source
// channel+topic names onto each redelivered deliver op — where an idle/reconnecting
// peer renders "Channel <name> › topic <name>:" off the swept deliver, not the live
// fan-out. RED if the settle site drops the names.
func TestSweepSessionCarriesSourceChannelAndTopicNames(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {textMessage("owed-1", author, "first")},
	}
	// The message's topic ("topic-1", the textMessage default) resolves to its
	// source channel+topic names.
	reads.seedTopicNames("topic-1", "engineering", "general")
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 || got[0].kind != opDeliver || got[0].messageID != "owed-1" {
		t.Fatalf("dispatch = %+v, want one deliver of owed-1", got)
	}
	if got[0].channelName != "engineering" || got[0].topicName != "general" {
		t.Fatalf("swept deliver source names = (%q, %q), want (engineering, general)", got[0].channelName, got[0].topicName)
	}
}
