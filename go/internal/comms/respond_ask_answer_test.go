//go:build pgtest

package comms

// T4 (RIG-2257): RespondToAsk publishes MessagePosted for the NEW answer
// message (the delivery trigger) alongside MessageUpdated for the ask, and the
// wire edge rejects a client-supplied ask_answer block on the POST path as the
// server-owned variant. The waker rail is gone — the answer rides the normal
// message rail.

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// twoEventsAfterBoundary opens a since_seq=0 stream, drops the snapshot-boundary
// frame, and returns the next TWO event frames in order — RespondToAsk publishes
// MessageUpdated then MessagePosted, so a test can assert both fan out.
func twoEventsAfterBoundary(t *testing.T, h streamHarness, actor store.AccountID) <-chan []*compassv1.SubscribeCommsResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan []*compassv1.SubscribeCommsResponse, 1)
	connectReq := connect.NewRequest(&compassv1.SubscribeCommsRequest{SinceSeq: 0})
	if actor != "" {
		connectReq.Header().Set(commsTestActorHeader, string(actor))
	}
	go func() {
		stream, err := h.client.SubscribeComms(ctx, connectReq)
		if err != nil {
			t.Errorf("SubscribeComms: %v", err)
			out <- nil
			return
		}
		if !stream.Receive() {
			t.Errorf("stream closed before boundary: %v", stream.Err())
			out <- nil
			return
		}
		if b := stream.Msg(); b.GetSeq() != 0 || b.GetPayload() != nil {
			t.Errorf("first frame = seq %d payload %T, want boundary", b.GetSeq(), b.GetPayload())
			out <- nil
			return
		}
		var got []*compassv1.SubscribeCommsResponse
		for range 2 {
			if !stream.Receive() {
				t.Errorf("stream closed before two events: %v", stream.Err())
				out <- got
				return
			}
			got = append(got, stream.Msg())
		}
		out <- got
	}()
	return out
}

// TestRespondToAskFansOutMessagePosted pins the T4 delivery trigger: answering
// an ask fans out MessageUpdated for the ask AND MessagePosted for the new
// answer message, whose ask_answer block snapshots the answered ask with the
// asking agent denormalized.
func TestRespondToAskFansOutMessagePosted(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	agent := mustUser(t, h.store, "agent")
	ch, err := h.store.CreateChannel(ctx, agent.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := h.store.AppendMessage(ctx, store.Message{AuthorAccountID: agent.ID, Blocks: []store.MessageBlock{pendingAskStore("ask-1")}}, string(ch.ID), store.TopicRef{Name: "general"}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	events := twoEventsAfterBoundary(t, h, agent.ID)

	if _, err := h.svc.RespondToAsk(WithActor(ctx, agent.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-1",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}

	var got []*compassv1.SubscribeCommsResponse
	select {
	case got = <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the two fan-out events")
	}
	if len(got) != 2 {
		t.Fatalf("received %d events, want 2 (MessageUpdated + MessagePosted)", len(got))
	}

	// First: MessageUpdated for the ask.
	if got[0].GetMessageUpdated() == nil {
		t.Fatalf("event[0] = %T, want MessageUpdated", got[0].GetPayload())
	}
	// Second: MessagePosted for the answer message.
	mp := got[1].GetMessagePosted()
	if mp == nil {
		t.Fatalf("event[1] = %T, want MessagePosted", got[1].GetPayload())
	}
	answer := mp.GetMessage()
	if answer.GetAuthorAccountId() != string(agent.ID) {
		t.Fatalf("answer author = %q, want the answerer %q", answer.GetAuthorAccountId(), agent.ID)
	}
	blocks := answer.GetBlocks()
	if len(blocks) != 1 {
		t.Fatalf("answer blocks = %d, want 1", len(blocks))
	}
	aa := blocks[0].GetAskAnswer()
	if aa == nil {
		t.Fatalf("answer block = %T, want an ask_answer block", blocks[0].GetBlock())
	}
	if aa.GetAskerAccountId() != string(agent.ID) {
		t.Fatalf("ask_answer asker = %q, want the ask author %q", aa.GetAskerAccountId(), agent.ID)
	}
	if aa.GetAsk().GetAskId() != "ask-1" || !aa.GetAsk().GetAnswered() {
		t.Fatalf("ask_answer snapshot = %+v, want ask-1 answered", aa.GetAsk())
	}
}

// TestPostMessageRejectsAskAnswerBlock pins the wire-edge server-ownership
// enforcement: a client PostMessage carrying an ask_answer block is refused with
// CodeInvalidArgument — the ask_answer variant enters only through RespondToAsk.
func TestPostMessageRejectsAskAnswerBlock(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	author := mustUser(t, h.store, "author")
	ch, err := h.store.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err = h.svc.PostMessage(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_AskAnswer{AskAnswer: &compassv1.AskAnswerBlock{
			Ask: &compassv1.Ask{
				AskId:     "ask-1",
				Answered:  true,
				Questions: []*compassv1.AskQuestion{{QuestionId: "q1", Question: "?"}},
			},
			AskerAccountId: "agent",
		}}}},
	}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "PostMessage with an ask_answer block")
}
