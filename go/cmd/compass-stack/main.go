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
	"net"
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
// --listen is unset. It is deliberately a fixed port, never ":0": later clients
// of the detached stack find the door only by this address (resolveConfig
// rejects ":0").
const defaultListenAddr = "127.0.0.1:50052"

var errStackVersionMismatch = errors.New("the live stack was built by a different version of compass-stack; " +
	"stop it and run `compass-stack down` (or restart the stack) before bringing up this build")

func main() {
	ctx := context.Background()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.ErrorContext(ctx, "compass-stack exited with an error", "error", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	if errors.Is(err, errStackVersionMismatch) {
		return stack.VersionMismatchExitCode
	}
	return 1
}

// run dispatches the subcommand in args[0] to up/down/status/preflight. An
// unknown or empty subcommand is a usage error naming the four. Logs go to stderr.
func run(ctx context.Context, args []string) error {
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
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "up":
		return runUp(ctx, rest)
	case "down":
		return runDown(ctx, rest)
	case "status":
		return runStatus(ctx, rest)
	case "preflight":
		return runPreflight(ctx, rest)
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
	// The microVM guest knobs. runtimeBackend stays empty unless the flag or
	// env explicitly selects a backend: an implicit "microvm" here would put
	// every stack on a backend its host may not support.
	runtimeBackend     string
	guestArtifact      string
	guestDir           string
	gatewayImage       string
	gatewayExternal    string
	gatewayExternalSet bool
	linger             bool
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
	fs.StringVar(&f.runtimeBackend, "runtime-backend", "",
		"Session backend the runner runs agents on ('microvm'). Empty leaves the "+
			"runner on its own resolution — there is no implicit microVM. "+
			"Honors $COMPASS_RUNTIME_BACKEND; the flag wins.")
	fs.StringVar(&f.guestArtifact, "guest-artifact", "",
		"Digest-pinned repo@sha256:<hex> guest image artifact to materialise "+
			"under the state dir before the runner starts (microvm backend). "+
			"Honors $COMPASS_GUEST_ARTIFACT; the flag wins. Mutually exclusive "+
			"with --guest-dir.")
	fs.StringVar(&f.guestDir, "guest-dir", "",
		"Already-materialised guest directory (kernel, rootfs.erofs, initrd, "+
			"manifest.sha256) to use as-is, skipping all fetching — the "+
			"air-gapped path. Honors $COMPASS_GUEST_DIR; the flag wins. Mutually "+
			"exclusive with --guest-artifact.")
	fs.StringVar(&f.gatewayImage, "gateway-image", stack.DefaultGatewayImage, "Container image for the bundled LLM gateway. Required unless --gateway-external is set.")
	fs.StringVar(&f.gatewayExternal, "gateway-external", "", "Do not start the bundled LLM gateway; use this external gateway URL instead.")
	if lingerable {
		fs.BoolVar(&f.linger, "linger", false,
			"Leave the stack running after this process exits (records Config.Linger).")
	}
	return fs, f
}

// markExplicitFlags lets resolveConfig distinguish an explicitly empty external
// endpoint from an unset opt-out.
func markExplicitFlags(fs *flag.FlagSet, f *configFlags) {
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "otel-external" {
			f.otelExternalSet = true
		}
		if fl.Name == "nats-external" {
			f.natsExternalSet = true
		}
		if fl.Name == "gateway-external" {
			f.gatewayExternalSet = true
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
	// The detached CLI exits after Up, so an ephemeral port would be known to no
	// later client; the in-process stack accepts :0 because its caller reads it.
	if _, port, err := net.SplitHostPort(listen); err == nil && port == "0" {
		return stack.Config{}, fmt.Errorf("--listen %q must be a fixed port, not :0 (the detached stack cannot publish an ephemeral port)", listen)
	}

	socketPath := f.socket
	if socketPath == "" {
		p, err := server.DefaultSocketPath()
		if err != nil {
			return stack.Config{}, fmt.Errorf("resolving default socket path: %w", err)
		}
		socketPath = p
	}

	// Use a private unix socket under the state dir. Flag wins over the
	// environment and state-dir default; postgres listens on this socket directory.
	dsn := f.database
	if dsn == "" {
		dsn = os.Getenv("COMPASS_DATABASE_DSN")
	}
	// Check before defaulting: an empty DSN is invalid for external postgres, but
	// flag/env emptiness is no longer distinguishable after applying a default.
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

	// Empty explicitly set means the opt-out intent cannot be honored; detect it
	// before defaults erase the distinction from an unset flag.
	if f.otelExternalSet && f.otelExternal == "" {
		return stack.Config{}, errors.New("--otel-external requires an explicit OTLP endpoint: point compass surfaces at your own collector (omit the flag to bundle one)")
	}

	// Empty explicitly set means bundle NATS would contradict the requested opt-out.
	if f.natsExternalSet && f.natsExternal == "" {
		return stack.Config{}, errors.New("--nats-external requires an explicit nats:// URL: point compass surfaces at your own broker (omit the flag to bundle one)")
	}
	if f.gatewayExternalSet && f.gatewayExternal == "" {
		return stack.Config{}, errors.New("--gateway-external requires an explicit URL")
	}

	// No default is safe: implicit backend selection could silently choose microVM.
	// File existence is checked at runtime, not by this pure resolver.
	runtimeBackend := f.runtimeBackend
	if runtimeBackend == "" {
		runtimeBackend = os.Getenv("COMPASS_RUNTIME_BACKEND")
	}
	guestArtifact := f.guestArtifact
	if guestArtifact == "" {
		guestArtifact = os.Getenv("COMPASS_GUEST_ARTIFACT")
	}
	guestDir := f.guestDir
	if guestDir == "" {
		guestDir = os.Getenv("COMPASS_GUEST_DIR")
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
		// No flags for these: the CLI pins the default host ports.
		NatsClientPort:      stack.DefaultNatsClientPort,
		NatsMonitorPort:     stack.DefaultNatsMonitorPort,
		CollectorGRPCPort:   stack.DefaultCollectorGRPCPort,
		CollectorHTTPPort:   stack.DefaultCollectorHTTPPort,
		CollectorHealthPort: stack.DefaultCollectorHealthPort,
		RuntimeBackend:      runtimeBackend,
		GuestArtifact:       guestArtifact,
		GuestDir:            guestDir,
		GatewayImage:        f.gatewayImage,
		ExternalGatewayURL:  f.gatewayExternal,
		Linger:              f.linger,
	}
	if err := cfg.Validate(); err != nil {
		return stack.Config{}, fmt.Errorf("invalid stack config: %w", err)
	}
	return cfg, nil
}

// defaultDSN keeps the socket dir beside PGDATA; nesting it makes the bind mount
// non-empty and causes initdb to fail.
func defaultDSN(stateDir string) string {
	sockDir := filepath.Join(stateDir, "pgsock")
	return fmt.Sprintf("host=%s port=5432 dbname=compass sslmode=disable", sockDir)
}

// defaultRuntimeDir keeps the runner socket within sun_path limits. It prefers
// the short tmpfs-backed XDG dir and falls back under the state dir.
func defaultRuntimeDir(stateDir string) string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "compass")
	}
	return filepath.Join(stateDir, "run")
}

// buildDeps wires real effect adapters. It returns errors because the
// container-backed postgres adapter resolves the OS superuser and must not
// silently choose a different role on failure.
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
	// Bundle postgres lifecycle seams so cross-process down can remove its container.
	if !cfg.ExternalDatabase {
		pc, err := adapters.NewPostgresContainer()
		if err != nil {
			return stack.Deps{}, err
		}
		deps.PostgresContainer = pc
		deps.Containers = pc
	}
	// Bundle collector seams unless opted out; down needs the shared container
	// controller even when the database is external.
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
	// Bundle NATS seams unless opted out, reusing or supplying the shared controller.
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
	if cfg.ExternalGatewayURL == "" {
		gc, err := adapters.NewGatewayContainer()
		if err != nil {
			return stack.Deps{}, err
		}
		deps.GatewayContainer, deps.GatewayProber = gc, gc
		if deps.Containers == nil {
			deps.Containers = gc
		}
	}
	return deps, nil
}

// runUp returns when the stack is Ready or attached; children keep running.
// A version mismatch on attach is surfaced as a restart-stack prompt.
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
			return errStackVersionMismatch
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

// runDown tears down a prior process's stack through stack.DownDetached, which
// reads its persisted pgid record. This lets a fresh down process stop children
// it did not spawn.
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

// runStatus uses Up's attach-if-live path because only a Stack exposes Health.
// Up attaches without spawning when the server already answers.
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
