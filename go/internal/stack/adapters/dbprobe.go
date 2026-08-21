//go:build unix

package adapters

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// DBProber is the real stack.DBProber: it opens a short-lived pgx connection to
// the DSN, pings it, and closes it — the lightest genuine reachability check for
// "postgres is accepting on this DSN". A ping failure (postgres still starting,
// or the compass database not yet created by the postgres wrapper's
// ensureDatabase) returns the error, which the core's postgres poll reads as
// "not yet reachable".
type DBProber struct{}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.DBProber = (*DBProber)(nil)

// NewDBProber builds a DBProber.
func NewDBProber() *DBProber {
	return &DBProber{}
}

// ProbeDB connects to dsn (pgx parses the supervisor's keyword/value DSN),
// pings, and closes. A nil error means postgres is accepting on the full DSN —
// the exact precondition compass-server's store.Open needs, since it pings once
// with no retry. Any connect or ping error means not-yet-reachable. The
// connection is always closed, on both the ping-failure and success paths, so a
// probe leaks no pooled-less connection.
func (p *DBProber) ProbeDB(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	// Close on every path — deferred so the ping-failure return below still
	// closes. Close on a healthy conn returns nil; on an already-broken conn the
	// error is not actionable here (the probe's verdict is the ping result), so
	// it is joined-free discarded via the deferred call's own error handling.
	defer func() { _ = conn.Close(ctx) }()

	if err := conn.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}
