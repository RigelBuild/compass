//go:build unix

package delivery

// RIG-1723 T7 — the session-start pin sweep, RED-first. A session-start edge runs
// the cursor sweep then the sibling pin step (sweepPins): for every channel the
// agent sweeps, each PinnedEntry's message is dispatched REGARDLESS of cursor
// position. Each case gates on the recorder's observed dispatches — never a sleep.

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// Case T7-1: a FRESH session receives the channel's current pins even when the
// cursor is already caught up past the pinned message, so the cursor sweep owes
// NOTHING. The pin's message is absent from the owed set yet the pin sweep injects
// it, because it dispatches regardless of cursor position.
func TestPinSweepDeliversCurrentPinsWhenCursorCaughtUp(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	// The cursor owes nothing (acked_seq >= pin seq): the cursor sweep is empty.
	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	// The agent sweeps this channel, whose board pins one message the cursor
	// already covers.
	reads.sweepChannels[recipient] = []store.ChannelID{ch}
	reads.pins[ch] = []store.PinnedEntry{{MessageID: "pinned-1", Position: 0}}
	reads.seedMessage(textMessage("pinned-1", author, "the pinned board"))

	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	if !disp.waitForMessage(t, "pinned-1") {
		t.Fatal("pinned-1 never dispatched: a fresh session must receive current pins even when acked_seq >= pin seq")
	}

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only the pin; the cursor sweep owed nothing)", len(got))
	}
	if got[0].sessionID != "sess-recip" || got[0].kind != opDeliver {
		t.Fatalf("pin dispatch = {session %q, kind %d}, want {sess-recip, deliver}", got[0].sessionID, got[0].kind)
	}
}

// Case T7-2: pins the D1 live-edit path in isolation. A board edit mints a NEW
// message → a normal MessagePosted → normal fan-out. NO session-start edge fires, so
// sweepPins is NOT exercised: it only asserts the D1 live path emits the edit's
// message exactly ONCE (the sweep-side double-dispatch is a separate test).
func TestPinSweepDoesNotDoubleHandleLiveEdit(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	// The recipient is a live subscriber. The board currently pins the edit's new
	// message id — but no session-start edge fires, so the pin sweep never runs.
	reads.subscribers[ch] = []store.AccountID{recipient}
	reads.sweepChannels[recipient] = []store.ChannelID{ch}
	reads.pins[ch] = []store.PinnedEntry{{MessageID: "edit-2", Position: 0}}
	reads.seedMessage(textMessage("edit-2", author, "edited board"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	// The edit's new message rides D1: a live MessagePosted fans out once.
	c.bus.Publish(postedResponse(wireText("edit-2", author, "edited board")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (the live edit fans out once via D1; the pin sweep did not run)", len(got))
	}
	if got[0].messageID != "edit-2" {
		t.Fatalf("dispatched %q, want edit-2 (the live edit path)", got[0].messageID)
	}
}

// Case T7-3: a message BOTH owed (cursor sweep) AND pinned (pin sweep) is dispatched
// by BOTH sweeps server-side on the one start pass — the pin sweep does NOT skip an
// id the cursor sweep already dispatched (no server-side dedup; the single-delivery
// guarantee is AGENT-SIDE, DL-073/T5). Asserts the owed+pinned message dispatches TWICE.
func TestPinSweepIsUnconditionalWhenAlsoOwed(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	// The same message is owed (cursor sweep dispatches it) AND pinned (pin sweep
	// dispatches it): both fire this start pass.
	reads.owed[recipient] = map[store.ChannelID][]store.Message{
		ch: {textMessage("dup-1", author, "owed and pinned")},
	}
	reads.sweepChannels[recipient] = []store.ChannelID{ch}
	reads.pins[ch] = []store.PinnedEntry{{MessageID: "dup-1", Position: 0}}
	reads.seedMessage(textMessage("dup-1", author, "owed and pinned"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	// Two dispatches of the same id: the cursor sweep and the pin sweep each emit
	// it. Server-side dedup is deliberately NOT applied (design-literal).
	disp.waitForDispatches(t, 2)

	var dup int
	for _, d := range disp.snapshot() {
		if d.messageID == "dup-1" {
			dup++
		}
	}
	if dup != 2 {
		t.Fatalf("dup-1 dispatched %d times, want 2 (cursor sweep + pin sweep, both unconditional; agent-side dedup collapses to one delivery — out of scope here)", dup)
	}
}

// RIG-2880: the pin sweep preserves the author handle from its store row on the
// delivered wire Message and DeliverControl.
func TestSweepPinsCarriesAuthorFromHandle(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	reads.sweepChannels[recipient] = []store.ChannelID{ch}
	reads.pins[ch] = []store.PinnedEntry{{MessageID: "pinned-1", Position: 0}}
	reads.seedMessage(textMessage("pinned-1", author, "the pinned board"))
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	if !disp.waitForMessage(t, "pinned-1") {
		t.Fatal("pinned-1 never dispatched: a fresh session must receive current pins")
	}

	got := disp.snapshot()
	if len(got) != 1 || got[0].kind != opDeliver || got[0].messageID != "pinned-1" {
		t.Fatalf("dispatch = %+v, want one deliver of pinned-1", got)
	}
	if got[0].fromHandle != "matt" || got[0].messageAuthorHandle != "matt" {
		t.Fatalf("pin author handles = (%q, %q), want (matt, matt)", got[0].messageAuthorHandle, got[0].fromHandle)
	}
}

// RIG-2956 T0 (pin-sweep coverage): the pin sweep denormalizes the source
// channel+topic names onto each re-delivered pin op. Reuses
// TestSweepPinsCarriesAuthorFromHandle's harness; RED if the settle site drops
// the names.
func TestSweepPinsCarriesSourceChannelAndTopicNames(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	reads.sweepChannels[recipient] = []store.ChannelID{ch}
	reads.pins[ch] = []store.PinnedEntry{{MessageID: "pinned-1", Position: 0}}
	reads.seedMessage(textMessage("pinned-1", author, "the pinned board"))
	reads.seedTopicNames("topic-1", "engineering", "general")
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	if !disp.waitForMessage(t, "pinned-1") {
		t.Fatal("pinned-1 never dispatched: a fresh session must receive current pins")
	}

	got := disp.snapshot()
	if len(got) != 1 || got[0].kind != opDeliver || got[0].messageID != "pinned-1" {
		t.Fatalf("dispatch = %+v, want one deliver of pinned-1", got)
	}
	if got[0].channelName != "engineering" || got[0].topicName != "general" {
		t.Fatalf("pin deliver source names = (%q, %q), want (engineering, general)", got[0].channelName, got[0].topicName)
	}
}
