//go:build pgtest && unix

package delivery

// RIG-2257 T7 — the ask-answer recovery acceptance cycle, end-to-end over the
// FULL stack against a REAL Postgres (the store of record is only proven against
// the database it targets — no mock). The answer to an ask is a normal message
// authored by the answerer, carrying a server-owned ask_answer block, so it
// rides the existing message rail: fan-out, the ack-gated cursor sweep, the
// OnSessionStarted resweep, and — for an out-of-sweep asker — the owed-mention
// backstop re-derivable by the recovery scan. Each scenario drives the true
// consumer (real *store.Store, the shared fakes for the hub's dispatch +
// resolution roles) and gates on a durable observable effect — a cursor deliver,
// an owed row, a steer — never a sleep. context.Background() is the test root
// (rule://go-thread-context exemption for _test.go), threaded into Run and every
// store read.

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/store"
)

// postAsk commits a pending ask authored by agent into ch's "general" topic and
// returns its ask id. The ask is agent-authored, exactly as the real flow.
func postAsk(t *testing.T, ctx context.Context, s *store.Store, ch store.ChannelID, agent store.AccountID, askID string) {
	t.Helper()
	if _, _, err := s.AppendMessage(ctx, store.Message{
		AuthorAccountID: agent,
		Blocks: []store.MessageBlock{{Ask: &store.Ask{
			AskID: askID,
			Questions: []store.AskQuestion{{
				QuestionID: "q1", Question: "Which environment?",
				Options: []store.AskOption{{ID: "opt-a", Label: "staging"}, {ID: "opt-b", Label: "prod"}},
			}},
		}}},
	}, string(ch), store.TopicRef{Name: "general"}, ""); err != nil {
		t.Fatalf("AppendMessage(ask %q): %v", askID, err)
	}
}

// answerAndPublish answers askID as answerer and publishes MessagePosted for the
// resulting answer message onto the consumer's bus — the comms RespondToAsk
// effect (the store insert + the delivery trigger) without the RPC edge.
func answerAndPublish(t *testing.T, ctx context.Context, s *store.Store, c *Consumer, answerer store.AccountID, askID string) store.Message {
	t.Helper()
	_, answer, err := s.AnswerAsk(ctx, answerer, askID, []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	if err != nil {
		t.Fatalf("AnswerAsk(%q): %v", askID, err)
	}
	c.bus.Publish(postedResponse(comms.MessageToWire(answer)))
	return answer
}

// answerOnly answers askID without publishing — the severed path for the
// recovery-scan scenario (the answer is committed, mentions_routed_at NULL, but
// no MessagePosted reaches the fresh bus).
func answerOnly(t *testing.T, ctx context.Context, s *store.Store, answerer store.AccountID, askID string) store.Message {
	t.Helper()
	_, answer, err := s.AnswerAsk(ctx, answerer, askID, []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	if err != nil {
		t.Fatalf("AnswerAsk(%q): %v", askID, err)
	}
	return answer
}

// Scenario 1: answer submitted with NO live asker session → the OnSessionStarted
// cursor sweep delivers the answer message → an ack advances the cursor → a
// second start delivers nothing. The asker is a SUBSCRIBED agent (in the sweep
// set), so the cursor sweep is its rail.
func TestT7OfflineAnswerSweptAtSessionStart(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID)
	subscribeMember(t, ctx, s, owner.ID, ch, asker.ID)

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	c, disp, res := newPgConsumer(t, s)
	startConsumer(t, c)

	// The owner answers while the asker has no live session.
	answer := answerAndPublish(t, ctx, s, c, owner.ID, "ask-1")

	// No live session: the fan-out cannot deliver, so nothing is dispatched yet.
	// The asker's session start sweeps the owed answer via the cursor.
	res.bind(asker.ID, "sess-asker")
	c.OnSessionStarted("sess-asker", asker.ID)
	if !disp.waitForMessage(t, string(answer.ID)) {
		t.Fatalf("answer %s never delivered on the asker's session start", answer.ID)
	}

	// Ack advances the cursor: the answer leaves the durable owed set, so a
	// reconnect re-promotion re-sweeps nothing (the load-bearing durable read;
	// a dispatch-count negative is not a reliable barrier — see the crash leg).
	if err := s.AckDelivery(ctx, asker.ID, ch, string(answer.ID)); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}
	owed, err := s.UndeliveredMessages(ctx, asker.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	for _, msgs := range owed {
		for _, m := range msgs {
			if m.ID == answer.ID {
				t.Fatalf("answer %s still owed after ack: the cursor did not advance", answer.ID)
			}
		}
	}
}

// Scenario 2: the answer is delivered but the ack is lost → a reconnect
// re-promotion redelivers it (at-least-once). Modeled by a second
// OnSessionStarted with no intervening ack: the cursor still owes the answer.
func TestT7AckLostReconnectRedelivers(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID)
	subscribeMember(t, ctx, s, owner.ID, ch, asker.ID)

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	c, disp, res := newPgConsumer(t, s)
	startConsumer(t, c)
	answer := answerAndPublish(t, ctx, s, c, owner.ID, "ask-1")

	res.bind(asker.ID, "sess-asker")
	c.OnSessionStarted("sess-asker", asker.ID)
	if !disp.waitForMessage(t, string(answer.ID)) {
		t.Fatalf("answer %s not delivered on first start", answer.ID)
	}
	first := len(disp.snapshot())

	// Ack LOST: no AckDelivery. A reconnect re-promotion redelivers (the cursor
	// still owes it) — at-least-once.
	c.OnSessionStarted("sess-asker", asker.ID)
	disp.waitForDispatches(t, first+1) // redelivered: the cursor still owed it after the lost ack
}

// Scenario 3: a duplicate RespondToAsk is ErrConflict and exactly one answer
// message ever exists — the answer-once guard caps it independent of delivery.
func TestT7DuplicateRespondExactlyOneAnswer(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID)

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	if _, _, err := s.AnswerAsk(ctx, owner.ID, "ask-1", []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}); err != nil {
		t.Fatalf("first AnswerAsk: %v", err)
	}
	if _, _, err := s.AnswerAsk(ctx, owner.ID, "ask-1", []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-b"}}}); err == nil {
		t.Fatal("second AnswerAsk = nil, want ErrConflict")
	}

	// Exactly one ask_answer message exists in the channel.
	msgs, err := s.ListMessages(ctx, store.ListMessagesQuery{Actor: owner.ID, ChannelID: ch, Page: store.Page{}})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	answers := 0
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.AskAnswer != nil {
				answers++
			}
		}
	}
	if answers != 1 {
		t.Fatalf("answer messages = %d, want exactly 1", answers)
	}
}

// Scenario 4: a member-not-subscribed OFFLINE asker gets the answer via the owed
// row → delivered as a STEER at the next session start.
func TestT7OutOfSweepOfflineAskerOwedThenSteered(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	// asker is a member but NOT subscribed (out of the sweep set).
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID)
	if in, err := s.InSweepSet(ctx, asker.ID, ch); err != nil || in {
		t.Fatalf("InSweepSet(asker,ch) = (%v,%v), want (false,nil)", in, err)
	}

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	c, disp, res := newPgConsumer(t, s)
	startConsumer(t, c)
	answer := answerAndPublish(t, ctx, s, c, owner.ID, "ask-1")

	// Out-of-sweep + offline: the fan-out records a durable owed row.
	waitOwed(t, ctx, s, asker.ID, 1)

	// The asker's session start sweeps the owed answer as a STEER.
	res.bind(asker.ID, "sess-asker")
	c.OnSessionStarted("sess-asker", asker.ID)
	if !disp.waitForMessage(t, string(answer.ID)) {
		t.Fatalf("owed answer %s never steered on session start", answer.ID)
	}
	for _, d := range disp.snapshot() {
		if d.messageID == string(answer.ID) && d.kind != opSteer {
			t.Fatalf("answer dispatch kind = %v, want steer (owed rows sweep as STEER)", d.kind)
		}
	}
}

// Scenario 5: a member-not-subscribed LIVE asker gets the answer dispatched
// DIRECTLY, without a session restart (the F2 regression).
func TestT7OutOfSweepLiveAskerDirectDispatch(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID)
	if in, err := s.InSweepSet(ctx, asker.ID, ch); err != nil || in {
		t.Fatalf("InSweepSet(asker,ch) = (%v,%v), want (false,nil)", in, err)
	}

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	c, disp, res := newPgConsumer(t, s)
	// The asker is LIVE at answer time — no OnSessionStarted after.
	res.bind(asker.ID, "sess-asker")
	startConsumer(t, c)
	answer := answerAndPublish(t, ctx, s, c, owner.ID, "ask-1")

	if !disp.waitForMessage(t, string(answer.ID)) {
		t.Fatalf("answer %s never directly dispatched to a live out-of-sweep asker (F2 regression)", answer.ID)
	}
	// The durable owed row is recorded alongside the live dispatch (complements).
	waitOwed(t, ctx, s, asker.ID, 1)
}

// Scenario 6: the owed answer swept at session start arrives as a STEER and
// carries the ask_answer block the agent's steer arm renders through — the same
// ask_answer render path the deliver lane uses. Asserts the steered wire message
// carries the ask_answer block (T6's render input).
func TestT7OwedAnswerSweptAsSteerCarriesAskAnswerBlock(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID) // unsubscribed → out of sweep

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	c, disp, res := newPgConsumer(t, s)
	startConsumer(t, c)
	answer := answerAndPublish(t, ctx, s, c, owner.ID, "ask-1")
	waitOwed(t, ctx, s, asker.ID, 1)

	res.bind(asker.ID, "sess-asker")
	c.OnSessionStarted("sess-asker", asker.ID)
	if !disp.waitForMessage(t, string(answer.ID)) {
		t.Fatalf("owed answer %s never steered", answer.ID)
	}
	// Re-read the answer and confirm its wire form carries an ask_answer block —
	// the render input both the steer lane and the deliver lane feed to T6.
	stored, err := s.MessageByID(ctx, string(answer.ID))
	if err != nil {
		t.Fatalf("MessageByID: %v", err)
	}
	wire := comms.MessageToWire(stored)
	if _, ok := askAnswerTarget(wire); !ok {
		t.Fatalf("steered answer message carries no ask_answer block; T6 render path would have nothing to render")
	}
}

// Scenario 7: the consumer restarts BEFORE fanOut runs; the out-of-sweep asker
// still recovers. The answer is committed (mentions_routed_at NULL) but never
// published to this fresh consumer's bus — only the recovery scan
// (scanMissedMentions) can re-derive the owed row through the shared targeting
// body, and the next OnSessionStarted delivers it.
func TestT7RestartBeforeFanoutRecoveryScanReDerives(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	asker := mustAgentAcct(t, ctx, s, owner.ID, "asker")
	ch := mustRoomWithMembers(t, ctx, s, owner.ID, asker.ID) // unsubscribed → out of sweep

	postAsk(t, ctx, s, ch, asker.ID, "ask-1")

	// The answer is committed but its MessagePosted is severed (never published).
	answer := answerOnly(t, ctx, s, owner.ID, "ask-1")

	// A fresh bus + consumer: Run's start scan is the only thing that can surface
	// the committed-NULL answer.
	c, disp, res := newPgConsumer(t, s)
	startConsumer(t, c)

	// The recovery scan re-derives the durable owed row purely from durable state.
	waitOwed(t, ctx, s, asker.ID, 1)
	waitMarked(t, ctx, s, string(answer.ID))

	// The next session start then delivers it as a steer.
	res.bind(asker.ID, "sess-asker")
	c.OnSessionStarted("sess-asker", asker.ID)
	if !disp.waitForMessage(t, string(answer.ID)) {
		t.Fatalf("recovered answer %s never steered on session start", answer.ID)
	}
}
