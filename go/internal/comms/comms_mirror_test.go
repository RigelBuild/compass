//go:build pgtest

package comms

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type mirrorRecorder struct {
	user store.AccountID
	req  *compassv1.MirrorToForgeRequest
}

func (m *mirrorRecorder) MirrorAsUser(_ context.Context, user store.Account, req *compassv1.MirrorToForgeRequest) (string, error) {
	m.user = user.ID
	m.req = req
	return "https://forge.example/comment", nil
}

type mirrorDriverRecorder struct {
	calls chan []string
}

func (m mirrorDriverRecorder) DriveAsks(_ context.Context, askIDs []string) {
	m.calls <- append([]string(nil), askIDs...)
}

func (m mirrorDriverRecorder) DriveAgent(context.Context, store.AccountID) {}

type agentMirrorDriverRecorder struct {
	calls chan store.AccountID
}

func (m agentMirrorDriverRecorder) DriveAsks(context.Context, []string) {}

func (m agentMirrorDriverRecorder) DriveAgent(_ context.Context, agent store.AccountID) {
	m.calls <- agent
}

func TestBoardWriteDrivesAgentMirrors(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-mirror-driver-owner")
	agent := mustAgent(t, st, owner.ID, "board-mirror-driver-agent")
	driver := agentMirrorDriverRecorder{calls: make(chan store.AccountID, 1)}
	svc.SetMirrorer(nil, driver)
	if _, err := svc.UpdateBoardAsAccount(t.Context(), agent.ID, &compassv1.UpdateAgentBoardRequest{}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	select {
	case got := <-driver.calls:
		if got != agent.ID {
			t.Fatalf("DriveAgent account = %q, want %q", got, agent.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DriveAgent did not receive board-writing agent")
	}
}

func TestBoardReadDrivesNoMirrors(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "board-read-driver-owner")
	agent := mustAgent(t, st, owner.ID, "board-read-driver-agent")
	driver := agentMirrorDriverRecorder{calls: make(chan store.AccountID, 2)}
	svc.SetMirrorer(nil, driver)
	if _, err := svc.UpdateBoardAsAccount(t.Context(), agent.ID, &compassv1.UpdateAgentBoardRequest{Automatic: true}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	// The write is driven; a driven read would add a second call.
	if _, err := svc.UpdateBoardAsAccount(t.Context(), agent.ID, &compassv1.UpdateAgentBoardRequest{}); err != nil {
		t.Fatalf("UpdateBoardAsAccount write: %v", err)
	}
	select {
	case <-driver.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("DriveAgent did not receive the board write")
	}
	select {
	case <-driver.calls:
		t.Fatal("the empty automatic read drove the agent's mirrors")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCommittedAskMessagesDriveMirrors(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "mirror-driver-owner")
	agent := mustAgent(t, st, owner.ID, "mirror-driver-agent")
	channel, err := st.CreateChannel(context.Background(), owner.ID, store.NewChannel{
		Name: "mirror-driver-room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	driver := mirrorDriverRecorder{calls: make(chan []string, 2)}
	svc.SetMirrorer(nil, driver)
	posted, err := svc.PostMessage(WithActor(context.Background(), agent.ID), &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(channel.ID)},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: "mirror-asks"}, CreateTopic: true,
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
			Ref: "RIG-12", Questions: []*compassv1.AskQuestion{{QuestionId: "q1", Question: "Ship?", Options: []*compassv1.AskOption{{Id: "yes", Label: "Yes"}}}},
		}}}},
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	askID := posted.GetMessage().GetBlocks()[0].GetAsk().GetAskId()
	assertDrivenAskID(t, driver.calls, askID)
	if _, err := svc.RespondToAsk(WithActor(context.Background(), owner.ID), &compassv1.RespondToAskRequest{
		AskId: askID, Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"yes"}}},
	}); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}
	assertDrivenAskID(t, driver.calls, askID)
}

func assertDrivenAskID(t *testing.T, calls <-chan []string, want string) {
	t.Helper()
	select {
	case got := <-calls:
		if len(got) != 1 || got[0] != want {
			t.Fatalf("DriveAsks ids = %v, want [%s]", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("DriveAsks did not receive ask %q", want)
	}
}

func TestMirrorToForgeIsUserOnlyAndDelegates(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "mirror-edge-owner")
	agent := mustAgent(t, st, owner.ID, "mirror-edge-agent")
	mirrorer := &mirrorRecorder{}
	svc.SetMirrorer(mirrorer, nil)
	request := &compassv1.MirrorToForgeRequest{Ref: "RIG-12", TopicId: "topic", ChannelId: "channel"}

	_, err := svc.MirrorToForge(WithActor(context.Background(), agent.ID), request)
	connectCodeIs(t, err, connect.CodePermissionDenied, "agent MirrorToForge")
	if mirrorer.req != nil {
		t.Fatal("MirrorAsUser was called for an agent")
	}

	response, err := svc.MirrorToForge(WithActor(context.Background(), owner.ID), request)
	if err != nil {
		t.Fatalf("user MirrorToForge: %v", err)
	}
	if mirrorer.user != owner.ID || mirrorer.req != request || response.GetCommentUrl() != "https://forge.example/comment" {
		t.Fatalf("delegated mirror user=%q request=%p response=%+v", mirrorer.user, mirrorer.req, response)
	}
}

func TestMirrorToForgeWithoutMirrorerIsUnimplemented(t *testing.T) {
	svc, st := newHandler(t)
	owner := mustUser(t, st, "mirror-edge-unwired")
	_, err := svc.MirrorToForge(WithActor(context.Background(), owner.ID), &compassv1.MirrorToForgeRequest{})
	connectCodeIs(t, err, connect.CodeUnimplemented, "MirrorToForge without a mirrorer")
}
