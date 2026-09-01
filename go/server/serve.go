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
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	connectcors "connectrpc.com/cors"
	"connectrpc.com/otelconnect"
	"github.com/rs/cors"
	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TLSConfig carries operator-provisioned PEM paths for the authenticated TCP
// door. Consumed by the network door (T3); nil on the socket-only shipped path.
type TLSConfig struct {
	CertPath string
	KeyPath  string
}

// S3Config is the object-store archive tier configuration, re-exported from the
// store package so the CLI builds it without importing internal/store directly.
type S3Config = store.S3Config

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
	// S3 is the object-store archive tier config (RIG-1667 T4). Optional: when
	// unset (no endpoint/bucket) the server boots without an archive tier and the
	// store's nil object-store guard fails a flush loudly only if one is ever
	// attempted. Mirrors the DATABASE_DSN flag/env precedence at the CLI.
	S3 S3Config
	// Listen, when set, is the TCP address the authenticated network door binds
	// (e.g. "0.0.0.0:8443"). Empty on the socket-only shipped path. When set, TLS
	// is required — a bearer token over cleartext is credential disclosure.
	Listen string
	// AdminHandle is the handle of the bootstrap-admin account created (or found)
	// at startup — the identity the local-socket door attributes callers to and
	// the network door's AdminGate compares against. Empty defaults to "admin".
	// Bootstrap is find-or-create by handle, so a handle that already names a
	// non-admin account fails startup rather than elevating it. When the network
	// door is enabled, this account's token is written 0600 under the state dir.
	AdminHandle string
	// StateDir is the directory the bootstrap-admin token file is written under
	// (0600). Defaults to the socket's parent directory when empty.
	StateDir string
	// CORSAllowedOrigin, when set, is the single browser origin the network door
	// exposes gRPC-Web CORS for. Empty = closed (no CORS on the network door).
	CORSAllowedOrigin string
	// PublicURL is the per-deployment public base URL Compass is reachable at
	// (e.g. "https://host.example.ts.net"), the base for the Linear Agent
	// responder's "Open in Compass" deep links (RIG-2717 T5, design Part 6).
	// Per-deployment, never hardcoded and with no default — the managed-service
	// host is a deployment concern that does not live in this repo. The CLI
	// supplies it (flag --public-url / $COMPASS_PUBLIC_URL); a deployment that
	// consumes Linear webhooks needs it non-empty, enforced by deepLinkFor's
	// boot guard where the responder is assembled.
	PublicURL string
	// Forge is the board webhook-ingestion lane + forge-write config (RIG-2883).
	// All-optional exactly like S3: board ingestion is off (no GitHub App) and
	// writes are off (no write secrets) unless the operator opts in — today's
	// behavior, zero new requirements on existing deployments. See ForgeConfig,
	// boardIngestionEnabled, and forgeWritesEnabled.
	Forge ForgeConfig
	// OtelEndpoint is the OTLP collector endpoint (OTEL_EXPORTER_OTLP_ENDPOINT);
	// empty = tracing off. It gates OTel emission on both binaries: when unset,
	// the bootstrap installs no provider and the RPC interceptors are inert
	// no-ops (no active span, so no traceresponse header).
	OtelEndpoint string
}

// ForgeConfig configures the board webhook-ingestion lane (RIG-2883) and the
// agent forge-WRITE path. All-optional: the board lane is off (no App config)
// and writes are off (no write secrets) unless the operator opts in, leaving
// today's behavior. The forge tables exist but sit empty — a migration is not a
// behavior change.
type ForgeConfig struct {
	// Host is the forge host the lane binds (default "github.com"); the API
	// base URL derives from it. Seed rows and the live target set are keyed
	// under this host, so changing it between boots abandons (does not migrate)
	// the prior host's rows.
	Host string
	// SeedRepos are "owner/name" repos boot-reconciled into
	// forge_repo_subscriptions (bootstrap-only insert, ON CONFLICT DO NOTHING;
	// lowercased for GITHUB) — a declarative SEED, not the live target set. The
	// live target set is always the table (WHERE enabled, read per pass).
	SeedRepos []string
	// App is the GitHub App credential the board webhook lane runs on
	// (RIG-2883, App-only cutover). The lane runs iff App.AppID != 0 AND both
	// App secrets are declared; otherwise board ingestion is hard-off with a
	// boot Warn. No PAT fallback on the read path (Constraint #3).
	App ForgeAppConfig
	// ReviewerApp is the SECOND GitHub App credential — a distinct App
	// definition (own AppID + private key + one installation) serving ONLY the
	// reviewer write client (the submit_review arm). A distinct GitHub identity
	// from the primary App so an agent approving a PR it authored dispatches
	// submit_review on a different account than it authored with, dissolving the
	// author-approving-own-PR 422 at the credential layer (F1, DEC-1). The
	// reviewer App registers NO webhook and no read lane, so its
	// AppWebhookSecretName is unused; reads/webhooks/board/author-writes all ride
	// the primary App (2-App topology, DEC-3).
	ReviewerApp ForgeAppConfig
	// LinearClientIDSecretName / LinearClientSecretName are the declared
	// server_only secret NAMEs holding the Linear OAuth client-credentials pair
	// (actor=app, the RIG-2682 "Compass" app). The Linear write + notify lanes
	// mint one shared client-credentials token from this pair (never a member
	// PAT); a Linear coordinate + notify lane are wired iff BOTH names resolve to
	// a declared secret (the VALUEs never cross config or a flag). Default to
	// LINEAR_FORGE_CLIENT_ID / LINEAR_FORGE_CLIENT_SECRET.
	LinearClientIDSecretName string
	LinearClientSecretName   string
	// LinearWebhookSecretName is the declared server_only secret NAME holding
	// the Linear webhook signing secret the shared POST /webhooks ingress
	// verifies deliveries against (the VALUE never crosses config or a flag).
	// The Linear data-change arm runs iff this resolves to a declared secret —
	// INDEPENDENT of the GitHub App gate (a deployment can run Linear
	// notifications without a GitHub App and vice versa).
	LinearWebhookSecretName string
}

// ForgeAppConfig is the GitHub App credential the board webhook-ingestion lane
// runs on (RIG-2883, frozen surface at
// docs/designs/server/compass-forge-agent-notification/design.md:1035-1048).
// The lane runs iff AppID != 0 AND both AppPrivateKeySecret and
// AppWebhookSecretName are declared (mirrors validateForgeSecret's fail-fast);
// no App -> board ingestion hard-off with a boot Warn (Constraint #3).
type ForgeAppConfig struct {
	// AppID is the GitHub App id (numeric). Zero means the board lane is off.
	AppID int64
	// InstallationID is the App installation id the token is minted for.
	InstallationID int64
	// AppPrivateKeySecret is the declared server_only secret NAME holding the
	// App PEM private key (the VALUE never crosses config or a flag).
	AppPrivateKeySecret string
	// AppWebhookSecretName is the declared server_only secret NAME holding the
	// webhook signing secret the ingress verifies deliveries against.
	AppWebhookSecretName string
	// ReconcileBackstop is the board reconciler's sweep cadence (default 30m).
	ReconcileBackstop time.Duration
}

// Forge config defaults, applied by resolveForge when a field is zero.
const (
	defaultForgeHost = "github.com"
	// defaultForgeLinearClientIDSecretName / defaultForgeLinearClientSecretName
	// are the default declared-secret NAMEs holding the Linear OAuth
	// client-credentials pair (actor=app, the RIG-2682 "Compass" app). The Linear
	// write coordinate + notify lane are wired iff BOTH resolve to a declared
	// secret; otherwise the write path is GitHub-only and the notify lane is off.
	// Secret NAMEs, not values (see the gosec note below).
	defaultForgeLinearClientIDSecretName = "LINEAR_FORGE_CLIENT_ID"     //nolint:gosec // G101: the default declared-secret NAME (an env-var identifier), not a credential value — resolved from the secrets provider, never hardcoded
	defaultForgeLinearClientSecretName   = "LINEAR_FORGE_CLIENT_SECRET" //nolint:gosec // G101: the default declared-secret NAME (an env-var identifier), not a credential value — resolved from the secrets provider, never hardcoded
	// defaultReconcileBackstop is the board reconciler's default sweep cadence
	// (OQ-5): startup sweep + a 30-min ticker (a 304 page-1 GET per enabled repo
	// is ≈ free, notify_reader.go:12-13; a cold-start zero watermark walks once).
	defaultReconcileBackstop = 30 * time.Minute
	// forgeTokenTTL is the TTL the cachedWebhookSecret hot-path cache holds a
	// resolved webhook signing secret for: /webhooks/{github,linear} resolve the
	// secret on every request before the HMAC check, and a resolve reads the whole
	// declared-secret registry, writes a manifest temp file, and drives a full
	// secretspec provider Load (resolver.go:135-165), so an uncached resolve would
	// let a garbage POST force that whole Load ahead of authentication. The cache
	// bounds the per-request cost to a memcmp; a rotated secret still takes effect
	// within the TTL.
	forgeTokenTTL = 5 * time.Minute
)

// boardIngestionEnabled reports whether the board webhook lane runs: iff the
// GitHub App is configured (AppID != 0). The two App secrets are additionally
// required and fail fast in buildBoardWebhookWiring (the shared site, RIG-2991);
// this predicate is the cheap AppID gate the caller checks before resolving
// anything (Constraint #3).
func (c ForgeConfig) boardIngestionEnabled() bool {
	return c.App.AppID != 0
}

// resolved returns the config with its zero fields defaulted (Host, the Linear
// client-credentials secret NAMEs, App.ReconcileBackstop). SeedRepos and App ids
// are taken verbatim; App private-key/webhook secret NAMEs are operator-set with
// no default (a configured App names them explicitly).
func (c ForgeConfig) resolved() ForgeConfig {
	if c.Host == "" {
		c.Host = defaultForgeHost
	}
	if c.LinearClientIDSecretName == "" {
		c.LinearClientIDSecretName = defaultForgeLinearClientIDSecretName
	}
	if c.LinearClientSecretName == "" {
		c.LinearClientSecretName = defaultForgeLinearClientSecretName
	}
	if c.App.ReconcileBackstop <= 0 {
		c.App.ReconcileBackstop = defaultReconcileBackstop
	}
	return c
}

// forgeWritesEnabled reports whether the agent forge-WRITE path is enabled: iff
// BOTH the primary App and the reviewer App are configured — each with AppID != 0
// AND its private-key secret present in declared (the 2-App cutover, DEC-1/DEC-3,
// re-keying the retired two-PAT-names predicate). Requiring the primary App to
// enable writes force-enables board ingestion (boardIngestionEnabled keys on the
// same App.AppID) — the unified shape Matt explicitly wants (DL-305). It is a
// pure predicate over the resolved declared-secret set so the rule is
// unit-testable without a running Serve; buildForgeWriteService re-validates each
// App key through validateForgeSecret to fail fast. Called on the resolved()
// config so the defaulted names apply.
func (c ForgeConfig) forgeWritesEnabled(declared []secrets.ResolvedSecret) bool {
	havePrimary, haveReviewer := c.forgeWriteAppsConfigured(declared)
	return havePrimary && haveReviewer
}

// forgeWriteAppsConfigured reports which of the two required write Apps — the
// primary (c.App) and the reviewer (c.ReviewerApp) — are configured: AppID != 0
// AND the App's private-key secret NAME present in the resolved declared set.
// Both true is the writes-enabled state (forgeWritesEnabled); exactly one true is
// a partial misconfiguration warnPartialForgeWriteSecrets surfaces. Resolves the
// config internally so any defaulted names apply — the caller need not
// pre-resolve.
func (c ForgeConfig) forgeWriteAppsConfigured(declared []secrets.ResolvedSecret) (havePrimary, haveReviewer bool) {
	fc := c.resolved()
	havePrimary = fc.App.AppID != 0 && secretDeclared(declared, fc.App.AppPrivateKeySecret)
	haveReviewer = fc.ReviewerApp.AppID != 0 && secretDeclared(declared, fc.ReviewerApp.AppPrivateKeySecret)
	return havePrimary, haveReviewer
}

// secretDeclared reports whether a non-empty name is present in the resolved
// declared set. An empty name is never declared (an App with a zero key-secret
// name is not configured).
func secretDeclared(declared []secrets.ResolvedSecret, name string) bool {
	if name == "" {
		return false
	}
	for _, s := range declared {
		if s.Name == name {
			return true
		}
	}
	return false
}

// The bootstrap-admin identity the local-socket door attributes callers to until
// the T3 interceptor sets a real caller (design.md:1219-1222). A fixed handle so
// BootstrapAdmin is idempotent across restarts (find-or-create by handle).
const (
	bootstrapAdminHandle      = "admin"
	bootstrapAdminDisplayName = "Administrator"
)

// resolvedAdminHandle is the bootstrap-admin handle after defaulting: cfg.AdminHandle
// when the operator set --admin-handle, else bootstrapAdminHandle ("admin"). Both
// the Serve bootstrap and the network door read the handle through this one seam so
// the minted account and the door's logging never drift.
func (cfg ServeConfig) resolvedAdminHandle() string {
	if cfg.AdminHandle != "" {
		return cfg.AdminHandle
	}
	return bootstrapAdminHandle
}

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

// openStore opens the store of record and wires the RIG-1667 T4 object-store
// archive seam onto it. When the S3 config is ABSENT (no endpoint/bucket) the
// seam is left nil and the server boots socket-only — the store's nil-guard
// fails a flush loudly only if one is ever attempted, so a dev server with no
// archive configured still starts. A present-but-invalid S3 config fails here.
func openStore(ctx context.Context, cfg ServeConfig) (*store.Store, error) {
	st, err := store.Open(ctx, cfg.DatabaseDSN)
	if err != nil {
		return nil, fmt.Errorf("opening store: %w", err)
	}
	if cfg.S3.Endpoint != "" && cfg.S3.Bucket != "" {
		objStore, err := store.NewS3ObjectStore(cfg.S3)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("constructing s3 object store: %w", err)
		}
		st.SetObjectStore(objStore)
	}
	return st, nil
}

// seedBootstrapAccounts brings up the two platform accounts a boot needs before
// serving: the bootstrap admin — the account the local-socket door attributes
// every RPC to until the network door sets a real caller (the 0600 socket is the
// local credential) — and the reserved system sender @compass, the first-turn
// delivery sender. Both are find-or-create by handle: minted on first boot,
// fetched on every later one, so the call is idempotent. The admin handle is
// operator-settable via --admin-handle (defaulting to bootstrapAdminHandle); a
// non-default handle already naming a non-admin account fails rather than
// elevating it, and a pre-existing @compass row of the wrong shape fails rather
// than being silently adopted — neither reserved account is ever quietly reused.
func seedBootstrapAccounts(ctx context.Context, st *store.Store, cfg ServeConfig) (admin, system store.Account, err error) {
	admin, err = st.BootstrapAdmin(ctx, store.NewUser{Handle: cfg.resolvedAdminHandle(), DisplayName: bootstrapAdminDisplayName})
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("bootstrapping admin account: %w", err)
	}
	system, err = st.EnsureSystemAccount(ctx)
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("seeding system account: %w", err)
	}
	slog.Default().Info("system account seeded", "account_id", system.ID, "handle", system.Handle)
	linearBridge, err := st.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		return store.Account{}, store.Account{}, fmt.Errorf("seeding linear bridge account: %w", err)
	}
	slog.Default().Info("linear bridge account seeded", "account_id", linearBridge.ID, "handle", linearBridge.Handle)
	return admin, system, nil
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
		return failStartup(udsListener, listeners, fmt.Errorf("chmod 0600 %s: %w", cfg.SocketPath, err))
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

	// The store of record (T1) backs the comms vertical and the token store, and
	// carries the RIG-1667 T4 object-store archive seam. Open it before serving so
	// a bad DSN, a failed migration, or a bad S3 config fails startup here, not
	// mid-request.
	st, err := openStore(ctx, cfg)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	defer st.Close()

	// Seed the platform accounts before serving: the bootstrap admin, the
	// reserved system sender @compass, and the reserved Linear bridge sender
	// @linear. Created unconditionally — even socket-only — so the AdminGate
	// always has a real admin id to compare against and the Linear responder
	// (a later wave) always finds its bridge author. All idempotent
	// find-or-create; a wrong-shape row under any reserved handle fails startup
	// rather than being adopted. The helper logs each seeded row so it is
	// observable at boot.
	admin, systemAccount, err := seedBootstrapAccounts(ctx, st, cfg)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}

	// Log the resolved public base URL so an operator can see which host the
	// Linear "Open in Compass" deep links will point at — there is no default, so
	// a deploy that forgets --public-url/$COMPASS_PUBLIC_URL logs an empty base
	// (and the responder-assembly boot guard rejects it when Linear webhooks are
	// enabled), and this line is how that misconfiguration is observable.
	slog.Default().Info("public base URL resolved", "public_url", cfg.PublicURL)

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

	// The Server-authoritative issue board projection: the durable PG-rehydrated
	// issue cache ListBoardIssues re-snapshots and the issue=16 live fan-out onto
	// SubscribeEvents ride the same instance (part 4). Rehydrate seeds it from the
	// store before serving so the first snapshot/fan-out is complete; nothing is
	// subscribed yet at boot, so it does not publish.
	issueBrd := board.NewIssueProjection(bus, st)
	if err := issueBrd.Rehydrate(ctx); err != nil {
		return failStartup(udsListener, listeners, fmt.Errorf("rehydrating issue board: %w", err))
	}

	// The comms event stream rides a second bus instance — its own seq space and
	// per-boot instance_epoch, distinct from the CompassService bus above. Built
	// before the RunnerHub because the hub's RelayCommsCall leg executes
	// agent-initiated comms calls through this handler (the CommsCaller).
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	defer commsBus.Close()
	commsSvc := comms.NewComms(st, commsBus, admin.ID)
	// Register the coordination-channel reconcile as the store's in-tx hook, so
	// the two parent-edge writers auto-provision/reconcile a manager's
	// coordination channel atomically with the tree edge (RIG-1722 T5). Wired here
	// before serving; the store invokes it on its own tx.
	commsSvc.RegisterCoordinationHook(st)

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
	hub := newRunnerHub(st, brd, tail, commsSvc, hubLog)
	svc := newService(cfg.Version, bus, st, hub, brd, issueBrd, tail)
	// Break the hub<->lifecycle (RIG-1618 T5), hub<->board (agent primary
	// lifecycle T3-a, RelayBoardCall), and comms<->hub ask-answer wake (RIG-1577)
	// construction cycles; see wireHubServiceCycles in sinks.go.
	wireHubServiceCycles(hub, commsSvc, st, issueBrd)
	// Seed the root Manager "supervisor" on first launch (RIG-1820). The seed
	// needs a Runner whose command stream can serve Provision/Start, which is not
	// up at boot — the embedded stack starts the Runner only after the server is
	// serving, and its command stream attaches only after it enrolls — so it
	// hangs off the hub's runner-ready hook, fired once a Runner's Sessions
	// stream attaches. Idempotent (find-or-create-then-start, empty-tree-gated
	// create), so a reconnect re-fire is safe. adminID is the bootstrap admin the
	// supervisor is owned by.
	seedLog := slog.Default()
	hub.SetRunnerReadyHook(func() { seedRootSupervisor(ctx, st, svc, commsSvc, admin.ID, systemAccount.ID, seedLog) })
	// The SecretsService is an account-facing sibling of CompassService/CommsService:
	// it mounts on every account door (socket, dev, network) behind the same bearer +
	// admin-gate chain, which classifies its three procedures authenticatedOpen — the
	// door admits any authenticated account and the handler enforces the user-only
	// writes / user-or-agent list. The hub is its SecretsVersion signaler (a Set/Delete
	// notifies live sessions to re-fetch); it shares the one resolver with FetchSecrets.
	secretsSvc := newSecretsService(st, resolver, hub)

	// The forge read-side credentials, built BEFORE the doors because the network
	// door mounts the board lane's webhook ingress (sink + secret resolver) and
	// the Linear notify lane, both threaded into buildDoors. Fails fast HERE on
	// the shared cleanup path. Board fields are nil when the GitHub App is absent;
	// forge.linearTokens is nil when Linear is not configured. Serve starts each
	// lane's arm + reconciler below.
	forgeWiring, err := buildForgeReadWiring(ctx, cfg, st, issueBrd, hub, resolver, hubLog)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}

	// Assemble the three compass.v1 doors (shipped Unix socket, optional dev
	// loopback, optional authenticated network) plus the Linear notify lane the
	// net door's /webhooks/linear handler feeds. On a net-door build error the
	// listeners this Serve bound are still ours to close.
	doors, err := buildDoors(ctx, cfg, svc, commsSvc, secretsSvc, hub, st, admin.ID, resolver,
		devListener, netListener, netTLS, forgeWiring.webhookSink, forgeWiring.webhookSecret, forgeWiring.linearTokens)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}

	// The forge-WRITE caller: enabled iff BOTH the primary and reviewer Apps are
	// configured (the 2-App cutover). The author write leg REUSES the shared
	// primary App client (forgeWiring.primaryClient) so it rides the one budget
	// gate; the reviewer leg gets its own App client; the Linear coordinate rides
	// the shared forgeWiring.linearTokens instance.
	if err := wireForgeWriteCaller(ctx, cfg, st, issueBrd, resolver, hub, hubLog, forgeWiring.primaryClient, forgeWiring.linearTokens, udsListener, listeners); err != nil {
		return err
	}

	// Run every door under one scoped group. errgroup.WithContext gives the
	// listener/drain coordination scoped lifecycle, first-error-wins, and sibling
	// cancellation in one audited primitive: gctx is cancelled when the parent ctx
	// is cancelled (normal shutdown) or when a server self-terminates with an
	// error (the first exit tears down the peers). The UDS door is primary; its
	// error — recorded first by the group — wins over the drain result below.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return classifyServe(doors.uds.Serve(udsListener), "compass.v1 UDS server") })
	if doors.dev != nil {
		g.Go(func() error { return classifyServe(doors.dev.Serve(devListener), "dev gRPC-Web server") })
	}
	if doors.net != nil {
		// ServeTLS wraps the pre-bound listener with the configured keypair
		// (already in TLSConfig, so the empty cert/key args skip a file load) and
		// advertises ALPN h2 from Protocols. A clean shutdown returns
		// ErrServerClosed, mapped to nil by classifyServe.
		g.Go(func() error {
			return classifyServe(doors.net.ServeTLS(netListener, "", ""), "compass.v1 network door")
		})
	}
	// The forge webhook-ingestion lanes (RIG-2883 board + RIG-2732 T7 notify):
	// each lane's webhook-arm drain and reconciler sweep join the SAME scoped
	// group so they inherit the doors' lifecycle exactly — cancelled on
	// SIGINT/SIGTERM via gctx, first-error-wins, drained with everything else.
	// The board + GitHub notify lanes are nil when the GitHub App is absent (they
	// share the App gate); the Linear notify lane is nil when Linear is not
	// configured (its client-credentials pair undeclared). A nil lane starts
	// nothing. Every Run returns nil on ctx-cancel.
	startForgeIngestLanes(gctx, g, forgeWiring.boardLane, forgeWiring.notifyLane, doors.linearNotify)
	// The comms-bus consumers (RIG-1569): the T3 delivery fan-out consumer and
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
			uds:      doors.uds,
			dev:      doors.dev,
			net:      doors.net,
			hub:      hub,
			log:      hubLog,
		})
	})

	return g.Wait()
}

// serveDoors holds the three compass.v1 doors Serve drives: the shipped Unix
// socket (always built), the optional dev loopback browser door, and the
// optional authenticated network door. dev and net are nil when their listener
// is off (dev unless --dev-http, network unless --listen).
type serveDoors struct {
	uds *http.Server
	dev *http.Server
	net *http.Server
	// linearNotify is the Linear agent-notification lane (RIG-2732 T7), built
	// beside the webhook handler it feeds; nil when Linear is not configured (its
	// client-credentials pair undeclared). Serve starts its arm + reconciler on
	// the serve group.
	linearNotify *forgeNotifyLane
}

// buildDoors assembles the three compass.v1 doors off the already-built service
// graph and pre-bound listeners. It is split out of Serve so the door assembly —
// whose interceptor ordering is security-critical (see the dev door below) —
// reads as one unit. It does not serve; the returned servers are driven by the
// caller's errgroup. On a net-door build error it returns the error and the
// caller owns listener cleanup (nothing here is registered for teardown).
func buildDoors(
	ctx context.Context,
	cfg ServeConfig,
	svc *service,
	commsSvc *comms.Comms,
	secretsSvc *secretsService,
	hub *runnerhub.Hub,
	st *store.Store,
	adminID store.AccountID,
	resolver secrets.Resolver,
	devListener net.Listener,
	netListener net.Listener,
	netTLS *tls.Config,
	webhookSink ForgeEventSink,
	webhookSecret func(ctx context.Context) ([]byte, error),
	linearTokens *linearagent.TokenSource,
) (serveDoors, error) {
	// otelconnect produces the server RPC span (and, once a MeterProvider is
	// installed, RPC duration/count metrics); NewTraceResponseInterceptor stamps
	// the span's trace id onto the "traceresponse" response header. Both are
	// inert no-ops when OtelEndpoint is empty (no provider ⇒ no active span), so
	// they are mounted unconditionally. otelconnect goes FIRST (outermost) in
	// every chain so the span envelopes the security-critical interceptors and
	// the AdminGate→Ambient ordering is unchanged relative to itself.
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		return serveDoors{}, fmt.Errorf("otel: rpc interceptor: %w", err)
	}
	// CommsService rides the socket + dev doors and the network shared chain; it
	// mounts the same otelconnect + trace-response pair as CompassService.
	commsPath, commsHandler := compassv1connect.NewCommsServiceHandler(commsSvc,
		connect.WithInterceptors(otelIC, otel.NewTraceResponseInterceptor()))
	// Shipped door: the Unix socket serves native gRPC (cleartext HTTP/2),
	// gRPC-Web, and Connect off the one connect-go handler. No CORS — the socket
	// is same-origin (the shell's webview / a native client). The 0600 socket is
	// the credential: the CompassService handler mounts the ambient-identity
	// interceptor pair (bootstrap admin), and comms attributes every RPC to the
	// bootstrap admin via its own actor fallback. Cleartext HTTP/2 is served
	// natively (http.Protocols); these connections are tracked by
	// http.Server.Shutdown and drain with the rest on shutdown.
	socketPath, socketHandler := compassv1connect.NewCompassServiceHandler(svc,
		connect.WithInterceptors(otelIC, otel.NewTraceResponseInterceptor(),
			auth.AmbientIdentity(adminID), auth.AmbientStreamInterceptor(adminID)))
	// SecretsService rides the same ambient-identity pair so its handler reads a
	// caller via CallerFrom (the bootstrap admin, a user): the socket's 0600 mode
	// is the credential, and admin being a user satisfies the user-only writes.
	secretsSocketPath, secretsSocketHandler := compassv1connect.NewSecretsServiceHandler(secretsSvc,
		connect.WithInterceptors(auth.AmbientIdentity(adminID), auth.AmbientStreamInterceptor(adminID)))
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
			connect.WithInterceptors(otelIC, otel.NewTraceResponseInterceptor(),
				auth.NewAdminGate(adminID), auth.AmbientIdentity(adminID), auth.AmbientStreamInterceptor(adminID)))
		// SecretsService on the dev door: same admin-gate + ambient chain as
		// CompassService. The gate classifies its 3 procedures authenticatedOpen,
		// so it passes them, then the ambient pair attaches the bootstrap admin
		// (a user) the handler reads for the user-only write authz.
		devSecretsPath, devSecretsHandler := compassv1connect.NewSecretsServiceHandler(secretsSvc,
			connect.WithInterceptors(auth.NewAdminGate(adminID), auth.AmbientIdentity(adminID), auth.AmbientStreamInterceptor(adminID)))
		devMux := http.NewServeMux()
		devMux.Handle(devPath, devHandler)
		devMux.Handle(commsPath, commsHandler)
		devMux.Handle(devSecretsPath, devSecretsHandler)
		devServer = &http.Server{Handler: devCORS().Handler(devMux), Protocols: cleartextHTTP2()} //nolint:gosec // G112: loopback dev-only door (off on the shipped path), so the Slowloris ReadHeaderTimeout does not apply here either
	}

	// The Linear agent-notification lane (RIG-2732 T7): App-INDEPENDENT, gated on
	// the shared Linear client-credentials token source (nil when Linear is not
	// configured). Built here — beside the webhook handler it feeds — so its
	// data-change sink threads straight into buildLinearWebhookWiring below,
	// replacing the injected-and-nil-for-now sink so a verified /webhooks/linear
	// Issue/Comment event routes to subscribers instead of ack-and-drop. Nil when
	// linearTokens is nil (the handler's data branch then acks-and-drops). The
	// lane is returned in serveDoors so Serve can start its arm + reconciler on
	// the serve group.
	linearNotifyLane := buildLinearNotifyLane(st, hub, linearTokens, slog.Default())
	var linearDataSink ForgeEventSink
	if linearNotifyLane != nil {
		linearDataSink = linearNotifyLane.sink
	}

	// The Linear webhook ingress (RIG-2732 T7d / RIG-2717): a shared
	// POST /webhooks/linear handler (DL-302) built iff the Linear webhook secret
	// is declared — an App-INDEPENDENT gate (a deployment can run Linear
	// notifications without a GitHub App). Its data-change arm's sink is the
	// Linear-provider-bound notify lane's sink (linearDataSink), so a verified
	// Issue/Comment event routes to subscribers at the LINEAR/linear.app
	// coordinate; nil when the notify lane is off (Linear not configured), and the
	// handler's data branch then acks-and-drops. The two gates are independent:
	// the webhook secret gates the handler; the Linear client-credentials pair
	// gates the sink. Its session arm is left unwired (nil sessionSink -> logged-drop)
	// until the RIG-2717 responder assembly wires a *linearagent.Dispatcher here.
	// The handler is mounted only on the net door below, when one exists.
	linearWebhookHandler, err := buildLinearWebhookWiring(ctx, cfg, resolver, linearDataSink, slog.Default())
	if err != nil {
		return serveDoors{}, err
	}

	// Authenticated network door, built only when --listen is given. It mints and
	// writes the bootstrap token 0600 under the state dir (so a socket-only start
	// leaves none behind) and mounts the CompassService + CommsService behind the
	// bearer + admin-gate interceptors, plus the internal RunnerService door a
	// Runner enrolls over (Runner-subject bearer, separate from the account door)
	// and — when the board lane is on (webhookSink != nil) — the internet-facing
	// POST /webhooks/github ingress OUTSIDE the bearer/admin gate. On a build
	// error the listeners this Serve bound are still ours to close.
	var netServer *http.Server
	if netListener != nil {
		s, err := buildNetworkServer(ctx, cfg, svc, commsSvc, secretsSvc, hub, st, adminID, netTLS, resolver, otelIC, webhookSink, webhookSecret, linearWebhookHandler)
		if err != nil {
			return serveDoors{}, err
		}
		netServer = s
	}

	return serveDoors{uds: udsServer, dev: devServer, net: netServer, linearNotify: linearNotifyLane}, nil
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
		ExposedHeaders:   append(connectcors.ExposedHeaders(), "traceresponse"),
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

// boardIngestLane is the assembled board webhook-ingestion lane (RIG-2883): the
// T1 webhook arm (whose Run drains the ingress queue and whose Enqueue is the
// accepted-event sink) and the T3 reconciler (whose Run sweeps the backstop).
// The caller composes sink into the ingress fanoutSink and adds arm.Run +
// reconciler.Run to the serve errgroup.
type boardIngestLane struct {
	arm        *ingest.BoardWebhookArm
	reconciler *ingest.BoardReconciler
	sink       ForgeEventSink // the arm; the ingress fan-out registers it
	// client is the shared GitHub App client this lane's arm + reconciler ride
	// (the SAME object buildForgeNotifyLane is handed). Recorded here as
	// test-observable state so the RIG-2991 one-budget-gate unit test can assert
	// both lanes ride ONE client — a builder that minted its own would record a
	// different pointer and flip that assertion red. Production reads it never.
	client *forge.GitHub
}

// forgeReadWiring bundles the forge read-side credentials Serve builds once and
// threads into the doors + the write path: the board + notify lanes over the
// shared primary App client (the board arm's webhook ingress sink + secret), the
// shared primary App client the author write leg reuses, and the ONE Linear
// OAuth token source both the notify lane and the Linear write coordinate ride.
// Board fields are nil when the GitHub App is absent; linearTokens is nil when
// Linear is not configured.
type forgeReadWiring struct {
	boardLane     *boardIngestLane
	notifyLane    *forgeNotifyLane
	webhookSink   ForgeEventSink
	webhookSecret func(ctx context.Context) ([]byte, error)
	primaryClient *forge.GitHub
	linearTokens  *linearagent.TokenSource
}

// buildForgeReadWiring assembles the forge read-side credentials in one call:
// the board webhook wiring (both App lanes + the shared primary client) and the
// shared Linear OAuth token source. Either half is independently off (App absent
// -> nil board fields; Linear unconfigured -> nil linearTokens); a resolve or
// boot-time-mint fault from either is returned so Serve fails fast on its
// cleanup path.
func buildForgeReadWiring(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	hub *runnerhub.Hub,
	resolver secrets.Resolver,
	log *slog.Logger,
) (forgeReadWiring, error) {
	boardLane, notifyLane, webhookSink, webhookSecret, primaryClient, err := buildBoardWebhookWiring(ctx, cfg, st, issueBrd, hub, resolver, log)
	if err != nil {
		return forgeReadWiring{}, err
	}
	linearTokens, err := buildLinearTokenSource(ctx, cfg, resolver, log)
	if err != nil {
		return forgeReadWiring{}, err
	}
	return forgeReadWiring{
		boardLane:     boardLane,
		notifyLane:    notifyLane,
		webhookSink:   webhookSink,
		webhookSecret: webhookSecret,
		primaryClient: primaryClient,
		linearTokens:  linearTokens,
	}, nil
}

// buildBoardWebhookWiring builds BOTH forge lanes (board ingestion + agent
// notification) over ONE shared GitHub App client and derives the network
// door's webhook ingress wiring: the lanes themselves (whose arms + reconcilers
// Serve starts under the door errgroup), the fan-out sink each accepted delivery
// is enqueued onto, and the lazy resolver for the delivery-signing secret. When
// the App is absent both lanes are hard-off, so it Warns when enabled
// subscription rows exist and returns all-nil, which the network door reads as
// "mount no ingress". A validation fault (a configured App with a missing
// secret) is returned so Serve fails fast on its cleanup path. The fanoutSink
// carries BOTH forge arms: the board arm (RIG-2883) and the notify arm (T7), so
// each accepted delivery fans out to both the board ingest and the
// agent-notification hot path off the one /webhooks/github ingress. Serve starts
// each lane's arm + reconciler on the same errgroup.
//
// This is the single site that gates on the App, validates the two App secrets,
// and builds the App token source + GitHub client — the ONE client both lanes
// ride, so the client-side rate-budget/resetAt gate is a single gate across
// board ingestion and agent notification (RIG-2991), not two independent gates
// against the same installation.
func buildBoardWebhookWiring(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	hub *runnerhub.Hub,
	resolver secrets.Resolver,
	log *slog.Logger,
) (*boardIngestLane, *forgeNotifyLane, ForgeEventSink, func(ctx context.Context) ([]byte, error), *forge.GitHub, error) {
	rc := cfg.Forge.resolved()
	// App absent -> both forge lanes hard-off. Warn once here (the single
	// diagnostic site) when enabled subscription rows exist, and mount nothing.
	if !cfg.Forge.boardIngestionEnabled() {
		warnDisabledBoardIngestion(ctx, st, store.ForgeProviderGitHub, rc.Host, log)
		return nil, nil, nil, nil, nil, nil
	}
	// Validate both App secrets ONCE at this shared site (distinct fail-fast
	// texts via validateForgeSecret) so a configured App with a missing secret
	// fails startup and BOTH lanes inherit a validated App — the notify lane no
	// longer relies on the board lane validating first.
	if err := validateForgeSecret(ctx, resolver, "board webhook app key", rc.App.AppPrivateKeySecret); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if err := validateForgeSecret(ctx, resolver, "board webhook secret", rc.App.AppWebhookSecretName); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	// Build the ONE shared App token source + GitHub client both lanes ride.
	// appTokenSource is safe for concurrent use (mint singleflighted), so the
	// read lanes and the poll driver sharing one client is sound.
	tok, err := forge.NewAppTokenSource(forge.GitHubAppConfig{
		AppID:          rc.App.AppID,
		InstallationID: rc.App.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, rc.App.AppPrivateKeySecret),
		Host:           rc.Host,
	})
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("board webhook app token source: %w", err)
	}
	client := forge.NewGitHub(forge.GitHubConfig{Host: rc.Host, Token: tok})

	lane, err := buildBoardIngestLane(ctx, cfg, st, issueBrd, client, log)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	notifyLane := buildForgeNotifyLane(cfg, st, hub, client, log)
	sink := &fanoutSink{sinks: []ForgeEventSink{lane.sink, notifyLane.sink}}
	secret := newCachedWebhookSecret(resolver, rc.App.AppWebhookSecretName)
	// client is returned as the shared primary App client the author write leg
	// REUSES (RIG-2991 + the App cutover): the SAME *forge.GitHub object board
	// reads, notify reads, AND author writes ride, so one client-side
	// rate-budget/resetAt gate spans all three against the single installation.
	return lane, notifyLane, sink, secret, client, nil
}

// buildLinearWebhookWiring builds the shared Linear POST /webhooks/linear
// handler (DL-302) when the Linear webhook secret is declared — an
// App-INDEPENDENT gate (a deployment can run Linear notifications without a
// GitHub App, so this does NOT check boardIngestionEnabled). When the secret is
// undeclared it returns a nil handler and buildNetworkServer mounts no
// /webhooks/linear route. A resolve FAULT fails startup (the same fail-fast as
// forgeSecretDeclared); an absent name is the clean off-state, not an error.
//
// dataSink is the data-change arm's sink, injected-and-nil-for-now by driver
// decision (DL-302): feeding the GitHub-coordinate notify+board fanoutSink would
// mis-route Linear events (Linear subs looked up under a GitHub coordinate), so
// the handler's data branch acks-and-drops on a nil sink until a
// Linear-provider-bound notify lane injects a real sink here. The session arm is
// left unwired (nil sessionSink -> the handler logs-and-drops session events
// with a 200): the RIG-2717 responder assembly, a separate in-flight lane, wires
// a real *linearagent.Dispatcher once it assembles one in Serve. The secret is
// TTL-cached (newCachedWebhookSecret): /webhooks/linear is an internet-facing,
// unauthenticated endpoint whose secret is resolved on EVERY request BEFORE the
// HMAC check, so an uncached resolve would let a garbage POST force a full
// secretspec Load ahead of authentication.
func buildLinearWebhookWiring(
	ctx context.Context,
	cfg ServeConfig,
	resolver secrets.Resolver,
	dataSink ForgeEventSink,
	log *slog.Logger,
) (http.Handler, error) {
	name := cfg.Forge.LinearWebhookSecretName
	if name == "" {
		return nil, nil //nolint:nilnil // undeclared secret is a valid off-state: a nil handler is the signal buildNetworkServer guards on, not an ambiguous nil-nil.
	}
	declared, err := forgeSecretDeclared(ctx, resolver, name)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, nil //nolint:nilnil // a set-but-undeclared name is the operator's off-state; the write path's forgeSecretDeclared treats an absent optional name the same way.
	}
	secret := newCachedWebhookSecret(resolver, name)
	_, handler := NewLinearWebhookHandler(secret, dataSink, nil, log)
	return handler, nil
}

// buildBoardIngestLane assembles the App-only board webhook-ingestion lane
// (RIG-2883 T5) over the shared GitHub client. The caller
// (buildBoardWebhookWiring) owns the App gate, the two App-secret validations,
// and the client construction — this builder is only reached when the App is
// configured and validated, so it assembles unconditionally.
//
// In order it: (1) reconciles the seed repos into forge_repo_subscriptions
// (bootstrap-only insert, ON CONFLICT DO NOTHING, lowercased for GITHUB) so the
// target rows are visible before the first sweep; (2) assembles the pipeline
// over the shared client — the Ingester (sharing issueBrd) -> the T1 webhook arm
// (over the store's IsEnabledForgeRepo target check) and the T3 reconciler (over
// the store's enabled-repo + watermark seam). Both ingest seams are adapted onto
// *store.Store binding (provider, host) — the ingest package owns no store type,
// so go/server supplies these thin adapters.
func buildBoardIngestLane(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	client *forge.GitHub,
	log *slog.Logger,
) (*boardIngestLane, error) {
	fc := cfg.Forge.resolved()
	// The board lane ships a GitHub client only this slice (Global Constraints);
	// the bound provider is GITHUB.
	const provider = store.ForgeProviderGitHub

	// (1) Seed reconcile BEFORE the first sweep: each seed repo lowercased (for
	// GITHUB) and inserted enabled under ON CONFLICT DO NOTHING — additive, never
	// destructive, never re-enabling. Bad repo format fails startup.
	if err := reconcileForgeSeed(ctx, st, provider, fc.Host, cfg.Forge.SeedRepos); err != nil {
		return nil, err
	}

	// (2) Assemble the pipeline over the shared client: the Ingester (sharing the
	// existing issue projection), and the two arms over store adapters binding
	// (provider, host).
	ing := ingest.NewIngester(client, issueBrd, &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     fc.Host,
	})
	arm := ingest.NewBoardWebhookArm(client, ing, &boardTargetStore{st: st}, ingest.BoardArmConfig{Log: log})
	reconciler := ingest.NewBoardReconciler(client, ing, &boardReconcileStore{st: st, provider: provider, host: fc.Host}, ingest.BoardReconcileConfig{
		Backstop: fc.App.ReconcileBackstop,
		Log:      log,
	})
	return &boardIngestLane{arm: arm, reconciler: reconciler, sink: arm, client: client}, nil
}

// boardTargetStore adapts *store.Store to ingest.TargetChecker: the point
// membership check the webhook arm gates each event on. IsEnabledForgeRepo is
// repo-only keyed (unambiguous in a github.com-only deployment), so this adapter
// binds no coordinate — it delegates directly.
type boardTargetStore struct {
	st *store.Store
}

// IsEnabledRepo reports whether an enabled subscription exists for repo.
func (a *boardTargetStore) IsEnabledRepo(ctx context.Context, repo string) (bool, error) {
	return a.st.IsEnabledForgeRepo(ctx, repo)
}

// boardReconcileStore adapts *store.Store to ingest.BoardStore, binding the
// forge coordinate half (provider, host) so the ingest-side seam stays
// repo-keyed and this package holds the store dependency the ingest package's
// no-store rule keeps out. ListEnabledRepos is repo-only keyed (the T4 seam),
// so the bound coordinate is used only for the coordinate-keyed watermark
// load/store.
type boardReconcileStore struct {
	st       *store.Store
	provider store.ForgeProvider
	host     string
}

// ListEnabledRepos returns every enabled target's repo across coordinates (the
// reconciler's per-sweep target enumeration).
func (a *boardReconcileStore) ListEnabledRepos(ctx context.Context) ([]string, error) {
	return a.st.ListEnabledForgeRepos(ctx)
}

// LoadRepoWatermark returns the repo's swept-updated-at watermark + list ETag
// for the bound coordinate (zero values on a never-swept repo).
func (a *boardReconcileStore) LoadRepoWatermark(ctx context.Context, repo string) (time.Time, string, error) {
	return a.st.LoadForgeRepoWatermark(ctx, a.provider, a.host, repo)
}

// StoreRepoWatermark persists the repo's watermark + ETag for the bound
// coordinate after its rows sank (advance-after-sink).
func (a *boardReconcileStore) StoreRepoWatermark(ctx context.Context, repo string, mark time.Time, etag string) error {
	return a.st.StoreForgeRepoWatermark(ctx, a.provider, a.host, repo, mark, etag)
}

// forgeNotifyLane is the assembled GitHub agent-notification lane (RIG-2732 T7):
// the notify webhook arm (whose Run drains the ingress queue and whose Enqueue is
// the accepted-event sink) and the notify reconciler (whose Run sweeps the
// backstop). It rides the SAME /webhooks/github ingress the board lane mounts —
// the caller composes sink into the ingress fanoutSink and adds arm.Run +
// reconciler.Run to the serve errgroup, exactly like the board lane.
type forgeNotifyLane struct {
	arm        *ingest.NotifyWebhookArm
	reconciler *ingest.NotifyReconciler
	sink       ForgeEventSink // the arm; the ingress fan-out registers it
	// reader is the notify-read client this lane's reconciler + checks roller
	// ride. For the GitHub lane it is the SAME *forge.GitHub buildBoardIngestLane
	// is handed (the shared App client); the Linear lane records its own
	// *forge.Linear. Recorded here as test-observable state so the RIG-2991
	// one-budget-gate unit test can assert the GitHub board + notify lanes ride
	// ONE client — a builder that minted its own would record a different pointer.
	// Production reads it never.
	reader forge.NotifyReader
}

// forgeNotifyStore adapts *store.Store to ingest.NotifyStore, binding the forge
// coordinate half (provider, host) so the ingest-side seam stays coordinate-free
// and this package holds the store dependency the ingest package's no-store rule
// keeps out. The ingest ArtifactKind enum and the store ForgeArtifactKind enum
// share the same underlying int32 values (both mirror the forge.proto kind), so
// the (provider, host)-drop conversion is a direct cast, the established pattern
// (ListForgeNotifyTargets, forge_subscriptions.go:401).
type forgeNotifyStore struct {
	st       *store.Store
	provider store.ForgeProvider
	host     string
}

// LoadArtifactCursor point-reads the coordinate's shared FETCH cursor, dropping
// the bound (provider, host) — the ingest cursor is coordinate-bound. nil (never
// observed) maps to nil.
func (a *forgeNotifyStore) LoadArtifactCursor(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (*ingest.ArtifactCursor, error) {
	cur, err := a.st.LoadForgeArtifactCursor(ctx, a.provider, a.host, repo, store.ForgeArtifactKind(kind), number)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, nil //nolint:nilnil // a never-observed coordinate is (nil, nil) by the seam contract (notify_router.go:79-81); the router guards nil.
	}
	return toIngestCursor(cur), nil
}

// SubscribersForArtifact returns the subscribers a change fans out to for the
// bound coordinate, converting the store subscriber rows to the ingest mirror.
func (a *forgeNotifyStore) SubscribersForArtifact(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, project string, opened bool) ([]ingest.NotifySubscriber, error) {
	subs, err := a.st.SubscribersForArtifact(ctx, a.provider, a.host, repo, store.ForgeArtifactKind(kind), number, project, opened)
	if err != nil {
		return nil, err
	}
	return toIngestSubscribers(subs), nil
}

// ListNotifyTargets enumerates every subscribed coordinate + its cursor and
// riding subscribers for the bound (provider, host) — the reconcile sweep's work
// list.
func (a *forgeNotifyStore) ListNotifyTargets(ctx context.Context) ([]ingest.NotifyTarget, error) {
	targets, err := a.st.ListForgeNotifyTargets(ctx, a.provider, a.host)
	if err != nil {
		return nil, err
	}
	out := make([]ingest.NotifyTarget, 0, len(targets))
	for _, t := range targets {
		nt := ingest.NotifyTarget{
			Repo:        t.Repo,
			Kind:        compassv1internal.ForgeArtifactKind(t.Kind),
			Number:      t.Number,
			Subscribers: toIngestSubscribers(t.Subscribers),
		}
		nt.Cursor = toIngestCursor(t.Cursor)
		out = append(out, nt)
	}
	return out, nil
}

// UpsertArtifactCursor writes the coordinate's shared FETCH cursor, re-binding
// the (provider, host) the ingest type drops.
func (a *forgeNotifyStore) UpsertArtifactCursor(ctx context.Context, cur ingest.ArtifactCursor) error {
	return a.st.UpsertForgeArtifactCursor(ctx, store.ForgeArtifactCursor{
		Provider:     a.provider,
		Host:         a.host,
		Repo:         cur.Repo,
		Kind:         store.ForgeArtifactKind(cur.Kind),
		Number:       cur.Number,
		ETag:         cur.ETag,
		CommentsETag: cur.CommentsETag,
		ChecksETag:   cur.ChecksETag,
		Revision:     cur.Revision,
		Snapshot:     cur.Snapshot,
	})
}

// toIngestSubscribers converts the store subscriber rows to the ingest mirror
// (the no-store rule keeps the store type out of the ingest package).
func toIngestSubscribers(subs []store.ForgeNotifySubscriber) []ingest.NotifySubscriber {
	if len(subs) == 0 {
		return nil
	}
	out := make([]ingest.NotifySubscriber, 0, len(subs))
	for _, s := range subs {
		out = append(out, ingest.NotifySubscriber{
			SubscriptionID:    s.SubscriptionID,
			AgentAccountID:    string(s.AgentAccountID),
			DeliveredRevision: s.DeliveredRevision,
			Project:           s.Project,
		})
	}
	return out
}

// toIngestCursor converts a store artifact cursor to the ingest mirror
// (nil-in -> nil-out), the sibling of toIngestSubscribers. Both the point-read
// (LoadArtifactCursor) and the sweep enumeration (ListNotifyTargets) share it so
// the 8-field copy lives once and a new cursor field can't drift between them.
func toIngestCursor(cur *store.ForgeArtifactCursor) *ingest.ArtifactCursor {
	if cur == nil {
		return nil
	}
	return &ingest.ArtifactCursor{
		Repo:         cur.Repo,
		Kind:         compassv1internal.ForgeArtifactKind(cur.Kind),
		Number:       cur.Number,
		ETag:         cur.ETag,
		CommentsETag: cur.CommentsETag,
		ChecksETag:   cur.ChecksETag,
		Revision:     cur.Revision,
		Snapshot:     cur.Snapshot,
	}
}

// errNoLiveSession is the sentinel forgeNotifyDispatcher.Notify returns when the
// resolved subscriber has no live session: the router logs it and moves on, and
// the reconcile sweep re-notifies from the durable gap (W3). It is DELIBERATELY
// not a cursor advance — no cursor is touched on a no-session dispatch.
var errNoLiveSession = errors.New("forge notify: no live session for account")

// notifySessionDispatcher is the hub surface forgeNotifyDispatcher drives:
// resolve an account to its live session, then dispatch a control frame to it.
// Both methods are on *runnerhub.Hub (the production impl); naming them as a
// local interface lets a unit test inject a fake and exercise Notify's resolve/
// miss branches and the AgentControl wrapping without a live hub or Postgres.
// It mirrors the delivery package's SessionResolver + ControlDispatcher split.
type notifySessionDispatcher interface {
	SessionForAccount(account store.AccountID) (sessionID string, ok bool)
	DispatchControl(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error
}

// forgeNotifyDispatcher adapts the hub to ingest.NotifyDispatcher: resolve the
// subscriber's account to its live session and DispatchControl the notification.
// It NEVER advances delivered_revision (W3) — the hub's ForgeNotificationAck arm
// owns that (already boot-wired via SetDeliveryStore).
type forgeNotifyDispatcher struct {
	hub notifySessionDispatcher
}

// Notify resolves account -> live session -> DispatchControl(ForgeNotification).
// A missing live session returns errNoLiveSession (the router logs and moves on);
// a live session dispatches the notification wrapped as an AgentControl and
// returns the dispatch error.
func (d *forgeNotifyDispatcher) Notify(ctx context.Context, account string, n *compassv1internal.ForgeNotification) error {
	sessionID, ok := d.hub.SessionForAccount(store.AccountID(account))
	if !ok {
		return errNoLiveSession
	}
	return d.hub.DispatchControl(ctx, sessionID, &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_ForgeNotification{ForgeNotification: n},
	})
}

// forgeNotifyChecksRoller adapts a forge.NotifyReader to ingest.ChecksRoller: the
// combined checks roll-up a CHECKS event needs. NotifyReader's ChecksConditional
// has the exact RollUp signature (notify_router.go:111-113), so this is a
// one-method forwarding adapter rather than a method value, keeping the seam an
// explicit named type. It holds the NotifyReader interface (not a concrete
// *forge.GitHub) so ONE adapter serves both the GitHub lane (client is a
// *forge.GitHub) and the Linear lane (client is a *forge.Linear) — both satisfy
// forge.NotifyReader (notify_reader.go:77,367). Linear's ChecksConditional
// returns ErrUnsupported, but the router invokes RollUp only on a CHECKS-kind
// event, which Linear (issues-only) never produces — so it is correct and
// never-called (fail-closed if one somehow arrived).
type forgeNotifyChecksRoller struct {
	client forge.NotifyReader
}

// RollUp resolves the combined checks roll-up for the head SHA, forwarding to the
// client's conditional checks read.
func (r *forgeNotifyChecksRoller) RollUp(ctx context.Context, repo string, number uint64, headSHA, etag string) (forge.ConditionalResult[forge.Checks], error) {
	return r.client.ChecksConditional(ctx, repo, number, headSHA, etag)
}

// buildForgeNotifyLane assembles the App-only GitHub agent-notification lane
// (RIG-2732 T7) over the shared GitHub client. The caller
// (buildBoardWebhookWiring) owns the App gate, secret validation, and client
// construction, and only reaches this builder when the App is configured — so it
// assembles unconditionally and cannot fail (no resolve, no error return).
//
// It rides the SAME forge.GitHub client the board lane rides (RIG-2991), so the
// client-side rate-budget/resetAt gate — the gate the reconciler's
// ErrBudgetExhausted rides — is ONE gate shared across board ingestion and
// agent notification against the single App installation, not two independent
// gates. It assembles the pipeline: the (provider, host)-bound store adapter,
// the hub-backed dispatcher, the checks roller over the shared client, the
// router, the webhook arm, and the reconciler.
func buildForgeNotifyLane(
	cfg ServeConfig,
	st *store.Store,
	hub *runnerhub.Hub,
	client *forge.GitHub,
	log *slog.Logger,
) *forgeNotifyLane {
	fc := cfg.Forge.resolved()
	const provider = store.ForgeProviderGitHub

	notifyStore := &forgeNotifyStore{st: st, provider: provider, host: fc.Host}
	dispatcher := &forgeNotifyDispatcher{hub: hub}
	checks := &forgeNotifyChecksRoller{client: client}
	forgeRef := &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     fc.Host,
	}
	router := ingest.NewNotifyRouter(notifyStore, dispatcher, checks, forgeRef, log)
	arm := ingest.NewNotifyWebhookArm(router, ingest.NotifyArmConfig{Log: log})
	reconciler := ingest.NewNotifyReconciler(client, notifyStore, router,
		compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, fc.Host, ingest.ReconcileConfig{
			Backstop: fc.App.ReconcileBackstop,
			Log:      log,
		})
	return &forgeNotifyLane{arm: arm, reconciler: reconciler, sink: arm, reader: client}
}

// buildLinearNotifyLane assembles the Linear agent-notification lane (RIG-2732
// T7), the Linear sibling of buildForgeNotifyLane. It is gated by the caller on
// the shared Linear OAuth client-credentials token source: a nil tokens means
// Linear is not configured, so it returns nil — the off-state the caller reads
// as "mount no Linear notify sink". A non-nil tokens builds the lane over a
// Linear client riding the SAME token-source instance the write coordinate rides
// (the one-instance rule, DEC-4), so it cannot fail (no resolve, no error
// return).
//
// Linear is issues-only and check-less (DL-051): its event alphabet is
// Issue/Comment, so no CHECKS/REVIEW arms ever fire. The checks roller is still
// wired (the router's ChecksRoller seam is non-optional) over the same
// forgeNotifyChecksRoller adapter the GitHub lane uses — the Linear client's
// ChecksConditional returns ErrUnsupported, but the router invokes RollUp only on
// a CHECKS event Linear never produces (correct and never-called).
func buildLinearNotifyLane(
	st *store.Store,
	hub *runnerhub.Hub,
	tokens *linearagent.TokenSource,
	log *slog.Logger,
) *forgeNotifyLane {
	if tokens == nil {
		return nil // Linear not configured (client-credentials pair undeclared): lane off.
	}
	const (
		provider = store.ForgeProviderLinear
		host     = "linear.app"
	)
	client := forge.NewLinear(forge.LinearConfig{Token: tokens, Log: log})

	notifyStore := &forgeNotifyStore{st: st, provider: provider, host: host}
	dispatcher := &forgeNotifyDispatcher{hub: hub}
	checks := &forgeNotifyChecksRoller{client: client}
	forgeRef := &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     host,
	}
	router := ingest.NewNotifyRouter(notifyStore, dispatcher, checks, forgeRef, log)
	arm := ingest.NewNotifyWebhookArm(router, ingest.NotifyArmConfig{Log: log})
	reconciler := ingest.NewNotifyReconciler(client, notifyStore, router,
		compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, host, ingest.ReconcileConfig{
			Backstop: 0, // no App config carries a Linear backstop; 0 -> ingest's defaultBackstop.
			Log:      log,
		})
	return &forgeNotifyLane{arm: arm, reconciler: reconciler, sink: arm, reader: client}
}

// newDeclaredSecretResolver returns a func that resolves the declared server_only
// secret NAME to its raw value bytes on each call — the lazy PEM/webhook-secret
// seam the App token source and the webhook ingress consume. A resolve fault or
// an absent name is an error the caller surfaces (a per-mint auth failure for the
// token source; a 503 for the ingress). It threads the caller's ctx into the
// resolve, never re-rooting.
func newDeclaredSecretResolver(resolver secrets.Resolver, name string) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		resolved, err := resolver.Resolve(ctx, "board webhook")
		if err != nil {
			return nil, fmt.Errorf("board webhook secret resolve: %w", err)
		}
		for _, s := range resolved {
			if s.Name == name {
				return []byte(s.Value), nil
			}
		}
		return nil, fmt.Errorf("board webhook secret %q not declared", name)
	}
}

// cachedWebhookSecret wraps newDeclaredSecretResolver in a TTL cache for the hot
// path: the webhook-secret resolver is invoked on EVERY request to the
// internet-facing, unauthenticated POST /webhooks/github, BEFORE the HMAC check
// (github_webhook.go resolves the secret, then verifies the signature). An
// uncached resolve there lets an attacker force one full secretspec provider
// Load (registry read + manifest temp-file write + provider Load) per cheap
// garbage POST — an asymmetric-cost amplification ahead of authentication. The
// cache (forgeTokenTTL) bounds the per-request cost to a memcmp while a rotated
// signing secret still takes effect within the TTL. A resolve fault is surfaced to the caller (a 503),
// never cached. Unlike the App-key resolver (also newDeclaredSecretResolver but
// cold — NewAppTokenSource caches the minted token and only reads the key on
// mint), this one is on the request hot path, so it needs the cache.
type cachedWebhookSecret struct {
	base func(ctx context.Context) ([]byte, error)
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	value   []byte
	expires time.Time
	valid   bool
}

// newCachedWebhookSecret returns the door-consumable resolver method value over
// a fresh cache with the default forgeTokenTTL and wall clock.
func newCachedWebhookSecret(resolver secrets.Resolver, name string) func(ctx context.Context) ([]byte, error) {
	c := &cachedWebhookSecret{base: newDeclaredSecretResolver(resolver, name), ttl: forgeTokenTTL, now: time.Now}
	return c.get
}

// get serves the cached secret while unexpired, else re-resolves through base. A
// resolve fault drops validity and is returned (never cached), so the ingress
// fails closed with a 503 and the next request retries.
func (c *cachedWebhookSecret) get(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.now().Before(c.expires) {
		return c.value, nil
	}
	resolved, err := c.base(ctx)
	if err != nil {
		c.valid = false
		return nil, err
	}
	c.value = resolved
	c.expires = c.now().Add(c.ttl)
	c.valid = true
	return c.value, nil
}

// validateForgeSecret resolves the declared secret set once and asserts the
// configured name is present. A resolve that ERRORS is transient
// ("forge secret resolve failed at startup: %w"); a name ABSENT from the
// resolved set is a permanent misconfiguration ("forge secret %q not declared").
// The two are distinguishable so a crash-loop is diagnosable.
func validateForgeSecret(ctx context.Context, resolver secrets.Resolver, reason, name string) error {
	resolved, err := resolver.Resolve(ctx, reason)
	if err != nil {
		return fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	for _, s := range resolved {
		if s.Name == name {
			return nil
		}
	}
	return fmt.Errorf("forge secret %q not declared", name)
}

// wireForgeWriteCaller builds the write chokepoint and mounts it on the hub via
// SetForgeCaller when the forge-WRITE path is enabled (BOTH the primary and
// reviewer Apps configured). A resolve FAULT (not an absent name) fails startup
// regardless of whether writes are on; when writes are off, the caller is left
// unwired and Hub.RelayForgeCall fail-closes to an in-band CodeUnavailable
// (relay_forge.go), the clean degrade — but a PARTIAL misconfig (exactly one App
// configured) logs a Warn first, since that is a likely operator typo rather than
// an intentional off. primaryClient is the shared primary App client the author
// write leg reuses (nil when the App is absent — which, since writes now require
// the primary App, only coincides with writes being off); linearTokens is the
// shared Linear token source the Linear coordinate rides (nil when Linear is not
// configured). On any startup fault it unwinds the caller-bound listeners
// (udsListener + listeners) before returning, the same teardown path the poll
// driver and Rehydrate faults use.
func wireForgeWriteCaller(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	resolver secrets.Resolver,
	hub *runnerhub.Hub,
	log *slog.Logger,
	primaryClient *forge.GitHub,
	linearTokens *linearagent.TokenSource,
	udsListener net.Listener,
	listeners boundListeners,
) error {
	declaredSecrets, err := resolver.Resolve(ctx, "forge write")
	if err != nil {
		return failStartup(udsListener, listeners, fmt.Errorf("forge secret resolve failed at startup: %w", err))
	}
	if !cfg.Forge.forgeWritesEnabled(declaredSecrets) {
		warnPartialForgeWriteSecrets(cfg.Forge, declaredSecrets, log)
		return nil
	}
	forgeSvc, err := buildForgeWriteService(ctx, cfg, st, issueBrd, resolver, primaryClient, linearTokens, log)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	hub.SetForgeCaller(forgeSvc)
	return nil
}

// buildForgeWriteService assembles the agent forge-WRITE chokepoint (the
// runnerhub.ForgeCaller) for the 2-App cutover: enabled iff BOTH the primary and
// reviewer Apps are configured. In order it: (1) validates the reviewer App key
// secret ONCE at startup (the primary App key was already validated when the
// board wiring built primaryClient) via validateForgeSecret so a misconfig fails
// fast; (2) builds the reviewer App token source + client; (3) builds the
// provider registry, registering the GitHub coordinate (author = the shared
// primary App client, reviewer = the reviewer App client, F1) and — when the
// shared Linear token source is present — a Linear coordinate; and (4) returns
// the forgeService the caller mounts with hub.SetForgeCaller.
//
// The author write leg REUSES primaryClient (NOT a fresh client over the same
// token source): the client-side rate-budget/resetAt gate is per-*forge.GitHub
// client (mu-guarded), so threading only the token source into a new client would
// give the author writes a SEPARATE budget gate from the board/notify reads.
// Reusing the one client keeps author writes and reads on ONE shared budget gate
// against the single installation (RIG-2991). primaryClient is non-nil here: the
// caller only reaches this when writes are enabled, which requires the primary
// App configured, which is exactly when buildBoardWebhookWiring built the client.
func buildForgeWriteService(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	resolver secrets.Resolver,
	primaryClient *forge.GitHub,
	linearTokens *linearagent.TokenSource,
	log *slog.Logger,
) (*forgeService, error) {
	fc := cfg.Forge.resolved()

	// (1) The author write leg rides the shared primary App client. A defensive
	// guard: the caller only reaches here when writes are enabled (primary App
	// configured), so primaryClient is built — but a nil here would otherwise
	// register a nil author client, so fail fast with a clear error rather than
	// panic on first write.
	if primaryClient == nil {
		return nil, errors.New("forge write: primary App client is nil (writes enabled without a configured primary App)")
	}

	// (2) The reviewer App client: validate its key secret (distinct fail-fast
	// text), then build its own installation-token source + client — a distinct
	// GitHub identity from the primary App so F1's author-cannot-approve holds.
	if err := validateForgeSecret(ctx, resolver, "forge reviewer app key", fc.ReviewerApp.AppPrivateKeySecret); err != nil {
		return nil, err
	}
	reviewerTok, err := forge.NewAppTokenSource(forge.GitHubAppConfig{
		AppID:          fc.ReviewerApp.AppID,
		InstallationID: fc.ReviewerApp.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, fc.ReviewerApp.AppPrivateKeySecret),
		Host:           fc.Host,
	})
	if err != nil {
		return nil, fmt.Errorf("forge reviewer app token source: %w", err)
	}
	reviewerClient := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: reviewerTok})

	// (3) The provider registry: the GitHub coordinate (author = shared primary
	// client, reviewer = reviewer App client, F1) plus a Linear coordinate when
	// the shared Linear token source is configured.
	registry := newForgeProviderRegistry()
	registerGitHubForgeCoordinate(registry, fc, primaryClient, reviewerClient)

	// Linear write coordinate — registered ONLY when Linear is configured (the
	// shared client-credentials token source is non-nil, else GitHub-only). Linear
	// is issues-only (DL-051): its PR/review ops return ErrUnsupported, which the
	// chokepoint flattens to in-band unimplemented. One client serves both roles —
	// Linear has no author/reviewer split (no review concept), so the same client
	// is the author and the reviewer entry. It rides the SAME linearTokens
	// instance the notify lane rides (the one-instance rule, DEC-4). Its
	// coordinate host is left empty so a Linear-provider ForgeRef with no host
	// resolves it via the registry's per-provider default; the GraphQL endpoint
	// default lives inside NewLinear. isDefault=false: the GitHub coordinate is the
	// default a nil/unset ForgeRef resolves to, so Linear is the additive
	// coordinate a LINEAR-addressed ForgeRef selects explicitly.
	if linearTokens != nil {
		linear := forge.NewLinear(forge.LinearConfig{Token: linearTokens, Log: log})
		registry.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}, linear, linear, false)
	}

	return newForgeService(st, issueBrd, registry), nil
}

// registerGitHubForgeCoordinate registers the production GitHub write coordinate
// — the AUTHOR client (the shared primary App client) and the REVIEWER client
// (the reviewer App client, F1) — as the default coordinate a nil/unset ForgeRef
// resolves to. The two roles are distinct GitHub App identities so an agent
// approving a PR it authored dispatches submit_review on a different account than
// it authored with, dissolving the author-approving-own-PR rejection at the
// credential layer.
func registerGitHubForgeCoordinate(reg *forgeProviderRegistry, fc ForgeConfig, author, reviewer *forge.GitHub) {
	reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: fc.Host}, author, reviewer, true)
}

// buildLinearTokenSource builds the ONE shared Linear OAuth client-credentials
// token source (actor=app) from the declared client-id/secret pair, or returns
// nil when Linear is not configured (neither name declared — the clean off-state
// for a deployment that runs no Linear lane). Exactly ONE of the two names
// declared is a likely operator typo: it Warns and treats Linear as off (mirrors
// warnPartialForgeWriteSecrets). When BOTH are declared it builds the source and
// runs a boot-time mint check — one Token(ctx) call — so a bad pair or a disabled
// client_credentials toggle fails Serve at startup (fail-fast like
// validateForgeSecret), not on the first write. The returned instance is passed
// to BOTH the notify lane and the write coordinate (the one-instance rule, DEC-4).
func buildLinearTokenSource(ctx context.Context, cfg ServeConfig, resolver secrets.Resolver, log *slog.Logger) (*linearagent.TokenSource, error) {
	fc := cfg.Forge.resolved()
	declared, err := resolver.Resolve(ctx, "forge linear")
	if err != nil {
		return nil, fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	var clientID, clientSecret string
	for _, s := range declared {
		switch s.Name {
		case fc.LinearClientIDSecretName:
			clientID = s.Value
		case fc.LinearClientSecretName:
			clientSecret = s.Value
		}
	}
	haveID, haveSecret := clientID != "", clientSecret != ""
	if !haveID && !haveSecret {
		return nil, nil //nolint:nilnil // Linear not configured is a valid off-state: a nil source is the signal (the callers guard `if tokens != nil`), not an ambiguous nil-nil.
	}
	if haveID != haveSecret {
		declaredName, missingName := fc.LinearClientIDSecretName, fc.LinearClientSecretName
		if haveSecret {
			declaredName, missingName = fc.LinearClientSecretName, fc.LinearClientIDSecretName
		}
		log.Warn("forge Linear lanes disabled: only one of the two Linear client-credential secrets is declared; both are required",
			"declared", declaredName, "missing", missingName)
		return nil, nil //nolint:nilnil // a partial Linear config is an operator typo, surfaced by the Warn; treated as off (a nil source), never a fatal.
	}
	// The 30s-bounded client matches NewGitHub/appTokenSource: it caps the
	// boot-time mint below (an unbounded doer would let a half-open TCP to
	// api.linear.app wedge Serve's whole boot, defeating this fail-fast check)
	// and the same instance the notify lane + write coordinate later reuse.
	tokens := linearagent.NewTokenSource(clientID, clientSecret, &http.Client{Timeout: 30 * time.Second}, "")
	// Boot-time mint check: a Token(ctx) call proves the pair mints (the DL-204
	// degrade probe guards only attribution, not the mint path), so a bad secret
	// or a disabled client_credentials toggle fails Serve here, not on first write.
	if _, err := tokens.Token(ctx); err != nil {
		return nil, fmt.Errorf("forge Linear boot-time mint check failed (verify the client-credentials pair and the app's client-credentials toggle): %w", err)
	}
	return tokens, nil
}

// forgeSecretDeclared reports whether name is present in the resolved
// declared-secret set — the additive Linear gate (register a Linear coordinate
// iff its secret is declared). A resolve fault fails fast the same way
// validateForgeSecret's does; unlike validateForgeSecret an absent name is NOT an
// error (Linear is the optional additive coordinate, not the required path).
func forgeSecretDeclared(ctx context.Context, resolver secrets.Resolver, name string) (bool, error) {
	resolved, err := resolver.Resolve(ctx, "forge write")
	if err != nil {
		return false, fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	for _, s := range resolved {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// reconcileForgeSeed inserts each seed repo as an enabled target, bootstrap-only
// (ON CONFLICT DO NOTHING via EnsureForgeRepoSubscription): the flag creates
// rows that do not yet exist and leaves an existing row entirely untouched (a
// dropped-from-flag repo is neither deleted nor disabled; a soft-disabled row
// stays disabled). Each repo is validated owner/name and lowercased for GITHUB
// before insert, so a Owner/Name and owner/name seed mint ONE PK row.
func reconcileForgeSeed(ctx context.Context, st *store.Store, provider store.ForgeProvider, host string, seed []string) error {
	for _, raw := range seed {
		repo, err := normalizeGitHubRepo(raw)
		if err != nil {
			return err
		}
		if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
			Provider: provider,
			Host:     host,
			Repo:     repo,
			Enabled:  true,
		}); err != nil {
			return fmt.Errorf("seeding forge repo subscription %q: %w", repo, err)
		}
	}
	return nil
}

// warnDisabledBoardIngestion emits exactly one slog.Warn when the board webhook
// lane is off (no GitHub App configured) but the table already holds enabled
// subscription rows for the bound (provider, host) — a deployment that landed a
// manual row (or a prior --forge-repos seed) without wiring the App. Never
// fail-fast: it keeps board ingestion hard-off (Constraint #3). No Warn when no
// enabled rows exist. The count covers ONLY the bound coordinate.
func warnDisabledBoardIngestion(ctx context.Context, st *store.Store, provider store.ForgeProvider, host string, log *slog.Logger) {
	enabled, err := st.ListEnabledForgeRepoSubscriptions(ctx, provider, host)
	if err != nil {
		log.Error("board ingestion: list enabled targets at boot", "err", err)
		return
	}
	if len(enabled) == 0 {
		return
	}
	log.Warn("board ingestion disabled (no GitHub App configured) but enabled targets exist; set --forge-app-id and the App secrets",
		"targets", len(enabled), "forge_host", host)
}

// fanoutSink fans one accepted webhook event to every registered sink; it
// satisfies server.ForgeEventSink (github_webhook.go:47). Board-first this is a
// single-element fan-out over the board arm; the notify arm composes in later
// via T7 through the one-line registration seam (OQ-7). Enqueue MUST NOT block:
// it just forwards to each sink's non-blocking Enqueue, adding no blocking work.
type fanoutSink struct{ sinks []ForgeEventSink }

// Enqueue forwards the event to every registered sink.
func (f *fanoutSink) Enqueue(ctx context.Context, ev forge.ForgeEvent) {
	for _, s := range f.sinks {
		s.Enqueue(ctx, ev)
	}
}

// warnPartialForgeWriteSecrets emits exactly one slog.Warn when the forge-WRITE
// path is disabled because exactly ONE of the two required Apps (the primary and
// the reviewer) is configured — a likely operator typo (a missing App id or an
// undeclared App key secret), which otherwise silently fails every agent forge
// write closed (CodeUnavailable) with nothing in the startup log to explain it.
// The intentional both-absent OFF state stays silent. Mirrors
// warnDisabledBoardIngestion: diagnostic only, never fail-fast, and logs a role
// name never a secret value.
func warnPartialForgeWriteSecrets(fc ForgeConfig, declared []secrets.ResolvedSecret, log *slog.Logger) {
	havePrimary, haveReviewer := fc.forgeWriteAppsConfigured(declared)
	if havePrimary == haveReviewer {
		return // both configured (enabled path, not here) or both absent (intentional off)
	}
	configured, missing := "primary App", "reviewer App"
	if haveReviewer {
		configured, missing = "reviewer App", "primary App"
	}
	log.Warn("forge write path disabled: only one of the two required GitHub Apps is configured; both are required",
		"configured", configured, "missing", missing)
}

// normalizeGitHubRepo validates an "owner/name" repo string and lowercases it
// (GitHub owner/name is case-insensitive-but-case-preserving, so Owner/Name and
// owner/name must not mint two PK rows — two poll targets). A string that is not
// exactly one non-empty owner and one non-empty name is a startup error.
func normalizeGitHubRepo(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	owner, name, ok := strings.Cut(trimmed, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid forge repo %q: want \"owner/name\"", raw)
	}
	return strings.ToLower(trimmed), nil
}

// failStartup tears down the UDS socket + eager-bound listeners on any post-bind
// startup fault, then returns err unchanged — the one cleanup path every Serve
// fail-fast (and wireForgeWriteCaller) shares, so a new fault site is one call,
// not a repeated three-line teardown.
func failStartup(udsListener net.Listener, listeners boundListeners, err error) error {
	udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
	listeners.close()
	return err
}

func closeListener(l net.Listener) {
	if l != nil {
		l.Close() //nolint:errcheck,gosec // best-effort listener close on teardown — nothing actionable remains (errcheck + its gosec G104 twin)
	}
}
