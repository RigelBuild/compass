package store

import (
	"runtime"
	"testing"
)

func TestPoolConfigDefaultsToFixedCap(t *testing.T) {
	// The cap must not follow the host's core count, or a large host silently
	// opens a pool that a multi-instance deploy cannot fit in max_connections.
	for _, dsn := range []string{
		"postgres://u:p@db:5432/compass",
		"host=db user=u dbname=compass",
	} {
		cfg, err := poolConfig(dsn)
		if err != nil {
			t.Fatalf("poolConfig(%q): %v", dsn, err)
		}
		if cfg.MaxConns != defaultPoolMaxConns {
			t.Errorf("poolConfig(%q).MaxConns = %d, want %d (NumCPU=%d)", dsn, cfg.MaxConns, defaultPoolMaxConns, runtime.NumCPU())
		}
	}
}

func TestPoolConfigHonoursDSNPoolMaxConns(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@db:5432/compass?pool_max_conns=3",
		"host=db user=u dbname=compass pool_max_conns=3",
	} {
		cfg, err := poolConfig(dsn)
		if err != nil {
			t.Fatalf("poolConfig(%q): %v", dsn, err)
		}
		if cfg.MaxConns != 3 {
			t.Errorf("poolConfig(%q).MaxConns = %d, want 3", dsn, cfg.MaxConns)
		}
	}
}

func TestPoolConfigRejectsBadPoolMaxConns(t *testing.T) {
	if _, err := poolConfig("postgres://u:p@db:5432/compass?pool_max_conns=0"); err == nil {
		t.Fatal("poolConfig accepted pool_max_conns=0")
	}
}
