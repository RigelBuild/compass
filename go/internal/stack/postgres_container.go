//go:build unix

package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// containerStopTimeout is podman's no-explicit-timeout fallback; it covers the server's 30s shutdown grace.
// Detached down passes its shorter postgresDrainBudget explicitly.
const containerStopTimeout = 30 * time.Second

// PostgresContainerSpec resolves the container launch inputs as a pure value for the adapter.
type PostgresContainerSpec struct {
	// Name is stable per state dir and persisted as the v2 teardown identity.
	Name string
	// Image is the postgres image ref, normally the pinned DefaultPostgresImage.
	Image string
	// DataDir is the state-dir PGDATA mount, matching the host wrapper's layout.
	DataDir string
	// SocketDir is mounted at the same path so the host DSN reaches postgres's socket.
	SocketDir string
	// Port selects the socket filename even though this private cluster has no TCP listener.
	Port string
	// StopTimeout is the --stop-timeout pinned into the run.
	StopTimeout time.Duration
}

// postgresContainerSpec parses the DSN socket path and port, then resolves state-dir paths and name without I/O.
func postgresContainerSpec(cfg Config) (PostgresContainerSpec, error) {
	kv, err := parseKeywordValueDSN(cfg.DatabaseDSN)
	if err != nil {
		return PostgresContainerSpec{}, err
	}
	socketDir := kv["host"]
	if socketDir == "" {
		return PostgresContainerSpec{}, fmt.Errorf("stack config: DatabaseDSN is missing a host (unix socket directory) for the container postgres: %q", cfg.DatabaseDSN)
	}
	port := kv["port"]
	if port == "" {
		return PostgresContainerSpec{}, fmt.Errorf("stack config: DatabaseDSN is missing a port for the container postgres: %q", cfg.DatabaseDSN)
	}
	return PostgresContainerSpec{
		Name:        containerName(cfg.StateDir),
		Image:       cfg.PostgresImage,
		DataDir:     filepath.Join(cfg.StateDir, "postgres"),
		SocketDir:   socketDir,
		Port:        port,
		StopTimeout: containerStopTimeout,
	}, nil
}

// containerName hashes the cleaned state dir for a deterministic podman name; the v2 record remains teardown authority.
func containerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-postgres-" + hex.EncodeToString(sum[:6])
}

// parseKeywordValueDSN parses the supervisor's simple space-separated key=value grammar.
// It is duplicated because compass-postgres is a main package.
func parseKeywordValueDSN(dsn string) (map[string]string, error) {
	out := make(map[string]string)
	for tok := range strings.FieldsSeq(dsn) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("malformed DSN pair %q: expected key=value", tok)
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil, errors.New("empty DSN: expected space-separated key=value pairs")
	}
	return out, nil
}
