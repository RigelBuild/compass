//go:build pgtest

package comms

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func askWithRef(ref string) *compassv1.MessageBlock {
	return &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
		Ref: ref,
		Questions: []*compassv1.AskQuestion{{
			QuestionId: "q1",
			Question:   "Ship it?",
			Options:    []*compassv1.AskOption{{Id: "yes", Label: "yes"}},
		}},
	}}}
}

// TestAskRefRoundTrip posts an ask with a ref and follows it through the
// ListMessages read and RespondToAsk: the ask and the answer snapshot both
// carry the canonical ref.
func TestAskRefRoundTrip(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	author := mustUser(t, st, "author")
	ch, err := st.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	const want = "rigelbuild/compass#12"
	posted, err := svc.PostMessage(WithActor(ctx, author.ID), &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: testTopic},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{askWithRef("RigelBuild/Compass#12")},
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	ask := posted.GetMessage().GetBlocks()[0].GetAsk()
	if ask.GetRef() != want {
		t.Fatalf("posted ask ref = %q, want %q", ask.GetRef(), want)
	}

	// refsOnWire reads the ask's ref and, once answered, the answer snapshot's
	// ref off the ListMessages wire read.
	refsOnWire := func() (askRef, answerRef string) {
		t.Helper()
		list, err := svc.ListMessages(WithActor(ctx, author.ID), &compassv1.ListMessagesRequest{
			Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
		})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		for _, m := range list.GetMessages() {
			for _, b := range m.GetBlocks() {
				if a := b.GetAsk(); a.GetAskId() == ask.GetAskId() {
					askRef = a.GetRef()
				}
				if aa := b.GetAskAnswer(); aa.GetAsk().GetAskId() == ask.GetAskId() {
					answerRef = aa.GetAsk().GetRef()
				}
			}
		}
		return askRef, answerRef
	}
	if got, _ := refsOnWire(); got != want {
		t.Fatalf("ListMessages ask ref = %q, want %q", got, want)
	}

	if _, err := svc.RespondToAsk(WithActor(ctx, author.ID), &compassv1.RespondToAskRequest{
		AskId:   ask.GetAskId(),
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"yes"}}},
	}); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}
	askRef, answerRef := refsOnWire()
	if askRef != want {
		t.Fatalf("answered ask ref = %q, want %q", askRef, want)
	}
	if answerRef != want {
		t.Fatalf("answer snapshot ref = %q, want %q", answerRef, want)
	}
}

// TestPostMessageRejectsMalformedAskRef refuses an ask ref that is not exactly
// one ref, and stores nothing.
func TestPostMessageRejectsMalformedAskRef(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	author := mustUser(t, st, "author")
	ch, err := st.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	_, err = svc.PostMessage(WithActor(ctx, author.ID), &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: testTopic},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{askWithRef("see RIG-12")},
	})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "post an ask with a malformed ref")
	list, err := svc.ListMessages(WithActor(ctx, author.ID), &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(list.GetMessages()) != 0 {
		t.Fatalf("ListMessages = %d messages after a refused post, want 0", len(list.GetMessages()))
	}
}
