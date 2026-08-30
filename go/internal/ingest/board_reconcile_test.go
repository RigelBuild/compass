package ingest

// Board reconciliation-sweep acceptance (RIG-2883 T3, design.md:335-403). Fakes
// for the updatedLister + BoardStore seams, over a REAL Ingester with a
// recording sink. context.Background() here is the test root — the sanctioned
// F-ttsr exemption (mirrors notify_reconcile_test.go). Time-dependent behavior
// (immediate startup sweep, Backstop ticker) runs under testing/synctest so the
// virtual clock advances deterministically without a real sleep.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
)

// fakeUpdatedLister scripts ListUpdatedIssues. results is consumed one per call (so a
// test can script a per-sweep sequence); once exhausted it returns the last
// entry. err (when set) is returned on every call.
type fakeUpdatedLister struct {
	results []forge.ConditionalResult[[]forge.Issue]
	err     error
	// calls is read from the test goroutine while the sweep goroutine writes it
	// (Run drives sweeps concurrently), so it is atomic — synctest.Wait gives no
	// happens-before edge on a plain int, and -race flags the unsynchronized access.
	calls atomic.Int64
}

func (l *fakeUpdatedLister) ListUpdatedIssues(_ context.Context, _ string, _ time.Time, _ string) (forge.ConditionalResult[[]forge.Issue], error) {
	n := l.calls.Add(1)
	if l.err != nil {
		return forge.ConditionalResult[[]forge.Issue]{}, l.err
	}
	if len(l.results) == 0 {
		return forge.ConditionalResult[[]forge.Issue]{NotModified: true}, nil
	}
	i := int(n) - 1
	if i >= len(l.results) {
		i = len(l.results) - 1
	}
	return l.results[i], nil
}

// storedMark is one repo's persisted watermark row.
type storedMark struct {
	mark time.Time
	etag string
}

// fakeBoardStore is the in-memory BoardStore. loadErr / storeErr script seam
// failures; storeCalls records every StoreRepoWatermark invocation in order.
type fakeBoardStore struct {
	repos      []string
	marks      map[string]storedMark
	loadErr    error
	storeErr   error
	storeCalls []storedMark
}

func newBoardStore(repos ...string) *fakeBoardStore {
	return &fakeBoardStore{repos: repos, marks: map[string]storedMark{}}
}

func (s *fakeBoardStore) ListEnabledRepos(_ context.Context) ([]string, error) {
	return s.repos, nil
}

func (s *fakeBoardStore) LoadRepoWatermark(_ context.Context, repo string) (time.Time, string, error) {
	if s.loadErr != nil {
		return time.Time{}, "", s.loadErr
	}
	m := s.marks[repo]
	return m.mark, m.etag, nil
}

func (s *fakeBoardStore) StoreRepoWatermark(_ context.Context, repo string, mark time.Time, etag string) error {
	if s.storeErr != nil {
		return s.storeErr
	}
	s.marks[repo] = storedMark{mark: mark, etag: etag}
	s.storeCalls = append(s.storeCalls, storedMark{mark: mark, etag: etag})
	return nil
}

// newBoardHarness wires a fake lister + a real Ingester (recording sink) + a
// fake store into a BoardReconciler with pacing disabled (no real sleeps).
func newBoardHarness(t *testing.T, l *fakeUpdatedLister, st *fakeBoardStore, sink *recordingSink) *BoardReconciler {
	t.Helper()
	in := NewIngester(nil, sink, testForgeRef())
	return NewBoardReconciler(l, in, st, BoardReconcileConfig{Pace: -1})
}

func ts(sec int) time.Time {
	return time.Date(2026, 8, 1, 12, 0, sec, 0, time.UTC)
}

// TestBoardSweepSinksAndAdvancesWatermark: a repo with two updated rows sinks
// both, then stores max(UpdatedAt) + the fresh list ETag.
func TestBoardSweepSinksAndAdvancesWatermark(t *testing.T) {
	l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{
		V: []forge.Issue{
			{Number: 7, UpdatedAt: ts(10)},
			{Number: 8, UpdatedAt: ts(30)},
		},
		ETag: `"e1"`,
	}}}
	st := newBoardStore("o/r")
	sink := &recordingSink{}
	newBoardHarness(t, l, st, sink).sweep(context.Background())

	if len(sink.got) != 2 {
		t.Fatalf("sank %d issues, want 2", len(sink.got))
	}
	got := st.marks["o/r"]
	if !got.mark.Equal(ts(30)) {
		t.Errorf("watermark = %v, want %v (max UpdatedAt)", got.mark, ts(30))
	}
	if got.etag != `"e1"` {
		t.Errorf("stored etag = %q, want %q (fresh list ETag)", got.etag, `"e1"`)
	}
}

// TestBoardSweep304NoSink: a NotModified list costs no sink and leaves the
// watermark untouched (the stored ETag stays the truth).
func TestBoardSweep304NoSink(t *testing.T) {
	l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{NotModified: true}}}
	st := newBoardStore("o/r")
	st.marks["o/r"] = storedMark{mark: ts(5), etag: `"e0"`}
	sink := &recordingSink{}
	newBoardHarness(t, l, st, sink).sweep(context.Background())

	if len(sink.got) != 0 {
		t.Fatalf("sank %d issues, want 0 on a 304", len(sink.got))
	}
	if len(st.storeCalls) != 0 {
		t.Fatalf("stored watermark %d times, want 0 on a 304", len(st.storeCalls))
	}
	if got := st.marks["o/r"]; !got.mark.Equal(ts(5)) || got.etag != `"e0"` {
		t.Errorf("watermark mutated on a 304: %+v", got)
	}
}

// TestBoardSweepColdStartFullWalk: a zero/absent watermark lists everything
// (cold-start / reinstall backfill) — the store is queried with a zero since and
// every returned row sinks.
func TestBoardSweepColdStartFullWalk(t *testing.T) {
	l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{
		V:    []forge.Issue{{Number: 1, UpdatedAt: ts(10)}, {Number: 2, UpdatedAt: ts(20)}},
		ETag: `"e1"`,
	}}}
	st := newBoardStore("o/r") // no stored mark -> zero watermark
	sink := &recordingSink{}
	newBoardHarness(t, l, st, sink).sweep(context.Background())

	if len(sink.got) != 2 {
		t.Fatalf("cold-start sank %d, want 2 (full walk)", len(sink.got))
	}
	if got := st.marks["o/r"]; !got.mark.Equal(ts(20)) {
		t.Errorf("watermark = %v, want %v", got.mark, ts(20))
	}
}

// TestBoardSweepSinkFailureDoesNotAdvance: when the ONLY row's sink fails, the
// watermark does not advance, so the row is re-listed next sweep.
func TestBoardSweepSinkFailureDoesNotAdvance(t *testing.T) {
	l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{
		V:    []forge.Issue{{Number: 7, UpdatedAt: ts(10)}},
		ETag: `"e1"`,
	}}}
	st := newBoardStore("o/r")
	sink := &recordingSink{failOn: 1, failErr: errors.New("boom")}
	newBoardHarness(t, l, st, sink).sweep(context.Background())

	if len(st.storeCalls) != 0 {
		t.Fatalf("stored watermark %d times, want 0 (nothing sank)", len(st.storeCalls))
	}
	if got := st.marks["o/r"]; !got.mark.IsZero() {
		t.Errorf("watermark advanced to %v on a total sink failure, want zero", got.mark)
	}
}

// TestBoardSweepPoisonRowBounded: a persistently-failing row does NOT pin the
// watermark. The poison row is skipped, the healthy rows sink, and the watermark
// advances PAST the healthy rows — so the re-walk window stays bounded rather
// than growing every sweep.
func TestBoardSweepPoisonRowBounded(t *testing.T) {
	// Row #7 (ts 10) is poison — its sink fails every time. Rows #8/#9 are
	// healthy. The recording sink fails only on the poison issue number.
	sink := &poisonSink{poison: 7}
	l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{
		V: []forge.Issue{
			{Number: 7, UpdatedAt: ts(10)}, // poison
			{Number: 8, UpdatedAt: ts(20)},
			{Number: 9, UpdatedAt: ts(30)},
		},
		ETag: `"e1"`,
	}}}
	st := newBoardStore("o/r")
	in := NewIngester(nil, sink, testForgeRef())
	NewBoardReconciler(l, in, st, BoardReconcileConfig{Pace: -1}).sweep(context.Background())

	// The healthy rows sank; the watermark advanced past them (to ts 30),
	// NOT pinned at the poison row's ts 10.
	got := st.marks["o/r"]
	if got.mark.IsZero() {
		t.Fatal("watermark pinned at zero — poison row livelocked the sweep")
	}
	if !got.mark.Equal(ts(30)) {
		t.Errorf("watermark = %v, want %v (advanced past healthy rows)", got.mark, ts(30))
	}
	// A poison sweep drops the ETag so the next sweep re-lists unconditionally
	// (bounded to rows at/after the watermark), rather than 304-suppressing the
	// retry.
	if got.etag != "" {
		t.Errorf("stored etag = %q, want empty (poison forces an unconditional re-list)", got.etag)
	}
}

// poisonSink sinks every issue except the poison number, which fails on every
// call — modeling a persistently-rejected row. attempts counts how many times
// the poison number was attempted, so a test can prove a persistently-failing
// row keeps being retried across sweeps rather than being abandoned.
type poisonSink struct {
	got      []*compassv1.Issue
	poison   uint32
	attempts int
}

func (s *poisonSink) PublishIssueUpdate(_ context.Context, issue *compassv1.Issue) error {
	if issue.GetNumber() == s.poison {
		s.attempts++
		return errors.New("poison row: persistent store validation error")
	}
	s.got = append(s.got, issue)
	return nil
}

// sinceAwareLister returns, on each call, the subset of a fixed issue set whose
// UpdatedAt is NOT strictly below since (mirroring ListUpdatedIssues' strictly-<
// since stop with == since re-included), so a multi-sweep test sees the re-walk
// window actually shrink to the rows at/after the stored watermark. lastWindow
// records the size of the most recent returned window.
type sinceAwareLister struct {
	all        []forge.Issue
	etag       string
	calls      atomic.Int64
	lastWindow atomic.Int64
}

func (l *sinceAwareLister) ListUpdatedIssues(_ context.Context, _ string, since time.Time, _ string) (forge.ConditionalResult[[]forge.Issue], error) {
	l.calls.Add(1)
	var out []forge.Issue
	for _, iss := range l.all {
		if iss.UpdatedAt.Before(since) {
			continue // strictly older than the watermark — excluded
		}
		out = append(out, iss)
	}
	l.lastWindow.Store(int64(len(out)))
	return forge.ConditionalResult[[]forge.Issue]{V: out, ETag: l.etag}, nil
}

// TestBoardSweepPoisonNewestBoundedAcrossSweeps: a persistently-failing row that
// is the NEWEST in the list stays above the advanced watermark, so it is
// re-listed and retried EVERY sweep — but the re-walk window stays BOUNDED (it
// does not grow sweep over sweep) and the watermark floor holds at the healthy
// max. This is the multi-sweep invariant the single-sweep
// TestBoardSweepPoisonRowBounded only implies, over the poison-is-newest variant.
func TestBoardSweepPoisonNewestBoundedAcrossSweeps(t *testing.T) {
	// Row #9 (ts 30) is poison AND the newest; #7/#8 (ts 10/20) are healthy.
	sink := &poisonSink{poison: 9}
	l := &sinceAwareLister{
		all: []forge.Issue{
			{Number: 7, UpdatedAt: ts(10)},
			{Number: 8, UpdatedAt: ts(20)},
			{Number: 9, UpdatedAt: ts(30)}, // poison, newest
		},
		etag: `"e1"`,
	}
	st := newBoardStore("o/r")
	in := NewIngester(nil, sink, testForgeRef())
	rc := NewBoardReconciler(l, in, st, BoardReconcileConfig{Pace: -1})
	// Sweep 1: cold start lists all 3; #7/#8 sink, #9 fails. The watermark
	// advances to the healthy max (ts 20), NOT to the poison's ts 30.
	rc.sweep(context.Background())
	if got := st.marks["o/r"]; !got.mark.Equal(ts(20)) {
		t.Fatalf("after sweep 1: watermark = %v, want %v (healthy max, below the poison)", got.mark, ts(20))
	}
	window1 := l.lastWindow.Load()

	// Sweep 2: lists since=ts20 → rows at/after 20 (#8 re-included at == since,
	// #9). #8 re-sinks, #9 fails again. The watermark holds at ts 20 (no regress,
	// never advances past the poison), and the re-walk window did NOT grow.
	rc.sweep(context.Background())
	if got := st.marks["o/r"]; !got.mark.Equal(ts(20)) {
		t.Fatalf("after sweep 2: watermark = %v, want %v (floor holds; poison never advances it)", got.mark, ts(20))
	}
	window2 := l.lastWindow.Load()
	if window2 > window1 {
		t.Errorf("re-walk window grew %d -> %d across sweeps; the bound must not grow behind the poison", window1, window2)
	}
	if window2 != 2 {
		t.Errorf("sweep 2 window = %d, want 2 (rows at/after the ts20 watermark: #8 re-included, #9 poison)", window2)
	}
	// The poison was attempted on BOTH sweeps — retried every sweep, not abandoned.
	if sink.attempts != 2 {
		t.Errorf("poison attempts = %d, want 2 (persistently retried within the bounded window)", sink.attempts)
	}
}

// TestBoardSweepBudgetAbortStopsSweep: a repo whose list returns
// ErrBudgetExhausted aborts the sweep — a later repo is NOT listed (resumed next
// interval).
func TestBoardSweepBudgetAbortStopsSweep(t *testing.T) {
	l := &fakeUpdatedLister{err: forge.ErrBudgetExhausted}
	st := newBoardStore("o/r", "o/r2")
	sink := &recordingSink{}
	newBoardHarness(t, l, st, sink).sweep(context.Background())

	if l.calls.Load() != 1 {
		t.Errorf("lister calls = %d, want 1 (sweep aborted after the first budget-exhausted repo)", l.calls.Load())
	}
	if len(st.storeCalls) != 0 {
		t.Errorf("stored watermark %d times, want 0 (budget abort before any write)", len(st.storeCalls))
	}
}

// TestBoardSweepPerRepoErrorIsolation: a repo whose list errors (non-budget) is
// logged and skipped; the sweep continues to the next repo, which sinks.
func TestBoardSweepPerRepoErrorIsolation(t *testing.T) {
	l := &perRepoLister{
		errRepos: map[string]error{"o/bad": errors.New("boom")},
		results: map[string]forge.ConditionalResult[[]forge.Issue]{
			"o/good": {V: []forge.Issue{{Number: 1, UpdatedAt: ts(10)}}, ETag: `"e1"`},
		},
	}
	st := newBoardStore("o/bad", "o/good")
	sink := &recordingSink{}
	in := NewIngester(nil, sink, testForgeRef())
	NewBoardReconciler(l, in, st, BoardReconcileConfig{Pace: -1}).sweep(context.Background())

	if len(sink.got) != 1 {
		t.Fatalf("sank %d, want 1 (o/good after o/bad isolated)", len(sink.got))
	}
	if _, ok := st.marks["o/good"]; !ok {
		t.Error("o/good watermark not stored after o/bad was isolated")
	}
}

// perRepoLister scripts a distinct list result / error per repo.
type perRepoLister struct {
	errRepos map[string]error
	results  map[string]forge.ConditionalResult[[]forge.Issue]
}

func (l *perRepoLister) ListUpdatedIssues(_ context.Context, repo string, _ time.Time, _ string) (forge.ConditionalResult[[]forge.Issue], error) {
	if err, ok := l.errRepos[repo]; ok {
		return forge.ConditionalResult[[]forge.Issue]{}, err
	}
	if res, ok := l.results[repo]; ok {
		return res, nil
	}
	return forge.ConditionalResult[[]forge.Issue]{NotModified: true}, nil
}

// TestBoardRunStartupSweepFiresImmediately: Run performs one immediate sweep at
// startup (before any tick), then blocks on the ticker.
func TestBoardRunStartupSweepFiresImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{
			V: []forge.Issue{{Number: 7, UpdatedAt: ts(10)}}, ETag: `"e1"`,
		}}}
		st := newBoardStore("o/r")
		sink := &recordingSink{}
		rc := NewBoardReconciler(l, NewIngester(nil, sink, testForgeRef()), st,
			BoardReconcileConfig{Backstop: time.Hour, Pace: -1})
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- rc.Run(ctx) }()

		synctest.Wait() // immediate sweep completes; blocked on the ticker
		if l.calls.Load() != 1 {
			t.Errorf("lister calls after startup = %d, want 1 (immediate sweep)", l.calls.Load())
		}
		if len(sink.got) != 1 {
			t.Errorf("sank %d after startup, want 1", len(sink.got))
		}
		cancel()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v, want nil (clean shutdown)", err)
		}
	})
}

// TestBoardRunTickerSweepsAtBackstop: after the immediate sweep, the ticker
// fires another sweep at the Backstop cadence.
func TestBoardRunTickerSweepsAtBackstop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{NotModified: true}}}
		st := newBoardStore("o/r")
		sink := &recordingSink{}
		rc := NewBoardReconciler(l, NewIngester(nil, sink, testForgeRef()), st,
			BoardReconcileConfig{Backstop: 30 * time.Minute, Pace: -1})
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- rc.Run(ctx) }()

		synctest.Wait()
		if l.calls.Load() != 1 {
			t.Fatalf("lister calls after startup = %d, want 1", l.calls.Load())
		}
		time.Sleep(30 * time.Minute) //nolint:forbidigo // synctest virtual clock: advances the fake clock to the first tick, not a real wall-clock wait (rule://go-no-sleep-in-test synctest exemption)
		synctest.Wait()
		if l.calls.Load() != 2 {
			t.Errorf("lister calls after one Backstop tick = %d, want 2", l.calls.Load())
		}
		cancel()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	})
}

// TestBoardRunCtxCancelReturnsNil: Run returns nil on ctx cancel (clean
// shutdown).
func TestBoardRunCtxCancelReturnsNil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{NotModified: true}}}
		st := newBoardStore("o/r")
		rc := NewBoardReconciler(l, NewIngester(nil, &recordingSink{}, testForgeRef()), st,
			BoardReconcileConfig{Backstop: time.Hour, Pace: -1})
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- rc.Run(ctx) }()

		synctest.Wait()
		cancel()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v, want nil on ctx cancel", err)
		}
	})
}

// TestBoardSweepCtxCancelNoList: a context cancelled before the sweep returns
// promptly without listing.
func TestBoardSweepCtxCancelNoList(t *testing.T) {
	l := &fakeUpdatedLister{}
	st := newBoardStore("o/r")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newBoardHarness(t, l, st, &recordingSink{}).sweep(ctx)
	if l.calls.Load() != 0 {
		t.Errorf("lister calls = %d, want 0 (cancelled before sweep)", l.calls.Load())
	}
}
