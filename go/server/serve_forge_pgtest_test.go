//go:build pgtest && unix

package server

// Store-gated end-to-end proofs for the SEA-1810 forge boot wiring: the pieces
// buildForgeDriver assembles (the forgePollStore adapter over a real *store.Store,
// the shared IssueProjection sink, ingest.NewDriver) driven over a FAKE pager
// against a REAL Postgres — no live GitHub. Behind `pgtest && unix` (SKIP when no
// runtime). Each test opens its own isolated-schema store (pgtest.RequireDSN +
// store.Open), so parallel packages never collide.
//
// The four store-backed record test-cycle items live here:
//   - test 3: end-to-end boot-wiring + durable-cursor (issue sinks to the
//     projection, lands in the store, leaves a forge_list_cursors page-1 row with
//     the scripted ETag), with a DIRECT ordering assertion (the seeded row is
//     visible at the pager's first call);
//   - test 4: restart-resync (a fresh pipeline over the same schema scripting a
//     304 for the stored ETag issues no unconditional fetch and re-sinks nothing);
//   - test 5: the no-clobber contract's runtime proof (a human-set State survives
//     a forge re-ingest; MUST be able to go RED against state = EXCLUDED.state);
//   - test 8: the seed-reconcile semantic (bootstrap-only insert, additive, never
//     re-enabling; MUST be able to go RED against destructive-sync / DO-UPDATE).
// The polling-disabled Warn (test 2's store-backed leg) is proven here too.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/forge"
	"github.com/sealedsecurity/compass/go/internal/ingest"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/secrets"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// --- fakes -----------------------------------------------------------------

const forgeTestHost = "github.com"

// scriptedPage is one scripted fetch outcome for the fake pager.
type scriptedPage struct {
	page forge.ListPage
	err  error
}

// fakePager is a scripted ingest pageLister (satisfies the driver's fetch seam
// structurally). It records every call and, on the FIRST call, snapshots the
// enabled repo subscriptions visible in the store at that instant — the direct
// ordering proof that the seed reconcile ran BEFORE the first pass.
type fakePager struct {
	st   *store.Store
	host string

	mu       sync.Mutex
	results  map[pageCoord]scriptedPage
	calls    []pagerCall
	firstSub []string // enabled repos snapshotted at the first ListIssuesPage call
}

type pageCoord struct {
	repo string
	page int
}

type pagerCall struct {
	repo string
	page int
	etag string
}

func newFakePager(st *store.Store) *fakePager {
	return &fakePager{st: st, host: forgeTestHost, results: map[pageCoord]scriptedPage{}}
}

func (p *fakePager) ListIssuesPage(ctx context.Context, repo string, _ forge.IssueFilter, page int, etag string) (forge.ListPage, error) {
	p.mu.Lock()
	first := len(p.calls) == 0
	p.calls = append(p.calls, pagerCall{repo, page, etag})
	p.mu.Unlock()

	if first {
		// Snapshot the enabled targets the store shows AT the first fetch: the
		// seed reconcile must already have committed them (ordering proof).
		subs, err := p.st.ListEnabledForgeRepoSubscriptions(ctx, store.ForgeProviderGitHub, p.host)
		if err != nil {
			return forge.ListPage{}, err
		}
		repos := make([]string, len(subs))
		for i, s := range subs {
			repos[i] = s.Repo
		}
		p.mu.Lock()
		p.firstSub = repos
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	res, ok := p.results[pageCoord{repo, page}]
	if !ok {
		return forge.ListPage{}, nil
	}
	return res.page, res.err
}

func (p *fakePager) set(res scriptedPage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results[pageCoord{"owner/repo", 1}] = res
}

func (p *fakePager) allCalls() []pagerCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pagerCall(nil), p.calls...)
}

func (p *fakePager) firstCallSubs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.firstSub...)
}

// noopForgeReader satisfies ingest's forgeReader so NewIngester can be
// constructed; the driver fetches page-wise through the pageLister and never
// calls ListIssues, so this is never invoked.
type noopForgeReader struct{}

func (noopForgeReader) ListIssues(context.Context, string, forge.IssueFilter) ([]forge.Issue, error) {
	return nil, nil
}

// signalHandler is a slog.Handler that fires a channel the first time it sees a
// record whose message equals msg — the event the pgtest gates the immediate
// pass on (never a sleep).
type signalHandler struct {
	msg   string
	once  sync.Once
	fired chan struct{}
}

func (h *signalHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *signalHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() { close(h.fired) })
	}
	return nil
}
func (h *signalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *signalHandler) WithGroup(string) slog.Handler      { return h }

// capHandler captures slog records for the Warn-path assertions.
type capHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *capHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capHandler) WithGroup(string) slog.Handler      { return h }
func (h *capHandler) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

// --- harness ---------------------------------------------------------------

// forgeTestStore opens a fresh isolated-schema store for a forge pgtest.
func forgeTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(context.Background(), dsn) // test root context
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// runOnePass builds the driver over the given store + pager + sink (the same
// pieces buildForgeDriver composes), runs it, and returns once the immediate
// pass has COMPLETED the repo walk (gated on the driver's "repo polled" Info
// record, so the sink + cursor advance have committed before we cancel) — then
// cancels and waits for Run to return. Event-gated, never a sleep. A long
// interval means only the immediate pass fires.
func runOnePass(t *testing.T, st *store.Store, brd *board.IssueProjection, p *fakePager) {
	t.Helper()
	adapter := &forgePollStore{st: st, provider: store.ForgeProviderGitHub, host: forgeTestHost}
	ing := ingest.NewIngester(noopForgeReader{}, brd, &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     forgeTestHost,
	})
	passDone := &signalHandler{msg: "forge poll: repo polled", fired: make(chan struct{}, 1)}
	driver := ingest.NewDriver(p, ing, adapter, ingest.DriverConfig{
		Interval: time.Hour,
		Log:      slog.New(passDone),
	})

	ctx, cancel := context.WithCancel(context.Background()) // test root
	done := make(chan error, 1)
	go func() { done <- driver.Run(ctx) }()

	select {
	case <-passDone.fired:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the immediate pass did not complete within the deadline")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("driver Run returned %v, want nil on clean cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("driver Run did not return after cancel")
	}
}

// oneIssuePage returns a scripted 200 page carrying a single issue for number,
// with the given ETag and no next page.
func oneIssuePage(etag string) forge.ListPage {
	return forge.ListPage{
		Issues: []forge.Issue{{
			Number:       1,
			Title:        "a bug",
			Body:         "it broke",
			State:        "open",
			URL:          "https://github.com/owner/repo/issues/1",
			ForgeAccount: "octocat",
		}},
		ETag:    etag,
		HasNext: false,
	}
}

// --- test 3: end-to-end boot-wiring + durable cursor -----------------------

func TestForgeEndToEndBootWiringSinksAndStoresCursor(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	// Seed reconcile — exactly what buildForgeDriver does before the first pass.
	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, forgeTestHost, []string{"owner/repo"}); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)

	pager := newFakePager(st)
	pager.set(scriptedPage{page: oneIssuePage(`"etag-p1"`)})

	runOnePass(t, st, brd, pager)

	// DIRECT ordering proof: the seeded row was visible in the store AT the
	// pager's first call (the reconcile ran before the first pass).
	if subs := pager.firstCallSubs(); len(subs) != 1 || subs[0] != "owner/repo" {
		t.Fatalf("enabled subs at first fetch = %v, want [owner/repo] (seed visible before first pass)", subs)
	}

	// The issue sank to the real projection and landed durably in the store.
	issues, err := st.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("stored issues = %d, want 1 (the sunk issue)", len(issues))
	}
	if issues[0].Number != 1 || issues[0].Repo != "owner/repo" {
		t.Fatalf("stored issue = %+v, want number 1 repo owner/repo", issues[0])
	}

	// The durable cursor advanced: a forge_list_cursors page-1 row carries the
	// scripted ETag.
	cur, err := st.ForgeListCursor(ctx, store.ForgeProviderGitHub, forgeTestHost, "owner/repo")
	if err != nil {
		t.Fatalf("ForgeListCursor: %v", err)
	}
	if len(cur) != 1 || cur[0].Page != 1 {
		t.Fatalf("cursor rows = %+v, want one page-1 row", cur)
	}
	if cur[0].ETag != `"etag-p1"` {
		t.Fatalf("stored ETag = %q, want %q", cur[0].ETag, `"etag-p1"`)
	}
}

// --- test 4: restart-resync (durable cursor's headline buy) ----------------

func TestForgeRestartResyncIssuesConditionalFetch(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, forgeTestHost, []string{"owner/repo"}); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	// First boot: sink one issue, store the ETag.
	bus1 := events.NewBus[busPayload]()
	t.Cleanup(bus1.Close)
	brd1 := board.NewIssueProjection(bus1, st)
	pager1 := newFakePager(st)
	pager1.set(scriptedPage{page: oneIssuePage(`"etag-p1"`)})
	runOnePass(t, st, brd1, pager1)

	// "Restart": a fresh pipeline over the SAME schema. The pager scripts a 304
	// for the stored ETag and would FAIL if asked for an unconditional fetch
	// (empty etag) — so an inverted conditional-fetch path reddens here.
	bus2 := events.NewBus[busPayload]()
	t.Cleanup(bus2.Close)
	brd2 := board.NewIssueProjection(bus2, st)
	if err := brd2.Rehydrate(ctx); err != nil {
		t.Fatalf("rehydrate on restart: %v", err)
	}
	pager2 := newFakePager(st)
	pager2.set(scriptedPage{page: forge.ListPage{NotModified: true}})

	runOnePass(t, st, brd2, pager2)

	// The restart pass issued a CONDITIONAL fetch carrying the stored ETag.
	calls := pager2.allCalls()
	if len(calls) == 0 {
		t.Fatal("restart pass made no fetch, want one conditional fetch")
	}
	if calls[0].etag != `"etag-p1"` {
		t.Fatalf("restart fetch etag = %q, want the stored %q (no unconditional re-fetch)", calls[0].etag, `"etag-p1"`)
	}

	// Nothing new sank (a 304 re-sinks nothing) and the projection still serves
	// the issue (Rehydrate covers the read side).
	issues, err := st.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("stored issues after restart = %d, want 1 (no re-sink)", len(issues))
	}
	if len(brd2.Snapshot()) != 1 {
		t.Fatalf("projection snapshot after restart = %d, want 1 (rehydrated)", len(brd2.Snapshot()))
	}
}

// --- test 5: the no-clobber contract's runtime proof (MUST go RED) ----------

func TestForgeReingestDoesNotClobberHumanSetState(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, forgeTestHost, []string{"owner/repo"}); err != nil {
		t.Fatalf("seed reconcile: %v", err)
	}

	// (1) Ingest/create the issue via the projection (forge_state open).
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)
	pager := newFakePager(st)
	pager.set(scriptedPage{page: oneIssuePage(`"etag-open"`)})
	runOnePass(t, st, brd, pager)

	issues, err := st.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("stored issues = %d, want 1", len(issues))
	}
	id := issues[0].ID

	// (2) Set a NON-DEFAULT lifecycle State through the part-5 write path. This
	// is load-bearing: the default is BACKLOG(1), so a human-set IN_PROGRESS(5)
	// is a value a state = EXCLUDED.state regression would visibly clobber back
	// to BACKLOG (the zero-value baseline would stay green vacuously).
	if err := st.SetIssueState(ctx, id, store.IssueStateInProgress); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}

	// (3) Re-ingest a forge UPDATE for the same coordinate (open -> closed) via a
	// fresh pass. A new ETag forces a 200 re-sink of the changed page.
	bus2 := events.NewBus[busPayload]()
	t.Cleanup(bus2.Close)
	brd2 := board.NewIssueProjection(bus2, st)
	pager2 := newFakePager(st)
	closedPage := oneIssuePage(`"etag-closed"`)
	closedPage.Issues[0].State = "closed"
	pager2.set(scriptedPage{page: closedPage})
	runOnePass(t, st, brd2, pager2)

	// (4) ForgeState updated to the new forge truth, State UNCHANGED from the
	// human-set value. A regression adding state = EXCLUDED.state to
	// UpsertIssueForgeFields turns this red (State would drop back to BACKLOG).
	got, err := st.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.ForgeState != "closed" {
		t.Fatalf("ForgeState = %q, want closed (forge update applied)", got.ForgeState)
	}
	if got.State != store.IssueStateInProgress {
		t.Fatalf("State = %d, want %d (human-set IN_PROGRESS untouched by re-poll)",
			got.State, store.IssueStateInProgress)
	}
}

// --- test 8: seed-reconcile semantic (MUST go RED) -------------------------

func TestForgeSeedReconcileIsAdditiveAndNeverReEnables(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const host = forgeTestHost

	// Reconcile {a/b, c/d} -> both rows enabled.
	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, host, []string{"a/b", "c/d"}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if got := enabledRepos(t, st); !equalSet(got, []string{"a/b", "c/d"}) {
		t.Fatalf("after initial reconcile enabled = %v, want [a/b c/d]", got)
	}

	// Insert e/f directly (the "table-added" repo, not from any flag).
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
		Provider: store.ForgeProviderGitHub, Host: host, Repo: "e/f", Enabled: true,
	}); err != nil {
		t.Fatalf("insert e/f: %v", err)
	}

	// Re-run the reconcile with seed {a/b} — a repo DROPPED from the flag. c/d
	// and e/f BOTH survive, still enabled: additive, never destructive. A
	// destructive-sync regression (delete/disable rows absent from the seed)
	// turns this red.
	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, host, []string{"a/b"}); err != nil {
		t.Fatalf("re-reconcile with shrunk seed: %v", err)
	}
	if got := enabledRepos(t, st); !equalSet(got, []string{"a/b", "c/d", "e/f"}) {
		t.Fatalf("after shrunk reconcile enabled = %v, want [a/b c/d e/f] (no auto-delete/disable)", got)
	}

	// Soft-disable c/d, then re-run the reconcile with c/d STILL in the seed:
	// c/d STAYS disabled — the flag never flips an existing row's enabled. A
	// DO-UPDATE-SET-enabled regression turns this red.
	if err := st.SetForgeRepoSubscriptionEnabled(ctx, store.ForgeProviderGitHub, host, "c/d", false); err != nil {
		t.Fatalf("disable c/d: %v", err)
	}
	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, host, []string{"a/b", "c/d"}); err != nil {
		t.Fatalf("re-reconcile with c/d back in seed: %v", err)
	}
	if got := enabledRepos(t, st); !equalSet(got, []string{"a/b", "e/f"}) {
		t.Fatalf("after re-reconcile enabled = %v, want [a/b e/f] (c/d stays disabled)", got)
	}

	// Disable e/f (absent from the seed) and re-run -> stays disabled.
	if err := st.SetForgeRepoSubscriptionEnabled(ctx, store.ForgeProviderGitHub, host, "e/f", false); err != nil {
		t.Fatalf("disable e/f: %v", err)
	}
	if err := reconcileForgeSeed(ctx, st, store.ForgeProviderGitHub, host, []string{"a/b"}); err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	if got := enabledRepos(t, st); !equalSet(got, []string{"a/b"}) {
		t.Fatalf("final enabled = %v, want [a/b] (e/f stays disabled)", got)
	}
}

// --- test 2 (store-backed leg): polling-disabled Warn ----------------------

func TestForgeDisabledPollingWarnsOnEnabledRows(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const host = forgeTestHost

	t.Run("no Warn when no enabled rows exist", func(t *testing.T) {
		h := &capHandler{}
		warnDisabledForgePolling(ctx, st, store.ForgeProviderGitHub, host, slog.New(h))
		if n := warnCount(h.records()); n != 0 {
			t.Fatalf("Warn count with no enabled rows = %d, want 0", n)
		}
	})

	// Seed a row, then disabled polling must emit exactly ONE Warn.
	if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
		Provider: store.ForgeProviderGitHub, Host: host, Repo: "a/b", Enabled: true,
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	t.Run("exactly one Warn when enabled rows exist for the bound coordinate", func(t *testing.T) {
		h := &capHandler{}
		warnDisabledForgePolling(ctx, st, store.ForgeProviderGitHub, host, slog.New(h))
		if n := warnCount(h.records()); n != 1 {
			t.Fatalf("Warn count with an enabled row = %d, want exactly 1", n)
		}
	})

	t.Run("no Warn for a different bound host (abandoned rows give no false comfort)", func(t *testing.T) {
		h := &capHandler{}
		warnDisabledForgePolling(ctx, st, store.ForgeProviderGitHub, "other.example.com", slog.New(h))
		if n := warnCount(h.records()); n != 0 {
			t.Fatalf("Warn count for a different host = %d, want 0 (count is bound-coordinate only)", n)
		}
	})
}

// --- test 7 (store-backed leg): fail-fast texts as Serve would surface them --

// TestForgeStartupSecretFailFastTexts pins that the two startup failure texts
// buildForgeDriver returns are the exact record strings and distinguishable.
// This exercises buildForgeDriver's error RETURN directly. Serve routes that
// return through its listener-cleanup branch (udsListener.Close +
// listeners.close), mirroring the sibling Rehydrate/buildDoors early returns;
// that Serve-level routing is not separately covered here because the package
// has no Serve harness (the sibling cleanup branches are equally uncovered).
// The resolver is a fake; the store is real because buildForgeDriver reconciles
// the seed after a successful resolve.
func TestForgeStartupSecretFailFastTexts(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewIssueProjection(bus, st)
	cfg := ServeConfig{Forge: ForgeConfig{Poll: true, Host: forgeTestHost}}

	t.Run("secret name absent -> not declared", func(t *testing.T) {
		res := &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: "OTHER"}}}
		_, err := buildForgeDriver(ctx, cfg, st, brd, res, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "not declared") {
			t.Fatalf("buildForgeDriver err = %v, want a not-declared error", err)
		}
	})

	t.Run("resolve errors -> resolve failed at startup", func(t *testing.T) {
		res := &fakeResolver{err: errors.New("provider down")}
		_, err := buildForgeDriver(ctx, cfg, st, brd, res, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "resolve failed at startup") {
			t.Fatalf("buildForgeDriver err = %v, want a resolve-failed error", err)
		}
	})
}

// --- helpers ---------------------------------------------------------------

func enabledRepos(t *testing.T, st *store.Store) []string {
	t.Helper()
	subs, err := st.ListEnabledForgeRepoSubscriptions(context.Background(), store.ForgeProviderGitHub, forgeTestHost)
	if err != nil {
		t.Fatalf("ListEnabledForgeRepoSubscriptions: %v", err)
	}
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.Repo
	}
	return out
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

func warnCount(recs []slog.Record) int {
	n := 0
	for _, r := range recs {
		if r.Level == slog.LevelWarn {
			n++
		}
	}
	return n
}
