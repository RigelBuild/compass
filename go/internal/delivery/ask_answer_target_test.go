//go:build unix

package delivery

// RIG-2257 T5 — the ask_answer targeted-asker arm, RED-first. An answer message
// (authored by the answerer, carrying a server-owned ask_answer block naming the
// asking agent) must reach its asker even when the asker is a channel member
// OUTSIDE the sweep set: the durable owed row is the offline backstop, the
// live-session re-check is the latency path, and the arm is re-derived by the
// recovery scan so a restart in the commit→fanOut window still backstops the
// asker. Each case drives the consumer through the real bus + fakes and gates on
// the recorder — never a sleep. context.Background() is the test root
// (rule://go-thread-context exemption for _test.go), threaded into Run/scan.

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// wireAskAnswer builds a wire answer Message authored by answerer, carrying a
// single ask_answer block whose snapshot is an answered ask and whose
// asker_account_id targets asker — the shape RespondToAsk publishes.
func wireAskAnswer(id string, answerer, asker store.AccountID) *compassv1.Message {
	return &compassv1.Message{
		Id:              id,
		TopicId:         "topic-1",
		AuthorAccountId: string(answerer),
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_AskAnswer{AskAnswer: &compassv1.AskAnswerBlock{
			Ask: &compassv1.Ask{
				AskId:     "ask-1",
				Answered:  true,
				Questions: []*compassv1.AskQuestion{{QuestionId: "q1", Question: "?", ChosenOptionIds: []string{"opt-a"}}},
			},
			AskerAccountId: string(asker),
		}}}},
	}
}

// storeAskAnswer builds the store-side answer message for the recovery-scan
// path (seedUnrouted re-reads a store.Message).
func storeAskAnswer(id string, answerer, asker store.AccountID) store.Message {
	return store.Message{
		ID:              store.MessageID(id),
		TopicID:         "topic-1",
		AuthorAccountID: answerer,
		Blocks: []store.MessageBlock{{AskAnswer: &store.AskAnswerBlock{
			Ask: store.Ask{
				AskID:     "ask-1",
				Answered:  true,
				Questions: []store.AskQuestion{{QuestionID: "q1", Question: "?", ChosenOptionIDs: []string{"opt-a"}}},
			},
			AskerAccountID: asker,
		}}},
	}
}

// Case 1 (T5): an answer whose asker is a member-not-subscribed + OFFLINE agent
// records a durable owed row (the offline backstop) and dispatches nothing — the
// next OnSessionStarted sweep delivers it as a STEER.
func TestAskAnswerOfflineOutOfSweepAskerRecordsOwed(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const answerer store.AccountID = "human-1"
	const asker store.AccountID = "agent-asker"

	// asker is a channel member but NOT subscribed (out of the sweep set) and
	// offline (never bound). answerer authors the answer.
	reads.members[ch] = []store.AccountID{asker}
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireAskAnswer("ans-1", answerer, asker)))
	reads.waitForOwed(t, asker, 1)

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline out-of-sweep asker: owed row only)", len(got))
	}
	if n := reads.owedCount(asker); n != 1 {
		t.Fatalf("owed rows for asker = %d, want 1", n)
	}
}

// Case 2 (T5): an answer whose asker is a member-not-subscribed + LIVE agent is
// dispatched DIRECTLY as a steer (the latency path) AND records the durable owed
// row (the backstop) — complements, not either/or. No session restart needed
// (the F2 regression the design pins).
func TestAskAnswerLiveOutOfSweepAskerDispatchesDirectly(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const answerer store.AccountID = "human-1"
	const asker store.AccountID = "agent-asker"

	reads.members[ch] = []store.AccountID{asker}
	// asker out of the sweep set but LIVE.
	res.bind(asker, "sess-asker")
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireAskAnswer("ans-1", answerer, asker)))
	if !disp.waitForMessage(t, "ans-1") {
		t.Fatal("ans-1 never dispatched: a live out-of-sweep asker must be steered directly")
	}

	got := disp.snapshot()
	if len(got) != 1 || got[0].sessionID != "sess-asker" || got[0].kind != opSteer {
		t.Fatalf("dispatch = %+v, want a single steer to sess-asker", got)
	}
	if n := reads.owedCount(asker); n != 1 {
		t.Fatalf("owed rows for asker = %d, want 1 (durable backstop recorded alongside the live steer)", n)
	}
}

// Case 3 (T5): an answer whose asker is SUBSCRIBED (in the sweep set) needs
// NEITHER arm — no owed row, no direct steer from the targeting arm. The normal
// deliver loop + cursor sweep already reach a swept asker.
func TestAskAnswerSubscribedAskerNoOwedNoDirect(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const answerer store.AccountID = "human-1"
	const asker store.AccountID = "agent-asker"

	// asker subscribed AND in the sweep set, but OFFLINE — so the normal deliver
	// loop wakes it (no direct steer) and records no owed row.
	reads.members[ch] = []store.AccountID{asker}
	reads.subscribers[ch] = []store.AccountID{asker}
	reads.sweepSet[asker] = map[store.ChannelID]bool{ch: true}
	w := withWaker(c)
	startConsumer(t, c)

	c.bus.Publish(postedResponse(wireAskAnswer("ans-1", answerer, asker)))
	w.waitForWakes(t, 1) // the normal deliver arm wakes the offline subscriber

	if n := reads.owedCount(asker); n != 0 {
		t.Fatalf("owed rows for asker = %d, want 0 (in-sweep-set: the cursor sweep is the backstop)", n)
	}
	// No STEER from the targeting arm: the only observable dispatch would be a
	// deliver via the normal loop, but the asker is offline so nothing dispatched.
	for _, d := range disp.snapshot() {
		if d.kind == opSteer {
			t.Fatalf("unexpected steer %+v: a subscribed asker must not be directly steered by the targeting arm", d)
		}
	}
}

// Case 7 (T5/T7): a consumer restart between AnswerAsk's commit and fanOut leaves
// the answer message committed with mentions_routed_at IS NULL. The recovery scan
// re-derives the owed row through the SHARED targeting body — so the out-of-sweep
// asker still recovers rather than stranding permanently.
func TestAskAnswerRecoveryScanReDerivesOwed(t *testing.T) {
	c, disp, _, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const answerer store.AccountID = "human-1"
	const asker store.AccountID = "agent-asker"

	reads.members[ch] = []store.AccountID{asker}
	// asker offline + out of the sweep set. The answer message is committed but
	// unmarked (the fanOut never ran — restart in the commit→fanOut window).
	reads.seedUnrouted(storeAskAnswer("ans-1", answerer, asker), ch, 1)

	c.scanMissedMentions(context.Background())

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches = %d, want 0 (offline asker: owed row only)", len(got))
	}
	if n := reads.owedCount(asker); n != 1 {
		t.Fatalf("owed rows for asker = %d, want 1 (recovery scan re-derives the owed row)", n)
	}
	if got := reads.markCount("ans-1"); got != 1 {
		t.Fatalf("marks for ans-1 = %d, want 1 (a processed message is marked complete)", got)
	}
}
