//go:build podman

package e2e

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/internal/stack"
	"github.com/sealedsecurity/compass/go/internal/stack/adapters"
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
// fixture (no canned model); WithCannedModel turns on the SEA-1787 H3 backend.
type fixtureConfig struct {
	canned       bool
	cannedScript []CannedTurn
	// site, when non-nil, makes NewFixture reuse a persistent root/stateDir/ports
	// (WithSite) instead of minting fresh ephemeral ones — the SEA-1790 H6
	// cross-restart substrate. nil is the default ephemeral fixture.
	site *fixtureSite
}

// fixtureOption mutates a fixtureConfig. Variadic options keep NewFixture's
// existing two-arg call sites (H2's primitives test) byte-identical while
// letting the real-turn test opt into canned mode.
type fixtureOption func(*fixtureConfig)

// WithCannedModel makes NewFixture stand up the deterministic canned model
// backend (SEA-1787 H3) with a single pure-text turn: it starts the stub SSE
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
// ordered multi-turn script (SEA-1788 H4): the agent settles request N on
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

// WithSite makes NewFixture reuse a persistent site (root/stateDir/ports) rather
// than minting fresh ephemeral ones — the SEA-1790 H6 cross-restart substrate.
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
// WithCannedModel to stand up the SEA-1787 H3 deterministic model backend so a
// real agent turn can settle with no live-model egress.
func NewFixture(ctx context.Context, t *testing.T, opts ...fixtureOption) *Fixture {
	t.Helper()

	var fc fixtureConfig
	for _, opt := range opts {
		opt(&fc)
	}
	// Compile the three stack child binaries from the module root and put them on
	// PATH: the ProcessSupervisor resolves each Component to a bare binary name
	// via exec.LookPath, so the stack only stands up if they are found.
	binDir := buildBinariesFromModuleRoot(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

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
	}

	// Canned-model mode (SEA-1787 H3): stand up the deterministic stub, write a
	// models.yml pointing the agent's custom openai-completions provider at it,
	// and pin the three A4 knobs so the agent resolves that provider and its
	// default-deny egress permits exactly the stub. Overrides the illustrative
	// AgentModel/EgressAllow above (which only prove the forward path); a nil
	// stub means plain mode and every canned field stays as set above.
	var stub *cannedModelServer
	if fc.canned {
		stub = configureCannedModel(t, &cfg, root, fc.cannedScript)
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
func configureCannedModel(t *testing.T, cfg *stack.Config, cfgRoot string, script []CannedTurn) *cannedModelServer {
	t.Helper()

	hostAddr, err := hostRoutableAddr()
	if err != nil {
		t.Fatalf("resolve host routable address for canned model: %v", err)
	}
	stub, err := startCannedModelServer(hostAddr+":0", script)
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

// buildBinariesFromModuleRoot compiles the three stack child binaries from the
// module root into a temp dir and returns it. This package lives at go/e2e, so
// the module root (the dir holding go.mod) is ONE `..` up — verified by the
// go.mod check below, which fails legibly if the layout ever moves.
func buildBinariesFromModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	moduleRoot := filepath.Join(wd, "..") // go/e2e -> go
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil {
		t.Fatalf("module root %q has no go.mod (layout changed?): %v", moduleRoot, err)
	}
	binDir := t.TempDir()
	for _, name := range []string{"compass-postgres", "compass-server", "compass-runner"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, name), "./cmd/"+name)
		cmd.Dir = moduleRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
	}
	return binDir
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
// WithSite. The SEA-1790 H6 cross-restart leg is its only user.
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
