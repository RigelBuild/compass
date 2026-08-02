//go:build unix

// Package server is the serve loop: bind the compass.v1 service to a
// Unix domain socket and drive it until shutdown. The socket serves native gRPC
// (HTTP/2), gRPC-Web, and Connect off one connect-go handler, so the shell's
// webview and native clients share one door — no localhost TCP on the shipped
// path. An optional dev-only loopback endpoint (DevHTTP) serves the same handler
// with permissive CORS for a browser dev server; it is off unless requested.
//
// connect-go serves gRPC, gRPC-Web, and Connect from a single handler, so one
// door needs no separate gRPC-Web translation layer; cleartext HTTP/2 on the
// Unix socket carries native gRPC.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	connectcors "connectrpc.com/cors"
	"github.com/rs/cors"
	"golang.org/x/sync/errgroup"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/comms"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/secrets"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TLSConfig carries operator-provisioned PEM paths for the authenticated TCP
// door. Consumed by the network door (T3); nil on the socket-only shipped path.
type TLSConfig struct {
	CertPath string
	KeyPath  string
}

// ServeConfig configures the serve loop.
type ServeConfig struct {
	// SocketPath is the Unix domain socket the server binds and serves on.
	SocketPath string
	// Version is the server build + contract version reported by GetServerInfo.
	Version string
	// DevHTTP, when set, is a loopback address on which the server also serves
	// the handler with permissive CORS for a browser dev server. Enforced to be
	// loopback here, not just by the CLI.
	DevHTTP *netip.AddrPort
	// TLS is the network-door termination config (T3); nil on the socket path.
	TLS *TLSConfig
	// DatabaseDSN is the Postgres connection string for the store of record
	// (T1). Required: the comms vertical is store-backed, so Serve opens the
	// store at startup and refuses to serve without it.
	DatabaseDSN string
	// Listen, when set, is the TCP address the authenticated network door binds
	// (e.g. "0.0.0.0:8443"). Empty on the socket-only shipped path. When set, TLS
	// is required — a bearer token over cleartext is credential disclosure.
	Listen string
	// AdminHandle is the bootstrap-admin account handle minted on network-door
	// startup (default "admin"). Its token is written 0600 under the state dir.
	AdminHandle string
	// StateDir is the directory the bootstrap-admin token file is written under
	// (0600). Defaults to the socket's parent directory when empty.
	StateDir string
	// CORSAllowedOrigin, when set, is the single browser origin the network door
	// exposes gRPC-Web CORS for. Empty = closed (no CORS on the network door).
	CORSAllowedOrigin string
}

// The bootstrap-admin identity the local-socket door attributes callers to until
// the T3 interceptor sets a real caller (design.md:1219-1222). A fixed handle so
// BootstrapAdmin is idempotent across restarts (find-or-create by handle).
const (
	bootstrapAdminHandle      = "admin"
	bootstrapAdminDisplayName = "Administrator"
)

// secretsStateDir returns the Server-owned directory the secret resolver writes
// its generated SecretSpec manifests under: a "secrets" subdirectory of the same
// state dir the bootstrap-admin token is written under (cfg.StateDir, defaulting
// to the socket's parent, then "."), matching the network door's stateDir
// derivation (network_door.go). Server state, never repo state; NewSpecResolver
// creates it 0700 if absent.
func secretsStateDir(cfg ServeConfig) string {
	base := cfg.StateDir
	if base == "" {
		if base = parentDir(cfg.SocketPath); base == "" {
			base = "."
		}
	}
	return filepath.Join(base, "secrets")
}

// Serve binds the compass.v1 service to cfg.SocketPath and drives it until ctx
// is cancelled.
//
// It ensures a private (0700) parent directory for any path segment it creates,
// refuses to start if a live server already answers at the socket (a stale
// socket is cleared, a non-socket path is rejected), binds the listener and
// restricts it to the owner (mode 0600), then publishes the initial
// ServerStatus{Ready} so a snapshot subscriber sees liveness immediately.
//
// When cfg.DevHTTP is set it must be a loopback address (enforced here); the
// server then also serves gRPC-Web with CORS on that TCP port. Its listener is
// bound before serving, so a bind failure fails Serve up front rather than
// dying unseen in a background goroutine. Both servers are driven off ctx, so
// cancelling it drains them; if either exits on its own the other is torn down
// and the error propagates.
//
// On shutdown it closes the bus (waking every open SubscribeEvents stream so
// graceful drain completes), then removes the socket file iff it is still the
// one it bound (inode-checked, so a racing successor server's socket is intact).
func Serve(ctx context.Context, cfg ServeConfig) error {
	// Eager-bind the optional dev and network-door TCP listeners before any
	// on-disk state, so a bad address, an in-use port, or a bad TLS keypair
	// fails Serve here — leaving no socket, directory, or admin-token file
	// behind — rather than dying unobserved in a detached serving goroutine.
	listeners, err := bindListeners(cfg)
	if err != nil {
		return err
	}
	devListener, netListener, netTLS := listeners.dev, listeners.network, listeners.netTLS

	// Create the parent chain first, tightening any directory we create to 0700
	// so the socket never sits briefly reachable under a world-traversable dir.
	if parent := parentDir(cfg.SocketPath); parent != "" {
		if err := ensurePrivateDir(parent); err != nil {
			listeners.close()
			return err
		}
	}

	// Single-instance: never unlink a socket a live server is still serving.
	if err := clearStaleSocket(cfg.SocketPath); err != nil {
		listeners.close()
		return err
	}

	// Bind the socket under a restrictive umask so it is born owner-only (0600)
	// with no window in which a local peer could connect before the mode is
	// tightened. net.Listen creates the socket file with 0666 & ^umask, so
	// without this a permissive umask under a pre-existing traversable parent
	// would expose a connectable socket until the chmod below; a connection
	// opened in that window survives the later chmod. Startup is single-
	// goroutine here (servers spawn afterward), so the process-global umask is
	// safe to set and restore around the bind. The explicit chmod stays as
	// belt-and-suspenders: it pins exactly 0600 regardless of the prior umask.
	udsListener, err := listenUnixPrivate(cfg.SocketPath)
	if err != nil {
		listeners.close()
		return fmt.Errorf("binding socket at %s: %w", cfg.SocketPath, err)
	}

	// Owner-only: the socket is the server's whole trust boundary on the local
	// machine, so no other user may connect.
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing chmod path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return fmt.Errorf("chmod 0600 %s: %w", cfg.SocketPath, err)
	}

	// Pin the inode we bound so shutdown cleanup can tell our socket apart from
	// a successor server that rebound the same path.
	boundInode, boundOK := socketInode(cfg.SocketPath)
	defer cleanupSocket(cfg.SocketPath, boundInode, boundOK)

	// The one event bus every sequenced stream rides. Publish the initial Ready
	// status so a snapshot subscriber sees liveness immediately.
	bus := events.NewBus[busPayload]()
	defer bus.Close()
	publishReady(bus)

	// The store of record (T1) backs the comms vertical and the token store. Open
	// it before serving so a bad DSN or a failed migration fails startup here, not
	// mid-request.
	st, err := store.Open(ctx, cfg.DatabaseDSN)
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	// The bootstrap admin is the account the local-socket door attributes every
	// RPC to until the network-door interceptor sets a real caller identity (the
	// 0600 socket is the local credential). Idempotent: created on first boot,
	// fetched on every later one. Created unconditionally — even socket-only —
	// so the AdminGate always has a real admin id to compare against.
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: bootstrapAdminHandle, DisplayName: bootstrapAdminDisplayName})
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return fmt.Errorf("bootstrapping admin account: %w", err)
	}

	// The store of record backs the network door's bearer credentials: IssueToken
	// persists a token hash into it, and the bearer interceptor resolves against it.
	//
	// The RunnerHub is the Server side of the Server<->Runner seam: it routes the
	// container-lifecycle RPCs to the owning Runner and write-throughs relayed
	// agent events. The Bridge board is its lifecycle sink: a session lifecycle
	// transition is recorded into the board projection and fanned onto
	// SubscribeEvents, and GetAgentStatus reads the board's snapshot — so one
	// board instance is shared by the hub (writer) and the service (reader). Built
	// unconditionally so a lifecycle RPC has a hub to route through; the
	// RunnerService door a Runner enrolls over is mounted only on the network door
	// (buildNetworkServer) — Runners are remote, so they dial the authenticated
	// TLS door, never the loopback socket.
	brd := board.NewProjection(bus)

	// The comms event stream rides a second bus instance — its own seq space and
	// per-boot instance_epoch, distinct from the CompassService bus above. Built
	// before the RunnerHub because the hub's RelayCommsCall leg executes
	// agent-initiated comms calls through this handler (the CommsCaller).
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	defer commsBus.Close()
	commsSvc := comms.NewComms(st, commsBus, admin.ID)
	commsPath, commsHandler := compassv1connect.NewCommsServiceHandler(commsSvc)

	// The Server-side secret resolver: reads the store's names registry, generates
	// the SecretSpec manifest under a Server-owned state dir, and resolves values
	// from the configured provider. It is the single place SecretSpec runs — the
	// RunnerService FetchSecrets handler and the SecretsService write path both
	// delegate to this one instance. The state dir is a "secrets" subdirectory of
	// the same state dir the bootstrap-admin token is written under (secretsStateDir);
	// NewSpecResolver creates it 0700 if absent.
	resolver := secrets.NewSpecResolver(st, secretsStateDir(cfg))

	// One sessionTail instance is the hub's session-tail sink (writer) and the
	// service's SubscribeAgentSession source (reader) — a frame the hub relays
	// fans to that session's stream subscribers.
	tail := newSessionTail()
	// One logger for the hub's per-frame diagnostics and for the frame-loss
	// summary the drain logs, so both land on the same sink.
	hubLog := slog.Default()
	hub := newRunnerHub(brd, tail, commsSvc, hubLog)
	svc := newService(cfg.Version, bus, st, hub, brd, tail)
	// SEA-1618 T5: lifecycleService serves RelayLifecycleCall; setter breaks the hub<->service cycle (sinks.go).
	hub.SetLifecycleCaller(newLifecycleService(st, hub))
	// The SecretsService is an account-facing sibling of CompassService/CommsService:
	// it mounts on every account door (socket, dev, network) behind the same bearer +
	// admin-gate chain, which classifies its three procedures authenticatedOpen — the
	// door admits any authenticated account and the handler enforces the user-only
	// writes / user-or-agent list. The hub is its SecretsVersion signaler (a Set/Delete
	// notifies live sessions to re-fetch); it shares the one resolver with FetchSecrets.
	secretsSvc := newSecretsService(st, resolver, hub)

	// Shipped door: the Unix socket serves native gRPC (cleartext HTTP/2),
	// gRPC-Web, and Connect off the one connect-go handler. No CORS — the socket
	// is same-origin (the shell's webview / a native client). The 0600 socket is
	// the credential: the CompassService handler mounts the ambient-identity
	// interceptor pair (bootstrap admin), and comms attributes every RPC to the
	// bootstrap admin via its own actor fallback. Cleartext HTTP/2 is served
	// natively (http.Protocols); these connections are tracked by
	// http.Server.Shutdown and drain with the rest on shutdown.
	socketPath, socketHandler := compassv1connect.NewCompassServiceHandler(svc,
		connect.WithInterceptors(auth.AmbientIdentity(admin.ID), auth.AmbientStreamInterceptor(admin.ID)))
	// SecretsService rides the same ambient-identity pair so its handler reads a
	// caller via CallerFrom (the bootstrap admin, a user): the socket's 0600 mode
	// is the credential, and admin being a user satisfies the user-only writes.
	secretsSocketPath, secretsSocketHandler := compassv1connect.NewSecretsServiceHandler(secretsSvc,
		connect.WithInterceptors(auth.AmbientIdentity(admin.ID), auth.AmbientStreamInterceptor(admin.ID)))
	udsMux := http.NewServeMux()
	udsMux.Handle(socketPath, socketHandler)
	udsMux.Handle(commsPath, commsHandler)
	udsMux.Handle(secretsSocketPath, secretsSocketHandler)
	udsServer := &http.Server{Handler: udsMux, Protocols: cleartextHTTP2()} //nolint:gosec // G112: socket-only door (never internet-facing), so the Slowloris ReadHeaderTimeout does not apply; the network door below sets it

	// Dev-only browser door: the same services with permissive CORS on the
	// pre-bound loopback listener. Off unless DevHTTP is passed; the shipped path
	// stays socket-only (no TCP port).
	//
	// Interceptor ORDER is load-bearing and security-critical. connect runs the
	// first interceptor in the slice outermost, so AdminGate runs BEFORE the
	// ambient pair attaches a caller:
	//   - adminOnly RPCs (IssueToken, the agent-session lifecycle RPCs): AdminGate
	//     runs first, finds no caller yet, and fail-closes to PermissionDenied —
	//     the ambient interceptor never runs. Without this a page loaded against a
	//     configured --dev-http could mint a bootstrap-admin token via IssueToken
	//     and replay it against the TLS network door.
	//   - authenticatedOpen RPCs (GetServerInfo, SubscribeEvents, and the
	//     SubscribeAgentSession observation stream): AdminGate passes them, then
	//     the ambient pair attaches the bootstrap admin so a handler that needs a
	//     caller (SubscribeAgentSession authorizes home-channel membership) sees
	//     one — the dev door thus mirrors the shipped Unix-socket door's
	//     ambient-admin behavior for the session pane.
	// Reversing the order (ambient before AdminGate) would attach caller=admin
	// before the gate, ADMITTING IssueToken on the dev browser door — never do
	// that. CommsService keeps its own per-account authz under the ambient admin.
	var devServer *http.Server
	if devListener != nil {
		devPath, devHandler := compassv1connect.NewCompassServiceHandler(svc,
			connect.WithInterceptors(auth.NewAdminGate(admin.ID), auth.AmbientIdentity(admin.ID), auth.AmbientStreamInterceptor(admin.ID)))
		// SecretsService on the dev door: same admin-gate + ambient chain as
		// CompassService. The gate classifies its 3 procedures authenticatedOpen,
		// so it passes them, then the ambient pair attaches the bootstrap admin
		// (a user) the handler reads for the user-only write authz.
		devSecretsPath, devSecretsHandler := compassv1connect.NewSecretsServiceHandler(secretsSvc,
			connect.WithInterceptors(auth.NewAdminGate(admin.ID), auth.AmbientIdentity(admin.ID), auth.AmbientStreamInterceptor(admin.ID)))
		devMux := http.NewServeMux()
		devMux.Handle(devPath, devHandler)
		devMux.Handle(commsPath, commsHandler)
		devMux.Handle(devSecretsPath, devSecretsHandler)
		devServer = &http.Server{Handler: devCORS().Handler(devMux), Protocols: cleartextHTTP2()} //nolint:gosec // G112: loopback dev-only door (off on the shipped path), so the Slowloris ReadHeaderTimeout does not apply here either
	}

	// Authenticated network door, built only when --listen is given. It mints and
	// writes the bootstrap token 0600 under the state dir (so a socket-only start
	// leaves none behind) and mounts the CompassService + CommsService behind the
	// bearer + admin-gate interceptors, plus the internal RunnerService door a
	// Runner enrolls over (Runner-subject bearer, separate from the account door).
	// On a build error the listeners this Serve bound are still ours to close.
	var netServer *http.Server
	if netListener != nil {
		s, err := buildNetworkServer(ctx, cfg, svc, commsSvc, secretsSvc, hub, st, admin.ID, netTLS, resolver)
		if err != nil {
			udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
			listeners.close()
			return err
		}
		netServer = s
	}

	// Run every door under one scoped group. errgroup.WithContext gives the
	// listener/drain coordination scoped lifecycle, first-error-wins, and sibling
	// cancellation in one audited primitive: gctx is cancelled when the parent ctx
	// is cancelled (normal shutdown) or when a server self-terminates with an
	// error (the first exit tears down the peers). The UDS door is primary; its
	// error — recorded first by the group — wins over the drain result below.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return classifyServe(udsServer.Serve(udsListener), "compass.v1 UDS server") })
	if devServer != nil {
		g.Go(func() error { return classifyServe(devServer.Serve(devListener), "dev gRPC-Web server") })
	}
	if netServer != nil {
		// ServeTLS wraps the pre-bound listener with the configured keypair
		// (already in TLSConfig, so the empty cert/key args skip a file load) and
		// advertises ALPN h2 from Protocols. A clean shutdown returns
		// ErrServerClosed, mapped to nil by classifyServe.
		g.Go(func() error {
			return classifyServe(netServer.ServeTLS(netListener, "", ""), "compass.v1 network door")
		})
	}
	// The comms-bus consumers (SEA-1569): the T3 delivery fan-out consumer and
	// the T8 presence projection, both tailing the comms bus with their bus-tail
	// goroutines on the serve group rooted on gctx (cancels at shutdown; each also
	// ends when the comms bus closes in drainDoors).
	startCommsBusConsumers(gctx, g, commsBus, st, hub, hubLog)
	// Drain member of the same group: wake on gctx cancellation (parent shutdown
	// or a server erroring), then hand off to drainDoors. A drain that overruns
	// (a handler still wedged — e.g. a stream stuck mid replay to a stalled
	// client) surfaces as the error rather than being swallowed into a false
	// clean shutdown; because the group keeps the first error, a real serve error
	// still wins over this drain.
	g.Go(func() error {
		<-gctx.Done()
		return drainDoors(drainSet{ //nolint:contextcheck // drainDoors deliberately takes no ctx: the inherited one is already cancelled (that cancellation is what woke this drain), so a bounded shutdown needs the fresh ctx drainDoors makes internally, not the dead one
			bus:      bus,
			commsBus: commsBus,
			uds:      udsServer,
			dev:      devServer,
			net:      netServer,
			hub:      hub,
			log:      hubLog,
		})
	})

	return g.Wait()
}

// drainSet is the shutdown-side view of what Serve built: the two buses whose
// held-open streams must end before a door can drain, the doors themselves
// (dev and net are nil when not configured), and the hub whose frame-loss
// counters are final once every door is drained.
type drainSet struct {
	bus      *events.Bus[busPayload]
	commsBus *events.Bus[*compassv1.SubscribeCommsResponse]
	uds      *http.Server
	dev      *http.Server
	net      *http.Server
	hub      *runnerhub.Hub
	log      *slog.Logger
}

// drainDoors ends the live streams, drains every configured door under one
// bounded deadline, and reports the hub's final frame accounting. It is split
// out of Serve so the shutdown sequence reads as one unit: the ordering here is
// load-bearing and was previously buried at the bottom of a 200-line function.
func drainDoors(d drainSet) error {
	// Close both buses so held-open streams end and release their handlers:
	// the CompassService SubscribeEvents rides bus, the CommsService
	// SubscribeComms rides commsBus, and both serve on every door. Closing only
	// bus would leave a live SubscribeComms subscriber wedged (its ctx is not
	// cancelled by Shutdown), stalling the drain to the deadline. Both closes
	// are idempotent, so the deferred Close of each stays a safe no-op.
	d.bus.Close()
	d.commsBus.Close()
	// Fresh ctx is deliberate: the parent is already cancelled (that woke this
	// drain), so the bounded drain needs a live ctx, not the dead inherited one.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drainErr := d.uds.Shutdown(shutdownCtx)
	for _, srv := range []*http.Server{d.dev, d.net} {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(shutdownCtx); err != nil && drainErr == nil {
			drainErr = err
		}
	}
	// Every door is drained, so no further frame can arrive: the hub's counters
	// are final. Report them — this is the only non-test reader of the frame-loss
	// accounting, and without it a run that committed none of the agent's
	// conversation would end indistinguishably from one that committed all of it.
	logFrameDiagnostics(shutdownCtx, d.log, d.hub)
	if drainErr != nil {
		return fmt.Errorf("draining compass.v1 servers on shutdown: %w", drainErr)
	}
	return nil
}

// publishReady stamps the initial ServerStatus{Ready} onto the bus.
func publishReady(bus *events.Bus[busPayload]) {
	bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_ServerStatus{
			ServerStatus: &compassv1.ServerStatus{State: compassv1.ServerState_SERVER_STATE_READY},
		},
	})
}

// devCORS is the permissive CORS policy for the dev-only loopback endpoint: any
// origin (it is a loopback dev port, not a production surface) with the
// Connect/gRPC-Web headers and status trailers exposed so the browser client can
// read grpc-status.
func devCORS() *cors.Cors {
	return cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   connectcors.AllowedMethods(),
		AllowedHeaders:   connectcors.AllowedHeaders(),
		ExposedHeaders:   connectcors.ExposedHeaders(),
		AllowCredentials: false,
	})
}

// cleartextHTTP2 enables HTTP/1.1 and prior-knowledge cleartext HTTP/2 (h2c) on
// a server: native gRPC clients speak HTTP/2, while gRPC-Web and Connect clients
// reach the same handler over either version. Because the stdlib server serves
// both, their connections are tracked by http.Server.Shutdown and drain
// gracefully.
func cleartextHTTP2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// classifyServe maps an http.Server.Serve result to a server error. A clean
// shutdown (ErrServerClosed) is nil; anything else is wrapped with ctx.
func classifyServe(err error, ctx string) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("%s terminated with an error: %w", ctx, err)
}

func closeListener(l net.Listener) {
	if l != nil {
		l.Close() //nolint:errcheck,gosec // best-effort listener close on teardown — nothing actionable remains (errcheck + its gosec G104 twin)
	}
}
