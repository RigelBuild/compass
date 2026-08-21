//go:build unix

// The authenticated network door: TLS termination, bearer + admin-gate
// interceptors, the bootstrap-admin token, and the network-door CORS policy.
// Kept out of serve.go so the serve loop's ordering stays readable; serve.go
// wires these helpers into the errgroup alongside the socket and dev doors.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// adminTokenFile is the basename of the 0600 file the bootstrap-admin token is
// written to under the state dir. The operator reads the freshly minted bearer
// token from here; it is never logged.
const adminTokenFile = "admin-token"

// boundListeners holds the TCP listeners eagerly bound before any on-disk
// state, plus the network door's validated TLS config. Binding up front means a
// bad address, an in-use port, or a bad keypair fails Serve before it creates a
// socket, directory, or admin-token file — so a startup failure leaves nothing
// behind. A nil listener means that door is off (dev unless --dev-http, network
// unless --listen).
type boundListeners struct {
	dev     net.Listener
	network net.Listener
	netTLS  *tls.Config
}

// close releases every bound listener. Safe on a partially-populated value
// (closeListener tolerates nil), so it is the single cleanup path for any
// startup error after binding.
func (b boundListeners) close() {
	closeListener(b.dev)
	closeListener(b.network)
}

// bindListeners eagerly binds the optional dev and network-door TCP listeners
// (and loads the network door's TLS keypair) before Serve touches disk. On any
// error it closes whatever it already bound and returns, so the caller never
// sees a half-bound value. The dev endpoint must be loopback (defense in depth;
// the CLI checks it too) and the network door requires TLS (a bearer token over
// cleartext is credential disclosure).
func bindListeners(cfg ServeConfig) (boundListeners, error) {
	var b boundListeners
	if cfg.DevHTTP != nil {
		if !cfg.DevHTTP.Addr().IsLoopback() {
			return boundListeners{}, fmt.Errorf("dev_http must be a loopback address (127.0.0.1 or ::1), got %s", cfg.DevHTTP)
		}
		l, err := net.Listen("tcp", cfg.DevHTTP.String())
		if err != nil {
			return boundListeners{}, fmt.Errorf("binding dev gRPC-Web endpoint at %s: %w", cfg.DevHTTP, err)
		}
		b.dev = l
	}
	if cfg.Listen != "" {
		t, err := loadNetworkTLS(cfg.TLS)
		if err != nil {
			b.close()
			return boundListeners{}, err
		}
		l, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			b.close()
			return boundListeners{}, fmt.Errorf("binding network door at %s: %w", cfg.Listen, err)
		}
		b.network, b.netTLS = l, t
	}
	return b, nil
}

// loadNetworkTLS validates the operator-provisioned PEM paths and loads them
// into a *tls.Config for the network door. It is called EARLY in Serve (before
// any on-disk state) so a missing/unreadable/invalid cert fails Serve up front,
// leaving nothing behind. NextProtos is left to http.Server, which advertises
// ALPN h2 from the server's Protocols (see networkProtocols) so the door is
// HTTP/2-native and gRPC-Web negotiates h2 over the same port.
func loadNetworkTLS(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil || cfg.CertPath == "" || cfg.KeyPath == "" {
		// A bearer token over cleartext is credential disclosure, so TLS is
		// required whenever the network door is enabled. The CLI checks this too;
		// this is defense in depth against a caller that set Listen without TLS.
		return nil, errors.New("network door requires TLS: both a certificate and a key are required with --listen")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading TLS keypair (cert %q, key %q): %w", cfg.CertPath, cfg.KeyPath, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// TLS 1.3 minimum: this door is a new 2026 internet-facing surface whose
		// bearer-token confidentiality rests entirely on TLS, and its only client
		// is the controlled compass client (no legacy-browser compat burden), so
		// require 1.3 to drop the 1.2 downgrade/legacy-cipher surface entirely.
		MinVersion: tls.VersionTLS13,
	}, nil
}

// networkProtocols enables HTTP/1.1 and encrypted HTTP/2 (ALPN h2) on the
// network door. Unlike the socket/dev doors (cleartext h2c), the network door
// terminates TLS, so HTTP/2 is negotiated via ALPN: http.Server advertises "h2"
// in the TLS handshake from these protocols, and gRPC-Web browser clients
// negotiate h2 over the same port.
func networkProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	return p
}

// networkCORS is the single-origin gRPC-Web CORS policy for the network door.
// Unlike devCORS (any origin, a loopback dev convenience), the network door is
// internet-facing, so it exposes exactly the one operator-configured origin. It
// additionally allows the Authorization request header — the network door
// authenticates with a bearer token, so a cross-origin browser client must be
// permitted to send it — and exposes the gRPC-Web status trailers so the client
// can read grpc-status.
func networkCORS(origin string) *cors.Cors {
	return cors.New(cors.Options{
		AllowedOrigins:   []string{origin},
		AllowedMethods:   connectcors.AllowedMethods(),
		AllowedHeaders:   append(connectcors.AllowedHeaders(), "Authorization"),
		ExposedHeaders:   connectcors.ExposedHeaders(),
		AllowCredentials: false,
	})
}

// networkBodyReadTimeout bounds how long the whole request body of a
// bounded-body RPC may take to arrive on the network door. It sits well above
// the 10s ReadHeaderTimeout so a genuinely slow-but-real client is unaffected,
// while a slow-body (slowloris) drip is cut. Every compass.v1 RPC that is not
// in bodyDeadlineExempt carries a small control-plane protobuf, so no legitimate
// caller needs longer.
const networkBodyReadTimeout = 30 * time.Second

// bodyDeadlineExempt is the set of procedure paths whose REQUEST body is
// legitimately long-lived, so a body-read deadline must NOT apply to them: the
// Runner streams its request half for the whole life of the connection. Both
// live on the internal RunnerService door:
//   - Sessions (bidi): the Runner's request stream carries command results
//     upward for as long as it is enrolled.
//   - PublishEvents (client-stream): the Runner drips agent event frames upward
//     continuously.
//
// Everything else on the network door — every unary RPC, both server-streams
// (SubscribeEvents/SubscribeComms, whose long-lived half is the RESPONSE, not
// the request), and RunnerService.Enroll (unary) — has a bounded request body,
// so the deadline protects them without ever cutting a legitimate call.
//
// Residual risk (accepted, tracked as a SEA-1298 follow-up): the exemption is
// keyed on r.URL.Path and applied before authentication, so an UNAUTHENTICATED
// client can still slow-drip a request body to these two paths with no deadline
// armed. The bearer interceptor rejects it on the headers, but the HTTP-layer
// body drip sits below auth and IdleTimeout does not reap an actively-dripping
// connection. This is a strict improvement over the prior state (every path was
// exposed); closing it fully needs an auth-gated deadline (arm on connect, clear
// only once the Runner handshake authenticates) or per-IP/per-connection limits
// on the network door, neither of which connect exposes a clean seam for.
var bodyDeadlineExempt = map[string]struct{}{
	compassv1internalconnect.RunnerServiceSessionsProcedure:      {},
	compassv1internalconnect.RunnerServicePublishEventsProcedure: {},
}

// withBodyReadDeadline wraps the network-door handler to close the slow-body DoS
// window that ReadHeaderTimeout leaves open (SEA-1298): it sets a per-request
// read deadline via http.ResponseController, so a client that sends headers
// promptly then drips the body no longer ties up a connection. The deadline is
// per-HTTP/2-stream (Go 1.20+ SetReadDeadline semantics), so one slow request
// does not disturb other streams multiplexed on the same TLS connection.
//
// It is the OUTERMOST wrapper (above CORS), at the HTTP-body layer where the
// drip happens — below connect's message decode — so it is a plain
// http.Handler middleware rather than a connect interceptor. Requests to a
// bodyDeadlineExempt procedure are passed through untouched, keeping the
// long-lived Runner request streams alive.
func withBodyReadDeadline(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, exempt := bodyDeadlineExempt[r.URL.Path]; !exempt {
			// SetReadDeadline bounds the request-body read. A failure means the
			// server does not support deadlines on this connection (never true
			// for the network door's TLS/HTTP server: withBodyReadDeadline is
			// outermost and rs/cors forwards the raw ResponseWriter, so
			// NewResponseController reaches the underlying *response, which
			// implements SetReadDeadline). So there is nothing to recover — fall
			// through and serve rather than reject a legitimate request. But a
			// future middleware inserted between this wrapper and the server
			// that wraps w without an Unwrap() would silently disable the
			// slow-body protection, so log the impossible-but-load-bearing
			// failure rather than let it vanish without a trace.
			rc := http.NewResponseController(w)
			if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
				slog.Warn("network door body-read deadline not armed; slow-body protection disabled for this request",
					"err", err, "path", r.URL.Path)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// buildNetworkServer mints and writes the bootstrap admin token (0600 under the
// state dir, so a socket-only start leaves none behind), then constructs the
// authenticated network door: the compass.v1 CompassService and CommsService
// handlers behind the bearer interceptors (outer, they authenticate and inject
// the caller) and the admin gate (inner, it rejects a non-admin on the
// privileged session RPCs), plus the internal RunnerService door a Runner
// enrolls over — behind its own Runner-subject bearer interceptor, sharing the
// same auth.ResolveToken resolver but Kind-gated to a Runner token (an account
// token is Unauthenticated there, and a Runner token is Unauthenticated on the
// account/comms doors: the OQ7 cross-door rule). Optionally wrapped in the
// single-origin network CORS policy. It does not bind or serve — the listener is
// already bound (boundListeners) — so on a token error the caller owns listener
// cleanup.
func buildNetworkServer(
	ctx context.Context,
	cfg ServeConfig,
	svc *service,
	commsSvc compassv1connect.CommsServiceHandler,
	secretsSvc compassv1connect.SecretsServiceHandler,
	hub *runnerhub.Hub,
	st *store.Store,
	adminID store.AccountID,
	netTLS *tls.Config,
	resolver secrets.Resolver,
) (*http.Server, error) {
	handle := cfg.resolvedAdminHandle()
	stateDir := cfg.StateDir
	if stateDir == "" {
		// Default to the socket's parent dir; a bare-filename socket has no
		// parent (parentDir returns ""), so fall back to the current dir.
		if stateDir = parentDir(cfg.SocketPath); stateDir == "" {
			stateDir = "."
		}
	}
	tokenPath, err := issueAndWriteAdminToken(ctx, st, adminID, stateDir)
	if err != nil {
		return nil, err
	}
	// Log the path, never the token: a logged bearer credential lets anyone
	// who can read process output or aggregated logs impersonate the admin.
	slog.Info("network door bootstrap admin token written",
		"path", tokenPath, "handle", handle, "listen", cfg.Listen)

	interceptors := connect.WithInterceptors(
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(adminID),
	)
	netPath, netHandler := compassv1connect.NewCompassServiceHandler(svc, interceptors)
	netCommsPath, netCommsHandler := compassv1connect.NewCommsServiceHandler(commsSvc, interceptors)
	netMux := http.NewServeMux()
	netMux.Handle(netPath, netHandler)
	netMux.Handle(netCommsPath, netCommsHandler)
	// SecretsService rides the same bearer + admin-gate chain: the gate classifies
	// its 3 procedures authenticatedOpen, so any authenticated account clears it and
	// the handler enforces the user-only writes / user-or-agent list.
	netSecretsPath, netSecretsHandler := compassv1connect.NewSecretsServiceHandler(secretsSvc, interceptors)
	netMux.Handle(netSecretsPath, netSecretsHandler)

	// The internal RunnerService door: the surface a Runner dials out to (Enroll +
	// Sessions bidi + PublishEvents client-stream). It is mounted only here, on the
	// authenticated network door — a Runner is a remote host, so it reaches the
	// server over TLS, never the loopback socket. Its own bearer interceptor
	// (runnerhub, applied by NewMountedHandler) authenticates every RPC through the
	// shared auth.ResolveToken resolver, Kind-gated to a Runner-subject token: an
	// account token is Unauthenticated here and a Runner token is Unauthenticated on
	// the CompassService/CommsService doors above (OQ7 cross-door rejection). The
	// admin gate is not applied — the RunnerService is not part of the account
	// contract, and the Kind gate is its whole authorization.
	runnerResolve := func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		return auth.ResolveToken(ctx, st, presented, want)
	}
	// configStore is nil until the SEA-1568 T1 fleet config-bundle store (SEA-1624)
	// lands: FetchAgentConfig then fails CodeFailedPrecondition (a no-config-surface
	// server, tolerated by the Runner), exactly as a nil resolver does for
	// FetchSecrets. T1 wires the real *store.Store (it satisfies AgentConfigStore
	// via CurrentAgentConfig) here.
	runnerPath, runnerHandler := runnerhub.NewMountedHandler(hub, runnerResolve, resolver, nil)
	netMux.Handle(runnerPath, runnerHandler)
	var netRoot http.Handler = netMux
	if cfg.CORSAllowedOrigin != "" {
		// Network door defaults closed: CORS only for the one explicit
		// operator-configured browser origin.
		netRoot = networkCORS(cfg.CORSAllowedOrigin).Handler(netMux)
	}
	// Outermost: bound the request-body read so a slow-body drip cannot tie up
	// a connection (SEA-1298). Wraps whatever netRoot is above (CORS or the bare
	// mux) at the HTTP-body layer; the long-lived Runner request streams are
	// exempt (bodyDeadlineExempt).
	netRoot = withBodyReadDeadline(netRoot, networkBodyReadTimeout)
	return &http.Server{
		Handler:   netRoot,
		TLSConfig: netTLS,
		Protocols: networkProtocols(),
		// G112: the network door is the internet-facing surface, so bound the
		// header read and idle connection lifetime to close the slow-loris DoS
		// window (the UDS/dev doors are loopback/local). The request-body half
		// of that window (a client that sends headers promptly then drips the
		// body, SEA-1298) is closed by withBodyReadDeadline above rather than a
		// blunt http.Server.ReadTimeout: a ReadTimeout caps the whole request
		// lifetime and would kill the long-lived Runner request streams, so the
		// body deadline is applied per-request and skips those (bodyDeadlineExempt).
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}

// issueAndWriteAdminToken mints a bearer token for the bootstrap admin and
// writes it 0600 under stateDir. It runs only when the network door is enabled
// (--listen): the token is the network-door credential, so socket-only startup
// (whose credential is the 0600 socket) mints none. The token is written to
// disk (never logged) for the operator to read; the returned path is logged (the
// path, not the token). A failed write leaves no partial credential.
func issueAndWriteAdminToken(ctx context.Context, st *store.Store, adminID store.AccountID, stateDir string) (string, error) {
	token, err := auth.IssueAccountToken(ctx, st, adminID)
	if err != nil {
		return "", err
	}
	return writeTokenFile(stateDir, token)
}

// writeTokenFile writes token to a 0600 file named adminTokenFile under dir,
// atomically: a temp file in the same directory (born 0600) is written, synced,
// and renamed over the final path, so a reader never observes a partial token
// and a crash mid-write leaves either the old file or the new one, never a
// truncated credential. Returns the final path. A failed write removes the temp
// file so no partial credential is left behind.
func writeTokenFile(dir, token string) (string, error) {
	if err := ensurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("ensuring state dir %q for admin token: %w", dir, err)
	}
	final := filepath.Join(dir, adminTokenFile)
	tmp, err := os.CreateTemp(dir, adminTokenFile+".*")
	if err != nil {
		return "", fmt.Errorf("creating temp admin-token file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// os.CreateTemp already creates the file 0600; the explicit chmod pins it
	// regardless of umask, belt-and-suspenders for a live credential.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("chmod 0600 admin-token temp file: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("writing admin token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("syncing admin token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("closing admin-token temp file: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("renaming admin-token file into place at %q: %w", final, err)
	}
	return final, nil
}
