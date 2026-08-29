//go:build unix

package delivery

// RIG-1641 T2 — the offline-mention redelivery arms, RED-first: the routeMentions
// offline arm (record an owed row for an out-of-sweep-set member, wake all
// offline mentioned members), the fanOut deliver arm (wake an offline subscriber,
// no owed row), and the sweepOwedMentions start-edge step (dispatch every owed
// mention as a STEER, clearing a permanently-unreadable row). Each case drives
// the consumer through the real events bus + hand-written fakes and gates on the
// recorder's observed dispatches / wakes — never a sleep. context.Background() is
// the test root (rule://go-thread-context exemption for _test.go); it is threaded
// into Run via startConsumer and never re-rooted below.

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// withWaker wires a fresh counting waker onto the consumer and returns it.
func withWaker(c *Consumer) *fakeWaker {
	w := newFakeWaker()
	c.SetAgentWaker(w)
	return w
}

// Case 1: an offline UNSUBSCRIBED mentioned member (out of the sweep set) gets a
// durable owed row recorded AND a wake — but no immediate dispatch (there is no
// live session to steer). The owed row is the no-loss backstop; the wake is the
// latency path.
func TestOfflineUnsubscribedMentionRecordsOwedAndWakes(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// agentA is offline (never bound) and NOT in the sweep set (sweepSet unseeded).
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa ping")))
	w.waitForWakes(t, 1)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline member: no live session to steer)", len(got))
	}
	if n := reads.owedCount(agentA); n != 1 {
		t.Fatalf("owed rows for agentA = %d, want 1 (out-of-sweep-set offline mention records durably)", n)
	}
	if got := w.count(agentA); got != 1 {
		t.Fatalf("wakes for agentA = %d, want 1", got)
	}
}

// Case 2: an offline SUBSCRIBED (in-sweep-set) mentioned member gets a wake but
// NO owed row — the cursor sweep is its durable backstop, so recording would be
// redundant.
func TestOfflineSubscribedMentionWakesNoOwed(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	reads.sweepSet[agentA] = map[store.ChannelID]bool{ch: true} // in the sweep set
	// agentA is offline (never bound).
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa ping")))
	w.waitForWakes(t, 1)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline member)", len(got))
	}
	if n := reads.owedCount(agentA); n != 0 {
		t.Fatalf("owed rows for agentA = %d, want 0 (in-sweep-set: the cursor sweep is the backstop)", n)
	}
	if got := w.count(agentA); got != 1 {
		t.Fatalf("wakes for agentA = %d, want 1", got)
	}
}

// Case 3: an offline SUBSCRIBED unmentioned recipient (the deliver arm) gets a
// wake but NO owed row — the cursor sweep is its backstop. Pure latency path.
func TestOfflineSubscriberDeliverArmWakesNoOwed(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	// agentA is a subscriber, NOT mentioned, and offline (never bound).
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "no mention here")))
	w.waitForWakes(t, 1)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline subscriber)", len(got))
	}
	if n := reads.owedCount(agentA); n != 0 {
		t.Fatalf("owed rows for agentA = %d, want 0 (deliver arm records no owed row)", n)
	}
	if got := w.count(agentA); got != 1 {
		t.Fatalf("wakes for agentA = %d, want 1", got)
	}
}

// Case 4: a broadcast @agents to N offline members wakes each (N wakes), and
// records owed rows only for the out-of-sweep-set members. Here agentA is
// out-of-sweep-set (owed), agentB is in-sweep-set (no owed) — both woken.
func TestBroadcastMentionWakesAllOwedOnlyOutOfSweepSet(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.members[ch] = []store.AccountID{agentA, agentB}
	reads.subscribers[ch] = []store.AccountID{agentB}
	reads.sweepSet[agentB] = map[store.ChannelID]bool{ch: true} // B is in the sweep set
	// A is out of the sweep set (unseeded). Both offline.
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@agents standup")))
	w.waitForWakes(t, 2)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (both offline)", len(got))
	}
	if got := w.total(); got != 2 {
		t.Fatalf("total wakes = %d, want 2 (wake-all across offline mentioned members)", got)
	}
	if got := w.count(agentA); got != 1 {
		t.Fatalf("wakes for agentA = %d, want 1 (each mentioned member woken individually)", got)
	}
	if got := w.count(agentB); got != 1 {
		t.Fatalf("wakes for agentB = %d, want 1 (each mentioned member woken individually)", got)
	}
	if n := reads.owedCount(agentA); n != 1 {
		t.Fatalf("owed rows for agentA = %d, want 1 (out of sweep set)", n)
	}
	if n := reads.owedCount(agentB); n != 0 {
		t.Fatalf("owed rows for agentB = %d, want 0 (in sweep set)", n)
	}
}

// Case 5: a start edge dispatches the freshly-live session's owed mention as a
// STEER, even though the cursor sweep (UndeliveredMessages) owes NOTHING —
// sweepOwedMentions is independent of the cursor sweep.
func TestStartEdgeSweepsOwedMentionAsSteer(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	// The cursor sweep owes nothing; the pin sweep visits no channel.
	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	// A durable owed mention exists for the recipient, its message re-readable.
	reads.seedMessage(textMessage("owed-1", author, "@recip you were paged"))
	reads.seedOwedMention(recipient, ch, textMessage("owed-1", author, "@recip you were paged"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	if !disp.waitForMessage(t, "owed-1") {
		t.Fatal("owed-1 never dispatched: sweepOwedMentions must dispatch an owed mention independent of the cursor sweep")
	}

	got := disp.snapshot()
	if len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (only the owed mention; the cursor sweep owed nothing)", len(got))
	}
	if got[0].sessionID != "sess-recip" || got[0].messageID != "owed-1" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want {sess-recip, owed-1, steer}", got[0])
	}
}

// Case 6: an ack clears the owed row (T1), so a re-sweep after the ack dispatches
// nothing. Modeled by clearing the owed row (the ack's effect) between two start
// edges: the first sweeps the owed mention, the second sweeps nothing.
func TestOwedMentionResweepAfterAckDispatchesNothing(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	reads.seedMessage(textMessage("owed-1", author, "@recip paged"))
	reads.seedOwedMention(recipient, ch, textMessage("owed-1", author, "@recip paged"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	if !disp.waitForMessage(t, "owed-1") {
		t.Fatal("owed-1 never dispatched on first start")
	}

	// The ack's effect: the owed row is cleared (T1 AckDelivery clears owed rows).
	if err := reads.ClearOwedMention(nil, recipient, "owed-1"); err != nil { //nolint:staticcheck // nil ctx: the fake ignores it (test root exemption)
		t.Fatalf("clear owed mention: %v", err)
	}

	// A second start edge now sweeps nothing owed.
	c.OnSessionStarted("sess-recip", recipient)
	c.waitStartsDrained(t)

	if got := disp.snapshot(); len(got) != 1 {
		t.Fatalf("dispatches = %d, want 1 (the first sweep only; the re-sweep after ack owes nothing)", len(got))
	}
}

// Case 7: a permanently-unreadable owed message (its message row vanished) is
// CLEARED and logged on the start sweep, not dispatched — and NOT re-swept on the
// next start (the clear stops the every-start re-log loop).
func TestUnreadableOwedMentionClearedNotReswept(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const recipient store.AccountID = "agent-recip"

	reads.owed[recipient] = map[store.ChannelID][]store.Message{}
	// An owed row whose message is NOT seeded → storeMessageToWire fails (vanished).
	reads.seedOwedMention(recipient, ch, textMessage("gone-1", author, "poof"))
	res.bind(recipient, "sess-recip")
	startConsumer(t, c)

	c.OnSessionStarted("sess-recip", recipient)
	// The sweep's observable effect is the clear (owed -> 0); gate on it. A bare
	// waitStartsDrained only proves the start edge was DEQUEUED, not that the
	// sweep's ClearOwedMention ran (introspect_test.go), so asserting owedCount
	// right after it races the async sweep and flakes red under CI load.
	reads.waitForOwed(t, recipient, 0)
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (an unreadable owed message is cleared, not dispatched)", len(got))
	}

	// A second start edge finds nothing owed — the row was cleared, so no re-log loop.
	c.OnSessionStarted("sess-recip", recipient)
	c.waitStartsDrained(t)
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 on re-sweep (the cleared row is gone)", len(got))
	}
}

// Case 8: a NIL waker (the default, no SetAgentWaker) — routing an offline
// out-of-sweep-set mention records the owed row and returns without panicking.
// The nil-safe c.wake helper is the guard.
func TestNilWakerRoutesWithoutPanic(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t) // no SetAgentWaker: agentWaker stays nil
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	if c.agentWaker != nil {
		t.Fatal("precondition: a freshly-built consumer has no AgentWaker wired")
	}
	reads.members[ch] = []store.AccountID{agentA}
	reads.subscribers[ch] = []store.AccountID{agentA} // also a subscriber, so the deliver arm runs too
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// agentA offline, out of sweep set.
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa ping")))
	// The owed row is the observable effect (a nil-safe wake is a no-op): gate on it.
	reads.waitForOwed(t, agentA, 1)
	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline, nil waker)", len(got))
	}
}

// Case (race): a mention to a member that is offline at the first resolve but
// becomes live between the record and the post-record re-resolve is steered
// DIRECTLY — closing the record-vs-wake race. The afterRecord hook flips the
// resolver live right after RecordOwedMention returns.
func TestOfflineMentionNowLiveAfterRecordSteersDirectly(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	w := withWaker(c)
	const ch store.ChannelID = "chan-1"
	const author store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.members[ch] = []store.AccountID{agentA}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	// Out of the sweep set (unseeded) so the record path runs. Offline at first
	// resolve; the record hook binds it live for the post-record re-check.
	reads.afterRecord = func(agent store.AccountID, _ store.ChannelID, _ string) {
		res.bind(agent, "sess-a")
	}
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireText("m1", author, "@aa ping")))
	if !disp.waitForMessage(t, "m1") {
		t.Fatal("m1 never steered: a now-live-after-record member must be steered directly")
	}

	got := disp.snapshot()
	if len(got) != 1 || got[0].sessionID != "sess-a" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want a single steer to sess-a (the record-vs-wake race close)", got)
	}
	if n := reads.owedCount(agentA); n != 1 {
		t.Fatalf("owed rows for agentA = %d, want 1 (recorded durably before the now-live steer)", n)
	}
	// The wake still fires (durability-first, then wake-all).
	w.waitForWakes(t, 1)
}
