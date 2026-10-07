package usage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

// ComputeUsageSeries implements Store.
func (p *Postgres) ComputeUsageSeries(ctx context.Context, q SeriesQuery) ([]ComputeBucket, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	var rows []db.ComputeUsageSeriesRow
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = db.New(tx).ComputeUsageSeries(ctx, db.ComputeUsageSeriesParams{
			Granularity:     int32(q.Granularity),
			StartAt:         bound(q.StartUnixMs),
			EndAt:           bound(q.EndUnixMs),
			AgentAccountIds: q.AgentAccountIDs,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read compute usage series: %w", err)
	}
	series := make([]ComputeBucket, len(rows))
	for i := range rows {
		series[i] = ComputeBucket{
			StartUnixMs: rows[i].BucketStart.Time.UnixMilli(),
			ActiveMs:    rows[i].ActiveMs,
			Intervals:   rows[i].Intervals,
		}
	}
	return series, nil
}

// RebuildComputeUsageRollups implements Store.
func (p *Postgres) RebuildComputeUsageRollups(ctx context.Context) error {
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockComputeUsage(ctx); err != nil {
			return err
		}
		horizon, err := q.ComputeUsagePruneHorizon(ctx)
		if err != nil {
			return err
		}
		if err := q.DeleteComputeUsageRollupsFrom(ctx, horizon); err != nil {
			return err
		}
		return q.RollUpComputeUsageFrom(ctx, horizon)
	})
	if err != nil {
		return fmt.Errorf("usage: rebuild compute usage rollups: %w", err)
	}
	return nil
}

// PruneComputeUsageBefore deletes closed intervals before the UTC day cutoff.
func (p *Postgres) PruneComputeUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error) {
	cutoff := bound(GranularityDay.BucketStart(beforeUnixMs))
	var tenants []string
	err := p.st.WithTx(store.WithSystemRole(ctx), func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.AdvanceComputeUsagePruneHorizon(ctx, cutoff)
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("prune horizon row is missing")
		}
		tenants, err = q.ListTenantIDs(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("usage: advance compute usage prune horizon: %w", err)
	}
	var deleted int64
	for _, id := range tenants {
		n, err := p.pruneComputeTenant(ctx, id, cutoff)
		if err != nil {
			return deleted, fmt.Errorf("usage: prune compute usage of tenant %s: %w", id, err)
		}
		deleted += n
	}
	return deleted, nil
}

// pruneComputeTenant deletes only this tenant's closed intervals. It runs as
// the system role so the request role never needs DELETE on the raw log.
func (p *Postgres) pruneComputeTenant(ctx context.Context, tenantID string, cutoff pgtype.Timestamptz) (int64, error) {
	var deleted int64
	err := p.st.WithTx(store.WithSystemRole(ctx), func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockComputeUsageTenant(ctx, tenantID); err != nil {
			return err
		}
		var err error
		deleted, err = q.DeleteComputeUsageIntervalsBefore(ctx, db.DeleteComputeUsageIntervalsBeforeParams{
			TenantID: tenantID,
			Cutoff:   cutoff,
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
