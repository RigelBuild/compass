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

// containerStopTimeout is the graceful-stop budget pinned into the container's
// run spec as `--stop-timeout` (S4). It is the safe default for any `podman
// stop` that passes no explicit `-t`: postgres never force-kills itself and its
// smart-shutdown drain matches the host wrapper's 30s grace
// (compass-postgres/main.go shutdownGrace), so a shorter default would SIGKILL a
// still-draining cluster. It is DISTINCT from postgresDrainBudget
// (downdetached.go): the detached-`down` path passes an explicit
// `podman stop -t <postgresDrainBudget>` (10s) for behavior parity with the
// process model's capped drain, while this value governs a stop that names no
// timeout. The two knobs are deliberately not equal (S4).
const containerStopTimeout = 30 * time.Second

// PostgresContainerSpec is the fully-resolved description of the container-backed
// postgres child (S4). The core builds it from Config; the adapter translates it
// into the `podman run` argv — the same core-builds-spec / adapter-runs-it split
// serverSpec/runnerSpec use for ProcessSpec, so the exact flag/env set is a pure,
// unit-testable value rather than argv assembled behind the seam.
type PostgresContainerSpec struct {
	// Name is the stable per-state-dir container name (derived from StateDir),
	// the teardown identity a fresh `down` reconstructs and the v2 pgid record
	// persists (S4). Unique per state dir so concurrent stacks never collide.
	Name string
	// Image is the postgres image ref to run (Config.PostgresImage; the pinned
	// stock DefaultPostgresImage on the installed path).
	Image string
	// DataDir is the host PGDATA directory bind-mounted into the container: the
	// initdb cluster, fixed under the state dir (<StateDir>/postgres), mirroring
	// the host wrapper's newPGConfig data dir so a dev-box and installed stack
	// key off the same layout.
	DataDir string
	// SocketDir is the host unix-socket directory (the DSN `host=`) bind-mounted
	// into the container at the SAME path, so compass-server on the host opens
	// the identical `host=<SocketDir>` DSN over the byte-identical socket the
	// container's postgres binds (S4: the DSN shape is unchanged).
	SocketDir string
	// Port is the DSN port. The private cluster is socket-only (no TCP), but the
	// port still names the socket file (.s.PGSQL.<port>) libpq resolves, so the
	// server must listen on exactly this port for host=<dir> port=<port> to
	// find the socket.
	Port string
	// StopTimeout is the `--stop-timeout` pinned into the run (containerStopTimeout).
	StopTimeout time.Duration
}

// postgresContainerSpec builds the S4 container run spec from the resolved
// config: it parses the DSN for the socket dir + port (the bind-mount target and
// the socket-file port), fixes the data dir under the state dir, and derives the
// stable container name. It is pure (no I/O) so the flag/env set it encodes is
// unit-tested directly, and it errors on a DSN missing the fields the container
// bind-mount and socket contract need rather than running podman against a
// half-formed spec.
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

// containerName derives the stable per-state-dir postgres container name (S4).
// The name is a deterministic function of the state dir alone so a fresh `down`
// with no in-memory handle reconstructs the same name from config, and the hash
// keeps concurrent stacks on different state dirs from colliding in podman's
// flat container namespace — the same collision-avoidance the lock/pgid files
// get from living IN the state dir. It is also persisted in the v2 container
// entry, which is the authoritative teardown copy (derivation is the
// collision-avoidance scheme, the record is the identity).
func containerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-postgres-" + hex.EncodeToString(sum[:6])
}

// parseKeywordValueDSN parses the supervisor's space-separated pgx keyword/value
// DSN (host=/x port=5432 dbname=compass sslmode=disable) into a map. It mirrors
// the compass-postgres wrapper's parser (cmd/compass-postgres/main.go): the
// supervisor always forms a simple space-separated k/v DSN, so a full pgx parse
// would be dead weight. A token without an '=' is a malformed DSN and errors.
// Duplicated here rather than imported because the wrapper is a main package
// (not importable) and the two parse the identical, deliberately small grammar.
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
