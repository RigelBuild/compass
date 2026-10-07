package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

// Postgres is the Store backed by the store of record. Each call but the prune
// runs in one transaction that row-level security scopes to ctx's tenant.
type Postgres struct {
	st *store.Store
}

var _ Store = (*Postgres)(nil)

// NewPostgres returns the Store that st backs.
func NewPostgres(st *store.Store) *Postgres {
	return &Postgres{st: st}
}

// AppendTokenUsage implements Store.
func (p *Postgres) AppendTokenUsage(ctx context.Context, events []TokenUsageEvent) error {
	if err := ValidateEvents(events); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	params := appendParams(events)
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockTokenUsage(ctx); err != nil {
			return err
		}
		return q.AppendTokenUsageEvents(ctx, params)
	})
	if err != nil {
		return fmt.Errorf("usage: append token usage: %w", err)
	}
	return nil
}

// TokenUsageSeries implements Store.
func (p *Postgres) TokenUsageSeries(ctx context.Context, q SeriesQuery) ([]Bucket, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	var rows []db.TokenUsageSeriesRow
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = db.New(tx).TokenUsageSeries(ctx, db.TokenUsageSeriesParams{
			Granularity:     int32(q.Granularity),
			StartAt:         bound(q.StartUnixMs),
			EndAt:           bound(q.EndUnixMs),
			AgentAccountIds: q.AgentAccountIDs,
			Provider:        q.Provider,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read token usage series: %w", err)
	}
	series := make([]Bucket, len(rows))
	for i := range rows {
		r := &rows[i]
		series[i] = Bucket{
			StartUnixMs:      r.BucketStart.Time.UnixMilli(),
			InputTokens:      r.InputTokens,
			OutputTokens:     r.OutputTokens,
			CacheReadTokens:  r.CacheReadTokens,
			CacheWriteTokens: r.CacheWriteTokens,
			TotalTokens:      r.TotalTokens,
			CostMicroUSD:     r.CostMicroUsd,
		}
	}
	return series, nil
}

// RebuildTokenUsageRollups implements Store.
func (p *Postgres) RebuildTokenUsageRollups(ctx context.Context) error {
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockTokenUsage(ctx); err != nil {
			return err
		}
		horizon, err := q.TokenUsagePruneHorizon(ctx)
		if err != nil {
			return err
		}
		if err := q.DeleteTokenUsageRollupsFrom(ctx, horizon); err != nil {
			return err
		}
		return q.RollUpTokenUsageFrom(ctx, horizon)
	})
	if err != nil {
		return fmt.Errorf("usage: rebuild token usage rollups: %w", err)
	}
	return nil
}

// PruneTokenUsageBefore implements Store. The BYPASSRLS system role is only for
// the named background loops, so each tenant's events go in that tenant's tx.
func (p *Postgres) PruneTokenUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error) {
	cutoff := bound(GranularityDay.BucketStart(beforeUnixMs))
	// The horizon commits before any delete. Its update waits out a rebuild that
	// holds the old horizon, and every later rebuild reads the new one.
	var tenants []string
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.AdvanceTokenUsagePruneHorizon(ctx, cutoff)
		if err != nil {
			return err
		}
		// Without the row the rebuild also fails, so the prune must not delete events.
		if n == 0 {
			return errors.New("prune horizon row is missing")
		}
		tenants, err = q.ListTenantIDs(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("usage: advance token usage prune horizon: %w", err)
	}
	var deleted int64
	for _, id := range tenants {
		n, err := p.pruneTenant(store.WithTenant(store.WithoutSystemRole(ctx), store.TenantID(id)), cutoff)
		if err != nil {
			// The earlier tenants' deletes have committed, so the count keeps them.
			return deleted, fmt.Errorf("usage: prune token usage of tenant %s: %w", id, err)
		}
		deleted += n
	}
	return deleted, nil
}

// pruneTenant deletes ctx's tenant's events before cutoff. Row-level security is
// what keeps the delete inside that tenant.
func (p *Postgres) pruneTenant(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error) {
	var n int64
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		n, err = db.New(tx).DeleteTokenUsageEventsBefore(ctx, cutoff)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// appendParams lays the batch out one column per slice, the shape the append
// statement binds. A repeated id keeps its first event, as in the reference.
func appendParams(events []TokenUsageEvent) db.AppendTokenUsageEventsParams {
	n := len(events)
	p := db.AppendTokenUsageEventsParams{
		Ids:              make([]string, 0, n),
		OccurredAt:       make([]pgtype.Timestamptz, 0, n),
		AgentAccountIds:  make([]string, 0, n),
		OwnerUserIds:     make([]string, 0, n),
		SessionIds:       make([]string, 0, n),
		RequestIds:       make([]string, 0, n),
		Providers:        make([]string, 0, n),
		Models:           make([]string, 0, n),
		CredentialIds:    make([]string, 0, n),
		InputTokens:      make([]int64, 0, n),
		OutputTokens:     make([]int64, 0, n),
		CacheReadTokens:  make([]int64, 0, n),
		CacheWriteTokens: make([]int64, 0, n),
		TotalTokens:      make([]int64, 0, n),
		CostMicroUsd:     make([]int64, 0, n),
		RateVersions:     make([]string, 0, n),
		Outcomes:         make([]string, 0, n),
	}
	seen := make(map[string]struct{}, n)
	for i := range events {
		e := &events[i]
		if _, dup := seen[e.ID]; dup {
			continue
		}
		seen[e.ID] = struct{}{}
		p.Ids = append(p.Ids, e.ID)
		p.OccurredAt = append(p.OccurredAt, timestamptz(e.OccurredAtUnixMs))
		p.AgentAccountIds = append(p.AgentAccountIds, e.AgentAccountID)
		p.OwnerUserIds = append(p.OwnerUserIds, e.OwnerUserID)
		p.SessionIds = append(p.SessionIds, e.SessionID)
		p.RequestIds = append(p.RequestIds, e.RequestID)
		p.Providers = append(p.Providers, e.Provider)
		p.Models = append(p.Models, e.Model)
		p.CredentialIds = append(p.CredentialIds, e.CredentialID)
		p.InputTokens = append(p.InputTokens, e.InputTokens)
		p.OutputTokens = append(p.OutputTokens, e.OutputTokens)
		p.CacheReadTokens = append(p.CacheReadTokens, e.CacheReadTokens)
		p.CacheWriteTokens = append(p.CacheWriteTokens, e.CacheWriteTokens)
		p.TotalTokens = append(p.TotalTokens, e.TotalTokens)
		p.CostMicroUsd = append(p.CostMicroUsd, e.CostMicroUSD)
		p.RateVersions = append(p.RateVersions, e.RateVersion)
		p.Outcomes = append(p.Outcomes, e.Outcome)
	}
	return p
}

// timestamptz converts unix milliseconds to the Postgres parameter type.
func timestamptz(unixMs int64) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.UnixMilli(unixMs), Valid: true}
}

// bound converts a comparison bound. pgx wraps an out-of-range time silently,
// so such a bound becomes the infinity that compares the same way.
func bound(unixMs int64) pgtype.Timestamptz {
	switch {
	case unixMs < minTimestamptzMs:
		return pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	case unixMs > maxTimestamptzMs:
		return pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	}
	return timestamptz(unixMs)
}
