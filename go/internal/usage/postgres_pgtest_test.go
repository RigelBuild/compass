//go:build pgtest && unix

package usage_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/store/db"
	"github.com/RigelBuild/compass/go/internal/usage"
	"github.com/RigelBuild/compass/go/internal/usage/usagetest"
)

// pgTenants are seeded into every fresh schema, so a fixed id names each one.
var pgTenants = [...]store.TenantID{"usage-tenant-0", "usage-tenant-1"}

func TestPostgres(t *testing.T) {
	usagetest.Run(t, usagetest.Harness{
		New: newPostgres,
		Ctx: func(t *testing.T, tenant int) context.Context {
			t.Helper()
			return store.WithTenant(t.Context(), pgTenants[tenant])
		},
		CorruptRollups: func(t *testing.T, s usage.Store) {
			t.Helper()
			if err := usage.ClearPostgresRollups(t.Context(), s); err != nil {
				t.Fatalf("clear rollups: %v", err)
			}
		},
	})
}

// newPostgres opens a store on a fresh schema and seeds both tenants.
func newPostgres(t *testing.T) usage.Store {
	t.Helper()
	_, s := openPostgres(t)
	return s
}

// openPostgres is newPostgres that also returns the store of record, so a test
// can hold its own tx beside the usage store's.
func openPostgres(t *testing.T) (*store.Store, usage.Store) {
	t.Helper()
	st, err := store.Open(t.Context(), pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	err = st.WithTx(t.Context(), func(tx pgx.Tx) error {
		for _, id := range pgTenants {
			if err := db.New(tx).InsertTenant(t.Context(), db.InsertTenantParams{
				ID:              string(id),
				Slug:            string(id),
				DisplayName:     string(id),
				CreatedAtUnixMs: time.Now().UnixMilli(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
	return st, usage.NewPostgres(st)
}

// A rebuild reads the horizon FOR SHARE, so a prune must not advance it, and
// so must not delete events the rebuild would miss, until the rebuild commits.
func TestPostgresPruneWaitsForHeldHorizon(t *testing.T) {
	st, s := openPostgres(t)
	ctx := store.WithTenant(t.Context(), pgTenants[0])
	day0 := time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC)
	if err := s.AppendTokenUsage(ctx, []usage.TokenUsageEvent{
		pgEvent("old-1", day0.Add(time.Hour), 1),
		pgEvent("old-2", day0.Add(2*time.Hour), 2),
	}); err != nil {
		t.Fatalf("AppendTokenUsage: %v", err)
	}

	pid, commit := holdTx(t, st, ctx, func(tx pgx.Tx) error {
		_, err := db.New(tx).TokenUsagePruneHorizon(ctx)
		return err
	})
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := s.PruneTokenUsageBefore(t.Context(), day0.Add(48*time.Hour).UnixMilli())
		done <- result{n, err}
	}()

	waitBlockedBy(t, st, pid)
	if n := countEvents(t, st, ctx); n != 2 {
		t.Fatalf("with the horizon held, %d events remain, want 2: the prune did not wait", n)
	}

	commit()
	r := <-done
	if r.err != nil || r.n != 2 {
		t.Fatalf("PruneTokenUsageBefore = %d, %v; want 2, nil", r.n, r.err)
	}
	if n := countEvents(t, st, ctx); n != 0 {
		t.Fatalf("after the prune, %d events remain, want 0", n)
	}
}

// A rebuild beside an append would count the append twice or lose it, so an
// append waits for whoever holds the tenant's usage lock.
func TestPostgresAppendWaitsForUsageLock(t *testing.T) {
	st, s := openPostgres(t)
	ctx := store.WithTenant(t.Context(), pgTenants[0])
	day0 := time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC)
	if err := s.AppendTokenUsage(ctx, []usage.TokenUsageEvent{pgEvent("e1", day0, 1)}); err != nil {
		t.Fatalf("AppendTokenUsage: %v", err)
	}
	q := usage.SeriesQuery{
		Granularity: usage.GranularityDay,
		StartUnixMs: day0.UnixMilli(),
		EndUnixMs:   day0.Add(24 * time.Hour).UnixMilli(),
	}
	before := series(t, s, ctx, q)

	pid, commit := holdTx(t, st, ctx, func(tx pgx.Tx) error {
		return db.New(tx).LockTokenUsage(ctx)
	})
	done := make(chan error, 1)
	go func() {
		done <- s.AppendTokenUsage(ctx, []usage.TokenUsageEvent{pgEvent("e2", day0, 2)})
	}()

	waitBlockedBy(t, st, pid)
	if got := series(t, s, ctx, q); !slices.Equal(got, before) {
		t.Fatalf("with the usage lock held, rollups moved: got %+v, want %+v", got, before)
	}

	commit()
	if err := <-done; err != nil {
		t.Fatalf("AppendTokenUsage: %v", err)
	}
	got := series(t, s, ctx, q)
	if len(got) != 1 || got[0].InputTokens != 3 {
		t.Fatalf("after the append, series = %+v, want one bucket of 3 input tokens", got)
	}
}

// pgEvent is a valid event with n input tokens.
func pgEvent(id string, at time.Time, n int64) usage.TokenUsageEvent {
	return usage.TokenUsageEvent{
		ID:               id,
		OccurredAtUnixMs: at.UnixMilli(),
		AgentAccountID:   "a1",
		OwnerUserID:      "owner-1",
		Provider:         "anthropic",
		Model:            "model-1",
		InputTokens:      n,
		TotalTokens:      n,
		Outcome:          usage.OutcomeOK,
	}
}

// holdTx runs fn in a tx scoped by ctx and holds that tx open until commit
// runs. It returns the tx's backend pid, which a waiter reports as its blocker.
func holdTx(t *testing.T, st *store.Store, ctx context.Context, fn func(pgx.Tx) error) (pid int, commit func()) {
	t.Helper()
	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.WithTx(ctx, func(tx pgx.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			close(ready)
			<-release
			return nil
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("hold tx: %v", err)
	}
	var once sync.Once
	// A failed test still releases the tx, so the pool can close.
	t.Cleanup(func() { once.Do(func() { close(release); <-done }) })
	return pid, func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("commit held tx: %v", err)
			}
		})
	}
}

// waitBlockedBy returns once some backend waits on pid. Postgres signals no
// lock wait, so this polls the catalog. On timeout it returns anyway, and the
// caller's assertion reports the waiter that never waited.
func waitBlockedBy(t *testing.T, st *store.Store, pid int) {
	t.Helper()
	ctx := store.WithSystemRole(t.Context())
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		err := st.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`,
				pid).Scan(&blocked)
		})
		if err != nil {
			t.Fatalf("poll pg_blocking_pids: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}

// countEvents counts ctx's tenant's raw events, which the rollups outlive.
func countEvents(t *testing.T, st *store.Store, ctx context.Context) int {
	t.Helper()
	var n int
	err := st.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM token_usage_events").Scan(&n)
	})
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func series(t *testing.T, s usage.Store, ctx context.Context, q usage.SeriesQuery) []usage.Bucket {
	t.Helper()
	got, err := s.TokenUsageSeries(ctx, q)
	if err != nil {
		t.Fatalf("TokenUsageSeries: %v", err)
	}
	return got
}
