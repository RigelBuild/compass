//go:build unix

// The authenticated network door: TLS termination, bearer + admin-gate
// interceptors, the bootstrap-admin token, and the network-door CORS policy.
// Kept out of serve.go so the serve loop's ordering stays readable; serve.go
// wires these helpers into the errgroup alongside the socket and dev doors.
package server

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/bodydeadline"
	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// tracedIngress creates a server span named operation, linked to, never parented
// by, any inbound traceparent: these endpoints face the internet.
func tracedIngress(h http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(h, operation,
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
		otelhttp.WithSpanNameFormatter(func(op string, _ *http.Request) string { return op }))
}

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

// gatewayAuthMaxReadBytes caps VerifyAgentToken bodies, which are decoded before
// the bearer check runs; a minted token is 43 bytes.
const gatewayAuthMaxReadBytes = 4 << 10

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
func bindListeners(ctx context.Context, cfg ServeConfig) (boundListeners, error) {
	var b boundListeners
	if cfg.DevHTTP != nil {
		if !cfg.DevHTTP.Addr().IsLoopback() {
			if cfg.ListenListener != nil {
				_ = cfg.ListenListener.Close()
			}
			return boundListeners{}, fmt.Errorf("dev_http must be a loopback address (127.0.0.1 or ::1), got %s", cfg.DevHTTP)
		}
		l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.DevHTTP.String())
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
			l, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
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

// networkBodyReadTimeout bounds request-body arrival time on the network door.
// Authenticated Runner streams release the deadline after bearer authentication.
const networkBodyReadTimeout = 30 * time.Second

// bodyDeadlineReleasable names Runner request streams that clear their deadline
// after bearer authentication, so unauthenticated request bodies stay bounded.
var bodyDeadlineReleasable = map[string]struct{}{
	compassv1internalconnect.RunnerServiceSessionsProcedure:      {},
	compassv1internalconnect.RunnerServicePublishEventsProcedure: {},
}

// withBodyReadDeadline bounds request-body reads, then lets authenticated Runner
// streams clear their deadline before decoding begins.
func withBodyReadDeadline(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			slog.WarnContext(r.Context(), "network door body-read deadline not armed; slow-body protection disabled for this request",
				"err", err, "path", r.URL.Path)
		}
		if _, releasable := bodyDeadlineReleasable[r.URL.Path]; releasable {
			var once sync.Once
			ctx := bodydeadline.WithRelease(r.Context(), func(ctx context.Context) {
				once.Do(func() {
					if err := rc.SetReadDeadline(time.Time{}); err != nil {
						slog.WarnContext(ctx, "network door body-read deadline not released",
							"err", err, "path", r.URL.Path)
					}
				})
			})
			r = r.WithContext(ctx)
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
	agentRepositorySvc compassv1connect.AgentRepositoryServiceHandler,
	hub *runnerhub.Hub,
	st *store.Store,
	adminID store.AccountID,
	netTLS *tls.Config,
	resolver agentSecretResolver,
	otelIC connect.ServerInterceptor,
	webhookSink ForgeEventSink,
	webhookSecret func(ctx context.Context) ([]byte, error),
	linearWebhookHandler http.Handler,
	linearSessionLinkHandler http.Handler,
	runnerVerifier *auth.RunnerVerifier,
	gateway gatewayServices,
	health *serverHealth,
	streams *streamDrain,
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
	tokenPath, minted, err := issueAndWriteAdminToken(ctx, st, adminID, stateDir, cfg.AdminTokenFile)
	if err != nil {
		return nil, err
	}
	// Log the path, never the token: a logged bearer credential lets anyone who can
	// read process output or aggregated logs impersonate the admin. minted tells
	// the operator whether clients need the new file contents.
	slog.InfoContext(ctx, "network door bootstrap admin token ready",
		"path", tokenPath, "minted", minted, "handle", handle, "listen", networkListenAddr(cfg))
	warnIfSharedTokenDir(ctx, slog.Default(), filepath.Dir(tokenPath))

	// otelconnect (outermost) produces the RPC span and stamps traceresponse,
	// prepended to the shared bearer + admin-gate chain (inert when no provider).
	// NewSessionIDInterceptor reads the UI's X-POSTHOG-SESSION-ID and stamps
	// session.id; it sits after otelIC (which creates the span) and before auth.
	interceptors := otel.HandlerInterceptors(otelIC,
		otel.NewSessionIDInterceptor(),
		auth.BearerInterceptor(st),
		auth.NewAdminGate(adminID),
	)
	// WithReadMaxBytes caps a single inbound message (M1, defense in depth):
	// connect-go has NO default cap, so an operator could otherwise stream a huge
	// Put buffered whole in memory. Sized to PutAgentConfig's 64 MiB bundle plus
	// wire/framing headroom; 128 MiB. Mirrors the internal doors' posture.
	netMux := http.NewServeMux()
	compassServer := connect.NewServer(interceptors)
	compassv1connect.RegisterCompassServiceHandler(compassServer, svc)
	connecthttp.Mount(netMux, compassServer, connecthttp.WithReadMaxBytes(compassServiceMaxReadBytes))
	commsServer := connect.NewServer(interceptors)
	compassv1connect.RegisterCommsServiceHandler(commsServer, commsSvc)
	connecthttp.Mount(netMux, commsServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	// SecretsService rides the same bearer + admin-gate chain: the gate classifies
	// its 3 procedures authenticatedOpen, so any authenticated account clears it
	// and the handler enforces the split — user-only writes, user-or-agent list.
	secretsServer := connect.NewServer(interceptors)
	compassv1connect.RegisterSecretsServiceHandler(secretsServer, secretsSvc)
	connecthttp.Mount(netMux, secretsServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	// UsageService uses the same authenticated chain and applies agent scope in its handler.
	usageServer := connect.NewServer(interceptors)
	compassv1connect.RegisterUsageServiceHandler(usageServer, usageSvc)
	connecthttp.Mount(netMux, usageServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	agentRepositoryServer := connect.NewServer(interceptors)
	compassv1connect.RegisterAgentRepositoryServiceHandler(agentRepositoryServer, agentRepositorySvc)
	connecthttp.Mount(netMux, agentRepositoryServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	health.mount(netMux)

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
	runnerhub.Mount(netMux, hub, runnerResolve, resolver, st, otelIC)
	mountGatewayServices(netMux, gateway, otelIC, runnerResolve)

	// The internet-facing GitHub App webhook ingress (RIG-2883 T5), mounted only
	// when the board lane is on. It sits on the TLS door and OUTSIDE the bearer +
	// admin-gate: GitHub signs each delivery with the webhook secret, so the
	// handler's own VerifyGitHubSignature is its whole authentication.
	if webhookSink != nil {
		webhookPath, webhookHandler := NewGitHubWebhookHandler(webhookSecret, webhookSink, slog.Default())
		netMux.Handle(webhookPath, tracedIngress(webhookHandler, "github-webhook"))
	}

	// The internet-facing Linear webhook ingress (RIG-2732 T7d), mounted only when
	// the Linear webhook secret is declared (App-INDEPENDENT). Like the GitHub
	// ingress it sits OUTSIDE the bearer + admin-gate: Linear signs each delivery
	// with the webhook secret, so VerifySignature is its whole authentication.
	if linearWebhookHandler != nil {
		netMux.Handle(linearWebhookPath, tracedIngress(linearWebhookHandler, "linear-webhook"))
	}
	// Unauthenticated by design: it mutates nothing, and the redirect target is
	// itself an auth-gated Compass surface.
	if linearSessionLinkHandler != nil {
		netMux.Handle(linearSessionLinkPattern, tracedIngress(linearSessionLinkHandler, "linear-session-link"))
	}
	var netRoot http.Handler = netMux
	if cfg.CORSAllowedOrigin != "" {
		// Network door defaults closed: CORS only for the one explicit
		// operator-configured browser origin.
		netRoot = networkCORS(cfg.CORSAllowedOrigin).Handler(netMux)
	}
	// Bound request bodies before auth; Runner streams stay bounded until bearer auth.
	netRoot = withBodyReadDeadline(netRoot, networkBodyReadTimeout)
	// Shutdown does not unblock a stalled stream write; drain cuts it via this registry.
	netRoot = streams.wrap(netRoot)
	return &http.Server{
		Handler:   netRoot,
		TLSConfig: netTLS,
		Protocols: networkProtocols(),
		// G112: bound the header and idle lifetime on the internet-facing door.
		// The body deadline is released for Runner streams after bearer auth.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}

// gatewayServices are the LLM gateway's doors; a nil service is not mounted.
type gatewayServices struct {
	credentials *gatewayCredentialsService
	registry    *gatewayRegistryService
	auth        *gatewayAuthService
}

// newGatewayServices builds every service the LLM gateway calls on the network door.
func newGatewayServices(st *store.Store, masterKey envelope.Key, keyVersion int16) gatewayServices {
	creds := gatewaycred.NewPostgres(st, masterKey, keyVersion)
	tokens := auth.NewGatewayTokens(st)
	return gatewayServices{
		credentials: newGatewayCredentialsService(tokens, creds, creds, slog.Default()),
		registry:    newGatewayRegistryService(st, slog.Default()),
		auth:        newGatewayAuthService(tokens),
	}
}

func mountGatewayServices(
	netMux *http.ServeMux,
	services gatewayServices,
	otelIC connect.ServerInterceptor,
	runnerResolve runnerhub.TokenResolver,
) {
	if services.credentials == nil && services.registry == nil && services.auth == nil {
		return
	}
	interceptors := otel.HandlerInterceptors(otelIC, auth.ServiceBearerInterceptor(runnerResolve, auth.LLMGatewayServiceID))
	if services.credentials != nil {
		credentialsServer := connect.NewServer(interceptors)
		compassv1internalconnect.RegisterGatewayCredentialsHandler(credentialsServer, services.credentials)
		connecthttp.Mount(netMux, credentialsServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	}
	if services.registry != nil {
		registryServer := connect.NewServer(interceptors)
		compassv1internalconnect.RegisterGatewayRegistryHandler(registryServer, services.registry)
		connecthttp.Mount(netMux, registryServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	}
	if services.auth != nil {
		authServer := connect.NewServer(interceptors)
		compassv1internalconnect.RegisterGatewayAuthHandler(authServer, services.auth)
		connecthttp.Mount(netMux, authServer, connecthttp.WithReadMaxBytes(gatewayAuthMaxReadBytes))
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

// issueAndWriteAdminToken keeps a 0600 admin token under stateDir; a supplied token file stays stable across DB recreates.
func issueAndWriteAdminToken(ctx context.Context, st *store.Store, adminID store.AccountID, stateDir, suppliedTokenFile string) (string, bool, error) {
	final := filepath.Join(stateDir, adminTokenFile)
	if suppliedTokenFile != "" {
		token, err := readSuppliedAdminToken(suppliedTokenFile)
		if err != nil {
			return "", false, err
		}
		if err := auth.RegisterAccountToken(ctx, st, adminID, token); err != nil {
			return "", false, fmt.Errorf("registering supplied admin token from %q: %w", suppliedTokenFile, err)
		}
		if err := writeSuppliedAdminTokenFile(stateDir, token); err != nil {
			return "", false, err
		}
		return final, false, nil
	}
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

func readSuppliedAdminToken(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0) //nolint:gosec // G304: the operator-configured path is opened directly
	if err != nil {
		return "", fmt.Errorf("opening supplied admin token file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("checking supplied admin token file %q: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("checking supplied admin token file %q: cannot read file group", path)
	}
	effectiveGID := os.Getegid()
	var groups []int
	if info.Mode().Perm()&0o040 != 0 && int64(stat.Gid) != int64(effectiveGID) {
		groups, err = os.Getgroups()
		if err != nil {
			return "", fmt.Errorf("checking supplied admin token file %q group membership: %w", path, err)
		}
	}
	if !suppliedAdminTokenModeAllowed(info.Mode(), stat.Gid, effectiveGID, groups) {
		return "", fmt.Errorf("supplied admin token file %q must be a private regular file", path)
	}

	raw, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("reading supplied admin token file %q: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || token != base64.RawURLEncoding.EncodeToString(decoded) {
		return "", fmt.Errorf("supplied admin token file %q must contain a base64url token encoding exactly 32 bytes", path)
	}
	return token, nil
}

func suppliedAdminTokenModeAllowed(mode os.FileMode, gid uint32, effectiveGID int, groups []int) bool {
	if !mode.IsRegular() || mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	permissions := mode.Perm()
	if permissions&0o037 != 0 {
		return false
	}
	if permissions&0o040 == 0 || int64(gid) == int64(effectiveGID) {
		return true
	}
	for _, group := range groups {
		if int64(group) == int64(gid) {
			return true
		}
	}
	return false
}

func writeSuppliedAdminTokenFile(dir, token string) error {
	final := filepath.Join(dir, adminTokenFile)
	info, err := os.Lstat(final)
	if err == nil && info.Mode()&(os.ModeType|0o077) == 0 {
		raw, err := os.ReadFile(final) //nolint:gosec // G304: the fixed path was Lstat'd above
		if err != nil {
			return fmt.Errorf("reading admin token file %q: %w", final, err)
		}
		if string(raw) == token {
			return nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking admin token file %q: %w", final, err)
	}
	if _, err := writeTokenFile(dir, token); err != nil {
		return err
	}
	return nil
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
func warnIfSharedTokenDir(ctx context.Context, log *slog.Logger, dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		log.WarnContext(ctx, "network door admin-token dir could not be checked", "path", dir, "err", err)
		return
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		log.WarnContext(ctx, "network door admin-token dir is accessible to other users; use a 0700 --state-dir",
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
