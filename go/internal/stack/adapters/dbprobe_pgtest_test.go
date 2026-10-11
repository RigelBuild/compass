//go:build pgtest && unix

package adapters

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

// The stack probes the same DSN the server's store opens, so a DSN carrying the
// documented pool_max_conns override must still probe as reachable.
func TestProbeDBAcceptsPoolMaxConns(t *testing.T) {
	dsn := pgtest.RequireDSN(t) + "&pool_max_conns=3"
	if err := NewDBProber().ProbeDB(context.Background(), dsn); err != nil {
		t.Fatalf("ProbeDB(%q): %v", dsn, err)
	}
}
