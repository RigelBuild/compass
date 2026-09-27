//go:build pgtest && unix

package usage_test

import (
	"context"
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
	return usage.NewPostgres(st)
}
