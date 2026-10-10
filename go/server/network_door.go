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
	"strings"
	"time"

	"connectrpc.com/connect"
	connectcors "connectrpc.com/cors"
	"connectrpc.com/otelconnect"
	"github.com/rs/cors"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// adminTokenFile is the basename of the 0600 file the bootstrap-admin token is
// kept under the state dir. The operator reads the bearer token from here; it
// is never logged.
const adminTokenFile = "admin-token"

// compassServiceMaxReadBytes caps a single inbound message on the CompassService
// network door (M1). It clears the largest legitimate message the mount carries
// — a PutAgentConfig bundle up to the store's 64 MiB decompressed cap — with
// headroom for wire framing, while bounding an operator's PutModelRegistry /
// PutAgentConfig against an unbounded in-memory buffer.
const compassServiceMaxReadBytes = 128 << 20 // 128 MiB

// siblingServiceMaxReadBytes caps a single inbound message on the CommsService
// and SecretsService mounts, which ride the same network door as CompassService
// but carry only small unary messages (a chat turn, a secret value) — none of
// the 64 MiB config-bundle scale CompassService's PutAgentConfig reaches (M2).
// connect-go imposes NO default read cap, and CommsService is authenticatedOpen
// (any member reaches PostMessage), so an uncapped sibling mount is STRICTLY
// more exposed than the admin-gated CompassService the round-1 fix already
// capped: an ordinary account could stream an arbitrarily large PostMessage the
// server buffers whole in memory. 16 MiB is generous over any legitimate
// message here while closing that hole — the guestd vsock.go:94 posture.
const siblingServiceMaxReadBytes = 16 << 20 // 16 MiB

// boundListeners holds the TCP listeners eagerly bound before any on-disk
// state, plus the network door's validated TLS config. Binding up front means a
// bad address, an in-use port, or a bad keypair fails Serve before it creates a
// socket, directory, or admin-token file — so a startup failure leaves nothing
// behind. A nil listener means that door is off (dev unless --dev-http, network
// unless --listen or an inherited listener).
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
// (and loads the network door's TLS keypair) before Serve touches disk; an
// inherited network listener arrives already bound. On any error it closes
// every listener it holds, so the caller never sees a half-bound value. The dev
// endpoint must be loopback and the network door requires TLS.
func bindListeners(cfg ServeConfig) (boundListeners, error) {
	var b boundListeners
	if cfg.DevHTTP != nil {
		if !cfg.DevHTTP.Addr().IsLoopback() {
			if cfg.ListenListener != nil {
				_ = cfg.ListenListener.Close()
			}
			return boundListeners{}, fmt.Errorf("dev_http must be a loopback address (127.0.0.1 or ::1), got %s", cfg.DevHTTP)
		}
		l, err := net.Listen("tcp", cfg.DevHTTP.String())
		if err != nil {
			if cfg.ListenListener != nil {
				_ = cfg.ListenListener.Close()
			}
			return boundListeners{}, fmt.Errorf("binding dev gRPC-Web endpoint at %s: %w", cfg.DevHTTP, err)
		}
		b.dev = l
	}
	if cfg.Listen != "" || cfg.ListenListener != nil {
		l := cfg.ListenListener
		if l == nil {
			var err error
			l, err = net.Listen("tcp", cfg.Listen)
			if err != nil {
				b.close()
				return boundListeners{}, fmt.Errorf("binding network door at %s: %w", cfg.Listen, err)
			}
		}
		b.network = l
		t, err := loadNetworkTLS(cfg.TLS)
		if err != nil {
			b.close()
			return boundListeners{}, err
		}
		b.netTLS = t
	}
	if cfg.OnBound != nil {
		cfg.OnBound(listenerAddr(b.dev), listenerAddr(b.network))
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
		// this is defense in depth against a caller that set a door without TLS.
		return nil, errors.New("network door requires TLS: both a certificate and a key are required with --listen or --listen-fd")
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
		AllowedOrigins: []string{origin},
		AllowedMethods: connectcors.AllowedMethods(),
		// otel.PostHogSessionHeader is the INBOUND mirror of the traceresponse
		// exposure below: a browser cannot SEND a request header absent from
		// Access-Control-Allow-Headers (the preflight fails), so without it the J1
		// session-id interceptor is unreachable from the UI.
		AllowedHeaders:   append(connectcors.AllowedHeaders(), "Authorization", otel.PostHogSessionHeader),
		ExposedHeaders:   append(connectcors.ExposedHeaders(), "traceresponse"),
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
// Residual risk (accepted, tracked as a RIG-1298 follow-up): the exemption is
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
// window that ReadHeaderTimeout leaves open (RIG-1298): it sets a per-request
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
			// SetReadDeadline bounds the request-body read. A failure means no
			// deadline support (never true for the network door), so fall through
			// and serve. But a future middleware wrapping w without Unwrap() would
			// silently disable slow-body protection, so log the load-bearing failure.
			rc := http.NewResponseController(w)
			if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
				slog.Warn("network door body-read deadline not armed; slow-body protection disabled for this request",
					"err", err, "path", r.URL.Path)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// buildNetworkServer ensures the bootstrap admin token (reused or minted, 0600 under the
// state dir, so a socket-only start leaves none behind), then constructs the
// authenticated network door: the compass.v1 CompassService and CommsService
// handlers behind the bearer interceptors (outer, they authenticate and inject
// the caller) and the admin gate (inner, it rejects a non-admin on the
// privileged session RPCs), plus the internal RunnerService door a Runner
// enrolls over — behind its own Runner-subject bearer interceptor, sharing the
// same auth.ResolveToken resolver but Kind-gated to a Runner token (an account
// token is Unauthenticated there, and a Runner token is Unauthenticated on the
// account/comms doors: the OQ7 cross-door rule). The credential and registry
// gateway services mount independently behind their service-bearer allowlist.
// Optionally wrapped in the single-origin network CORS policy. It does not bind
// or serve — the listener is already bound (boundListeners) — so on a token error
// the caller owns listener cleanup.
func buildNetworkServer(
	ctx context.Context,
	cfg ServeConfig,
	svc *service,
	commsSvc compassv1connect.CommsServiceHandler,
	secretsSvc compassv1connect.SecretsServiceHandler,
	usageSvc compassv1connect.UsageServiceHandler,
	hub *runnerhub.Hub,
	st *store.Store,
	adminID store.AccountID,
	netTLS *tls.Config,
	resolver agentSecretResolver,
	otelIC *otelconnect.Interceptor,
	webhookSink ForgeEventSink,
	webhookSecret func(ctx context.Context) ([]byte, error),
	linearWebhookHandler http.Handler,
	linearSessionLinkHandler http.Handler,
	runnerVerifier *auth.RunnerVerifier,
	gateway gatewayServices,
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
	tokenPath, minted, err := issueAndWriteAdminToken(ctx, st, adminID, stateDir)
	if err != nil {
		return nil, err
	}
	// Log the path, never the token: a logged bearer credential lets anyone who can
	// read process output or aggregated logs impersonate the admin. minted tells
	// the operator whether clients need the new file contents.
	slog.Info("network door bootstrap admin token ready",
		"path", tokenPath, "minted", minted, "handle", handle, "listen", networkListenAddr(cfg))
	warnIfSharedTokenDir(slog.Default(), filepath.Dir(tokenPath))

	// otelconnect (outermost) produces the RPC span and stamps traceresponse,
	// prepended to the shared bearer + admin-gate chain (inert when no provider).
	// NewSessionIDInterceptor reads the UI's X-POSTHOG-SESSION-ID and stamps
	// session.id; it sits after otelIC (which creates the span) and before auth.
	interceptors := connect.WithInterceptors(
		otelIC,
		otel.NewTraceResponseInterceptor(),
		otel.NewSessionIDInterceptor(),
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(adminID),
	)
	// WithReadMaxBytes caps a single inbound message (M1, defense in depth):
	// connect-go has NO default cap, so an operator could otherwise stream a huge
	// Put buffered whole in memory. Sized to PutAgentConfig's 64 MiB bundle plus
	// wire/framing headroom; 128 MiB. Mirrors the internal doors' posture.
	netPath, netHandler := compassv1connect.NewCompassServiceHandler(svc, interceptors, connect.WithReadMaxBytes(compassServiceMaxReadBytes))
	netCommsPath, netCommsHandler := compassv1connect.NewCommsServiceHandler(commsSvc, interceptors, connect.WithReadMaxBytes(siblingServiceMaxReadBytes))
	netMux := http.NewServeMux()
	netMux.Handle(netPath, netHandler)
	netMux.Handle(netCommsPath, netCommsHandler)
	// SecretsService rides the same bearer + admin-gate chain: the gate classifies
	// its 3 procedures authenticatedOpen, so any authenticated account clears it
	// and the handler enforces the split — user-only writes, user-or-agent list.
	netSecretsPath, netSecretsHandler := compassv1connect.NewSecretsServiceHandler(secretsSvc, interceptors, connect.WithReadMaxBytes(siblingServiceMaxReadBytes))
	netMux.Handle(netSecretsPath, netSecretsHandler)
	// UsageService uses the same authenticated chain and applies agent scope in its handler.
	netUsagePath, netUsageHandler := compassv1connect.NewUsageServiceHandler(usageSvc, interceptors, connect.WithReadMaxBytes(siblingServiceMaxReadBytes))
	netMux.Handle(netUsagePath, netUsageHandler)

	// The internal RunnerService door: the surface a Runner dials out to, mounted
	// only here on the authenticated network door (a Runner is remote, over TLS).
	// Its bearer interceptor Kind-gates to a Runner-subject token (cross-door
	// rejection, OQ7); the admin gate is not applied, the Kind gate is the authz.
	// Converted only when set: a nil pointer in the interface would not compare nil.
	var verifier runnerTokenVerifier
	if runnerVerifier != nil {
		verifier = runnerVerifier
	}
	runnerResolve := newRunnerResolve(func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		return auth.ResolveToken(ctx, st, presented, want)
	}, verifier)
	// Wire the store of record as the runner door's fleet config-bundle surface:
	// *store.Store satisfies AgentConfigStore, so FetchAgentConfig streams whatever
	// PutAgentConfig last wrote (nil would leave every agent unconfigured). otelIC
	// is the outermost interceptor (it creates the RelayCommsCall origin span).
	runnerPath, runnerHandler := runnerhub.NewMountedHandler(hub, runnerResolve, resolver, st, otelIC)
	netMux.Handle(runnerPath, runnerHandler)
	mountGatewayServices(netMux, gateway, otelIC, runnerResolve)

	// The internet-facing GitHub App webhook ingress (RIG-2883 T5), mounted only
	// when the board lane is on. It sits on the TLS door and OUTSIDE the bearer +
	// admin-gate: GitHub signs each delivery with the webhook secret, so the
	// handler's own VerifyGitHubSignature is its whole authentication.
	if webhookSink != nil {
		webhookPath, webhookHandler := NewGitHubWebhookHandler(webhookSecret, webhookSink, slog.Default())
		netMux.Handle(webhookPath, webhookHandler)
	}

	// The internet-facing Linear webhook ingress (RIG-2732 T7d), mounted only when
	// the Linear webhook secret is declared (App-INDEPENDENT). Like the GitHub
	// ingress it sits OUTSIDE the bearer + admin-gate: Linear signs each delivery
	// with the webhook secret, so VerifySignature is its whole authentication.
	if linearWebhookHandler != nil {
		netMux.Handle(linearWebhookPath, linearWebhookHandler)
	}
	// Unauthenticated by design: it mutates nothing, and the redirect target is
	// itself an auth-gated Compass surface.
	if linearSessionLinkHandler != nil {
		netMux.Handle(linearSessionLinkPattern, linearSessionLinkHandler)
	}
	var netRoot http.Handler = netMux
	if cfg.CORSAllowedOrigin != "" {
		// Network door defaults closed: CORS only for the one explicit
		// operator-configured browser origin.
		netRoot = networkCORS(cfg.CORSAllowedOrigin).Handler(netMux)
	}
	// Outermost: bound the request-body read so a slow-body drip cannot tie up a
	// connection (RIG-1298). Long-lived Runner request streams are exempt.
	netRoot = withBodyReadDeadline(netRoot, networkBodyReadTimeout)
	return &http.Server{
		Handler:   netRoot,
		TLSConfig: netTLS,
		Protocols: networkProtocols(),
		// G112: the network door is the internet-facing surface, so bound the header
		// read and idle lifetime to close the slow-loris window. The request-body
		// half is closed by withBodyReadDeadline (per-request, skipping the exempt
		// long-lived Runner streams) rather than a blunt whole-request ReadTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}

// gatewayServices are the LLM gateway's doors; a nil service is not mounted.
type gatewayServices struct {
	credentials *gatewayCredentialsService
	registry    *gatewayRegistryService
}

func mountGatewayServices(netMux *http.ServeMux, services gatewayServices, otelIC *otelconnect.Interceptor, runnerResolve runnerhub.TokenResolver) {
	if services.credentials == nil && services.registry == nil {
		return
	}
	interceptors := connect.WithInterceptors(otelIC, auth.ServiceBearerInterceptor(runnerResolve, auth.LLMGatewayServiceID))
	options := []connect.HandlerOption{interceptors, connect.WithReadMaxBytes(siblingServiceMaxReadBytes)}
	if services.credentials != nil {
		path, handler := compassv1internalconnect.NewGatewayCredentialsHandler(services.credentials, options...)
		netMux.Handle(path, handler)
	}
	if services.registry != nil {
		path, handler := compassv1internalconnect.NewGatewayRegistryHandler(services.registry, options...)
		netMux.Handle(path, handler)
	}
}

// runnerTokenVerifier authenticates a projected ServiceAccount token to a Runner
// subject. *auth.RunnerVerifier satisfies it; nil means no clusters registered.
type runnerTokenVerifier interface {
	Verify(ctx context.Context, token string) (store.Subject, error)
}

// newRunnerResolve splits the Runner door by token shape. A compact JWS never
// reaches the hash lookup: it is a projected token or nothing.
func newRunnerResolve(lookup runnerhub.TokenResolver, verifier runnerTokenVerifier) runnerhub.TokenResolver {
	return func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		if !auth.LooksLikeJWT(presented) {
			return lookup(ctx, presented, want)
		}
		if want != store.SubjectRunner {
			return store.Subject{}, auth.ErrWrongKind
		}
		if verifier == nil {
			return store.Subject{}, auth.ErrTokenNotFound
		}
		return verifier.Verify(ctx, presented)
	}
}

// issueAndWriteAdminToken ensures stateDir holds a 0600 bearer token for the
// bootstrap admin and reports whether it minted one. A private regular file
// whose token still resolves to adminID is reused, so restarts neither pile up
// live tokens nor break clients; anything else (missing, revoked, unknown,
// another subject, a symlink, group/other-readable) is replaced atomically. A
// store error during the check reads as not-found and also re-mints. It runs
// only with --listen or ListenListener; socket-only startup mints none. The token is never logged.
func issueAndWriteAdminToken(ctx context.Context, st *store.Store, adminID store.AccountID, stateDir string) (string, bool, error) {
	final := filepath.Join(stateDir, adminTokenFile)
	reuse, err := reusableAdminToken(ctx, st, adminID, final)
	if err != nil {
		return "", false, err
	}
	if reuse {
		return final, false, nil
	}
	token, err := auth.IssueAccountToken(ctx, st, adminID)
	if err != nil {
		return "", false, err
	}
	path, err := writeTokenFile(stateDir, token)
	return path, err == nil, err
}

// reusableAdminToken reports whether path is a private regular file holding a
// live token for adminID. Surrounding whitespace (an editor's trailing newline)
// is ignored, as every other token reader does; tokens never contain any.
func reusableAdminToken(ctx context.Context, st *store.Store, adminID store.AccountID, path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking existing admin token %q: %w", path, err)
	}
	// A symlink (Lstat mode 0777), any other non-regular file, or a group/other
	// bit disqualifies reuse; the re-mint's rename then replaces it with 0600.
	if info.Mode()&(os.ModeType|0o077) != 0 {
		return false, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator-configured state dir plus a fixed name, Lstat'd above
	if err != nil {
		return false, fmt.Errorf("reading existing admin token %q: %w", path, err)
	}
	subj, err := auth.ResolveToken(ctx, st, strings.TrimSpace(string(raw)), store.SubjectAccount)
	return err == nil && subj.ID == string(adminID), nil
}

// warnIfSharedTokenDir logs when the admin token's dir grants any group or
// other permission bit: ensurePrivateDir keeps an existing dir's mode, so such
// a dir exposes the file's presence and may let those users delete or replace it.
func warnIfSharedTokenDir(log *slog.Logger, dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		log.Warn("network door admin-token dir could not be checked", "path", dir, "err", err)
		return
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		log.Warn("network door admin-token dir is accessible to other users; use a 0700 --state-dir",
			"path", dir, "mode", fmt.Sprintf("%#o", perm))
	}
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

// networkListenAddr names the door for logs: the inherited listener's address, else --listen.
func networkListenAddr(cfg ServeConfig) string {
	if cfg.ListenListener != nil {
		return cfg.ListenListener.Addr().String()
	}
	return cfg.Listen
}
