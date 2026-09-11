//go:build podman

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec" //nolint:depguard // e2e harness: LookPath-resolved stack child binaries + podman image probe
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/stack"
	"github.com/RigelBuild/compass/go/internal/stack/adapters"
	"github.com/RigelBuild/compass/go/internal/store"
)

// agentImage is the REAL agent image the dogfood stack runs — present in the
// local containers-storage on the dev/CI box, never a public stand-in. The
// runner refuses to boot without it present; the stack pulls/validates it but
// does not run it as a container at up (a per-agent container is on-demand via a
// later RPC, out of H1 scope).
const agentImage = "compass-agent:latest"

// expectedVersion is the stack build version the fixture drives. It only gates
// the attach-if-live path (a version mismatch there is ErrVersionMismatch); a
// fresh spawn never compares it, so any non-empty value is fine for the fixture,
// which always spawns a fresh stack under its own short root.
const expectedVersion = "e2e-test"

// Fixture is one test's live embedded stack plus the authenticated clients and
// store handle the harness legs consume. It is produced by NewFixture, which
// registers teardown on the test, so a consumer never manages the stack's
// lifecycle directly.
type Fixture struct {
	compass   compassServiceClient
	comms     commsServiceClient
	stack     *stack.Stack
	dsn       string
	caPath    string
	serverURL string
	// adminToken is the bootstrap-admin bearer read from disk at Up (the same
	// credential the authed clients carry). Retained so a client-mode leg can
	// build its own bridge target bearer against the real door; exposed via
	// AdminToken(). Never logged.
	adminToken string
	// runtimeDir is this fixture's unique runner runtime-dir (shortRoot/rt),
	// forwarded to the runner as --runtime-dir. Exposed so a process-table
	// assertion can scope its match to this fixture's own runner.
	runtimeDir string
	// stub is the canned model backend when the fixture was built with
	// WithCannedModel, else nil. Its lifecycle rides a t.Cleanup registered at
	// startup, so a consumer never closes it directly.
	stub *cannedModelServer
	// now is the injectable wall-clock for the enrollment-readiness poll;
	// defaults to time.Now. A test overrides it to drive the budget-timeout
	// branch of waitRunnerEnrolled — the enrollment counterpart to the stack's
	// s.deps.now() seam.
	now func() time.Time
}

// fixtureConfig holds the optional knobs a caller flips through fixtureOption
// before NewFixture stands the stack up. The zero value is the plain H1/H2
// fixture (no canned model); WithCannedModel turns on the RIG-1787 H3 backend.
type fixtureConfig struct {
	canned       bool
	cannedScript []CannedTurn
	// cannedMarkers are caller-supplied off-script body-marker routes threaded
	// into the canned backend (WithCannedMarkerReply): a request whose body carries a
	// marker settles on its reply without consuming a positional script slot,
	// additive to the built-in Setup marker. Empty is the default (Setup marker
	// only).
	cannedMarkers []cannedMarker
	// site, when non-nil, makes NewFixture reuse a persistent root/stateDir/ports
	// (WithSite) instead of minting fresh ephemeral ones — the RIG-1790 H6
	// cross-restart substrate. nil is the default ephemeral fixture.
	site *fixtureSite
	// onUp, when non-nil, receives the live stack immediately after a successful
	// Up (WithStackObserver), so a caller with a detached t can still reap the
	// children if a later construction gate aborts. nil is the default.
	onUp func(*stack.Stack)
}

// fixtureOption mutates a fixtureConfig. Variadic options keep NewFixture's
// existing two-arg call sites (H2's primitives test) byte-identical while
// letting the real-turn test opt into canned mode.
type fixtureOption func(*fixtureConfig)

// WithCannedModel makes NewFixture stand up the deterministic canned model
// backend (RIG-1787 H3) with a single pure-text turn: it starts the stub SSE
// server on the host's routable interface, writes a models.yml custom
// openai-completions provider pointing at it (through the pasta host-gateway)
// into a host dir bind-mounted at the agent's ~/.omp/agent, and pins the
// fixture's AgentModel/EgressAllow so the agent resolves that provider and its
// default-deny egress permits exactly the stub. reply is the assistant text the
// single scripted turn settles on. For a multi-turn script (H4), use
// WithCannedScript.
func WithCannedModel(reply string) fixtureOption {
	return func(fc *fixtureConfig) {
		fc.canned = true
		fc.cannedScript = []CannedTurn{CannedText(reply)}
	}
}

// WithCannedScript makes NewFixture stand up the canned model backend serving an
// ordered multi-turn script (RIG-1788 H4): the agent settles request N on
// script[N], so a multi-round scenario (e.g. a tool-call turn then a closing
// text turn) advances one scripted turn per model round-trip. It shares the same
// underlying backend as WithCannedModel — the single-turn convenience is just a
// one-CannedText script.
func WithCannedScript(script ...CannedTurn) fixtureOption {
	return func(fc *fixtureConfig) {
		fc.canned = true
		fc.cannedScript = script
	}
}

// WithCannedMarkerReply adds an off-script body-marker route to the canned
// backend (newCannedMarker): a model request whose body contains marker
// settles on reply as a clean text turn WITHOUT advancing the positional script
// counter. It is additive to the built-in Setup marker and composes with
// WithCannedScript — a leg routes a shared-backend turn it does not want drawn
// off its ordered script (the leg-4 mention-driven steer and deliver turns) the
// same way the root-supervisor Setup turn is already routed. Repeat the option
// to register several markers.
func WithCannedMarkerReply(marker, reply string) fixtureOption {
	return func(fc *fixtureConfig) {
		fc.cannedMarkers = append(fc.cannedMarkers, newCannedMarker(marker, reply))
	}
}

// WithCannedMarkerScript adds an off-script body-marker route serving an ORDERED
// SEQUENCE of turns (newCannedMarkerScript, RIG-3528 T1) rather than the single
// text reply WithCannedMarkerReply registers: matching request N of this marker
// draws turns[N], and the terminal element repeats once the sequence is
// exhausted. Like WithCannedMarkerReply it never advances the positional script
// counter, so it composes with WithCannedScript and with the built-in Setup
// marker; repeat the option to register several marker scripts.
//
// This is what lets a marker-routed turn issue a TOOL CALL: a tool-call turn
// needs two model round-trips to settle (the call, then the follow-up that
// settles on text), and the follow-up re-matches the same marker — so a
// single-turn marker route would re-serve the tool call forever. Pass
// [CannedToolCall(...), CannedText(...)] and the second round-trip settles.
func WithCannedMarkerScript(marker string, turns ...CannedTurn) fixtureOption {
	return func(fc *fixtureConfig) {
		fc.cannedMarkers = append(fc.cannedMarkers, newCannedMarkerScript(marker, turns...))
	}
}

// WithSite makes NewFixture reuse a persistent site (root/stateDir/ports) rather
// than minting fresh ephemeral ones — the RIG-1790 H6 cross-restart substrate.
// Two NewFixture calls over the SAME site drive two stack lifecycles that share
// the postgres data dir (under stateDir), so the second Up re-attaches the
// persisted cluster and the same handle resolves to the same account. The site's
// lifecycle (its RemoveAll) is owned by newPersistentSite's t.Cleanup, so
// NewFixture registers only the per-Up Down, never a root RemoveAll, on this
// path. Absent this option NewFixture behaves exactly as before.
func WithSite(site fixtureSite) fixtureOption {
	return func(fc *fixtureConfig) {
		fc.site = &site
	}
}

// WithStackObserver hands the live stack to onUp the moment Up succeeds, before
// any later gate can abort construction. A caller whose t is detached — whose
// t.Cleanup Down is therefore unreachable — needs this to reap the children when
// a post-Up gate t.Fatalf's, since NewFixture returns its *Fixture only at the
// very end and a Goexit means it never returns at all.
func WithStackObserver(onUp func(*stack.Stack)) fixtureOption {
	return func(fc *fixtureConfig) {
		fc.onUp = onUp
	}
}

// Compass is the authenticated CompassService client dialed at the loopback TLS
// door with the admin bearer.
func (f *Fixture) Compass() compassServiceClient { return f.compass }

// Comms is the authenticated CommsService client dialed at the same door.
func (f *Fixture) Comms() commsServiceClient { return f.comms }

// Stack is the live *stack.Stack handle (Health, Down) the fixture stood up.
func (f *Fixture) Stack() *stack.Stack { return f.stack }

// DSN is the private-postgres keyword/value DSN for store-side assertions.
func (f *Fixture) DSN() string { return f.dsn }

// ServerURL is the https loopback TLS-door base URL the stack listens on — the
// server_url a native client-mode connection dials. Exposed for a client-mode
// leg that builds its own bridge target against the real door.
func (f *Fixture) ServerURL() string { return f.serverURL }

// CAPath is the filesystem path to the stack's self-signed TLS anchor
// (StateDir/tls.crt) — the ca_cert a native client-mode connection pins.
func (f *Fixture) CAPath() string { return f.caPath }

// AdminToken is the bootstrap-admin bearer the stack minted at Up. Exposed for a
// client-mode leg that arms its own bridge target; it is the same credential the
// authed clients carry. Never log it.
func (f *Fixture) AdminToken() string { return f.adminToken }

// RuntimeDir is this fixture's unique runner runtime-dir (shortRoot/rt). Exposed
// so a process-hygiene assertion can scope its /proc scan to this fixture's own
// child processes rather than matching unrelated host processes.
func (f *Fixture) RuntimeDir() string { return f.runtimeDir }

// AsObserver mints a NON-ADMIN bearer for an existing account and returns the
// two Connect clients scoped to it, so a leg can assert what that account CAN
// and CANNOT see over the real TLS door (RIG-3528 T1). Every other fixture RPC
// rides the bootstrap-admin bearer (newAuthedClients), which is why no existing
// leg can prove a NEGATIVE — an admin sees everything.
//
// It mints a CLIENT/observer credential, NOT an agent identity. Agent
// authorship needs no credential at all: the Runner asserts no account and the
// server resolves session_id → account from its own binding
// (runnerhub/relay_comms.go:7-15, the ratified OQ-2 trust model). Do not reach
// for this to author an agent's post — script the agent's turn instead.
//
// IssueToken is admin-gated (server/service.go:407-415), so the mint rides the
// fixture's admin client; the returned clients then dial the SAME door through
// newAuthedClients with the minted bearer, so there is exactly one dial path.
// An account the server cannot resolve is NOT_FOUND, surfaced as the returned
// error (never a panic — the caller, a test, decides fatality).
//
// The argument accepts EITHER spelling — an account id or a handle — because the
// two surfaces disagree: IssueTokenRequest.account_handle is documented as a
// handle (compass.proto:758-763) but the server consumes it as an account ID
// (service.go:420-425 feeds store.AccountID(...) straight into GetAccount, which
// keys on accounts.id), while CommsService's member/owner fields resolve strictly
// through account_handles.handle. So this resolves the ref to an id over
// ListAccounts first. An unresolvable ref is passed through UNCHANGED so the
// SERVER decides the code — that keeps NOT_FOUND the server's answer rather than
// a locally-synthesized one.
func (f *Fixture) AsObserver(ctx context.Context, handle string) (compassServiceClient, commsServiceClient, error) {
	// Best-effort id resolution; a miss (unknown ref, or a list error) leaves the
	// caller's spelling intact for the server to reject.
	target := handle
	if acc, err := f.lookupAccount(ctx, handle); err == nil {
		target = acc.GetId()
	}
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().IssueToken(rctx, connect.NewRequest(&compassv1.IssueTokenRequest{
		AccountHandle: target,
	}))
	if err != nil {
		return nil, nil, fmt.Errorf("IssueToken RPC: %w", err)
	}
	token := resp.Msg.GetToken()
	if token == "" {
		return nil, nil, fmt.Errorf("IssueToken for %q returned an empty token", handle)
	}
	compass, comms, err := newAuthedClients(f.caPath, f.serverURL, token)
	if err != nil {
		return nil, nil, fmt.Errorf("observer clients for %q: %w", handle, err)
	}
	return compass, comms, nil
}

// lookupAccount resolves an account ref — an id OR a handle — to its Account
// over ListAccounts (the only account read CommsService exposes; there is no
// GetAccount RPC). It exists because the id/handle spelling required differs per
// request field (see AsObserver), so a fixture wrapper taking one spelling has to
// be able to reach the other. An unmatched ref is store-shaped ErrNotFound-like:
// a plain error naming the ref, for the caller to wrap or ignore.
func (f *Fixture) lookupAccount(ctx context.Context, ref string) (*compassv1.Account, error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().ListAccounts(rctx, connect.NewRequest(&compassv1.ListAccountsRequest{}))
	if err != nil {
		return nil, fmt.Errorf("ListAccounts RPC: %w", err)
	}
	for _, acc := range resp.Msg.GetAccounts() {
		if acc.GetId() == ref || acc.GetHandle() == ref {
			return acc, nil
		}
	}
	return nil, fmt.Errorf("no visible account matching %q (by id or handle)", ref)
}

// CreateUser creates a human user account over CommsService and returns its
// account id — the owner-tier setup primitive a multi-tenant leg needs (two
// owner users, each with its own agents). Thin client-RPC primitive in the style
// of CreateAgent; returns an error rather than panicking so the caller (a test)
// decides fatality, and the per-call deadline is threaded from ctx.
func (f *Fixture) CreateUser(ctx context.Context, handle, displayName string) (ownerID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().CreateUser(rctx, connect.NewRequest(&compassv1.CreateUserRequest{
		Handle:      handle,
		DisplayName: displayName,
	}))
	if err != nil {
		return "", fmt.Errorf("CreateUser RPC: %w", err)
	}
	return resp.Msg.GetAccount().GetId(), nil
}

// CreateChannel creates a plain (kind=CHANNEL) channel over CommsService with
// ownerID as a founding member, and returns its channel id.
//
// private selects the channel's D9 VISIBILITY, which in this schema is a
// property of the channel's GROUP, not of ChannelKind — the ChannelKind enum is
// CHANNEL / DM / (retired) GROUP_DM and carries no private member
// (comms.proto:289-295), and a DM is a two-party conversation the manual create
// path is server-FORBIDDEN from minting (store/channels.go:126-139), so it is
// not the private form of a channel. Both cases are therefore
// CHANNEL_KIND_CHANNEL and differ in placement:
//   - private=true → UNGROUPED (empty group_id): membership-only visibility.
//     comms.proto:237-239 ("empty for an ungrouped channel, which is
//     owner-scoped to its creating caller (the OWNER default), not global"), and
//     the read predicate agrees — its group arm requires group_id NOT NULL with
//     effective visibility SHARED (store/db/channels.sql.go:495-505), so an
//     ungrouped channel is reachable only through channel_members.
//   - private=false → created inside a freshly minted SHARED channel group, so
//     every account can see it (the globally-visible canary surface).
//
// ownerID is threaded as a MEMBER, not an owner field: CreateChannelRequest has
// NO owner field (name/group_id/kind/member_handles, comms.proto:654-664) and the
// store derives owner scoping from the CALLER plus transitive owner-membership
// (store/channels.go:76-83) — a user is added automatically for any of its agents
// in the member set. Membership is what makes the channel readable by that
// account, which is the property a leg asserts. The creating caller is the
// fixture's admin client, so the admin is a founding member by construction.
//
// member_handles resolves strictly through account_handles.handle (an account id
// never resolves — store/db/accounts.sql.go:248-265), while the signature takes
// an id, so the id is converted to its handle first via lookupAccount. An
// unresolvable ownerID is passed through unchanged so the SERVER answers
// NOT_FOUND rather than a locally-synthesized error.
func (f *Fixture) CreateChannel(ctx context.Context, ownerID, name string, private bool) (channelID string, err error) {
	ownerHandle := ownerID
	if acc, lookupErr := f.lookupAccount(ctx, ownerID); lookupErr == nil {
		ownerHandle = acc.GetHandle()
	}
	var groupID string
	if !private {
		groupID, err = f.createSharedGroup(ctx, name+"-group")
		if err != nil {
			return "", err
		}
	}
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().CreateChannel(rctx, connect.NewRequest(&compassv1.CreateChannelRequest{
		Name:          name,
		GroupId:       groupID,
		Kind:          compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{ownerHandle},
	}))
	if err != nil {
		return "", fmt.Errorf("CreateChannel RPC: %w", err)
	}
	return resp.Msg.GetChannel().GetId(), nil
}

// createSharedGroup mints a top-level SHARED-visibility channel group and
// returns its id — the container that makes a channel globally visible (see
// CreateChannel's private=false arm). Top-level, so no parent visibility
// ceiling applies (store/channels.go:13-22).
func (f *Fixture) createSharedGroup(ctx context.Context, name string) (groupID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().CreateChannelGroup(rctx, connect.NewRequest(&compassv1.CreateChannelGroupRequest{
		Name:       name,
		Visibility: compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_SHARED,
	}))
	if err != nil {
		return "", fmt.Errorf("CreateChannelGroup RPC: %w", err)
	}
	return resp.Msg.GetGroup().GetId(), nil
}

// NewFixture stands up the real embedded stack over stack.Up with the real
// adapter set and returns a Fixture with authenticated Connect clients. It
// registers a t.Cleanup that Downs the stack (safe to call twice), so a t.Fatal
// after Up still drains the children. ctx is the caller's context, threaded into
// Up and the teardown Down — the fixture mints no context of its own.
//
// A container-less sandbox is handled by the caller's podmanUsable() skip-guard
// before NewFixture is reached; here podman and the real image are assumed
// present.
//
// opts default to none — NewFixture(ctx, t) is the plain H1/H2 fixture. Pass
// WithCannedModel to stand up the RIG-1787 H3 deterministic model backend so a
// real agent turn can settle with no live-model egress.
func NewFixture(ctx context.Context, t *testing.T, opts ...fixtureOption) *Fixture {
	t.Helper()

	var fc fixtureConfig
	for _, opt := range opts {
		opt(&fc)
	}
	// The three stack child binaries are built once per run by TestMain and
	// exported on PATH there (see main_test.go); the ProcessSupervisor resolves
	// each Component to a bare binary name via exec.LookPath against that entry.

	// Acquire the root/stateDir/ports either fresh (the default ephemeral
	// fixture) or from a persistent site (WithSite — the H6 cross-restart
	// substrate). The site path reuses one root/stateDir/ports across two Ups so
	// the second re-attaches the persisted postgres cluster; the ephemeral path
	// mints per-call state exactly as before. Only the acquisition differs — the
	// downstream cfg build is shared.
	var root, stateDir string
	var listenPort, pgPort int
	if fc.site != nil {
		root = fc.site.root
		stateDir = fc.site.stateDir
		listenPort, pgPort = fc.site.listenPort, fc.site.pgPort
	} else {
		// shortRoot registers its own RemoveAll on t.Cleanup; the site path must
		// NOT (else run1's cleanup would delete the persisted DB before run2), so
		// newPersistentSite owns the site's single end-of-test RemoveAll instead.
		root = shortRoot(t, "h1")
		stateDir = t.TempDir() // TLS anchor (tls.crt/tls.key) + postgres data dir; not sun_path-budgeted
		ports := freePorts(t, 2)
		listenPort, pgPort = ports[0], ports[1]
	}
	pgSockDir := filepath.Join(root, "pg")
	runtimeDir := filepath.Join(root, "rt")
	serverSock := filepath.Join(root, "s.sock")
	if err := os.MkdirAll(pgSockDir, 0o700); err != nil {
		t.Fatalf("mkdir pg sock dir: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}

	// The DSN host is the socket DIRECTORY postgres -k listens on (libpq unix
	// convention); the postgres wrapper creates it and binds
	// SocketDir/.s.PGSQL.<port>.
	dsn := "host=" + pgSockDir + " port=" + strconv.Itoa(pgPort) + " dbname=compass sslmode=disable"

	cfg := stack.Config{
		StateDir:    stateDir, // TLS anchor (tls.crt/tls.key) + postgres data dir; not sun_path-budgeted
		SocketPath:  serverSock,
		ListenAddr:  "127.0.0.1:" + strconv.Itoa(listenPort),
		DatabaseDSN: dsn,
		AgentImage:  agentImage,
		RuntimeDir:  runtimeDir,
		// The A4 plumbing under test: non-zero values so the green case proves
		// they reach the runner's flags (asserted deterministically in the
		// runnerSpec unit test; here they exercise the real forward path).
		AgentModel:  "anthropic/claude-opus",
		EgressAllow: []string{"api.anthropic.com", "10.0.0.1"},
		// The real compass-agent image ships /workspace (the runner's default
		// checkout dir) non-writable — only $HOME is agent-owned — so Provision's
		// in-container `mkdir` of the checkout dir fails there. Anchor the checkout
		// under $HOME so every leg that Provisions is launchable against the real
		// image without a production or image change (mirrors
		// runner/config_delivery_e2e_test.go).
		CheckoutDir: "/home/agent/repo",
		// The bundled Plane-B fan-in collector is a container-only component (no
		// light/process path, unlike postgres above which the fixture runs via the
		// ProcessSupervisor). This headless stack emits no OTLP, so a running
		// collector would only drop-sink an empty stream — pure CI cost for zero
		// assertions; the real collector container start/teardown/readiness is
		// covered by the podman-guarded collector_container / collector_podman
		// tests. Opt out via the --otel-external switch, mirroring the fixture's
		// light-postgres choice, so spawnChain skips startCollector entirely rather
		// than dereferencing the (deliberately unwired) CollectorContainer seam.
		ExternalOTLPEndpoint: "127.0.0.1:4317",
		// This headless stack connects to no broker — the server/runner NATS
		// cutover is a later slice — so a bundled NATS would be pure CI cost
		// (and the e2e deps below wire no NatsContainer/NatsProber, so the
		// bundle path would hit the nil-dep error next). Opt out via the
		// --nats-external switch, mirroring the collector choice above, so
		// spawnChain skips startNats entirely.
		ExternalNatsURL: "nats://127.0.0.1:4222",
	}

	// Canned-model mode (RIG-1787 H3): stand up the deterministic stub, write a
	// models.yml pointing the agent's custom openai-completions provider at it,
	// and pin the three A4 knobs so the agent resolves that provider and its
	// default-deny egress permits exactly the stub. Overrides the illustrative
	// AgentModel/EgressAllow above (which only prove the forward path); a nil
	// stub means plain mode and every canned field stays as set above.
	var stub *cannedModelServer
	if fc.canned {
		stub = configureCannedModel(t, &cfg, root, fc.cannedScript, fc.cannedMarkers)
	}

	deps := stack.Deps{
		Supervisor:      adapters.NewProcessSupervisor(),
		Certs:           adapters.NewCertEnsurer(0), // 0 -> DefaultRotateWindow
		Tokens:          adapters.NewTokenEnsurer(cfg.DatabaseDSN),
		Images:          adapters.NewImageEnsurer(),
		Prober:          adapters.NewHealthProber(),
		DBProber:        adapters.NewDBProber(),
		Now:             time.Now,
		ExpectedVersion: expectedVersion,
	}

	st, err := stack.Up(ctx, cfg, deps)
	if err != nil {
		t.Fatalf("stack.Up: %v", err)
	}
	// Register teardown immediately after a successful Up so a later t.Fatal still
	// drains the children. Down is safe to call twice; the happy-path test asserts
	// Down's outcome explicitly, so this guard only covers a failed/panicked test.
	t.Cleanup(func() {
		_ = st.Down(ctx) // best-effort teardown guard; a Down error here is not actionable during cleanup
	})
	// A detached-t caller's cleanup above is unreachable, so hand it the live
	// stack here — the only point that is both after a successful Up and before
	// any gate that can Goexit without ever returning a *Fixture.
	if fc.onUp != nil {
		fc.onUp(st)
	}

	// The TLS anchor lives under StateDir (cert.go: tls.crt/tls.key). The
	// bootstrap-admin token is written by the network door under the server
	// SOCKET's parent dir (serve.go defaults StateDir to parentDir(SocketPath)),
	// which is `root` here — not cfg.StateDir. Ready is the Up postcondition, so
	// the token file exists by the time Up returns; no sleep-poll.
	caPath := filepath.Join(cfg.StateDir, "tls.crt")
	adminTokenPath := filepath.Join(filepath.Dir(serverSock), "admin-token")
	raw, err := os.ReadFile(adminTokenPath)
	if err != nil {
		t.Fatalf("read admin-token file %q: %v", adminTokenPath, err)
	}
	adminToken := strings.TrimSpace(string(raw))
	if adminToken == "" {
		t.Fatalf("admin-token file %q is empty", adminTokenPath)
	}

	serverURL := "https://" + cfg.ListenAddr
	compass, comms, err := newAuthedClients(caPath, serverURL, adminToken)
	if err != nil {
		t.Fatalf("build authed clients: %v", err)
	}

	f := &Fixture{
		compass:    compass,
		comms:      comms,
		stack:      st,
		dsn:        dsn,
		caPath:     caPath,
		serverURL:  serverURL,
		adminToken: adminToken,
		runtimeDir: runtimeDir,
		stub:       stub,
		now:        time.Now,
	}

	// stack.Up returns as soon as the compass-runner CHILD is spawned, but the
	// runner enrolls with the server ASYNCHRONOUSLY over the TLS door AFTER Up
	// returns. A leg that Provisions immediately would otherwise race that
	// enrollment and fail `unavailable: no runner enrolled to serve session`.
	// Gate the fixture's post-Up readiness on the runner being enrolled — the
	// enrollment counterpart to the stack's own waitReady/waitPostgres — so every
	// leg starts against an enrolled runner. Event-gated on a real cross-process
	// signal (an enrollment-gated probe), never a sleep. On the WithSite re-attach
	// path the runner is already enrolled, so the first probe passes immediately.
	if err := f.waitRunnerEnrolled(ctx); err != nil {
		t.Fatalf("wait for runner enrollment: %v", err)
	}

	// The runner is enrolled, but the first-launch root-supervisor seed
	// (server/serve_seed.go) fires on that SAME Sessions-stream attach and drives
	// its own Provision+Start of the supervisor on the hook goroutine. A leg that
	// Provisions the instant this returns would race the seed's in-flight
	// Provision — two cold rootless-podman bring-ups contending on the engine
	// storage lock, overrunning the leg's 30s rpcTimeout under CI load (RIG-2403).
	// Gate on the seed's Provision having recorded its durable placement, so the
	// seed's container work finishes before any leg Provisions and the two run
	// serially. Event-gated on the real cross-process placement row, never a sleep;
	// a short-lived store connection scoped to the gate (the fixture holds none).
	// On the WithSite re-attach path the placement persists from the prior boot, so
	// this passes on the first probe and does not wait on the doomed re-fired seed.
	seedStore, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store for seed-settle gate: %v", err)
	}
	seedErr := f.waitSeedSettled(ctx, seedStore)
	seedStore.Close()
	if seedErr != nil {
		t.Fatalf("wait for root-supervisor seed to settle: %v", seedErr)
	}

	return f
}

// cannedAgentDir is the in-container path the canned models.yml is delivered
// to: the agent user's SDK agent dir ($HOME/.omp/agent, getAgentDir() default),
// where the ModelRegistry auto-discovers models.yml. $HOME is the runner's
// --home-dir (cmd/compass-runner/main.go default /home/agent), so this is
// /home/agent/.omp/agent.
const cannedAgentDir = "/home/agent/.omp/agent"

// cannedProvider / cannedModelID name the custom openai-completions provider the
// canned models.yml declares; cannedSelector is the provider/id form COMPASS_MODEL
// carries, which resolveProviderModelReference matches exactly (model-resolver.ts
// findExactModelReferenceMatch).
const (
	cannedProvider = "cannedci"
	cannedModelID  = "canned"
	cannedSelector = cannedProvider + "/" + cannedModelID
)

// pastaHostGateway is the in-container alias for the host under rootless podman's
// pasta networking (== host.containers.internal). Grounded firsthand on this box
// (podman 5.8.4, rootlessNetworkCmd=pasta): a container reaches a host-side
// listener at this address, NOT at 10.0.2.2 (slirp4netns's gateway) nor at the
// host's own loopback (pasta does not forward 127.0.0.1). It is the address the
// agent's model client dials AND the exact egress-allow entry the firewall must
// permit for that dial to clear default-deny.
const pastaHostGateway = "169.254.1.2"

// configureCannedModel starts the canned stub and rewrites cfg's three A4 knobs
// so a real agent turn settles on the scripted reply with zero live egress:
//   - the stub binds the host's routable interface (pasta forwards a container's
//     host-gateway traffic there; a loopback bind is unreachable);
//   - a models.yml declaring a custom openai-completions provider whose baseUrl
//     is the stub reached THROUGH the pasta host-gateway is written to a host
//     dir and bind-mounted read-write at the agent's ~/.omp/agent (rw, not ro:
//     the SDK writes agent.db / sessions/ / models.db as siblings in that same
//     dir — a ro mount over it would break boot; keep-id maps the host dir owner
//     to the agent uid so the writes land);
//   - AgentModel is the provider/id selector resolving to that entry, and
//     EgressAllow is EXACTLY the host-gateway so default-deny permits only the
//     stub.
//
// It returns the running stub; its Close rides a t.Cleanup so teardown never
// leaks it. cfgRoot is the fixture's short root (the models.yml host dir lives
// under it, short enough to stay clear of any path budget).
func configureCannedModel(t *testing.T, cfg *stack.Config, cfgRoot string, script []CannedTurn, markers []cannedMarker) *cannedModelServer {
	t.Helper()

	hostAddr, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("resolve host routable address for canned model: %v", err)
	}
	stub, err := startCannedModelServer(hostAddr+":0", script, markers...)
	if err != nil {
		t.Fatalf("start canned model server: %v", err)
	}
	t.Cleanup(func() {
		if err := stub.Close(); err != nil {
			t.Errorf("canned model server Close: %v", err)
		}
	})

	// The agent dials the stub through the pasta host-gateway, not the stub's
	// bind address — a NAT sits between container and host.
	baseURL := stub.BaseURL(pastaHostGateway)
	modelsYML := "" +
		"providers:\n" +
		"  " + cannedProvider + ":\n" +
		"    api: openai-completions\n" +
		"    baseUrl: " + baseURL + "\n" +
		"    auth: none\n" +
		"    models:\n" +
		"      - id: " + cannedModelID + "\n"

	agentCfgDir := filepath.Join(cfgRoot, "agentcfg")
	if err := os.MkdirAll(agentCfgDir, 0o700); err != nil {
		t.Fatalf("mkdir canned agent-config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentCfgDir, "models.yml"), []byte(modelsYML), 0o600); err != nil {
		t.Fatalf("write canned models.yml: %v", err)
	}

	cfg.AgentModel = cannedSelector
	cfg.EgressAllow = []string{pastaHostGateway}
	cfg.Mounts = []string{agentCfgDir + ":" + cannedAgentDir}
	return stub
}

// freePorts returns n distinct free TCP ports on loopback by binding :0 on each,
// reading the kernel-assigned port, then closing — the only way to a fixed port
// Config.Validate accepts (it rejects :0; there is no bound-address discovery
// API). All listeners are held open until every port is read so the kernel
// cannot hand the same port twice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	lns := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		if err := ln.Close(); err != nil {
			t.Fatalf("release reserved port: %v", err)
		}
	}
	return ports
}

// shortRoot creates a short, unique, 0700 root under /tmp for one test and
// registers its RemoveAll. Short because everything sun_path-budgeted lives
// under it (pg socket dir, runtime dir, server socket); unique off os.Getpid()
// plus suffix so nothing collides with a concurrent or crashed run.
func shortRoot(t *testing.T, suffix string) string {
	t.Helper()
	root := filepath.Join("/tmp", "ce"+strconv.Itoa(os.Getpid())+suffix)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir short root: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root) // best-effort: the state here is this test's alone and its Down has drained the children
	})
	return root
}

// fixtureSite is a persistent stack substrate — the root, state dir, and two
// ports — that outlives a single NewFixture call so two Ups (WithSite) can share
// it: the postgres data dir lives under stateDir, so the second Up re-attaches
// the cluster the first initialized. Produced by newPersistentSite, consumed via
// WithSite. The RIG-1790 H6 cross-restart leg is its only user.
type fixtureSite struct {
	root       string
	stateDir   string
	listenPort int
	pgPort     int
}

// newPersistentSite mints a persistent site whose lifetime spans a whole test —
// two back-to-back Ups over it re-attach the same postgres cluster. It registers
// exactly ONE end-of-test RemoveAll for the root (not per-Up), so run1's Down
// cannot delete the persisted DB before run2; the state dir is a t.TempDir (the
// framework reaps it after the test). The root stays short off shortRoot because
// the runner's agent-socket path under it is sun_path-budgeted (run.go
// validateRuntimeDir); the state dir is not budgeted, so a t.TempDir is fine
// there. The two ports are allocated ONCE — run1's Down closes its listeners
// before run2's Up rebinds them, so a single freePorts pair serves both.
func newPersistentSite(t *testing.T) fixtureSite {
	t.Helper()
	// A short, unique root — NOT via shortRoot, whose t.Cleanup RemoveAll fires
	// at the enclosing test's end but would be fine either way; the reason to
	// inline it is to keep the site's single RemoveAll here, alongside the rest
	// of the site's lifecycle, rather than split across helpers. suffix "h6"
	// keeps it distinct from an ephemeral fixture's "h1" root in the same test.
	root := filepath.Join("/tmp", "ce"+strconv.Itoa(os.Getpid())+"h6")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir persistent site root: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root) // best-effort end-of-test sweep: the site is this test's alone and both Downs have drained by now
	})
	ports := freePorts(t, 2)
	return fixtureSite{
		root:       root,
		stateDir:   t.TempDir(),
		listenPort: ports[0],
		pgPort:     ports[1],
	}
}

// podmanUsable reports whether rootless podman can run the real agent image
// here. A missing binary or broken rootless setup means SKIP, not fail — a
// container-less sandbox is not a test failure.
func podmanUsable() bool {
	err := exec.Command("podman", "run", "--rm", agentImage, "true").Run()
	return err == nil
}
