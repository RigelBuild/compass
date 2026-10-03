package usage

import (
	"context"

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
		if _, err := tx.Exec(ctx, "DELETE FROM token_usage_rollups_hourly"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM token_usage_rollups_daily")
		return err
	})
}
