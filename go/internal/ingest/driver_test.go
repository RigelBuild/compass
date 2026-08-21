package ingest

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
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
	rows := s.cursors[repo]
	for i, r := range rows {
		if r.Page == cur.Page {
			rows[i] = cur // persist: overwrite the existing page row
			return nil
		}
	}
	s.cursors[repo] = append(rows, cur) // persist: insert a new page row
	return nil
}

func (s *fakePollStore) PruneListCursorPages(_ context.Context, repo string, maxPage int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes = append(s.prunes, pruneCall{repo, maxPage})
	rows := s.cursors[repo]
	kept := make([]ListPageCursor, 0, len(rows))
	for _, r := range rows {
		if r.Page <= maxPage { // persist: drop rows past the walked tail
			kept = append(kept, r)
		}
	}
	s.cursors[repo] = kept
	return nil
}

// cursor returns the persisted cursor row for a page (zero value if absent).
func (s *fakePollStore) cursor(repo string, page int) ListPageCursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.cursors[repo] {
		if r.Page == page {
			return r
		}
	}
	return ListPageCursor{}
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
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{
		Issues:             []forge.Issue{issue(1), issue(2)},
		ETag:               "a1",
		HasNext:            false,
		RateLimitRemaining: 4321,
	}})

	d, recs := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	r, ok := findRec(recs(), slog.LevelInfo, "")
	if !ok {
		t.Fatalf("happy pass did not log at Info")
	}
	for _, key := range []string{"repo", "issues", "pages", "not_modified", "dur", "ratelimit_remaining"} {
		if _, ok := recField(r, key); !ok {
			t.Fatalf("Info record missing field %q", key)
		}
	}
	if v, _ := recField(r, "issues"); v.Int64() != 2 {
		t.Fatalf("issues field = %d, want 2", v.Int64())
	}
	if v, _ := recField(r, "not_modified"); v.Bool() {
		t.Fatalf("not_modified = true on a 200-advancing pass, want false")
	}
	if v, _ := recField(r, "ratelimit_remaining"); v.Int64() != 4321 {
		t.Fatalf("ratelimit_remaining = %d, want 4321 (last observed header)", v.Int64())
	}
}

// An all-304 walk logs not_modified=true and the last 304's ratelimit_remaining.
func TestLogFieldsAll304Pass(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	store.cursors["owner/a"] = []ListPageCursor{{Page: 1, ETag: "e1", HasNext: false}}
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true, RateLimitRemaining: 77}})

	d, recs := newDriverHarness(t, lister, store, time.Hour)
	d.pollRepo(context.Background(), "owner/a")

	r, ok := findRec(recs(), slog.LevelInfo, "")
	if !ok {
		t.Fatalf("all-304 pass did not log at Info")
	}
	if v, _ := recField(r, "not_modified"); !v.Bool() {
		t.Fatalf("not_modified = false on an all-304 pass, want true")
	}
	if v, _ := recField(r, "ratelimit_remaining"); v.Int64() != 77 {
		t.Fatalf("ratelimit_remaining = %d, want 77", v.Int64())
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

// --- item 13: M1 boundary probe + zero-issue upsert-skip --------------------

// probePageFetches returns the recorded fetches of the probe page (lastPage+1 =
// page 2 in these single-stored-page scenarios), across walks.
func probePageFetches(calls []listCall) []listCall {
	var out []listCall
	for _, c := range calls {
		if c.page == 2 {
			out = append(out, c)
		}
	}
	return out
}

// probePageUpserts returns the cursor upserts targeting the probe page (page 2).
func probePageUpserts(ups []upsertCall) []upsertCall {
	var out []upsertCall
	for _, u := range ups {
		if u.cur.Page == 2 {
			out = append(out, u)
		}
	}
	return out
}

// newProbeDriver builds a driver with a custom M1 probe cadence over the fakes.
func newProbeDriver(t *testing.T, lister *fakeLister, store *fakePollStore, sink *recordingSink, probeEvery int) *Driver {
	t.Helper()
	log, _ := newCapHandler()
	ing := NewIngester(&noopReader{}, sink, testForgeRef())
	return NewDriver(lister, ing, store, DriverConfig{Interval: time.Hour, Log: log, ProbeEveryAll304: probeEvery})
}

// A content probe on the Nth all-304 walk DURABLY re-anchors the tail: page
// lastPage+1 is fetched unconditionally, its issues sink, its cursor upserts,
// AND the stored anchor page is promoted to HasNext=true so the next
// conditional walk threads the 304 chain THROUGH the grown tail instead of
// pruning it and re-probing forever (design.md:555-556).
func TestBoundaryProbeContentReanchors(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	store.cursors["owner/a"] = []ListPageCursor{{Page: 1, ETag: "e1", HasNext: false}}
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true}})
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{
		Issues:  []forge.Issue{issue(9)},
		ETag:    "n2",
		HasNext: false,
	}})
	d := newProbeDriver(t, lister, store, &recordingSink{}, 2)

	// Walk 1: all-304, streak = 1 (< 2) -> NO probe of page 2.
	d.pollRepo(context.Background(), "owner/a")
	if got := len(probePageFetches(lister.callsFor("owner/a"))); got != 0 {
		t.Fatalf("probe fired on the 1st all-304 walk: page-2 fetches = %d, want 0", got)
	}

	// Walk 2: all-304, streak hits 2 -> ONE unconditional probe of page 2 that
	// re-anchors the tail.
	d.pollRepo(context.Background(), "owner/a")
	p2 := probePageFetches(lister.callsFor("owner/a"))
	if len(p2) != 1 {
		t.Fatalf("Nth all-304 walk: page-2 fetches = %d, want 1 (the probe)", len(p2))
	}
	if p2[0].etag != "" {
		t.Fatalf("probe etag = %q, want empty (unconditional GET)", p2[0].etag)
	}
	if ups := probePageUpserts(store.upsertsFor("owner/a")); len(ups) != 1 || ups[0].cur.ETag != "n2" {
		t.Fatalf("content probe upserts for page 2 = %+v, want one carrying n2 (re-anchor)", ups)
	}
	// THE durable re-anchor: the anchor page (served 304 this walk) is promoted
	// to HasNext=true so its stored ETag still threads the chain to page 2.
	if a := store.cursor("owner/a", 1); !a.HasNext || a.ETag != "e1" {
		t.Fatalf("anchor page-1 cursor = %+v, want {e1, HasNext:true} (durable re-anchor)", a)
	}

	// The tail has already been sunk; steady state is a 304 on the grown page.
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{NotModified: true}})

	// Walk 3: the promoted anchor threads the 304 chain THROUGH page 2 — page 2
	// is re-fetched CONDITIONALLY (its stored etag), never re-probed
	// unconditionally, and its cursor survives the post-walk prune.
	d.pollRepo(context.Background(), "owner/a")
	p2 = probePageFetches(lister.callsFor("owner/a"))
	if len(p2) != 2 {
		t.Fatalf("page-2 fetches after walk 3 = %d, want 2 (probe + durable conditional walk)", len(p2))
	}
	if p2[1].etag != "n2" {
		t.Fatalf("walk-3 page-2 fetch etag = %q, want %q (conditional durable thread, not a re-probe)", p2[1].etag, "n2")
	}
	if got := store.cursor("owner/a", 2); got.Page != 2 {
		t.Fatalf("page-2 cursor pruned after walk 3 (got %+v); the re-anchor must keep it durable", got)
	}
}

// An empty probe page (zero issues) writes NO cursor row — the zero-issue skip
// keeps it from persisting an empty tail that would 304 forever.
func TestBoundaryProbeEmptyWritesNoCursor(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	store.cursors["owner/a"] = []ListPageCursor{{Page: 1, ETag: "e1", HasNext: false}}
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true}})
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{HasNext: false}}) // empty 200
	d := newProbeDriver(t, lister, store, &recordingSink{}, 2)

	d.pollRepo(context.Background(), "owner/a") // walk 1: streak 1
	d.pollRepo(context.Background(), "owner/a") // walk 2: probe page 2

	if got := len(probePageFetches(lister.callsFor("owner/a"))); got != 1 {
		t.Fatalf("page-2 probe fetches = %d, want exactly 1", got)
	}
	if got := len(probePageUpserts(store.upsertsFor("owner/a"))); got != 0 {
		t.Fatalf("empty probe wrote %d cursor rows for page 2, want 0", got)
	}
}

// A plain 200 page with zero issues sinks nothing and upserts no cursor row.
func TestZeroIssuePageSkipsUpsert(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{HasNext: false}}) // 200, zero issues
	d, _ := newDriverHarness(t, lister, store, time.Hour)

	d.pollRepo(context.Background(), "owner/a")

	if got := len(store.upsertsFor("owner/a")); got != 0 {
		t.Fatalf("zero-issue 200 upserted %d cursor rows, want 0", got)
	}
}

// Any content advance resets the per-repo all-304 streak: a probe that would
// have fired without the reset does not.
func TestContentAdvanceResetsStreak(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	store.cursors["owner/a"] = []ListPageCursor{{Page: 1, ETag: "e1", HasNext: false}}
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true}})
	// page 2 scripted with content so a probe, if it fired, would be observable.
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{
		Issues: []forge.Issue{issue(9)}, ETag: "n2", HasNext: false,
	}})
	d := newProbeDriver(t, lister, store, &recordingSink{}, 3)

	d.pollRepo(context.Background(), "owner/a") // streak 1
	d.pollRepo(context.Background(), "owner/a") // streak 2
	// Content advance on page 1 -> streak resets to 0.
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{
		Issues: []forge.Issue{issue(1)}, ETag: "m1", HasNext: false,
	}})
	d.pollRepo(context.Background(), "owner/a") // advanced -> reset
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{NotModified: true}})
	d.pollRepo(context.Background(), "owner/a") // streak 1 (not 3)
	d.pollRepo(context.Background(), "owner/a") // streak 2 (not 4)

	if got := len(probePageFetches(lister.callsFor("owner/a"))); got != 0 {
		t.Fatalf("streak not reset by content advance: page-2 probe fired (%d), want 0", got)
	}
}

// --- LOW-2: a true mid-walk cancel returns nil with no spurious Error --------

// blockingLister blocks every fetch until ctx is cancelled, then returns
// ctx.Err(). It closes `reached` on the first call so a test can cancel exactly
// while the walk is in flight.
type blockingLister struct {
	reached chan struct{}
	once    sync.Once
}

func (b *blockingLister) ListIssuesPage(ctx context.Context, _ string, _ forge.IssueFilter, _ int, _ string) (forge.ListPage, error) {
	b.once.Do(func() { close(b.reached) })
	<-ctx.Done()
	return forge.ListPage{}, ctx.Err()
}

func TestRunReturnsNilOnMidWalkCancel(t *testing.T) {
	lister := &blockingLister{reached: make(chan struct{})}
	store := newFakePollStore("owner/a")
	log, recs := newCapHandler()
	ing := NewIngester(&noopReader{}, &recordingSink{}, testForgeRef())
	d := NewDriver(lister, ing, store, DriverConfig{Interval: time.Hour, Log: log})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()

	<-lister.reached // the walk is blocked in-flight on the fetch
	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on mid-walk cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return promptly after mid-walk cancel")
	}
	if _, ok := findRec(recs(), slog.LevelError, ""); ok {
		t.Fatalf("mid-walk cancel logged at Error; want a clean return (LOW-1)")
	}
}

// --- LOW-3: a mid-page sink failure aborts the whole walk, not just the page --

func TestMultiPageSinkAbortStopsWalk(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore("owner/a")
	lister.set("owner/a", 1, pageResult{page: forge.ListPage{
		Issues:  []forge.Issue{issue(1), issue(2), issue(3)},
		ETag:    "n1",
		HasNext: true, // there IS a page 2 to walk to
	}})
	lister.set("owner/a", 2, pageResult{page: forge.ListPage{
		Issues: []forge.Issue{issue(4)}, ETag: "n2", HasNext: false,
	}})

	log, _ := newCapHandler()
	sink := &recordingSink{failOn: 2, failErr: errBoom} // fails mid page-1 batch
	ing := NewIngester(&noopReader{}, sink, testForgeRef())
	d := NewDriver(lister, ing, store, DriverConfig{Interval: time.Hour, Log: log})

	d.pollRepo(context.Background(), "owner/a")

	calls := lister.callsFor("owner/a")
	if len(calls) != 1 || calls[0].page != 1 {
		t.Fatalf("fetched %+v, want only page 1 (mid-page sink abort stops the whole walk)", calls)
	}
	if got := len(store.upsertsFor("owner/a")); got != 0 {
		t.Fatalf("sink-aborted walk upserted %d cursors, want 0", got)
	}
	if store.pruneCount() != 0 {
		t.Fatalf("sink-aborted walk pruned; want no prune")
	}
}

// --- LOW-4: NewDriver guards a non-positive Interval so Run cannot panic ------

func TestNewDriverZeroIntervalDoesNotPanic(t *testing.T) {
	lister := newFakeLister()
	store := newFakePollStore() // no repos: the immediate pass is idle
	log, _ := newCapHandler()
	ing := NewIngester(&noopReader{}, &recordingSink{}, testForgeRef())
	d := NewDriver(lister, ing, store, DriverConfig{Interval: 0, Log: log})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead: immediate pass runs, then Run returns at the ticker select
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run err = %v, want nil (zero Interval must default, not panic)", err)
	}
}
