//go:build pgtest

package comms

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestListAgentBoardsIncludesVisibleAsks(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-asks-owner")
	viewer := mustUser(t, st, "board-asks-viewer")
	agent := mustAgent(t, st, owner.ID, "board-asks-agent")
	private, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "private-asks", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(private): %v", err)
	}
	shared, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "roster-room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{owner.ID, viewer.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(shared): %v", err)
	}
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	postBoardAsk(t, svc, ctx, agent.ID, private.ID, "owner/repo#13")
	postBoardAsk(t, svc, ctx, agent.ID, shared.ID, "owner/repo#12")

	viewerBoards, err := svc.ListBoardsAsAccount(ctx, viewer.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount(viewer): %v", err)
	}
	if len(viewerBoards.GetBoards()) != 1 || len(viewerBoards.GetBoards()[0].GetAsks()) != 1 {
		t.Fatalf("viewer boards = %v, want visible agent with only its shared-channel ask", viewerBoards.GetBoards())
	}
	if got := viewerBoards.GetBoards()[0].GetAsks()[0]; got.GetRef() != "owner/repo#12" || got.GetChannelId() != string(shared.ID) {
		t.Fatalf("viewer ask = %v, want only shared-channel ask", got)
	}

	ownerBoards, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount(owner): %v", err)
	}
	if len(ownerBoards.GetBoards()) != 1 {
		t.Fatalf("owner boards = %v, want one agent", ownerBoards.GetBoards())
	}
	asks := ownerBoards.GetBoards()[0].GetAsks()
	if len(asks) != 2 || asks[0].GetRef() != "owner/repo#12" || asks[1].GetRef() != "owner/repo#13" {
		t.Fatalf("owner asks = %v, want both asks newest first", asks)
	}
}

func TestListAgentBoardsIncludesForgeMirrorStates(t *testing.T) {
	svc, st := newHandler(t)
	ctx := t.Context()
	owner := mustUser(t, st, "board-ask-mirror-owner")
	agent := mustAgent(t, st, owner.ID, "board-ask-mirror-agent")
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "ask-mirror-states", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/mirrors", Number: 12}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{
		Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: store.ForgeMirrorAuto,
	}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	firstID := postBoardAsk(t, svc, ctx, agent.ID, channel.ID, "owner/mirrors#12")
	secondID := postBoardAsk(t, svc, ctx, agent.ID, channel.ID, "owner/mirrors#12")
	rows, err := st.AskForgeMirrors(ctx, []string{firstID, secondID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("AskForgeMirrors before update = (%+v, %v), want two auto rows", rows, err)
	}
	for i, row := range rows {
		state, commentKey, commentURL := int16(3), "", ""
		if i == 0 {
			state, commentKey, commentURL = 2, "done-comment", "https://github.com/owner/mirrors/issues/12#issuecomment-1"
		}
		if _, ok, err := st.LeaseForgeMirror(ctx, row.ID, "board-ask-mirror-"+row.AskID, time.Minute); err != nil || !ok {
			t.Fatalf("LeaseForgeMirror(%q): ok=%t err=%v", row.ID, ok, err)
		}
		if err := st.MarkForgeMirror(ctx, row.ID, "board-ask-mirror-"+row.AskID, store.ForgeMirrorMark{
			PostedRev: 1, State: state, CommentKey: commentKey, URL: commentURL, Err: "delivery failed",
		}); err != nil {
			t.Fatalf("MarkForgeMirror(%q): %v", row.ID, err)
		}
	}
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	listed, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount: %v", err)
	}
	if len(listed.GetBoards()) != 1 {
		t.Fatalf("boards = %v, want the agent's board", listed.GetBoards())
	}
	asks := listed.GetBoards()[0].GetAsks()
	if len(asks) != 2 {
		t.Fatalf("board asks = %v, want two asks", asks)
	}
	var done, failed *compassv1.BoardAsk
	for _, ask := range asks {
		switch ask.GetAskId() {
		case firstID:
			done = ask
		case secondID:
			failed = ask
		}
	}
	if done == nil || done.GetMirrorState() != compassv1.BoardMirrorState_BOARD_MIRROR_STATE_DONE || done.GetMirrorUrl() != "https://github.com/owner/mirrors/issues/12#issuecomment-1" {
		t.Fatalf("done ask mirror = %v, want done with its URL", done)
	}
	if failed == nil || failed.GetMirrorState() != compassv1.BoardMirrorState_BOARD_MIRROR_STATE_FAILED || failed.GetMirrorUrl() != "" {
		t.Fatalf("failed ask mirror = %v, want failed without URL", failed)
	}
}

func TestListAgentBoardsOmitsOldAnsweredAsk(t *testing.T) {
	st, dsn := newTestStoreDSN(t)
	ctx := t.Context()
	bus := newBus(t)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "board-ask-age-root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	svc := NewComms(t.Context(), st, bus, nil, admin.ID)
	owner := mustUser(t, st, "board-ask-age-owner")
	agent := mustAgent(t, st, owner.ID, "board-ask-age-agent")
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "answer-window", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.UpdateBoardAsAccount(ctx, agent.ID, &compassv1.UpdateAgentBoardRequest{}); err != nil {
		t.Fatalf("UpdateBoardAsAccount: %v", err)
	}
	oldAsk := postBoardAsk(t, svc, ctx, agent.ID, channel.ID, "owner/repo#13")
	recentAsk := postBoardAsk(t, svc, ctx, agent.ID, channel.ID, "owner/repo#12")
	answer := []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"yes"}}}
	_, oldAnswer, err := st.AnswerAsk(ctx, owner.ID, oldAsk, answer)
	if err != nil {
		t.Fatalf("AnswerAsk(old): %v", err)
	}
	_, recentAnswer, err := st.AnswerAsk(ctx, owner.ID, recentAsk, answer)
	if err != nil {
		t.Fatalf("AnswerAsk(recent): %v", err)
	}
	oldSource, err := st.AskForForgeMirror(ctx, owner.ID, oldAsk)
	if err != nil {
		t.Fatalf("AskForForgeMirror(old): %v", err)
	}
	recentSource, err := st.AskForForgeMirror(ctx, owner.ID, recentAsk)
	if err != nil {
		t.Fatalf("AskForForgeMirror(recent): %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()
	now := time.Now()
	for _, aged := range []struct {
		id string
		at time.Time
	}{
		{id: string(oldSource.Message.ID), at: now.Add(-48 * time.Hour)},
		{id: string(oldAnswer.ID), at: now.Add(-36 * time.Hour)},
		{id: string(recentSource.Message.ID), at: now.Add(-3 * time.Hour)},
		{id: string(recentAnswer.ID), at: now.Add(-2 * time.Hour)},
	} {
		if _, err := pool.Exec(ctx, "UPDATE messages SET at_unix_ms = $1 WHERE id = $2", aged.at.UnixMilli(), aged.id); err != nil {
			t.Fatalf("age message %q: %v", aged.id, err)
		}
	}

	listed, err := svc.ListBoardsAsAccount(ctx, owner.ID, &compassv1.ListAgentBoardsRequest{})
	if err != nil {
		t.Fatalf("ListBoardsAsAccount: %v", err)
	}
	if len(listed.GetBoards()) != 1 || len(listed.GetBoards()[0].GetAsks()) != 1 {
		t.Fatalf("board asks = %v, want only the recent answered ask", listed.GetBoards())
	}
	ask := listed.GetBoards()[0].GetAsks()[0]
	if !ask.GetAnswered() || ask.GetRef() != "owner/repo#12" {
		t.Fatalf("recent answered ask = %v, want answered ask owner/repo#12", ask)
	}
}

func postBoardAsk(t *testing.T, svc *Comms, ctx context.Context, agent store.AccountID, channel store.ChannelID, ref string) string {
	t.Helper()
	posted, err := svc.PostAsAccount(ctx, agent, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(channel)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: testTopic},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{askWithRef(ref)},
	})
	if err != nil {
		t.Fatalf("PostAsAccount(ask %s): %v", ref, err)
	}
	return posted.GetMessage().GetBlocks()[0].GetAsk().GetAskId()
}
