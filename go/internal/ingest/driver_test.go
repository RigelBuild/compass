package ingest

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sealedsecurity/compass/go/internal/forge"
)

// --- fakes -----------------------------------------------------------------

// pageKey identifies one scripted fetch: a (repo, page) coordinate.
type pageKey struct {
	repo string
	page int
}

// pageResult is one scripted fetch outcome — a ListPage or an error.
type pageResult struct {
	page forge.ListPage
	err  error
}

// fakeLister is a scripted pageLister. It records every call in order and
// returns the scripted result for the (repo, page) coordinate; an unscripted
// coordinate returns an empty, no-next page (walk terminates).
type fakeLister struct {
	mu      sync.Mutex
	results map[pageKey]pageResult
	calls   []listCall
}

type listCall struct {
	repo string
	page int
	etag string
}

func newFakeLister() *fakeLister {
	return &fakeLister{results: map[pageKey]pageResult{}}
}

func (f *fakeLister) ListIssuesPage(_ context.Context, repo string, _ forge.IssueFilter, page int, etag string) (forge.ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, listCall{repo, page, etag})
	res, ok := f.results[pageKey{repo, page}]
	if !ok {
		return forge.ListPage{}, nil
	}
	return res.page, res.err
}
func (f *fakeLister) set(repo string, page int, res pageResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[pageKey{repo, page}] = res
}

func (f *fakeLister) callsFor(repo string) []listCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []listCall
	for _, c := range f.calls {
		if c.repo == repo {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeLister) allCalls() []listCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]listCall(nil), f.calls...)
}

// fakePollStore is a scripted PollStore recording every mutation.
type fakePollStore struct {
	mu       sync.Mutex
	repos    []string
	reposErr error
	cursors  map[string][]ListPageCursor
	upserts  []upsertCall
	prunes   []pruneCall
}

type upsertCall struct {
	repo string
	cur  ListPageCursor
}

type pruneCall struct {
	repo    string
	maxPage int
}

func newFakePollStore(repos ...string) *fakePollStore {
	return &fakePollStore{repos: repos, cursors: map[string][]ListPageCursor{}}
}

func (s *fakePollStore) ListEnabledRepos(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reposErr != nil {
		return nil, s.reposErr
	}
	return append([]string(nil), s.repos...), nil
}

func (s *fakePollStore) ListCursor(_ context.Context, repo string) ([]ListPageCursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ListPageCursor(nil), s.cursors[repo]...), nil
}

func (s *fakePollStore) UpsertListCursorPage(_ context.Context, repo string, cur ListPageCursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, upsertCall{repo, cur})
	return nil
}

func (s *fakePollStore) PruneListCursorPages(_ context.Context, repo string, maxPage int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes = append(s.prunes, pruneCall{repo, maxPage})
	return nil
}
func (s *fakePollStore) setRepos(repos ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = repos
}

func (s *fakePollStore) upsertsFor(repo string) []upsertCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []upsertCall
	for _, u := range s.upserts {
		if u.repo == repo {
			out = append(out, u)
		}
	}
	return out
}

func (s *fakePollStore) pruneCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prunes)
}

// capHandler is a slog.Handler capturing every record for field assertions.
type capHandler struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func newCapHandler() (*slog.Logger, func() []slog.Record) {
	var mu sync.Mutex
	recs := []slog.Record{}
	h := capHandler{mu: &mu, recs: &recs}
	get := func() []slog.Record {
		mu.Lock()
		defer mu.Unlock()
		return append([]slog.Record(nil), recs...)
	}
	return slog.New(h), get
}

func (h capHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h capHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, r.Clone())
	return nil
}
func (h capHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capHandler) WithGroup(string) slog.Handler      { return h }

func recField(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, found = a.Value, true
			return false
		}
		return true
	})
	return v, found
}

func findRec(recs []slog.Record, level slog.Level, msgSub string) (slog.Record, bool) {
	for _, r := range recs {
		if r.Level == level && (msgSub == "" || contains(r.Message, msgSub)) {
			return r, true
		}
	}
	return slog.Record{}, false
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// issue builds a raw forge issue with the given number.
func issue(n uint64) forge.Issue {
	return forge.Issue{Number: n, Title: "t", Body: "b", State: "open", UpdatedAt: time.Unix(0, 0)}
}

// newDriverHarness wires a driver over the fakes with a captured logger.
func newDriverHarness(t *testing.T, lister *fakeLister, store *fakePollStore, interval time.Duration) (*Driver, func() []slog.Record) {
	t.Helper()
	log, recs := newCapHandler()
	sink := &recordingSink{}
	ing := NewIngester(&noopReader{}, sink, testForgeRef())
	d := NewDriver(lister, ing, store, DriverConfig{Interval: interval, Log: log})
	return d, recs
}

// noopReader satisfies forgeReader; the driver never calls ListIssues (it fetches
// page-wise through the pageLister), so this is only here to construct Ingester.
type noopReader struct{}

func (noopReader) ListIssues(context.Context, string, forge.IssueFilter) ([]forge.Issue, error) {
	return nil, nil
}

// --- item 1: immediate pass before the first tick --------------------------

func TestRunImmediatePassBeforeFirstTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := newFakeLister()
		store := newFakePollStore("owner/a", "owner/b")
		d, _ := newDriverHarness(t, lister, store, time.Hour) // long interval: only the immediate pass fires

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			_ = d.Run(ctx)
			close(done)
		}()

		synctest.Wait() // immediate pass completes; blocked on the ticker

		if got := len(lister.callsFor("owner/a")); got == 0 {
			t.Fatalf("owner/a not polled in the immediate pass")
		}
		if got := len(lister.callsFor("owner/b")); got == 0 {
			t.Fatalf("owner/b not polled in the immediate pass")
		}
		cancel()
		<-done
	})
}

// --- item 2: ctx cancel between AND during a pass returns nil promptly ------

func TestRunReturnsNilOnCancelBetweenTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := newFakeLister()
		store := newFakePollStore("owner/a")
		d, _ := newDriverHarness(t, lister, store, time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- d.Run(ctx) }()

		synctest.Wait() // immediate pass done, parked on the ticker

		select {
		case <-errc:
			t.Fatalf("Run returned while ctx still live")
		default:
		}

		cancel()
		synctest.Wait()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v, want nil on cancel", err)
		}
	})
}

func TestRunReturnsNilOnCancelDuringPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := newFakeLister()
		// Many repos so the pass is mid-walk when we cancel; the pass loop checks
		// ctx.Err() between repos and returns promptly.
		store := newFakePollStore("r0", "r1", "r2", "r3", "r4")
		d, _ := newDriverHarness(t, lister, store, time.Hour)

		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		cancel() // ctx already dead: the immediate pass bails after the first repo check
		go func() { errc <- d.Run(ctx) }()

		synctest.Wait()
		if err := <-errc; err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	})
}

// --- item 3: conditional walk; 304 walks on stored HasNext; all-304 no-op ---

func TestConditionalWalkAll304SinksNothing(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	store.cursors["owner/a"] = []ListPageCursor{
		{Page: 1, ETag: "e1", HasNext: true},
		{Page: 2, ETag: "e2", HasNext: false},
	}
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true}})
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{NotModified: true}})

	d, _ := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	calls := lister.callsFor("owner/a")
	if len(calls) != 2 {
		t.Fatalf("fetched %d pages, want 2 (304 on page 1 must still walk to page 2)", len(calls))
	}
	if calls[0].etag != "e1" || calls[1].etag != "e2" {
		t.Fatalf("etags = %q,%q, want e1,e2 (stored cursor etags)", calls[0].etag, calls[1].etag)
	}
	if got := len(store.upsertsFor("owner/a")); got != 0 {
		t.Fatalf("all-304 pass upserted %d cursors, want 0", got)
	}
}

// --- item 4: cursor advance gated on sink success --------------------------

func TestCursorAdvanceGatedOnSinkSuccess(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{
		Issues:  []forge.Issue{issue(1), issue(2), issue(3)},
		ETag:    "new-e1",
		HasNext: false,
	}})

	log, _ := newCapHandler()
	// sink fails on the 2nd publish -> page 1 must NOT advance, pass aborts.
	sink := &recordingSink{failOn: 2, failErr: errBoom}
	ing := NewIngester(&noopReader{}, sink, testForgeRef())
	d := NewDriver(lister, ing, store, DriverConfig{Interval: time.Hour, Log: log})

	d.pollRepo(context.Background(), "owner/a")

	if got := len(store.upsertsFor("owner/a")); got != 0 {
		t.Fatalf("mid-page sink failure upserted %d cursors, want 0 (no advance past unsunk content)", got)
	}
	if store.pruneCount() != 0 {
		t.Fatalf("aborted pass pruned; want no prune")
	}

	// Next tick: re-fetch carries the OLD (empty) etag; every issue re-sinks.
	sink.failOn = 0
	d.pollRepo(context.Background(), "owner/a")
	calls := lister.callsFor("owner/a")
	last := calls[len(calls)-1]
	if last.etag != "" {
		t.Fatalf("re-fetch etag = %q, want empty (old etag, cursor never advanced)", last.etag)
	}
	ups := store.upsertsFor("owner/a")
	if len(ups) != 1 || ups[0].cur.ETag != "new-e1" {
		t.Fatalf("after successful re-poll upserts = %+v, want one carrying new-e1", ups)
	}
}

// --- item 5: prune on a shrunk walk; aborted pass never prunes -------------

func TestPruneOnShrunkWalk(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	store.cursors["owner/a"] = []ListPageCursor{
		{Page: 1, ETag: "e1", HasNext: true},
		{Page: 2, ETag: "e2", HasNext: true},
		{Page: 3, ETag: "e3", HasNext: true},
		{Page: 4, ETag: "e4", HasNext: false},
	}
	// Walk now ends at page 2 (page 2 response has no next).
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true}})
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{Issues: []forge.Issue{issue(1)}, ETag: "n2", HasNext: false}})

	d, _ := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	if store.pruneCount() != 1 {
		t.Fatalf("prune count = %d, want 1", store.pruneCount())
	}
	if store.prunes[0].maxPage != 2 {
		t.Fatalf("pruned maxPage = %d, want 2", store.prunes[0].maxPage)
	}
}

func TestAbortedPassNeverPrunes(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{err: errBoom})

	d, _ := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	if store.pruneCount() != 0 {
		t.Fatalf("error-aborted pass pruned; want no prune")
	}
}

// --- item 6: per-repo error isolation --------------------------------------

func TestRepoErrorDoesNotStopOtherRepo(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a", "owner/b")
	lister.set("owner/a", 1, pageResult{err: errBoom})
	lister.set("owner/b", 1, pageResult{page: forge.ListPage{Issues: []forge.Issue{issue(1)}, ETag: "b1", HasNext: false}})

	d, _ := newDriverHarness(t, lister, store, time.Hour)
	d.pass(context.Background())

	if got := len(store.upsertsFor("owner/b")); got != 1 {
		t.Fatalf("owner/b upserts = %d, want 1 (repo A failure must not stop repo B)", got)
	}
}

// --- item 7: budget skip -> Warn, already-advanced pages stay -------------

func TestBudgetSkipWarnsAndKeepsAdvanced(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{Issues: []forge.Issue{issue(1)}, ETag: "a1", HasNext: true}})
	lister.set("owner/a", 2, pageResult{err: forge.ErrBudgetExhausted})

	d, recs := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	if got := len(store.upsertsFor("owner/a")); got != 1 {
		t.Fatalf("page 1 advance lost on budget skip: upserts = %d, want 1", got)
	}
	if store.pruneCount() != 0 {
		t.Fatalf("budget-abandoned pass pruned; want no prune")
	}
	if _, ok := findRec(recs(), slog.LevelWarn, "budget"); !ok {
		t.Fatalf("budget skip did not log at Warn")
	}
	if _, ok := findRec(recs(), slog.LevelError, ""); ok {
		t.Fatalf("budget skip logged at Error; want Warn only")
	}
}

// --- item 8: interval respected — N ticks -> N+1 passes --------------------

func TestIntervalDrivesExpectedPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := newFakeLister()
		store := newFakePollStore("owner/a")
		d, _ := newDriverHarness(t, lister, store, time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_ = d.Run(ctx)
			close(done)
		}()

		synctest.Wait() // immediate pass (pass #1)
		if got := len(lister.callsFor("owner/a")); got != 1 {
			t.Fatalf("after immediate pass, fetches = %d, want 1", got)
		}

		// Three ticks -> three more passes, deterministically on virtual time.
		for i := range 3 {
			time.Sleep(time.Second)
			synctest.Wait()
			if got := len(lister.callsFor("owner/a")); got != i+2 {
				t.Fatalf("after tick %d, fetches = %d, want %d", i+1, got, i+2)
			}
		}

		cancel()
		<-done
	})
}

// --- item 9: log fields on happy / budget / error passes -------------------

func TestLogFieldsHappyPass(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{Issues: []forge.Issue{issue(1), issue(2)}, ETag: "a1", HasNext: false}})

	d, recs := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	r, ok := findRec(recs(), slog.LevelInfo, "")
	if !ok {
		t.Fatalf("happy pass did not log at Info")
	}
	for _, key := range []string{"repo", "issues", "pages", "dur"} {
		if _, ok := recField(r, key); !ok {
			t.Fatalf("Info record missing field %q", key)
		}
	}
	if v, _ := recField(r, "issues"); v.Int64() != 2 {
		t.Fatalf("issues field = %d, want 2", v.Int64())
	}
}

func TestLogFieldsErrorPass(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{err: errBoom})

	d, recs := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	r, ok := findRec(recs(), slog.LevelError, "")
	if !ok {
		t.Fatalf("genuine error did not log at Error")
	}
	if _, ok := recField(r, "err"); !ok {
		t.Fatalf("Error record missing err field")
	}
}

// --- item 10: IngestIssues refactor regression -----------------------------

func TestIngestIssuesStopsAtFirstSinkError(t *testing.T) {
	sink := &recordingSink{failOn: 2, failErr: errBoom}
	in := NewIngester(&noopReader{}, sink, testForgeRef())

	err := in.IngestIssues(context.Background(), "owner/a", []forge.Issue{issue(1), issue(2), issue(3)})
	if err == nil {
		t.Fatalf("IngestIssues returned nil, want wrapped sink error")
	}
	if !contains(err.Error(), "ingest: publish issue #2 for \"owner/a\"") {
		t.Fatalf("error shape = %q, want the wrapped ingest.go:60-62 shape", err.Error())
	}
	if len(sink.got) != 2 {
		t.Fatalf("sinked %d issues, want 2 (stops at the failure)", len(sink.got))
	}
}

func TestIngestIssuesSinksWholeBatch(t *testing.T) {
	sink := &recordingSink{}
	in := NewIngester(&noopReader{}, sink, testForgeRef())

	if err := in.IngestIssues(context.Background(), "owner/a", []forge.Issue{issue(1), issue(2)}); err != nil {
		t.Fatalf("IngestIssues err = %v, want nil", err)
	}
	if len(sink.got) != 2 {
		t.Fatalf("sinked %d, want 2", len(sink.got))
	}
}

// --- item 11: per-pass target enumeration is live --------------------------

func TestPerPassTargetEnumerationLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := newFakeLister()
		store := newFakePollStore("owner/a")
		d, _ := newDriverHarness(t, lister, store, time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_ = d.Run(ctx)
			close(done)
		}()

		synctest.Wait() // immediate pass: only owner/a
		if got := len(lister.callsFor("owner/b")); got != 0 {
			t.Fatalf("owner/b polled before being enabled")
		}

		// Add owner/b, remove owner/a between ticks.
		store.setRepos("owner/b")
		time.Sleep(time.Second)
		synctest.Wait()

		if got := len(lister.callsFor("owner/b")); got == 0 {
			t.Fatalf("owner/b added between ticks was not polled next pass")
		}
		aBefore := len(lister.callsFor("owner/a"))

		// Empty target set -> idle pass, no new fetches, no Error log.
		store.setRepos()
		time.Sleep(time.Second)
		synctest.Wait()
		if got := len(lister.callsFor("owner/a")); got != aBefore {
			t.Fatalf("owner/a polled after removal (%d -> %d)", aBefore, got)
		}

		cancel()
		<-done
	})
}

func TestZeroTargetsIdlePass(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore() // no repos
	d, recs := newDriverHarness(t, lister, store, time.Hour)

	d.pass(context.Background())

	if got := len(lister.allCalls()); got != 0 {
		t.Fatalf("zero-target pass fetched %d pages, want 0", got)
	}
	if _, ok := findRec(recs(), slog.LevelError, ""); ok {
		t.Fatalf("zero-target pass logged at Error; want idle")
	}
}

// --- item 12: ListEnabledRepos error -> Error, pass skipped, keeps ticking --

func TestListEnabledReposErrorSkipsPassKeepsTicking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := newFakeLister()
		store := newFakePollStore("owner/a")
		store.reposErr = errBoom
		d, recs := newDriverHarness(t, lister, store, time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_ = d.Run(ctx)
			close(done)
		}()

		synctest.Wait() // immediate pass: enumeration errored
		if _, ok := findRec(recs(), slog.LevelError, "enabled repos"); !ok {
			t.Fatalf("enumeration error did not log at Error")
		}
		if got := len(lister.allCalls()); got != 0 {
			t.Fatalf("skipped pass still fetched %d pages", got)
		}

		// Enumeration recovers: next tick re-enumerates and polls.
		store.mu.Lock()
		store.reposErr = nil
		store.mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if got := len(lister.callsFor("owner/a")); got == 0 {
			t.Fatalf("Run stopped ticking after an enumeration error")
		}

		cancel()
		<-done
	})
}
