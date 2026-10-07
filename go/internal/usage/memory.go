package usage

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/RigelBuild/compass/go/internal/store"
)

// rollupGranularities are the widths every event is rolled up at.
var rollupGranularities = [...]Granularity{GranularityHour, GranularityDay}

// rollupKey is one token rollup row's identity.
type rollupKey struct {
	granularity    Granularity
	startUnixMs    int64
	ownerUserID    string
	agentAccountID string
	provider       string
	model          string
}

// computeRollupKey is one compute rollup row identity.
type computeRollupKey struct {
	granularity    Granularity
	startUnixMs    int64
	ownerUserID    string
	agentAccountID string
}

// memoryTenant holds one tenant's raw usage and derived rollups.
type memoryTenant struct {
	events         map[string]TokenUsageEvent
	rollups        map[rollupKey]Bucket
	computeEvents  map[string]ComputeInterval
	computeRollups map[computeRollupKey]ComputeBucket
}

// Memory is the in-memory reference Store. It keeps the tenant from ctx, or ""
// when ctx carries none.
type Memory struct {
	mu      sync.Mutex
	tenants map[store.TenantID]*memoryTenant
	// The horizons are the UTC day each raw log was last pruned through.
	horizonUnixMs        int64
	computeHorizonUnixMs int64
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{
		tenants:              map[store.TenantID]*memoryTenant{},
		horizonUnixMs:        math.MinInt64,
		computeHorizonUnixMs: math.MinInt64,
	}
}

// AppendComputeInterval adds a test/reference interval and incrementally rolls
// up its duration when it is closed.
func (m *Memory) AppendComputeInterval(ctx context.Context, interval ComputeInterval) error {
	if err := validateComputeInterval(interval); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tenant(ctx)
	existing, exists := t.computeEvents[interval.IntervalID]
	if exists {
		if existing.EndUnixMs != 0 || interval.EndUnixMs == 0 {
			return nil
		}
		existing.EndUnixMs = interval.EndUnixMs
		if err := validateComputeInterval(existing); err != nil {
			return err
		}
		t.computeEvents[interval.IntervalID] = existing
		interval = existing
	} else {
		t.computeEvents[interval.IntervalID] = interval
	}
	if interval.EndUnixMs != 0 {
		// Unclipped: an interval open at prune time is in no frozen bucket yet.
		t.rollUpCompute(interval, math.MinInt64)
	}
	return nil
}

func (t *memoryTenant) rollUpCompute(interval ComputeInterval, horizon int64) {
	if interval.EndUnixMs == 0 || (horizon != math.MinInt64 && interval.EndUnixMs < horizon) {
		return
	}
	for _, g := range rollupGranularities {
		width := g.widthMs()
		startBucket := g.BucketStart(interval.StartUnixMs)
		horizonStart := int64(math.MinInt64)
		if horizon != math.MinInt64 {
			horizonStart = g.BucketStart(horizon)
		}
		first := max(startBucket, horizonStart)
		last := max(startBucket, g.BucketStart(interval.EndUnixMs-1))
		for start := first; start <= last; start += width {
			activeStart := max(interval.StartUnixMs, start)
			activeEnd := min(interval.EndUnixMs, start+width)
			count := int64(0)
			if interval.StartUnixMs >= horizon && startBucket == start {
				count = 1
			}
			key := computeRollupKey{
				granularity:    g,
				startUnixMs:    start,
				ownerUserID:    interval.OwnerUserID,
				agentAccountID: interval.AgentAccountID,
			}
			bucket := t.computeRollups[key]
			bucket.StartUnixMs = start
			bucket.ActiveMs += max(0, activeEnd-activeStart)
			bucket.Intervals += count
			t.computeRollups[key] = bucket
		}
	}
}

// validateComputeInterval rejects data that cannot be represented in UTC buckets.
func validateComputeInterval(interval ComputeInterval) error {
	switch {
	case interval.IntervalID == "":
		return fmt.Errorf("%w: empty compute interval id", ErrInvalidArgument)
	case interval.StartUnixMs <= 0 || interval.StartUnixMs > maxTimestamptzMs:
		return fmt.Errorf("%w: compute interval start is outside the supported range", ErrInvalidArgument)
	case interval.EndUnixMs < 0 || interval.EndUnixMs > maxTimestamptzMs ||
		(interval.EndUnixMs != 0 && interval.EndUnixMs < interval.StartUnixMs):
		return fmt.Errorf("%w: compute interval end is outside the supported range", ErrInvalidArgument)
	case interval.AgentAccountID == "" || interval.OwnerUserID == "":
		return fmt.Errorf("%w: empty compute interval rollup key", ErrInvalidArgument)
	}
	return nil
}

// ComputeUsageSeries implements Store.
func (m *Memory) ComputeUsageSeries(ctx context.Context, q SeriesQuery) ([]ComputeBucket, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sums := map[int64]ComputeBucket{}
	for key, bucket := range m.tenant(ctx).computeRollups {
		if key.granularity != q.Granularity || key.startUnixMs < q.StartUnixMs || key.startUnixMs >= q.EndUnixMs {
			continue
		}
		if len(q.AgentAccountIDs) > 0 && !slices.Contains(q.AgentAccountIDs, key.agentAccountID) {
			continue
		}
		sum := sums[key.startUnixMs]
		sum.StartUnixMs = key.startUnixMs
		sum.ActiveMs += bucket.ActiveMs
		sum.Intervals += bucket.Intervals
		sums[key.startUnixMs] = sum
	}
	series := make([]ComputeBucket, 0, len(sums))
	for _, bucket := range sums {
		series = append(series, bucket)
	}
	slices.SortFunc(series, func(a, b ComputeBucket) int { return cmp.Compare(a.StartUnixMs, b.StartUnixMs) })
	return series, nil
}

// RebuildComputeUsageRollups implements Store.
func (m *Memory) RebuildComputeUsageRollups(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tenant(ctx)
	for key := range t.computeRollups {
		if key.startUnixMs >= GranularityDay.BucketStart(m.computeHorizonUnixMs) {
			delete(t.computeRollups, key)
		}
	}
	for _, interval := range t.computeEvents {
		if interval.EndUnixMs != 0 && GranularityDay.BucketStart(interval.EndUnixMs) >= m.computeHorizonUnixMs {
			// Clip to the horizon: older buckets were kept above, not cleared.
			t.rollUpCompute(interval, m.computeHorizonUnixMs)
		}
	}
	return nil
}

// PruneComputeUsageBefore deletes both events of closed intervals before cutoff.
func (m *Memory) PruneComputeUsageBefore(_ context.Context, beforeUnixMs int64) (int64, error) {
	cutoff := GranularityDay.BucketStart(beforeUnixMs)
	m.mu.Lock()
	defer m.mu.Unlock()
	var deleted int64
	for _, t := range m.tenants {
		for id, interval := range t.computeEvents {
			if interval.EndUnixMs != 0 && GranularityDay.BucketStart(interval.EndUnixMs) < cutoff {
				delete(t.computeEvents, id)
				deleted += 2
			}
		}
	}
	m.computeHorizonUnixMs = max(m.computeHorizonUnixMs, cutoff)
	return deleted, nil
}

// AppendTokenUsage implements Store.
func (m *Memory) AppendTokenUsage(ctx context.Context, events []TokenUsageEvent) error {
	if err := ValidateEvents(events); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tenant(ctx)
	for i := range events {
		if _, seen := t.events[events[i].ID]; seen {
			continue
		}
		t.events[events[i].ID] = events[i]
		t.rollUp(&events[i])
	}
	return nil
}

// rollUp adds e to every rollup bucket that holds it.
func (t *memoryTenant) rollUp(e *TokenUsageEvent) {
	for _, g := range rollupGranularities {
		k := rollupKey{
			granularity:    g,
			startUnixMs:    g.BucketStart(e.OccurredAtUnixMs),
			ownerUserID:    e.OwnerUserID,
			agentAccountID: e.AgentAccountID,
			provider:       e.Provider,
			model:          e.Model,
		}
		b := t.rollups[k].plus(e.usage())
		b.StartUnixMs = k.startUnixMs
		t.rollups[k] = b
	}
}

// TokenUsageSeries implements Store.
func (m *Memory) TokenUsageSeries(ctx context.Context, q SeriesQuery) ([]Bucket, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sums := map[int64]Bucket{}
	for k, b := range m.tenant(ctx).rollups {
		if k.granularity != q.Granularity || k.startUnixMs < q.StartUnixMs || k.startUnixMs >= q.EndUnixMs {
			continue
		}
		if len(q.AgentAccountIDs) > 0 && !slices.Contains(q.AgentAccountIDs, k.agentAccountID) {
			continue
		}
		if q.Provider != "" && k.provider != q.Provider {
			continue
		}
		sums[k.startUnixMs] = sums[k.startUnixMs].plus(b)
	}
	series := make([]Bucket, 0, len(sums))
	for start, b := range sums {
		b.StartUnixMs = start
		series = append(series, b)
	}
	slices.SortFunc(series, func(a, b Bucket) int { return cmp.Compare(a.StartUnixMs, b.StartUnixMs) })
	return series, nil
}

// RebuildTokenUsageRollups implements Store.
func (m *Memory) RebuildTokenUsageRollups(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tenant(ctx)
	for k := range t.rollups {
		if k.startUnixMs >= m.horizonUnixMs {
			delete(t.rollups, k)
		}
	}
	for id := range t.events {
		if e := t.events[id]; e.OccurredAtUnixMs >= m.horizonUnixMs {
			t.rollUp(&e)
		}
	}
	return nil
}

// PruneTokenUsageBefore implements Store.
func (m *Memory) PruneTokenUsageBefore(_ context.Context, beforeUnixMs int64) (int64, error) {
	cutoff := GranularityDay.BucketStart(beforeUnixMs)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, t := range m.tenants {
		for id, e := range t.events {
			if e.OccurredAtUnixMs < cutoff {
				delete(t.events, id)
				n++
			}
		}
	}
	// An earlier cutoff must not pull the horizon back over pruned days.
	m.horizonUnixMs = max(m.horizonUnixMs, cutoff)
	return n, nil
}

// clearRollups drops every tenant's rollups and keeps the raw events.
func (m *Memory) clearRollups() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tenants {
		clear(t.rollups)
		clear(t.computeRollups)
	}
}

// tenant returns ctx's tenant state, creating it. The caller holds m.mu.
func (m *Memory) tenant(ctx context.Context) *memoryTenant {
	id, _ := store.TenantFromContext(ctx)
	t, ok := m.tenants[id]
	if !ok {
		t = &memoryTenant{
			events:         map[string]TokenUsageEvent{},
			rollups:        map[rollupKey]Bucket{},
			computeEvents:  map[string]ComputeInterval{},
			computeRollups: map[computeRollupKey]ComputeBucket{},
		}
		m.tenants[id] = t
	}
	return t
}
