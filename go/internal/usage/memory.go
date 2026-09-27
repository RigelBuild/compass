package usage

import (
	"cmp"
	"context"
	"math"
	"slices"
	"sync"

	"github.com/RigelBuild/compass/go/internal/store"
)

// rollupGranularities are the widths every event is rolled up at.
var rollupGranularities = [...]Granularity{GranularityHour, GranularityDay}

// rollupKey is one rollup row's identity, the same key the Postgres tables use.
type rollupKey struct {
	granularity    Granularity
	startUnixMs    int64
	ownerUserID    string
	agentAccountID string
	provider       string
	model          string
}

// memoryTenant is one tenant's raw events and rollups.
type memoryTenant struct {
	events  map[string]TokenUsageEvent
	rollups map[rollupKey]Bucket
}

// Memory is the in-memory reference Store. It keeps the tenant from ctx, or ""
// when ctx carries none.
type Memory struct {
	mu      sync.Mutex
	tenants map[store.TenantID]*memoryTenant
	// horizonUnixMs is the UTC day the latest prune cut at. Rollups before it
	// may count pruned events, so a rebuild must keep them.
	horizonUnixMs int64
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{tenants: map[store.TenantID]*memoryTenant{}, horizonUnixMs: math.MinInt64}
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
	}
}

// tenant returns ctx's tenant state, creating it. The caller holds m.mu.
func (m *Memory) tenant(ctx context.Context) *memoryTenant {
	id, _ := store.TenantFromContext(ctx)
	t, ok := m.tenants[id]
	if !ok {
		t = &memoryTenant{events: map[string]TokenUsageEvent{}, rollups: map[rollupKey]Bucket{}}
		m.tenants[id] = t
	}
	return t
}
