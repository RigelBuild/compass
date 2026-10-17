//go:build pgtest && unix

package pgtest

import (
	"context"
	"slices"
	"sync"

	"github.com/RigelBuild/compass/go/internal/sqlc"
	"github.com/jackc/pgx/v5"
)

// SQLCQueryCounter is a pgx.QueryTracer that records the sqlc-generated
// statements a store runs, by query name. Only statements with the sqlc header
// count, so the tenant-arming SET LOCAL/set_config statements batched ahead of
// each query do not.
type SQLCQueryCounter struct {
	mu    sync.Mutex
	names []string
}

// Reset forgets every recorded statement, so a test measures only the call
// under test.
func (c *SQLCQueryCounter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = nil
}

// Count returns how many sqlc statements ran since the last Reset.
func (c *SQLCQueryCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.names)
}

// Names returns the sqlc query names that ran since the last Reset, in order.
func (c *SQLCQueryCounter) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.names)
}

// TraceQueryStart records a pool-direct or tx statement.
func (c *SQLCQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.record(d.SQL)
	return ctx
}

// TraceQueryEnd is a no-op; the statement is recorded at start.
func (c *SQLCQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart is a no-op; each batched statement is recorded on its own.
func (c *SQLCQueryCounter) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

// TraceBatchQuery records a statement sent in a batch, the store's
// tenant-scoped pool path.
func (c *SQLCQueryCounter) TraceBatchQuery(_ context.Context, _ *pgx.Conn, d pgx.TraceBatchQueryData) {
	c.record(d.SQL)
}

// TraceBatchEnd is a no-op.
func (c *SQLCQueryCounter) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (c *SQLCQueryCounter) record(query string) {
	name, ok := sqlc.QueryName(query)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
}
