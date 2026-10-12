//go:build pgtest && unix

package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

type forgeMirrorSignalDriver struct {
	inner comms.MirrorDriver
	done  chan struct{}
}

func (d forgeMirrorSignalDriver) DriveAsks(ctx context.Context, askIDs []string) {
	d.inner.DriveAsks(ctx, askIDs)
	d.done <- struct{}{}
}

func waitForForgeMirrorDrive(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ask mirror driver did not finish")
	}
}

type blockingMirrorPoster struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingMirrorPoster) PostMirror(ctx context.Context, _ store.AccountID, _ store.ForgeMirrorMode, _ refs.Ref, _ string, _ string, _ string) (forge.Comment, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return forge.Comment{ID: 1, Key: "blocked-comment", URL: "https://forge.example/comment"}, nil
	case <-ctx.Done():
		return forge.Comment{}, ctx.Err()
	}
}

func (*blockingMirrorPoster) ListMirrorComments(context.Context, store.AccountID, refs.Ref, time.Time) ([]forge.Comment, error) {
	return nil, nil
}

func (*blockingMirrorPoster) DeleteMirrorComment(context.Context, store.AccountID, refs.Ref, string) error {
	return nil
}

func (*blockingMirrorPoster) BodyLimit(refs.Ref) int { return 4096 }
func TestCommittedAskMirrorsAndAnswerEditsOneComment(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-ask-owner", DisplayName: "Mirror Ask Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mirror-ask-agent", DisplayName: "Mirror Ask Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "mirror-auto-asks", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{
		Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Mode: store.ForgeMirrorAuto,
	}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	if err := st.GrantForgeScope(ctx, store.ForgeScope{AccountID: owner.ID, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto"}); err != nil {
		t.Fatalf("GrantForgeScope: %v", err)
	}

	author := forge.NewFakeProvider("gh-author")
	author.CommentResult = forge.Comment{ID: 15, Key: "auto-comment", URL: "https://github.com/owner/auto/issues/12#issuecomment-15"}
	author.EditCommentResult = forge.Comment{ID: 15, Key: "auto-comment", URL: author.CommentResult.URL}
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	cm := comms.NewComms(st, commsBus, nil, owner.ID)
	forgeSvc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	forgeSvc.store = st
	forgeSvc.mirrorStoreDB = st
	forgeSvc.enforceScopes = true
	done := make(chan struct{}, 2)
	cm.SetMirrorer(forgeSvc, forgeMirrorSignalDriver{inner: forgeSvc, done: done})
	posted, err := cm.PostMessage(comms.WithActor(ctx, agent.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(channel.ID)},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: "ask-thread"}, CreateTopic: true,
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
			Ref: "owner/auto#12", Questions: []*compassv1.AskQuestion{{QuestionId: "q1", Question: "Ship this?", Options: []*compassv1.AskOption{{Id: "yes", Label: "Yes"}}}},
		}}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	waitForForgeMirrorDrive(t, done)
	askID := posted.Msg.GetMessage().GetBlocks()[0].GetAsk().GetAskId()
	assertForgeMirrorPosted(t, st, ctx, askID, 1)
	clicker, err := st.GetAccount(ctx, owner.ID)
	if err != nil {
		t.Fatalf("GetAccount(owner): %v", err)
	}
	clickURL, err := forgeSvc.MirrorAsUser(ctx, clicker, &compassv1.MirrorToForgeRequest{
		Ref: "owner/auto#12", AskId: askID, ChannelId: string(channel.ID),
	})
	if err != nil || clickURL != author.CommentResult.URL {
		t.Fatalf("MirrorAsUser(auto ask) = (%q, %v), want the existing comment URL", clickURL, err)
	}
	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "CommentOnIssue" || !strings.Contains(calls[0].Body, "agent=mirror-ask-agent owner=mirror-ask-owner") || !strings.Contains(calls[0].Body, "compass-mirror:") {
		t.Fatalf("automatic ask post calls = %+v, want one stamped comment with a marker", calls)
	}

	if _, err := cm.RespondToAsk(comms.WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId: askID, Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"yes"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}
	waitForForgeMirrorDrive(t, done)
	assertForgeMirrorPosted(t, st, ctx, askID, 2)
	calls = author.Calls()
	if len(calls) != 2 || calls[1].Method != "EditComment" || calls[1].CommentKey != "auto-comment" || !strings.Contains(calls[1].Body, "Yes") {
		t.Fatalf("automatic ask answer calls = %+v, want one edit to the original comment containing the answer", calls)
	}
}

func assertForgeMirrorPosted(t *testing.T, st *store.Store, ctx context.Context, askID string, postedRev int16) {
	t.Helper()
	rows, err := st.AskForgeMirrors(ctx, []string{askID})
	if err != nil {
		t.Fatalf("AskForgeMirrors: %v", err)
	}
	if len(rows) != 1 || rows[0].State != 2 || rows[0].PostedRev != postedRev || rows[0].CommentKey == "" {
		t.Fatalf("ask mirror rows = %+v, want one delivered row at revision %d", rows, postedRev)
	}
}

func TestForgeMirrorModeOffBeforePostMarksRowFailed(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-off-owner", DisplayName: "Mirror Off Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mirror-off-agent", DisplayName: "Mirror Off Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "mirror-off-channel", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	coord := store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Mode: store.ForgeMirrorAuto}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, coord); err != nil {
		t.Fatalf("SetForgeMirrorSetting(auto): %v", err)
	}
	text := "Ship this?"
	msg, _, err := st.AppendMessage(ctx, store.Message{
		AuthorAccountID: agent.ID,
		Blocks: []store.MessageBlock{{Ask: &store.Ask{Ref: "owner/auto#12", RefCoord: &store.ForgeCoord{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Number: 12}, Questions: []store.AskQuestion{{
			QuestionID: "q1", Question: text, Options: []store.AskOption{{ID: "yes", Label: "Yes"}},
		}}}}},
	}, string(channel.ID), store.TopicRef{Name: "mirror-off-topic", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{
		Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: store.ForgeMirrorOff,
	}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(off): %v", err)
	}
	author := forge.NewFakeProvider("gh-author")
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	svc.store = st
	svc.mirrorStoreDB = st
	svc.DriveAsks(ctx, []string{msg.Blocks[0].Ask.AskID})
	rows, err := st.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
	if err != nil {
		t.Fatalf("AskForgeMirrors: %v", err)
	}
	if len(rows) != 1 || rows[0].State != 3 {
		t.Fatalf("ask mirror rows = %+v, want one failed row", rows)
	}
	if calls := author.Calls(); len(calls) != 0 {
		t.Fatalf("forge provider calls = %+v, want no comment after disabling coordinate", calls)
	}
}
func TestForgeMirrorAsUserAuthorizesVisibleTopicAndEditsOneComment(t *testing.T) {
	ctx := context.Background()
	realStore := forgeTestStore(t)
	user, err := realStore.CreateUser(ctx, store.NewUser{Handle: "mirror-clicker", DisplayName: "Mirror Clicker"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := realStore.CreateChannel(ctx, user.ID, store.NewChannel{
		Name: "mirror-visible", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	text := "first source message"
	message, _, err := realStore.AppendMessage(ctx, store.Message{
		AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &text}},
	}, string(channel.ID), store.TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	author, svc, fakeStore := setupUserMirrorTest(t, realStore, user)
	request := &compassv1.MirrorToForgeRequest{Ref: "owner/repo#12", TopicId: message.TopicID, ChannelId: string(channel.ID)}
	url, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], request)
	if err != nil || url != author.CommentResult.URL {
		t.Fatalf("MirrorAsUser(first click) = (%q, %v), want successful topic delivery", url, err)
	}
	text = "second source message"
	if _, _, err := realStore.AppendMessage(ctx, store.Message{
		AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &text}},
	}, string(channel.ID), store.TopicRef{ID: message.TopicID}, ""); err != nil {
		t.Fatalf("AppendMessage(second): %v", err)
	}
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], request); err != nil {
		t.Fatalf("MirrorAsUser(second click): %v", err)
	}
	calls := author.Calls()
	if len(calls) != 2 || calls[0].Method != "CommentOnIssue" || calls[1].Method != "EditComment" {
		t.Fatalf("provider calls = %+v, want one comment followed by one edit", calls)
	}
	for _, call := range calls {
		clean, _, stamped := forge.StripOwner(call.Body)
		if stamped || strings.Count(clean, "Mirrored by @mirror-clicker") != 1 || !strings.Contains(call.Body, "compass-mirror:") {
			t.Errorf("manual body = %q, want one user attribution, a marker, and no owner header", call.Body)
		}
	}
	if !strings.Contains(calls[1].Body, "second source message") {
		t.Fatalf("edited body = %q, want current topic contents", calls[1].Body)
	}
}

func TestForgeMirrorAsUserRefusesInvisibleAndInvalidSources(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-visible-user", DisplayName: "Mirror Visible User"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := st.CreateChannel(ctx, user.ID, store.NewChannel{Name: "visible", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	other, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-invisible-user", DisplayName: "Mirror Invisible User"})
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	invisible, err := st.CreateChannel(ctx, other.ID, store.NewChannel{Name: "invisible", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{other.ID}})
	if err != nil {
		t.Fatalf("CreateChannel(invisible): %v", err)
	}
	_, svc, fakeStore := setupUserMirrorTest(t, st, user)
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{Ref: "owner/repo#12", TopicId: "missing", ChannelId: string(invisible.ID)}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("invisible channel error = %v, want not_found", err)
	}
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{Ref: "owner/repo#12", AskId: "ask", TopicId: "topic", ChannelId: string(channel.ID)}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid source error = %v, want invalid_argument", err)
	}
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{Ref: "owner/disabled#13", TopicId: "topic", ChannelId: string(channel.ID)}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("off target error = %v, want failed_precondition", err)
	}
}

func TestForgeMirrorAsUserMirrorsAskWithUserAttribution(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-ask-user", DisplayName: "Mirror Ask User"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := st.CreateChannel(ctx, user.ID, store.NewChannel{Name: "ask-channel", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Ask: &store.Ask{Ref: "owner/repo#12", Questions: []store.AskQuestion{{QuestionID: "q", Question: "Ship this ask?", Options: []store.AskOption{{ID: "yes", Label: "Yes"}}}}}}}}, string(channel.ID), store.TopicRef{Name: "asks", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}
	askID := msg.Blocks[0].Ask.AskID
	author, svc, fakeStore := setupUserMirrorTest(t, st, user)
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{Ref: "owner/repo#12", AskId: askID, ChannelId: string(channel.ID)}); err != nil {
		t.Fatalf("MirrorAsUser(ask): %v", err)
	}
	calls := author.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].Body, "Ship this ask?") || !strings.Contains(calls[0].Body, "Mirrored by @mirror-ask-user") {
		t.Fatalf("ask mirror calls = %+v, want ask text and user attribution", calls)
	}
}

func setupUserMirrorTest(t *testing.T, st *store.Store, user store.Account) (*forge.FakeProvider, *forgeService, *fakeForgeStore) {
	t.Helper()
	author := forge.NewFakeProvider("gh-author")
	author.CommentResult = forge.Comment{ID: 9, Key: "comment-9", URL: "https://github.com/owner/repo/issues/12#issuecomment-9"}
	author.EditCommentResult = forge.Comment{ID: 9, Key: "comment-9", URL: author.CommentResult.URL}
	svc, fakeStore := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	svc.mirrorStoreDB = st
	fakeStore.accounts[user.ID] = store.Account{ID: user.ID, Handle: user.Handle, User: &store.UserAccount{Role: store.UserRoleMember}}
	fakeStore.mirrorModes["1|github.com|owner/repo"] = store.ForgeMirrorManual
	return author, svc, fakeStore
}

func TestForgeMirrorClickWaitIsBoundedByRequestContext(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-blocked", DisplayName: "Mirror Blocked"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := st.SetForgeMirrorSetting(ctx, user.ID, store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Mode: store.ForgeMirrorManual}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	row, created, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: "topic", Actor: user.ID, TargetRef: "owner/repo#12", BodyRev: 1, State: 1})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: row=%+v created=%v err=%v", row, created, err)
	}
	poster := &blockingMirrorPoster{entered: make(chan struct{}, 1), release: make(chan struct{})}
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	svc.store = st
	svc.mirrorStoreDB = st
	svc.mirrorPoster = poster
	requestCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		select {
		case <-poster.release:
		default:
			close(poster.release)
		}
		svc.StopMirrorLifetime()
	})
	result := make(chan error, 1)
	go func() {
		_, err := svc.clickDrive(requestCtx, row, func(context.Context, store.ForgeMirrorRow, int) (string, error) {
			return "source", nil
		}, func(code connect.Code, err error) (string, error) {
			return "", connect.NewError(code, err)
		})
		result <- err
	}()
	select {
	case <-poster.entered:
	case <-time.After(5 * time.Second):
		close(poster.release)
		t.Fatal("blocking poster did not start")
	}
	cancel()
	select {
	case err := <-result:
		if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "mirror still delivering") {
			t.Fatalf("click result = %v; want unavailable while delivery continues", err)
		}
	case <-time.After(5 * time.Second):
		close(poster.release)
		t.Fatal("MirrorAsUser did not return after request cancellation")
	}
	close(poster.release)
	svc.StopMirrorLifetime()
}

func TestForgeMirrorSettingsHandlersEnforceTenantAdmin(t *testing.T) {
	ctx := context.Background()
	st, admin, member := newNetworkStore(t)
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
		Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/settings", Enabled: true,
	}); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription: %v", err)
	}
	adminToken, err := auth.IssueAccountToken(ctx, st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}
	memberToken, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	client := networkDoorHandler(t, newService("forge-mirror-settings", bus, st, nil, nil, nil, nil), st, admin)
	setRequest := func(token string, mode compassv1.ForgeMirrorMode) *connect.Request[compassv1.SetForgeMirrorSettingRequest] {
		req := connect.NewRequest(&compassv1.SetForgeMirrorSettingRequest{Setting: &compassv1.ForgeMirrorSetting{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
			Repo:  "owner/settings", Mode: mode,
		}})
		req.Header().Set("Authorization", "Bearer "+token)
		return req
	}

	if _, err := client.SetForgeMirrorSetting(ctx, setRequest(memberToken, compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_AUTO)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("member SetForgeMirrorSetting code = %v, err = %v; want permission_denied", connect.CodeOf(err), err)
	}
	if _, err := client.SetForgeMirrorSetting(ctx, setRequest(adminToken, compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_UNSPECIFIED)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("UNSPECIFIED SetForgeMirrorSetting code = %v, err = %v; want invalid_argument", connect.CodeOf(err), err)
	}
	if _, err := client.SetForgeMirrorSetting(ctx, setRequest(adminToken, compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_AUTO)); err != nil {
		t.Fatalf("admin SetForgeMirrorSetting: %v", err)
	}

	listRequest := func(token string) *connect.Request[compassv1.ListForgeMirrorSettingsRequest] {
		req := connect.NewRequest(&compassv1.ListForgeMirrorSettingsRequest{})
		req.Header().Set("Authorization", "Bearer "+token)
		return req
	}
	memberList, err := client.ListForgeMirrorSettings(ctx, listRequest(memberToken))
	if err != nil {
		t.Fatalf("member ListForgeMirrorSettings: %v", err)
	}
	if memberList.Msg.GetCanEdit() || len(memberList.Msg.GetSettings()) != 1 || memberList.Msg.GetSettings()[0].GetMode() != compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_AUTO {
		t.Fatalf("member settings response = %+v, want auto setting and can_edit=false", memberList.Msg)
	}
	adminList, err := client.ListForgeMirrorSettings(ctx, listRequest(adminToken))
	if err != nil {
		t.Fatalf("admin ListForgeMirrorSettings: %v", err)
	}
	if !adminList.Msg.GetCanEdit() || len(adminList.Msg.GetSettings()) != 1 {
		t.Fatalf("admin settings response = %+v, want one setting and can_edit=true", adminList.Msg)
	}
}
