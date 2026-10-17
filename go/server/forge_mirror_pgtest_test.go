//go:build pgtest && unix

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/mirror"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

type forgeMirrorLeaseLossPoster struct {
	store     *store.Store
	rowID     string
	deleteRow bool
}

func (*forgeMirrorLeaseLossPoster) PostMirror(context.Context, store.AccountID, string, store.ForgeMirrorMode, refs.Ref, string, string, string) (forge.Comment, error) {
	return forge.Comment{}, errors.New("unexpected post during recheck")
}

func (p *forgeMirrorLeaseLossPoster) ListMirrorComments(ctx context.Context, _ store.AccountID, _ refs.Ref, _ time.Time) ([]forge.Comment, error) {
	if err := p.store.WithTx(ctx, func(tx pgx.Tx) error {
		query := `UPDATE forge_mirrors SET lease_until = now() - interval '1 second' WHERE id = $1`
		if p.deleteRow {
			query = `DELETE FROM forge_mirrors WHERE id = $1`
		}
		_, err := tx.Exec(ctx, query, p.rowID)
		return err
	}); err != nil {
		return nil, err
	}
	return []forge.Comment{{ID: 30, Key: "comment", Body: mirror.WithMarker("body", p.rowID)}}, nil
}

func (*forgeMirrorLeaseLossPoster) DeleteMirrorComment(context.Context, store.AccountID, refs.Ref, string) error {
	return nil
}

func (*forgeMirrorLeaseLossPoster) BodyLimit(refs.Ref) int { return 4096 }

func TestForgeMirrorRecheckLeaseLossReschedulesOnce(t *testing.T) {
	delays, timers := runLeaseLossRecheck(t, "mirror-lease-loss", false)
	if len(delays) != 2 || delays[1] != time.Minute || timers != 1 {
		t.Fatalf("scheduled delays=%v pending timers=%d, want one lease-loss retry at 1m", delays, timers)
	}
}

func TestForgeMirrorRecheckLeaseLossRetriesWhenRereadFails(t *testing.T) {
	delays, timers := runLeaseLossRecheck(t, "mirror-lease-reread", true)
	if len(delays) != 2 || delays[1] != time.Minute || timers != 1 {
		t.Fatalf("scheduled delays=%v pending timers=%d, want one retry at 1m after a failed reread", delays, timers)
	}
}

func runLeaseLossRecheck(t *testing.T, handle string, deleteRow bool) ([]time.Duration, int) {
	t.Helper()
	ctx := context.Background()
	st := forgeTestStore(t)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: handle, DisplayName: "Mirror Lease Loss"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	row, created, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{
		Source: 2, SourceKey: handle + "-recheck", Actor: owner.ID, TargetRef: "owner/repo#30", BodyRev: 1, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: created=%v err=%v", created, err)
	}
	if _, ok, err := st.LeaseForgeMirror(ctx, row.ID, "lease-loss-setup", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "lease-loss-setup", store.ForgeMirrorMark{PostedRev: 1, State: 2, CommentID: 30, CommentKey: "comment", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror: %v", err)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second' WHERE id = $1`, row.ID)
		return err
	}); err != nil {
		t.Fatalf("expire recheck: %v", err)
	}
	row, err = st.ForgeMirrorByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("ForgeMirrorByID: %v", err)
	}
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	svc.mirrorStoreDB = st
	svc.mirrorPoster = &forgeMirrorLeaseLossPoster{store: st, rowID: row.ID, deleteRow: deleteRow}
	delays := make([]time.Duration, 0, 2)
	callbacks := make([]func(), 0, 2)
	svc.scheduleMirrorAfter = func(delay time.Duration, fn func()) func() bool {
		delays = append(delays, delay)
		callbacks = append(callbacks, fn)
		return func() bool { return true }
	}
	svc.SetMirrorLifetime(ctx)
	t.Cleanup(svc.StopMirrorLifetime)
	svc.scheduleMirrorRecheck(ctx, row, nil, false)
	callbacks[0]()
	return delays, len(svc.mirrorTimers)
}

func TestForgeMirrorFailedRechecksKeepOnePendingTimer(t *testing.T) {
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	delays := make([]time.Duration, 0, 3)
	svc.scheduleMirrorAfter = func(delay time.Duration, _ func()) func() bool {
		delays = append(delays, delay)
		return func() bool { return true }
	}
	svc.SetMirrorLifetime(context.Background())
	t.Cleanup(svc.StopMirrorLifetime)
	recheckAt := time.Now().Add(-time.Second)
	row := store.ForgeMirrorRow{ID: "failed-recheck", RecheckAt: &recheckAt}
	for range 3 {
		svc.scheduleMirrorRecheck(context.Background(), row, nil, true)
	}
	if len(delays) != 1 || delays[0] != time.Minute || len(svc.mirrorTimers) != 1 {
		t.Fatalf("scheduled delays = %v pending timers = %d, want one 1m retry", delays, len(svc.mirrorTimers))
	}
}

func TestForgeMirrorFailedDueRecheckBacksOff(t *testing.T) {
	ctx := context.Background()
	storeDB := forgeTestStore(t)
	user, err := storeDB.CreateUser(ctx, store.NewUser{Handle: "mirror-retry-user", DisplayName: "Mirror Retry User"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := storeDB.SetForgeMirrorSetting(ctx, user.ID, store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Mode: store.ForgeMirrorManual}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	row, created, err := storeDB.ClaimForgeMirror(ctx, store.ForgeMirrorRow{
		Source: 2, SourceKey: "retry-topic", Actor: user.ID, TargetRef: "owner/repo#13", BodyRev: 1, PostedRev: 0, State: 1,
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: created=%v err=%v", created, err)
	}
	if _, ok, err := storeDB.LeaseForgeMirror(ctx, row.ID, "retry-lease", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if err := storeDB.MarkForgeMirror(ctx, row.ID, "retry-lease", store.ForgeMirrorMark{PostedRev: 1, State: 2, CommentKey: "comment", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror: %v", err)
	}
	if err := storeDB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second' WHERE id = $1`, row.ID)
		return err
	}); err != nil {
		t.Fatalf("expire recheck: %v", err)
	}
	row, err = storeDB.ForgeMirrorByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("ForgeMirrorByID: %v", err)
	}
	poster := forge.NewFakeProvider("gh-author")
	poster.SetError("ListCommentsSince", errors.New("temporary comment scan failure"))
	svc, _ := newForgeServiceForTest(t, poster, forge.NewFakeProvider("gh-reviewer"))
	svc.providers.entries[forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: "github.com"}] = forgeProviderRoles{
		author:   &mirrorIdentityProvider{Provider: poster, login: "gh-author"},
		reviewer: forge.NewFakeProvider("gh-reviewer"),
	}
	svc.store = storeDB
	svc.mirrorStoreDB = storeDB
	delays := make([]time.Duration, 0, 4)
	callbacks := make([]func(), 0, 4)
	svc.scheduleMirrorAfter = func(delay time.Duration, fn func()) func() bool {
		delays = append(delays, delay)
		callbacks = append(callbacks, fn)
		return func() bool { return true }
	}
	svc.SetMirrorLifetime(ctx)
	t.Cleanup(svc.StopMirrorLifetime)
	svc.scheduleMirrorRecheck(ctx, row, nil, false)
	for range 3 {
		if err := storeDB.WithTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second' WHERE id = $1`, row.ID)
			return err
		}); err != nil {
			t.Fatalf("expire retry lease: %v", err)
		}
		callbacks[len(callbacks)-1]()
	}
	if len(delays) != 4 || delays[0] != 0 || delays[1] < time.Minute || delays[1] > 2*time.Minute || delays[2] != 5*time.Minute || delays[3] != 15*time.Minute {
		t.Fatalf("scheduled delays = %v, want immediate due check, lease expiry, then 5m and 15m retries", delays)
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
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], request); err != nil {
		t.Fatalf("MirrorAsUser(topic): %v", err)
	}
	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "CommentOnIssue" || !strings.Contains(calls[0].Body, "first source message") {
		t.Fatalf("topic mirror calls = %+v, want one comment with source", calls)
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
	calls = author.Calls()
	if len(calls) != 2 || calls[0].Method != "CommentOnIssue" || calls[1].Method != "EditComment" || calls[1].CommentKey != "comment-9" {
		t.Fatalf("provider calls = %+v, want one comment followed by an edit of the same comment", calls)
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

func TestForgeMirrorSelectedMessageIDsCanonicalizeAndRejectDuplicates(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-selected-canonical", DisplayName: "Mirror Selected Canonical"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := st.CreateChannel(ctx, user.ID, store.NewChannel{
		Name: "mirror-selected-canonical", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	firstText, secondText := "first selected message", "second selected message"
	first, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &firstText}}}, string(channel.ID), store.TopicRef{Name: "first", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(first): %v", err)
	}
	second, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &secondText}}}, string(channel.ID), store.TopicRef{Name: "second", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(second): %v", err)
	}
	author, svc, fakeStore := setupUserMirrorTest(t, st, user)
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{
		Ref: "owner/repo#12", ChannelId: string(channel.ID), MessageIds: []string{string(first.ID), string(second.ID), string(first.ID)},
	}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("MirrorAsUser(duplicate IDs) error = %v, want invalid_argument", err)
	}
	if calls := author.Calls(); len(calls) != 0 {
		t.Fatalf("provider calls after duplicate IDs = %+v, want none", calls)
	}
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{
		Ref: "owner/repo#12", ChannelId: string(channel.ID), MessageIds: []string{string(second.ID), string(first.ID)},
	}); err != nil {
		t.Fatalf("MirrorAsUser(reversed IDs): %v", err)
	}
	if _, err := svc.MirrorAsUser(ctx, fakeStore.accounts[user.ID], &compassv1.MirrorToForgeRequest{
		Ref: "owner/repo#12", ChannelId: string(channel.ID), MessageIds: []string{string(first.ID), string(second.ID)},
	}); err != nil {
		t.Fatalf("MirrorAsUser(forward IDs): %v", err)
	}
	calls := author.Calls()
	if len(calls) != 2 || calls[0].Method != "CommentOnIssue" || calls[1].Method != "EditComment" || calls[1].CommentKey != "comment-9" {
		t.Fatalf("provider calls = %+v, want one comment followed by edit of the same comment", calls)
	}
	if !strings.Contains(calls[1].Body, firstText) || !strings.Contains(calls[1].Body, secondText) || strings.Count(calls[1].Body, "Mirrored by @mirror-selected-canonical") != 1 {
		t.Fatalf("edited body = %q, want both selected messages and one user attribution", calls[1].Body)
	}
}

func TestForgeMirrorSelectedMessageRequiresCurrentMembership(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-membership", DisplayName: "Mirror Membership"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := st.CreateChannel(ctx, user.ID, store.NewChannel{
		Name: "mirror-membership", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	text := "secret source"
	posted, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &text}}}, string(channel.ID), store.TopicRef{Name: "membership-topic", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if _, _, err := st.UpdateChannelMembers(ctx, user.ID, channel.ID, []store.MemberUpdate{{AccountID: user.ID, Remove: true}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(remove self): %v", err)
	}
	_, svc, fakeStore := setupUserMirrorTest(t, st, user)
	request := &compassv1.MirrorToForgeRequest{ChannelId: string(channel.ID), MessageIds: []string{string(posted.ID)}}
	_, _, err = svc.resolveUserMirrorSource(ctx, fakeStore.accounts[user.ID], request, channel.Name)
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("resolveUserMirrorSource after membership removal = %v, want not_found", err)
	}
}

func TestForgeMirrorTopicKeepsNamesWithoutPublicURL(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-topic-names", DisplayName: "Mirror Topic Names"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := st.CreateChannel(ctx, user.ID, store.NewChannel{
		Name: "topic-channel-name", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	text := "topic message"
	message, _, err := st.AppendMessage(ctx, store.Message{
		AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &text}},
	}, string(channel.ID), store.TopicRef{Name: "topic-name", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	_, svc, fakeStore := setupUserMirrorTest(t, st, user)
	svc.publicURL = ""
	request := &compassv1.MirrorToForgeRequest{Ref: "owner/repo#12", TopicId: message.TopicID, ChannelId: string(channel.ID)}
	_, body, err := svc.resolveUserMirrorSource(ctx, fakeStore.accounts[user.ID], request, channel.Name)
	if err != nil {
		t.Fatalf("resolveUserMirrorSource: %v", err)
	}
	rendered, err := body(ctx, store.ForgeMirrorRow{}, 4096)
	if err != nil {
		t.Fatalf("render topic body: %v", err)
	}
	if !strings.Contains(rendered, "#topic-channel-name › topic-name") || strings.Contains(rendered, string(channel.ID)) || strings.Contains(rendered, message.TopicID) {
		t.Fatalf("topic body header = %q, want channel and topic names without IDs", rendered)
	}
}

func TestForgeMirrorTopicNotesNewestFiftyMessages(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-topic-limit", DisplayName: "Mirror Topic Limit"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	channel, err := st.CreateChannel(ctx, user.ID, store.NewChannel{
		Name: "topic-limit-channel", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{user.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	var topicID string
	for i := range 51 {
		text := fmt.Sprintf("topic message %02d", i)
		topic := store.TopicRef{ID: topicID}
		if topicID == "" {
			topic = store.TopicRef{Name: "topic-limit", Create: true}
		}
		message, _, err := st.AppendMessage(ctx, store.Message{
			AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Text: &text}},
		}, string(channel.ID), topic, "")
		if err != nil {
			t.Fatalf("AppendMessage(%d): %v", i, err)
		}
		topicID = message.TopicID
	}
	_, svc, fakeStore := setupUserMirrorTest(t, st, user)
	request := &compassv1.MirrorToForgeRequest{Ref: "owner/repo#12", TopicId: topicID, ChannelId: string(channel.ID)}
	_, body, err := svc.resolveUserMirrorSource(ctx, fakeStore.accounts[user.ID], request, channel.Name)
	if err != nil {
		t.Fatalf("resolveUserMirrorSource: %v", err)
	}
	rendered, err := body(ctx, store.ForgeMirrorRow{}, 100_000)
	if err != nil {
		t.Fatalf("render topic body: %v", err)
	}
	if !strings.Contains(rendered, "Showing the newest 50 messages.") || !strings.Contains(rendered, "topic message 50") || strings.Contains(rendered, "topic message 00") {
		t.Fatalf("topic mirror body omits cap note or wrong messages: %q", rendered)
	}
}

type forgeMirrorSignalDriver struct {
	inner comms.MirrorDriver
	done  chan struct{}
}

func (d forgeMirrorSignalDriver) DriveAsks(ctx context.Context, askIDs []string) {
	d.inner.DriveAsks(ctx, askIDs)
	d.done <- struct{}{}
}

func (d forgeMirrorSignalDriver) DriveAgent(ctx context.Context, agent store.AccountID) {
	d.inner.DriveAgent(ctx, agent)
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
	coord := store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Mode: store.ForgeMirrorAuto}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, coord); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	if err := st.GrantForgeScope(ctx, store.ForgeScope{AccountID: owner.ID, Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo}); err != nil {
		t.Fatalf("GrantForgeScope: %v", err)
	}
	author := forge.NewFakeProvider("gh-author")
	author.CommentResult = forge.Comment{ID: 15, Key: "auto-comment", URL: "https://github.com/owner/auto/issues/12#issuecomment-15"}
	author.EditCommentResult = forge.Comment{ID: 15, Key: "auto-comment", URL: author.CommentResult.URL}
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	cm := comms.NewComms(t.Context(), st, commsBus, nil, owner.ID)
	forgeSvc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	forgeSvc.store = st
	forgeSvc.mirrorStoreDB = st
	forgeSvc.enforceScopes = true
	done := make(chan struct{}, 2)
	cm.SetMirrorer(forgeSvc, forgeMirrorSignalDriver{inner: forgeSvc, done: done})
	// The relay puts the verified posting session on ctx; the ask row keeps it.
	posted, err := cm.PostMessage(comms.WithActor(store.WithAgentSession(ctx, "sess-ask"), agent.ID), &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(channel.ID)},
		Topic:     &compassv1.PostMessageRequest_TopicName{TopicName: "ask-thread"}, CreateTopic: true,
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
			Ref: "owner/auto#12", Questions: []*compassv1.AskQuestion{{QuestionId: "q1", Question: "Ship this?", Options: []*compassv1.AskOption{{Id: "yes", Label: "Yes"}}}},
		}}}},
	})
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	waitForForgeMirrorDrive(t, done)
	askID := posted.GetMessage().GetBlocks()[0].GetAsk().GetAskId()
	assertForgeMirrorPosted(t, st, ctx, askID, 1)
	clicker, err := st.GetAccount(ctx, owner.ID)
	if err != nil {
		t.Fatalf("GetAccount(owner): %v", err)
	}
	clickURL, err := forgeSvc.MirrorAsUser(ctx, clicker, &compassv1.MirrorToForgeRequest{Ref: "owner/auto#12", AskId: askID, ChannelId: string(channel.ID)})
	if err != nil || clickURL != author.CommentResult.URL {
		t.Fatalf("MirrorAsUser(auto ask) = (%q, %v), want existing comment URL", clickURL, err)
	}
	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "CommentOnIssue" || !strings.Contains(calls[0].Body, "agent=mirror-ask-agent owner=mirror-ask-owner session=sess-ask") || !strings.Contains(calls[0].Body, "compass-mirror:") {
		t.Fatalf("automatic ask post calls = %+v, want one stamped comment with a marker", calls)
	}
	if !strings.Contains(calls[0].Body, "#mirror-auto-asks › ask-thread") || strings.Contains(calls[0].Body, string(channel.ID)) || strings.Contains(calls[0].Body, askID) {
		t.Fatalf("automatic ask body = %q, want channel/topic names without ids", calls[0].Body)
	}
	if _, err := cm.RespondToAsk(comms.WithActor(ctx, owner.ID), &compassv1.RespondToAskRequest{
		AskId: askID, Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"yes"}}},
	}); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}
	waitForForgeMirrorDrive(t, done)
	assertForgeMirrorPosted(t, st, ctx, askID, 2)
	calls = author.Calls()
	if len(calls) != 2 || calls[1].Method != "EditComment" || calls[1].CommentKey != "auto-comment" || !strings.Contains(calls[1].Body, "Yes") {
		t.Fatalf("automatic ask answer calls = %+v, want edit of original comment containing answer", calls)
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
	msg, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: agent.ID, Blocks: []store.MessageBlock{{Ask: &store.Ask{
		Ref: "owner/auto#12", RefCoord: &store.ForgeCoord{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Number: 12},
		Questions: []store.AskQuestion{{QuestionID: "q1", Question: text, Options: []store.AskOption{{ID: "yes", Label: "Yes"}}}},
	}}}}, string(channel.ID), store.TopicRef{Name: "mirror-off-topic", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: store.ForgeMirrorOff}); err != nil {
		t.Fatalf("SetForgeMirrorSetting(off): %v", err)
	}
	author := forge.NewFakeProvider("gh-author")
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	svc.store = st
	svc.mirrorStoreDB = st
	svc.DriveAsks(ctx, []string{msg.Blocks[0].Ask.AskID})
	rows, err := st.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
	if err != nil || len(rows) != 1 || rows[0].State != 3 {
		t.Fatalf("ask mirror rows = %+v, err=%v; want one failed row", rows, err)
	}
	if calls := author.Calls(); len(calls) != 0 {
		t.Fatalf("forge provider calls = %+v, want no comment after disabling coordinate", calls)
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
	ask := &store.Ask{Ref: "owner/repo#12", Questions: []store.AskQuestion{{QuestionID: "q", Question: "Ship this ask?", Options: []store.AskOption{{ID: "yes", Label: "Yes"}}}}}
	msg, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: user.ID, Blocks: []store.MessageBlock{{Ask: ask}}}, string(channel.ID), store.TopicRef{Name: "asks", Create: true}, "")
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

type blockingMirrorPoster struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingMirrorPoster) PostMirror(ctx context.Context, _ store.AccountID, _ string, _ store.ForgeMirrorMode, _ refs.Ref, _ string, _ string, _ string) (forge.Comment, error) {
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
			return "", connect.NewError(code, err.Error()).WithCause(err)
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
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/settings", Enabled: true}); err != nil {
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
	setRequest := func(token string, mode compassv1.ForgeMirrorMode) *compassv1.SetForgeMirrorSettingRequest {
		req := &compassv1.SetForgeMirrorSettingRequest{Setting: &compassv1.ForgeMirrorSetting{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"},
			Repo:  "owner/settings", Mode: mode,
		}}
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
	listRequest := func(token string) *compassv1.ListForgeMirrorSettingsRequest {
		req := &compassv1.ListForgeMirrorSettingsRequest{}
		req.Header().Set("Authorization", "Bearer "+token)
		return req
	}
	memberList, err := client.ListForgeMirrorSettings(ctx, listRequest(memberToken))
	if err != nil {
		t.Fatalf("member ListForgeMirrorSettings: %v", err)
	}
	if memberList.GetCanEdit() || len(memberList.GetSettings()) != 1 || memberList.GetSettings()[0].GetMode() != compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_AUTO {
		t.Fatalf("member settings response = %+v, want auto setting and can_edit=false", memberList)
	}
	adminList, err := client.ListForgeMirrorSettings(ctx, listRequest(adminToken))
	if err != nil {
		t.Fatalf("admin ListForgeMirrorSettings: %v", err)
	}
	if !adminList.GetCanEdit() || len(adminList.GetSettings()) != 1 {
		t.Fatalf("admin settings response = %+v, want one setting and can_edit=true", adminList)
	}
}

type driveAgentMirrorPoster struct {
	bodies []string
}

func (p *driveAgentMirrorPoster) PostMirror(_ context.Context, _ store.AccountID, _ string, _ store.ForgeMirrorMode, _ refs.Ref, _ string, _ string, body string) (forge.Comment, error) {
	p.bodies = append(p.bodies, body)
	return forge.Comment{ID: 17, Key: "agent-comment", URL: "https://github.com/owner/auto/issues/12#issuecomment-17"}, nil
}

func (*driveAgentMirrorPoster) ListMirrorComments(context.Context, store.AccountID, refs.Ref, time.Time) ([]forge.Comment, error) {
	return nil, nil
}

func (*driveAgentMirrorPoster) DeleteMirrorComment(context.Context, store.AccountID, refs.Ref, string) error {
	return nil
}

func (*driveAgentMirrorPoster) BodyLimit(refs.Ref) int { return 4096 }

func TestForgeMirrorDriveAgentDeliversOpenAskRow(t *testing.T) {
	st := forgeTestStore(t)
	tenant := st.EffectiveTenant(context.Background())
	ctx := store.WithTenant(context.Background(), tenant)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-open-row-owner", DisplayName: "Mirror Open Row Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mirror-open-row-agent", DisplayName: "Mirror Open Row Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "mirror-open-row", Kind: store.ChannelKindChannel, MemberAccountIDs: []store.AccountID{owner.ID, agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/auto", Number: 12}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{
		Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: store.ForgeMirrorAuto,
	}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	message, _, err := st.AppendMessage(ctx, store.Message{
		AuthorAccountID: agent.ID,
		Blocks: []store.MessageBlock{{Ask: &store.Ask{
			Ref: "owner/auto#12", RefCoord: &coord,
			Questions: []store.AskQuestion{{QuestionID: "q1", Question: "Re-drive this ask?", Options: []store.AskOption{{ID: "yes", Label: "Yes"}}}},
		}}},
	}, string(channel.ID), store.TopicRef{Name: "open-ask", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	askID := message.Blocks[0].Ask.AskID
	rows, err := st.AskForgeMirrors(ctx, []string{askID})
	if err != nil || len(rows) != 1 || rows[0].State != 1 {
		t.Fatalf("AskForgeMirrors before drive = (%+v, %v), want one pending row", rows, err)
	}
	poster := &driveAgentMirrorPoster{}
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	svc.mirrorStoreDB = st
	svc.mirrorPoster = poster
	svc.DriveAgent(ctx, agent.ID)
	if len(poster.bodies) != 1 || !strings.Contains(poster.bodies[0], "Re-drive this ask?") {
		t.Fatalf("DriveAgent post bodies = %q, want the open ask delivered", poster.bodies)
	}
	row, err := st.ForgeMirrorByID(ctx, rows[0].ID)
	if err != nil || row.State != 2 || row.PostedRev != 1 || row.CommentKey != "agent-comment" {
		t.Fatalf("ForgeMirrorByID after DriveAgent = (%+v, %v), want delivered row", row, err)
	}
}
