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

	// S3 archive tier: each flag falls back to its $COMPASS_S3_* env, mirroring
	// the DATABASE_DSN precedence. All-optional: an absent endpoint/bucket leaves
	// the archive tier unconfigured and the server boots socket-only (the store's
	// nil object-store guard fails a flush loudly only if one is ever attempted).
	s3Config := server.S3Config{
		Endpoint:  firstNonEmpty(*s3EndpointFlag, os.Getenv("COMPASS_S3_ENDPOINT")),
		Bucket:    firstNonEmpty(*s3BucketFlag, os.Getenv("COMPASS_S3_BUCKET")),
		AccessKey: firstNonEmpty(*s3AccessKeyFlag, os.Getenv("COMPASS_S3_ACCESS_KEY")),
		SecretKey: firstNonEmpty(*s3SecretKeyFlag, os.Getenv("COMPASS_S3_SECRET_KEY")),
		Region:    firstNonEmpty(*s3RegionFlag, os.Getenv("COMPASS_S3_REGION")),
		UseTLS:    *s3UseTLSFlag || envTrue(os.Getenv("COMPASS_S3_USE_TLS")),
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
	// stderr can tell a graceful drain from a hard death. A dedicated registration
	// rather than ctx.Done(), so it fires only on a real signal — not when Serve
	// returns on its own and the deferred stop() then cancels ctx.
	drainSig := make(chan os.Signal, 1)
	signal.Notify(drainSig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(drainSig)
	go func() {
		if _, ok := <-drainSig; ok {
			slog.Info("shutdown signal received; draining")
		}
	}()

	return server.Serve(ctx, server.ServeConfig{
		SocketPath:  socketPath,
		Version:     version,
		DevHTTP:     devHTTP,
		Listen:      listen,
		TLS:         tlsConfig,
		DatabaseDSN: databaseDSN,
		S3:          s3Config,
	})
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
