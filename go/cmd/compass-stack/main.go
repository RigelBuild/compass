//go:build unix

// Command compass-stack is the operator/integration entry point for the T2
// embedded stack supervisor: it resolves a stack.Config from flags/env, wires
// the real seam adapters into stack.Deps, and dispatches
// up|down|status|preflight to the internal/stack core.
//
//   - up:     bring the embedded stack to Ready (or attach to a live one) and
//     return once ready; the children keep running (up does NOT block).
//   - down:   attach to the live stack and stop its children, releasing the lock.
//   - status: attach and report the stack's health (state + detail).
//   - preflight: check the host's KVM/podman/microVM prerequisites; no stack
//     contact, no config resolution.
//
// All logs go to stderr; the command's own output (a status line, --version)
// goes to stdout. This is a thin wrapper around internal/stack — mirroring
// cmd/compass-mint-runner-token and cmd/compass-gen-cert.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
	"github.com/RigelBuild/compass/go/internal/stack/adapters"
	"github.com/RigelBuild/compass/go/server"
)

// version is the build version; override at build time with -ldflags
// "-X main.version=<v>". It feeds Deps.ExpectedVersion, so the attach mismatch
// check compares a live server's version against this build's.
var version = "0.1.0"

// defaultListenAddr is the fixed loopback TLS door the server binds when
// --listen is unset. It is deliberately a fixed port, never ":0": the server
// exposes no bound-address discovery API (Config.Validate rejects ":0").
const defaultListenAddr = "127.0.0.1:50052"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("compass-stack exited with an error", "error", err)
		os.Exit(1)
	}
}

// run dispatches the subcommand in args[0] to up/down/status/preflight. An
// unknown or empty subcommand is a usage error naming the four. Logs go to stderr.
func run(args []string) error {
	// Version is the command's own output, so it goes to stdout. Handle it
	// before subcommand dispatch so `compass-stack --version` works without a
	// subcommand.
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version") {
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if len(args) == 0 {
		return usageError("")
	}

	sub, rest := args[0], args[1:]

	// The process root context, cancelled on SIGINT/SIGTERM so a Ctrl-C during
	// up cancels the bring-up cleanly. This is the top-of-main root context;
	// every downstream call threads it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "up":
		return runUp(ctx, rest)
	case "down":
		return runDown(ctx, rest)
	case "status":
		return runStatus(ctx, rest)
	case "preflight":
		return runPreflight(rest)
	default:
		return usageError(sub)
	}
}

// usageError names the four valid subcommands. An empty sub is the no-subcommand
// case; a non-empty sub is an unknown one.
func usageError(sub string) error {
	if sub == "" {
		return errors.New("a subcommand is required: one of up, down, status, preflight")
	}
	return fmt.Errorf("unknown subcommand %q: expected one of up, down, status, preflight", sub)
}

// configFlags holds the resolved flag values shared by every subcommand's config
// resolution. It is the input to resolveConfig, keeping that helper pure and
// unit-testable (no flag.FlagSet, no os.Args).
type configFlags struct {
	stateDir         string
	socket           string
	listen           string
	database         string
	image            string
	runtimeDir       string
	postgresImage    string
	collectorImage   string
	databaseExternal bool
	otelExternal     string
	// otelExternalSet records whether --otel-external was explicitly passed
	// (via fs.Visit), so resolveConfig can reject an explicit empty endpoint the
	// way --database-external+empty-DSN is rejected — a bare string flag cannot
	// tell "set to empty" from "unset" on the value alone.
	otelExternalSet bool
	natsImage       string
	natsExternal    string
	// natsExternalSet records whether --nats-external was explicitly passed, for
	// the same explicit-empty reject otelExternalSet drives.
	natsExternalSet bool
	linger          bool
}

// newFlagSet builds a flag.FlagSet for one subcommand, registering the config
// flags common to all three. lingerable governs whether --linger is offered
// (up only). The returned pointers are read into a configFlags after Parse.
func newFlagSet(name string, lingerable bool) (*flag.FlagSet, *configFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	f := &configFlags{}
	fs.StringVar(&f.stateDir, "state-dir", "",
		"App state directory: the O_EXCL lock, the private postgres data dir, and "+
			"the TLS anchor live here. Required.")
	fs.StringVar(&f.socket, "socket", "",
		"Server unix socket path. Defaults to server.DefaultSocketPath() "+
			"($XDG_RUNTIME_DIR/compass/server.sock, else $HOME/.compass/server.sock).")
	fs.StringVar(&f.listen, "listen", defaultListenAddr,
		"Fixed loopback TLS door (host:port). Must be a fixed port, never :0.")
	fs.StringVar(&f.database, "database", "",
		"Postgres DSN for the store of record. Defaults to a keyword/value DSN over "+
			"a unix socket under the state dir (the private, app-private postgres). "+
			"Honors $COMPASS_DATABASE_DSN; the flag wins.")
	fs.StringVar(&f.image, "image", "",
		"Agent container image ref. Required — the runner won't boot without it.")
	fs.StringVar(&f.runtimeDir, "runtime-dir", "",
		"Runner-owned base dir for per-container agent sockets. Defaults under "+
			"$XDG_RUNTIME_DIR/compass, else a short state-dir fallback (kept short "+
			"for the AF_UNIX sun_path budget).")
	fs.StringVar(&f.postgresImage, "postgres-image", stack.DefaultPostgresImage,
		"Container image for the bundled postgres store of record (S4). Defaults "+
			"to the pinned stock postgres:18 digest. Set empty to use the dev-path "+
			"compass-postgres wrapper on PATH instead of a container. Ignored with "+
			"--database-external.")
	fs.BoolVar(&f.databaseExternal, "database-external", false,
		"Do not start postgres; use the --database DSN as-is (the external-DB "+
			"opt-out, S4). Point --database at your own postgres.")
	fs.StringVar(&f.collectorImage, "collector-image", stack.DefaultCollectorImage,
		"Container image for the bundled Plane-B fan-in OTel Collector (T4). "+
			"Defaults to the pinned upstream opentelemetry-collector-contrib digest. "+
			"Ignored with --otel-external.")
	fs.StringVar(&f.otelExternal, "otel-external", "",
		"Do not start the bundled OTel Collector; point compass surfaces at this "+
			"OTLP endpoint instead (the --otel-external opt-out, D3). The managed "+
			"plane supplies its own collector.")
	fs.StringVar(&f.natsImage, "nats-image", stack.DefaultNatsImage,
		"Container image for the bundled NATS message broker. Defaults to the "+
			"pinned upstream nats alpine digest. Ignored with --nats-external.")
	fs.StringVar(&f.natsExternal, "nats-external", "",
		"Do not start the bundled NATS; point compass surfaces at this nats:// URL "+
			"instead. The managed plane supplies its own broker.")
	if lingerable {
		fs.BoolVar(&f.linger, "linger", false,
			"Leave the stack running after this process exits (records Config.Linger).")
	}
	return fs, f
}

// markExplicitFlags records which string flags were explicitly passed (as
// opposed to left at their default), for the flags whose "set to empty" must be
// told apart from "unset": --otel-external and --nats-external. For each, an
// explicit empty value is a misuse resolveConfig rejects, while an unset flag is
// the default (bundle the component). Called after fs.Parse on every subcommand.
func markExplicitFlags(fs *flag.FlagSet, f *configFlags) {
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "otel-external" {
			f.otelExternalSet = true
		}
		if fl.Name == "nats-external" {
			f.natsExternalSet = true
		}
	})
}

// resolveConfig builds and validates a stack.Config from the parsed flags,
// applying the defaults the core does not: the socket path, the DSN over a
// state-dir unix socket, and a short runtime dir within the sun_path budget. It
// is pure (no I/O beyond env/DefaultSocketPath) so it is unit-tested directly.
func resolveConfig(f configFlags) (stack.Config, error) {
	if f.stateDir == "" {
		return stack.Config{}, errors.New("a state directory is required: pass --state-dir")
	}
	if f.image == "" {
		return stack.Config{}, errors.New("an agent image ref is required: pass --image")
	}

	listen := f.listen
	if listen == "" {
		listen = defaultListenAddr
	}

	socketPath := f.socket
	if socketPath == "" {
		p, err := server.DefaultSocketPath()
		if err != nil {
			return stack.Config{}, fmt.Errorf("resolving default socket path: %w", err)
		}
		socketPath = p
	}

	// The DSN points postgres at a unix socket UNDER the state dir, so the
	// private store is app-private. The host is the socket DIRECTORY postgres
	// -k listens on (the compass-postgres wrapper creates it). Flag wins, else
	// $COMPASS_DATABASE_DSN, else this state-dir default.
	dsn := f.database
	if dsn == "" {
		dsn = os.Getenv("COMPASS_DATABASE_DSN")
	}
	// The external-DB opt-out points the stack at an operator-run postgres, so it
	// MUST carry an explicit DSN. With no --database and no $COMPASS_DATABASE_DSN,
	// the state-dir default below would silently aim at the private socket this
	// path never starts, surfacing as a slow "postgres did not accept connections"
	// timeout on the obvious misuse rather than a clear config error. Reject it
	// here, where flag/env emptiness is still visible — once defaulted, Validate
	// cannot tell a supplied DSN from a defaulted one.
	if f.databaseExternal && dsn == "" {
		return stack.Config{}, errors.New("--database-external requires an explicit --database DSN (or $COMPASS_DATABASE_DSN): point the stack at your own postgres")
	}
	if dsn == "" {
		dsn = defaultDSN(f.stateDir)
	}

	runtimeDir := f.runtimeDir
	if runtimeDir == "" {
		runtimeDir = defaultRuntimeDir(f.stateDir)
	}

	// The --otel-external opt-out points compass surfaces at an operator/managed
	// endpoint, so it MUST carry an explicit endpoint. An explicit empty value
	// (--otel-external "") is a misuse: it would set ExternalOTLPEndpoint empty,
	// which the core reads as "bundle the collector" — the opposite of the
	// operator's intent — so reject it here where the explicit-empty is still
	// visible (fs.Visit recorded it), mirroring the --database-external reject.
	if f.otelExternalSet && f.otelExternal == "" {
		return stack.Config{}, errors.New("--otel-external requires an explicit OTLP endpoint: point compass surfaces at your own collector (omit the flag to bundle one)")
	}

	// The --nats-external opt-out is rejected on an explicit empty value for the
	// same reason: an empty ExternalNatsURL reads as "bundle NATS", the opposite
	// of the operator's intent.
	if f.natsExternalSet && f.natsExternal == "" {
		return stack.Config{}, errors.New("--nats-external requires an explicit nats:// URL: point compass surfaces at your own broker (omit the flag to bundle one)")
	}

	cfg := stack.Config{
		StateDir:             f.stateDir,
		SocketPath:           socketPath,
		ListenAddr:           listen,
		DatabaseDSN:          dsn,
		AgentImage:           f.image,
		RuntimeDir:           runtimeDir,
		PostgresImage:        f.postgresImage,
		ExternalDatabase:     f.databaseExternal,
		CollectorImage:       f.collectorImage,
		ExternalOTLPEndpoint: f.otelExternal,
		NatsImage:            f.natsImage,
		ExternalNatsURL:      f.natsExternal,
		Linger:               f.linger,
	}
	if err := cfg.Validate(); err != nil {
		return stack.Config{}, fmt.Errorf("invalid stack config: %w", err)
	}
	return cfg, nil
}

// defaultDSN builds the keyword/value DSN for the private postgres reachable over
// a unix socket under the state dir. The host is the socket directory postgres
// listens on (compass-postgres -k <dir> on the process path; a bind-mount on the
// container path), not a file, per libpq's unix-socket convention.
//
// The socket dir is <state-dir>/pgsock — a SIBLING of the PGDATA dir
// (<state-dir>/postgres), NOT nested under it. On the container path PGDATA is a
// podman bind-mount whose source must pre-exist, and postgres's initdb refuses a
// non-empty PGDATA; a socket dir nested inside PGDATA (the old
// <state-dir>/postgres/sock) would have to be created first and would then make
// PGDATA non-empty, breaking initdb. A sibling dir sidesteps that entirely and
// is transparent to the process path (the wrapper binds whatever dir the DSN
// names).
func defaultDSN(stateDir string) string {
	sockDir := filepath.Join(stateDir, "pgsock")
	return fmt.Sprintf("host=%s port=5432 dbname=compass sslmode=disable", sockDir)
}

// defaultRuntimeDir resolves the runner socket base dir, kept short for the
// AF_UNIX sun_path budget Config.Validate enforces on RuntimeDir. It prefers
// $XDG_RUNTIME_DIR/compass (short and tmpfs-backed on Linux) and falls back to a
// short dir under the state dir.
func defaultRuntimeDir(stateDir string) string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "compass")
	}
	return filepath.Join(stateDir, "run")
}

// buildDeps wires the real seam adapters into stack.Deps. Each field is one
// genuine external effect; the CLI is the composition root that supplies them.
// It returns an error because the container-backed postgres adapter resolves
// the OS user (the container superuser, S4) at construction and a failure there
// must surface loudly rather than default to a wrong role.
func buildDeps(cfg stack.Config) (stack.Deps, error) {
	deps := stack.Deps{
		Supervisor:      adapters.NewProcessSupervisor(),
		Certs:           adapters.NewCertEnsurer(0), // 0 -> DefaultRotateWindow
		Tokens:          adapters.NewTokenEnsurer(cfg.DatabaseDSN),
		Images:          adapters.NewImageEnsurer(),
		Prober:          adapters.NewHealthProber(),
		DBProber:        adapters.NewDBProber(),
		GroupSignaller:  adapters.NewGroupSignaller(),
		Now:             time.Now,
		ExpectedVersion: version,
	}
	// The container-backed postgres seams (start + teardown) are wired whenever
	// a container postgres could be in play: the container start path (up with a
	// PostgresImage) AND the cross-process down path (which reads a v2 container
	// entry and needs Containers to tear it down, without knowing at down time
	// whether up used the container). The external-DB path needs neither, but
	// wiring them unconditionally when not external is simplest and the adapter
	// is inert unless dispatched to. One adapter satisfies both seams.
	if !cfg.ExternalDatabase {
		pc, err := adapters.NewPostgresContainer()
		if err != nil {
			return stack.Deps{}, err
		}
		deps.PostgresContainer = pc
		deps.Containers = pc
	}
	// The bundled collector seams (start + readiness probe) are wired whenever a
	// bundled collector could be in play: not on the --otel-external opt-out. Its
	// teardown reuses the ContainerController contract like postgres — the
	// concrete Stop/Remove/Exists are name-agnostic podman calls, so ONE
	// Containers seam tears down both container components by their recorded
	// names. Set Containers from the collector adapter when the postgres block
	// above did not (the external-DB + bundled-collector combo), so a
	// cross-process down that reads a collector container entry always has a
	// teardown seam.
	if cfg.ExternalOTLPEndpoint == "" {
		cc, err := adapters.NewCollectorContainer()
		if err != nil {
			return stack.Deps{}, err
		}
		deps.CollectorContainer = cc
		deps.CollectorProber = cc
		if deps.Containers == nil {
			deps.Containers = cc
		}
	}
	// The bundled nats seams (start + readiness probe) are wired whenever a
	// bundled NATS could be in play: not on the --nats-external opt-out. Its
	// teardown reuses the same name-agnostic ContainerController contract, so the
	// one Containers seam already set above tears down every container component
	// by its recorded name; set it from the nats adapter only when neither block
	// above did (external DB + external OTLP + bundled nats), so a cross-process
	// down that reads a nats container entry always has a teardown seam.
	if cfg.ExternalNatsURL == "" {
		nc, err := adapters.NewNatsContainer()
		if err != nil {
			return stack.Deps{}, err
		}
		deps.NatsContainer = nc
		deps.NatsProber = nc
		if deps.Containers == nil {
			deps.Containers = nc
		}
	}
	return deps, nil
}

// runUp resolves config, wires deps, and brings the stack up. stack.Up returns
// once the stack is Ready (or attached); the children keep running, so up prints
// status and returns rather than blocking (lingering vs quit is Config.Linger's
// concern). A version mismatch on attach is surfaced as a restart-stack prompt.
func runUp(ctx context.Context, args []string) error {
	fs, f := newFlagSet("up", true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	markExplicitFlags(fs, f)
	cfg, err := resolveConfig(*f)
	if err != nil {
		return err
	}
	deps, err := buildDeps(cfg)
	if err != nil {
		return fmt.Errorf("wiring stack dependencies: %w", err)
	}

	st, err := stack.Up(ctx, cfg, deps)
	if err != nil {
		if errors.Is(err, stack.ErrVersionMismatch) {
			return errors.New("the live stack was built by a different version of compass-stack; " +
				"stop it and run `compass-stack down` (or restart the stack) before bringing up this build")
		}
		return fmt.Errorf("bringing up the stack: %w", err)
	}

	health, err := st.Health(ctx)
	if err != nil {
		return fmt.Errorf("probing stack health after up: %w", err)
	}
	printStatus(health)
	return nil
}

// runDown tears the stack down across the process boundary via
// stack.DownDetached: it reads the pgid record a prior up persisted and signals
// those process groups, so a fresh down process stops a stack it never spawned.
// This replaces the old attach-path Up+Down, which was a silent no-op across
// processes (the attached Stack owned no child handles to signal).
func runDown(ctx context.Context, args []string) error {
	fs, f := newFlagSet("down", false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	markExplicitFlags(fs, f)
	cfg, err := resolveConfig(*f)
	if err != nil {
		return err
	}
	deps, err := buildDeps(cfg)
	if err != nil {
		return fmt.Errorf("wiring stack dependencies: %w", err)
	}

	if err := stack.DownDetached(ctx, cfg, deps); err != nil {
		if errors.Is(err, stack.ErrStackStarting) {
			return errors.New("a stack is starting; retry `compass-stack down` once it is up")
		}
		if errors.Is(err, stack.ErrNoTeardownRecord) {
			return errors.New("the live stack has no teardown record for this build; " +
				"stop it with the build that started it")
		}
		return fmt.Errorf("bringing down the stack: %w", err)
	}
	_, _ = fmt.Fprintln(os.Stdout, "stack down") // best-effort CLI output; a write failure to the terminal is not actionable
	return nil
}

// runStatus attaches to the live stack (Up's attach-if-live path) and prints its
// health. The public API exposes readiness only via a *Stack's Health, and the
// only way to a *Stack is Up — which attaches without spawning when the server
// already answers — so status goes through Up, then Health.
func runStatus(ctx context.Context, args []string) error {
	fs, f := newFlagSet("status", false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	markExplicitFlags(fs, f)
	cfg, err := resolveConfig(*f)
	if err != nil {
		return err
	}
	deps, err := buildDeps(cfg)
	if err != nil {
		return fmt.Errorf("wiring stack dependencies: %w", err)
	}

	st, err := stack.Up(ctx, cfg, deps)
	if err != nil {
		if errors.Is(err, stack.ErrVersionMismatch) {
			printStatus(stack.Status{
				State:  stack.StatusAttached,
				Detail: "live stack version does not match this build; restart the stack",
			})
			return nil
		}
		return fmt.Errorf("attaching to the stack for status: %w", err)
	}
	health, err := st.Health(ctx)
	if err != nil {
		return fmt.Errorf("probing stack health: %w", err)
	}
	printStatus(health)
	return nil
}

// printStatus writes a status line to stdout: the state and, when present, the
// detail.
func printStatus(s stack.Status) {
	if s.Detail == "" {
		_, _ = fmt.Fprintln(os.Stdout, s.State.String()) // best-effort CLI output; a write failure to the terminal is not actionable
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s: %s\n", s.State.String(), s.Detail) // best-effort CLI output; a write failure to the terminal is not actionable
}
