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
	"github.com/RigelBuild/compass/go/internal/refs"
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
// by a fresh service and store handle: same body, stored session. The first
// render is taken from a live twin row so the bodies can be compared whole.
func TestMirrorMessagesRedriveAfterRestartUsesStoredPayloadAndSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(f *agentMirrorFixture, ref string) *compassv1internal.MirrorMessagesRequest
	}{
		{"topic payload", func(f *agentMirrorFixture, ref string) *compassv1internal.MirrorMessagesRequest {
			return &compassv1internal.MirrorMessagesRequest{Ref: ref, TopicId: f.topicID, Note: "Kept across restart."}
		}},
		{"message id payload", func(f *agentMirrorFixture, ref string) *compassv1internal.MirrorMessagesRequest {
			return &compassv1internal.MirrorMessagesRequest{Ref: ref, MessageIds: []string{f.msgIDs[1], f.msgIDs[0]}, Note: "Kept across restart."}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentMirrorFixture(t)
			live := f.mirror(t, f.svc, "call-live", tc.req(f, "owner/auto#12"))
			if live.GetIssueComment() == nil || len(f.author.Calls()) != 1 {
				t.Fatalf("live result = %v, want one posted comment", live)
			}
			f.svc.StopMirrorLifetime()
			pending := f.mirror(t, f.svc, "call-restart", tc.req(f, "owner/auto#14"))
			if pending.GetError().GetCode() != connect.CodeUnavailable.String() || len(f.author.Calls()) != 1 {
				t.Fatalf("closing-server result = %v, calls=%d; want unavailable and no new post", pending, len(f.author.Calls()))
			}
			fresh := f.service(t, openAgentMirrorStore(t, f.dsn))
			fresh.DriveAgent(f.ctx, f.agent.ID)
			calls := f.author.Calls()
			if len(calls) != 2 {
				t.Fatalf("provider calls after redrive = %+v, want one more post", calls)
			}
			// Each body ends with its own row marker; everything before it is the render.
			first, redriven := stripMirrorMarker(calls[0].Body), stripMirrorMarker(calls[1].Body)
			if redriven != first {
				t.Fatalf("redriven body =\n%s\nwant the first render\n%s", redriven, first)
			}
			if !strings.Contains(redriven, "session="+agentMirrorSession) || !strings.Contains(redriven, "Kept across restart.") {
				t.Fatalf("redriven body = %q, want the stored session and note", redriven)
			}
			if rows, err := fresh.mirrorStoreDB.OpenForgeMirrors(f.ctx, f.agent.ID); err != nil || len(rows) != 0 {
				t.Fatalf("open rows after redrive = (%+v, %v), want none", rows, err)
			}
		})
	}
}

func stripMirrorMarker(body string) string {
	if i := strings.LastIndex(body, "<!-- compass-mirror:"); i >= 0 {
		return body[:i]
	}
	return body
}

// A redrive after the agent lost the channel fails the row without posting,
// and a retry of the key reports the stored not_found, not a generic code.
func TestMirrorMessagesRedriveFailsWhenSourceIsNoLongerVisible(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(f *agentMirrorFixture) *compassv1internal.MirrorMessagesRequest
	}{
		{"topic payload", func(f *agentMirrorFixture) *compassv1internal.MirrorMessagesRequest {
			return &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID}
		}},
		{"message id payload", func(f *agentMirrorFixture) *compassv1internal.MirrorMessagesRequest {
			return &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", MessageIds: f.msgIDs}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentMirrorFixture(t)
			f.svc.StopMirrorLifetime()
			f.mirror(t, f.svc, "call-revoked", tc.req(f))
			if _, _, err := f.st.UpdateChannelMembers(f.ctx, f.owner.ID, f.channel.ID, []store.MemberUpdate{{AccountID: f.agent.ID, Remove: true}}, store.MemberUpdatesOptions{}); err != nil {
				t.Fatalf("UpdateChannelMembers(remove agent): %v", err)
			}
			fresh := f.service(t, f.st)
			fresh.DriveAgent(f.ctx, f.agent.ID)
			if calls := f.author.Calls(); len(calls) != 0 {
				t.Fatalf("provider calls = %+v, want none after the agent lost the channel", calls)
			}
			row, found, err := f.st.ForgeMirrorByRequest(f.ctx, f.agent.ID, "call-revoked")
			if err != nil || !found || row.State != 3 {
				t.Fatalf("row = (%+v, found=%v, err=%v), want failed", row, found, err)
			}
			retry := f.mirror(t, fresh, "call-revoked", tc.req(f))
			fe := retry.GetError()
			if fe.GetCode() != connect.CodeNotFound.String() || fe.GetMessage() != row.LastError || row.LastError == "" {
				t.Fatalf("retry after failed redrive = %v, want not_found with stored message %q", fe, row.LastError)
			}
			if calls := f.author.Calls(); len(calls) != 0 {
				t.Fatalf("provider calls after retry = %+v, want none", calls)
			}
		})
	}
}

// A same-key retry returns the stored outcome even after the target turned
// off or the agent lost the source: the policy checks gate new claims only.
func TestMirrorMessagesRetryAfterPolicyChangeReturnsFirstComment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, f *agentMirrorFixture)
	}{
		{"target off", func(t *testing.T, f *agentMirrorFixture) {
			t.Helper()
			if err := f.st.SetForgeMirrorSetting(f.ctx, f.owner.ID, store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Mode: store.ForgeMirrorOff}); err != nil {
				t.Fatalf("SetForgeMirrorSetting(off): %v", err)
			}
		}},
		{"agent removed from channel", func(t *testing.T, f *agentMirrorFixture) {
			t.Helper()
			if _, _, err := f.st.UpdateChannelMembers(f.ctx, f.owner.ID, f.channel.ID, []store.MemberUpdate{{AccountID: f.agent.ID, Remove: true}}, store.MemberUpdatesOptions{}); err != nil {
				t.Fatalf("UpdateChannelMembers(remove agent): %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentMirrorFixture(t)
			req := &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID}
			first := f.mirror(t, f.svc, "call-policy", req)
			if first.GetIssueComment() == nil {
				t.Fatalf("first result = %v, want a comment", first)
			}
			tc.change(t, f)
			retry := f.mirror(t, f.svc, "call-policy", req)
			if retry.GetIssueComment().GetUrl() != first.GetIssueComment().GetUrl() || retry.GetIssueComment().GetCommentId() != first.GetIssueComment().GetCommentId() {
				t.Fatalf("retry = %v, want the first CommentRef %v", retry, first)
			}
			if calls := f.author.Calls(); len(calls) != 1 {
				t.Fatalf("provider calls = %+v, want only the first post", calls)
			}
		})
	}
}

// Retrying the call that re-mirrored a source must not bump and edit again.
func TestMirrorMessagesRemirrorRetryDoesNotEditAgain(t *testing.T) {
	f := newAgentMirrorFixture(t)
	f.mirror(t, f.svc, "call-first", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Note: "one"})
	remirror := &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID, Note: "two"}
	second := f.mirror(t, f.svc, "call-second", remirror)
	row, found, err := f.st.ForgeMirrorByRequest(f.ctx, f.agent.ID, "call-second")
	if err != nil || !found {
		t.Fatalf("ForgeMirrorByRequest(call-second) = (found=%v, err=%v), want the re-mirrored row", found, err)
	}
	retry := f.mirror(t, f.svc, "call-second", remirror)
	if retry.GetIssueComment().GetCommentId() != second.GetIssueComment().GetCommentId() || second.GetIssueComment() == nil {
		t.Fatalf("re-mirror retry = %v, want %v", retry, second)
	}
	edits := 0
	for _, call := range f.author.Calls() {
		if call.Method == "EditComment" {
			edits++
		}
	}
	if edits != 1 {
		t.Fatalf("EditComment calls = %d, want 1 (the re-mirror, not its retry)", edits)
	}
	after, err := f.st.ForgeMirrorByID(f.ctx, row.ID)
	if err != nil || after.BodyRev != row.BodyRev {
		t.Fatalf("body_rev after retry = %d (err %v), want unchanged %d", after.BodyRev, err, row.BodyRev)
	}
}

// A key reused for another target is refused; the result arm comes from the
// stored target, so a PR row on the new ref cannot relabel the stored comment.
func TestMirrorMessagesRetryRejectsDifferentRefAndKeepsStoredArm(t *testing.T) {
	f := newAgentMirrorFixture(t)
	first := f.mirror(t, f.svc, "call-ref", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID})
	if first.GetIssueComment() == nil {
		t.Fatalf("first result = %v, want an issue comment", first)
	}
	if _, err := f.st.UpsertPullRequest(f.ctx, store.PullRequestRow{
		Coord: store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Number: 15},
		State: "open", CreatedAt: time.Unix(1_700_000_000, 0), UpdatedAt: time.Unix(1_700_000_000, 0), PR: []byte(`{}`),
	}, nil); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	other := f.mirror(t, f.svc, "call-ref", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#15", TopicId: f.topicID})
	if other.GetError().GetCode() != connect.CodeFailedPrecondition.String() {
		t.Fatalf("retry on another ref = %v, want failed_precondition", other)
	}
	same := f.mirror(t, f.svc, "call-ref", &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID})
	if same.GetIssueComment().GetCommentId() != first.GetIssueComment().GetCommentId() {
		t.Fatalf("retry on the stored ref = %v, want the stored issue comment", same)
	}
	if calls := f.author.Calls(); len(calls) != 1 {
		t.Fatalf("provider calls = %+v, want only the first post", calls)
	}
}

// A same-key retry while the first call's row is still pending is unavailable.
func TestMirrorMessagesPendingRetryIsUnavailable(t *testing.T) {
	f := newAgentMirrorFixture(t)
	f.svc.StopMirrorLifetime()
	req := &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID}
	f.mirror(t, f.svc, "call-pending", req)
	retry := f.mirror(t, f.service(t, f.st), "call-pending", req)
	if retry.GetError().GetCode() != connect.CodeUnavailable.String() {
		t.Fatalf("pending retry = %v, want unavailable", retry)
	}
	if calls := f.author.Calls(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none while pending", calls)
	}
}

// A claim that loses its key to a racing re-mirror gets back the re-mirrored
// row, whose own client_request_id is the first call's. The key reservation
// marks it a retry, so the caller gets the stored result, not a conflict.
func TestMirrorMessagesClaimResolvedByReservationIsARetry(t *testing.T) {
	f := newAgentMirrorFixture(t)
	req := &compassv1internal.MirrorMessagesRequest{Ref: "owner/auto#12", TopicId: f.topicID}
	first := f.mirror(t, f.svc, "call-first", req)
	if first.GetIssueComment() == nil {
		t.Fatalf("first result = %v, want a comment", first)
	}
	stored, found, err := f.st.ForgeMirrorByRequest(f.ctx, f.agent.ID, "call-first")
	if err != nil || !found {
		t.Fatalf("ForgeMirrorByRequest(call-first) = (found=%v, err=%v)", found, err)
	}
	// The racing re-mirror reserves call-shared on the existing row.
	remirrored, created, err := f.st.ClaimForgeMirror(f.ctx, store.ForgeMirrorRow{
		Source: 2, SourceKey: stored.SourceKey, Actor: f.agent.ID, Agent: f.agent.ID, ClientRequestID: "call-shared",
		TargetRef: stored.TargetRef, BodyRev: 1, State: 1, SessionID: agentMirrorSession, SourcePayload: stored.SourcePayload,
	})
	if err != nil || created || remirrored.ID != stored.ID || remirrored.ClientRequestID != "call-first" {
		t.Fatalf("re-mirror = (%+v, created=%v, err=%v), want the first row under its own key", remirrored, created, err)
	}
	f.svc.DriveAgent(f.ctx, f.agent.ID)
	done, err := f.st.ForgeMirrorByID(f.ctx, stored.ID)
	if err != nil || done.State != 2 {
		t.Fatalf("row after drive = (%+v, %v), want done", done, err)
	}
	calls := len(f.author.Calls())
	// The losing claim was for another source with the same key.
	ref, err := refs.Parse("owner/auto#12")
	if err != nil {
		t.Fatalf("refs.Parse: %v", err)
	}
	result := f.svc.claimedMirrorResult(f.ctx, f.agent.ID, "call-shared", ref, "another-source", done, false)
	if result.GetIssueComment().GetCommentId() != first.GetIssueComment().GetCommentId() {
		t.Fatalf("reserved-key claim result = %v, want the stored comment %v", result, first)
	}
	if got := len(f.author.Calls()); got != calls {
		t.Fatalf("provider calls = %d after the reserved-key claim, want %d", got, calls)
	}
}
