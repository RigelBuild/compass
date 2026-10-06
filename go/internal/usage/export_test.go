package usage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store"
)

// ClearMemoryRollups drops every rollup of a NewMemory store. The contract
// suite runs in usage_test, so it reaches the unexported state through here.
func ClearMemoryRollups(s Store) { s.(*Memory).clearRollups() }

// ClearPostgresRollups drops every tenant's rollups from a NewPostgres store.
// It runs as the system role, because a tenant sees only its own rows.
func ClearPostgresRollups(ctx context.Context, s Store) error {
	ctx = store.WithSystemRole(ctx)
	return s.(*Postgres).st.WithTx(ctx, func(tx pgx.Tx) error {
		for _, table := range []string{
			"token_usage_rollups_hourly", "token_usage_rollups_daily",
			"compute_usage_rollups_hourly", "compute_usage_rollups_daily",
		} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
				return err
			}
		}
		return nil
	})
}

// SeedPostgresComputeIntervals writes test intervals through the production trigger.
func SeedPostgresComputeIntervals(ctx context.Context, s Store, intervals ...ComputeInterval) error {
	p := s.(*Postgres)
	return p.st.WithTx(ctx, func(tx pgx.Tx) error {
		for _, interval := range intervals {
			if err := validateComputeInterval(interval); err != nil {
				return err
			}
			args := []any{
				interval.IntervalID + "-start", interval.IntervalID, "start",
				time.UnixMilli(interval.StartUnixMs), interval.AgentAccountID,
				interval.OwnerUserID, "session-" + interval.IntervalID, "runner-1",
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO compute_usage_events (
				    id, interval_id, kind, occurred_at, agent_account_id, owner_user_id,
				    session_id, runner_id
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (tenant_id, interval_id, kind) DO NOTHING`, args...); err != nil {
				return err
			}
			if interval.EndUnixMs == 0 {
				continue
			}
			args[0] = interval.IntervalID + "-end"
			args[2] = "end"
			args[3] = time.UnixMilli(interval.EndUnixMs)
			if _, err := tx.Exec(ctx, `
				INSERT INTO compute_usage_events (
				    id, interval_id, kind, occurred_at, agent_account_id, owner_user_id,
				    session_id, runner_id
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (tenant_id, interval_id, kind) DO NOTHING`, args...); err != nil {
				return err
			}
		}
		return nil
	})
}
