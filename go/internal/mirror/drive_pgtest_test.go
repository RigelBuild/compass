//go:build pgtest

package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
)

type postResult struct {
	comment forge.Comment
	err     error
	lose    bool
}

type recordedPost struct {
	session  string
	need     store.ForgeMirrorMode
	key      string
	clientID string
	body     string
}

type fakePoster struct {
	mu          sync.Mutex
	release     sync.Once
	posts       []recordedPost
	comments    []forge.Comment
	deletedKeys []string
	results     []postResult
	listErr     error
	nextID      uint64
	entered     chan struct{}
	proceed     chan struct{}
	onBodyLimit func()
	onPost      func(int)
}

func (p *fakePoster) BodyLimit(refs.Ref) int {
	if p.onBodyLimit != nil {
		p.onBodyLimit()
	}
	return 4096
}

func (p *fakePoster) PostMirror(_ context.Context, _ store.AccountID, session string, need store.ForgeMirrorMode, ref refs.Ref, key, clientID, body string) (forge.Comment, error) {
	p.mu.Lock()
	p.posts = append(p.posts, recordedPost{session: session, need: need, key: key, clientID: clientID, body: body})
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	var result postResult
	if len(p.results) > 0 {
		result = p.results[0]
		p.results = p.results[1:]
	}

	var existing *forge.Comment
	for i := range p.comments {
		if p.comments[i].Key == key || (key == "" && ref.Kind == refs.KindTracker && p.comments[i].Key == clientID) {
			existing = &p.comments[i]
			break
		}
	}
	if existing != nil {
		existing.Body = body
		existing.ForgeAccount = "compass-app"
		result.comment = *existing
	} else {
		if result.comment.ID == 0 {
			p.nextID++
			commentKey := strconv.FormatUint(p.nextID, 10)
			result.comment = forge.Comment{ID: p.nextID, Key: commentKey, URL: "https://forge.test/comment/" + commentKey}
		}
		if ref.Kind == refs.KindTracker {
			result.comment.Key = clientID
		}
		result.comment.ForgeAccount = "compass-app"
		if key == "" && (result.err == nil || result.lose) {
			comment := result.comment
			comment.Body = body
			p.comments = append(p.comments, comment)
		}
	}
	call := len(p.posts)
	p.mu.Unlock()

	if p.onPost != nil {
		p.onPost(call)
	}
	if p.proceed != nil {
		<-p.proceed
	}
	return result.comment, result.err
}

func (p *fakePoster) ListMirrorComments(_ context.Context, _ store.AccountID, _ refs.Ref, _ time.Time) ([]forge.Comment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	comments := make([]forge.Comment, 0, len(p.comments))
	for _, comment := range p.comments {
		if comment.ForgeAccount == "compass-app" {
			comments = append(comments, comment)
		}
	}
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	return comments, nil
}

func (p *fakePoster) DeleteMirrorComment(_ context.Context, _ store.AccountID, _ refs.Ref, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletedKeys = append(p.deletedKeys, key)
	for i := range p.comments {
		if p.comments[i].Key == key {
			p.comments = append(p.comments[:i], p.comments[i+1:]...)
			break
		}
	}
	return nil
}

func (p *fakePoster) snapshot() ([]recordedPost, []forge.Comment, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedPost(nil), p.posts...), append([]forge.Comment(nil), p.comments...), append([]string(nil), p.deletedKeys...)
}

func (p *fakePoster) unblock() {
	p.release.Do(func() { close(p.proceed) })
}

func openMirrorStore(t *testing.T) (*store.Store, context.Context, string) {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st, ctx, dsn
}

func mirrorOwner(t *testing.T, ctx context.Context, st *store.Store) store.Account {
	t.Helper()
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "mirror-owner", DisplayName: "Mirror Owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	return owner
}

func claimClickRow(t *testing.T, ctx context.Context, st *store.Store, owner store.Account, target string) store.ForgeMirrorRow {
	t.Helper()
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Mode: store.ForgeMirrorManual}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	row, created, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: target, Actor: owner.ID, TargetRef: target, BodyRev: 1, State: 1})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: created=%v err=%v", created, err)
	}
	return row
}

func mirrorAsk(t *testing.T, ctx context.Context, st *store.Store) (store.Account, store.ForgeMirrorRow) {
	t.Helper()
	owner := mirrorOwner(t, ctx, st)
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mirror-agent", DisplayName: "Mirror Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Kind: store.ChannelKindChannel, Name: "mirror-asks", MemberAccountIDs: []store.AccountID{owner.ID, agent.ID}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo", Number: 17}
	if err := st.SetForgeMirrorSetting(ctx, owner.ID, store.ForgeMirrorSetting{Provider: coord.Provider, Host: coord.Host, Repo: coord.Repo, Mode: store.ForgeMirrorAuto}); err != nil {
		t.Fatalf("SetForgeMirrorSetting: %v", err)
	}
	ask := &store.Ask{Ref: "owner/repo#17", RefCoord: &coord, Questions: []store.AskQuestion{{QuestionID: "q1", Question: "Ship this?", Options: []store.AskOption{{ID: "yes", Label: "Yes"}}}}}
	msg, _, err := st.AppendMessage(ctx, store.Message{AuthorAccountID: agent.ID, Blocks: []store.MessageBlock{{Ask: ask}}}, string(channel.ID), store.TopicRef{Name: "decisions", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}
	rows, err := st.AskForgeMirrors(ctx, []string{msg.Blocks[0].Ask.AskID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("AskForgeMirrors: rows=%d err=%v", len(rows), err)
	}
	return owner, rows[0]
}

func clickBody(context.Context, store.ForgeMirrorRow, int) (string, error) {
	return "manual body", nil
}

func openMirrorPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertMirrorState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, state int16) store.ForgeMirrorRow {
	t.Helper()
	var actualState, bodyRev, postedRev int16
	var commentID pgtype.Int8
	var commentKey, commentURL, lastError string
	var recheckAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT state, body_rev, posted_rev, comment_id, comment_key, comment_url, last_error, recheck_at FROM forge_mirrors WHERE id = $1`, id).Scan(&actualState, &bodyRev, &postedRev, &commentID, &commentKey, &commentURL, &lastError, &recheckAt); err != nil {
		t.Fatalf("read mirror state: %v", err)
	}
	if actualState != state {
		t.Fatalf("mirror state = %d, want %d", actualState, state)
	}
	var recheck *time.Time
	if recheckAt.Valid {
		recheck = &recheckAt.Time
	}
	row := store.ForgeMirrorRow{ID: id, State: actualState, BodyRev: bodyRev, PostedRev: postedRev, CommentKey: commentKey, CommentURL: commentURL, LastError: lastError, RecheckAt: recheck}
	if commentID.Valid {
		row.CommentID = uint64(commentID.Int64)
	}
	return row
}

func immediateSleep(t *testing.T) {
	t.Helper()
	old := sleep
	sleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { sleep = old })
}

func TestDriveSourceTwoPostsManualAndStoresResult(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "RIG-12")
	poster := &fakePoster{results: []postResult{{comment: forge.Comment{ID: 7, Key: "created-key", URL: "https://linear.app/comment/7"}}}}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)

	posts, comments, _ := poster.snapshot()
	if len(posts) != 1 || posts[0].need != store.ForgeMirrorManual || posts[0].key != "" || posts[0].clientID != row.ClientCommentID {
		t.Fatalf("posts = %+v, want one manual create with row client id", posts)
	}
	if len(comments) != 1 || !HasMarker(comments[0].Body, row.ID) {
		t.Fatalf("comments = %+v, want one marked comment", comments)
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.CommentKey != row.ClientCommentID || stored.CommentURL != "https://linear.app/comment/7" || stored.RecheckAt != nil {
		t.Fatalf("stored row = %+v, want client comment ID, URL, and no recheck", stored)
	}
}

// The driver hands the poster the row's stored session, which the agent owner
// header names.
func TestDrivePassesRowSessionToPoster(t *testing.T) {
	st, ctx, _ := openMirrorStore(t)
	owner := mirrorOwner(t, ctx, st)
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mirror-session-agent", DisplayName: "Mirror Session Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	row, created, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{
		Source: 2, SourceKey: "session-topic", Actor: agent.ID, Agent: agent.ID, TargetRef: "RIG-14", BodyRev: 1, State: 1, SessionID: "sess-drive",
	})
	if err != nil || !created {
		t.Fatalf("ClaimForgeMirror: created=%v err=%v", created, err)
	}
	poster := &fakePoster{}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 || posts[0].session != "sess-drive" || posts[0].need != store.ForgeMirrorAuto {
		t.Fatalf("posts = %+v, want one auto post carrying the row session", posts)
	}
}

func TestDriveRetriesThreeFailures(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "RIG-12")
	poster := &fakePoster{results: []postResult{{err: errors.New("first")}, {err: errors.New("second")}, {err: errors.New("third")}}}
	immediateSleep(t)
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, _, _ := poster.snapshot()
	if len(posts) != 3 {
		t.Fatalf("post calls = %d, want 3", len(posts))
	}
	if failed := assertMirrorState(t, ctx, pool, row.ID, 3); failed.LastError != "third" {
		t.Fatalf("last error = %q, want third", failed.LastError)
	}
}

func TestDriveRefusalMarksFailedWithoutRetry(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "RIG-12")
	poster := &fakePoster{results: []postResult{{err: fmt.Errorf("policy: %w", ErrRefused)}}}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 {
		t.Fatalf("post calls = %d, want 1", len(posts))
	}
	if failed := assertMirrorState(t, ctx, pool, row.ID, 3); failed.LastError == "" {
		t.Fatal("refused row has no failure reason")
	}
}

func TestDriveAdoptsLostGitHubPostWithManyLaterComments(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#7")
	poster := &fakePoster{results: []postResult{
		{comment: forge.Comment{ID: 1, Key: "first-comment", ForgeAccount: "compass-app"}, err: errors.New("response lost"), lose: true},
		{comment: forge.Comment{ID: 1, Key: "first-comment", URL: "https://github.test/comment/1", ForgeAccount: "compass-app"}},
	}}
	for i := range 60 {
		poster.comments = append(poster.comments, forge.Comment{ID: uint64(i + 2), Key: fmt.Sprintf("later-%d", i), Body: "later comment"})
	}
	immediateSleep(t)
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, comments, _ := poster.snapshot()
	if len(posts) != 2 || posts[0].key != "" || posts[1].key != "first-comment" {
		t.Fatalf("posts = %+v, want lost create then edit of adopted comment", posts)
	}
	marked := 0
	for _, comment := range comments {
		if HasMarker(comment.Body, row.ID) {
			marked++
		}
	}
	if len(comments) != 61 || marked != 1 {
		t.Fatalf("fake forge comments = %d (%d marked), want 61 comments and one marked", len(comments), marked)
	}
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.CommentKey != "first-comment" || stored.RecheckAt == nil {
		t.Fatalf("stored GitHub row = %+v, want adopted key and recheck", stored)
	}
}

func TestDriveRecoveryScanErrorDoesNotCreateDuplicate(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#8")
	_, ok, err := st.LeaseForgeMirror(ctx, row.ID, "prior-token", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	attempt, ok, err := st.BeginForgeMirrorAttempt(ctx, row.ID, "prior-token")
	if err != nil || !ok {
		t.Fatalf("BeginForgeMirrorAttempt: ok=%v err=%v", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "prior-token", store.ForgeMirrorMark{PostedRev: 0, State: 1}); err != nil {
		t.Fatalf("MarkForgeMirror(pending): %v", err)
	}
	leased, ok, err := st.LeaseForgeMirror(ctx, row.ID, "recovery-token", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror(recovery): ok=%v err=%v", ok, err)
	}
	if attempt.FirstAttemptAt == nil || leased.FirstAttemptAt == nil || leased.LastAttemptAt == nil {
		t.Fatalf("attempt timestamps missing: attempt=%+v leased=%+v", attempt, leased)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire mirror lease: %v", err)
	}
	immediateSleep(t)
	poster := &fakePoster{listErr: errors.New("forge unavailable")}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{leased}, nil, clickBody)
	posts, _, _ := poster.snapshot()
	if len(posts) != 0 {
		t.Fatalf("post calls = %d, want no create while recovery scan fails", len(posts))
	}
	if got := assertMirrorState(t, ctx, pool, row.ID, 3); got.LastError == "" {
		t.Fatal("failed recovery scan did not persist its error")
	}
}

func TestDriveLinearRetryReusesClientCommentID(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "RIG-12")
	poster := &fakePoster{results: []postResult{{err: errors.New("lost response"), lose: true}, {comment: forge.Comment{Key: "linear-comment", URL: "https://linear.app/comment/1"}}}}
	immediateSleep(t)
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, comments, _ := poster.snapshot()
	if len(posts) != 2 || posts[0].clientID != row.ClientCommentID || posts[1].clientID != row.ClientCommentID {
		t.Fatalf("post client IDs = %+v, want same row UUID on both tries", posts)
	}
	if len(comments) != 1 {
		t.Fatalf("fake Linear comments = %d, want one idempotent comment", len(comments))
	}
	if got := assertMirrorState(t, ctx, pool, row.ID, 2); got.CommentKey != row.ClientCommentID {
		t.Fatalf("stored row = %+v, want Linear client ID", got)
	}
}

func TestDriveEditsSameCommentAfterAnswer(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner, row := mirrorAsk(t, ctx, st)
	poster := &fakePoster{}
	link := func(channelID, topicID string) Link {
		return Link{Channel: "mirror", Topic: topicID, URL: "https://compass.test/channel/" + channelID + "/topic/" + topicID}
	}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, link, nil)
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 || posts[0].key != "" || posts[0].need != store.ForgeMirrorAuto {
		t.Fatalf("initial posts = %+v, want one automatic ask post", posts)
	}
	if !strings.Contains(posts[0].body, "#mirror-asks › decisions") || strings.Contains(posts[0].body, string(row.Agent)) {
		t.Fatalf("initial body header = %q, want stored channel and topic names", posts[0].body)
	}
	if !strings.Contains(posts[0].body, "Ship this?") || strings.Contains(posts[0].body, "Answers:") {
		t.Fatalf("initial body = %q, want pending ask only", posts[0].body)
	}
	if _, _, err := st.AnswerAsk(ctx, owner.ID, row.AskID, []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"yes"}}}); err != nil {
		t.Fatalf("AnswerAsk: %v", err)
	}
	rows, err := st.AskForgeMirrors(ctx, []string{row.AskID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("AskForgeMirrors after answer: rows=%+v err=%v", rows, err)
	}
	Drive(ctx, st, poster, rows, link, nil)
	posts, comments, _ := poster.snapshot()
	if len(posts) != 2 || posts[0].key != "" || posts[1].key == "" {
		t.Fatalf("posts = %+v, want create then edit of same comment", posts)
	}
	if !strings.Contains(posts[1].body, "Answers:") || !strings.Contains(posts[1].body, "Answered by @mirror-owner") || len(comments) != 1 {
		t.Fatalf("posts=%+v comments=%+v, want answered content on the original comment", posts, comments)
	}
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.CommentKey == "" || stored.PostedRev != 2 || stored.BodyRev != 2 {
		t.Fatalf("stored row = %+v, want updated comment and revision 2", stored)
	}
}

func TestDriveConcurrentLeasesAllowOnePost(t *testing.T) {
	st, ctx, _ := openMirrorStore(t)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "RIG-12")
	poster := &fakePoster{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	defer poster.unblock()
	done := make(chan struct{})
	go func() {
		Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
		close(done)
	}()
	select {
	case <-poster.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first Drive did not reach poster")
	}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	poster.unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("first Drive did not finish")
	}
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 {
		t.Fatalf("post calls = %d, want one lease winner", len(posts))
	}
}

func TestDriveExpiredLeaseAdoptsMarker(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#9")
	_, ok, err := st.LeaseForgeMirror(ctx, row.ID, "expired-token", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.BeginForgeMirrorAttempt(ctx, row.ID, "expired-token"); err != nil || !ok {
		t.Fatalf("BeginForgeMirrorAttempt: ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second', first_attempt_at = now() - interval '3 minutes', last_attempt_at = now() - interval '3 minutes' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire mirror lease: %v", err)
	}
	poster := &fakePoster{comments: []forge.Comment{{ID: 9, Key: "adopt-me", ForgeAccount: "compass-app", Body: WithMarker("old body", row.ID)}}}
	immediateSleep(t)
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 || posts[0].key != "adopt-me" {
		t.Fatalf("posts = %+v, want edit adopted after lease expiry", posts)
	}
	if got := assertMirrorState(t, ctx, pool, row.ID, 2); got.CommentKey != "adopt-me" {
		t.Fatalf("stored row = %+v, want adopted key", got)
	}
}

func TestDriveStaleTokenMarkWritesNothing(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "RIG-12")
	leased, ok, err := st.LeaseForgeMirror(ctx, row.ID, "current-token", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.BeginForgeMirrorAttempt(ctx, row.ID, "current-token"); err != nil || !ok {
		t.Fatalf("BeginForgeMirrorAttempt: ok=%v err=%v", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "stale-token", store.ForgeMirrorMark{PostedRev: 1, State: 2, CommentKey: "wrong"}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("MarkForgeMirror(stale) = %v, want ErrLeaseLost", err)
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 1)
	var token string
	if err := pool.QueryRow(ctx, `SELECT lease_token FROM forge_mirrors WHERE id = $1`, row.ID).Scan(&token); err != nil {
		t.Fatalf("read lease token: %v", err)
	}
	if stored.CommentKey != "" || stored.PostedRev != 0 || token != leased.LeaseToken {
		t.Fatalf("stale mark changed row: row=%+v token=%q want lease=%q", stored, token, leased.LeaseToken)
	}
}

func TestDriveRecheckDeletesNewerDuplicateAndKeepsOldest(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#10")
	leased, ok, err := st.LeaseForgeMirror(ctx, row.ID, "recheck-token", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "recheck-token", store.ForgeMirrorMark{PostedRev: 1, State: 2, CommentID: 10, CommentKey: "newer", URL: "new-url", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror(recheck): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire recheck: %v", err)
	}
	leased.RecheckAt = new(time.Time)
	*leased.RecheckAt = time.Now().Add(-time.Second)
	poster := &fakePoster{comments: []forge.Comment{
		{ID: 8, Key: "oldest", ForgeAccount: "compass-app", URL: "old-url", Body: WithMarker("old", row.ID)},
		{ID: 10, Key: "newer", ForgeAccount: "compass-app", URL: "new-url", Body: WithMarker("new", row.ID)},
	}}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{leased}, nil, clickBody)
	_, comments, deleted := poster.snapshot()
	if len(deleted) != 1 || deleted[0] != "newer" || len(comments) != 1 || comments[0].Key != "oldest" || comments[0].Body != WithMarker("manual body", row.ID) {
		t.Fatalf("deleted=%v comments=%+v, want delete newer and edit retained oldest comment", deleted, comments)
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.PostedRev != 1 || stored.CommentKey != "oldest" || stored.CommentURL != "old-url" || stored.RecheckAt != nil {
		t.Fatalf("repointed row = %+v, want oldest comment pointer and original posted revision", stored)
	}
}

func TestDriveCertainGitHubPostDoesNotScheduleRecheck(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#11")
	poster := &fakePoster{}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 {
		t.Fatalf("post calls = %d, want 1", len(posts))
	}
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.RecheckAt != nil {
		t.Fatalf("certain GitHub post scheduled recheck: %+v", stored)
	}
}

func TestDriveForeignMarkerCommentIsNotAdoptedOrDeleted(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#12")
	_, ok, err := st.LeaseForgeMirror(ctx, row.ID, "foreign-token", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.BeginForgeMirrorAttempt(ctx, row.ID, "foreign-token"); err != nil || !ok {
		t.Fatalf("BeginForgeMirrorAttempt: ok=%v err=%v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_mirrors SET lease_until = now() - interval '1 second', first_attempt_at = now() - interval '3 minutes', last_attempt_at = now() - interval '3 minutes' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire mirror lease: %v", err)
	}
	foreign := forge.Comment{ID: 4, Key: "foreign-marker", ForgeAccount: "person", Body: WithMarker("copied", row.ID)}
	poster := &fakePoster{comments: []forge.Comment{foreign}}
	immediateSleep(t)
	Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
	posts, comments, deleted := poster.snapshot()
	if len(posts) != 1 || len(deleted) != 0 || len(comments) != 2 {
		t.Fatalf("posts=%+v deleted=%v comments=%+v, want foreign comment preserved and a new owned comment", posts, deleted, comments)
	}
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.CommentKey == foreign.Key {
		t.Fatalf("adopted foreign comment: %+v", stored)
	}
}

func TestDriveRevisionBumpBetweenRenderAndAttemptRemainsPending(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#14")
	leased, ok, err := st.LeaseForgeMirror(ctx, row.ID, "render-race", time.Minute)
	if err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	ready := make(chan struct{})
	continueRender := make(chan struct{})
	bodyFn := func(ctx context.Context, row store.ForgeMirrorRow, _ int) (string, error) {
		body := fmt.Sprintf("body rendered at revision %d", row.BodyRev)
		close(ready)
		select {
		case <-continueRender:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return body, nil
	}
	poster := &fakePoster{}
	done := make(chan error, 1)
	go func() {
		done <- processLeasedRow(ctx, st, poster, leased, "render-race", nil, bodyFn, slog.Default())
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		close(continueRender)
		t.Fatal("Drive did not finish rendering the body")
	}
	if _, _, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: row.SourceKey, Actor: owner.ID, TargetRef: row.TargetRef, BodyRev: 1, State: 1}); err != nil {
		close(continueRender)
		t.Fatalf("ClaimForgeMirror(revision bump): %v", err)
	}
	close(continueRender)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("processLeasedRow: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Drive did not finish after render resumed")
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 1)
	if stored.BodyRev != 2 || stored.PostedRev != 1 {
		t.Fatalf("mirror revisions = (%d, %d), want (2, 1) pending", stored.BodyRev, stored.PostedRev)
	}
	posts, _, _ := poster.snapshot()
	if len(posts) != 1 || !strings.Contains(posts[0].body, "body rendered at revision 1") {
		t.Fatalf("posts = %+v, want the original revision-1 body", posts)
	}
}

func TestDriveOneRedrivesAskAnsweredDuringPost(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner, row := mirrorAsk(t, ctx, st)
	poster := &fakePoster{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	done := make(chan struct{})
	var final store.ForgeMirrorRow
	go func() {
		final, _ = DriveOne(ctx, st, poster, row, nil, nil)
		close(done)
	}()
	select {
	case <-poster.entered:
	case <-time.After(10 * time.Second):
		poster.unblock()
		t.Fatal("DriveOne did not reach the initial post")
	}
	if _, _, err := st.AnswerAsk(ctx, owner.ID, row.AskID, []store.AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"yes"}}}); err != nil {
		poster.unblock()
		t.Fatalf("AnswerAsk during post: %v", err)
	}
	poster.unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DriveOne did not finish after post release")
	}
	if final.State != 2 || final.BodyRev != 2 || final.PostedRev != 2 {
		t.Fatalf("final row = %+v, want revision 2 delivered", final)
	}
	posts, comments, _ := poster.snapshot()
	if len(posts) != 2 || !strings.Contains(posts[1].body, "Answers:") || len(comments) != 1 {
		t.Fatalf("posts=%+v comments=%+v, want answer edit of the same comment", posts, comments)
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.BodyRev != 2 || stored.PostedRev != 2 {
		t.Fatalf("stored revisions = (%d, %d), want (2, 2)", stored.BodyRev, stored.PostedRev)
	}
}

func TestDriveRecheckRepointRedeliversCurrentBodyAndDeletesDuplicate(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#13")
	if _, ok, err := st.LeaseForgeMirror(ctx, row.ID, "repoint-token", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror: ok=%v err=%v", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "repoint-token", store.ForgeMirrorMark{PostedRev: 2, State: 2, CommentID: 11, CommentKey: "newer", URL: "new-url", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror(recheck): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second', body_rev = 2 WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("expire recheck: %v", err)
	}
	leased, err := st.ForgeMirrorByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("ForgeMirrorByID(due recheck): %v", err)
	}
	poster := &fakePoster{comments: []forge.Comment{
		{ID: 9, Key: "oldest", ForgeAccount: "compass-app", URL: "old-url", Body: WithMarker("old body", row.ID)},
		{ID: 11, Key: "newer", ForgeAccount: "compass-app", URL: "new-url", Body: WithMarker("new body", row.ID)},
	}}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{leased}, nil, clickBody)
	posts, comments, deleted := poster.snapshot()
	if len(posts) != 1 || posts[0].key != "oldest" || len(deleted) != 1 || deleted[0] != "newer" || len(comments) != 1 || comments[0].Body != WithMarker("manual body", row.ID) {
		t.Fatalf("posts=%+v deleted=%v comments=%+v, want oldest current body and duplicate removal", posts, deleted, comments)
	}
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.CommentKey != "oldest" || stored.PostedRev != 2 || stored.RecheckAt != nil {
		t.Fatalf("recheck delivery = %+v, want oldest current body and cleared recheck", stored)
	}
}

func TestDriveRedrivesAfterRevisionBumpDuringPost(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#14")
	poster := &fakePoster{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		Drive(ctx, st, poster, []store.ForgeMirrorRow{row}, nil, clickBody)
		close(done)
	}()
	select {
	case <-poster.entered:
	case <-time.After(10 * time.Second):
		poster.unblock()
		t.Fatal("Drive did not reach the poster")
	}
	if _, _, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: row.SourceKey, Actor: owner.ID, TargetRef: row.TargetRef, BodyRev: 1, State: 1}); err != nil {
		poster.unblock()
		t.Fatalf("ClaimForgeMirror(revision bump): %v", err)
	}
	poster.unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Drive did not finish after poster release")
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.BodyRev != 2 || stored.PostedRev != 2 {
		t.Fatalf("mirror revisions = (%d, %d), want (2, 2) after same-call redrive", stored.BodyRev, stored.PostedRev)
	}
	posts, _, _ := poster.snapshot()
	if len(posts) != 2 {
		t.Fatalf("post calls = %d, want initial delivery and redrive", len(posts))
	}
}

func TestDriveOneRedrivesAfterFailedPassWithRevisionBump(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#20")
	poster := &fakePoster{results: []postResult{{err: errors.New("first")}, {err: errors.New("second")}, {err: errors.New("third")}, {comment: forge.Comment{ID: 20, Key: "recovered", URL: "https://forge.test/comment/20"}}}}
	poster.onPost = func(call int) {
		if call != 1 {
			return
		}
		if _, _, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: row.SourceKey, Actor: owner.ID, TargetRef: row.TargetRef, BodyRev: 1, State: 1}); err != nil {
			t.Errorf("ClaimForgeMirror(revision bump during failed post): %v", err)
		}
	}
	immediateSleep(t)
	got, err := DriveOne(ctx, st, poster, row, nil, func(_ context.Context, row store.ForgeMirrorRow, _ int) (string, error) {
		return fmt.Sprintf("body at rev %d", row.BodyRev), nil
	})
	if err != nil {
		t.Fatalf("DriveOne: %v", err)
	}
	posts, _, _ := poster.snapshot()
	if got.State != 2 || got.BodyRev != 2 || got.PostedRev != 2 || len(posts) != 4 || !strings.Contains(posts[0].body, "rev 1") || !strings.Contains(posts[3].body, "rev 2") {
		t.Fatalf("DriveOne result=%+v posts=%+v; want failed rev-1 pass followed by delivered rev-2 pass", got, posts)
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.BodyRev != 2 || stored.PostedRev != 2 {
		t.Fatalf("stored revisions = (%d, %d), want (2, 2)", stored.BodyRev, stored.PostedRev)
	}
}

func TestDriveOneRedrivesAcrossFourRevisionBumps(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#21")
	poster := &fakePoster{}
	poster.onPost = func(call int) {
		if call > 4 {
			return
		}
		if _, _, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: row.SourceKey, Actor: owner.ID, TargetRef: row.TargetRef, BodyRev: 1, State: 1}); err != nil {
			t.Errorf("ClaimForgeMirror(revision bump %d): %v", call, err)
		}
	}
	immediateSleep(t)
	got, err := DriveOne(ctx, st, poster, row, nil, func(_ context.Context, row store.ForgeMirrorRow, _ int) (string, error) {
		return fmt.Sprintf("body at rev %d", row.BodyRev), nil
	})
	if err != nil {
		t.Fatalf("DriveOne: %v", err)
	}
	posts, _, _ := poster.snapshot()
	if got.State != 2 || got.BodyRev != 5 || got.PostedRev != 5 || len(posts) != 5 {
		t.Fatalf("DriveOne result=%+v post count=%d; want revision 5 delivered after four bumps", got, len(posts))
	}
	for i, post := range posts {
		if !strings.Contains(post.body, fmt.Sprintf("rev %d", i+1)) {
			t.Errorf("post %d body = %q, want revision %d", i+1, post.body, i+1)
		}
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.BodyRev != 5 || stored.PostedRev != 5 {
		t.Fatalf("stored revisions = (%d, %d), want (5, 5)", stored.BodyRev, stored.PostedRev)
	}
}

func TestDriveOneRedrivesAfterRefusalRacesRevisionBump(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#22")
	poster := &fakePoster{results: []postResult{{err: ErrRefused}}}
	poster.onPost = func(call int) {
		if call != 1 {
			return
		}
		if _, _, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: row.SourceKey, Actor: owner.ID, TargetRef: row.TargetRef, BodyRev: 1, State: 1}); err != nil {
			t.Errorf("ClaimForgeMirror(revision bump during refusal): %v", err)
		}
	}
	immediateSleep(t)
	got, err := DriveOne(ctx, st, poster, row, nil, func(_ context.Context, row store.ForgeMirrorRow, _ int) (string, error) {
		return fmt.Sprintf("body at rev %d", row.BodyRev), nil
	})
	if err != nil || got.BodyRev != 2 || got.PostedRev != 2 || got.State != 2 {
		t.Fatalf("DriveOne result=(%+v, %v), want bumped revision 2 delivered after refusing rev 1", got, err)
	}
	posts, _, _ := poster.snapshot()
	if len(posts) != 2 || !strings.Contains(posts[0].body, "rev 1") || !strings.Contains(posts[1].body, "rev 2") {
		t.Fatalf("posts = %+v, want one refused rev-1 attempt and one rev-2 delivery", posts)
	}
	stored := assertMirrorState(t, ctx, pool, row.ID, 2)
	if stored.BodyRev != 2 || stored.PostedRev != 2 {
		t.Fatalf("stored revisions = (%d, %d), want (2, 2)", stored.BodyRev, stored.PostedRev)
	}
}

func TestDrivePreservesScheduledRecheckAcrossLaterEdit(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#15")
	if _, ok, err := st.LeaseForgeMirror(ctx, row.ID, "schedule-recheck", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror = (ok=%v, err=%v)", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "schedule-recheck", store.ForgeMirrorMark{PostedRev: 1, State: 2, CommentID: 15, CommentKey: "comment", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror(schedule): %v", err)
	}
	updated, created, err := st.ClaimForgeMirror(ctx, store.ForgeMirrorRow{Source: 2, SourceKey: row.SourceKey, Actor: owner.ID, TargetRef: row.TargetRef, BodyRev: 1, State: 1})
	if err != nil || created || updated.BodyRev != 2 || updated.State != 1 {
		t.Fatalf("ClaimForgeMirror(revision bump) = (%+v, %v, %v), want existing pending rev 2", updated, created, err)
	}
	poster := &fakePoster{comments: []forge.Comment{{ID: 15, Key: "comment", ForgeAccount: "compass-app", Body: WithMarker("old", row.ID)}}}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{updated}, nil, clickBody)
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.RecheckAt == nil {
		t.Fatal("later edit cleared scheduled recheck")
	}
}

func TestDriveRecheckClearsRecheckAndReeditsRepointedComment(t *testing.T) {
	st, ctx, dsn := openMirrorStore(t)
	pool := openMirrorPool(t, ctx, dsn)
	owner := mirrorOwner(t, ctx, st)
	row := claimClickRow(t, ctx, st, owner, "owner/repo#16")
	if _, ok, err := st.LeaseForgeMirror(ctx, row.ID, "recheck-edit", time.Minute); err != nil || !ok {
		t.Fatalf("LeaseForgeMirror = (ok=%v, err=%v)", ok, err)
	}
	if err := st.MarkForgeMirror(ctx, row.ID, "recheck-edit", store.ForgeMirrorMark{PostedRev: 2, State: 2, CommentID: 11, CommentKey: "newer", URL: "new-url", Recheck: true}); err != nil {
		t.Fatalf("MarkForgeMirror(schedule): %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_mirrors SET recheck_at = now() - interval '1 second', body_rev = 2, posted_rev = 2 WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("prepare stale-body recheck: %v", err)
	}
	leased, err := st.ForgeMirrorByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("ForgeMirrorByID(due recheck): %v", err)
	}
	poster := &fakePoster{comments: []forge.Comment{
		{ID: 9, Key: "oldest", ForgeAccount: "compass-app", URL: "old-url", Body: WithMarker("old body", row.ID)},
		{ID: 11, Key: "newer", ForgeAccount: "compass-app", URL: "new-url", Body: WithMarker("new body", row.ID)},
	}}
	Drive(ctx, st, poster, []store.ForgeMirrorRow{leased}, nil, clickBody)
	if got := assertMirrorState(t, ctx, pool, row.ID, 2); got.PostedRev != 2 || got.CommentKey != "oldest" || got.RecheckAt != nil {
		t.Fatalf("recheck result = %+v, want oldest comment redelivered and recheck cleared", got)
	}
	posts, comments, deleted := poster.snapshot()
	if len(posts) != 1 || posts[0].key != "oldest" || len(deleted) != 1 || deleted[0] != "newer" || len(comments) != 1 || comments[0].Body != WithMarker("manual body", row.ID) {
		t.Fatalf("posts=%+v deleted=%v comments=%+v, want oldest current body and duplicate removal", posts, deleted, comments)
	}
	if stored := assertMirrorState(t, ctx, pool, row.ID, 2); stored.CommentKey != "oldest" || stored.PostedRev != 2 || stored.RecheckAt != nil {
		t.Fatalf("recheck delivery = %+v, want oldest current body and cleared recheck", stored)
	}
}
