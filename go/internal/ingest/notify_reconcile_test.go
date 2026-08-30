package ingest

// Reconciliation-sweep acceptance (RIG-2732 T5, design.md:971-981). Fakes for
// the NotifyReader + the NotifyStore seam, over a REAL NotifyRouter with a fake
// dispatcher + checks roller. context.Background() here is the test root — the
// sanctioned F-ttsr exemption (mirrors notify_router_test.go).

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// fakeReader scripts the conditional reads per coordinate. A nil entry for a
// half means "not scripted" (the fetch returns a NotModified for that half so a
// test only wires the halves it exercises); errFor scripts a per-op error.
type fakeReader struct {
	issue        map[uint64]forge.ConditionalResult[forge.Issue]
	pull         map[uint64]forge.ConditionalResult[forge.PullRequest]
	comments     map[uint64]forge.ConditionalResult[[]forge.Comment]
	checks       map[uint64]forge.ConditionalResult[forge.Checks]
	newArtifacts forge.ConditionalResult[[]forge.Issue]

	issueErr   error
	pullErr    error
	commentErr error
	checksErr  error
	newErr     error

	calls int
}

func (r *fakeReader) GetIssueConditional(_ context.Context, _ string, number uint64, _ string) (forge.ConditionalResult[forge.Issue], error) {
	r.calls++
	if r.issueErr != nil {
		return forge.ConditionalResult[forge.Issue]{}, r.issueErr
	}
	if res, ok := r.issue[number]; ok {
		return res, nil
	}
	return forge.ConditionalResult[forge.Issue]{NotModified: true}, nil
}

func (r *fakeReader) GetPullRequestConditional(_ context.Context, _ string, number uint64, _ string) (forge.ConditionalResult[forge.PullRequest], error) {
	r.calls++
	if r.pullErr != nil {
		return forge.ConditionalResult[forge.PullRequest]{}, r.pullErr
	}
	if res, ok := r.pull[number]; ok {
		return res, nil
	}
	return forge.ConditionalResult[forge.PullRequest]{NotModified: true}, nil
}

func (r *fakeReader) ListComments(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, number uint64, _ string) (forge.ConditionalResult[[]forge.Comment], error) {
	r.calls++
	if r.commentErr != nil {
		return forge.ConditionalResult[[]forge.Comment]{}, r.commentErr
	}
	if res, ok := r.comments[number]; ok {
		return res, nil
	}
	return forge.ConditionalResult[[]forge.Comment]{NotModified: true}, nil
}

func (r *fakeReader) ChecksConditional(_ context.Context, _ string, number uint64, _, _ string) (forge.ConditionalResult[forge.Checks], error) {
	r.calls++
	if r.checksErr != nil {
		return forge.ConditionalResult[forge.Checks]{}, r.checksErr
	}
	if res, ok := r.checks[number]; ok {
		return res, nil
	}
	return forge.ConditionalResult[forge.Checks]{NotModified: true}, nil
}

func (r *fakeReader) ListNewArtifacts(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, _ uint64, _ string) (forge.ConditionalResult[[]forge.Issue], error) {
	r.calls++
	if r.newErr != nil {
		return forge.ConditionalResult[[]forge.Issue]{}, r.newErr
	}
	return r.newArtifacts, nil
}

func newReconciler(t *testing.T, rd forge.NotifyReader, st *fakeNotifyStore, d *fakeDispatcher) *NotifyReconciler {
	t.Helper()
	router := NewNotifyRouter(st, d, &fakeChecksRoller{}, testRef(), nil)
	return NewNotifyReconciler(rd, st, router,
		compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, "github.com",
		ReconcileConfig{Pace: -1}) // pacing disabled: no real sleeps in tests
}

// snapWith builds a stored issue cursor whose snapshot is the given
// ArtifactSnapshot (kind is always ISSUE in these fixtures).
func snapWith(t *testing.T, repo string, number uint64, snap ArtifactSnapshot) *ArtifactCursor {
	t.Helper()
	return &ArtifactCursor{
		Repo: repo, Kind: kindIssue, Number: number,
		Revision: SnapshotRevision(&snap), Snapshot: mustJSON(t, &snap),
	}
}

// TestSweepStartupHealsGap: the stored snapshot is BEHIND live (a missed webhook
// left a comment unrecorded). The startup sweep fetches the live comment set,
// detects the new comment, and Routes it -> a notification reaches the
// exact-coordinate subscriber.
func TestSweepStartupHealsGap(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{{
			Repo: "o/r", Kind: kindIssue, Number: 7,
			Cursor:      snapWith(t, "o/r", 7, prior),
			Subscribers: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a", DeliveredRevision: SnapshotRevision(&prior)}},
		}},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	rd := &fakeReader{
		issue:    map[uint64]forge.ConditionalResult[forge.Issue]{7: {V: forge.Issue{Number: 7, State: "open", URL: "u"}, ETag: `"i1"`}},
		comments: map[uint64]forge.ConditionalResult[[]forge.Comment]{7: {V: []forge.Comment{{URL: "https://gh/c1", Body: "hi", ForgeAccount: "octocat"}}, ETag: `"c1"`}},
	}
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if len(d.sent) != 1 || d.sent[0].GetChange() != chComment {
		t.Fatalf("dispatched %d notifications %v, want 1 COMMENT healing the gap", len(d.sent), d.sent)
	}
	if d.sent[0].GetComment().GetUrl() != "https://gh/c1" {
		t.Errorf("healed comment url = %q, want https://gh/c1", d.sent[0].GetComment().GetUrl())
	}
}

// TestSweepAll304NoDispatch: every conditional read 304s -> no change, no
// dispatch; the cursor is still re-upserted (polled_at/ETag refresh) but its
// revision is unchanged.
func TestSweepAll304NoDispatch(t *testing.T) {
	prior := ArtifactSnapshot{State: "open", Comments: map[string]SnapshotComment{"https://gh/c1": {URL: "https://gh/c1", Body: "hi", ForgeAccount: "octocat"}}}
	cur := snapWith(t, "o/r", 7, prior)
	cur.ETag, cur.CommentsETag = `"i0"`, `"c0"`
	st := &fakeNotifyStore{
		targets: []NotifyTarget{{
			Repo: "o/r", Kind: kindIssue, Number: 7, Cursor: cur,
			Subscribers: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a", DeliveredRevision: SnapshotRevision(&prior)}},
		}},
	}
	rd := &fakeReader{} // no scripted results -> every read 304s
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if len(d.sent) != 0 {
		t.Fatalf("dispatched %d, want 0 on an all-304 sweep", len(d.sent))
	}
	if len(st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1 (cursor still refreshed on 304)", len(st.upserts))
	}
	if st.upserts[0].Revision != SnapshotRevision(&prior) {
		t.Errorf("revision changed on all-304 sweep: %q != %q", st.upserts[0].Revision, SnapshotRevision(&prior))
	}
}

// TestSweepLaggingSubscriberEmptyDiffSynthesizesOneUpdate: no diff (all 304),
// but a subscriber's delivered_revision trails the cursor -> exactly ONE
// payload-free UPDATE, and delivered_revision is NEVER advanced by the sweep (W3).
func TestSweepLaggingSubscriberEmptyDiffSynthesizesOneUpdate(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	rev := SnapshotRevision(&prior)
	st := &fakeNotifyStore{
		targets: []NotifyTarget{{
			Repo: "o/r", Kind: kindIssue, Number: 7,
			Cursor: snapWith(t, "o/r", 7, prior),
			Subscribers: []NotifySubscriber{
				{SubscriptionID: "lag", AgentAccountID: "a-lag", DeliveredRevision: "stale"},
				{SubscriptionID: "current", AgentAccountID: "a-cur", DeliveredRevision: rev},
			},
		}},
	}
	rd := &fakeReader{} // all 304 -> empty diff
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if len(d.sent) != 1 {
		t.Fatalf("dispatched %d, want exactly 1 synthesized UPDATE", len(d.sent))
	}
	got := d.sent[0]
	if got.GetSubscriptionId() != "lag" || got.GetChange() != chUpdate {
		t.Errorf("synthesized = %s/%v, want lag/UPDATE", got.GetSubscriptionId(), got.GetChange())
	}
	if got.GetComment() != nil || got.GetChecks() != nil || got.GetState() != "" {
		t.Errorf("synthesized UPDATE carries a payload, want payload-free: %+v", got)
	}
	if got.GetRevision() != rev {
		t.Errorf("synthesized revision = %q, want the current cursor revision %q", got.GetRevision(), rev)
	}
	if st.advanceCalls != 0 {
		t.Errorf("delivered_revision advanced %d, want 0 (advance rides the ack, W3)", st.advanceCalls)
	}
}

// TestSweepLaggingSubscriberRealDiffNoSynthesis: a lagging subscriber AND a real
// diff -> the real change set is routed (which re-notifies the exact-coordinate
// subs), and NOTHING is synthesized (the synthesized-UPDATE arm is empty-diff only).
func TestSweepLaggingSubscriberRealDiffNoSynthesis(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{{
			Repo: "o/r", Kind: kindIssue, Number: 7,
			Cursor:      snapWith(t, "o/r", 7, prior),
			Subscribers: []NotifySubscriber{{SubscriptionID: "lag", AgentAccountID: "a", DeliveredRevision: "stale"}},
		}},
		artifactSub: []NotifySubscriber{{SubscriptionID: "lag", AgentAccountID: "a"}},
	}
	rd := &fakeReader{
		issue: map[uint64]forge.ConditionalResult[forge.Issue]{7: {V: forge.Issue{Number: 7, State: "closed", URL: "u"}, ETag: `"i1"`}},
	}
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if len(d.sent) != 1 {
		t.Fatalf("dispatched %d, want 1 (the real STATE change, nothing synthesized)", len(d.sent))
	}
	if d.sent[0].GetChange() != chState || d.sent[0].GetState() != "closed" {
		t.Errorf("dispatched %s/%q, want STATE/closed", d.sent[0].GetChange(), d.sent[0].GetState())
	}
}

// TestSweepContainerScopeOpensAboveHighWater: a container target (number 0) with
// ListNewArtifacts returning artifacts above the stored high-water -> one OPENED
// per new artifact, container-scope (project-matched); a >1-page burst is not
// truncated (the fake returns the full set the reader's walk assembled).
func TestSweepContainerScopeOpensAboveHighWater(t *testing.T) {
	prior := ArtifactSnapshot{HighWaterNumber: 40}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{{
			Repo: "SEA", Kind: kindIssue, Number: 0,
			Cursor: snapWith(t, "SEA", 0, prior),
		}},
		openedSub: []NotifySubscriber{{SubscriptionID: "c", AgentAccountID: "a-c", Project: "proj-A"}},
	}
	rd := &fakeReader{newArtifacts: forge.ConditionalResult[[]forge.Issue]{
		V: []forge.Issue{
			{Number: 42, URL: "u42", Project: "proj-A"},
			{Number: 41, URL: "u41", Project: "proj-A"},
			{Number: 30, URL: "u30", Project: "proj-A"}, // below high-water: ignored
		},
		ETag: `"n1"`,
	}}
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if len(d.sent) != 2 {
		t.Fatalf("dispatched %d OPENED, want 2 (42 and 41, not 30)", len(d.sent))
	}
	for _, n := range d.sent {
		if n.GetChange() != chOpened {
			t.Errorf("change = %v, want OPENED", n.GetChange())
		}
		if n.GetNumber() != 42 && n.GetNumber() != 41 {
			t.Errorf("opened number = %d, want 41 or 42", n.GetNumber())
		}
	}
	// The high-water advanced to 42 in the upserted container cursor.
	var snap ArtifactSnapshot
	mustUnJSON(t, st.upserts[0].Snapshot, &snap)
	if snap.HighWaterNumber != 42 {
		t.Errorf("container high-water = %d, want 42", snap.HighWaterNumber)
	}
}

// TestSweepBudgetAbortStopsSweep: a target returning ErrBudgetExhausted aborts
// the sweep — a later target is NOT fetched (resumed next interval).
func TestSweepBudgetAbortStopsSweep(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{
			{Repo: "o/r", Kind: kindIssue, Number: 7, Cursor: snapWith(t, "o/r", 7, prior)},
			{Repo: "o/r", Kind: kindIssue, Number: 8, Cursor: snapWith(t, "o/r", 8, prior)},
		},
	}
	rd := &fakeReader{issueErr: forge.ErrBudgetExhausted}
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	// Only the first target's issue read fired before the budget abort.
	if rd.calls != 1 {
		t.Errorf("reader calls = %d, want 1 (sweep aborted after the first budget-exhausted target)", rd.calls)
	}
	if len(st.upserts) != 0 {
		t.Errorf("upserts = %d, want 0 (budget abort before any cursor write)", len(st.upserts))
	}
}

// TestSweepBudgetAbortRateLimitError (RIG-2255 T5 regression pin): a target
// returning a *forge.RateLimitError — the TYPED rate-limit error, not the bare
// ErrBudgetExhausted sentinel — still takes the budget-exhausted abort branch
// (notify_reconcile.go, errors.Is walks RateLimitError.Unwrap → sentinel), so no
// consumer of the sentinel regressed when the emission sites started carrying the
// retry hint.
func TestSweepBudgetAbortRateLimitError(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{
			{Repo: "o/r", Kind: kindIssue, Number: 7, Cursor: snapWith(t, "o/r", 7, prior)},
			{Repo: "o/r", Kind: kindIssue, Number: 8, Cursor: snapWith(t, "o/r", 8, prior)},
		},
	}
	rd := &fakeReader{issueErr: &forge.RateLimitError{RetryAfter: 60 * time.Second}}
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if rd.calls != 1 {
		t.Errorf("reader calls = %d, want 1 (sweep aborted on a typed RateLimitError)", rd.calls)
	}
	if len(st.upserts) != 0 {
		t.Errorf("upserts = %d, want 0 (budget abort before any cursor write)", len(st.upserts))
	}
}

// TestSweepPerTargetErrorIsolation: a target that errors (non-budget) is logged
// and skipped; the sweep continues to the next target.
func TestSweepPerTargetErrorIsolation(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{
			{Repo: "o/r", Kind: kindIssue, Number: 7, Cursor: snapWith(t, "o/r", 7, prior)},
			{
				Repo: "o/r", Kind: kindIssue, Number: 8,
				Cursor:      snapWith(t, "o/r", 8, prior),
				Subscribers: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a", DeliveredRevision: SnapshotRevision(&prior)}},
			},
		},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	// #7's issue read errors; #8's comment read heals a gap.
	rd := &errByNumberReader{
		errNumbers: map[uint64]error{7: errors.New("boom")},
		delegate: &fakeReader{
			comments: map[uint64]forge.ConditionalResult[[]forge.Comment]{8: {V: []forge.Comment{{URL: "https://gh/c8", Body: "b", ForgeAccount: "u"}}, ETag: `"c8"`}},
		},
	}
	d := &fakeDispatcher{}
	newReconciler(t, rd, st, d).sweep(context.Background())

	if len(d.sent) != 1 || d.sent[0].GetNumber() != 8 {
		t.Fatalf("dispatched %v, want 1 healing #8 after #7 isolated", d.sent)
	}
}

// errByNumberReader wraps a delegate reader, erroring the issue read for scripted
// numbers (to drive per-target isolation).
type errByNumberReader struct {
	errNumbers map[uint64]error
	delegate   *fakeReader
}

func (r *errByNumberReader) GetIssueConditional(ctx context.Context, repo string, number uint64, etag string) (forge.ConditionalResult[forge.Issue], error) {
	if err, ok := r.errNumbers[number]; ok {
		return forge.ConditionalResult[forge.Issue]{}, err
	}
	return r.delegate.GetIssueConditional(ctx, repo, number, etag)
}

func (r *errByNumberReader) GetPullRequestConditional(ctx context.Context, repo string, number uint64, etag string) (forge.ConditionalResult[forge.PullRequest], error) {
	return r.delegate.GetPullRequestConditional(ctx, repo, number, etag)
}

func (r *errByNumberReader) ListComments(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, etag string) (forge.ConditionalResult[[]forge.Comment], error) {
	return r.delegate.ListComments(ctx, repo, kind, number, etag)
}

func (r *errByNumberReader) ChecksConditional(ctx context.Context, repo string, number uint64, headSHA, etag string) (forge.ConditionalResult[forge.Checks], error) {
	return r.delegate.ChecksConditional(ctx, repo, number, headSHA, etag)
}

func (r *errByNumberReader) ListNewArtifacts(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, sinceNumber uint64, etag string) (forge.ConditionalResult[[]forge.Issue], error) {
	return r.delegate.ListNewArtifacts(ctx, repo, kind, sinceNumber, etag)
}

// TestSweepCtxCancelPromptReturn: a context cancelled before the sweep returns
// promptly without fetching.
func TestSweepCtxCancelPromptReturn(t *testing.T) {
	st := &fakeNotifyStore{targets: []NotifyTarget{{Repo: "o/r", Kind: kindIssue, Number: 7}}}
	rd := &fakeReader{}
	d := &fakeDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newReconciler(t, rd, st, d).sweep(ctx)
	if rd.calls != 0 {
		t.Errorf("reader calls = %d, want 0 (cancelled before sweep)", rd.calls)
	}
}

// TestRunImmediateSweepThenCancel: Run performs one immediate sweep then returns
// nil when ctx cancels (clean shutdown).
func TestRunImmediateSweepThenCancel(t *testing.T) {
	prior := ArtifactSnapshot{State: "open"}
	st := &fakeNotifyStore{
		targets: []NotifyTarget{{
			Repo: "o/r", Kind: kindIssue, Number: 7,
			Cursor:      snapWith(t, "o/r", 7, prior),
			Subscribers: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a", DeliveredRevision: SnapshotRevision(&prior)}},
		}},
		artifactSub: []NotifySubscriber{{SubscriptionID: "s", AgentAccountID: "a"}},
	}
	rd := &fakeReader{
		comments: map[uint64]forge.ConditionalResult[[]forge.Comment]{7: {V: []forge.Comment{{URL: "https://gh/c1", Body: "hi", ForgeAccount: "octocat"}}, ETag: `"c1"`}},
	}
	d := &fakeDispatcher{}
	synctest.Test(t, func(t *testing.T) {
		rc := NewNotifyReconciler(rd, st, NewNotifyRouter(st, d, &fakeChecksRoller{}, testRef(), nil),
			compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, "github.com",
			ReconcileConfig{Backstop: time.Hour, Pace: -1}) // long backstop: only the immediate sweep fires
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- rc.Run(ctx) }()

		synctest.Wait() // immediate sweep completes; blocked on the ticker
		if len(d.sent) != 1 {
			t.Errorf("immediate sweep dispatched %d, want 1 (the startup heal)", len(d.sent))
		}
		cancel()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v, want nil (clean shutdown)", err)
		}
	})
}
