//go:build unix

// Command compass-server is the Compass server entry point: it parses the CLI,
// resolves the socket path, and serves compass.v1 over a Unix domain socket
// until a termination signal (SIGINT/SIGTERM) drains it. All transport logic
// lives in the server package; this binary is a thin wrapper over server.Serve.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sealedsecurity/compass/go/server"
)

// version is the server build + contract version reported by GetServerInfo (the
// workspace version 0.1.0); override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

// apiVersion is the compass.v1 contract version, logged at startup alongside the
// build version (the authoritative wire value is reported by the GetServerInfo
// RPC).
const apiVersion = "compass.v1"

func main() {
	if err := run(); err != nil {
		slog.Error("compass-server exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	socketFlag := flag.String("socket", "",
		"Unix socket to serve compass.v1 on. Defaults to "+
			"$XDG_RUNTIME_DIR/compass/server.sock, falling back to "+
			"$HOME/.compass/server.sock.")
	devHTTPFlag := flag.String("dev-http", "",
		"Dev only: also serve gRPC-Web on this loopback TCP address (e.g. "+
			"127.0.0.1:50051) for a browser dev server. Off by default; the "+
			"shipped path is socket-only. A non-loopback address is rejected.")
	listenFlag := flag.String("listen", "",
		"Serve the authenticated gRPC network door on this TCP address (e.g. "+
			"0.0.0.0:8443). Off by default; the shipped local path is socket-only. "+
			"Requires --tls-cert and --tls-key together — a bearer token over "+
			"cleartext is credential disclosure.")
	tlsCertFlag := flag.String("tls-cert", "",
		"PEM certificate file terminating TLS on the --listen network door. "+
			"Required with --listen.")
	tlsKeyFlag := flag.String("tls-key", "",
		"PEM private-key file terminating TLS on the --listen network door. "+
			"Required with --listen.")
	databaseFlag := flag.String("database", "",
		"Postgres DSN for the store of record (e.g. postgres://user:pass@host/compass). "+
			"Defaults to $COMPASS_DATABASE_DSN.")
	s3EndpointFlag := flag.String("s3-endpoint", "",
		"S3-compatible object-store endpoint host[:port] (no scheme) for the "+
			"transcript archive tier (Garage/R2/MinIO/AWS). Defaults to "+
			"$COMPASS_S3_ENDPOINT. Absent = no archive tier (dev server still boots).")
	s3BucketFlag := flag.String("s3-bucket", "",
		"Bucket archive segments are written under. Defaults to $COMPASS_S3_BUCKET.")
	s3AccessKeyFlag := flag.String("s3-access-key", "",
		"S3 access key. Defaults to $COMPASS_S3_ACCESS_KEY.")
	s3SecretKeyFlag := flag.String("s3-secret-key", "",
		"S3 secret key. Defaults to $COMPASS_S3_SECRET_KEY.")
	s3RegionFlag := flag.String("s3-region", "",
		"S3 region (e.g. us-east-1, or \"garage\" for Garage). Defaults to $COMPASS_S3_REGION.")
	s3UseTLSFlag := flag.Bool("s3-use-tls", false,
		"Use https to the S3 endpoint. Defaults to $COMPASS_S3_USE_TLS "+
			"(\"1\"/\"true\"/\"yes\"/\"on\" = on).")
	forgeFlags := registerForgeFlags()
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()

	if *showVersion {
		fmt.Printf("compass-server %s\n", version) //nolint:forbidigo // --version writes to stdout: that is a command's own CLI output, not logging, so the no-fmt-print rule does not apply
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	socketPath := *socketFlag
	if socketPath == "" {
		resolved, err := server.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("resolving default socket path: %w", err)
		}
		socketPath = resolved
	}

	var devHTTP *netip.AddrPort
	if *devHTTPFlag != "" {
		addr, err := netip.ParseAddrPort(*devHTTPFlag)
		if err != nil {
			return fmt.Errorf("parsing --dev-http address %q: %w", *devHTTPFlag, err)
		}
		// A dev endpoint must stay on the loopback interface — it has no auth and
		// exists only for the local browser dev server; binding it on a routable
		// address would expose the server to the network. server.Serve re-asserts
		// this invariant, but fail fast here rather than deep in the serve loop.
		if !addr.Addr().IsLoopback() {
			return fmt.Errorf("--dev-http must bind a loopback address (127.0.0.1 or ::1), got %s", addr)
		}
		devHTTP = &addr
	}

	listen, tlsConfig, err := resolveNetworkDoor(*listenFlag, *tlsCertFlag, *tlsKeyFlag)
	if err != nil {
		return err
	}

	databaseDSN := *databaseFlag
	if databaseDSN == "" {
		databaseDSN = os.Getenv("COMPASS_DATABASE_DSN")
	}
	if databaseDSN == "" {
		return errors.New("a Postgres DSN is required: pass --database or set $COMPASS_DATABASE_DSN")
	}

	s3Config := server.S3Config{
		Endpoint:  firstNonEmpty(*s3EndpointFlag, os.Getenv("COMPASS_S3_ENDPOINT")),
		Bucket:    firstNonEmpty(*s3BucketFlag, os.Getenv("COMPASS_S3_BUCKET")),
		AccessKey: firstNonEmpty(*s3AccessKeyFlag, os.Getenv("COMPASS_S3_ACCESS_KEY")),
		SecretKey: firstNonEmpty(*s3SecretKeyFlag, os.Getenv("COMPASS_S3_SECRET_KEY")),
		Region:    firstNonEmpty(*s3RegionFlag, os.Getenv("COMPASS_S3_REGION")),
		UseTLS:    *s3UseTLSFlag || envTrue(os.Getenv("COMPASS_S3_USE_TLS")),
	}

	// Forge poll driver (SEA-1810): flag-then-env, all-optional (see forgeFlags.resolve).
	forgeConfig, err := forgeFlags.resolve()
	if err != nil {
		return err
	}

	slog.Info("compass-server starting",
		"version", version,
		"api", apiVersion,
		"socket", socketPath,
	)

	// SIGINT (Ctrl-C) or SIGTERM (service stop) cancels the context, which drains
	// both servers gracefully. stop() restores default signal handling on return.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Log once when a termination signal actually arrives, so an operator watching
	// stderr can tell a graceful drain from a hard death (a dedicated registration
	// so it fires only on a real signal, not when Serve returns on its own).
	stopDrainLog := logOnDrainSignal()
	defer stopDrainLog()

	return server.Serve(ctx, server.ServeConfig{
		SocketPath:  socketPath,
		Version:     version,
		DevHTTP:     devHTTP,
		Listen:      listen,
		TLS:         tlsConfig,
		DatabaseDSN: databaseDSN,
		S3:          s3Config,
		Forge:       forgeConfig,
	})
}

// logOnDrainSignal registers a one-shot handler that logs the first SIGINT/SIGTERM
// so an operator can tell a graceful drain from a hard death. It returns a stop
// func the caller defers (unregisters the handler). A dedicated registration
// rather than ctx.Done() so it fires only on a real signal — not when Serve
// returns on its own and the caller's stop() then cancels ctx.
func logOnDrainSignal() func() {
	drainSig := make(chan os.Signal, 1)
	signal.Notify(drainSig, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, ok := <-drainSig; ok {
			slog.Info("shutdown signal received; draining")
		}
	}()
	return func() { signal.Stop(drainSig) }
}

// resolveNetworkDoor validates the three network-door flags as an all-or-none
// group and turns them into the ServeConfig fields the serve loop consumes.
// Either all three are set (the authenticated TCP door is enabled) or none are
// (the shipped socket-only path). Any partial combination is a startup error:
// --listen without both TLS flags would serve bearer tokens over cleartext
// (credential disclosure), and TLS paths without --listen is a keypair nothing
// serves — both are operator mistakes worth failing fast on rather than deep in
// the serve loop. server.Serve re-asserts the TLS-required invariant as defense
// in depth (network_door.go loadNetworkTLS); this is the friendly CLI-level check.
func resolveNetworkDoor(listen, tlsCert, tlsKey string) (string, *server.TLSConfig, error) {
	set := 0
	for _, v := range []string{listen, tlsCert, tlsKey} {
		if v != "" {
			set++
		}
	}
	switch set {
	case 0:
		return "", nil, nil
	case 3:
		return listen, &server.TLSConfig{CertPath: tlsCert, KeyPath: tlsKey}, nil
	default:
		var missing []string
		if listen == "" {
			missing = append(missing, "--listen")
		}
		if tlsCert == "" {
			missing = append(missing, "--tls-cert")
		}
		if tlsKey == "" {
			missing = append(missing, "--tls-key")
		}
		return "", nil, fmt.Errorf(
			"the network door needs --listen, --tls-cert, and --tls-key together "+
				"(missing %s); pass all three to enable it or none for the "+
				"socket-only default", strings.Join(missing, ", "))
	}
}

// forgeFlags holds the five SEA-1810 forge CLI flag pointers, registered as a
// group so run() stays short (they mirror the S3 flag set's precedence).
type forgeFlags struct {
	repos    *string
	poll     *bool
	interval *string
	secret   *string
	host     *string
}

// registerForgeFlags declares the five forge flags on the default FlagSet and
// returns their pointers. Split out of run() so the flag registration does not
// inflate it past the length budget.
func registerForgeFlags() forgeFlags {
	return forgeFlags{
		repos: flag.String("forge-repos", "",
			"Comma-separated owner/name repos to SEED into forge_repo_subscriptions "+
				"(SEA-1810 board poll). Defaults to $COMPASS_FORGE_REPOS. A declarative "+
				"seed reconciled at boot (bootstrap-only insert), NOT the live target "+
				"set — the table is authoritative after the first insert. A non-empty "+
				"seed enables the poll driver."),
		poll: flag.Bool("forge-poll", false,
			"Run the forge poll driver even with an empty --forge-repos seed (targets "+
				"already in the table). Defaults to $COMPASS_FORGE_POLL "+
				"(\"1\"/\"true\"/\"yes\"/\"on\" = on). Off by default; forge polling is "+
				"enabled iff this is set OR the seed is non-empty."),
		interval: flag.String("forge-poll-interval", "",
			"Forge poll cadence (a Go duration, e.g. 1m). Defaults to "+
				"$COMPASS_FORGE_POLL_INTERVAL, then 1m."),
		secret: flag.String("forge-secret", "",
			"Declared server_only secret NAME holding the forge token (the VALUE never "+
				"crosses a flag). Defaults to $COMPASS_FORGE_SECRET, then GITHUB_FORGE_TOKEN."),
		host: flag.String("forge-host", "",
			"Forge host the poll driver binds (github.com or a GHES host; the API base "+
				"derives from it). Defaults to $COMPASS_FORGE_HOST, then github.com."),
	}
}

// resolve applies the flag-then-env precedence to each forge flag and delegates
// to resolveForge (the pure input->output core, unit-tested directly).
func (f forgeFlags) resolve() (server.ForgeConfig, error) {
	return resolveForge(
		firstNonEmpty(*f.repos, os.Getenv("COMPASS_FORGE_REPOS")),
		*f.poll || envTrue(os.Getenv("COMPASS_FORGE_POLL")),
		firstNonEmpty(*f.interval, os.Getenv("COMPASS_FORGE_POLL_INTERVAL")),
		firstNonEmpty(*f.secret, os.Getenv("COMPASS_FORGE_SECRET")),
		firstNonEmpty(*f.host, os.Getenv("COMPASS_FORGE_HOST")),
	)
}

// resolveForge turns the five forge flags (already flag-then-env resolved) into
// the ServeConfig.Forge surface, mirroring resolveNetworkDoor's shape: pure
// input->output, no I/O. The repos string is a comma-separated owner/name list;
// each entry is validated (garbage is a startup error) and lowercased for GITHUB
// so Owner/Name and owner/name collapse to one target. An interval that is set
// but unparseable or non-positive is a startup error; empty leaves it zero for
// server-side defaulting. Empty host/secret likewise default server-side.
func resolveForge(repos string, poll bool, interval, secret, host string) (server.ForgeConfig, error) {
	seed, err := parseForgeRepos(repos)
	if err != nil {
		return server.ForgeConfig{}, err
	}
	var pollInterval time.Duration
	if interval != "" {
		d, perr := time.ParseDuration(interval)
		if perr != nil {
			return server.ForgeConfig{}, fmt.Errorf("invalid --forge-poll-interval %q: %w", interval, perr)
		}
		if d <= 0 {
			return server.ForgeConfig{}, fmt.Errorf("invalid --forge-poll-interval %q: must be positive", interval)
		}
		pollInterval = d
	}
	return server.ForgeConfig{
		Host:         host,
		SeedRepos:    seed,
		Poll:         poll,
		SecretName:   secret,
		PollInterval: pollInterval,
	}, nil
}

// parseForgeRepos splits a comma-separated owner/name list, validating and
// lowercasing each entry (GITHUB owner/name is case-insensitive-but-preserving,
// so Owner/Name and owner/name must collapse to one PK row). Blank entries are
// skipped; a malformed entry is a startup error. An empty input yields a nil
// slice (no seed).
func parseForgeRepos(repos string) ([]string, error) {
	if strings.TrimSpace(repos) == "" {
		return nil, nil
	}
	var out []string
	for raw := range strings.SplitSeq(repos, ",") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		owner, name, ok := strings.Cut(trimmed, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid --forge-repos entry %q: want \"owner/name\"", raw)
		}
		out = append(out, strings.ToLower(trimmed))
	}
	return out, nil
}

// firstNonEmpty returns a if it is non-empty, else b — the flag-then-env
// precedence used across the server config.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// envTrue reports whether an env value is a truthy toggle ("1"/"true", any case).
func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
