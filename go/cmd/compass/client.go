//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/runner"
)

// rpcTimeout bounds a single operator RPC: generous for a bundle push over a
// local door, short enough that a wedged connection fails visibly.
const rpcTimeout = 60 * time.Second

// bearerToken stamps the admin bearer credential on every outbound RPC (unary
// and streaming) so the Server door authenticates it (mirrors
// internal/runner/runner.go:71-94). Streaming is stamped too so a future
// streaming operator RPC cannot silently go out unauthenticated.
type bearerToken struct {
	token string
}

func (b *bearerToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b *bearerToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b *bearerToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// connConfig is the resolved connection material for one CLI invocation: where
// the Server is, the admin token, and the optional custom trust root.
type connConfig struct {
	serverAddr string
	token      string
	caPath     string
}

// resolveConn assembles the connection config from the command's flags with the
// ratified precedence. server-addr and ca resolve flag -> env -> config file
// (they are not secrets); the admin token resolves env -> --token-file ONLY
// (never a flag: a flag leaks into the process table). Errors name the missing
// input and how to supply it.
func resolveConn(cmd *cobra.Command) (connConfig, error) {
	// The config file is the lowest-precedence layer, loaded (when --config is
	// given) purely as the fallback source. Precedence across flag/env/file is
	// applied explicitly in layered below rather than through viper's implicit
	// tier ordering, so a future fourth layer can't silently reorder it.
	var file *viper.Viper
	if cfgPath, _ := cmd.Flags().GetString("config"); cfgPath != "" {
		file = viper.New()
		file.SetConfigFile(cfgPath)
		if err := file.ReadInConfig(); err != nil {
			return connConfig{}, fmt.Errorf("reading config file %q: %w", cfgPath, err)
		}
	}

	addr := layered(cmd, "server-addr", "COMPASS_SERVER_ADDR", file)
	if addr == "" {
		return connConfig{}, errors.New(
			"a server address is required: pass --server-addr, set $COMPASS_SERVER_ADDR, or supply it in --config")
	}
	if err := checkServerAddrScheme(addr); err != nil {
		return connConfig{}, err
	}

	ca := layered(cmd, "ca", "COMPASS_ADMIN_CA", file)

	token, err := resolveToken(cmd)
	if err != nil {
		return connConfig{}, err
	}
	return connConfig{serverAddr: addr, token: token, caPath: ca}, nil
}

// checkServerAddrScheme rejects a remote http:// server address so the admin
// bearer token the interceptor stamps on every call is never sent in cleartext.
// http is allowed only for a loopback host (localhost, 127.0.0.1, [::1]) so the
// local dogfood door still works; any other host must be https://.
func checkServerAddrScheme(addr string) error {
	u, err := url.Parse(addr)
	if err != nil {
		return fmt.Errorf("parsing server address %q: %w", addr, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf(
		"server address %q must be https:// so the admin token is not sent in cleartext (http is allowed only for localhost)",
		addr)
}

// isLoopbackHost reports whether host is a loopback name/address for which
// cleartext http is acceptable (the local dogfood door). It accepts the name
// "localhost" case-insensitively and any loopback IP (127.0.0.0/8, ::1, and
// their equivalent forms) via net.ParseIP; a non-loopback host is never treated
// as loopback, so a suffix spoof like 127.0.0.1.evil.com cannot bypass it.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// layered resolves a non-secret connection value with the ratified precedence:
// an explicitly-set flag wins, then the environment variable, then the config
// file, then empty. Viper is used only to parse the config file, never for its
// own precedence tiers.
func layered(cmd *cobra.Command, flagName, envName string, file *viper.Viper) string {
	if f := cmd.Flags().Lookup(flagName); f != nil && f.Changed {
		return f.Value.String()
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	if file != nil {
		return file.GetString(flagName)
	}
	return ""
}

// resolveToken reads the admin bearer token from $COMPASS_ADMIN_TOKEN, else from
// the file named by --token-file. It is deliberately not a flag value: a bearer
// credential on the command line leaks into the process table (the load-bearing
// convention at cmd/compass-runner/main.go:105-107).
func resolveToken(cmd *cobra.Command) (string, error) {
	if tok := os.Getenv("COMPASS_ADMIN_TOKEN"); tok != "" {
		return tok, nil
	}
	path, _ := cmd.Flags().GetString("token-file")
	if path == "" {
		return "", errors.New(
			"an admin token is required: set $COMPASS_ADMIN_TOKEN or pass --token-file " +
				"(a bearer token is never a flag — it would leak into the process table)")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is an operator-provided CLI flag, the whole point of --token-file
	if err != nil {
		return "", fmt.Errorf("reading --token-file %q: %w", path, err)
	}
	tok := trimToken(raw)
	if tok == "" {
		return "", fmt.Errorf("--token-file %q is empty", path)
	}
	return tok, nil
}

// trimToken strips surrounding whitespace/newlines from a token file so a file
// written with or without a trailing newline both yield the raw credential.
func trimToken(raw []byte) string {
	return strings.TrimSpace(string(raw))
}

// newClient constructs the CompassService client for the resolved connection: a
// custom-root http.Client when --ca is set (the self-signed dogfood door), else
// http.DefaultClient (system roots), with the bearer interceptor stamping the
// admin token on every call (mirrors internal/runner Dial).
func newClient(cfg connConfig) (compassv1connect.CompassServiceClient, error) {
	var httpClient connect.HTTPClient = http.DefaultClient
	if cfg.caPath != "" {
		c, err := runner.NewCATrustClient(cfg.caPath)
		if err != nil {
			return nil, err
		}
		httpClient = c
	}
	return compassv1connect.NewCompassServiceClient(
		httpClient, cfg.serverAddr,
		connect.WithInterceptors(&bearerToken{token: cfg.token}),
	), nil
}

// dialClient resolves the connection config and builds the client in one step —
// the shared prelude every agent-config subcommand runs.
func dialClient(cmd *cobra.Command) (compassv1connect.CompassServiceClient, error) {
	cfg, err := resolveConn(cmd)
	if err != nil {
		return nil, err
	}
	return newClient(cfg)
}
