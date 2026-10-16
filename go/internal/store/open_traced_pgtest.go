//go:build pgtest

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenTraced adds tracer to Open's query tracer on every pooled connection. It
// is build-tagged for pgtest suites that also count statements. The store's
// migrated query set reads s.pool per call, so replacing its pool is safe.
func OpenTraced(ctx context.Context, dsn string, tracer pgx.QueryTracer) (*Store, error) {
	cfg, err := poolConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = multitracer.New(cfg.ConnConfig.Tracer, tracer)
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
