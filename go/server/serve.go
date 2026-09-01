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
	// SecretName is the declared server_only secret NAME holding the forge token
	// (default "GITHUB_FORGE_TOKEN"; the VALUE never crosses config or a flag).
	SecretName string
	// ReviewerSecretName is the declared server_only secret NAME holding the
	// REVIEWER forge token (default "GITHUB_FORGE_REVIEWER_TOKEN"; the VALUE
	// never crosses config or a flag). A distinct GitHub identity from the
	// author token so an agent approving a PR it authored is a different account
	// (F1). The agent forge-WRITE path is enabled iff BOTH this and SecretName
	// resolve to a declared secret (Matt's 2026-08-19 ruling).
	ReviewerSecretName string
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
	defaultForgeHost       = "github.com"
	defaultForgeSecretName = "GITHUB_FORGE_TOKEN" //nolint:gosec // G101: this is the default declared-secret NAME (an env-var identifier), not a credential value — the value is resolved from the secrets provider, never hardcoded
	// defaultForgeReviewerSecretName is the default declared-secret NAME holding
	// the REVIEWER forge token (F1) — a distinct identity from the author token.
	// A secret NAME, not a value (see defaultForgeSecretName's gosec note).
	defaultForgeReviewerSecretName = "GITHUB_FORGE_REVIEWER_TOKEN" //nolint:gosec // G101: the default declared-secret NAME (an env-var identifier), not a credential value — resolved from the secrets provider, never hardcoded
	// defaultForgeLinearSecretName is the declared-secret NAME holding the Linear
	// write token (DL-051/DL-052). A Linear write coordinate is registered ONLY
	// when this secret is declared; otherwise the write path is GitHub-only. A
	// secret NAME, not a value.
	defaultForgeLinearSecretName = "LINEAR_FORGE_TOKEN" //nolint:gosec // G101: the default declared-secret NAME (an env-var identifier), not a credential value — resolved from the secrets provider, never hardcoded
	// defaultReconcileBackstop is the board reconciler's default sweep cadence
	// (OQ-5): startup sweep + a 30-min ticker (a 304 page-1 GET per enabled repo
	// is ≈ free, notify_reader.go:12-13; a cold-start zero watermark walks once).
	defaultReconcileBackstop = 30 * time.Minute
	// forgeTokenTTL is the TTL the driver's TokenSource caches a resolved token
	// for: a resolve reads the whole declared-secret registry, writes a manifest
	// temp file, and drives a full secretspec provider Load (resolver.go:135-165),
	// so re-resolving every poll pass would tax the store and provider. The
	// cache drops its value on TTL expiry or on Invalidate() (the client calls
	// it on a 401/bad-creds-403), so a rotated token still takes effect within
	// the TTL or immediately on the next auth failure.
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

// resolved returns the config with its zero fields defaulted (Host, SecretName,
// ReviewerSecretName, App.ReconcileBackstop). SeedRepos and App ids are taken
// verbatim.
func (c ForgeConfig) resolved() ForgeConfig {
	if c.Host == "" {
		c.Host = defaultForgeHost
	}
	if c.SecretName == "" {
		c.SecretName = defaultForgeSecretName
	}
	if c.ReviewerSecretName == "" {
		c.ReviewerSecretName = defaultForgeReviewerSecretName
	}
	if c.App.ReconcileBackstop <= 0 {
		c.App.ReconcileBackstop = defaultReconcileBackstop
	}
	return c
}

// forgeWritesEnabled reports whether the agent forge-WRITE path is enabled: iff
// BOTH the author secret (SecretName) and the reviewer secret
// (ReviewerSecretName) resolve to a name present in declared (Matt's 2026-08-19
// ruling — independent of boardIngestionEnabled, both secrets required). It is a
// pure predicate over the resolved declared-secret set so the "enabled = both
// declared" rule is unit-testable without a running Serve; buildForgeWriteService
// re-validates each name through validateForgeSecret to fail fast with the two
// distinct texts. Called on the resolved() config so the defaulted names apply.
func (c ForgeConfig) forgeWritesEnabled(declared []secrets.ResolvedSecret) bool {
	haveAuthor, haveReviewer := c.forgeWriteSecretsDeclared(declared)
	return haveAuthor && haveReviewer
}

// forgeWriteSecretsDeclared reports which of the two required write secrets —
// the author (SecretName) and the reviewer (ReviewerSecretName) — are present
// in the resolved declared set. Both true is the writes-enabled state
// (forgeWritesEnabled); exactly one true is a partial misconfiguration
// warnPartialForgeWriteSecrets surfaces. Resolves the config internally so the
// defaulted names apply — the caller need not pre-resolve.
func (c ForgeConfig) forgeWriteSecretsDeclared(declared []secrets.ResolvedSecret) (haveAuthor, haveReviewer bool) {
	fc := c.resolved()
	for _, s := range declared {
		switch s.Name {
		case fc.SecretName:
			haveAuthor = true
		case fc.ReviewerSecretName:
			haveReviewer = true
		}
	}
	return haveAuthor, haveReviewer
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

	// The store of record (T1) backs the comms vertical and the token store, and
	// carries the RIG-1667 T4 object-store archive seam. Open it before serving so
	// a bad DSN, a failed migration, or a bad S3 config fails startup here, not
	// mid-request.
	st, err := openStore(ctx, cfg)
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return err
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
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return err
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
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return fmt.Errorf("rehydrating issue board: %w", err)
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

	// The board webhook-ingestion lane (RIG-2883) and its webhook ingress wiring,
	// built BEFORE the doors because the network door mounts the lane's webhook
	// ingress (sink + secret resolver, threaded into buildDoors). Fails fast HERE
	// on the same udsListener.Close()+listeners.close() cleanup path the Rehydrate
	// fault above uses. All returns are nil when the App is absent. The notify
	// lane (T7) rides the SAME ingress via webhookSink; Serve starts its arm +
	// reconciler alongside the board lane's below.
	lane, notifyLane, webhookSink, webhookSecret, err := buildBoardWebhookWiring(ctx, cfg, st, issueBrd, hub, resolver, hubLog)
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return err
	}

	// Assemble the three compass.v1 doors (shipped Unix socket, optional dev
	// loopback, optional authenticated network) plus the Linear notify lane the
	// net door's /webhooks/linear handler feeds. On a net-door build error the
	// listeners this Serve bound are still ours to close.
	doors, err := buildDoors(ctx, cfg, svc, commsSvc, secretsSvc, hub, st, admin.ID, resolver,
		devListener, netListener, netTLS, webhookSink, webhookSecret)
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return err
	}

	// The forge-WRITE caller is independent of the board ingest lane (Matt's
	// 2026-08-19 ruling): enabled iff BOTH write secrets are declared.
	if err := wireForgeWriteCaller(ctx, cfg, st, issueBrd, resolver, hub, hubLog, udsListener, listeners); err != nil {
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
	// share the App gate); the Linear notify lane is nil when LINEAR_FORGE_TOKEN
	// is undeclared (its independent gate). A nil lane starts nothing. Every Run
	// returns nil on ctx-cancel.
	startForgeIngestLanes(gctx, g, lane, notifyLane, doors.linearNotify)
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
	// beside the webhook handler it feeds; nil when LINEAR_FORGE_TOKEN is
	// undeclared. Serve starts its arm + reconciler on the serve group.
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
	// LINEAR_FORGE_TOKEN. Built here — beside the webhook handler it feeds — so a
	// resolve fault fail-fasts door assembly, and its data-change sink threads
	// straight into buildLinearWebhookWiring below, replacing the
	// injected-and-nil-for-now sink so a verified /webhooks/linear Issue/Comment
	// event routes to subscribers instead of ack-and-drop. Nil when the secret is
	// undeclared (the handler's data branch then acks-and-drops). The lane is
	// returned in serveDoors so Serve can start its arm + reconciler on the serve
	// group.
	linearNotifyLane, err := buildLinearNotifyLane(ctx, st, hub, resolver, slog.Default())
	if err != nil {
		return serveDoors{}, err
	}
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
	// coordinate; nil when the notify lane is off (LINEAR_FORGE_TOKEN undeclared),
	// and the handler's data branch then acks-and-drops. The two gates are
	// independent: the webhook secret gates the handler; LINEAR_FORGE_TOKEN gates
	// the sink. Its session arm is left unwired (nil sessionSink -> logged-drop)
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
) (*boardIngestLane, *forgeNotifyLane, ForgeEventSink, func(ctx context.Context) ([]byte, error), error) {
	rc := cfg.Forge.resolved()
	// App absent -> both forge lanes hard-off. Warn once here (the single
	// diagnostic site) when enabled subscription rows exist, and mount nothing.
	if !cfg.Forge.boardIngestionEnabled() {
		warnDisabledBoardIngestion(ctx, st, store.ForgeProviderGitHub, rc.Host, log)
		return nil, nil, nil, nil, nil
	}
	// Validate both App secrets ONCE at this shared site (distinct fail-fast
	// texts via validateForgeSecret) so a configured App with a missing secret
	// fails startup and BOTH lanes inherit a validated App — the notify lane no
	// longer relies on the board lane validating first.
	if err := validateForgeSecret(ctx, resolver, "board webhook app key", rc.App.AppPrivateKeySecret); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := validateForgeSecret(ctx, resolver, "board webhook secret", rc.App.AppWebhookSecretName); err != nil {
		return nil, nil, nil, nil, err
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
		return nil, nil, nil, nil, fmt.Errorf("board webhook app token source: %w", err)
	}
	client := forge.NewGitHub(forge.GitHubConfig{Host: rc.Host, Token: tok})

	lane, err := buildBoardIngestLane(ctx, cfg, st, issueBrd, client, log)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	notifyLane := buildForgeNotifyLane(cfg, st, hub, client, log)
	sink := &fanoutSink{sinks: []ForgeEventSink{lane.sink, notifyLane.sink}}
	secret := newCachedWebhookSecret(resolver, rc.App.AppWebhookSecretName)
	return lane, notifyLane, sink, secret, nil
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
// T7), the Linear sibling of buildForgeNotifyLane. It gates App-INDEPENDENTLY on
// the LINEAR_FORGE_TOKEN read credential (forgeSecretDeclared) — the same secret
// the write path's Linear coordinate gates on (serve.go:1513-1517) — matching the
// house pattern that every forge lane gates as a unit on its own credential. An
// undeclared/absent secret returns (nil, nil), the off-state the caller reads as
// "mount no Linear notify sink"; a resolve FAULT returns the error (fail-fast,
// like forgeSecretDeclared elsewhere).
//
// Linear is issues-only and check-less (DL-051): its event alphabet is
// Issue/Comment, so no CHECKS/REVIEW arms ever fire. The checks roller is still
// wired (the router's ChecksRoller seam is non-optional) over the same
// forgeNotifyChecksRoller adapter the GitHub lane uses — the Linear client's
// ChecksConditional returns ErrUnsupported, but the router invokes RollUp only on
// a CHECKS event Linear never produces (correct and never-called).
func buildLinearNotifyLane(
	ctx context.Context,
	st *store.Store,
	hub *runnerhub.Hub,
	resolver secrets.Resolver,
	log *slog.Logger,
) (*forgeNotifyLane, error) {
	declared, err := forgeSecretDeclared(ctx, resolver, defaultForgeLinearSecretName)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, nil //nolint:nilnil // an undeclared LINEAR_FORGE_TOKEN is a valid off-state: a nil lane is the signal (the caller guards `if lane != nil`), not an ambiguous nil-nil — a sentinel error would force the caller to distinguish it from a real fault.
	}
	const (
		provider = store.ForgeProviderLinear
		host     = "linear.app"
	)
	client := forge.NewLinear(forge.LinearConfig{Token: newForgeTokenSource(resolver, defaultForgeLinearSecretName), Log: log})

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
	return &forgeNotifyLane{arm: arm, reconciler: reconciler, sink: arm, reader: client}, nil
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
// cache (same forgeTokenTTL and rotation semantics as forgeTokenSource) bounds
// the per-request cost to a memcmp while a rotated signing secret still takes
// effect within the TTL. A resolve fault is surfaced to the caller (a 503),
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

// wireForgeWriteCaller resolves the declared secrets, and — when the forge-WRITE
// path is enabled (both write secrets declared) — builds the write chokepoint
// and mounts it on the hub via SetForgeCaller. A resolve FAULT (not an absent
// name) fails startup regardless of whether writes are on; when writes are off,
// the caller is left unwired and Hub.RelayForgeCall fail-closes to an in-band
// CodeUnavailable (relay_forge.go), the clean degrade — but a PARTIAL misconfig
// (only one of the two write secrets declared) logs a Warn first, since that is
// a likely operator typo rather than an intentional off. On any startup fault it
// unwinds the caller-bound listeners (udsListener + listeners) before returning,
// the same teardown path the poll driver and Rehydrate faults use.
func wireForgeWriteCaller(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	resolver secrets.Resolver,
	hub *runnerhub.Hub,
	log *slog.Logger,
	udsListener net.Listener,
	listeners boundListeners,
) error {
	declaredSecrets, err := resolver.Resolve(ctx, "forge write")
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	if !cfg.Forge.forgeWritesEnabled(declaredSecrets) {
		warnPartialForgeWriteSecrets(cfg.Forge, declaredSecrets, log)
		return nil
	}
	forgeSvc, err := buildForgeWriteService(ctx, cfg, st, issueBrd, resolver, log)
	if err != nil {
		udsListener.Close() //nolint:errcheck,gosec // teardown on an already-failing startup path — nothing actionable remains (errcheck + its gosec G104 twin)
		listeners.close()
		return err
	}
	hub.SetForgeCaller(forgeSvc)
	return nil
}

// buildForgeWriteService assembles the agent forge-WRITE chokepoint (the
// runnerhub.ForgeCaller) per Matt's 2026-08-19 ruling: independent of the board
// ingest lane, enabled iff BOTH forge write secrets are declared. In order it: (1)
// validates BOTH the author and reviewer secrets ONCE at startup so a
// misconfiguration fails fast with the two distinct texts (undeclared name vs a
// resolve that errors, via validateForgeSecret — the same fail-fast the board
// lane's App-secret validation uses), (2) builds the provider registry, registering the GitHub
// coordinate (author+reviewer, F1) and — when its secret is declared — a Linear
// coordinate, and (3) returns the forgeService the caller mounts with
// hub.SetForgeCaller. The caller gates the whole call on forgeWritesEnabled, so
// this only runs when both write secrets are present; the validateForgeSecret
// calls are the fail-fast on a resolve fault.
//
// The registry always holds the GitHub coordinate (its host defaults to
// github.com, so registerGitHubForgeCoordinate registers it whenever writes are
// enabled) plus a Linear coordinate when LINEAR_FORGE_TOKEN is declared, so the
// returned service is always non-nil on success.
func buildForgeWriteService(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	resolver secrets.Resolver,
	log *slog.Logger,
) (*forgeService, error) {
	fc := cfg.Forge.resolved()

	// (1) Startup secret resolve for BOTH roles: fail fast with the distinct
	// texts so a permanent misconfig (an undeclared name) is not confused with a
	// transient outage (a resolve that errors). The TokenSources re-resolve later
	// on TTL/Invalidate.
	if err := validateForgeSecret(ctx, resolver, "forge write", fc.SecretName); err != nil {
		return nil, err
	}
	if err := validateForgeSecret(ctx, resolver, "forge write", fc.ReviewerSecretName); err != nil {
		return nil, err
	}

	// (2) The provider registry: the GitHub coordinate (author+reviewer, F1)
	// plus a Linear coordinate when its secret is declared.
	registry := newForgeProviderRegistry()
	registerGitHubForgeCoordinate(registry, fc, resolver)

	// Linear write coordinate — registered ONLY when LINEAR_FORGE_TOKEN is
	// declared (else GitHub-only). Linear is issues-only (DL-051): its PR/review
	// ops return ErrUnsupported, which the chokepoint flattens to in-band
	// unimplemented. One client serves both roles — Linear has no author/reviewer
	// split (no review concept), so the same client is the author and the reviewer
	// entry. Its coordinate host is left empty so a Linear-provider ForgeRef with
	// no host resolves it via the registry's per-provider default; the GraphQL
	// endpoint default lives inside NewLinear. isDefault=false: the GitHub
	// coordinate is the default a nil/unset ForgeRef resolves to, so Linear is the
	// additive coordinate a LINEAR-addressed ForgeRef selects explicitly.
	if linearDeclared, err := forgeSecretDeclared(ctx, resolver, defaultForgeLinearSecretName); err != nil {
		return nil, err
	} else if linearDeclared {
		linear := forge.NewLinear(forge.LinearConfig{Token: newForgeTokenSource(resolver, defaultForgeLinearSecretName), Log: log})
		registry.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}, linear, linear, false)
	}

	return newForgeService(st, issueBrd, registry), nil
}

// registerGitHubForgeCoordinate registers the production GitHub write coordinate
// — the AUTHOR client (over fc.SecretName) and the REVIEWER client (over
// fc.ReviewerSecretName, F1), each on its own TTL-caching TokenSource — as the
// default coordinate a nil/unset ForgeRef resolves to. The two roles are
// distinct GitHub identities so an agent approving a PR it authored dispatches
// submit_review on a different account than it authored with, dissolving the
// author-approving-own-PR rejection at the credential layer.
func registerGitHubForgeCoordinate(reg *forgeProviderRegistry, fc ForgeConfig, resolver secrets.Resolver) {
	author := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: newForgeTokenSource(resolver, fc.SecretName)})
	reviewer := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: newForgeTokenSource(resolver, fc.ReviewerSecretName)})
	reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: fc.Host}, author, reviewer, true)
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
// path is disabled because only ONE of the two required write secrets is
// declared — a likely operator typo in one of the two env-var NAMES, which
// otherwise silently fails every agent forge write closed (CodeUnavailable)
// with nothing in the startup log to explain it. The intentional both-absent
// OFF state stays silent. Mirrors warnDisabledBoardIngestion: diagnostic only,
// never fail-fast, and logs secret NAMES (env-var identifiers) never values.
func warnPartialForgeWriteSecrets(fc ForgeConfig, declared []secrets.ResolvedSecret, log *slog.Logger) {
	haveAuthor, haveReviewer := fc.forgeWriteSecretsDeclared(declared)
	if haveAuthor == haveReviewer {
		return // both present (enabled path, not here) or both absent (intentional off)
	}
	rc := fc.resolved()
	declaredName, missingName := rc.SecretName, rc.ReviewerSecretName
	if haveReviewer {
		declaredName, missingName = rc.ReviewerSecretName, rc.SecretName
	}
	log.Warn("forge write path disabled: only one of the two required forge write secrets is declared; both are required",
		"declared", declaredName, "missing", missingName)
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

// forgeTokenSource is the driver's forge.TokenSource: a TTL cache over the one
// SpecResolver, selecting the configured secret name from the resolved set. A
// resolve is not cheap (reads the whole registry, writes a manifest, drives a
// provider Load — resolver.go:135-165), so the value is cached for forgeTokenTTL
// and re-resolved only on TTL expiry or Invalidate() (the client calls the
// latter on a 401/bad-creds-403). This is the design's stated reason for a
// TTL-cache with an invalidation seam rather than a captured token or bare func.
type forgeTokenSource struct {
	resolver secrets.Resolver
	name     string
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
	valid   bool
}

// newForgeTokenSource returns a TokenSource resolving name through resolver,
// caching each resolved value for forgeTokenTTL.
func newForgeTokenSource(resolver secrets.Resolver, name string) *forgeTokenSource {
	return &forgeTokenSource{resolver: resolver, name: name, ttl: forgeTokenTTL, now: time.Now}
}

// Token returns the cached token while it is valid and unexpired, else
// re-resolves the declared set and selects the configured name. A missing name
// at Token time (declaration deleted post-boot) is an error the driver surfaces
// per-pass as an auth failure + retry next tick (idempotent).
//
// Single-caller by the driver's contract: the poll driver calls Token
// sequentially per fetch batch on one goroutine, and Invalidate runs on that
// same goroutine (never re-entrantly from inside Token), so holding t.mu across
// the resolve I/O never contends. Were a second concurrent caller ever added,
// the lock would simply serialize resolves (singleflight-like) — benign, but the
// single-caller assumption is the reason the resolve is inside the lock.
func (t *forgeTokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.valid && t.now().Before(t.expires) {
		return t.token, nil
	}
	resolved, err := t.resolver.Resolve(ctx, "forge poll")
	if err != nil {
		return "", fmt.Errorf("forge token resolve: %w", err)
	}
	for _, s := range resolved {
		if s.Name == t.name {
			t.token = s.Value
			t.expires = t.now().Add(t.ttl)
			t.valid = true
			return t.token, nil
		}
	}
	t.valid = false
	return "", fmt.Errorf("forge secret %q not declared", t.name)
}

// Invalidate drops the cached value so the next Token re-resolves — the client
// calls it when it observes an auth failure, so a rotated token takes effect
// immediately rather than after the TTL.
func (t *forgeTokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.valid = false
}

func closeListener(l net.Listener) {
	if l != nil {
		l.Close() //nolint:errcheck,gosec // best-effort listener close on teardown — nothing actionable remains (errcheck + its gosec G104 twin)
	}
}
