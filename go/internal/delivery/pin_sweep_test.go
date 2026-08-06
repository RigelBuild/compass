//go:build unix

package delivery

// SEA-1723 T7 — the session-start pin sweep, RED-first. A session-start edge
// (OnSessionStarted) drains through the loop's drainStarts, which runs the
// EXISTING cursor sweep (sweepSession) and then the new sibling pin step
// (sweepPins): for every channel the agent sweeps (SweepChannels, the D1
// disjunct), each PinnedEntry's message is dispatched as a DeliverControl
// REGARDLESS of cursor position (design.md:640-663). Each case drives the
// consumer through the real events bus + hand-written fakes and gates on the
// recorder's observed dispatches — never a sleep. context.Background() is the
// test root (rule://go-thread-context exemption for _test.go); it is threaded
// into Run and never re-rooted below.

import (
	"testing"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// Case T7-1: a FRESH session receives the channel's current pins even when the
// delivery cursor is already caught up past the pinned message (acked_seq >= pin
// seq), so the cursor sweep owes NOTHING. The pin's message is absent from the
// owed set (UndeliveredMessages returns empty) yet the pin sweep still injects
// it, because it dispatches regardless of cursor position (design.md:660-661).
// The channel is in the agent's SweepChannels set; the pinned message resolves
// via the message table. Its arrival is the whole point of the pin sweep.
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
// message via PostMessage → a normal MessagePosted on the bus → normal fan-out
// to live subscribers (design.md:642-645). Here NO session-start edge fires, so
// sweepPins is deliberately NOT exercised in this case: it only asserts that the
// pre-existing D1 live path emits the edit's message exactly ONCE. The
// sweep-side double-dispatch (a pin also owed by the cursor sweep) is covered
// separately by TestPinSweepIsUnconditionalWhenAlsoOwed.
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

// Case T7-3: a message that is BOTH owed (cursor sweep) AND pinned (pin sweep) is
// dispatched by BOTH sweeps server-side, on the one start pass — the pin sweep
// does NOT itself skip an id the cursor sweep already dispatched this pass
// (design.md:652-654: dispatch "REGARDLESS of cursor position"; no server-side
// dedup). The single-delivery "injected once" guarantee is the AGENT-SIDE
// per-session message_id dedup (DL-073/T5, design.md:263-264, :658-659) and is
// out of scope for the Go delivery pkg — it belongs to an agent-side/integration
// test, NOT a one-dispatch assertion here. This case asserts the server-observable
// truth: the pin sweep is unconditional, so the owed+pinned message is dispatched
// TWICE (once per sweep) and the pin sweep never conditions on the cursor.
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
