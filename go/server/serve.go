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
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
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
	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/fabric"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/linearagent"
	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
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
	// Rev is the git commit the binary was built from, reported by GetServerInfo.
	Rev string
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
	// NatsURL is the NATS connection string for the event fabric. Required:
	// comms publishes and delivery subscribes through the fabric, so Serve
	// refuses to boot when it cannot connect.
	NatsURL string
	// S3 is the object-store archive tier config (RIG-1667 T4). Optional: when
	// unset (no endpoint/bucket) the server boots without an archive tier and the
	// store's nil object-store guard fails a flush loudly only if one is ever
	// attempted. Mirrors the DATABASE_DSN flag/env precedence at the CLI.
	S3 S3Config
	// TranscriptSafetyValveCapBytes overrides the store hot-tail cap when positive.
	TranscriptSafetyValveCapBytes int
	// Listen, when set, is the TCP address the authenticated network door binds
	// (e.g. "0.0.0.0:8443"). Empty on the socket-only shipped path. When set, TLS
	// is required — a bearer token over cleartext is credential disclosure.
	Listen string
	// ListenListener is an already-bound network-door listener inherited from
	// the parent. When set, it replaces Listen as the bind source.
	ListenListener net.Listener
	// AdminHandle identifies the bootstrap admin used by the local door and network
	// AdminGate; empty defaults to "admin". Bootstrap finds or creates by handle and
	// rejects an existing non-admin account rather than elevating it. With the network
	// door enabled, its token is written 0600 under the state directory.
	AdminHandle string
	// AdminTokenFile names a private file containing the supplied bootstrap-admin token.
	AdminTokenFile string
	// StateDir is the directory the bootstrap-admin token file is written under
	// (0600). Defaults to the socket's parent directory when empty.
	StateDir string
	// CORSAllowedOrigin, when set, is the single browser origin the network door
	// exposes gRPC-Web CORS for. Empty = closed (no CORS on the network door).
	CORSAllowedOrigin string
	// PublicURL is the deployment's public base URL for Linear session links.
	// It has no default because the host is deployment-specific; the CLI supplies it
	// and webhook startup requires it when the responder is enabled.
	PublicURL string
	// Forge configures optional board webhook ingestion and forge writes. Both stay
	// off unless their App configuration and write secrets are supplied.
	Forge ForgeConfig
	// VCSMirrorDir enables server-side Git push support when non-empty.
	VCSMirrorDir string
	// VCSMirrorRepoCapBytes limits a single bare repository mirror.
	VCSMirrorRepoCapBytes int64
	// VCSMirrorTotalCapBytes limits all bare repository mirrors together.
	VCSMirrorTotalCapBytes int64
	// Mirror is configured from the GitHub App when VCSMirrorDir is set. An injected
	// implementation is preserved while the mirror is disabled.
	Mirror forge.Mirror
	// OtelEndpoint is the OTLP collector endpoint (OTEL_EXPORTER_OTLP_ENDPOINT);
	// empty = tracing off. It gates OTel emission on both binaries: when unset,
	// the bootstrap installs no provider and the RPC interceptors are inert
	// no-ops (no active span, so no traceresponse header).
	OtelEndpoint string
	// SecretProvider selects the secretspec provider used by both resolvers.
	// Empty preserves the default provider chain. The CLI supplies the optional
	// flag/env value; this setting controls deployment secret custody, including the
	// master key.
	SecretProvider string
	// UsageEventRetention is how long raw token- and compute-usage events are kept.
	// 0 turns the daily prune off. The CLI defaults it to 90 days.
	UsageEventRetention time.Duration
	// OnBound, when set, is called once the dev and network-door listeners are
	// bound, with each bound address (nil for a door that is off). It lets a
	// caller bind port 0 and learn the real port with no release-and-rebind gap.
	OnBound func(dev, network net.Addr)
	// RunnerClustersPath is the Runner cluster file (--runner-clusters). When
	// set, the RunnerService door also accepts projected ServiceAccount tokens.
	RunnerClustersPath string
	MergeQueueInterval time.Duration
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
	// ForgeCAPath is an optional PEM CA bundle trusted by GitHub/App clients.
	// Empty preserves the default system roots and existing behavior.
	ForgeCAPath string
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
	// ReviewerApp is a distinct GitHub App used only for submit_review. Its identity
	// lets an agent review from an account other than the one that authored the PR.
	// It has no webhook or read lane; those use the primary App.
	ReviewerApp ForgeAppConfig
	// These name the server-only Linear OAuth client-credentials pair; the values
	// never enter config or flags. The write and notify lanes share one app-actor
	// token, so both names must resolve. Defaults are LINEAR_FORGE_CLIENT_ID and
	// LINEAR_FORGE_CLIENT_SECRET.
	LinearClientIDSecretName string
	LinearClientSecretName   string
	// LinearWebhookSecretName names the server-only signing secret for the shared
	// webhook ingress. It enables the Linear data-change arm independently of the
	// GitHub App gate; the secret value never enters config or a flag.
	LinearWebhookSecretName string
	// TrunkTokenSecretName is the declared server-only secret name for Trunk API access.
	TrunkTokenSecretName string
	// MergeQueues maps GitHub repositories to Trunk target branches.
	MergeQueues []MergeQueueConfig
	// TrunkHTTP overrides the Trunk API client in tests.
	TrunkHTTP *http.Client
	// ScopeEnforcementDisabled opts out of grant-gated forge writes. The zero
	// value enforces; Dogfood opts out to keep the single-trust-domain posture.
	ScopeEnforcementDisabled bool
	// ScopeGrants seed account_forge_scopes in the bootstrap tenant on every boot
	// with forge writes on. Insert only: a server-up aid, not a maintained config.
	ScopeGrants []store.ForgeScope
}

// MergeQueueConfig binds a repository to a provider and queue target branch.
type MergeQueueConfig struct {
	Repo     string
	Provider string
	Branch   string
}

// ForgeAppConfig is the GitHub App credential the board webhook-ingestion lane
// runs on (RIG-2883, specified at
// RigelBuild/docs:docs/designs/compass/server/compass-forge-agent-notification/design.md:1035-1048).
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

const defaultMergeQueueSweepInterval = 2 * time.Minute

type mergeQueueRunner struct {
	sweeper *ingest.MergeQueueSweeper
}

func (r mergeQueueRunner) start(g *errgroup.Group, ctx context.Context) {
	if r.sweeper != nil {
		g.Go(func() error { return r.sweeper.Run(ctx) })
	}
}

func prepareMergeQueueSweeper(ctx context.Context, cfg ServeConfig, st *store.Store, issueBrd *board.IssueProjection, resolver secrets.Resolver) (mergeQueueRunner, error) {
	fc := cfg.Forge.resolved()
	if err := reconcileMergeQueueConfigs(ctx, st, fc.Host, fc.MergeQueues); err != nil {
		return mergeQueueRunner{}, fmt.Errorf("configure forge merge queues: %w", err)
	}
	if len(fc.MergeQueues) == 0 {
		return mergeQueueRunner{}, nil
	}
	if fc.TrunkTokenSecretName == "" {
		return mergeQueueRunner{}, errors.New("--forge-trunk-token-secret is required when merge queues are configured")
	}
	queues := mergeQueueProviders(fc.Host, newTrunkTokenResolver(resolver, fc.TrunkTokenSecretName), fc.TrunkHTTP)
	cfgs := &mergeQueueConfigStore{st: st, host: fc.Host}
	storeAdapter := mergeQueuePullRequestStore{st: st, host: fc.Host}
	sink := mergeQueueProjection{brd: issueBrd}
	every := cfg.MergeQueueInterval
	if every <= 0 {
		every = defaultMergeQueueSweepInterval
	}
	return mergeQueueRunner{sweeper: ingest.NewMergeQueueSweeper(queues, cfgs, storeAdapter, sink, every)}, nil
}

// Forge config defaults, applied by resolveForge when a field is zero.
const (
	defaultForgeHost = "github.com"
	// These are the default declared-secret names for the Linear OAuth pair.
	// Both must resolve for the write and notify lanes; otherwise Linear stays off.
	// They are names, not credential values.
	defaultForgeLinearClientIDSecretName = "LINEAR_FORGE_CLIENT_ID"     //nolint:gosec // G101: the default declared-secret NAME (an env-var identifier), not a credential value — resolved from the secrets provider, never hardcoded
	defaultForgeLinearClientSecretName   = "LINEAR_FORGE_CLIENT_SECRET" //nolint:gosec // G101: the default declared-secret NAME (an env-var identifier), not a credential value — resolved from the secrets provider, never hardcoded
	// defaultReconcileBackstop is the board reconciler's default sweep cadence
	// (OQ-5): startup sweep + a 30-min ticker (a 304 page-1 GET per enabled repo
	// is ≈ free, notify_reader.go:12-13; a cold-start zero watermark walks once).
	defaultReconcileBackstop = 30 * time.Minute
	// forgeTokenTTL bounds the cached webhook-secret lifetime. Each unauthenticated
	// request resolves the secret before HMAC validation; caching prevents garbage
	// POSTs from triggering a full secretspec export. Rotation takes effect within
	// the TTL.
	forgeTokenTTL = 5 * time.Minute
)

// boardIngestionEnabled is the cheap AppID gate. buildBoardWebhookWiring also
// validates both App secrets and fails fast if either is missing.
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

// forgeWritesEnabled requires both Apps' IDs and declared private-key names; the
// primary App also enables board ingestion. The predicate is pure over resolved
// declarations, and buildForgeWriteService revalidates both keys at startup.
func (c ForgeConfig) forgeWritesEnabled(declared []secrets.ResolvedSecret) bool {
	havePrimary, haveReviewer := c.forgeWriteAppsConfigured(declared)
	return havePrimary && haveReviewer
}

// forgeHTTPClient preserves the shared default transport when no CA is set.
// With a CA bundle, it clones the transport and adds the PEM to system roots;
// unrelated clients do not use this client.
func forgeHTTPClient(path string) (*http.Client, error) {
	if path == "" {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	transport = transport.Clone()
	pem, err := os.ReadFile(path) //nolint:gosec // operator-configured forge CA bundle
	if err != nil {
		return nil, fmt.Errorf("read forge CA bundle: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system cert pool: %w", err)
	}
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("forge CA bundle contains no certificates")
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.RootCAs = roots
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

// forgeWriteAppsConfigured reports whether each required App has a nonzero ID
// and a declared private-key name. Both configured enables writes; exactly one
// configured triggers the partial-configuration warning. It resolves defaults
// internally so callers need not pre-resolve.
func (c ForgeConfig) forgeWriteAppsConfigured(declared []secrets.ResolvedSecret) (havePrimary, haveReviewer bool) {
	fc := c.resolved()
	// Compare SERVER-prefixed names. Without the wrapper both Apps appear absent,
	// silently disabling writes because the partial-config warning treats that as
	// the intentional off-state.
	havePrimary = fc.App.AppID != 0 && secretDeclared(declared, serverSecretName(fc.App.AppPrivateKeySecret))
	haveReviewer = fc.ReviewerApp.AppID != 0 && secretDeclared(declared, serverSecretName(fc.ReviewerApp.AppPrivateKeySecret))
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

// serverSecretName maps forge secret names to the SERVER resolver's keyspace.
// Wrap names compared with ResolvedSecret.Name; keep operator-facing logs and
// errors unprefixed. A mismatch silently disables writes or tells operators to
// configure a name they did not set.
func serverSecretName(name string) string {
	if name == "" {
		// An unset name is "not configured", not a secret called "SERVER_".
		return ""
	}
	return store.ServerSecretPrefix + name
}

// masterKeyProvisioningHint is the operator runbook a failed master-key resolve
// names verbatim, so a fail-closed boot IS the instruction. It must stay in step
// with the self-host docs.
const masterKeyProvisioningHint = "generate a 256-bit key with `openssl rand -hex 32` and set it as the reserved server secret " + store.MasterKeyName + " in the configured secretspec provider before boot"

// masterKeyHexLen is the hex-encoded length of a 32-byte AES-256 key, the only
// length resolveMasterKey accepts before decode.
const masterKeyHexLen = 64

// masterKeySaltLen is the per-deployment random salt length for the key
// fingerprint: 256 bits, matching the SHA-256 digest width.
const masterKeySaltLen = 32

// initialKeyVersion is the generation a first-boot key is stamped with; it
// matches the server_key_state.key_version column default. Rotation (a later
// record) is what advances it.
const initialKeyVersion int16 = 1

// resolveMasterKey resolves and decodes the operator-provisioned key, then checks
// it against the server_key_state tripwire. It never generates a key or includes
// any key value in an error.
func resolveMasterKey(ctx context.Context, st *store.Store, server secrets.Resolver) (envelope.Key, int16, error) {
	resolved, err := server.Resolve(ctx, "master key resolve")
	if err != nil {
		return envelope.Key{}, 0, fmt.Errorf("resolve master key: %w", err)
	}
	var value string
	for _, s := range resolved {
		if s.Name == store.MasterKeyName {
			value = s.Value
			break
		}
	}
	if value == "" {
		// Fail closed: an unprovisioned or empty key is the intended first-run
		// experience, so the message is the runbook.
		return envelope.Key{}, 0, fmt.Errorf("master key %s is not provisioned: %s", store.MasterKeyName, masterKeyProvisioningHint)
	}

	raw, err := decodeMasterKey(value)
	if err != nil {
		return envelope.Key{}, 0, err
	}
	key, err := envelope.NewKey(raw)
	if err != nil {
		return envelope.Key{}, 0, fmt.Errorf("build master key: %w", err)
	}

	return reconcileKeyState(ctx, st, key)
}

// decodeMasterKey decodes the 64-hex-char master-key value to 32 raw bytes. Both
// failure arms name only the requirement — never the value or any byte of it.
func decodeMasterKey(value string) ([]byte, error) {
	if len(value) != masterKeyHexLen {
		return nil, fmt.Errorf("master key %s must be %d hex characters (a 32-byte key)", store.MasterKeyName, masterKeyHexLen)
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("master key %s must be valid hex (a 32-byte key)", store.MasterKeyName)
	}
	return raw, nil
}

// reconcileKeyState writes a fingerprint row on first boot. Later boots compare
// the key fingerprint in constant time and fail closed if the key differs from
// the one used to seal stored data.
func reconcileKeyState(ctx context.Context, st *store.Store, key envelope.Key) (envelope.Key, int16, error) {
	state, err := st.ServerKeyState(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		salt := make([]byte, masterKeySaltLen)
		if _, rerr := rand.Read(salt); rerr != nil {
			return envelope.Key{}, 0, fmt.Errorf("master key salt: %w", rerr)
		}
		row := store.ServerKeyState{
			KeyVersion:      initialKeyVersion,
			KeyFingerprint:  key.Fingerprint(salt),
			FingerprintSalt: salt,
		}
		if ierr := st.InsertServerKeyState(ctx, row); ierr != nil {
			// A racing booter won the insert: re-read and verify against the
			// winning row rather than trusting our own un-inserted salt.
			if errors.Is(ierr, store.ErrConflict) {
				return reconcileKeyState(ctx, st, key)
			}
			return envelope.Key{}, 0, fmt.Errorf("write master key state: %w", ierr)
		}
		return key, initialKeyVersion, nil
	case err != nil:
		return envelope.Key{}, 0, err
	}

	if subtle.ConstantTimeCompare(key.Fingerprint(state.FingerprintSalt), state.KeyFingerprint) != 1 {
		return envelope.Key{}, 0, errors.New(
			"master key " + store.MasterKeyName + " does not match the key the stored secrets were encrypted with; " +
				"proceeding would make every existing secret undecryptable, so boot fails closed")
	}
	return key, state.KeyVersion, nil
}

// buildServerSecretResolver creates the read-only SpecResolver over
// server_secrets. The user/container path uses a separate DB-backed resolver, so
// deployment secrets cannot be delivered into agent containers.
func buildServerSecretResolver(st *store.Store, cfg ServeConfig) secrets.Resolver {
	opts := []secrets.SpecOption{}
	if cfg.SecretProvider != "" {
		opts = append(opts, secrets.WithProvider(cfg.SecretProvider))
	}
	return secrets.NewSpecResolver(
		store.ServerDeclaredSecrets{Store: st},
		filepath.Join(secretsStateDir(cfg), "server"),
		opts...,
	)
}

// buildUserSecretResolver declares server-secret names, resolves the master key,
// and builds the DB-backed user resolver. A missing or invalid key fails startup,
// and boot never generates a replacement; nothing decrypts until this succeeds.
func buildUserSecretResolver(
	ctx context.Context, st *store.Store, cfg ServeConfig, serverResolver secrets.Resolver,
) (*secrets.StoreResolver, envelope.Key, int16, error) {
	if err := declareServerSecretNames(ctx, st, cfg); err != nil {
		return nil, envelope.Key{}, 0, err
	}
	if err := st.DeclareServerSecret(ctx, "", store.MasterKeyName); err != nil && !errors.Is(err, store.ErrConflict) {
		return nil, envelope.Key{}, 0, fmt.Errorf("declaring master key name: %w", err)
	}
	masterKey, keyVersion, err := resolveMasterKey(ctx, st, serverResolver)
	if err != nil {
		return nil, envelope.Key{}, 0, err
	}
	return secrets.NewStoreResolver(st, masterKey, keyVersion), masterKey, keyVersion, nil
}

// declareServerSecretNames registers configured forge secret names in
// server_secrets under the SERVER prefix. It tolerates existing-name conflicts
// and follows each lane's enable predicate. It declares names only; operators
// provision values directly in the provider before boot.
func declareServerSecretNames(ctx context.Context, st *store.Store, cfg ServeConfig) error {
	fc := cfg.Forge.resolved()
	var names []string
	if fc.boardIngestionEnabled() {
		names = append(names, fc.App.AppPrivateKeySecret, fc.App.AppWebhookSecretName)
	}
	// Declare the reviewer key independently: its AppID gate is separate from the
	// primary App's board-ingestion gate. Nesting it would miss reviewer-only setups
	// and silence the partial-write warning.
	if fc.ReviewerApp.AppID != 0 {
		names = append(names, fc.ReviewerApp.AppPrivateKeySecret)
	}
	// Gate Linear on raw config: resolved() supplies defaults, which would declare
	// both secrets for every deployment and force a provider resolve. A half-set
	// pair declares neither, matching buildLinearTokenSource's off-state.
	raw := cfg.Forge
	if raw.LinearClientIDSecretName != "" && raw.LinearClientSecretName != "" {
		names = append(names, raw.LinearClientIDSecretName, raw.LinearClientSecretName)
	}
	if raw.LinearWebhookSecretName != "" {
		names = append(names, raw.LinearWebhookSecretName)
	}
	if fc.TrunkTokenSecretName != "" {
		names = append(names, fc.TrunkTokenSecretName)
	}
	for _, name := range names {
		if name == "" {
			// An unset name is "not configured", never a bare prefix.
			continue
		}
		err := st.DeclareServerSecret(ctx, "", serverSecretName(name))
		if err == nil || errors.Is(err, store.ErrConflict) {
			continue
		}
		return fmt.Errorf("declaring server secret name %q: %w", name, err)
	}
	return nil
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

// secretsStateDir returns the server-owned secrets directory under cfg.StateDir
// (or the socket's parent, then "."). NewSpecResolver creates it with mode 0700.
func secretsStateDir(cfg ServeConfig) string {
	base := cfg.StateDir
	if base == "" {
		if base = parentDir(cfg.SocketPath); base == "" {
			base = "."
		}
	}
	return filepath.Join(base, "secrets")
}

// openStore opens the store and configures the optional S3 archive. Without S3 the
// server still starts; the store's nil guard fails a flush loudly only if one is
// attempted. An invalid S3 config fails startup.
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
	if cfg.TranscriptSafetyValveCapBytes > 0 {
		st.SetSafetyValveCapBytes(cfg.TranscriptSafetyValveCapBytes)
	}
	return st, nil
}

// seedBootstrapAccounts finds or creates the bootstrap admin, @compass system
// sender, and @linear bridge. Reserved handles with the wrong account shape fail
// startup rather than being adopted or elevated. The admin handle is configurable;
// the returned bridge serves the Linear responder and routing seed.
func seedBootstrapAccounts(ctx context.Context, st *store.Store, cfg ServeConfig) (admin, system, linearBridge store.Account, err error) {
	admin, err = st.BootstrapAdmin(ctx, store.NewUser{Handle: cfg.resolvedAdminHandle(), DisplayName: bootstrapAdminDisplayName})
	if err != nil {
		return store.Account{}, store.Account{}, store.Account{}, fmt.Errorf("bootstrapping admin account: %w", err)
	}
	system, err = st.EnsureSystemAccount(ctx)
	if err != nil {
		return store.Account{}, store.Account{}, store.Account{}, fmt.Errorf("seeding system account: %w", err)
	}
	slog.Default().InfoContext(ctx, "system account seeded", "account_id", system.ID, "handle", system.Handle)
	linearBridge, err = st.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		return store.Account{}, store.Account{}, store.Account{}, fmt.Errorf("seeding linear bridge account: %w", err)
	}
	slog.Default().InfoContext(ctx, "linear bridge account seeded", "account_id", linearBridge.ID, "handle", linearBridge.Handle)
	return admin, system, linearBridge, nil
}

// setCommsForges gives comms the forge coordinates for ask refs and board reads.
func setCommsForges(c *comms.Comms, cfg ForgeConfig) {
	c.SetForgeHost(cfg.resolved().Host)
	c.SetBoardForges(board.Forges{GitHubHost: cfg.resolved().Host, LinearHost: forge.LinearHost})
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
// It connects the event fabric at cfg.NatsURL before opening the store and
// fails startup when it cannot, so a Server never runs without delivery.
//
// On shutdown it closes the bus (waking every open SubscribeEvents stream so
// graceful drain completes), then removes the socket file iff it is still the
// one it bound (identity-checked, so a racing successor server's socket is intact).
func Serve(ctx context.Context, cfg ServeConfig) error {
	// Eager-bind the optional dev and network-door TCP listeners before any
	// on-disk state, so a bad address, an in-use port, or a bad TLS keypair
	// fails Serve here — leaving no socket, directory, or admin-token file
	// behind — rather than dying unobserved in a detached serving goroutine.
	listeners, err := bindListeners(ctx, cfg)
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
	if err := clearStaleSocket(ctx, cfg.SocketPath); err != nil {
		listeners.close()
		return err
	}

	// Bind the socket under a restrictive umask so it is born owner-only (0600):
	// net.Listen creates it with 0666 & ^umask, so a permissive umask under a
	// traversable parent would expose a connectable socket until the chmod below.
	// Startup is single-goroutine here, so the process-global umask is safe to set.
	udsListener, err := listenUnixPrivate(ctx, cfg.SocketPath)
	if err != nil {
		listeners.close()
		return fmt.Errorf("binding socket at %s: %w", cfg.SocketPath, err)
	}

	// Owner-only: the socket is the server's whole trust boundary on the local
	// machine, so no other user may connect.
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		return failStartup(udsListener, listeners, fmt.Errorf("chmod 0600 %s: %w", cfg.SocketPath, err))
	}

	// Pin the socket identity so shutdown cleanup can tell our socket apart from
	// a successor server that rebound the same path.
	boundID, boundOK := socketIdentity(cfg.SocketPath)
	defer cleanupSocket(cfg.SocketPath, boundID, boundOK)

	// The one event bus every sequenced stream rides. Publish the initial Ready
	// status so a snapshot subscriber sees liveness immediately.
	bus := events.NewBus[busPayload]()
	defer bus.Close()
	publishReady(ctx, bus)

	// The event fabric carries comms publishes and delivery's trigger; the store
	// of record backs comms, the token store, and the transcript archive seam.
	// Both open before serving, so an unreachable broker, a bad DSN, a failed
	// migration, or a bad S3 config fails startup here, not mid-request.
	fab, st, err := openFabricAndStore(ctx, cfg)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	defer st.Close()
	// Deferred after st.Close, so it runs first: the drain finishes in-flight
	// delivery callbacks, which read the store, while the store is still open.
	defer closeFabric(ctx, fab)

	// Seed the platform accounts before serving: bootstrap admin, reserved system
	// sender @compass, reserved Linear bridge sender @linear. Created
	// unconditionally so AdminGate always has an admin id and the Linear responder
	// finds its bridge author. Idempotent find-or-create; wrong-shape rows fail startup.
	admin, systemAccount, linearBridge, err := seedBootstrapAccounts(ctx, st, cfg)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}

	// Log the resolved public base URL: there is no default, so a deploy that
	// forgets --public-url/$COMPASS_PUBLIC_URL logs an empty base (and the
	// responder-assembly guard rejects it when Linear webhooks are enabled).
	slog.Default().InfoContext(ctx, "public base URL resolved", "public_url", cfg.PublicURL)

	// The RunnerHub is the Server side of the Server<->Runner seam, routing
	// container-lifecycle RPCs to the owning Runner. The Bridge board is its
	// lifecycle sink, shared by the hub (writer) and the service (reader). The
	// RunnerService door mounts only on the network door (Runners are remote).
	brd := board.NewProjection(bus)

	// Server-authoritative issue board projection: ListBoardIssues re-snapshots
	// and the issue=16 live fan-out share this instance (part 4). Rehydrate seeds
	// it from the store before serving; nothing is subscribed yet at boot.
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
	commsSvc := comms.NewComms(ctx, st, commsBus, fab, admin.ID)
	setCommsForges(commsSvc, cfg.Forge)
	// Register the coordination-channel reconcile as the store's in-tx hook, so
	// the two parent-edge writers auto-provision/reconcile a manager's
	// coordination channel atomically with the tree edge (RIG-1722 T5). Wired here
	// before serving; the store invokes it on its own tx.
	commsSvc.RegisterCoordinationHook(st)

	serverResolver := buildServerSecretResolver(st, cfg)
	resolver, masterKey, keyVersion, err := buildUserSecretResolver(ctx, st, cfg, serverResolver)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	if err := configureVCSMirror(ctx, &cfg, serverResolver, udsListener, listeners); err != nil {
		return err
	}
	// One sessionTail instance is the hub's session-tail sink (writer) and the
	// service's SubscribeAgentSession source (reader) — a frame the hub relays
	// fans to that session's stream subscribers.
	tail := newSessionTail()
	// One logger for the hub's per-frame diagnostics and for the frame-loss
	// summary the drain logs, so both land on the same sink.
	hubLog := slog.Default()
	hub := newRunnerHub(st, brd, tail, commsSvc, hubLog)
	svc := newService(cfg.Version, bus, st, hub, brd, issueBrd, tail).withRev(cfg.Rev).withAccountRenamer(commsSvc, admin.ID)
	// Break the hub<->lifecycle (RIG-1618 T5), hub<->board (agent primary
	// lifecycle T3-a, RelayBoardCall), and comms<->hub ask-answer wake (RIG-1577)
	// construction cycles; see wireHubServiceCycles in sinks.go.
	wireHubServiceCycles(hub, commsSvc, st, issueBrd)
	// SecretsService mounts on each account door behind the same auth chain as CompassService.
	// Its handler enforces user-only writes and user-or-agent listing; the hub signals
	// secret-version changes through the shared resolver.
	secretsSvc := newSecretsService(st, resolver, serverResolver, hub)

	// The forge read-side credentials, built BEFORE the doors because the network
	// door mounts the board lane's webhook ingress and the Linear notify lane,
	// both threaded into buildDoors. Board fields are nil when the GitHub App is
	// absent; forge.linearTokens is nil when Linear is not configured.
	forgeWiring, err := buildForgeReadWiring(ctx, cfg, st, issueBrd, hub, serverResolver, hubLog)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	// Seed the root Manager "supervisor" on first launch (RIG-1820). The seed
	// needs a Runner command stream, not up at boot, so it hangs off the hub's
	// runner-ready hook; a reconnect re-fire is safe. Set after the forge wiring,
	// which decides whether the Linear routing channel is seeded.
	cancelSeed := wireSeed(ctx, hub, seedDeps{st: st, svc: svc, cm: commsSvc, log: slog.Default(), adminID: admin.ID,
		compassID: systemAccount.ID, linearBridgeID: linearRoutingBridge(forgeWiring.linearTokens, linearBridge.ID)})
	defer cancelSeed()

	// Assemble the Compass doors, Linear notify lane, and session responder.
	// On assembly failure, Serve still owns and closes its bound listeners.
	doors, err := buildDoors(ctx, cfg, svc, commsSvc, secretsSvc, hub, st, admin.ID, linearBridge.ID, resolver, serverResolver,
		devListener, netListener, netTLS, forgeWiring.webhookSink, forgeWiring.webhookSecret, forgeWiring.linearTokens, masterKey, keyVersion)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	mergeQueueRunner, err := prepareMergeQueueSweeper(ctx, cfg, st, issueBrd, serverResolver)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}

	// The forge-WRITE caller: enabled iff BOTH primary and reviewer Apps are
	// configured (the 2-App cutover). The author leg REUSES the shared primary App
	// client so it rides the one budget gate; the reviewer leg gets its own; the
	// Linear coordinate rides the shared forgeWiring.linearTokens instance.
	forgeMirrorService, err := wireForgeWriteCaller(ctx, cfg, st, issueBrd, serverResolver, hub, commsSvc, hubLog, forgeWiring.primaryRouter, forgeWiring.linearTokens, udsListener, listeners)
	if err != nil {
		return err
	}
	// Run every door under one scoped group: scoped lifecycle, first-error-wins,
	// and sibling cancellation. gctx cancels on parent shutdown or a door's own
	// error and always ends by Wait, so the verifier's key refresh never outlives
	// Serve. The UDS door is primary; its error wins over the drain result below.
	g, gctx := newServeGroup(ctx, doors.runnerVerifier)
	if forgeMirrorService != nil {
		forgeMirrorService.SetMirrorLifetime(gctx)
		defer forgeMirrorService.StopMirrorLifetime()
		g.Go(func() error { return forgeMirrorService.runWriteSweep(gctx) })
	}
	startServeDoors(g, gctx, doors, udsListener, devListener, netListener, mergeQueueRunner)
	// The forge webhook-ingestion lanes (RIG-2883 board + RIG-2732 T7 notify):
	// each lane joins the SAME scoped group so it inherits the doors' lifecycle.
	// The board + GitHub notify lanes are nil when the GitHub App is absent; the
	// Linear notify lane is nil when Linear is not configured. A nil lane starts nothing.
	startForgeIngestLanes(gctx, g, forgeWiring.boardLane, doors.gitCredentials, forgeWiring.notifyLane, doors.linearNotify)
	// The Linear session responder drains on the same group; nil when Linear is off.
	startLinearResponder(gctx, g, doors.linearResponder, commsBus)
	// The comms consumers (delivery fan-out, presence projection) run on the serve
	// group rooted on gctx; presence also ends when drainDoors closes the comms bus.
	startCommsConsumers(gctx, g, commsBus, fab, st, hub, hubLog)
	// Background usage upkeep: the daily raw-log prune and the hourly orphan-interval close.
	startUsageSweepers(gctx, g, st, cfg.UsageEventRetention, hubLog)
	startServeDrain(g, gctx, doors.drainSet(drainSet{
		bus:         bus,
		commsBus:    commsBus,
		hub:         hub,
		log:         hubLog,
		cancelHooks: cancelSeed,
	}))

	return g.Wait()
}

// drainSet fills the door fields of d from the built doors.
func (s serveDoors) drainSet(d drainSet) drainSet {
	d.uds, d.dev, d.net, d.streams, d.health = s.uds, s.dev, s.net, s.streams, s.health
	return d
}

func startServeDoors(g *errgroup.Group, ctx context.Context, doors serveDoors, uds, dev, network net.Listener, mergeQueue mergeQueueRunner) {
	g.Go(func() error { return classifyServe(doors.uds.Serve(uds), "compass.v1 UDS server") })
	if doors.dev != nil {
		g.Go(func() error { return classifyServe(doors.dev.Serve(dev), "dev gRPC-Web server") })
	}
	if doors.net != nil {
		g.Go(func() error {
			return classifyServe(doors.net.ServeTLS(network, "", ""), "compass.v1 network door")
		})
	}
	mergeQueue.start(g, ctx)
}

// startServeDrain adds the drain member of the same group: wake on gctx cancellation, then hand off to
// drainDoors. A drain that overruns (a handler still wedged mid-replay)
// surfaces as the error rather than a false clean shutdown; a real serve
// error still wins because the group keeps the first error.
func startServeDrain(g *errgroup.Group, ctx context.Context, d drainSet) {
	g.Go(func() error {
		<-ctx.Done()
		return drainDoors(ctx, d)
	})
}

// serveDoors holds the three compass.v1 doors Serve drives: the shipped Unix
// socket (always built), the optional dev loopback browser door, and the
// optional authenticated network door. dev and net are nil when their listener
// is off (dev unless --dev-http, network unless --listen or cfg.ListenListener).
type serveDoors struct {
	uds *http.Server
	dev *http.Server
	net *http.Server
	// streams cuts stalled server-stream writes across every door during drain.
	streams *streamDrain
	health  *serverHealth
	// The net door must use the container StoreResolver or deployment secrets reach agents.
	netResolver *brokeredSecretResolver
	// gitCredentials refreshes the brokered App tokens; nil without a GitHub App.
	gitCredentials *gitCredentialBroker
	// linearNotify is the Linear agent-notification lane (RIG-2732 T7), built
	// beside the webhook handler it feeds; nil when Linear is not configured (its
	// client-credentials pair undeclared). Serve starts its arm + reconciler on
	// the serve group.
	linearNotify *forgeNotifyLane
	// linearResponder drains verified Linear session events; nil when Linear is off.
	linearResponder *linearagent.Dispatcher
	// runnerVerifier refreshes the RunnerService door's cluster keys; nil
	// without --runner-clusters. Serve starts it once startup cannot fail.
	runnerVerifier keyRefresher
}

// buildDoors assembles the doors from the service graph and pre-bound listeners.
// It returns servers for the caller's errgroup; on failure, the caller closes
// listeners. Interceptor ordering is security-critical (see the dev door).
func buildDoors(
	ctx context.Context,
	cfg ServeConfig,
	svc *service,
	commsSvc *comms.Comms,
	secretsSvc *secretsService,
	hub *runnerhub.Hub,
	st *store.Store,
	adminID store.AccountID,
	linearBridgeID store.AccountID,
	// resolver is the container StoreResolver used by FetchSecrets. serverResolver
	// reads server_secrets and is only for webhook wiring; mixing them would expose
	// deployment secrets to agent containers.
	resolver *secrets.StoreResolver,
	serverResolver secrets.Resolver,
	devListener net.Listener,
	netListener net.Listener,
	netTLS *tls.Config,
	webhookSink ForgeEventSink,
	webhookSecret func(ctx context.Context) ([]byte, error),
	linearTokens *linearagent.TokenSource,
	masterKey envelope.Key,
	keyVersion int16,
) (serveDoors, error) {
	streams := newStreamDrain()
	health := newServerHealth(hub)
	// Built even without a network door, so a bad cluster file or a reserved-ID
	// clash refuses start the same way on every topology.
	runnerVerifier, err := buildRunnerVerifier(ctx, cfg, st)
	if err != nil {
		return serveDoors{}, err
	}
	usageSvc := newUsageService(usage.NewPostgres(st), st)
	gateway := newGatewayServices(st, masterKey, keyVersion)
	// otelconnect produces the server RPC span; NewTraceResponseInterceptor stamps
	// the trace id onto "traceresponse". Both inert no-ops when OtelEndpoint is
	// empty, so mounted unconditionally. otelconnect goes FIRST so the span
	// envelopes the security-critical interceptors.
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		return serveDoors{}, fmt.Errorf("otel: rpc interceptor: %w", err)
	}
	// The forge broker supplies the host and the targeted secret signal before
	// any public door is assembled.
	repoInstallations, err := buildRepoInstallationResolver(cfg, serverResolver)
	if err != nil {
		return serveDoors{}, err
	}
	gitCredentials, err := buildGitCredentialBroker(cfg, st, serverResolver, repoInstallations, hub, slog.Default())
	if err != nil {
		return serveDoors{}, err
	}
	// A nil hub must reach the service as a nil interface, not a typed nil.
	var repoSignal agentSecretsSignaler
	if hub != nil {
		repoSignal = hub
	}
	reviewerInstallations, err := buildConfiguredReviewerInstallationResolver(cfg.Forge, serverResolver)
	if err != nil {
		return serveDoors{}, err
	}
	agentRepositorySvc := newAgentRepositoryService(st, gitCredentials, repoSignal, repoInstallations)
	if reviewerInstallations != nil {
		agentRepositorySvc.reviewerInstallations = reviewerInstallations
	}

	// CommsService rides the socket + dev doors and the network shared chain with
	// no auth pair of its own here: comms attributes every RPC to the bootstrap admin.
	ambient := []connect.ServerInterceptor{auth.AmbientIdentity(adminID)}
	commsServer := connect.NewServer(otel.HandlerInterceptors(otelIC))
	compassv1connect.RegisterCommsServiceHandler(commsServer, commsSvc)
	// Shipped door: the Unix socket serves native gRPC, gRPC-Web, and Connect off
	// one connect-go handler. No CORS: same-origin. The 0600 socket is the credential,
	// so each service mounts the ambient-identity pair (the bootstrap admin, a user).
	socketServer := connect.NewServer(otel.HandlerInterceptors(otelIC, ambient...))
	compassv1connect.RegisterCompassServiceHandler(socketServer, svc)
	secretsSocketServer := connect.NewServer(otel.HandlerInterceptors(otelIC, ambient...))
	compassv1connect.RegisterSecretsServiceHandler(secretsSocketServer, secretsSvc)
	agentRepositoryServer := connect.NewServer(otel.HandlerInterceptors(otelIC, ambient...))
	compassv1connect.RegisterAgentRepositoryServiceHandler(agentRepositoryServer, agentRepositorySvc)
	usageSocketServer := connect.NewServer(otel.HandlerInterceptors(otelIC, ambient...))
	compassv1connect.RegisterUsageServiceHandler(usageSocketServer, usageSvc)
	udsMux := http.NewServeMux()
	connecthttp.Mount(udsMux, socketServer, connecthttp.WithReadMaxBytes(compassServiceMaxReadBytes))
	connecthttp.Mount(udsMux, commsServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	connecthttp.Mount(udsMux, secretsSocketServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	connecthttp.Mount(udsMux, usageSocketServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	connecthttp.Mount(udsMux, agentRepositoryServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
	health.mount(udsMux)
	udsServer := newLocalDoorServer(streams.wrap(udsMux), localDoorHeaderTimeout, localDoorIdleTimeout)
	// Dev-only browser door: the same services with permissive CORS on the
	// loopback listener, off unless DevHTTP is passed. AdminGate must precede
	// ambient identity so adminOnly RPCs fail closed.
	var devServer *http.Server
	if devListener != nil {
		devAmbient := []connect.ServerInterceptor{auth.NewAdminGate(adminID), auth.AmbientIdentity(adminID)}
		compassDevServer := connect.NewServer(otel.HandlerInterceptors(otelIC, devAmbient...))
		compassv1connect.RegisterCompassServiceHandler(compassDevServer, svc)
		devSecretsServer := connect.NewServer(otel.HandlerInterceptors(otelIC, devAmbient...))
		compassv1connect.RegisterSecretsServiceHandler(devSecretsServer, secretsSvc)
		devAgentRepositoryServer := connect.NewServer(otel.HandlerInterceptors(otelIC, devAmbient...))
		compassv1connect.RegisterAgentRepositoryServiceHandler(devAgentRepositoryServer, agentRepositorySvc)
		devUsageServer := connect.NewServer(otel.HandlerInterceptors(otelIC, devAmbient...))
		compassv1connect.RegisterUsageServiceHandler(devUsageServer, usageSvc)
		devMux := http.NewServeMux()
		connecthttp.Mount(devMux, compassDevServer, connecthttp.WithReadMaxBytes(compassServiceMaxReadBytes))
		connecthttp.Mount(devMux, commsServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
		connecthttp.Mount(devMux, devSecretsServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
		connecthttp.Mount(devMux, devUsageServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
		connecthttp.Mount(devMux, devAgentRepositoryServer, connecthttp.WithReadMaxBytes(siblingServiceMaxReadBytes))
		health.mount(devMux)
		devServer = newLocalDoorServer(streams.wrap(devCORS().Handler(devMux)), localDoorHeaderTimeout, localDoorIdleTimeout)
	}
	linear, err := buildLinearWiring(ctx, cfg, st, hub, commsSvc, serverResolver, adminID, linearBridgeID, linearTokens)
	if err != nil {
		return serveDoors{}, err
	}
	// Authenticated network door, built only when --listen is given. It mints and
	// writes the bootstrap token 0600 under the state dir and mounts CompassService
	// + CommsService behind bearer + admin-gate, the RunnerService door, and — when
	// the board lane is on — the POST /webhooks/github ingress outside that gate.
	var netServer *http.Server
	// One variable feeds both the call and the record below, so the two cannot
	// drift apart and the recorded instance is always the delivered one. The
	// broker mints from the SERVER App key but adds only a repo-scoped token.
	netResolver := &brokeredSecretResolver{inner: resolver, broker: gitCredentials}
	if netListener != nil {
		s, err := buildNetworkServer(ctx, cfg, svc, commsSvc, secretsSvc, usageSvc, agentRepositorySvc, hub, st, adminID,
			netTLS, netResolver, otelIC, webhookSink, webhookSecret, linear.webhook, linear.sessionLink, runnerVerifier, gateway, health, streams)
		if err != nil {
			return serveDoors{}, err
		}
		netServer = s
	}

	// netResolver records WHICH instance reached the container delivery path; a
	// swap here is silent and severe: runnerhub's FetchSecrets would serve
	// `server_secrets`, handing every deployment secret to every agent container.
	doors := serveDoors{
		health: health,
		uds:    udsServer, dev: devServer, net: netServer, streams: streams,
		netResolver: netResolver, gitCredentials: gitCredentials,
		linearNotify: linear.notify, linearResponder: linear.responder,
	}
	// GitHub refs link wherever the App reads the forge, writes or not.
	svc.refLinkRules = buildRefLinkRuleSource(cfg, cfg.Forge.boardIngestionEnabled(), st, linear.notify, slog.Default())
	// Only when set: a nil pointer stored in the interface would not compare nil.
	if runnerVerifier != nil {
		doors.runnerVerifier = runnerVerifier
	}
	return doors, nil
}

// keyRefresher is the verifier's background half; *auth.RunnerVerifier satisfies it.
type keyRefresher interface {
	Start(ctx context.Context)
}

// newServeGroup returns Serve's scoped errgroup and starts v's key refresh under
// its ctx, which is cancelled when Wait returns. A nil v starts nothing.
func newServeGroup(ctx context.Context, v keyRefresher) (*errgroup.Group, context.Context) {
	g, gctx := errgroup.WithContext(ctx)
	if v != nil {
		v.Start(gctx)
	}
	return g, gctx
}

// drainSet is the shutdown-side view of what Serve built: the two buses whose
// held-open streams must end before a door can drain, the doors themselves
// (dev and net are nil when not configured), and the hub whose frame-loss
// counters are final once every door is drained.
type drainSet struct {
	bus      *events.Bus[busPayload]
	commsBus *events.Bus[*compassv1.SubscribeCommsResponse]
	streams  *streamDrain
	uds      *http.Server
	dev      *http.Server
	net      *http.Server
	health   *serverHealth
	hub      *runnerhub.Hub
	log      *slog.Logger
	// cancelHooks cancels the runner-ready seed's ctx; nil when none was derived.
	cancelHooks context.CancelFunc
}

// drainTimeout bounds the graceful drain of every door. A package var rather
// than a const only so a test can shorten it; never reassigned in production.
var drainTimeout = 5 * time.Second

// drainDoors ends live streams, drains every configured door under one bounded
// deadline, and reports the hub's final frame accounting.
func drainDoors(ctx context.Context, d drainSet) error {
	d.health.markDraining()
	// Close both buses so held-open streams end and release their handlers:
	// SubscribeEvents rides bus, SubscribeComms rides commsBus. Closing only bus would
	// wedge a SubscribeComms subscriber (Shutdown does not cancel its ctx). Both closes idempotent.
	d.bus.Close()
	d.commsBus.Close()
	// Shutdown neither cancels handler ctx nor unblocks a stalled Send, so cut stream writes.
	d.streams.abort(ctx)
	// Stop the runner-ready seed first: bar new fires and cancel its ctx, so it unwinds
	// while the doors drain. A drain woken by a group error has a live parent ctx.
	hooksDone := d.hub.StopReadyHooks()
	if d.cancelHooks != nil {
		d.cancelHooks()
	}
	// The parent is already cancelled (that woke this drain); keep its values, drop its cancel.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	doors := []*http.Server{d.uds}
	for _, srv := range []*http.Server{d.dev, d.net} {
		if srv != nil {
			doors = append(doors, srv)
		}
	}
	var drainErr error
	for _, srv := range doors {
		if err := srv.Shutdown(shutdownCtx); err != nil && drainErr == nil {
			drainErr = err
		}
	}
	if errors.Is(drainErr, context.DeadlineExceeded) {
		// Shutdown leaves overrunning handlers running; Close cancels their request
		// contexts so they release pool connections before the store close waits on them.
		for _, srv := range doors {
			_ = srv.Close() // best-effort: the overrun is already the reported error
		}
	}
	if err := awaitReadyHooks(shutdownCtx, hooksDone); err != nil && drainErr == nil {
		drainErr = err
	}
	// After a clean drain the hub's counters are final; after an overrun they are a
	// best-effort snapshot. This is the only non-test reader of the frame-loss accounting.
	logFrameDiagnostics(shutdownCtx, d.log, d.hub)
	if drainErr != nil {
		return fmt.Errorf("draining compass.v1 servers on shutdown: %w", drainErr)
	}
	return nil
}

// awaitReadyHooks joins the runner-ready seed. Past the drain deadline it reports
// the overrun but still waits: the seed's ctx is cancelled, and its store calls
// must finish before Serve's deferred store close.
func awaitReadyHooks(shutdownCtx context.Context, hooksDone <-chan struct{}) error {
	select {
	case <-hooksDone:
		return nil
	case <-shutdownCtx.Done():
	}
	select { // both may be ready at once; a finished seed is not an overrun
	case <-hooksDone:
		return nil
	default:
	}
	<-hooksDone
	return fmt.Errorf("runner-ready seed still running: %w", shutdownCtx.Err())
}

// publishReady stamps the initial ServerStatus{Ready} onto the bus.
func publishReady(ctx context.Context, bus *events.Bus[busPayload]) {
	bus.Publish(ctx, &compassv1.SubscribeEventsResponse{
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
		AllowedOrigins: []string{"*"},
		AllowedMethods: connectcors.AllowedMethods(),
		// Same inbound mirror as the network door: the dev door is what a
		// browser dev server dials, so the session header has to be allowed
		// here too or the preflight blocks the request outright.
		AllowedHeaders:   append(connectcors.AllowedHeaders(), otel.PostHogSessionHeader),
		ExposedHeaders:   append(connectcors.ExposedHeaders(), "traceresponse"),
		AllowCredentials: false,
	})
}

// Local doors (UDS, loopback dev) bound the header read and idle lifetime so a
// local process cannot hold connections open forever.
const (
	localDoorHeaderTimeout = 10 * time.Second
	localDoorIdleTimeout   = 120 * time.Second
)

func newLocalDoorServer(h http.Handler, header, idle time.Duration) *http.Server {
	return &http.Server{Handler: h, Protocols: cleartextHTTP2(), ReadHeaderTimeout: header, IdleTimeout: idle}
}

// cleartextHTTP2 enables HTTP/1.1 and cleartext HTTP/2 for gRPC, gRPC-Web, and
// Connect clients on one handler. http.Server tracks both protocols for shutdown.
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

// boardIngestLane combines the webhook arm and reconciliation sweep; Serve runs
// both in the shared errgroup and mounts its sink on the webhook ingress.
type boardIngestLane struct {
	arm        *ingest.BoardWebhookArm
	reconciler *ingest.BoardReconciler
	sink       ForgeEventSink
	// Tests observe this router to prove both lanes share one reader.
	router *forge.OwnerRoutedGitHub
}

// forgeReadWiring bundles the read-side credentials shared by doors and writes.
// Board and notify lanes route by owner; author writes share the primary client.
// Linear lanes reuse one OAuth token source. Board fields are nil without its App.
type forgeReadWiring struct {
	boardLane     *boardIngestLane
	notifyLane    *forgeNotifyLane
	webhookSink   ForgeEventSink
	webhookSecret func(ctx context.Context) ([]byte, error)
	primaryRouter *forge.OwnerRoutedGitHub
	linearTokens  *linearagent.TokenSource
}

// buildForgeReadWiring assembles the GitHub App read lanes and the Linear token source.
// No App turns both lanes off with one warning; a configured App with a bad secret fails startup.
func buildForgeReadWiring(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	hub *runnerhub.Hub,
	resolver secrets.Resolver,
	log *slog.Logger,
) (forgeReadWiring, error) {
	boardLane, notifyLane, webhookSink, webhookSecret, primaryRouter, err := buildBoardWebhookWiring(ctx, cfg, st, issueBrd, hub, resolver, log)
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
		primaryRouter: primaryRouter,
		linearTokens:  linearTokens,
	}, nil
}

// buildBoardWebhookWiring assembles both GitHub App lanes over one shared client and owner router.
// It fans webhooks to both lanes and returns setup faults so Serve can close listeners.
func buildBoardWebhookWiring(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	hub *runnerhub.Hub,
	resolver secrets.Resolver,
	log *slog.Logger,
) (*boardIngestLane, *forgeNotifyLane, ForgeEventSink, func(ctx context.Context) ([]byte, error), *forge.OwnerRoutedGitHub, error) {
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
	httpClient, err := forgeHTTPClient(rc.ForgeCAPath)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	appConfig := forge.GitHubAppConfig{
		AppID:          rc.App.AppID,
		InstallationID: rc.App.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, rc.App.AppPrivateKeySecret),
		Host:           rc.Host,
		Client:         httpClient,
	}
	tok, err := forge.NewAppTokenSource(appConfig)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("board webhook app token source: %w", err)
	}
	client := forge.NewGitHub(forge.GitHubConfig{Host: rc.Host, Token: tok, Client: httpClient})
	installationResolver, err := forge.NewInstallationResolver(appConfig)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("board webhook installation resolver: %w", err)
	}
	// Reuse the primary client so every default-installation lane shares one budget gate.
	router := forge.NewOwnerRoutedGitHub(client, installationResolver, appConfig, httpClient)
	// The board lane reuses the notify lane's head-SHA cache: every check_suite
	// reaches both lanes, so one cache keeps it to one lookup.
	notifyLane := buildForgeNotifyLane(cfg, st, hub, router, log)
	lane, err := buildBoardIngestLane(ctx, cfg, st, issueBrd, router, notifyLane.pulls, log)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	sink := &fanoutSink{sinks: []ForgeEventSink{lane.sink, notifyLane.sink}}
	secret := newCachedWebhookSecret(resolver, rc.App.AppWebhookSecretName)
	return lane, notifyLane, sink, secret, router, nil
}

// buildLinearWebhookWiring builds the shared Linear handler when its signing
// secret is declared, independent of the GitHub App gate. Missing secrets leave
// the route off; resolve faults fail startup. Nil sinks ack and drop events.
// Cache the secret because this unauthenticated endpoint checks HMAC after resolve.
func buildLinearWebhookWiring(
	ctx context.Context,
	cfg ServeConfig,
	resolver secrets.Resolver,
	dataSink ForgeEventSink,
	sessionSink SessionEventSink,
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
	_, handler := NewLinearWebhookHandler(secret, dataSink, sessionSink, log)
	return handler, nil
}

// buildBoardIngestLane assembles the GitHub board-ingestion pipeline with optional queue lookup.
func buildBoardIngestLane(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	router *forge.OwnerRoutedGitHub,
	pullNumbers ingest.PullNumberResolver,
	log *slog.Logger,
) (*boardIngestLane, error) {
	fc := cfg.Forge.resolved()
	const provider = store.ForgeProviderGitHub
	if err := reconcileForgeSeed(ctx, st, provider, fc.Host, cfg.Forge.SeedRepos); err != nil {
		return nil, err
	}
	var queues map[string]forge.MergeQueue
	var queueConfigs ingest.QueueConfigs
	if len(fc.MergeQueues) > 0 {
		queues = mergeQueueProviders(fc.Host, newTrunkTokenResolver(buildServerSecretResolver(st, cfg), fc.TrunkTokenSecretName), fc.TrunkHTTP)
		queueConfigs = &mergeQueueConfigStore{st: st, host: fc.Host}
	}
	ref := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: fc.Host}
	ing := ingest.NewIngester(router, issueBrd, ref)
	pulls := ingest.NewPullRequestHydrator(router, issueBrd, ref, queues, queueConfigs, log)
	arm := ingest.NewBoardWebhookArm(router, ing, &boardTargetStore{st: st}, ingest.BoardArmConfig{
		Log: log, Pulls: pulls, PullNumbers: pullNumbers,
	})
	reconciler := ingest.NewBoardReconciler(router, ing, &boardReconcileStore{st: st, provider: provider, host: fc.Host}, ingest.BoardReconcileConfig{
		Backstop: fc.App.ReconcileBackstop, Log: log, Pulls: pulls,
	})
	return &boardIngestLane{arm: arm, reconciler: reconciler, sink: arm, router: router}, nil
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

// boardReconcileStore adapts the store to ingest.BoardStore and binds provider
// and host for watermark operations. Target enumeration remains repo-keyed and
// spans coordinates.
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

// PullRequestUpdatedAt returns the stored PR row's forge updated_at.
func (a *boardReconcileStore) PullRequestUpdatedAt(ctx context.Context, repo string, number uint64) (time.Time, bool, error) {
	return a.st.PullRequestUpdatedAt(ctx, store.ForgeCoord{Provider: a.provider, Host: a.host, Repo: repo, Number: number})
}

// PRsBackfilledAt returns when the repo's PR backfill ran.
func (a *boardReconcileStore) PRsBackfilledAt(ctx context.Context, repo string) (time.Time, bool, error) {
	return a.st.PRsBackfilledAt(ctx, a.provider, a.host, repo)
}

// MarkPRsBackfilled records that the repo's PR backfill ran.
func (a *boardReconcileStore) MarkPRsBackfilled(ctx context.Context, repo string, at time.Time) error {
	return a.st.MarkPRsBackfilled(ctx, a.provider, a.host, repo, at)
}

// forgeNotifyLane combines the webhook arm and reconciliation sweep for the
// GitHub notification lane. Both run in the serve group and share the GitHub
// webhook ingress with the board lane.
type forgeNotifyLane struct {
	arm        *ingest.NotifyWebhookArm
	reconciler *ingest.NotifyReconciler
	sink       ForgeEventSink // the arm; the ingress fan-out registers it
	// reader is the notify-read client this lane's reconciler + checks roller
	// ride. GitHub records the shared owner router; Linear records its client.
	reader forge.NotifyReader
	// This field exists so tests can verify the builder wires the resolver.
	pulls ingest.PullNumberResolver
	// Retained so tests can verify the self-origin resolver is wired.
	identities ingest.IdentityResolver
}

// forgeNotifyStore adapts the store to ingest.NotifyStore, binding provider and
// host while keeping the ingest seam coordinate-free. Artifact-kind enums mirror
// the wire kind, so their conversion is a direct cast.
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

// AdvanceDeliveredRevisionCAS backs the router's suppress-path cursor advance
// over the store's compare-and-set writer. A lost CAS (advanced=false, nil err)
// is passed through unchanged so the router degrades open rather than treating
// it as a fault.
func (a *forgeNotifyStore) AdvanceDeliveredRevisionCAS(ctx context.Context, agentAccountID, subscriptionID, prior, next string) (bool, error) {
	return a.st.AdvanceForgeDeliveredRevisionCAS(ctx, store.AccountID(agentAccountID), subscriptionID, prior, next)
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
			Scope:             compassv1internal.ForgeSubscriptionScope(s.Scope),
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

// forgeIdentityResolver binds the store lookup to a forge coordinate and resolves
// owner-qualified handles for self-origin suppression. Clean misses and store
// faults return a zero Handle, so the router fails open and delivers.
type forgeIdentityResolver struct {
	st       *store.Store
	provider store.ForgeProvider
	host     string
}

// HandleForAccount resolves an agent and its owner's handle with two account reads.
// Missing rows or a non-agent account are clean misses with a zero Handle.
func (r *forgeIdentityResolver) HandleForAccount(ctx context.Context, accountID string) (ingest.Handle, error) {
	agent, err := r.st.GetAccount(ctx, store.AccountID(accountID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ingest.Handle{}, nil
		}
		return ingest.Handle{}, err
	}
	if agent.Agent == nil {
		// A non-agent account has no owner to qualify by — a clean miss.
		return ingest.Handle{}, nil
	}
	owner, err := r.st.GetAccount(ctx, agent.Agent.OwnerUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ingest.Handle{}, nil
		}
		return ingest.Handle{}, err
	}
	return ingest.Handle{Owner: owner.Handle, Agent: agent.Handle}, nil
}

// AuthorHandle resolves the recorded author at the bound coordinate, then uses
// HandleForAccount for owner qualification. Missing ownership or account rows are
// clean misses, not errors.
func (r *forgeIdentityResolver) AuthorHandle(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (ingest.Handle, error) {
	row, err := r.st.AuthoredArtifactByCoordinate(ctx, r.provider, r.host, repo, store.ForgeArtifactKind(kind), number)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ingest.Handle{}, nil
		}
		return ingest.Handle{}, err
	}
	return r.HandleForAccount(ctx, string(row.AgentAccountID))
}

// errNoLiveSession is the sentinel forgeNotifyDispatcher.Notify returns when the
// resolved subscriber has no live session: the router logs it and moves on, and
// the reconcile sweep re-notifies from the durable gap (W3). It is DELIBERATELY
// not a cursor advance — no cursor is touched on a no-session dispatch.
var errNoLiveSession = errors.New("forge notify: no live session for account")

// notifySessionDispatcher is the hub surface used by forgeNotifyDispatcher.
// Its local interface lets tests exercise dispatch without a live hub or database.
type notifySessionDispatcher interface {
	SessionForAccount(ctx context.Context, account store.AccountID) (sessionID string, ok bool)
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
	sessionID, ok := d.hub.SessionForAccount(ctx, store.AccountID(account))
	if !ok {
		return errNoLiveSession
	}
	return d.hub.DispatchControl(ctx, sessionID, &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_ForgeNotification{ForgeNotification: n},
	})
}

// forgeNotifyChecksRoller adapts NotifyReader to the checks roll-up seam.
// GitHub and Linear share this adapter; Linear returns ErrUnsupported, but its
// issues-only event stream never produces CHECKS, so RollUp is never called.
type forgeNotifyChecksRoller struct {
	client forge.NotifyReader
}

// RollUp resolves the combined checks roll-up for the head SHA, forwarding to the
// client's conditional checks read.
func (r *forgeNotifyChecksRoller) RollUp(ctx context.Context, repo string, number uint64, headSHA, etag string) (forge.ConditionalResult[forge.Checks], error) {
	return r.client.ChecksConditional(ctx, repo, number, headSHA, etag)
}

// forgeNotifyPullNumbers adapts the GitHub head-SHA-to-PR-number resolver needed
// by CHECKS events. Linear is issues-only, so its lane leaves this resolver nil.
type forgeNotifyPullNumbers struct {
	client forge.PullRequestResolver
}

// PullNumberForSHA resolves the head SHA's PR number, forwarding to the client's
// commit->pulls association read.
func (p *forgeNotifyPullNumbers) PullNumberForSHA(ctx context.Context, repo, headSHA string) (uint64, error) {
	return p.client.PullRequestForSHA(ctx, repo, headSHA)
}

// buildForgeNotifyLane assembles the GitHub notification lane through the shared owner router.
// ErrBudgetExhausted is shared by board and notify for the default installation.
func buildForgeNotifyLane(
	cfg ServeConfig,
	st *store.Store,
	hub *runnerhub.Hub,
	router *forge.OwnerRoutedGitHub,
	log *slog.Logger,
) *forgeNotifyLane {
	fc := cfg.Forge.resolved()
	const provider = store.ForgeProviderGitHub

	notifyStore := &forgeNotifyStore{st: st, provider: provider, host: fc.Host}
	dispatcher := &forgeNotifyDispatcher{hub: hub}
	checks := &forgeNotifyChecksRoller{client: router}
	// The head_sha->number step a check_suite needs, TTL-cached so the several
	// check_suite events one push fires (one per installed App, all carrying the
	// same head SHA) cost ONE commits/{sha}/pulls GET against the shared budget.
	pulls := ingest.NewCachedPullNumberResolver(&forgeNotifyPullNumbers{client: router})
	forgeRef := &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     fc.Host,
	}
	identities := &forgeIdentityResolver{st: st, provider: provider, host: fc.Host}
	eventRouter := ingest.NewNotifyRouter(notifyStore, dispatcher, checks, pulls, identities, forgeRef, log)
	arm := ingest.NewNotifyWebhookArm(eventRouter, ingest.NotifyArmConfig{Log: log})
	reconciler := ingest.NewNotifyReconciler(router, notifyStore, eventRouter,
		compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, fc.Host, ingest.ReconcileConfig{
			Backstop: fc.App.ReconcileBackstop,
			Log:      log,
		})
	return &forgeNotifyLane{arm: arm, reconciler: reconciler, sink: arm, reader: router, pulls: pulls, identities: identities}
}

// buildLinearNotifyLane returns nil when the shared Linear OAuth source is absent.
// Otherwise it reuses that source for the issue-only notify lane and write path.
// Linear emits no CHECKS events; the required checks roller's unsupported result
// is therefore never called.
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
		host     = forge.LinearHost
	)
	client := forge.NewLinear(forge.LinearConfig{Token: tokens, Log: log})

	notifyStore := &forgeNotifyStore{st: st, provider: provider, host: host}
	dispatcher := &forgeNotifyDispatcher{hub: hub}
	checks := &forgeNotifyChecksRoller{client: client}
	forgeRef := &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     host,
	}
	// Nil pull-number resolver: Linear is issues-only and never produces a CHECKS
	// event, so there is no head SHA to resolve (the router tolerates nil). The
	// identity resolver IS wired: self-origin suppression applies to Linear
	// COMMENT / OPENED / STATE the same as GitHub.
	identities := &forgeIdentityResolver{st: st, provider: provider, host: host}
	router := ingest.NewNotifyRouter(notifyStore, dispatcher, checks, nil, identities, forgeRef, log)
	arm := ingest.NewNotifyWebhookArm(router, ingest.NotifyArmConfig{Log: log})
	reconciler := ingest.NewNotifyReconciler(client, notifyStore, router,
		compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, host, ingest.ReconcileConfig{
			Backstop: 0, // no App config carries a Linear backstop; 0 -> ingest's defaultBackstop.
			Log:      log,
		})
	// pulls stays nil: Linear is issues-only and never emits a CHECKS event, so
	// there is no head SHA to resolve.
	return &forgeNotifyLane{arm: arm, reconciler: reconciler, sink: arm, reader: client, pulls: nil, identities: identities}
}

// newDeclaredSecretResolver resolves an operator-facing secret name from the
// SERVER resolver on each call. It compares the prefixed registry name but keeps
// errors unprefixed, propagates ctx, and surfaces faults or absent names.
// It serves App-token and webhook-HMAC paths; unprefixed comparison fails at runtime.
func newDeclaredSecretResolver(resolver secrets.Resolver, name string) func(ctx context.Context) ([]byte, error) {
	return resolveDeclaredSecret(resolver, name, "board webhook")
}

func newTrunkTokenResolver(resolver secrets.Resolver, name string) func(ctx context.Context) ([]byte, error) {
	return resolveDeclaredSecret(resolver, name, "Trunk API")
}

func resolveDeclaredSecret(resolver secrets.Resolver, name, label string) func(ctx context.Context) ([]byte, error) {
	want := serverSecretName(name)
	return func(ctx context.Context) ([]byte, error) {
		resolved, err := resolver.Resolve(ctx, label)
		if err != nil {
			return nil, fmt.Errorf("%s secret resolve: %w", label, err)
		}
		for _, secret := range resolved {
			if secret.Name == want {
				return []byte(secret.Value), nil
			}
		}
		return nil, fmt.Errorf("%s secret %q not declared", label, name)
	}
}

// cachedWebhookSecret TTL-caches the webhook secret because unauthenticated
// requests resolve it before HMAC validation. Without caching, each garbage POST
// could trigger a full secretspec export; rotation takes effect within the TTL.
// Resolve faults are returned, never cached. App-key resolution is cold-path.
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

// validateForgeSecret distinguishes transient resolve faults from permanent
// missing-secret misconfiguration. It compares prefixed names in the SERVER
// keyspace but reports the unprefixed operator-facing name.
func validateForgeSecret(ctx context.Context, resolver secrets.Resolver, reason, name string) error {
	resolved, err := resolver.Resolve(ctx, reason)
	if err != nil {
		return fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	want := serverSecretName(name)
	for _, s := range resolved {
		if s.Name == want {
			return nil
		}
	}
	// Names BOTH: the unprefixed name the operator configured (so they can find
	// it in their config) and the prefixed provider key they must actually
	// populate. The record requires the actionable "set SERVER_<NAME> in the
	// provider" form; naming only one of the two leaves the operator guessing.
	return fmt.Errorf("forge secret %q not declared: set %s in the provider", name, serverSecretName(name))
}

// wireForgeWriteCaller mounts writes only when both Apps are configured. Resolve
// faults fail startup; with writes off, Hub.RelayForgeCall fails closed with
// CodeUnavailable, and a partial config warns. Setup faults close bound listeners;
// Serve bounds mirror work to the returned service's lifetime (nil: writes off).
func wireForgeWriteCaller(ctx context.Context, cfg ServeConfig, st *store.Store, issueBrd *board.IssueProjection, resolver secrets.Resolver, hub *runnerhub.Hub, commsSvc *comms.Comms, log *slog.Logger, primaryRouter *forge.OwnerRoutedGitHub, linearTokens *linearagent.TokenSource, udsListener net.Listener, listeners boundListeners) (*forgeService, error) {
	declaredSecrets, err := resolver.Resolve(ctx, "forge write")
	if err != nil {
		return nil, failStartup(udsListener, listeners, fmt.Errorf("forge secret resolve failed at startup: %w", err))
	}
	if !cfg.Forge.forgeWritesEnabled(declaredSecrets) {
		warnPartialForgeWriteSecrets(ctx, cfg.Forge, declaredSecrets, log)
		return nil, nil //nolint:nilnil // nil service is the explicit disabled state.
	}
	forgeSvc, err := buildForgeWriteService(ctx, cfg, st, issueBrd, resolver, primaryRouter, linearTokens, log)
	if err != nil {
		return nil, failStartup(udsListener, listeners, err)
	}
	forgeSvc.publicURL = cfg.PublicURL
	hub.SetForgeCaller(forgeSvc)
	commsSvc.SetMirrorer(forgeSvc, forgeSvc)
	return forgeSvc, nil
}

// buildForgeWriteService builds the two-App write caller and registers GitHub
// plus optional Linear coordinates. The reviewer has a distinct identity;
// board, notify, and author writes share the primary router's rate gate.
func buildForgeWriteService(
	ctx context.Context,
	cfg ServeConfig,
	st *store.Store,
	issueBrd *board.IssueProjection,
	resolver secrets.Resolver,
	primaryRouter *forge.OwnerRoutedGitHub,
	linearTokens *linearagent.TokenSource,
	log *slog.Logger,
) (*forgeService, error) {
	fc := cfg.Forge.resolved()
	if primaryRouter == nil {
		return nil, errors.New("forge write: primary App client is nil (writes enabled without a configured primary App)")
	}

	// (2) The reviewer App client: validate its key secret (distinct fail-fast
	// text), then build its own installation-token source + client — a distinct
	// GitHub identity from the primary App so F1's author-cannot-approve holds.
	if err := validateForgeSecret(ctx, resolver, "forge reviewer app key", fc.ReviewerApp.AppPrivateKeySecret); err != nil {
		return nil, err
	}
	reviewerHTTP, err := forgeHTTPClient(fc.ForgeCAPath)
	if err != nil {
		return nil, err
	}
	reviewerConfig := forge.GitHubAppConfig{
		AppID:          fc.ReviewerApp.AppID,
		InstallationID: fc.ReviewerApp.InstallationID,
		PrivateKey:     newDeclaredSecretResolver(resolver, fc.ReviewerApp.AppPrivateKeySecret),
		Host:           fc.Host,
		Client:         reviewerHTTP,
	}
	reviewerTok, err := forge.NewAppTokenSource(reviewerConfig)
	if err != nil {
		return nil, fmt.Errorf("forge reviewer app token source: %w", err)
	}
	reviewerClient := forge.NewGitHub(forge.GitHubConfig{Host: fc.Host, Token: reviewerTok, Client: reviewerHTTP})
	reviewerResolver, err := forge.NewInstallationResolver(reviewerConfig)
	if err != nil {
		return nil, fmt.Errorf("forge reviewer installation resolver: %w", err)
	}
	reviewerRouter := forge.NewOwnerRoutedGitHub(reviewerClient, reviewerResolver, reviewerConfig, reviewerHTTP)
	// (3) The provider registry: the GitHub coordinate (author = shared primary
	// client, reviewer = reviewer App client, F1) plus a Linear coordinate when
	// the shared Linear token source is configured.
	registry := newForgeProviderRegistry()
	if err := registerGitHubForgeCoordinate(registry, fc, primaryRouter, reviewerRouter); err != nil {
		return nil, err
	}

	// Linear write coordinate — registered ONLY when Linear is configured (else
	// GitHub-only). Linear is issues-only (DL-051): PR/review ops return ErrUnsupported.
	// One client serves both roles, riding the SAME linearTokens instance (DEC-4).
	// isDefault=false: GitHub is the default, Linear is selected explicitly.
	if linearTokens != nil {
		if err := registerLinearForgeCoordinate(registry, forge.NewLinear(forge.LinearConfig{Token: linearTokens, Log: log})); err != nil {
			return nil, err
		}
	}

	if err := reconcileForgeScopeGrants(ctx, st, fc, log); err != nil {
		return nil, err
	}
	svc := newForgeService(st, issueBrd, registry)
	svc.enforceScopes = !fc.ScopeEnforcementDisabled
	return svc, nil
}

// forgeScopeGranter is the store surface the boot grant seed needs.
type forgeScopeGranter interface {
	GrantForgeScope(ctx context.Context, scope store.ForgeScope) error
}

// reconcileForgeScopeGrants seeds declared grants and warns when enforcement is
// on with none declared at boot; writes still need a matching store grant.
func reconcileForgeScopeGrants(ctx context.Context, st forgeScopeGranter, fc ForgeConfig, log *slog.Logger) error {
	for _, g := range fc.ScopeGrants {
		if err := st.GrantForgeScope(ctx, g); err != nil {
			return fmt.Errorf("seeding forge scope grant (provider %d) %s/%s for %s: %w", g.Provider, g.Host, g.Repo, g.AccountID, err)
		}
	}
	if !fc.ScopeEnforcementDisabled && len(fc.ScopeGrants) == 0 {
		log.WarnContext(ctx, "forge scope enforcement is on with no grants declared at boot; agent forge writes need a matching store grant")
	}
	return nil
}

// registerGitHubForgeCoordinate registers the production GitHub write coordinate
// as the default for a nil or unset ForgeRef. The author and reviewer providers
// use distinct GitHub App identities so the reviewing App cannot be the author.
func registerGitHubForgeCoordinate(reg *forgeProviderRegistry, fc ForgeConfig, author, reviewer forge.Provider) error {
	return reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: fc.Host}, author, reviewer, true)
}

func installationErrorCode(err error) connect.Code {
	if errors.Is(err, forge.ErrAppCredentials) {
		return connect.CodeInternal
	}
	if statusErr, ok := errors.AsType[*forge.StatusError](err); ok {
		if statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden {
			return connect.CodeInternal
		}
	}
	return connect.CodeUnavailable
}

// registerLinearForgeCoordinate registers the Linear write coordinate. It must
// carry forge.LinearHost: the DL-055 row rejects an empty host. One client
// serves both roles (Linear has no author/reviewer split).
func registerLinearForgeCoordinate(reg *forgeProviderRegistry, linear forge.Provider) error {
	return reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, host: forge.LinearHost}, linear, linear, false)
}

// buildLinearTokenSource returns nil when neither client-credential name is set;
// a partial pair warns and stays off. With both names declared, it builds and
// boot-checks one shared app-actor token source for notify and write lanes.
func buildLinearTokenSource(ctx context.Context, cfg ServeConfig, resolver secrets.Resolver, log *slog.Logger) (*linearagent.TokenSource, error) {
	fc := cfg.Forge.resolved()
	declared, err := resolver.Resolve(ctx, "forge linear")
	if err != nil {
		return nil, fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	var clientID, clientSecret string
	// Compare SERVER-prefixed names; keep the warning's names unprefixed for operators.
	wantID, wantSecret := serverSecretName(fc.LinearClientIDSecretName), serverSecretName(fc.LinearClientSecretName)
	for _, s := range declared {
		switch s.Name {
		case wantID:
			clientID = s.Value
		case wantSecret:
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
		log.WarnContext(ctx, "forge Linear lanes disabled: only one of the two Linear client-credential secrets is declared; both are required",
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

// forgeSecretDeclared checks the optional secret in the SERVER resolver; faults
// fail startup, while absence is a valid off-state for Linear.
// Compare the prefixed registry name but keep the operator-facing name unprefixed.
// An unwrapped comparison silently leaves the Linear webhook route unmounted.
func forgeSecretDeclared(ctx context.Context, resolver secrets.Resolver, name string) (bool, error) {
	resolved, err := resolver.Resolve(ctx, "forge write")
	if err != nil {
		return false, fmt.Errorf("forge secret resolve failed at startup: %w", err)
	}
	want := serverSecretName(name)
	for _, s := range resolved {
		if s.Name == want {
			return true, nil
		}
	}
	return false, nil
}

// reconcileForgeSeed inserts missing seed repos as enabled targets and leaves
// existing rows unchanged, including disabled rows. It validates and lowercases
// owner/name so case variants do not create duplicate keys.
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

// warnDisabledBoardIngestion warns when enabled targets exist for this coordinate
// but the GitHub App is absent. It never enables ingestion or fails startup.
func warnDisabledBoardIngestion(ctx context.Context, st *store.Store, provider store.ForgeProvider, host string, log *slog.Logger) {
	enabled, err := st.ListEnabledForgeRepoSubscriptions(ctx, provider, host)
	if err != nil {
		log.ErrorContext(ctx, "board ingestion: list enabled targets at boot", "err", err)
		return
	}
	if len(enabled) == 0 {
		return
	}
	log.WarnContext(ctx, "board ingestion disabled (no GitHub App configured) but enabled targets exist; set --forge-app-id and the App secrets",
		"targets", len(enabled), "forge_host", host)
}

// fanoutSink forwards each accepted webhook event to all registered sinks.
// Enqueue stays non-blocking because each sink's Enqueue is non-blocking.
type fanoutSink struct{ sinks []ForgeEventSink }

// Enqueue forwards the event to every registered sink.
func (f *fanoutSink) Enqueue(ctx context.Context, ev forge.ForgeEvent) {
	for _, s := range f.sinks {
		s.Enqueue(ctx, ev)
	}
}

// warnPartialForgeWriteSecrets warns when exactly one required App is configured,
// since writes would otherwise fail closed with no startup explanation.
// Both absent is the intentional off-state; the warning names roles, never secrets.
func warnPartialForgeWriteSecrets(ctx context.Context, fc ForgeConfig, declared []secrets.ResolvedSecret, log *slog.Logger) {
	havePrimary, haveReviewer := fc.forgeWriteAppsConfigured(declared)
	if havePrimary == haveReviewer {
		return // both configured (enabled path, not here) or both absent (intentional off)
	}
	configured, missing := "primary App", "reviewer App"
	if haveReviewer {
		configured, missing = "reviewer App", "primary App"
	}
	log.WarnContext(ctx, "forge write path disabled: only one of the two required GitHub Apps is configured; both are required",
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

// openFabricAndStore connects the event fabric, then opens the store, so an
// unreachable broker fails before migrations run. A store failure closes the fabric.
func openFabricAndStore(ctx context.Context, cfg ServeConfig) (*fabric.Fabric, *store.Store, error) {
	fab, err := fabric.New(ctx, fabric.Config{URL: cfg.NatsURL})
	if err != nil {
		return nil, nil, fmt.Errorf("connecting event fabric: %w", err)
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		closeFabric(ctx, fab)
		return nil, nil, err
	}
	return fab, st, nil
}

// closeFabric drains the event fabric on shutdown. Serve has already chosen its
// return value by then, so a failed drain is logged, not returned.
func closeFabric(ctx context.Context, fab *fabric.Fabric) {
	// Runs after shutdown cancels ctx; keep its values, drop the cancel.
	ctx = context.WithoutCancel(ctx)
	if err := fab.Close(ctx); err != nil {
		slog.Default().WarnContext(ctx, "draining event fabric on shutdown", "error", err)
	}
}

func closeListener(l net.Listener) {
	if l != nil {
		l.Close() //nolint:errcheck,gosec // best-effort listener close on teardown — nothing actionable remains (errcheck + its gosec G104 twin)
	}
}

// listenerAddr returns l's bound address, or nil for a door that is off.
func listenerAddr(l net.Listener) net.Addr {
	if l == nil {
		return nil
	}
	return l.Addr()
}
func configureVCSMirror(ctx context.Context, cfg *ServeConfig, resolver secrets.Resolver, udsListener net.Listener, listeners boundListeners) error {
	if cfg.VCSMirrorDir == "" {
		return nil
	}
	if cfg.VCSMirrorRepoCapBytes <= 0 || cfg.VCSMirrorTotalCapBytes <= 0 || cfg.VCSMirrorRepoCapBytes > cfg.VCSMirrorTotalCapBytes {
		return failStartup(udsListener, listeners, errors.New("vcs mirror requires positive per-repository and total byte caps, with per-repository no larger than total"))
	}
	if cfg.Forge.App.AppID == 0 {
		return failStartup(udsListener, listeners, errors.New("vcs mirror requires the primary github app"))
	}
	if err := validateForgeSecret(ctx, resolver, "vcs mirror app key", cfg.Forge.App.AppPrivateKeySecret); err != nil {
		return failStartup(udsListener, listeners, err)
	}
	mirrorHTTP, err := forgeHTTPClient(cfg.Forge.ForgeCAPath)
	if err != nil {
		return failStartup(udsListener, listeners, err)
	}
	tokens, err := forge.NewAppTokenSource(forge.GitHubAppConfig{
		AppID:          cfg.Forge.App.AppID,
		InstallationID: cfg.Forge.App.InstallationID,
		PrivateKey:     resolveDeclaredSecret(resolver, cfg.Forge.App.AppPrivateKeySecret, "vcs mirror app"),
		Host:           cfg.Forge.resolved().Host,
		Client:         mirrorHTTP,
	})
	if err != nil {
		return failStartup(udsListener, listeners, fmt.Errorf("vcs mirror app token source: %w", err))
	}
	cfg.Mirror = forge.NewGitMirrorWithHost(cfg.VCSMirrorDir, cfg.Forge.resolved().Host, cfg.VCSMirrorRepoCapBytes, cfg.VCSMirrorTotalCapBytes, func(ctx context.Context, _ string) (string, error) {
		return tokens.Token(ctx)
	})
	return nil
}
