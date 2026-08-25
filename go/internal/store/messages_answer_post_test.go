//go:build pgtest

package store

// T3 (RIG-2257): AnswerAsk posts the answer as a NEW message in the ask's
// channel+topic, authored by the answerer, in the SAME transaction as the
// Answered flip. These pin the answer-as-message contract on the real store: a
// single answer message carrying the answered snapshot; the answer-once guard
// still caps answers at one (no second message on a re-answer); and the
// atomicity — a failed answer leaves neither the flip nor a message. The
// answer-once corpus itself lives in messages_test.go and stays green.

import (
	"context"
	"errors"
	"testing"
)

// answerBlocks reads the ask_answer blocks a channel holds, resolving through
// the topic join, so a test asserts what was durably persisted.
func answerMessages(t *testing.T, ctx context.Context, s *Store, actor AccountID, ch ChannelID) []Message {
	t.Helper()
	msgs, err := s.ListMessages(ctx, ListMessagesQuery{Actor: actor, ChannelID: ch, Page: Page{}})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var out []Message
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.AskAnswer != nil {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// TestAnswerAskPostsAnswerMessage pins the headline T3 contract: answering an
// ask posts exactly one NEW message in the ask's channel+topic, authored by the
// answerer, carrying a single ask_answer block whose snapshot holds the recorded
// answer and whose asker_account_id is the ask message's author.
func TestAnswerAskPostsAnswerMessage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent := mustUser(t, s, "agent")
	answerer := mustUser(t, s, "answerer")
	ch := mustNamedChannel(t, s, agent.ID, "room")
	// The answerer must be a member of the ask's channel to answer it.
	if _, _, err := s.UpdateChannelMembers(ctx, agent.ID, ch.ID, []MemberUpdate{{AccountID: answerer.ID}}); err != nil {
		t.Fatalf("add answerer as member: %v", err)
	}

	// The agent posts the ask.
	askMsgIn, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{pendingAsk("ask-1", false)}}, string(ch.ID), TopicRef{Name: "general"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}

	askMsg, answerMsg, err := s.AnswerAsk(ctx, answerer.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	if err != nil {
		t.Fatalf("AnswerAsk: %v", err)
	}

	// The returned ask message is the flipped original.
	if askMsg.ID != askMsgIn.ID {
		t.Fatalf("returned askMsg id = %q, want the original ask %q", askMsg.ID, askMsgIn.ID)
	}

	// The answer message is a distinct row, authored by the answerer, in the
	// ask's own topic.
	if answerMsg.ID == askMsg.ID {
		t.Fatalf("answer message id == ask message id (%q); want a distinct new row", answerMsg.ID)
	}
	if answerMsg.AuthorAccountID != answerer.ID {
		t.Fatalf("answer author = %q, want the answerer %q", answerMsg.AuthorAccountID, answerer.ID)
	}
	if answerMsg.TopicID != askMsg.TopicID {
		t.Fatalf("answer topic = %q, want the ask topic %q", answerMsg.TopicID, askMsg.TopicID)
	}
	if got := messageChannel(t, ctx, s, answerMsg.ID); got != ch.ID {
		t.Fatalf("answer channel = %q, want the ask channel %q", got, ch.ID)
	}

	// The answer block snapshots the answered ask with the recorded answer and
	// the asking agent denormalized.
	if len(answerMsg.Blocks) != 1 || answerMsg.Blocks[0].AskAnswer == nil {
		t.Fatalf("answer blocks = %+v, want one ask_answer block", answerMsg.Blocks)
	}
	blk := answerMsg.Blocks[0].AskAnswer
	if blk.AskerAccountID != agent.ID {
		t.Fatalf("asker_account_id = %q, want the ask author %q", blk.AskerAccountID, agent.ID)
	}
	if blk.Ask.AskID != "ask-1" || !blk.Ask.Answered {
		t.Fatalf("snapshot ask = %+v, want ask-1 answered", blk.Ask)
	}
	if len(blk.Ask.Questions) != 1 || len(blk.Ask.Questions[0].ChosenOptionIDs) != 1 || blk.Ask.Questions[0].ChosenOptionIDs[0] != "opt-a" {
		t.Fatalf("snapshot answer = %+v, want q1 chosen opt-a", blk.Ask.Questions)
	}

	// Exactly one answer message exists in the channel.
	if got := answerMessages(t, ctx, s, agent.ID, ch.ID); len(got) != 1 {
		t.Fatalf("channel holds %d answer messages, want exactly 1", len(got))
	}
}

// TestAnswerAskSecondAnswerNoSecondMessage pins that the answer-once guard caps
// the answer messages at one: a second AnswerAsk is ErrConflict and posts NO
// second message.
func TestAnswerAskSecondAnswerNoSecondMessage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent := mustUser(t, s, "agent")
	ch := mustNamedChannel(t, s, agent.ID, "room")

	if _, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{pendingAsk("ask-1", false)}}, string(ch.ID), TopicRef{Name: "general"}, ""); err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}

	if _, _, err := s.AnswerAsk(ctx, agent.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}); err != nil {
		t.Fatalf("first AnswerAsk: %v", err)
	}
	_, _, err := s.AnswerAsk(ctx, agent.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-b"}}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second AnswerAsk err = %v, want ErrConflict", err)
	}

	if got := answerMessages(t, ctx, s, agent.ID, ch.ID); len(got) != 1 {
		t.Fatalf("channel holds %d answer messages after a rejected re-answer, want exactly 1", len(got))
	}
}

// TestAnswerAskInvalidAnswerNoMessage pins atomicity on the reject path: an
// answer that fails validation (an unknown question_id) rolls the whole tx back
// — neither the Answered flip nor an answer message survives.
func TestAnswerAskInvalidAnswerNoMessage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent := mustUser(t, s, "agent")
	ch := mustNamedChannel(t, s, agent.ID, "room")

	if _, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{pendingAsk("ask-1", false)}}, string(ch.ID), TopicRef{Name: "general"}, ""); err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}

	// An answer naming an unknown question fails coverage → ErrInvalidArgument.
	_, _, err := s.AnswerAsk(ctx, agent.ID, "ask-1", []AskAnswer{{QuestionID: "nonexistent", ChosenOptionIDs: []string{"opt-a"}}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid AnswerAsk err = %v, want ErrInvalidArgument", err)
	}

	// The tx rolled back: no answer message, and the ask is still pending.
	if got := answerMessages(t, ctx, s, agent.ID, ch.ID); len(got) != 0 {
		t.Fatalf("channel holds %d answer messages after a rejected answer, want 0", len(got))
	}
	if ask := askByID(t, ctx, s, agent.ID, ch.ID, "ask-1"); ask.Answered {
		t.Fatalf("ask reads Answered after a rejected answer, want still pending")
	}
}
