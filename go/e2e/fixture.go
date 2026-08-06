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

// NewFixture stands up the real embedded stack over stack.Up with the real
// adapter set and returns a Fixture with authenticated Connect clients. It
// registers a t.Cleanup that Downs the stack (safe to call twice), so a t.Fatal
// after Up still drains the children. ctx is the caller's context, threaded into
// Up and the teardown Down — the fixture mints no context of its own.
//
// A container-less sandbox is handled by the caller's podmanUsable() skip-guard
// before NewFixture is reached; here podman and the real image are assumed
// present.
func NewFixture(ctx context.Context, t *testing.T) *Fixture {
	t.Helper()

	// Compile the three stack child binaries from the module root and put them on
	// PATH: the ProcessSupervisor resolves each Component to a bare binary name
	// via exec.LookPath, so the stack only stands up if they are found.
	binDir := buildBinariesFromModuleRoot(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := shortRoot(t, "h1")
	pgSockDir := filepath.Join(root, "pg")
	runtimeDir := filepath.Join(root, "rt")
	serverSock := filepath.Join(root, "s.sock")
	if err := os.MkdirAll(pgSockDir, 0o700); err != nil {
		t.Fatalf("mkdir pg sock dir: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}

	ports := freePorts(t, 2)
	listenPort, pgPort := ports[0], ports[1]

	// The DSN host is the socket DIRECTORY postgres -k listens on (libpq unix
	// convention); the postgres wrapper creates it and binds
	// SocketDir/.s.PGSQL.<port>.
	dsn := "host=" + pgSockDir + " port=" + strconv.Itoa(pgPort) + " dbname=compass sslmode=disable"

	cfg := stack.Config{
		StateDir:    t.TempDir(), // TLS anchor (tls.crt/tls.key) + postgres data dir; not sun_path-budgeted
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

	return &Fixture{
		compass:   compass,
		comms:     comms,
		stack:     st,
		dsn:       dsn,
		caPath:    caPath,
		serverURL: serverURL,
	}
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

// podmanUsable reports whether rootless podman can run the real agent image
// here. A missing binary or broken rootless setup means SKIP, not fail — a
// container-less sandbox is not a test failure.
func podmanUsable() bool {
	err := exec.Command("podman", "run", "--rm", agentImage, "true").Run()
	return err == nil
}
