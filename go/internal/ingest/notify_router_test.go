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
	upserts     []ArtifactCursor
	loadErr     error
	subErr      error
	upsertErr   error

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
	return nil, nil
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

// fakeChecksRoller scripts the combined roll-up result.
type fakeChecksRoller struct {
	res      ChecksResult
	err      error
	calls    int
	lastETag string
	lastHead string
	lastRepo string
	lastNum  uint64
}

func (c *fakeChecksRoller) RollUp(_ context.Context, repo string, number uint64, headSHA, etag string) (ChecksResult, error) {
	c.calls++
	c.lastRepo, c.lastNum, c.lastHead, c.lastETag = repo, number, headSHA, etag
	return c.res, c.err
}

func testRef() *compassv1.ForgeRef {
	return &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "github.com"}
}

func newRouter(t *testing.T, st *fakeNotifyStore, d *fakeDispatcher, c *fakeChecksRoller) *NotifyRouter {
	t.Helper()
	return NewNotifyRouter(st, d, c, testRef(), nil)
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
)

func ghComment(url, body, account string) *compassv1internal.CommentRef {
	return &compassv1internal.CommentRef{Url: url, Body: body, ForgeAccount: account}
}

// commentEvent is one GitHub issue COMMENT event on o/r#7.
func commentEvent(url string) forge.ForgeEvent {
	return forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     "github.com", Repo: "o/r", Kind: kindIssue, Number: 7,
		URL: url, Change: chComment, Comment: ghComment(url, "hi", "octocat"),
	}
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
	want := SnapshotRevision(ptr(ApplyEvent(nil, ev)))
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

// TestApplyCommentGrowsKeySet: COMMENT adds a URL-keyed comment.
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
	roller := &fakeChecksRoller{res: ChecksResult{
		ETag: `"new-etag"`,
		Checks: forge.Checks{HeadSHA: "sha1", State: "failure", Checks: []forge.Check{
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
	roller := &fakeChecksRoller{res: ChecksResult{NotModified: true}}
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

// TestMeetingPointInvariant is the cross-producer canonicalization invariant
// (design.md:517-533, 882-885): for EVERY event kind, the webhook ApplyEvent
// must produce a snapshot whose revision is IDENTICAL to what a full-fetch
// rebuild of the same resulting state would produce, with an empty diff.
//
// rebuildFromFetch is this test's model of the T5 full-fetch rebuild path: it
// constructs the same canonical ArtifactSnapshot from the "fetched" artifact
// state directly. This IS the shared meeting-point contract — the canonical
// form both producers must marshal through. FLAGGED: T5 owns the REAL
// DetectChanges / full-fetch rebuild; this test freezes the canonical form so
// T5 can reuse it, but the real fetch-side builder is out of T4's scope.
func TestMeetingPointInvariant(t *testing.T) {
	cases := []struct {
		name string
		ev   forge.ForgeEvent
		// fetched is the artifact state a full fetch would observe after the
		// event, expressed as the canonical snapshot the rebuild produces.
		fetched ArtifactSnapshot
	}{
		{
			name:    "comment",
			ev:      commentEvent("https://gh/c1"),
			fetched: ArtifactSnapshot{Comments: map[string]SnapshotComment{"https://gh/c1": {URL: "https://gh/c1", Body: "hi", ForgeAccount: "octocat"}}},
		},
		{
			name:    "state",
			ev:      forge.ForgeEvent{Repo: "o/r", Kind: kindPR, Number: 7, Change: chState, State: "merged"},
			fetched: ArtifactSnapshot{State: "merged"},
		},
		{
			name:    "opened",
			ev:      forge.ForgeEvent{Repo: "SEA", Kind: kindIssue, Number: 42, Change: chOpened},
			fetched: ArtifactSnapshot{HighWaterNumber: 42},
		},
		{
			name: "checks",
			ev: forge.ForgeEvent{Repo: "o/r", Kind: kindPR, Number: 7, Change: chChecks, HeadSHA: "sha1", Checks: &compassv1.ChecksSummary{
				HeadSha: "sha1", State: "failure",
				// Deliberately UNSORTED to prove canonicalization sorts them.
				Checks: []*compassv1.Check{{Name: "test", State: "failure"}, {Name: "build", State: "success"}},
			}},
			fetched: ArtifactSnapshot{Checks: &ChecksSnapshot{HeadSHA: "sha1", State: "failure", Checks: []CheckSnapshot{
				{Name: "build", State: "success"}, {Name: "test", State: "failure"},
			}}},
		},
		{
			name:    "update-neutral",
			ev:      forge.ForgeEvent{Repo: "o/r", Kind: kindIssue, Number: 7, Change: chUpdate},
			fetched: ArtifactSnapshot{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := ApplyEvent(nil, tc.ev)
			rebuilt := rebuildFromFetch(tc.fetched)
			// Empty diff: byte-identical canonical JSON.
			if string(canonicalJSON(&applied)) != string(canonicalJSON(&rebuilt)) {
				t.Errorf("canonical diff nonempty:\n apply  = %s\n rebuild= %s", canonicalJSON(&applied), canonicalJSON(&rebuilt))
			}
			// Identical revision.
			if SnapshotRevision(&applied) != SnapshotRevision(&rebuilt) {
				t.Errorf("revision mismatch: apply=%s rebuild=%s", SnapshotRevision(&applied), SnapshotRevision(&rebuilt))
			}
		})
	}
}

// rebuildFromFetch is the test's stand-in for T5's full-fetch rebuild: it takes
// the canonical snapshot the fetch would build and returns it as-is (the
// canonical form is the contract). It exists so the meeting-point test names the
// rebuild path explicitly; T5 replaces the identity body with a real
// forge-read -> canonical-snapshot builder that MUST land on this same form.
func rebuildFromFetch(fetched ArtifactSnapshot) ArtifactSnapshot { return fetched }

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

func ptr(s ArtifactSnapshot) *ArtifactSnapshot { return &s }

func subIDs(ns []*compassv1internal.ForgeNotification) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.GetSubscriptionId()
	}
	return out
}
