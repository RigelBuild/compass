//go:build pgtest

package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

func TestComputeUsageRollupMigrationBackfillsClosedIntervals(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	// Look the migration up by file name, so its version is never hard-coded.
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	i := slices.IndexFunc(migs, func(m migration) bool { return m.name == "0002_compute_usage_rollups.sql" })
	if i < 0 {
		t.Fatal("migration 0002_compute_usage_rollups.sql is not embedded")
	}
	version := migs[i].version
	applyMigrationsThrough(t, dsn, version-1)

	seed, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed pre-rollup data: %v", err)
	}
	start := time.Date(2026, time.March, 10, 23, 30, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	_, err = seed.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ('rollup-tenant-a', 'rollup-tenant-a', 'A', 0), ('rollup-tenant-b', 'rollup-tenant-b', 'B', 0)")
	if err == nil {
		_, err = seed.Exec(ctx, `
			INSERT INTO compute_usage_events (
			    tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
			    owner_user_id, session_id, runner_id
			) VALUES
			    ('rollup-tenant-a', 'start', 'interval', 'start', $1, 'agent', 'owner', 'session', 'runner'),
			    ('rollup-tenant-a', 'end', 'interval', 'end', $2, 'agent', 'owner', 'session', 'runner'),
			    ('rollup-tenant-b', 'start', 'interval', 'start', $1, 'agent', 'owner', 'session', 'runner'),
			    ('rollup-tenant-b', 'end', 'interval', 'end', $2, 'agent', 'owner', 'session', 'runner')`, start, end)
	}
	seed.Close()
	if err != nil {
		t.Fatalf("seed pre-rollup closed intervals: %v", err)
	}

	applyMigrationsThrough(t, dsn, version)
	s := reopenStore(t, dsn)
	var horizon pgtype.Timestamptz
	if err := s.pool.QueryRow(ctx, "SELECT horizon FROM compute_usage_prune_horizon").Scan(&horizon); err != nil {
		t.Fatalf("read compute prune horizon: %v", err)
	}
	if horizon.InfinityModifier != pgtype.NegativeInfinity {
		t.Fatalf("compute prune horizon = %+v, want -infinity", horizon)
	}
	for _, tc := range []struct {
		name      string
		table     string
		buckets   []time.Time
		activeMS  []int64
		intervals []int64
	}{
		{
			name:      "hourly",
			table:     "compute_usage_rollups_hourly",
			buckets:   []time.Time{time.Date(2026, time.March, 10, 23, 0, 0, 0, time.UTC), time.Date(2026, time.March, 11, 0, 0, 0, 0, time.UTC)},
			activeMS:  []int64{30 * 60 * 1000, 30 * 60 * 1000},
			intervals: []int64{1, 0},
		},
		{
			name:      "daily",
			table:     "compute_usage_rollups_daily",
			buckets:   []time.Time{time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, time.March, 11, 0, 0, 0, 0, time.UTC)},
			activeMS:  []int64{30 * 60 * 1000, 30 * 60 * 1000},
			intervals: []int64{1, 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.pool.Query(ctx, "SELECT tenant_id, bucket_start, active_ms, intervals FROM "+tc.table+" ORDER BY tenant_id, bucket_start")
			if err != nil {
				t.Fatalf("read %s backfill: %v", tc.name, err)
			}
			defer rows.Close()
			for _, tenant := range []string{"rollup-tenant-a", "rollup-tenant-b"} {
				for bucketIndex, bucket := range tc.buckets {
					if !rows.Next() {
						t.Fatalf("%s backfill has no row for %s at %s", tc.name, tenant, bucket)
					}
					var gotTenant string
					var gotBucket time.Time
					var activeMS, intervals int64
					if err := rows.Scan(&gotTenant, &gotBucket, &activeMS, &intervals); err != nil {
						t.Fatalf("scan %s backfill: %v", tc.name, err)
					}
					if gotTenant != tenant || !gotBucket.Equal(bucket) || activeMS != tc.activeMS[bucketIndex] || intervals != tc.intervals[bucketIndex] {
						t.Errorf("%s backfill row = %s, %s, %d ms, %d intervals; want %s, %s, %d ms, %d intervals",
							tc.name, gotTenant, gotBucket, activeMS, intervals,
							tenant, bucket, tc.activeMS[bucketIndex], tc.intervals[bucketIndex])
					}
				}
			}
			if rows.Next() {
				t.Errorf("%s backfill has extra rows", tc.name)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate %s backfill: %v", tc.name, err)
			}
		})
	}
}

func applyMigrationsThrough(t *testing.T, dsn string, version int) {
	t.Helper()
	ctx := t.Context()
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to apply migrations: %v", err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire to apply migrations: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		t.Fatalf("acquire migration lock: %v", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			t.Errorf("release migration lock: %v", err)
		}
	}()
	if err := ensureMigrationsTable(ctx, conn); err != nil {
		t.Fatal(err)
	}
	applied, err := appliedChecksums(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migs {
		if _, done := applied[migration.version]; migration.version > version || done {
			continue
		}
		if err := applyMigration(ctx, conn, migration); err != nil {
			t.Fatal(err)
		}
	}
}
