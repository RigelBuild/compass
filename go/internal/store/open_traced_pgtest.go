//go:build pgtest

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenTraced is Open with tracer on every pooled connection, for pgtest suites
// in any package that pin a query count. It is build-tagged so production never
// links it. Open has no tracer seam, so the migrated store's pool is swapped for
// a traced one on the same DSN; s.q reads s.pool per call.
func OpenTraced(ctx context.Context, dsn string, tracer pgx.QueryTracer) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse traced dsn: %w", err)
	}
	cfg.ConnConfig.Tracer = tracer
	s, err := Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("store: traced pool: %w", err)
	}
	s.pool.Close()
	s.pool = pool
	return s, nil
}
