package ingest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// fakePulls is the PR read surface: GetPullRequest records each number read
// and fails with errFor[number] when set.
type fakePulls struct {
	mu     sync.Mutex
	reads  []uint64
	errFor map[uint64]error
	open   []forge.UpdatedPull
}

func (f *fakePulls) GetPullRequest(_ context.Context, _ string, number uint64) (forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, number)
	if err := f.errFor[number]; err != nil {
		return forge.PullRequest{}, err
	}
	return forge.PullRequest{
		Number:      number,
		State:       "open",
		Body:        "<!-- compass:owner v1 agent=a owner=o session=s -->\nbody",
		CreatedAt:   ts(1),
		UpdatedAt:   ts(2),
		ClosingRefs: []forge.IssueRef{{Repo: "o/r", Number: 3}},
	}, nil
}

func (f *fakePulls) ListOpenPullRequests(_ context.Context, _ string) ([]forge.UpdatedPull, error) {
	return f.open, nil
}

func (f *fakePulls) readNumbers() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reads)
}

type recordingPRSink struct{ got []IngestedPullRequest }

func (s *recordingPRSink) PublishPullRequestUpdate(_ context.Context, in IngestedPullRequest) error {
	s.got = append(s.got, in)
	return nil
}

// fakeNumbers resolves head SHAs; an unknown SHA has no PR.
type fakeNumbers map[string]uint64

func (f fakeNumbers) PullNumberForSHA(_ context.Context, _, sha string) (uint64, error) {
	if n, ok := f[sha]; ok {
		return n, nil
	}
	return 0, forge.ErrNoPullRequestForSHA
}

func newPRArm(t *testing.T, pulls *fakePulls, numbers PullNumberResolver) (*BoardWebhookArm, *recordingPRSink) {
	t.Helper()
	return newPRArmFor(t, pulls, numbers, newFakeTargets("owner/repo"))
}

func newPRArmFor(t *testing.T, pulls *fakePulls, numbers PullNumberResolver, targets TargetChecker) (*BoardWebhookArm, *recordingPRSink) {
	t.Helper()
	sink := &recordingPRSink{}
	h := NewPullRequestHydrator(pulls, sink, testForgeRef())
	ing := NewIngester(forge.NewFakeProvider("gh"), &recordingSink{}, testForgeRef())
	arm := NewBoardWebhookArm(&fakeHydrator{}, ing, targets, BoardArmConfig{QueueSize: 64, Pulls: h, PullNumbers: numbers})
	return arm, sink
}

// checksEvent is a SHA-only CHECKS event for head SHA "abc".
func checksEvent() forge.ForgeEvent {
	ev := prEvent(0, compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS)
	ev.HeadSHA = "abc"
	return ev
}

// TestArmChecksOnDisabledRepoSpendsNoLookup: a CHECKS SHA event on a repo not
// enabled on the board makes no resolver call and no read.
func TestArmChecksOnDisabledRepoSpendsNoLookup(t *testing.T) {
	pulls := &fakePulls{}
	res := &countingResolver{bySHA: map[string]uint64{"abc": 9}}
	arm, _ := newPRArmFor(t, pulls, res, newFakeTargets())
	arm.Enqueue(context.Background(), checksEvent())
	drainAll(context.Background(), arm)
	if res.calls != 0 || len(pulls.readNumbers()) != 0 {
		t.Fatalf("resolver calls=%d reads=%v, want none", res.calls, pulls.readNumbers())
	}
}

// TestArmResolverBudgetPausesDrain: a budget error resolving a SHA abandons the
// rest of the batch.
func TestArmResolverBudgetPausesDrain(t *testing.T) {
	pulls := &fakePulls{}
	arm, _ := newPRArm(t, pulls, &countingResolver{err: forge.ErrBudgetExhausted})
	arm.Enqueue(context.Background(), checksEvent())
	arm.Enqueue(context.Background(), prEvent(11, changeUpdate))
	drainAll(context.Background(), arm)
	if got := pulls.readNumbers(); len(got) != 0 {
		t.Fatalf("reads = %v, want none after the budget pause", got)
	}
}

// TestArmNumberedBeforeChecksCoalesces: a numbered event ahead of a CHECKS
// event for the same PR still costs one hydrate.
func TestArmNumberedBeforeChecksCoalesces(t *testing.T) {
	pulls := &fakePulls{}
	arm, _ := newPRArm(t, pulls, fakeNumbers{"abc": 9})
	arm.Enqueue(context.Background(), prEvent(9, changeUpdate))
	arm.Enqueue(context.Background(), checksEvent())
	drainAll(context.Background(), arm)
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{9}) {
		t.Fatalf("reads = %v, want [9]", got)
	}
}

// TestArmDropsChecksWithoutResolver: with no resolver a SHA-only CHECKS event
// is filtered at Enqueue.
func TestArmDropsChecksWithoutResolver(t *testing.T) {
	arm, _ := newPRArm(t, &fakePulls{}, nil)
	arm.Enqueue(context.Background(), checksEvent())
	if l := len(arm.queue); l != 0 {
		t.Fatalf("queue len = %d, want 0", l)
	}
}

func prEvent(number uint64, change compassv1internal.ForgeNotificationKind) forge.ForgeEvent {
	ev := issueEvent("Owner/Repo", number, change)
	ev.Kind = boardKindPR
	return ev
}

// TestArmHydratesEveryPRChangeKind: each PR change kind reaches the sink with
// the stamped coordinate, stripped attribution, forge times and closing refs.
func TestArmHydratesEveryPRChangeKind(t *testing.T) {
	kinds := []compassv1internal.ForgeNotificationKind{
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
		changeState, changeUpdate, changeComment,
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW,
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS,
	}
	for _, k := range kinds {
		t.Run(k.String(), func(t *testing.T) {
			pulls := &fakePulls{}
			arm, sink := newPRArm(t, pulls, fakeNumbers{})
			arm.Enqueue(context.Background(), prEvent(9, k))
			drainAll(context.Background(), arm)
			if len(sink.got) != 1 {
				t.Fatalf("sank %d PRs, want 1", len(sink.got))
			}
			got := sink.got[0]
			if got.PR.GetNumber() != 9 || got.PR.GetRepo() != "owner/repo" || got.PR.GetForge().GetHost() != "github.com" {
				t.Fatalf("coordinate = %d %q %v", got.PR.GetNumber(), got.PR.GetRepo(), got.PR.GetForge())
			}
			if got.PR.GetAgent().GetAgentHandle() != "a" {
				t.Fatalf("agent = %v, want a", got.PR.GetAgent())
			}
			if !got.CreatedAt.Equal(ts(1)) || !got.UpdatedAt.Equal(ts(2)) || len(got.ClosingRefs) != 1 {
				t.Fatalf("ingested = %+v", got)
			}
		})
	}
}

// TestArmResolvesChecksByHeadSHA: a CHECKS event with only a head SHA hydrates
// its PR; a SHA with no PR is skipped without a read.
func TestArmResolvesChecksByHeadSHA(t *testing.T) {
	pulls := &fakePulls{}
	arm, sink := newPRArm(t, pulls, fakeNumbers{"abc": 12})
	for _, sha := range []string{"abc", "nopr"} {
		ev := prEvent(0, compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS)
		ev.HeadSHA = sha
		arm.Enqueue(context.Background(), ev)
	}
	drainAll(context.Background(), arm)
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{12}) {
		t.Fatalf("reads = %v, want [12]", got)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sank %d, want 1", len(sink.got))
	}
}

// TestArmCoalescesPRBurst: review, comment and check events on one PR in one
// batch, including a CHECKS event resolved by SHA, cost one hydrate.
func TestArmCoalescesPRBurst(t *testing.T) {
	pulls := &fakePulls{}
	arm, _ := newPRArm(t, pulls, fakeNumbers{"abc": 9})
	arm.Enqueue(context.Background(), prEvent(9, compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW))
	arm.Enqueue(context.Background(), prEvent(9, changeComment))
	checks := prEvent(0, compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS)
	checks.HeadSHA = "abc"
	arm.Enqueue(context.Background(), checks)
	arm.Enqueue(context.Background(), prEvent(9, changeUpdate))
	drainAll(context.Background(), arm)
	if got := pulls.readNumbers(); len(got) != 1 {
		t.Fatalf("reads = %v, want one", got)
	}
}

// TestArmIssueAndPRSameNumberStayDistinct: an issue and a PR never coalesce.
func TestArmIssueAndPRSameNumberStayDistinct(t *testing.T) {
	pulls := &fakePulls{}
	arm, _ := newPRArm(t, pulls, nil)
	arm.Enqueue(context.Background(), issueEvent("owner/repo", 9, changeUpdate))
	arm.Enqueue(context.Background(), prEvent(9, changeUpdate))
	drainAll(context.Background(), arm)
	if got := pulls.readNumbers(); len(got) != 1 {
		t.Fatalf("PR reads = %v, want one", got)
	}
}

func newPRReconciler(l updatedLister, st *fakeBoardStore, pulls *fakePulls) (*BoardReconciler, *recordingSink) {
	sink := &recordingSink{}
	h := NewPullRequestHydrator(pulls, &recordingPRSink{}, testForgeRef())
	return NewBoardReconciler(l, NewIngester(nil, sink, testForgeRef()), st, BoardReconcileConfig{Pace: -1, Pulls: h}), sink
}

// rowsLister scripts one walk result per call and records each since.
type rowsLister struct {
	results []forge.ConditionalResult[forge.UpdatedRows]
	since   []time.Time
	etags   []string
}

func (l *rowsLister) ListUpdatedIssues(_ context.Context, _ string, since time.Time, etag string) (forge.ConditionalResult[forge.UpdatedRows], error) {
	l.since = append(l.since, since)
	l.etags = append(l.etags, etag)
	i := min(len(l.since)-1, len(l.results)-1)
	return l.results[i], nil
}

// TestReconcileSkipsUnchangedPR: a PR row no newer than the stored row is not
// re-hydrated; a newer one is, and a never-stored one is.
func TestReconcileSkipsUnchangedPR(t *testing.T) {
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{{V: forge.UpdatedRows{Pulls: []forge.UpdatedPull{
		{Number: 1, State: "open", UpdatedAt: ts(10)},
		{Number: 2, State: "open", UpdatedAt: ts(20)},
		{Number: 3, State: "open", UpdatedAt: ts(30)},
	}}, ETag: `"e"`}}}
	st := newBoardStore("o/r")
	st.marks["o/r"] = storedMark{mark: ts(5)}
	st.backfilled["o/r"] = ts(0)
	st.prUpdated[1] = ts(10)
	st.prUpdated[2] = ts(15)
	pulls := &fakePulls{}
	rc, _ := newPRReconciler(l, st, pulls)
	rc.sweep(context.Background())
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{2, 3}) {
		t.Fatalf("reads = %v, want [2 3]", got)
	}
	if m := st.marks["o/r"]; !m.mark.Equal(ts(30)) || m.etag != `"e"` {
		t.Fatalf("watermark = %+v, want ts(30) with the list ETag", m)
	}
}

// TestReconcileBudgetOnPRAbortsWindow: a budget error on a PR row aborts the
// sweep, keeps the watermark at since with no ETag, and the next sweep re-lists
// the window unconditionally so an older issue row is seen again.
func TestReconcileBudgetOnPRAbortsWindow(t *testing.T) {
	window := forge.ConditionalResult[forge.UpdatedRows]{V: forge.UpdatedRows{
		Issues: []forge.Issue{{Number: 4, UpdatedAt: ts(10)}},
		Pulls:  []forge.UpdatedPull{{Number: 5, State: "open", UpdatedAt: ts(20)}},
	}, ETag: `"e"`}
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{window}}
	st := newBoardStore("o/r", "o/next")
	st.marks["o/r"] = storedMark{mark: ts(5), etag: `"old"`}
	st.backfilled["o/r"] = ts(0)
	pulls := &fakePulls{errFor: map[uint64]error{5: forge.ErrBudgetExhausted}}
	rc, sink := newPRReconciler(l, st, pulls)
	rc.sweep(context.Background())

	if m := st.marks["o/r"]; !m.mark.Equal(ts(5)) || m.etag != "" {
		t.Fatalf("watermark = %+v, want since ts(5) with no ETag", m)
	}
	if len(l.since) != 1 {
		t.Fatalf("lists = %d, want 1 (sweep aborted before o/next)", len(l.since))
	}
	pulls.errFor = nil
	st.repos = []string{"o/r"}
	rc.sweep(context.Background())
	if !l.since[1].Equal(ts(5)) || l.etags[1] != "" {
		t.Fatalf("second list since=%v etag=%q, want ts(5) and no ETag", l.since[1], l.etags[1])
	}
	if n := countNumber(sink, 4); n != 2 {
		t.Fatalf("issue #4 sank %d times, want 2 (re-listed)", n)
	}
}

func countNumber(s *recordingSink, n uint32) int {
	c := 0
	for _, iss := range s.got {
		if iss.GetNumber() == n {
			c++
		}
	}
	return c
}

// TestReconcileBackfillsOpenPRsOnce: a repo with a watermark but no backfill
// mark hydrates its open PRs and recent rows once, then is marked.
func TestReconcileBackfillsOpenPRsOnce(t *testing.T) {
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{
		{NotModified: true},
		{V: forge.UpdatedRows{Pulls: []forge.UpdatedPull{{Number: 8, State: "closed", UpdatedAt: ts(3)}}}},
		{NotModified: true},
	}}
	st := newBoardStore("o/r")
	st.marks["o/r"] = storedMark{mark: ts(50), etag: `"e"`}
	pulls := &fakePulls{open: []forge.UpdatedPull{{Number: 7, State: "open", UpdatedAt: ts(40)}}}
	rc, _ := newPRReconciler(l, st, pulls)
	rc.sweep(context.Background())
	rc.sweep(context.Background())
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{7, 8}) {
		t.Fatalf("reads = %v, want [7 8] once", got)
	}
	if _, ok := st.backfilled["o/r"]; !ok {
		t.Fatal("repo not marked backfilled")
	}
	if !l.since[1].Equal(ts(50).Add(-prBackfillWindow)) {
		t.Fatalf("backfill window since = %v", l.since[1])
	}
}

// TestReconcileHydratesCreatedPRWhoseWebhookDropped: the create path stores the
// epoch as forge_updated_at, so the next sweep hydrates the PR.
func TestReconcileHydratesCreatedPRWhoseWebhookDropped(t *testing.T) {
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{{V: forge.UpdatedRows{Pulls: []forge.UpdatedPull{
		{Number: 6, State: "open", UpdatedAt: ts(10)},
	}}}}}
	st := newBoardStore("o/r")
	st.marks["o/r"] = storedMark{mark: ts(5)}
	st.backfilled["o/r"] = ts(0)
	st.prUpdated[6] = time.Unix(0, 0)
	pulls := &fakePulls{}
	rc, _ := newPRReconciler(l, st, pulls)
	rc.sweep(context.Background())
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{6}) {
		t.Fatalf("reads = %v, want [6]", got)
	}
}

// TestReconcileColdStartSkipsOldClosedPR: with no watermark, a closed PR older
// than the backfill window is not hydrated; an open one and a recent one are.
func TestReconcileColdStartSkipsOldClosedPR(t *testing.T) {
	now := time.Now()
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{{V: forge.UpdatedRows{Pulls: []forge.UpdatedPull{
		{Number: 1, State: "closed", UpdatedAt: now.Add(-40 * 24 * time.Hour)},
		{Number: 2, State: "open", UpdatedAt: now.Add(-90 * 24 * time.Hour)},
		{Number: 3, State: "closed", UpdatedAt: now.Add(-time.Hour)},
	}}}}}
	st := newBoardStore("o/r")
	pulls := &fakePulls{}
	rc, _ := newPRReconciler(l, st, pulls)
	rc.sweep(context.Background())
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{2, 3}) {
		t.Fatalf("reads = %v, want [2 3]", got)
	}
	if len(l.since) != 1 {
		t.Fatalf("lists = %d, want 1 (cold start needs no backfill window walk)", len(l.since))
	}
}

// TestReconcileBackfillStopsOnBudget: a budget error during backfill leaves the
// repo unmarked so the next sweep retries it.
func TestReconcileBackfillStopsOnBudget(t *testing.T) {
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{{NotModified: true}}}
	st := newBoardStore("o/r")
	st.marks["o/r"] = storedMark{mark: ts(50)}
	pulls := &fakePulls{
		open:   []forge.UpdatedPull{{Number: 7, State: "open", UpdatedAt: ts(40)}},
		errFor: map[uint64]error{7: forge.ErrBudgetExhausted},
	}
	rc, _ := newPRReconciler(l, st, pulls)
	if err := rc.reconcileRepo(context.Background(), "o/r"); !errors.Is(err, forge.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want budget exhausted", err)
	}
	if _, ok := st.backfilled["o/r"]; ok {
		t.Fatal("repo marked backfilled after a budget abort")
	}
}

// TestReconcileBackfillRetriesAfterRowFailure: a non-budget hydrate failure
// leaves the repo unmarked, and the next sweep retries only the failed row.
func TestReconcileBackfillRetriesAfterRowFailure(t *testing.T) {
	l := &rowsLister{results: []forge.ConditionalResult[forge.UpdatedRows]{{NotModified: true}}}
	st := newBoardStore("o/r")
	pulls := &fakePulls{
		open:   []forge.UpdatedPull{{Number: 7, State: "open", UpdatedAt: ts(40)}, {Number: 8, State: "open", UpdatedAt: ts(41)}},
		errFor: map[uint64]error{8: errors.New("boom")},
	}
	rc, _ := newPRReconciler(l, st, pulls)
	rc.sweep(context.Background())
	if _, ok := st.backfilled["o/r"]; ok {
		t.Fatal("repo marked backfilled after a failed row")
	}
	st.prUpdated[7] = ts(40)
	pulls.errFor = nil
	rc.sweep(context.Background())
	if got := pulls.readNumbers(); !slices.Equal(got, []uint64{7, 8, 8}) {
		t.Fatalf("reads = %v, want [7 8 8]", got)
	}
	if _, ok := st.backfilled["o/r"]; !ok {
		t.Fatal("repo not marked after the clean retry")
	}
}
