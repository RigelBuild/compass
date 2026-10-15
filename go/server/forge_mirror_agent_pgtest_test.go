//go:build pgtest && unix

package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

const agentMirrorSession = "sess-mirror-1"

// agentMirrorFixture is one tenant with an agent that can see one channel,
// a topic of messages in it, a channel it cannot see, and an auto target it
// holds scope on.
type agentMirrorFixture struct {
	ctx       context.Context
	dsn       string
	st        *store.Store
	owner     store.Account
	agent     store.Account
	channel   store.Channel
	topicID   string
	msgIDs    []string
	hidden    store.Channel
	hiddenMsg string
	hiddenTop string
	author    *forge.FakeProvider
	svc       *forgeService
}

func newAgentMirrorFixture(t *testing.T, texts ...string) *agentMirrorFixture {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	f := &agentMirrorFixture{dsn: dsn, st: openAgentMirrorStore(t, dsn)}
	f.ctx = store.WithTenant(context.Background(), f.st.EffectiveTenant(context.Background()))
	var err error
	if f.owner, err = f.st.CreateUser(f.ctx, store.NewUser{Handle: "mirror-tool-owner", DisplayName: "Mirror Tool Owner"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if f.agent, err = f.st.CreateAgent(f.ctx, f.owner.ID, store.NewAgent{Handle: "mirror-tool-agent", DisplayName: "Mirror Tool Agent"}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if f.channel, err = f.st.CreateChannel(f.ctx, f.owner.ID, store.NewChannel{Name: "mirror-tool", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{f.owner.ID, f.agent.ID}}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if len(texts) == 0 {
		texts = []string{"first message", "second message"}
	}
	for _, text := range texts {
		msg := f.appendText(t, f.channel.ID, f.owner.ID, "mirror-thread", text)
		f.topicID = msg.TopicID
		f.msgIDs = append(f.msgIDs, string(msg.ID))
	}
	other, err := f.st.CreateUser(f.ctx, store.NewUser{Handle: "mirror-tool-other", DisplayName: "Mirror Tool Other"})
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	if f.hidden, err = f.st.CreateChannel(f.ctx, other.ID, store.NewChannel{Name: "mirror-hidden", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{other.ID}}); err != nil {
		t.Fatalf("CreateChannel(hidden): %v", err)
	}
	hidden := f.appendText(t, f.hidden.ID, other.ID, "hidden-thread", "hidden message")
	f.hiddenMsg, f.hiddenTop = string(hidden.ID), hidden.TopicID
	for _, repo := range []string{"owner/auto", "owner/noscope"} {
		if err := f.st.SetForgeMirrorSetting(f.ctx, f.owner.ID, store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: repo, Mode: store.ForgeMirrorAuto}); err != nil {
			t.Fatalf("SetForgeMirrorSetting(%s): %v", repo, err)
		}
	}
	if err := f.st.SetForgeMirrorSetting(f.ctx, f.owner.ID, store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/manual", Mode: store.ForgeMirrorManual}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(manual): %v", err)
	}
	for _, repo := range []string{"owner/auto", "owner/manual", "owner/off"} {
		if err := f.st.GrantForgeScope(f.ctx, store.ForgeScope{AccountID: f.owner.ID, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: repo}); err != nil {
			t.Fatalf("GrantForgeScope(%s): %v", repo, err)
		}
	}
	f.author = forge.NewFakeProvider("gh-author")
	f.author.CommentResult = forge.Comment{ID: 21, Key: "tool-comment", URL: "https://github.com/owner/auto/issues/12#issuecomment-21"}
	f.author.EditCommentResult = f.author.CommentResult
	f.svc = f.service(t, f.st)
	return f
}

func openAgentMirrorStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsn) // test root context
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// service builds a forgeService over st with the fixture's fake author, as
// serve assembly does.
func (f *agentMirrorFixture) service(t *testing.T, st *store.Store) *forgeService {
	t.Helper()
	svc, _ := newForgeServiceForTest(t, f.author, forge.NewFakeProvider("gh-reviewer"))
	svc.store = st
	svc.mirrorStoreDB = st
	svc.enforceScopes = true
	t.Cleanup(svc.StopMirrorLifetime)
	return svc
}

func (f *agentMirrorFixture) appendText(t *testing.T, channel store.ChannelID, author store.AccountID, topic, text string) store.Message {
	t.Helper()
	body := text
	msg, _, err := f.st.AppendMessage(f.ctx, store.Message{AuthorAccountID: author, Blocks: []store.MessageBlock{{Text: &body}}}, string(channel), store.TopicRef{Name: topic, Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(%q): %v", text, err)
	}
	return msg
}

// mirror runs the arm through the forge dispatch as the fixture agent.
func (f *agentMirrorFixture) mirror(t *testing.T, svc *forgeService, key string, req *compassv1internal.MirrorMessagesRequest) *compassv1internal.ForgeCallResult {
	t.Helper()
	result, err := svc.ExecuteForgeCallAsAccount(f.ctx, f.agent.ID, agentMirrorSession, &compassv1internal.ForgeCallRequest{
		ClientRequestId: key,
		Call:            &compassv1internal.ForgeCallRequest_MirrorMessages{MirrorMessages: req},
	})
	if err != nil {
		t.Fatalf("ExecuteForgeCallAsAccount(mirror_messages): %v", err)
	}
	return result
}

func TestMirrorMessagesRejectionsMapToInvalidArgumentOrNotFound(t *testing.T) {
	f := newAgentMirrorFixture(t)
	fifty := make([]string, 51)
	for i := range fifty {
		fifty[i] = fmt.Sprintf("m-%02d", i)
	}
	for _, tc := range []struct {
		name string
		req  *compassv1internal.MirrorMessagesRequest
		code connect.Code
	}{
		{"both sources", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, MessageIds: f.msgIDs}, connect.CodeInvalidArgument},
		{"neither source", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12"}, connect.CodeInvalidArgument},
		{"more than 50 ids", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", MessageIds: fifty}, connect.CodeInvalidArgument},
		{"invalid ref", &compassv1internal.MirrorMessagesRequest{Ref: "not a ref", TopicId: f.topicID}, connect.CodeInvalidArgument},
		{"ids from two channels", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", MessageIds: []string{f.msgIDs[0], f.hiddenMsg}}, connect.CodeNotFound},
		{"invisible topic", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.hiddenTop}, connect.CodeNotFound},
		{"invisible message", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", MessageIds: []string{f.hiddenMsg}}, connect.CodeNotFound},
		{"unknown topic", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: "no-such-topic"}, connect.CodeNotFound},
		{"unknown channel name", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Channel: "no-such-channel"}, connect.CodeNotFound},
		{"invisible channel name", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Channel: "mirror-hidden"}, connect.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := f.mirror(t, f.svc, "", tc.req).GetError()
			if fe == nil || fe.GetCode() != tc.code.String() {
				t.Fatalf("mirror_messages error = %v, want %s", fe, tc.code)
			}
		})
	}
	if calls := f.author.Calls(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none for rejected calls", calls)
	}
	// An unknown and an invisible channel read the same, so neither leaks.
	unknown := f.mirror(t, f.svc, "", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Channel: "no-such-channel"}).GetError()
	invisible := f.mirror(t, f.svc, "", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Channel: "mirror-hidden"}).GetError()
	if unknown.GetMessage() != invisible.GetMessage() {
		t.Fatalf("unknown channel %q and invisible channel %q differ", unknown.GetMessage(), invisible.GetMessage())
	}
}

func TestMirrorMessagesRetryReturnsFirstCommentAndPostsOnce(t *testing.T) {
	f := newAgentMirrorFixture(t)
	req := &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", MessageIds: f.msgIDs, Channel: "mirror-tool", Note: "Context for review."}
	first := f.mirror(t, f.svc, "call-1", req)
	if first.GetIssueComment().GetUrl() != f.author.CommentResult.URL {
		t.Fatalf("first result = %v, want the posted issue comment", first)
	}
	second := f.mirror(t, f.svc, "call-1", req)
	if second.GetIssueComment().GetUrl() != first.GetIssueComment().GetUrl() || second.GetIssueComment().GetCommentId() != first.GetIssueComment().GetCommentId() {
		t.Fatalf("retry result = %v, want the first CommentRef %v", second, first)
	}
	calls := f.author.Calls()
	if len(calls) != 1 || calls[0].Method != "CommentOnIssue" {
		t.Fatalf("provider calls = %+v, want exactly one post", calls)
	}
	// The direct call's header names the calling session.
	if !strings.Contains(calls[0].Body, "agent=mirror-tool-agent owner=mirror-tool-owner session="+agentMirrorSession) {
		t.Fatalf("posted body = %q, want the caller's session in the owner header", calls[0].Body)
	}
	for _, want := range []string{"Context for review.", "first message", "second message", "#mirror-tool"} {
		if !strings.Contains(calls[0].Body, want) {
			t.Fatalf("posted body = %q, want %q", calls[0].Body, want)
		}
	}
}

func TestMirrorMessagesTopicTwiceEditsOneComment(t *testing.T) {
	f := newAgentMirrorFixture(t)
	first := f.mirror(t, f.svc, "call-a", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Note: "First note."})
	f.appendText(t, f.channel.ID, f.owner.ID, "mirror-thread", "third message")
	second := f.mirror(t, f.svc, "call-b", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Note: "Second note."})
	if first.GetIssueComment() == nil || second.GetIssueComment().GetCommentId() != first.GetIssueComment().GetCommentId() {
		t.Fatalf("results = %v then %v, want one comment", first, second)
	}
	calls := f.author.Calls()
	if len(calls) != 2 || calls[0].Method != "CommentOnIssue" || calls[1].Method != "EditComment" || calls[1].CommentKey != "tool-comment" {
		t.Fatalf("provider calls = %+v, want one post then an edit of it", calls)
	}
	if !strings.Contains(calls[1].Body, "Second note.") || strings.Contains(calls[1].Body, "First note.") || !strings.Contains(calls[1].Body, "third message") {
		t.Fatalf("edited body = %q, want the new note and message", calls[1].Body)
	}
}

func TestMirrorMessagesOverLimitDropsOldestWholeMessages(t *testing.T) {
	texts := make([]string, 6)
	for i := range texts {
		texts[i] = fmt.Sprintf("message-%d %s", i, strings.Repeat("x", 120))
	}
	f := newAgentMirrorFixture(t, texts...)
	f.author.BodyLimitResult = mirrorOwnerHeaderAllowance + 600
	result := f.mirror(t, f.svc, "call-limit", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID})
	if result.GetIssueComment() == nil {
		t.Fatalf("result = %v, want a posted comment", result)
	}
	body := f.author.Calls()[0].Body
	if !strings.Contains(body, "Dropped ") || !strings.Contains(body, "older messages.") {
		t.Fatalf("body = %q, want a dropped-messages note", body)
	}
	if strings.Contains(body, "message-0 ") || !strings.Contains(body, "message-5 ") {
		t.Fatalf("body = %q, want the oldest dropped and the newest kept", body)
	}
	if len(body) > f.author.BodyLimitResult {
		t.Fatalf("body has %d bytes, over the provider limit %d", len(body), f.author.BodyLimitResult)
	}
}

func TestMirrorMessagesRefusesOffManualAndUnscopedTargets(t *testing.T) {
	f := newAgentMirrorFixture(t)
	for _, tc := range []struct {
		name, ref string
		code      connect.Code
	}{
		{"off", "owner/off#1", connect.CodeFailedPrecondition},
		{"manual", "owner/manual#1", connect.CodeFailedPrecondition},
		{"no scope", "owner/noscope#1", connect.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := f.mirror(t, f.svc, "", &compassv1internal.MirrorMessagesRequest{Ref: tc.ref, TopicId: f.topicID}).GetError()
			if fe == nil || fe.GetCode() != tc.code.String() {
				t.Fatalf("mirror_messages(%s) error = %v, want %s", tc.ref, fe, tc.code)
			}
		})
	}
	if calls := f.author.Calls(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none", calls)
	}
}

func TestMirrorMessagesChannelNameDerivesOrMustMatch(t *testing.T) {
	f := newAgentMirrorFixture(t)
	other, err := f.st.CreateChannel(f.ctx, f.owner.ID, store.NewChannel{Name: "mirror-other", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{f.owner.ID, f.agent.ID}})
	if err != nil {
		t.Fatalf("CreateChannel(other): %v", err)
	}
	if fe := f.mirror(t, f.svc, "", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Channel: other.Name}).GetError(); fe.GetCode() != connect.CodeNotFound.String() {
		t.Fatalf("mismatched channel error = %v, want not_found", fe)
	}
	// A pull_requests row routes the post and the result to the PR arm.
	if _, err := f.st.UpsertPullRequest(f.ctx, store.PullRequestRow{
		Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Number: 13},
		State: "open", CreatedAt: time.Unix(1_700_000_000, 0), UpdatedAt: time.Unix(1_700_000_000, 0), PR: []byte(`{}`),
	}, nil); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	result := f.mirror(t, f.svc, "call-derived", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#13", MessageIds: f.msgIDs})
	if result.GetPrComment().GetUrl() != f.author.CommentResult.URL {
		t.Fatalf("omitted-channel result = %v, want a PR comment", result)
	}
	calls := f.author.Calls()
	if len(calls) != 1 || calls[0].Method != "CommentOnPullRequest" || !strings.Contains(calls[0].Body, "#mirror-tool") {
		t.Fatalf("provider calls = %+v, want one PR comment naming the derived channel", calls)
	}
}

// A row left pending by a server that stopped before driving it is redriven
// by a fresh service and store handle: same body, stored session.
func TestMirrorMessagesRedriveAfterRestartUsesStoredPayloadAndSession(t *testing.T) {
	f := newAgentMirrorFixture(t)
	f.svc.StopMirrorLifetime()
	pending := f.mirror(t, f.svc, "call-restart", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Note: "Kept across restart."})
	if pending.GetError().GetCode() != connect.CodeUnavailable.String() || len(f.author.Calls()) != 0 {
		t.Fatalf("closing-server result = %v, calls=%d; want unavailable and no post", pending, len(f.author.Calls()))
	}
	fresh := f.service(t, openAgentMirrorStore(t, f.dsn))
	fresh.DriveAgent(f.ctx, f.agent.ID)
	calls := f.author.Calls()
	if len(calls) != 1 {
		t.Fatalf("provider calls after redrive = %+v, want one post", calls)
	}
	for _, want := range []string{"session=" + agentMirrorSession, "Kept across restart.", "first message", "second message", "#mirror-tool"} {
		if !strings.Contains(calls[0].Body, want) {
			t.Fatalf("redriven body = %q, want %q", calls[0].Body, want)
		}
	}
	// The redriven body matches what the stored payload renders now.
	rows, err := fresh.mirrorStoreDB.OpenForgeMirrors(f.ctx, f.agent.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("open rows after redrive = (%+v, %v), want none", rows, err)
	}
}

// A redrive after the agent lost the channel fails the row without posting.
func TestMirrorMessagesRedriveFailsWhenSourceIsNoLongerVisible(t *testing.T) {
	f := newAgentMirrorFixture(t)
	f.svc.StopMirrorLifetime()
	f.mirror(t, f.svc, "call-revoked", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID})
	if _, _, err := f.st.UpdateChannelMembers(f.ctx, f.owner.ID, f.channel.ID, []store.MemberUpdate{{AccountID: f.agent.ID, Remove: true}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(remove agent): %v", err)
	}
	fresh := f.service(t, f.st)
	fresh.DriveAgent(f.ctx, f.agent.ID)
	if calls := f.author.Calls(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none after the agent lost the channel", calls)
	}
	retry := f.mirror(t, fresh, "call-revoked", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID})
	if fe := retry.GetError(); fe == nil || fe.GetCode() == connect.CodeUnavailable.String() {
		t.Fatalf("retry after failed redrive = %v, want the stored failure", retry)
	}
	var state int16
	rows, err := f.st.ForgeMirrorByID(f.ctx, mirrorRowIDForKey(t, f, "call-revoked"))
	if err == nil {
		state = rows.State
	}
	if state != 3 {
		t.Fatalf("row state = %d (err %v), want failed", state, err)
	}
}

func mirrorRowIDForKey(t *testing.T, f *agentMirrorFixture, key string) string {
	t.Helper()
	row, created, err := f.st.ClaimForgeMirror(f.ctx, store.ForgeMirrorRow{
		Source: 2, SourceKey: f.topicID, Actor: f.agent.ID, Agent: f.agent.ID, ClientRequestID: key,
		TargetRef: "owner/auto#12", BodyRev: 1, State: 1,
	})
	if err != nil || created {
		t.Fatalf("ClaimForgeMirror(lookup %q) = (created=%v, err=%v), want the existing row", key, created, err)
	}
	return row.ID
}
