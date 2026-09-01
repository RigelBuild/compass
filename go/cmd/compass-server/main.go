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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/server"
)

// version is the server build + contract version reported by GetServerInfo (the
// workspace version 0.1.0); override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

// apiVersion is the compass.v1 contract version, logged at startup alongside the
// build version (the authoritative wire value is reported by the GetServerInfo
// RPC).
const apiVersion = "compass.v1"

// errUsage marks a CLI usage error (a bad flag) that buildServeConfig's FlagSet
// has ALREADY reported to stderr (usage + the parse error). run() returns it so
// main() can exit non-zero without re-logging it through slog — a typo'd flag is
// a usage mistake, not a server crash, and double-reporting it as a structured
// error line reads like the latter.
var errUsage = errors.New("cli usage error")

func main() {
	err := run()
	if err == nil {
		return
	}
	if errors.Is(err, errUsage) {
		// The FlagSet already printed usage + the parse error to stderr; exit with
		// the conventional usage code without re-reporting it as a server error.
		os.Exit(2)
	}
	slog.Error("compass-server exited with an error", "error", err)
	os.Exit(1)
}

func run() error {
	cfg, showVersion, err := buildServeConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		// -h/--help: the FlagSet already printed usage; a clean exit, not an error.
		return nil
	}
	if err != nil {
		return err
	}
	if showVersion {
		// Genuine CLI output — the --version result printed to stdout, not a
		// diagnostic — so an explicit writer, never slog and never a bare
		// fmt.Print (go-no-fmt-print-logging). stdout write errors on --version
		// are not actionable (the process is exiting cleanly regardless), so the
		// return is deliberately discarded.
		_, _ = fmt.Fprintf(os.Stdout, "compass-server %s\n", version)
		return nil
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	slog.Info("compass-server starting",
		"version", version,
		"api", apiVersion,
		"socket", cfg.SocketPath,
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

	// OTel emission (T4b): endpoint-gated off the ENV-only knob. When
	// OTEL_EXPORTER_OTLP_ENDPOINT is empty, Setup* install no provider and return
	// no-op shutdowns, so this is zero-overhead on the shipped socket-only path.
	otelCfg := otel.Config{
		ServiceName:    "compass-server",
		ServiceVersion: version,
		Endpoint:       cfg.OtelEndpoint,
	}
	traceShutdown, err := otel.SetupTracerProvider(ctx, otelCfg)
	if err != nil {
		return fmt.Errorf("otel: tracer provider: %w", err)
	}
	// The drain ctx is already cancelled by the time these defers fire (the signal
	// that ends Serve is the same one that cancels ctx), so a raw ctx.Shutdown
	// would abort its final ForceFlush and drop the last batch. Sever the
	// cancellation and bound the flush at 2s (design.md: mirror the agent's 2s
	// shutdown bound), derived HERE at fire time so the deadline is not consumed
	// by the process lifetime.
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = traceShutdown(sctx) // best-effort flush on drain; a collector error here is not actionable at exit
	}()
	meterShutdown, err := otel.SetupMeterProvider(ctx, otelCfg)
	if err != nil {
		return fmt.Errorf("otel: meter provider: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = meterShutdown(sctx) // best-effort flush on drain; a collector error here is not actionable at exit
	}()

	return server.Serve(ctx, cfg)
}

// buildServeConfig parses args into the ServeConfig the serve loop consumes,
// returning showVersion=true when --version was requested (the caller prints the
// version and exits). Split out of run() so the whole flag→config mapping is
// unit-testable without binding a socket or serving — a bad flag combination is
// an error here, not deep in Serve. It uses a private FlagSet (ContinueOnError)
// rather than the global flag.CommandLine, so a parse error returns up (never
// os.Exit) and tests never race on global flag state; a -h/--help request comes
// back as flag.ErrHelp with usage already printed.
func buildServeConfig(args []string) (server.ServeConfig, bool, error) {
	fs := flag.NewFlagSet("compass-server", flag.ContinueOnError)
	f := registerServeFlags(fs)
	forge := registerForgeFlags(fs)
	showVersion := fs.Bool("version", false, "Print the version and exit.")
	if err := fs.Parse(args); err != nil {
		// Wrap with errUsage AND the original: the FlagSet already printed usage +
		// the message, so main() exits 2 without re-logging. Multi-%w keeps
		// flag.ErrHelp visible to run()'s help check (a clean exit, not an error).
		return server.ServeConfig{}, false, fmt.Errorf("%w: %w", errUsage, err)
	}
	if *showVersion {
		return server.ServeConfig{}, true, nil
	}

	socketPath := *f.socket
	if socketPath == "" {
		resolved, err := server.DefaultSocketPath()
		if err != nil {
			return server.ServeConfig{}, false, fmt.Errorf("resolving default socket path: %w", err)
		}
		socketPath = resolved
	}

	var devHTTP *netip.AddrPort
	if *f.devHTTP != "" {
		addr, err := netip.ParseAddrPort(*f.devHTTP)
		if err != nil {
			return server.ServeConfig{}, false, fmt.Errorf("parsing --dev-http address %q: %w", *f.devHTTP, err)
		}
		// A dev endpoint must stay on the loopback interface — it has no auth and
		// exists only for the local browser dev server; binding it on a routable
		// address would expose the server to the network. server.Serve re-asserts
		// this invariant, but fail fast here rather than deep in the serve loop.
		if !addr.Addr().IsLoopback() {
			return server.ServeConfig{}, false, fmt.Errorf("--dev-http must bind a loopback address (127.0.0.1 or ::1), got %s", addr)
		}
		devHTTP = &addr
	}

	listen, tlsConfig, err := resolveNetworkDoor(*f.listen, *f.tlsCert, *f.tlsKey)
	if err != nil {
		return server.ServeConfig{}, false, err
	}

	// The network door's CORS contract is exactly one explicit origin, never a
	// wildcard — it is internet-facing, so an any-origin door is exactly the state
	// the design forbids. rs/cors treats a literal "*" (or a '*' pattern) as
	// all-origins, so a copy-pasted "*" would silently open the door with no error.
	// Reject any '*' up front rather than deep in the CORS layer.
	if strings.Contains(*f.corsAllowedOrigin, "*") {
		return server.ServeConfig{}, false, fmt.Errorf(
			"--cors-allowed-origin must be a single explicit origin, not a wildcard: %q", *f.corsAllowedOrigin)
	}

	databaseDSN := firstNonEmpty(*f.database, os.Getenv("COMPASS_DATABASE_DSN"))
	if databaseDSN == "" {
		return server.ServeConfig{}, false, errors.New("a Postgres DSN is required: pass --database or set $COMPASS_DATABASE_DSN")
	}

	// S3 archive tier: each flag falls back to its $COMPASS_S3_* env, mirroring
	// the DATABASE_DSN precedence. All-optional: an absent endpoint/bucket leaves
	// the archive tier unconfigured and the server boots socket-only.
	s3Config := server.S3Config{
		Endpoint:  firstNonEmpty(*f.s3Endpoint, os.Getenv("COMPASS_S3_ENDPOINT")),
		Bucket:    firstNonEmpty(*f.s3Bucket, os.Getenv("COMPASS_S3_BUCKET")),
		AccessKey: firstNonEmpty(*f.s3AccessKey, os.Getenv("COMPASS_S3_ACCESS_KEY")),
		SecretKey: firstNonEmpty(*f.s3SecretKey, os.Getenv("COMPASS_S3_SECRET_KEY")),
		Region:    firstNonEmpty(*f.s3Region, os.Getenv("COMPASS_S3_REGION")),
		UseTLS:    *f.s3UseTLS || envTrue(os.Getenv("COMPASS_S3_USE_TLS")),
	}

	// Forge poll driver (RIG-1810): flag-then-env, all-optional (see forge.resolve).
	forgeConfig, err := forge.resolve()
	if err != nil {
		return server.ServeConfig{}, false, err
	}

	return server.ServeConfig{
		SocketPath:        socketPath,
		Version:           version,
		DevHTTP:           devHTTP,
		Listen:            listen,
		TLS:               tlsConfig,
		DatabaseDSN:       databaseDSN,
		S3:                s3Config,
		Forge:             forgeConfig,
		StateDir:          *f.stateDir,
		AdminHandle:       *f.adminHandle,
		CORSAllowedOrigin: *f.corsAllowedOrigin,
		PublicURL:         firstNonEmpty(*f.publicURL, os.Getenv("COMPASS_PUBLIC_URL")),
		// ENV-ONLY knob (Matt 2026-08-28): the OTLP exporter and the enable-gate
		// read one source, so no --otel-endpoint flag. Empty = tracing off.
		OtelEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}, false, nil
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

// serveFlags holds the pointers for the core (non-forge) compass-server CLI
// flags. Registered as a group by registerServeFlags so buildServeConfig stays
// short and the flag→field assembly reads as one block (mirrors forgeFlags).
type serveFlags struct {
	socket            *string
	devHTTP           *string
	listen            *string
	tlsCert           *string
	tlsKey            *string
	database          *string
	s3Endpoint        *string
	s3Bucket          *string
	s3AccessKey       *string
	s3SecretKey       *string
	s3Region          *string
	s3UseTLS          *bool
	stateDir          *string
	adminHandle       *string
	corsAllowedOrigin *string
	publicURL         *string
}

// registerServeFlags declares the core compass-server flags on the given FlagSet
// and returns their pointers. Split out of buildServeConfig so the declaration
// boilerplate does not inflate it past the length budget.
func registerServeFlags(fs *flag.FlagSet) serveFlags {
	return serveFlags{
		socket: fs.String("socket", "",
			"Unix socket to serve compass.v1 on. Defaults to "+
				"$XDG_RUNTIME_DIR/compass/server.sock, falling back to "+
				"$HOME/.compass/server.sock."),
		devHTTP: fs.String("dev-http", "",
			"Dev only: also serve gRPC-Web on this loopback TCP address (e.g. "+
				"127.0.0.1:50051) for a browser dev server. Off by default; the "+
				"shipped path is socket-only. A non-loopback address is rejected."),
		listen: fs.String("listen", "",
			"Serve the authenticated gRPC network door on this TCP address (e.g. "+
				"0.0.0.0:8443). Off by default; the shipped local path is socket-only. "+
				"Requires --tls-cert and --tls-key together — a bearer token over "+
				"cleartext is credential disclosure."),
		tlsCert: fs.String("tls-cert", "",
			"PEM certificate file terminating TLS on the --listen network door. "+
				"Required with --listen."),
		tlsKey: fs.String("tls-key", "",
			"PEM private-key file terminating TLS on the --listen network door. "+
				"Required with --listen."),
		database: fs.String("database", "",
			"Postgres DSN for the store of record (e.g. postgres://user:pass@host/compass). "+
				"Defaults to $COMPASS_DATABASE_DSN."),
		s3Endpoint: fs.String("s3-endpoint", "",
			"S3-compatible object-store endpoint host[:port] (no scheme) for the "+
				"transcript archive tier (Garage/R2/MinIO/AWS). Defaults to "+
				"$COMPASS_S3_ENDPOINT. Absent = no archive tier (dev server still boots)."),
		s3Bucket: fs.String("s3-bucket", "",
			"Bucket archive segments are written under. Defaults to $COMPASS_S3_BUCKET."),
		s3AccessKey: fs.String("s3-access-key", "",
			"S3 access key. Defaults to $COMPASS_S3_ACCESS_KEY."),
		s3SecretKey: fs.String("s3-secret-key", "",
			"S3 secret key. Defaults to $COMPASS_S3_SECRET_KEY."),
		s3Region: fs.String("s3-region", "",
			"S3 region (e.g. us-east-1, or \"garage\" for Garage). Defaults to $COMPASS_S3_REGION."),
		s3UseTLS: fs.Bool("s3-use-tls", false,
			"Use https to the S3 endpoint. Defaults to $COMPASS_S3_USE_TLS "+
				"(\"1\"/\"true\"/\"yes\"/\"on\" = on)."),
		stateDir: fs.String("state-dir", "",
			"Directory the bootstrap-admin token file is written under (0600). "+
				"Defaults to the socket's parent directory."),
		adminHandle: fs.String("admin-handle", "",
			"Handle of the bootstrap-admin account created (or found) at startup. "+
				"Defaults to \"admin\". A handle that already names a non-admin "+
				"account fails startup rather than elevating it."),
		corsAllowedOrigin: fs.String("cors-allowed-origin", "",
			"Single browser origin the network door exposes gRPC-Web CORS for "+
				"(e.g. https://host.example.ts.net). Empty = no CORS on the network door."),
		publicURL: fs.String("public-url", "",
			"Per-deployment public base URL Compass is reachable at (e.g. "+
				"https://host.example.ts.net), the base for the Linear Agent "+
				"responder's \"Open in Compass\" deep links. Falls back to "+
				"$COMPASS_PUBLIC_URL. No default: a deployment that consumes "+
				"Linear webhooks must set it."),
	}
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

// forgeFlags holds the RIG-1810/RIG-2883 forge CLI flag pointers, registered as
// a group so run() stays short (they mirror the S3 flag set's precedence).
type forgeFlags struct {
	repos                  *string
	host                   *string
	appID                  *string
	installationID         *string
	appKeySecret           *string
	appWebhook             *string
	reviewerAppID          *string
	reviewerInstallationID *string
	reviewerAppKeySecret   *string
	linearClientID         *string
	linearClientSecret     *string
	linearWebhook          *string
}

// registerForgeFlags declares the forge flags on the given FlagSet and returns
// their pointers, so buildServeConfig registers them alongside the rest on its
// private FlagSet rather than the global flag.CommandLine.
func registerForgeFlags(fs *flag.FlagSet) forgeFlags {
	return forgeFlags{
		repos: fs.String("forge-repos", "",
			"Comma-separated owner/name repos to SEED into forge_repo_subscriptions "+
				"(RIG-2883 board ingestion). Defaults to $COMPASS_FORGE_REPOS. A declarative "+
				"seed reconciled at boot (bootstrap-only insert), NOT the live target "+
				"set — the table is authoritative after the first insert."),
		host: fs.String("forge-host", "",
			"Forge host the board lane binds (github.com or a GHES host; the API base "+
				"derives from it). Defaults to $COMPASS_FORGE_HOST, then github.com."),
		appID: fs.String("forge-app-id", "",
			"PRIMARY GitHub App id (numeric): serves board reads, notify reads, author "+
				"writes, board, and webhooks (2-App topology). Defaults to "+
				"$COMPASS_FORGE_APP_ID. Board ingestion runs iff this is set AND both App "+
				"secrets are declared; the forge-WRITE path additionally requires the "+
				"reviewer App."),
		installationID: fs.String("forge-installation-id", "",
			"PRIMARY GitHub App installation id the token is minted for. Defaults to "+
				"$COMPASS_FORGE_INSTALLATION_ID."),
		appKeySecret: fs.String("forge-app-key-secret", "",
			"Declared server_only secret NAME holding the PRIMARY App PEM private key "+
				"(the VALUE never crosses a flag). Defaults to "+
				"$COMPASS_FORGE_APP_KEY_SECRET."),
		appWebhook: fs.String("forge-app-webhook-secret", "",
			"Declared server_only secret NAME holding the webhook signing secret the "+
				"ingress verifies deliveries against. Defaults to "+
				"$COMPASS_FORGE_APP_WEBHOOK_SECRET."),
		reviewerAppID: fs.String("forge-reviewer-app-id", "",
			"REVIEWER GitHub App id (numeric): a distinct App identity serving ONLY the "+
				"reviewer write client (F1 author-cannot-approve-own-PR). Defaults to "+
				"$COMPASS_FORGE_REVIEWER_APP_ID. The forge-WRITE path runs iff this AND the "+
				"primary App are both configured. The reviewer App registers no webhook."),
		reviewerInstallationID: fs.String("forge-reviewer-app-installation-id", "",
			"REVIEWER GitHub App installation id the token is minted for. Defaults to "+
				"$COMPASS_FORGE_REVIEWER_APP_INSTALLATION_ID."),
		reviewerAppKeySecret: fs.String("forge-reviewer-app-key-secret", "",
			"Declared server_only secret NAME holding the REVIEWER App PEM private key "+
				"(the VALUE never crosses a flag; conventionally "+
				"FORGE_REVIEWER_APP_PRIVATE_KEY). Defaults to "+
				"$COMPASS_FORGE_REVIEWER_APP_KEY_SECRET."),
		linearClientID: fs.String("forge-linear-client-id", "",
			"Declared server_only secret NAME holding the Linear OAuth client id (the "+
				"client-credentials actor=app pair, the VALUE never crosses a flag). "+
				"Defaults to $COMPASS_FORGE_LINEAR_CLIENT_ID, then LINEAR_FORGE_CLIENT_ID. "+
				"The Linear write + notify lanes run iff BOTH this and the client secret "+
				"are declared."),
		linearClientSecret: fs.String("forge-linear-client-secret", "",
			"Declared server_only secret NAME holding the Linear OAuth client secret "+
				"(the VALUE never crosses a flag). Defaults to "+
				"$COMPASS_FORGE_LINEAR_CLIENT_SECRET, then LINEAR_FORGE_CLIENT_SECRET."),
		linearWebhook: fs.String("forge-linear-webhook-secret", "",
			"Declared server_only secret NAME holding the Linear webhook signing "+
				"secret the shared POST /webhooks/linear ingress verifies deliveries "+
				"against. Defaults to $COMPASS_FORGE_LINEAR_WEBHOOK_SECRET. The Linear "+
				"data-change/session arm runs iff this is declared, independent of the "+
				"GitHub App gate."),
	}
}

// resolve applies the flag-then-env precedence to each forge flag and delegates
// to resolveForge (the pure input->output core, unit-tested directly).
func (f forgeFlags) resolve() (server.ForgeConfig, error) {
	return resolveForge(forgeInputs{
		repos:                  firstNonEmpty(*f.repos, os.Getenv("COMPASS_FORGE_REPOS")),
		host:                   firstNonEmpty(*f.host, os.Getenv("COMPASS_FORGE_HOST")),
		appID:                  firstNonEmpty(*f.appID, os.Getenv("COMPASS_FORGE_APP_ID")),
		installationID:         firstNonEmpty(*f.installationID, os.Getenv("COMPASS_FORGE_INSTALLATION_ID")),
		appKeySecret:           firstNonEmpty(*f.appKeySecret, os.Getenv("COMPASS_FORGE_APP_KEY_SECRET")),
		appWebhook:             firstNonEmpty(*f.appWebhook, os.Getenv("COMPASS_FORGE_APP_WEBHOOK_SECRET")),
		reviewerAppID:          firstNonEmpty(*f.reviewerAppID, os.Getenv("COMPASS_FORGE_REVIEWER_APP_ID")),
		reviewerInstallationID: firstNonEmpty(*f.reviewerInstallationID, os.Getenv("COMPASS_FORGE_REVIEWER_APP_INSTALLATION_ID")),
		reviewerAppKeySecret:   firstNonEmpty(*f.reviewerAppKeySecret, os.Getenv("COMPASS_FORGE_REVIEWER_APP_KEY_SECRET")),
		linearClientID:         firstNonEmpty(*f.linearClientID, os.Getenv("COMPASS_FORGE_LINEAR_CLIENT_ID")),
		linearClientSecret:     firstNonEmpty(*f.linearClientSecret, os.Getenv("COMPASS_FORGE_LINEAR_CLIENT_SECRET")),
		linearWebhook:          firstNonEmpty(*f.linearWebhook, os.Getenv("COMPASS_FORGE_LINEAR_WEBHOOK_SECRET")),
	})
}

// forgeInputs is the already-resolved (flag-then-env) forge input set the pure
// resolveForge core maps onto server.ForgeConfig. A struct rather than a long
// positional list so a new knob is a named field, not another unlabeled arg.
type forgeInputs struct {
	repos                  string
	host                   string
	appID                  string
	installationID         string
	appKeySecret           string
	appWebhook             string
	reviewerAppID          string
	reviewerInstallationID string
	reviewerAppKeySecret   string
	linearClientID         string
	linearClientSecret     string
	linearWebhook          string
}

// resolveForge turns the forge inputs (already flag-then-env resolved) into the
// ServeConfig.Forge surface, mirroring resolveNetworkDoor's shape: pure
// input->output, no I/O. The repos string is a comma-separated owner/name list;
// each entry is validated (garbage is a startup error) and lowercased for GITHUB
// so Owner/Name and owner/name collapse to one target. App ids are parsed as
// int64 when set (garbage is a startup error); empty leaves them zero. Empty
// host / secret NAMEs are left zero for server-side defaulting.
func resolveForge(in forgeInputs) (server.ForgeConfig, error) {
	seed, err := parseForgeRepos(in.repos)
	if err != nil {
		return server.ForgeConfig{}, err
	}
	id, err := parseForgeInt(in.appID, "--forge-app-id")
	if err != nil {
		return server.ForgeConfig{}, err
	}
	instID, err := parseForgeInt(in.installationID, "--forge-installation-id")
	if err != nil {
		return server.ForgeConfig{}, err
	}
	reviewerID, err := parseForgeInt(in.reviewerAppID, "--forge-reviewer-app-id")
	if err != nil {
		return server.ForgeConfig{}, err
	}
	reviewerInstID, err := parseForgeInt(in.reviewerInstallationID, "--forge-reviewer-app-installation-id")
	if err != nil {
		return server.ForgeConfig{}, err
	}
	return server.ForgeConfig{
		Host:                     in.host,
		SeedRepos:                seed,
		LinearClientIDSecretName: in.linearClientID,
		LinearClientSecretName:   in.linearClientSecret,
		LinearWebhookSecretName:  in.linearWebhook,
		App: server.ForgeAppConfig{
			AppID:                id,
			InstallationID:       instID,
			AppPrivateKeySecret:  in.appKeySecret,
			AppWebhookSecretName: in.appWebhook,
		},
		ReviewerApp: server.ForgeAppConfig{
			AppID:               reviewerID,
			InstallationID:      reviewerInstID,
			AppPrivateKeySecret: in.reviewerAppKeySecret,
		},
	}, nil
}

// parseForgeInt parses an int64 flag value, treating empty as zero (unset) and a
// non-numeric value as a startup error naming the flag.
func parseForgeInt(v, flagName string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", flagName, v, err)
	}
	return n, nil
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

// firstNonEmpty returns the first non-empty argument, or "" when all are empty —
// the flag-then-env precedence used across the server config.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
