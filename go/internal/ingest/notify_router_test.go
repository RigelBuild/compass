package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// ---- fakes for both seams (design.md:873) ----

// fakeNotifyStore is the durable seam fake. It holds the coordinate's cursor,
// scripts the subscribers SubscribersForArtifact returns, records every upsert,
// and — the W3 guard — exposes an Advance counter the router has NO seam path to
// call, so a nonzero count proves the router touched delivered_revision.
type fakeNotifyStore struct {
	cursor      *ArtifactCursor
	artifactSub []NotifySubscriber // returned when opened=false
	openedSub   []NotifySubscriber // extra rows returned when opened=true
	targets     []NotifyTarget     // returned by ListNotifyTargets (the T5 sweep)
	upserts     []ArtifactCursor
	loadErr     error
	subErr      error
	upsertErr   error
	targetsErr  error

	lastOpened  bool
	lastProject string
	// advanceCalls MUST stay zero: the router never advances delivered_revision
	// (W3). No NotifyStore method advances it; this counter is the runtime guard
	// that no future edit sneaks an advance onto the router's path.
	advanceCalls int
}

func (f *fakeNotifyStore) LoadArtifactCursor(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, _ uint64) (*ArtifactCursor, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.cursor, nil
}

func (f *fakeNotifyStore) SubscribersForArtifact(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, _ uint64, project string, opened bool) ([]NotifySubscriber, error) {
	f.lastOpened = opened
	f.lastProject = project
	if f.subErr != nil {
		return nil, f.subErr
	}
	if opened {
		// Container-scope: only the project-matched container subs (the store's
		// SubscribersForArtifact does the project match; the fake mirrors it).
		var out []NotifySubscriber
		for _, s := range f.openedSub {
			if s.Project == project {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return f.artifactSub, nil
}

func (f *fakeNotifyStore) ListNotifyTargets(_ context.Context) ([]NotifyTarget, error) {
	if f.targetsErr != nil {
		return nil, f.targetsErr
	}
	return f.targets, nil
}

func (f *fakeNotifyStore) UpsertArtifactCursor(_ context.Context, cur ArtifactCursor) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, cur)
	f.cursor = &cur
	return nil
}

// fakeDispatcher records every notification per account.
type fakeDispatcher struct {
	sent    []*compassv1internal.ForgeNotification
	toFail  map[string]error // account -> error to return
	sawFail int
}

func (d *fakeDispatcher) Notify(_ context.Context, account string, n *compassv1internal.ForgeNotification) error {
	if err, ok := d.toFail[account]; ok {
		d.sawFail++
		return err
	}
	d.sent = append(d.sent, n)
	return nil
}

// fakeChecksRoller scripts the combined roll-up result (the real
// forge.ConditionalResult[forge.Checks] the collapsed seam returns).
type fakeChecksRoller struct {
	res      forge.ConditionalResult[forge.Checks]
	err      error
	calls    int
	lastETag string
	lastHead string
	lastRepo string
	lastNum  uint64
}

func (c *fakeChecksRoller) RollUp(_ context.Context, repo string, number uint64, headSHA, etag string) (forge.ConditionalResult[forge.Checks], error) {
	c.calls++
	c.lastRepo, c.lastNum, c.lastHead, c.lastETag = repo, number, headSHA, etag
	return c.res, c.err
}

// fakePullNumbers scripts the head_sha->PR-number seam (RIG-2869) and counts
// every call, so a test can assert BOTH the resolved coordinate and that a
// non-CHECKS / nil-resolver path never consults it.
type fakePullNumbers struct {
	number   uint64
	err      error
	calls    int
	lastRepo string
	lastSHA  string
}

func (p *fakePullNumbers) PullNumberForSHA(_ context.Context, repo, headSHA string) (uint64, error) {
	p.calls++
	p.lastRepo, p.lastSHA = repo, headSHA
	return p.number, p.err
}

func testRef() *compassv1.ForgeRef {
	return &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"}
}

// newRouter builds a router with NO pull-number resolver — the legacy shape
// every pre-RIG-2869 case exercises (a zero-number event is rejected).
func newRouter(t *testing.T, st *fakeNotifyStore, d *fakeDispatcher, c *fakeChecksRoller) *NotifyRouter {
	t.Helper()
	return NewNotifyRouter(st, d, c, nil, nil, testRef(), nil)
}

// newRouterWithPulls builds a router with the head_sha->number resolution seam
// wired (the RIG-2869 shape the prod GitHub lane uses).
func newRouterWithPulls(t *testing.T, st *fakeNotifyStore, d *fakeDispatcher, c *fakeChecksRoller, p PullNumberResolver) *NotifyRouter {
	t.Helper()
	return NewNotifyRouter(st, d, c, p, nil, testRef(), nil)
}

const (
	kindIssue = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE
	kindPR    = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	chComment = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT
	chState   = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE
	chChecks  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS
	chOpened  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED
	chUpdate  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE
	chReview  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW

	scopeContainer = compassv1internal.ForgeSubscriptionScope_FORGE_SUBSCRIPTION_SCOPE_CONTAINER

	// selfAgent is the agent handle every suppression fixture uses; the owner
	// leg is what the cases vary.
	selfAgent = "atlas"
)

func ghComment(url, body, account string) *compassv1internal.CommentRef {
	return &compassv1internal.CommentRef{Url: url, CommentKey: url, Body: body, ForgeAccount: account}
}

// commentEvent is one GitHub issue COMMENT event on o/r#7.
func commentEvent(url string) forge.ForgeEvent {
	return forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     "github.com", Repo: "o/r", Kind: kindIssue, Number: 7,
		URL: url, Change: chComment, Comment: ghComment(url, "hi", "octocat"),
	}
}

// fakeIdentityResolver scripts the two identity-seam reads: an account-id ->
// Handle map for HandleForAccount, and a single author Handle for AuthorHandle
// (the OPENED coordinate). Either read can be forced to fault. authorMiss makes
// AuthorHandle return a zero Handle with a nil error (the ErrNotFound / clean
// miss the router treats as fail-open).
type fakeIdentityResolver struct {
	accounts    map[string]Handle
	author      Handle
	authorMiss  bool
	accountErr  error
	authorErr   error
	authorCalls int
}

func (f *fakeIdentityResolver) HandleForAccount(_ context.Context, accountID string) (Handle, error) {
	if f.accountErr != nil {
		return Handle{}, f.accountErr
	}
	return f.accounts[accountID], nil
}

func (f *fakeIdentityResolver) AuthorHandle(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, _ uint64) (Handle, error) {
	f.authorCalls++
	if f.authorErr != nil {
		return Handle{}, f.authorErr
	}
	if f.authorMiss {
		return Handle{}, nil
	}
	return f.author, nil
}

// newRouterWithIDs builds a router with the identity seam wired — the shape the
// self-origin suppression cases exercise.
func newRouterWithIDs(t *testing.T, st *fakeNotifyStore, d *fakeDispatcher, ids IdentityResolver) *NotifyRouter {
	t.Helper()
	return NewNotifyRouter(st, d, &fakeChecksRoller{}, nil, ids, testRef(), nil)
}

// compassComment is a COMMENT event whose commenter is a Compass agent, carrying
// the owner-qualified attribution the header parse stamps.
func compassComment(owner string) forge.ForgeEvent {
	ev := commentEvent("https://gh/o/r/issues/7#c1")
	ev.Comment.Agent = &compassv1.AgentAttribution{AgentHandle: selfAgent, OwnerHandle: owner}
	return ev
}

// ---- tests ----

// TestRouteEachKindRoutesAndNotifies: each kind reaches the exact-coordinate
// subscriber with a notification, and delivered_revision is NEVER advanced.
func TestRouteEachKindRoutesAndNotifies(t *testing.T) {
	kinds := []struct {
		name string
		ev   forge.ForgeEvent
	}{
		{"comment", commentEvent("https://gh/o/r/issues/7#c1")},
		{"state", forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "o/r", Kind: kindPR, Number: 7, URL: "u", Change: chState, State: "merged"}},
		{"update", forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "o/r", Kind: kindIssue, Number: 7, URL: "u", Change: chUpdate}},
		{"review", forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "o/r", Kind: kindPR, Number: 7, URL: "u", Change: chReview, State: "approved", Comment: ghComment("u", "lgtm", "rev")}},
	}
	for _, tc := range kinds {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "sub-1", AgentAccountID: "acct-1"}}}
			d := &fakeDispatcher{}
			r := newRouter(t, st, d, &fakeChecksRoller{})

			if err := r.Route(context.Background(), tc.ev); err != nil {
				t.Fatalf("Route: %v", err)
			}
			if len(d.sent) != 1 {
				t.Fatalf("notifications = %d, want 1", len(d.sent))
			}
			n := d.sent[0]
			if n.GetSubscriptionId() != "sub-1" || n.GetChange() != tc.ev.Change || n.GetNumber() != 7 {
				t.Errorf("notification = %+v, want sub-1/%v/#7", n, tc.ev.Change)
			}
			if st.advanceCalls != 0 {
				t.Errorf("delivered_revision advanced %d times, want 0 (W3)", st.advanceCalls)
			}
			// The cursor upsert happened BEFORE notify (fetch-side truth advances
			// unconditionally): exactly one upsert with a nonempty revision.
			if len(st.upserts) != 1 || st.upserts[0].Revision == "" {
				t.Errorf("upserts = %+v, want one with a revision", st.upserts)
			}
		})
	}
}

// TestRouteCarriesRevision: the dispatched notification's revision equals
// SnapshotRevision of the applied snapshot (the ack-echo contract).
func TestRouteCarriesRevision(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}}}
	d := &fakeDispatcher{}
	ev := commentEvent("https://gh/c1")
	if err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	want := SnapshotRevision(new(ApplyEvent(nil, ev)))
	if d.sent[0].GetRevision() != want {
		t.Errorf("notification revision = %q, want %q", d.sent[0].GetRevision(), want)
	}
	if st.upserts[0].Revision != want {
		t.Errorf("cursor revision = %q, want %q", st.upserts[0].Revision, want)
	}
}

// TestRouteOpenedContainerScopeProjectMatch: an OPENED event reaches the
// matching Linear-project container subscriber and NOT a mismatched one, and
// uses opened=true container scope (never the exact-coordinate path).
func TestRouteOpenedContainerScopeProjectMatch(t *testing.T) {
	st := &fakeNotifyStore{
		openedSub: []NotifySubscriber{
			{SubscriptionID: "match", AgentAccountID: "a-match", Project: "proj-A"},
			{SubscriptionID: "miss", AgentAccountID: "a-miss", Project: "proj-B"},
		},
		// An artifact-scope sub must NOT be reached on OPENED.
		artifactSub: []NotifySubscriber{{SubscriptionID: "artifact", AgentAccountID: "a-art"}},
	}
	d := &fakeDispatcher{}
	ev := forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: "linear.app",
		Repo: "SEA", Kind: kindIssue, Number: 42, Project: "proj-A", URL: "u", Change: chOpened,
	}
	if err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !st.lastOpened {
		t.Error("SubscribersForArtifact called with opened=false, want true (OPENED -> container scope)")
	}
	if st.lastProject != "proj-A" {
		t.Errorf("project passed = %q, want proj-A", st.lastProject)
	}
	if len(d.sent) != 1 || d.sent[0].GetSubscriptionId() != "match" {
		t.Fatalf("notified %d subs %v, want only 'match'", len(d.sent), subIDs(d.sent))
	}
}

// TestRoutePerArtifactExactCoordinateOnly: a per-artifact event resolves subs
// with opened=false (exact coordinate, no fan-in).
func TestRoutePerArtifactExactCoordinateOnly(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}}}
	d := &fakeDispatcher{}
	if err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), commentEvent("https://gh/c1")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if st.lastOpened {
		t.Error("SubscribersForArtifact called with opened=true, want false (per-artifact -> exact coordinate)")
	}
}

// TestApplyCommentGrowsKeySet: COMMENT adds a comment keyed by its stable comment key.
func TestApplyCommentGrowsKeySet(t *testing.T) {
	s0 := ApplyEvent(nil, commentEvent("https://gh/c1"))
	if len(s0.Comments) != 1 {
		t.Fatalf("after first comment: %d keys, want 1", len(s0.Comments))
	}
	s1 := ApplyEvent(&s0, commentEvent("https://gh/c2"))
	if len(s1.Comments) != 2 {
		t.Errorf("after second distinct comment: %d keys, want 2", len(s1.Comments))
	}
	// prev must not be mutated (pure).
	if len(s0.Comments) != 1 {
		t.Errorf("ApplyEvent mutated prev: %d keys, want 1", len(s0.Comments))
	}
}

// TestApplyStateFlip: STATE overwrites the state half.
func TestApplyStateFlip(t *testing.T) {
	s0 := ApplyEvent(nil, forge.ForgeEvent{Repo: "o/r", Kind: kindPR, Number: 7, Change: chState, State: "open"})
	if s0.State != "open" {
		t.Fatalf("state = %q, want open", s0.State)
	}
	s1 := ApplyEvent(&s0, forge.ForgeEvent{Repo: "o/r", Kind: kindPR, Number: 7, Change: chState, State: "merged"})
	if s1.State != "merged" {
		t.Errorf("state = %q, want merged", s1.State)
	}
}

// TestApplyOpenedHighWater: OPENED bumps the container high-water number, and
// never regresses on a lower number.
func TestApplyOpenedHighWater(t *testing.T) {
	s0 := ApplyEvent(nil, forge.ForgeEvent{Repo: "SEA", Kind: kindIssue, Number: 10, Change: chOpened})
	if s0.HighWaterNumber != 10 {
		t.Fatalf("high water = %d, want 10", s0.HighWaterNumber)
	}
	s1 := ApplyEvent(&s0, forge.ForgeEvent{Repo: "SEA", Kind: kindIssue, Number: 15, Change: chOpened})
	if s1.HighWaterNumber != 15 {
		t.Errorf("high water = %d, want 15", s1.HighWaterNumber)
	}
	s2 := ApplyEvent(&s1, forge.ForgeEvent{Repo: "SEA", Kind: kindIssue, Number: 3, Change: chOpened})
	if s2.HighWaterNumber != 15 {
		t.Errorf("high water regressed to %d, want 15", s2.HighWaterNumber)
	}
}

// TestRouteDuplicateCommentUnchangedButNotified: a duplicate COMMENT (same URL)
// leaves the snapshot (revision) unchanged but STILL notifies — at-least-once,
// dedup is NOT content-based (design.md:878-880).
func TestRouteDuplicateCommentUnchangedButNotified(t *testing.T) {
	ev := commentEvent("https://gh/c1")
	// Prime the cursor with a snapshot already holding this comment.
	primed := ApplyEvent(nil, ev)
	st := &fakeNotifyStore{
		cursor:      &ArtifactCursor{Repo: "o/r", Kind: kindIssue, Number: 7, Revision: SnapshotRevision(&primed), Snapshot: mustJSON(t, &primed)},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	d := &fakeDispatcher{}
	if err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if st.upserts[0].Revision != SnapshotRevision(&primed) {
		t.Errorf("revision changed on duplicate comment: %q, want unchanged %q", st.upserts[0].Revision, SnapshotRevision(&primed))
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (still notified despite unchanged snapshot)", len(d.sent))
	}
}

// TestRouteChecksRollUpCombinedTruth: a CHECKS event resolves the roll-up via
// ChecksRoller (passing the cursor's checks_etag) BEFORE apply; the snapshot's
// checks half holds the COMBINED truth, and the notification carries it.
func TestRouteChecksRollUpCombinedTruth(t *testing.T) {
	st := &fakeNotifyStore{
		cursor:      &ArtifactCursor{Repo: "o/r", Kind: kindPR, Number: 7, ChecksETag: `"prev-etag"`},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	roller := &fakeChecksRoller{res: forge.ConditionalResult[forge.Checks]{
		ETag: `"new-etag"`,
		V: forge.Checks{HeadSHA: "sha1", State: "failure", Checks: []forge.Check{
			{Name: "build", State: "success"},
			{Name: "test", State: "failure"},
		}},
	}}
	d := &fakeDispatcher{}
	ev := forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "o/r", Kind: kindPR, Number: 7, URL: "u", Change: chChecks, HeadSHA: "sha1"}
	if err := newRouter(t, st, d, roller).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if roller.calls != 1 || roller.lastETag != `"prev-etag"` {
		t.Errorf("roller calls=%d lastETag=%q, want 1 call with the cursor's prev etag", roller.calls, roller.lastETag)
	}
	// Snapshot's checks half holds the COMBINED roll-up (state "failure",
	// BOTH checks), never one suite's conclusion.
	last := st.upserts[0]
	var snap ArtifactSnapshot
	mustUnJSON(t, last.Snapshot, &snap)
	if snap.Checks == nil || snap.Checks.State != "failure" || len(snap.Checks.Checks) != 2 {
		t.Fatalf("snapshot checks = %+v, want combined failure over 2 checks", snap.Checks)
	}
	// New etag stored for the next conditional GET.
	if last.ChecksETag != `"new-etag"` {
		t.Errorf("stored checks etag = %q, want new-etag", last.ChecksETag)
	}
	// The notification carries the combined summary.
	if d.sent[0].GetChecks().GetState() != "failure" || len(d.sent[0].GetChecks().GetChecks()) != 2 {
		t.Errorf("notification checks = %+v, want combined truth", d.sent[0].GetChecks())
	}
}

// TestRouteChecksNotModifiedCarriesPrior: a 304 from the roller carries the
// prior stored combined checks forward (snapshot + notification), without a
// fresh roll-up overwriting truth.
func TestRouteChecksNotModifiedCarriesPrior(t *testing.T) {
	prior := ArtifactSnapshot{Checks: &ChecksSnapshot{HeadSHA: "sha1", State: "success", Checks: []CheckSnapshot{{Name: "build", State: "success"}}}}
	st := &fakeNotifyStore{
		cursor:      &ArtifactCursor{Repo: "o/r", Kind: kindPR, Number: 7, ChecksETag: `"e"`, Snapshot: mustJSON(t, &prior)},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	roller := &fakeChecksRoller{res: forge.ConditionalResult[forge.Checks]{NotModified: true}}
	d := &fakeDispatcher{}
	ev := forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "o/r", Kind: kindPR, Number: 7, URL: "u", Change: chChecks, HeadSHA: "sha1"}
	if err := newRouter(t, st, d, roller).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.sent[0].GetChecks().GetState() != "success" {
		t.Errorf("notification checks state = %q, want prior success carried forward", d.sent[0].GetChecks().GetState())
	}
	var snap ArtifactSnapshot
	mustUnJSON(t, st.upserts[0].Snapshot, &snap)
	if snap.Checks == nil || snap.Checks.State != "success" {
		t.Errorf("snapshot checks = %+v, want prior combined truth preserved", snap.Checks)
	}
}

// TestRouteVanishedSubscriptionLoggedNoCrash: a dispatch error (subscription
// vanished mid-flight) is logged and skipped — Route returns nil.
func TestRouteVanishedSubscriptionLoggedNoCrash(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{
		{SubscriptionID: "gone", AgentAccountID: "a-gone"},
		{SubscriptionID: "live", AgentAccountID: "a-live"},
	}}
	d := &fakeDispatcher{toFail: map[string]error{"a-gone": errors.New("no live session")}}
	if err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), commentEvent("https://gh/c1")); err != nil {
		t.Fatalf("Route returned error on a vanished subscription, want nil: %v", err)
	}
	if d.sawFail != 1 {
		t.Errorf("dispatch failures = %d, want 1", d.sawFail)
	}
	if len(d.sent) != 1 || d.sent[0].GetSubscriptionId() != "live" {
		t.Errorf("delivered %v, want the live sub still notified", subIDs(d.sent))
	}
	if st.advanceCalls != 0 {
		t.Errorf("delivered_revision advanced %d, want 0 even on dispatch failure (W3)", st.advanceCalls)
	}
}

// TestRouteLoadCursorErrorAborts: a LoadArtifactCursor error aborts the route
// before any upsert or dispatch — the wrapped error surfaces and no fetch-side
// truth advances.
func TestRouteLoadCursorErrorAborts(t *testing.T) {
	loadErr := errors.New("load boom")
	st := &fakeNotifyStore{
		loadErr:     loadErr,
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	d := &fakeDispatcher{}
	err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), commentEvent("https://gh/c1"))
	if !errors.Is(err, loadErr) {
		t.Fatalf("err = %v, want wrapped load error", err)
	}
	if len(st.upserts) != 0 {
		t.Errorf("upserts = %d, want 0 (aborted before upsert)", len(st.upserts))
	}
	if len(d.sent) != 0 {
		t.Errorf("notifications = %d, want 0 (aborted before notify)", len(d.sent))
	}
}

// TestRouteChecksRollerErrorAborts: a ChecksRoller error on a CHECKS event
// aborts BEFORE apply — the wrapped error surfaces, no upsert, no dispatch.
func TestRouteChecksRollerErrorAborts(t *testing.T) {
	rollErr := errors.New("roll boom")
	st := &fakeNotifyStore{
		cursor:      &ArtifactCursor{Repo: "o/r", Kind: kindPR, Number: 7, ChecksETag: `"e"`},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	d := &fakeDispatcher{}
	ev := forge.ForgeEvent{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com", Repo: "o/r", Kind: kindPR, Number: 7, URL: "u", Change: chChecks, HeadSHA: "sha1"}
	err := newRouter(t, st, d, &fakeChecksRoller{err: rollErr}).Route(context.Background(), ev)
	if !errors.Is(err, rollErr) {
		t.Fatalf("err = %v, want wrapped roller error", err)
	}
	if len(st.upserts) != 0 {
		t.Errorf("upserts = %d, want 0 (aborted before apply)", len(st.upserts))
	}
	if len(d.sent) != 0 {
		t.Errorf("notifications = %d, want 0 (aborted before notify)", len(d.sent))
	}
}

// TestRouteUpsertErrorNoDispatch: an UpsertArtifactCursor error surfaces and NO
// notification dispatches — proving the upsert-before-notify ordering under
// failure (fetch-side truth must land before any agent is told).
func TestRouteUpsertErrorNoDispatch(t *testing.T) {
	upsertErr := errors.New("upsert boom")
	st := &fakeNotifyStore{
		upsertErr:   upsertErr,
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	d := &fakeDispatcher{}
	err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), commentEvent("https://gh/c1"))
	if !errors.Is(err, upsertErr) {
		t.Fatalf("err = %v, want wrapped upsert error", err)
	}
	if len(d.sent) != 0 {
		t.Errorf("notifications = %d, want 0 (upsert failed before notify)", len(d.sent))
	}
}

// TestMeetingPointInvariant is the cross-producer canonicalization invariant
// (design.md:517-533, 882-885, 978-981): for EVERY event kind, T4's webhook
// ApplyEvent must produce a snapshot whose revision is IDENTICAL to what T5's
// full-fetch rebuild of the SAME resulting state produces, with an empty diff.
//
// This is the REAL form of T4's rebuildFromFetch stand-in: the fetch-side
// builder is DetectChanges over a FetchedArtifact that observes the same state
// the event produced. The T5-half acceptance: apply-event-then-full-fetch of the
// same state -> empty diff (no synthetic changes), identical revision.
func TestMeetingPointInvariant(t *testing.T) {
	cases := []struct {
		name    string
		ev      forge.ForgeEvent
		fetched FetchedArtifact // the state a full fetch observes AFTER the event
	}{
		{
			name: "comment",
			ev:   commentEvent("https://gh/c1"),
			fetched: FetchedArtifact{
				Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com",
				Repo: "o/r", Kind: kindIssue, Number: 7,
				Comments: []forge.Comment{{Key: "https://gh/c1", URL: "https://gh/c1", Body: "hi", ForgeAccount: "octocat"}},
			},
		},
		{
			name:    "state",
			ev:      forge.ForgeEvent{Repo: "o/r", Kind: kindPR, Number: 7, Change: chState, State: "merged"},
			fetched: FetchedArtifact{Repo: "o/r", Kind: kindPR, Number: 7, State: "merged"},
		},
		{
			name: "opened",
			ev:   forge.ForgeEvent{Repo: "SEA", Kind: kindIssue, Number: 42, Change: chOpened},
			fetched: FetchedArtifact{
				Repo: "SEA", Kind: kindIssue, Number: 0, Container: true,
				NewArtifacts: []forge.Issue{{Number: 42}},
			},
		},
		{
			name: "checks",
			ev: forge.ForgeEvent{Repo: "o/r", Kind: kindPR, Number: 7, Change: chChecks, HeadSHA: "sha1", Checks: &compassv1.ChecksSummary{
				HeadSha: "sha1", State: "failure",
				Checks: []*compassv1.Check{{Name: "test", State: "failure"}, {Name: "build", State: "success"}},
			}},
			fetched: FetchedArtifact{
				Repo: "o/r", Kind: kindPR, Number: 7,
				// Deliberately UNSORTED to prove canonicalization sorts them.
				Checks: &forge.Checks{HeadSHA: "sha1", State: "failure", Checks: []forge.Check{
					{Name: "test", State: "failure"}, {Name: "build", State: "success"},
				}},
			},
		},
		{
			name:    "update-neutral",
			ev:      forge.ForgeEvent{Repo: "o/r", Kind: kindIssue, Number: 7, Change: chUpdate},
			fetched: FetchedArtifact{Repo: "o/r", Kind: kindIssue, Number: 7},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := ApplyEvent(nil, tc.ev)
			// The webhook-applied snapshot is the sweep's prior; a full fetch of
			// the same state must detect NO change and rebuild the identical
			// snapshot + revision (the meeting-point invariant, T5 half).
			changes, rebuilt, revision := DetectChanges(&applied, tc.fetched)
			if len(changes) != 0 {
				t.Errorf("apply-then-fetch produced %d changes, want 0 (phantom-diff heartbeat)", len(changes))
			}
			if string(canonicalJSON(&applied)) != string(canonicalJSON(&rebuilt)) {
				t.Errorf("canonical diff nonempty:\n apply  = %s\n rebuild= %s", canonicalJSON(&applied), canonicalJSON(&rebuilt))
			}
			if revision != SnapshotRevision(&applied) {
				t.Errorf("revision mismatch: apply=%s rebuild=%s", SnapshotRevision(&applied), revision)
			}
			// Baseline arm: a nil prior is a first observation -> rebuilds the
			// same canonical snapshot but emits NO changes.
			baseChanges, baseSnap, baseRev := DetectChanges(nil, tc.fetched)
			if len(baseChanges) != 0 {
				t.Errorf("baseline produced %d changes, want 0", len(baseChanges))
			}
			if baseRev != SnapshotRevision(&applied) || string(canonicalJSON(&baseSnap)) != string(canonicalJSON(&applied)) {
				t.Errorf("baseline rebuild diverged from applied: base=%s applied=%s", baseRev, SnapshotRevision(&applied))
			}
		})
	}
}

// TestCrossProducerLinearCommentNoPhantomDiff is the Fork 1 regression
// (RIG-2732): a Linear COMMENT webhook keys its snapshot comment by the stable
// comment key (the UUID both producers carry), NOT the delivered issue URL — so
// the reconcile sweep, which observes the same comment via its OWN comment URL,
// detects NO change and rebuilds a byte-identical revision. Before the key fix
// the webhook keyed by the parent issue URL (Linear's comment payload has no
// comment URL) and the sweep keyed by the comment URL, a phantom-diff heartbeat
// every sweep.
func TestCrossProducerLinearCommentNoPhantomDiff(t *testing.T) {
	const (
		issueURL   = "https://linear.app/acme/issue/RIG-7"
		commentURL = "https://linear.app/acme/issue/RIG-7#comment-abc"
		commentKey = "c-uuid-1"
		body       = "hi"
		account    = "Alice"
	)
	// Webhook arm: the delivered link is the parent ISSUE url (Linear's comment
	// payload carries no comment URL); the stable key is the comment UUID.
	ev := forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     "linear.app", Repo: "RIG", Kind: kindIssue, Number: 7,
		URL:    issueURL,
		Change: chComment,
		Comment: &compassv1internal.CommentRef{
			Url: issueURL, CommentKey: commentKey, Body: body, ForgeAccount: account,
		},
	}
	applied := ApplyEvent(nil, ev)

	// Sweep arm: the SAME comment state, observed with the comment's OWN url.
	fetched := FetchedArtifact{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     "linear.app", Repo: "RIG", Kind: kindIssue, Number: 7,
		Comments: []forge.Comment{{Key: commentKey, URL: commentURL, Body: body, ForgeAccount: account}},
	}
	changes, rebuilt, revision := DetectChanges(&applied, fetched)
	if len(changes) != 0 {
		t.Errorf("cross-producer sweep produced %d changes, want 0 (phantom-diff heartbeat)", len(changes))
	}
	if string(canonicalJSON(&applied)) != string(canonicalJSON(&rebuilt)) {
		t.Errorf("canonical diff nonempty:\n webhook = %s\n sweep   = %s", canonicalJSON(&applied), canonicalJSON(&rebuilt))
	}
	if revision != SnapshotRevision(&applied) {
		t.Errorf("revision mismatch: webhook=%s sweep=%s", SnapshotRevision(&applied), revision)
	}
}

func TestDetectChangesCommentAttributionIncludesOwner(t *testing.T) {
	body, err := forge.StampOwner("real body", forge.Author{AgentHandle: "agent-x", OwnerHandle: "owner-y", SessionID: "sess-1"}, 0)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	prev := &ArtifactSnapshot{Comments: map[string]SnapshotComment{}}
	fetched := FetchedArtifact{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     "github.com", Repo: "owner/repo", Kind: kindIssue, Number: 7,
		Comments: []forge.Comment{{Key: "comment-1", URL: "https://github.com/owner/repo/issues/7#comment-1", Body: body}},
	}
	changes, _, _ := DetectChanges(prev, fetched)
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(changes))
	}
	got := changes[0].Comment.GetAgent()
	if got.GetAgentHandle() != "agent-x" || got.GetOwnerHandle() != "owner-y" {
		t.Errorf("attribution = %v, want agent-x/owner-y", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustUnJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

// TestRouteZeroCoordinateRejected: a zero provider/kind is a caller bug.
func TestRouteZeroCoordinateRejected(t *testing.T) {
	st := &fakeNotifyStore{}
	err := newRouter(t, st, &fakeDispatcher{}, &fakeChecksRoller{}).Route(context.Background(), forge.ForgeEvent{Repo: "o/r", Number: 7, Change: chComment})
	if !errors.Is(err, errInvalidEvent) {
		t.Errorf("err = %v, want errInvalidEvent", err)
	}
	if len(st.upserts) != 0 {
		t.Error("upserted a cursor for an invalid event")
	}
}

// ---- helpers ----

func subIDs(ns []*compassv1internal.ForgeNotification) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.GetSubscriptionId()
	}
	return out
}

// ---- self-origin suppression (T1) ----

// stateEvent is a GitHub STATE event on o/r#7 (no reachable actor until the
// RIG-3331 memo consumer lands).
func stateEvent() forge.ForgeEvent {
	return forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     "github.com", Repo: "o/r", Kind: kindPR, Number: 7,
		URL: "u", Change: chState, State: "closed",
	}
}

// TestSelfOriginCommentSuppressedOnMatch: a COMMENT whose actor's
// owner-qualified handle equals the subscriber's is skipped.
func TestSelfOriginCommentSuppressedOnMatch(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}}}
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), compassComment("own")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 0 {
		t.Errorf("notifications = %d, want 0 (self-origin suppressed)", len(d.sent))
	}
}

// TestSelfOriginCommentDeliveredHumanCommenter: a human commenter (Agent unset)
// always delivers — there is no actor handle to match.
func TestSelfOriginCommentDeliveredHumanCommenter(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}}}
	// commentEvent leaves Comment.Agent nil (human commenter).
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), commentEvent("https://gh/c1")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (human commenter delivers)", len(d.sent))
	}
}

// TestSelfOriginCommentDeliveredCrossOwnerSameAgent: the load-bearing
// owner-namespace-collision case — atlas@owner-A acts, atlas@owner-B subscribes.
// The bare agent handle matches but the owner differs, so this MUST deliver (a
// bare-handle match would be a fail-CLOSED cross-agent suppression bug).
func TestSelfOriginCommentDeliveredCrossOwnerSameAgent(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-B"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-B": {Owner: "owner-B", Agent: "atlas"}}}
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), compassComment("owner-A")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (atlas@owner-A != atlas@owner-B, must deliver)", len(d.sent))
	}
}

// TestSelfOriginReviewSuppressedOnMatch: REVIEW shares the CommentRef actor
// source, so a self-review is suppressed the same way COMMENT is.
func TestSelfOriginReviewSuppressedOnMatch(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}}}
	ev := compassComment("own")
	ev.Kind = kindPR
	ev.Change = chReview
	ev.State = "approved"
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 0 {
		t.Errorf("notifications = %d, want 0 (self-review suppressed)", len(d.sent))
	}
}

// TestSelfOriginOpenedSuppressedViaAuthorHandle: OPENED resolves the actor
// through AuthorHandle; a match with the (container) subscriber suppresses.
func TestSelfOriginOpenedSuppressedViaAuthorHandle(t *testing.T) {
	st := &fakeNotifyStore{openedSub: []NotifySubscriber{
		{SubscriptionID: "s", AgentAccountID: "acct-self", Project: "proj-A", Scope: scopeContainer},
	}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{
		author:   Handle{Owner: "own", Agent: "atlas"},
		accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}},
	}
	ev := forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: "linear.app",
		Repo: "RIG", Kind: kindIssue, Number: 42, Project: "proj-A", URL: "u", Change: chOpened,
	}
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if ids.authorCalls != 1 {
		t.Errorf("AuthorHandle calls = %d, want 1 (OPENED resolves the actor once)", ids.authorCalls)
	}
	if len(d.sent) != 0 {
		t.Errorf("notifications = %d, want 0 (self-opened suppressed)", len(d.sent))
	}
}

// TestSelfOriginOpenedDeliveredOnAuthorMiss: the webhook-races-the-row case —
// AuthorHandle is a clean miss (the DL-055 row not yet committed), so the actor
// is unresolved and the OPENED dispatch delivers (fail open).
func TestSelfOriginOpenedDeliveredOnAuthorMiss(t *testing.T) {
	st := &fakeNotifyStore{openedSub: []NotifySubscriber{
		{SubscriptionID: "s", AgentAccountID: "acct-self", Project: "proj-A", Scope: scopeContainer},
	}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{
		authorMiss: true,
		accounts:   map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}},
	}
	ev := forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: "linear.app",
		Repo: "RIG", Kind: kindIssue, Number: 42, Project: "proj-A", URL: "u", Change: chOpened,
	}
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (author-row miss delivers, fail open)", len(d.sent))
	}
}

// TestSelfOriginChecksNeverSuppressed is the CHECKS invariant: CI results on an
// agent's own push are the point of watching CI. The event carries a matching
// Compass commenter, so the arm — not the absence of actor evidence — is what
// keeps the dispatch.
func TestSelfOriginChecksNeverSuppressed(t *testing.T) {
	st := &fakeNotifyStore{
		cursor:      &ArtifactCursor{Repo: "o/r", Kind: kindPR, Number: 7},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}},
	}
	d := &fakeDispatcher{}
	// A resolver that would match ANY subscriber — proving the CHECKS arm never
	// consults it.
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}}}
	roller := &fakeChecksRoller{res: forge.ConditionalResult[forge.Checks]{
		V: forge.Checks{HeadSHA: "sha1", State: "success"},
	}}
	r := NewNotifyRouter(st, d, roller, nil, ids, testRef(), nil)
	ev := compassComment("own")
	ev.Repo = "o/r"
	ev.Kind = kindPR
	ev.Number = 7
	ev.Change = chChecks
	ev.HeadSHA = "sha1"
	if err := r.Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (CHECKS is never suppressed)", len(d.sent))
	}
}

// TestSelfOriginUpdateNeverSuppressed: UPDATE never suppresses even when the
// event carries an actor matching the subscriber, so the arm is what delivers.
func TestSelfOriginUpdateNeverSuppressed(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}}}
	ev := compassComment("own")
	ev.Kind = kindIssue
	ev.Change = chUpdate
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), ev); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (UPDATE never suppressed)", len(d.sent))
	}
}

// TestSelfOriginStateDeliversWithNoMemoConsumer: STATE has no reachable actor
// through the two-method seam (RIG-3331's memo consumer is not wired), so the
// actor resolves to a zero Handle and STATE delivers (the safe interim).
func TestSelfOriginStateDeliversWithNoMemoConsumer(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "own", Agent: "atlas"}}}
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), stateEvent()); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (STATE interim-open, no memo consumer)", len(d.sent))
	}
}

// TestSelfOriginUnqualifiedActorDelivers: an actor whose owner (or agent) is
// empty is unqualified, so no positive match is possible and the dispatch
// delivers even to an identically-named subscriber.
func TestSelfOriginUnqualifiedActorDelivers(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accounts: map[string]Handle{"acct-self": {Owner: "", Agent: "atlas"}}}
	// Actor has an empty owner (a body whose header carried no owner).
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), compassComment("")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (unqualified actor -> fail open)", len(d.sent))
	}
}

// TestSelfOriginResolverFaultDelivers: a store fault from HandleForAccount is
// logged and treated as a miss — the dispatch delivers (fail open).
func TestSelfOriginResolverFaultDelivers(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	ids := &fakeIdentityResolver{accountErr: errors.New("resolver boom")}
	if err := newRouterWithIDs(t, st, d, ids).Route(context.Background(), compassComment("own")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (resolver fault -> fail open)", len(d.sent))
	}
}

// TestSelfOriginNilResolverDeliversEverything: a nil IdentityResolver disables
// suppression wholesale — even a self-comment that would otherwise match is
// delivered.
func TestSelfOriginNilResolverDeliversEverything(t *testing.T) {
	st := &fakeNotifyStore{artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "acct-self"}}}
	d := &fakeDispatcher{}
	// nil ids via newRouter (the legacy shape).
	if err := newRouter(t, st, d, &fakeChecksRoller{}).Route(context.Background(), compassComment("own")); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.sent) != 1 {
		t.Errorf("notifications = %d, want 1 (nil resolver disables suppression)", len(d.sent))
	}
}
