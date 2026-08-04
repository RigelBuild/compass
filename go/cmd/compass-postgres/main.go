//go:build unix

// Command compass-postgres is the private-database wrapper the stack supervisor
// spawns as its Postgres child (stack.ComponentPostgres resolves to
// `compass-postgres` on PATH). The supervisor invokes it as
//
//	compass-postgres --state-dir <StateDir> --database <DSN>
//
// (internal/stack/stack.go). The DSN is the same pgx keyword/value string
// compass-server later opens — `host=<socket-dir> port=<port> dbname=compass
// sslmode=disable` — so this wrapper's whole job is to bring up a Postgres that
// answers on exactly that socket, then hand its lifetime to the supervisor: it
// runs the real `postgres` as a child and forwards SIGTERM/SIGINT to it, so the
// supervisor's graceful stop drains the cluster cleanly.
//
// The cluster is a loopback-only, single-user private store living under the app
// state dir: socket-only (no TCP), trust auth on the local socket, one database.
// It is not a shared server and is never exposed on the network.
//
// The DSN parser, config assembly, and cluster-initialized check are pure
// functions (unit-tested); the initdb/postgres/createdb exec + signal forwarding
// is the impure orchestration exercised by the Linux integration test.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// version is the build version; override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		slog.Error("compass-postgres exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	stateDir := flag.String("state-dir", "",
		"The app state directory. The cluster's data dir is <state-dir>/postgres. Required.")
	databaseFlag := flag.String("database", "",
		"pgx keyword/value DSN describing where the cluster must listen "+
			"(host=<socket-dir> port=<port> dbname=<db> ...). "+
			"Defaults to $COMPASS_DATABASE_DSN.")
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()

	if *showVersion {
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}

	// Logs to stderr; this binary produces no stdout of its own once running, so
	// its stderr is the supervised child's log stream.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *stateDir == "" {
		return errors.New("a state directory is required: pass --state-dir")
	}
	dsn, err := resolveDSN(*databaseFlag)
	if err != nil {
		return err
	}
	cfg, err := newPGConfig(*stateDir, dsn)
	if err != nil {
		return err
	}

	// Process root context: cancelled on SIGTERM/SIGINT so the child postgres
	// receives the same graceful stop the supervisor sent us.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return serve(ctx, cfg)
}

// resolveDSN mirrors compass-mint-runner-token's precedence exactly (flag wins,
// else $COMPASS_DATABASE_DSN, else error), so every binary reads the store's
// location from one DSN source with no drift.
func resolveDSN(flagVal string) (string, error) {
	dsn := flagVal
	if dsn == "" {
		dsn = os.Getenv("COMPASS_DATABASE_DSN")
	}
	if dsn == "" {
		return "", errors.New("a Postgres DSN is required: pass --database or set $COMPASS_DATABASE_DSN")
	}
	return dsn, nil
}

// pgConfig is the fully-resolved cluster layout derived from the flags and the
// DSN. It is the pure bridge between argument parsing and the impure bring-up.
type pgConfig struct {
	DataDir   string // <state-dir>/postgres — the initdb cluster directory.
	SocketDir string // DSN host — the unix socket directory postgres listens on.
	Port      string // DSN port — kept as a string; it is only ever passed to exec'd tools.
	DBName    string // DSN dbname — the single database this store holds.
}

// newPGConfig assembles the cluster layout from the state dir and DSN. The data
// dir is fixed under the state dir; the socket dir, port, and database name come
// from the DSN so this wrapper listens on exactly the address compass-server
// will later open.
func newPGConfig(stateDir, dsn string) (pgConfig, error) {
	kv, err := parseKeywordValueDSN(dsn)
	if err != nil {
		return pgConfig{}, err
	}
	cfg := pgConfig{
		DataDir:   filepath.Join(stateDir, "postgres"),
		SocketDir: kv["host"],
		Port:      kv["port"],
		DBName:    kv["dbname"],
	}
	if cfg.SocketDir == "" {
		return pgConfig{}, fmt.Errorf("DSN is missing a host (unix socket directory): %q", dsn)
	}
	if cfg.Port == "" {
		return pgConfig{}, fmt.Errorf("DSN is missing a port: %q", dsn)
	}
	if cfg.DBName == "" {
		return pgConfig{}, fmt.Errorf("DSN is missing a dbname: %q", dsn)
	}
	return cfg, nil
}

// parseKeywordValueDSN parses the supervisor's space-separated pgx keyword/value
// DSN (host=/x port=5432 dbname=compass sslmode=disable) into a map. This is a
// deliberately small parser, not a full pgx connection-string parse: the
// supervisor always forms a simple space-separated k/v DSN, so a full parser
// would be dead weight. Multiple spaces between pairs are tolerated; a token
// without an '=' is a malformed DSN and errors.
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

// clusterInitialized reports whether dataDir already holds an initialized
// cluster. initdb writes a PG_VERSION file at the data dir root as the last step
// of a successful init, so its presence is the canonical "already initialized"
// marker — the same check postgres itself uses to refuse an uninitialized dir.
func clusterInitialized(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "PG_VERSION"))
	return err == nil
}

// serve is the impure orchestration: initialize the cluster if needed, launch
// postgres as a supervised child listening on the DSN's socket, ensure the
// database exists, then forward the caller's cancellation (SIGTERM/SIGINT) to
// postgres and exit with its status.
func serve(ctx context.Context, cfg pgConfig) error {
	if !clusterInitialized(cfg.DataDir) {
		if err := initCluster(ctx, cfg); err != nil {
			return err
		}
	}

	// The socket directory (the DSN host) must exist before postgres binds it.
	// 0700: the socket is app-private, reachable only by the owning OS user.
	if err := os.MkdirAll(cfg.SocketDir, 0o700); err != nil {
		return fmt.Errorf("creating socket directory %q: %w", cfg.SocketDir, err)
	}

	postgresBin, err := exec.LookPath("postgres")
	if err != nil {
		return fmt.Errorf("locating postgres binary: %w", err)
	}

	// listen_addresses='' makes this socket-only: no TCP port is opened, so the
	// private cluster is unreachable over the network by construction. -k sets
	// the unix_socket_directories to the DSN host so compass-server's DSN
	// resolves to this exact socket.
	//
	// The child is NOT started with the signal-cancelled ctx: we forward signals
	// to it explicitly below so we control shutdown ordering (drain postgres,
	// then exit). exec.CommandContext would SIGKILL it on ctx cancel, which is
	// the abrupt "immediate shutdown" we specifically want to avoid.
	cmd := exec.Command(postgresBin, //nolint:gosec // G204: the embedded-postgres seam — postgresBin is LookPath-resolved and the args are wrapper-built from pgConfig, neither user-controlled
		"-D", cfg.DataDir,
		"-k", cfg.SocketDir,
		"-p", cfg.Port,
		"-c", "listen_addresses=",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting postgres: %w", err)
	}
	slog.Info("postgres started",
		"datadir", cfg.DataDir, "socket", cfg.SocketDir, "port", cfg.Port, "pid", cmd.Process.Pid)

	// Ensure the target database exists once the socket is accepting. A failure
	// here is logged, not fatal: postgres is already the supervised child and we
	// must still forward shutdown to it; compass-server will surface a missing
	// database when it opens the DSN.
	go func() {
		if err := ensureDatabase(ctx, cfg); err != nil {
			slog.Error("ensuring database", "dbname", cfg.DBName, "error", err)
		}
	}()

	// Forward the caller's cancellation (the supervisor's SIGTERM/SIGINT) to
	// postgres as SIGTERM — Postgres treats SIGTERM as "smart shutdown", which
	// drains connections and stops cleanly. Then wait for the child and exit
	// with its status.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received; forwarding SIGTERM to postgres", "pid", cmd.Process.Pid)
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			slog.Error("forwarding SIGTERM to postgres", "error", err)
		}
		// Give postgres a bounded window to drain, then wait for its exit. We do
		// not SIGKILL: a smart shutdown that overruns is the supervisor's kill to
		// escalate, not ours.
		select {
		case err := <-waitErr:
			return childExitError(err)
		case <-time.After(shutdownGrace):
			slog.Warn("postgres still shutting down after grace window; awaiting its exit",
				"grace", shutdownGrace)
			return childExitError(<-waitErr)
		}
	case err := <-waitErr:
		// Postgres exited on its own (crash or external stop).
		return childExitError(err)
	}
}

// shutdownGrace bounds how long we wait after forwarding SIGTERM before logging
// that postgres is still draining. We never force-kill; escalation is the
// supervisor's job.
const shutdownGrace = 30 * time.Second

// childExitError normalizes postgres's exit: a clean exit is nil, and an exit
// caused by the SIGTERM we forwarded is treated as a clean shutdown rather than
// an error, so a graceful supervisor stop does not look like a failure.
func childExitError(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM {
			return nil
		}
	}
	return fmt.Errorf("postgres exited: %w", err)
}

// initCluster runs initdb to create the cluster in cfg.DataDir. --auth=trust is
// acceptable here because this is a loopback-only private cluster: the only way
// in is the app-private unix socket under the state dir (0700), so there is no
// untrusted network path to authenticate against.
func initCluster(ctx context.Context, cfg pgConfig) error {
	initdbBin, err := exec.LookPath("initdb")
	if err != nil {
		return fmt.Errorf("locating initdb binary: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data directory %q: %w", cfg.DataDir, err)
	}
	cmd := exec.CommandContext(ctx, initdbBin, "-D", cfg.DataDir, "--auth=trust") //nolint:gosec // G204: the embedded-postgres seam — initdbBin is LookPath-resolved and args are wrapper-built, neither user-controlled
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	slog.Info("initializing postgres cluster", "datadir", cfg.DataDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running initdb: %w", err)
	}
	return nil
}

// createDBBudget/createDBInterval bound the createdb retry below: postgres binds
// its socket a moment after launch, and createdb run too early fails with a
// connect error rather than blocking, so we retry until it connects or the
// budget elapses. The budget covers a cold cluster's first-accept latency with
// headroom on a loaded shared box.
const (
	createDBBudget   = 60 * time.Second
	createDBInterval = 200 * time.Millisecond
)

// ensureDatabase creates cfg.DBName once the socket is accepting connections,
// treating an "already exists" as success so restarts are idempotent. Postgres
// has no boot-time "create this database" flag, so we createdb over the socket
// after start. createdb does NOT block for the server — run before postgres
// accepts it fails fast with a connect error — so we retry on that transient
// failure until postgres accepts or the budget elapses; a real (non-connect)
// createdb error is returned immediately.
func ensureDatabase(ctx context.Context, cfg pgConfig) error {
	createdbBin, err := exec.LookPath("createdb")
	if err != nil {
		return fmt.Errorf("locating createdb binary: %w", err)
	}

	deadline := time.Now().Add(createDBBudget)
	ticker := time.NewTicker(createDBInterval)
	defer ticker.Stop()
	for {
		cmd := exec.CommandContext(ctx, createdbBin, //nolint:gosec // G204: the embedded-postgres seam — createdbBin is LookPath-resolved and args are wrapper-built from pgConfig, neither user-controlled
			"-h", cfg.SocketDir, "-p", cfg.Port, cfg.DBName)
		out, err := cmd.CombinedOutput()
		if err == nil {
			slog.Info("created database", "dbname", cfg.DBName)
			return nil
		}
		// createdb exits non-zero when the database already exists; that is the
		// steady-state restart case, not a failure.
		if strings.Contains(string(out), "already exists") {
			return nil
		}
		// A connection failure means postgres is not accepting yet: retry until
		// it is. Any other createdb error is a real failure — return it now.
		if !strings.Contains(string(out), "could not connect") &&
			!strings.Contains(string(out), "No such file or directory") &&
			!strings.Contains(string(out), "the database system is") {
			return fmt.Errorf("running createdb %q: %w: %s", cfg.DBName, err, strings.TrimSpace(string(out)))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("running createdb %q: postgres did not accept connections within %s: %w: %s",
				cfg.DBName, createDBBudget, err, strings.TrimSpace(string(out)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
