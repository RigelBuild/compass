//go:build unix

// Command compass-stack is the operator/integration entry point for the T2
// embedded stack supervisor: it resolves a stack.Config from flags/env, wires
// the real seam adapters into stack.Deps, and dispatches up|down|status to the
// internal/stack core.
//
//   - up:     bring the embedded stack to Ready (or attach to a live one) and
//     return once ready; the children keep running (up does NOT block).
//   - down:   attach to the live stack and stop its children, releasing the lock.
//   - status: attach and report the stack's health (state + detail).
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

// run dispatches the subcommand in args[0] to up/down/status. An unknown or
// empty subcommand is a usage error naming the three. Logs go to stderr.
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
	default:
		return usageError(sub)
	}
}

// usageError names the three valid subcommands. An empty sub is the no-subcommand
// case; a non-empty sub is an unknown one.
func usageError(sub string) error {
	if sub == "" {
		return errors.New("a subcommand is required: one of up, down, status")
	}
	return fmt.Errorf("unknown subcommand %q: expected one of up, down, status", sub)
}

// configFlags holds the resolved flag values shared by every subcommand's config
// resolution. It is the input to resolveConfig, keeping that helper pure and
// unit-testable (no flag.FlagSet, no os.Args).
type configFlags struct {
	stateDir   string
	socket     string
	listen     string
	database   string
	image      string
	runtimeDir string
	linger     bool
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
	if lingerable {
		fs.BoolVar(&f.linger, "linger", false,
			"Leave the stack running after this process exits (records Config.Linger).")
	}
	return fs, f
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
	if dsn == "" {
		dsn = defaultDSN(f.stateDir)
	}

	runtimeDir := f.runtimeDir
	if runtimeDir == "" {
		runtimeDir = defaultRuntimeDir(f.stateDir)
	}

	cfg := stack.Config{
		StateDir:    f.stateDir,
		SocketPath:  socketPath,
		ListenAddr:  listen,
		DatabaseDSN: dsn,
		AgentImage:  f.image,
		RuntimeDir:  runtimeDir,
		Linger:      f.linger,
	}
	if err := cfg.Validate(); err != nil {
		return stack.Config{}, fmt.Errorf("invalid stack config: %w", err)
	}
	return cfg, nil
}

// defaultDSN builds the keyword/value DSN for the private postgres reachable over
// a unix socket under the state dir. The host is the socket directory postgres
// listens on (compass-postgres -k <dir>), not a file, per libpq's unix-socket
// convention.
func defaultDSN(stateDir string) string {
	sockDir := filepath.Join(stateDir, "postgres", "sock")
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
func buildDeps(cfg stack.Config) stack.Deps {
	return stack.Deps{
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
	cfg, err := resolveConfig(*f)
	if err != nil {
		return err
	}
	deps := buildDeps(cfg)

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
	cfg, err := resolveConfig(*f)
	if err != nil {
		return err
	}
	deps := buildDeps(cfg)

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
	cfg, err := resolveConfig(*f)
	if err != nil {
		return err
	}
	deps := buildDeps(cfg)

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
