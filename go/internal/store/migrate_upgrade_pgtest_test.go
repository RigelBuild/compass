//go:build pgtest

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

// TestOpenUpgradesV1DatabaseToTokenUsage builds a database that stopped at v1,
// as a deployed one would, and proves Open migrates it forward. A fresh-schema
// test cannot catch a later migration that only works on an empty database.
func TestOpenUpgradesV1DatabaseToTokenUsage(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	const tenant TenantID = "upgrade-tenant"

	applyV1Only(t, dsn)
	// The tenant predates the upgrade; the token-usage foreign keys must accept it.
	seed, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed: %v", err)
	}
	err = db.New(seed).InsertTenant(ctx, db.InsertTenantParams{
		ID: string(tenant), Slug: string(tenant), DisplayName: string(tenant),
		CreatedAtUnixMs: time.Now().UnixMilli(),
	})
	seed.Close()
	if err != nil {
		t.Fatalf("seed tenant at v1: %v", err)
	}

	s := openStore(t, dsn)

	var version int
	if err := s.pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 2 {
		t.Fatalf("schema version after upgrade = %d, want 2", version)
	}

	for _, tbl := range []string{"token_usage_events", "token_usage_rollups_hourly", "token_usage_rollups_daily"} {
		var enabled, forced bool
		err := s.pool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class
			  WHERE oid = to_regclass(format('%I.%I', current_schema(), $1::text))`, tbl,
		).Scan(&enabled, &forced)
		if err != nil {
			t.Fatalf("%s: read RLS flags (table missing?): %v", tbl, err)
		}
		if !enabled || !forced {
			t.Errorf("%s: relrowsecurity=%t relforcerowsecurity=%t, want both true", tbl, enabled, forced)
		}
	}

	var horizon pgtype.Timestamptz
	if err := s.pool.QueryRow(ctx, "SELECT horizon FROM token_usage_prune_horizon").Scan(&horizon); err != nil {
		t.Fatalf("read prune horizon row: %v", err)
	}
	if horizon.InfinityModifier != pgtype.NegativeInfinity {
		t.Errorf("prune horizon = %+v, want -infinity", horizon)
	}

	at := time.Date(2026, time.March, 10, 5, 30, 0, 0, time.UTC)
	tctx := WithTenant(ctx, tenant)
	err = s.WithTx(tctx, func(tx pgx.Tx) error {
		return db.New(tx).AppendTokenUsageEvents(tctx, db.AppendTokenUsageEventsParams{
			Ids:              []string{"ev-1"},
			OccurredAt:       []pgtype.Timestamptz{{Time: at, Valid: true}},
			AgentAccountIds:  []string{"agent-1"},
			OwnerUserIds:     []string{"user-1"},
			SessionIds:       []string{"session-1"},
			RequestIds:       []string{"request-1"},
			Providers:        []string{"anthropic"},
			Models:           []string{"model-1"},
			CredentialIds:    []string{"cred-1"},
			InputTokens:      []int64{7},
			OutputTokens:     []int64{3},
			CacheReadTokens:  []int64{0},
			CacheWriteTokens: []int64{0},
			TotalTokens:      []int64{10},
			CostMicroUsd:     []int64{42},
			RateVersions:     []string{"v1"},
			Outcomes:         []string{"ok"},
		})
	})
	if err != nil {
		t.Fatalf("append token usage after upgrade: %v", err)
	}

	daily := db.TokenUsageSeriesParams{
		Granularity: 2,
		StartAt:     pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true},
		EndAt:       pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true},
	}
	if got := readSeries(t, s, tctx, daily); len(got) != 1 || got[0].TotalTokens != 10 || got[0].CostMicroUsd != 42 {
		t.Errorf("upgraded tenant daily series = %+v, want one bucket of 10 tokens / 42 micro-USD", got)
	}
	// The bootstrap tenant Open seeds must not see another tenant's usage.
	if got := readSeries(t, s, WithTenant(ctx, s.bootstrapTenantID), daily); len(got) != 0 {
		t.Errorf("bootstrap tenant daily series = %+v, want none (RLS leak)", got)
	}
}

// applyV1Only migrates the empty schema at dsn to v1 through the runner's own
// steps, leaving every later migration pending for Open.
func applyV1Only(t *testing.T, dsn string) {
	t.Helper()
	ctx := t.Context()
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for v1: %v", err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire for v1: %v", err)
	}
	defer conn.Release()
	if err := ensureMigrationsTable(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(ctx, conn, migs[0]); err != nil {
		t.Fatal(err)
	}
}

// readSeries reads one tenant's token-usage series in a tenant tx.
func readSeries(t *testing.T, s *Store, ctx context.Context, p db.TokenUsageSeriesParams) []db.TokenUsageSeriesRow {
	t.Helper()
	var rows []db.TokenUsageSeriesRow
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = db.New(tx).TokenUsageSeries(ctx, p)
		return err
	})
	if err != nil {
		t.Fatalf("read token usage series: %v", err)
	}
	return rows
}
