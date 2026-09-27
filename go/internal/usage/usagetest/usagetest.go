// Package usagetest is the contract suite every usage.Store backend must pass.
package usagetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/usage"
)

// Harness adapts one backend to the suite.
type Harness struct {
	// New returns an empty store. Each subtest calls it once.
	New func(t *testing.T) usage.Store
	// Ctx returns a context scoped to tenant 0 or tenant 1 of the store.
	Ctx func(t *testing.T, tenant int) context.Context
	// CorruptRollups deletes every rollup of s and keeps the raw events.
	CorruptRollups func(t *testing.T, s usage.Store)
}

// Run runs the contract suite against the backend h adapts.
func Run(t *testing.T, h Harness) {
	t.Helper()
	t.Run("reappending_the_same_ids_does_not_double_count", reappendDoesNotDoubleCount(h))
	t.Run("mixed_batch_counts_only_new_ids", mixedBatchCountsOnlyNewIDs(h))
	t.Run("buckets_split_on_utc_hour_and_day_boundaries", bucketsSplitOnUTCBoundaries(h))
	t.Run("agent_and_provider_filters_narrow_the_sum", filtersNarrowTheSum(h))
	t.Run("empty_range_returns_no_buckets", emptyRangeReturnsNoBuckets(h))
	t.Run("series_is_ordered_by_bucket_start", seriesIsOrdered(h))
	t.Run("rebuild_restores_cleared_rollups", rebuildRestoresClearedRollups(h))
	t.Run("prune_drops_old_events_and_keeps_rollups", pruneKeepsRollups(h))
	t.Run("tenants_are_isolated", tenantsAreIsolated(h))
	t.Run("invalid_events_are_rejected_and_write_nothing", invalidEventsWriteNothing(h))
	t.Run("invalid_queries_are_rejected", invalidQueriesAreRejected(h))
}

// day0 is a UTC midnight, so the fixtures sit on known bucket boundaries.
var day0 = time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC)

// at returns day0 plus d in unix milliseconds.
func at(d time.Duration) int64 { return day0.Add(d).UnixMilli() }

const (
	hour = time.Hour
	day  = 24 * time.Hour
)

// event builds a valid event whose sums are distinct multiples of n, so a
// wrong field in a rollup shows up as a wrong sum.
func event(id string, occurredAtUnixMs int64, agent, provider string, n int64) usage.TokenUsageEvent {
	return usage.TokenUsageEvent{
		ID:               id,
		OccurredAtUnixMs: occurredAtUnixMs,
		AgentAccountID:   agent,
		OwnerUserID:      "owner-1",
		SessionID:        "session-1",
		RequestID:        "request-" + id,
		Provider:         provider,
		Model:            "model-1",
		CredentialID:     "cred-1",
		InputTokens:      n,
		OutputTokens:     2 * n,
		CacheReadTokens:  3 * n,
		CacheWriteTokens: 4 * n,
		TotalTokens:      10 * n,
		CostMicroUSD:     100 * n,
		RateVersion:      "v1",
		Outcome:          usage.OutcomeOK,
	}
}

// bucket is the expected rollup for events whose n values sum to n.
func bucket(startUnixMs, n int64) usage.Bucket {
	return usage.Bucket{
		StartUnixMs:      startUnixMs,
		InputTokens:      n,
		OutputTokens:     2 * n,
		CacheReadTokens:  3 * n,
		CacheWriteTokens: 4 * n,
		TotalTokens:      10 * n,
		CostMicroUSD:     100 * n,
	}
}

// query is a query at granularity g over the whole fixture range.
func query(g usage.Granularity) usage.SeriesQuery {
	return usage.SeriesQuery{Granularity: g, StartUnixMs: at(-30 * day), EndUnixMs: at(30 * day)}
}

func mustAppend(t *testing.T, ctx context.Context, s usage.Store, events ...usage.TokenUsageEvent) {
	t.Helper()
	if err := s.AppendTokenUsage(ctx, events); err != nil {
		t.Fatalf("AppendTokenUsage: %v", err)
	}
}

func mustRebuild(t *testing.T, ctx context.Context, s usage.Store) {
	t.Helper()
	if err := s.RebuildTokenUsageRollups(ctx); err != nil {
		t.Fatalf("RebuildTokenUsageRollups: %v", err)
	}
}

func mustPrune(t *testing.T, ctx context.Context, s usage.Store, beforeUnixMs, wantDeleted int64) {
	t.Helper()
	n, err := s.PruneTokenUsageBefore(ctx, beforeUnixMs)
	if err != nil {
		t.Fatalf("PruneTokenUsageBefore: %v", err)
	}
	if n != wantDeleted {
		t.Fatalf("PruneTokenUsageBefore deleted %d events, want %d", n, wantDeleted)
	}
}

func wantSeries(t *testing.T, ctx context.Context, s usage.Store, q usage.SeriesQuery, want ...usage.Bucket) {
	t.Helper()
	got, err := s.TokenUsageSeries(ctx, q)
	if err != nil {
		t.Fatalf("TokenUsageSeries(%+v): %v", q, err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("TokenUsageSeries(%+v)\n got  %+v\n want %+v", q, got, want)
	}
}

func reappendDoesNotDoubleCount(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		batch := []usage.TokenUsageEvent{
			event("e1", at(1*hour), "a1", "anthropic", 1),
			event("e2", at(2*hour), "a1", "anthropic", 2),
		}
		mustAppend(t, ctx, s, batch...)
		mustAppend(t, ctx, s, batch...)
		wantSeries(t, ctx, s, query(usage.GranularityHour), bucket(at(1*hour), 1), bucket(at(2*hour), 2))
		wantSeries(t, ctx, s, query(usage.GranularityDay), bucket(at(0), 3))
	}
}

func mixedBatchCountsOnlyNewIDs(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		mustAppend(t, ctx, s, event("e1", at(1*hour), "a1", "anthropic", 1))
		// The repeated e1 carries other sums: the first write wins.
		mustAppend(t, ctx, s,
			event("e1", at(1*hour), "a1", "anthropic", 50),
			event("e2", at(1*hour), "a1", "anthropic", 2),
			event("e3", at(1*hour), "a1", "anthropic", 4),
			event("e3", at(1*hour), "a1", "anthropic", 4),
		)
		wantSeries(t, ctx, s, query(usage.GranularityHour), bucket(at(1*hour), 7))
	}
}

func bucketsSplitOnUTCBoundaries(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		mustAppend(t, ctx, s,
			event("e1", at(hour-time.Millisecond), "a1", "anthropic", 1),
			event("e2", at(hour), "a1", "anthropic", 2),
			event("e3", at(day-time.Millisecond), "a1", "anthropic", 4),
			event("e4", at(day), "a1", "anthropic", 8),
		)
		wantSeries(t, ctx, s, query(usage.GranularityHour),
			bucket(at(0), 1), bucket(at(hour), 2), bucket(at(23*hour), 4), bucket(at(day), 8))
		wantSeries(t, ctx, s, query(usage.GranularityDay), bucket(at(0), 7), bucket(at(day), 8))

		// The window is half-open on bucket start: [01:00, 23:00) keeps only the 01:00 bucket.
		wantSeries(t, ctx, s,
			usage.SeriesQuery{Granularity: usage.GranularityHour, StartUnixMs: at(hour), EndUnixMs: at(23 * hour)},
			bucket(at(hour), 2))
		// A window that starts inside a bucket excludes it.
		wantSeries(t, ctx, s,
			usage.SeriesQuery{Granularity: usage.GranularityDay, StartUnixMs: at(time.Millisecond), EndUnixMs: at(30 * day)},
			bucket(at(day), 8))
	}
}

func filtersNarrowTheSum(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		mustAppend(t, ctx, s,
			event("e1", at(hour), "a1", "anthropic", 1),
			event("e2", at(hour), "a2", "anthropic", 2),
			event("e3", at(hour), "a3", "openai", 4),
			event("e4", at(hour), "a1", "openai", 8),
		)
		q := query(usage.GranularityHour)
		wantSeries(t, ctx, s, q, bucket(at(hour), 15))

		q.AgentAccountIDs = []string{"a1", "a3"}
		wantSeries(t, ctx, s, q, bucket(at(hour), 13))

		q.Provider = "openai"
		wantSeries(t, ctx, s, q, bucket(at(hour), 12))

		q.AgentAccountIDs = nil
		q.Provider = "anthropic"
		wantSeries(t, ctx, s, q, bucket(at(hour), 3))

		q.AgentAccountIDs = []string{"a2"}
		q.Provider = "openai"
		wantSeries(t, ctx, s, q)
	}
}

func emptyRangeReturnsNoBuckets(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		wantSeries(t, ctx, s, query(usage.GranularityHour))

		mustAppend(t, ctx, s, event("e1", at(hour), "a1", "anthropic", 1))
		wantSeries(t, ctx, s, usage.SeriesQuery{Granularity: usage.GranularityHour, StartUnixMs: at(hour), EndUnixMs: at(hour)})
		wantSeries(t, ctx, s, usage.SeriesQuery{Granularity: usage.GranularityHour, StartUnixMs: at(2 * day), EndUnixMs: at(3 * day)})
		// Control: the event is stored, so only the windows above are empty.
		wantSeries(t, ctx, s, usage.SeriesQuery{Granularity: usage.GranularityHour, StartUnixMs: at(hour), EndUnixMs: at(2 * hour)},
			bucket(at(hour), 1))
	}
}

func seriesIsOrdered(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		mustAppend(t, ctx, s,
			event("e1", at(5*hour), "a1", "anthropic", 1),
			event("e2", at(2*day), "a1", "anthropic", 2),
			event("e3", at(0), "a1", "anthropic", 4),
		)
		mustAppend(t, ctx, s, event("e4", at(-day), "a1", "anthropic", 8))
		wantSeries(t, ctx, s, query(usage.GranularityHour),
			bucket(at(-day), 8), bucket(at(0), 4), bucket(at(5*hour), 1), bucket(at(2*day), 2))
		wantSeries(t, ctx, s, query(usage.GranularityDay),
			bucket(at(-day), 8), bucket(at(0), 5), bucket(at(2*day), 2))
	}
}

func rebuildRestoresClearedRollups(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		mustAppend(t, ctx, s,
			event("e1", at(hour), "a1", "anthropic", 1),
			event("e2", at(hour), "a2", "openai", 2),
			event("e3", at(day+hour), "a1", "anthropic", 4),
		)
		hourly := []usage.Bucket{bucket(at(hour), 3), bucket(at(day+hour), 4)}
		daily := []usage.Bucket{bucket(at(0), 3), bucket(at(day), 4)}
		wantSeries(t, ctx, s, query(usage.GranularityHour), hourly...)
		wantSeries(t, ctx, s, query(usage.GranularityDay), daily...)

		h.CorruptRollups(t, s)
		wantSeries(t, ctx, s, query(usage.GranularityHour))

		mustRebuild(t, ctx, s)
		wantSeries(t, ctx, s, query(usage.GranularityHour), hourly...)
		wantSeries(t, ctx, s, query(usage.GranularityDay), daily...)
	}
}

func pruneKeepsRollups(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx0, ctx1 := h.New(t), h.Ctx(t, 0), h.Ctx(t, 1)
		mustAppend(t, ctx0, s,
			event("t0-old", at(hour), "a1", "anthropic", 1),
			event("t0-new", at(day), "a1", "anthropic", 2),
		)
		mustAppend(t, ctx1, s,
			event("t1-old", at(23*hour), "a1", "anthropic", 4),
			event("t1-new", at(day+hour), "a1", "anthropic", 8),
		)
		hourly := []usage.Bucket{bucket(at(hour), 1), bucket(at(day), 2)}
		wantSeries(t, ctx0, s, query(usage.GranularityHour), hourly...)

		// The cutoff rounds down to the UTC day, so a mid-day-1 cutoff keeps day 1.
		// The prune spans tenants: it deletes one old event from each.
		mustPrune(t, ctx0, s, at(day+12*hour), 2)
		wantSeries(t, ctx0, s, query(usage.GranularityHour), hourly...)

		// A rebuild keeps the rollups older than the prune horizon.
		mustRebuild(t, ctx0, s)
		wantSeries(t, ctx0, s, query(usage.GranularityHour), hourly...)

		// With the rollups gone, a rebuild can only count the events that remain.
		h.CorruptRollups(t, s)
		mustRebuild(t, ctx0, s)
		wantSeries(t, ctx0, s, query(usage.GranularityHour), bucket(at(day), 2))
		wantSeries(t, ctx0, s, query(usage.GranularityDay), bucket(at(day), 2))

		mustPrune(t, ctx0, s, at(day+12*hour), 0)
	}
}

func tenantsAreIsolated(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx0, ctx1 := h.New(t), h.Ctx(t, 0), h.Ctx(t, 1)
		mustAppend(t, ctx0, s, event("t0-e1", at(hour), "a1", "anthropic", 1))
		wantSeries(t, ctx1, s, query(usage.GranularityHour))

		mustAppend(t, ctx1, s, event("t1-e1", at(hour), "a1", "anthropic", 2))
		wantSeries(t, ctx0, s, query(usage.GranularityHour), bucket(at(hour), 1))
		wantSeries(t, ctx1, s, query(usage.GranularityHour), bucket(at(hour), 2))

		// A rebuild in one tenant leaves the other tenant's rollups as they were.
		mustRebuild(t, ctx1, s)
		wantSeries(t, ctx0, s, query(usage.GranularityHour), bucket(at(hour), 1))
		wantSeries(t, ctx1, s, query(usage.GranularityHour), bucket(at(hour), 2))
	}
}

func invalidEventsWriteNothing(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		valid := event("ok", at(hour), "a1", "anthropic", 1)
		for name, mutate := range map[string]func(*usage.TokenUsageEvent){
			"empty_id":        func(e *usage.TokenUsageEvent) { e.ID = "" },
			"zero_time":       func(e *usage.TokenUsageEvent) { e.OccurredAtUnixMs = 0 },
			"empty_agent":     func(e *usage.TokenUsageEvent) { e.AgentAccountID = "" },
			"empty_owner":     func(e *usage.TokenUsageEvent) { e.OwnerUserID = "" },
			"empty_provider":  func(e *usage.TokenUsageEvent) { e.Provider = "" },
			"empty_model":     func(e *usage.TokenUsageEvent) { e.Model = "" },
			"negative_tokens": func(e *usage.TokenUsageEvent) { e.OutputTokens = -1 },
			"negative_cost":   func(e *usage.TokenUsageEvent) { e.CostMicroUSD = -1 },
			"unknown_outcome": func(e *usage.TokenUsageEvent) { e.Outcome = "timeout" },
		} {
			t.Run(name, func(t *testing.T) {
				s, ctx := h.New(t), h.Ctx(t, 0)
				bad := event("bad", at(hour), "a1", "anthropic", 2)
				mutate(&bad)
				err := s.AppendTokenUsage(ctx, []usage.TokenUsageEvent{valid, bad})
				if !errors.Is(err, usage.ErrInvalidArgument) {
					t.Fatalf("AppendTokenUsage error = %v, want ErrInvalidArgument", err)
				}
				wantSeries(t, ctx, s, query(usage.GranularityHour))

				// The valid event was not recorded, so appending it now counts it once.
				mustAppend(t, ctx, s, valid)
				wantSeries(t, ctx, s, query(usage.GranularityHour), bucket(at(hour), 1))
			})
		}
	}
}

func invalidQueriesAreRejected(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, ctx := h.New(t), h.Ctx(t, 0)
		for _, g := range []usage.Granularity{usage.GranularityUnspecified, usage.GranularityDay + 1, -1} {
			if _, err := s.TokenUsageSeries(ctx, query(g)); !errors.Is(err, usage.ErrInvalidArgument) {
				t.Fatalf("TokenUsageSeries(granularity %d) error = %v, want ErrInvalidArgument", g, err)
			}
		}
	}
}
