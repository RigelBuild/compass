//go:build podman

package main

// End-to-end Linux integration proof for the T2 embedded stack supervisor
// (RIG-1662 / RIG-1683, design.md:437-442). Every other T2 test stubs the
// external effects; THIS one drives the real composition root — resolveConfig +
// buildDeps wiring the six real adapters — against REAL initdb/postgres/podman/
// compass-server/compass-runner, proving the whole embedded stack stands up.
//
// The frozen gate (design.md:437-442): `up` reaches Health=Ready (server
// answering AND the runner spawned with an image present), `down` drains
// cleanly, a second `up` ATTACHES instead of double-spawning, and two concurrent
// `up`s produce exactly one stack.
//
// Build-tagged (`podman`) so it is OUT of the hermetic unit lane — it only runs
// under `-tags podman`. podmanUsable()-guarded so a container-less sandbox skips
// (never fails); wherever rootless podman exists the assertions are real.
//
// PROCESS SAFETY (this runs on a SHARED box; rule://process-safety): the stack
// under test creates NO containers — per-agent containers are on-demand via the
// ProvisionAgentWorkspace RPC, never called here; `up` only spawns the three
// child PROCESSES (compass-postgres, compass-server, compass-runner), and the
// postgres wrapper's own postgres child. Teardown is therefore process-based:
// stack.Down sends SIGTERM to each child's process group (adapters/process.go
// Setpgid), and every stack created registers a best-effort Down guard via
// t.Cleanup so a t.Fatal still drains its children. NEVER pkill/killall — only
// the stack's own Down (or the exact PIDs it owns) ever stops anything.
//
// AGENT IMAGE — alpine, not a real agent image (grounded): the stack pulls
// Config.AgentImage into the local store (adapters/image.go EnsureImage ->
// `podman pull`) and passes it to the runner as --image. It is NOT run as a
// container at up: cmd/compass-runner/main.go run() only validates a podman
// engine fact (the userns-remap version check, main.go:91) then
// runner.Run (internal/runner/run.go:85-122) DIALS the server's TLS door and
// enrolls; a per-agent container is built only later on a ProvisionAgentWorkspace
// RPC. So a pullable stand-in (docker.io/library/alpine:latest) satisfies the
// whole up path; the local-only compass-agent:latest is deliberately avoided (it
// is not pullable, so EnsureImage's real `podman pull` would fail).
//
// STRONGEST RELIABLE ASSERTION — "spawned + Ready" encodes the full chain:
// stack.spawnChain (stack.go:136-185) runs the seven cold-start steps IN ORDER,
// returning nil only after step 7 (start compass-runner) succeeds; Up then hands
// back a non-attached Stack whose Health is Ready. So a spawned Ready stack is
// itself proof that postgres came up, the server answered GetServerInfo, the
// runner token was minted, the agent image was pulled into the store, and the
// runner process was exec'd — the T2 gate, asserted without racing on the
// runner's async enrollment (which happens after Up has returned and does not
// gate readiness).

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// agentImage is a small, pullable public image standing in for the agent image:
// the stack pulls it and hands it to the runner, but never runs it as a
// container at up (see the file header). Same image the runtime lifecycle test
// uses.
const agentImage = "docker.io/library/alpine:latest"

// probeTimeout bounds a post-Down readiness probe so an assertion that the
// server is gone fails fast rather than waiting out a dial.
const probeTimeout = 2 * time.Second

// podmanUsable reports whether rootless podman can run a container here. A
// missing binary or broken rootless setup means skip, not fail — mirrors
// internal/runtime/lifecycle_test.go:56-60.
func podmanUsable() bool {
	out, err := exec.Command("podman", "run", "--rm", agentImage, "true").CombinedOutput()
	_ = out // combined output is only of interest on failure, which err already signals
	return err == nil
}

// buildBinaries compiles the three stack child binaries from the current tree
// into binDir and returns it. The ProcessSupervisor resolves each Component to a
// bare binary name via exec.LookPath (adapters/process.go:33-60), so the stack
// only stands up if compass-postgres/-server/-runner are found on PATH; the
// caller prepends binDir to PATH. Built from the module root (../.. of this
func buildBinariesFromModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	moduleRoot := filepath.Join(wd, "..", "..") // go/cmd/compass-stack -> go
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
// reading the kernel-assigned port, then closing — race-free enough for a test
// and the only way to a fixed port the config accepts (Config.Validate rejects
// :0; there is no bound-address discovery API). All listeners are held open
// until every port is read so the kernel cannot hand the same port twice.
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

// stackFixture is one subtest's fully-resolved stack config plus the derived
// paths the assertions probe. newFixture returns the wired stack.Deps alongside
// it (not a struct field — a fresh Deps set is built per Up in the concurrency
// subtest).
type stackFixture struct {
	cfg    stack.Config
	pgSock string // the postgres unix socket path (SocketDir/.s.PGSQL.<port>)
	socket string // the server unix socket path
}

// newFixture builds a short-path, free-port stack config through the SAME
// resolveConfig the CLI uses (no duplicated config logic) and wires the real
// adapters via buildDeps. shortRoot is a per-subtest dir kept short for the
// AF_UNIX sun_path budget Config.Validate enforces on RuntimeDir (~38 bytes on
// Linux); a t.TempDir path would overflow it. StateDir may be a normal long
// t.TempDir — only the socket/runtime paths are budget-constrained.
func newFixture(t *testing.T, shortRoot string) (stackFixture, stack.Deps) {
	t.Helper()

	pgSockDir := filepath.Join(shortRoot, "pg")
	runtimeDir := filepath.Join(shortRoot, "rt")
	serverSock := filepath.Join(shortRoot, "s.sock")
	if err := os.MkdirAll(pgSockDir, 0o700); err != nil {
		t.Fatalf("mkdir pg sock dir: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}

	ports := freePorts(t, 2)
	listenPort, pgPort := ports[0], ports[1]

	// The DSN host is the socket DIRECTORY postgres -k listens on (libpq unix
	// convention); the postgres wrapper (cmd/compass-postgres) creates it and
	// binds SocketDir/.s.PGSQL.<port>.
	dsn := "host=" + pgSockDir + " port=" + strconv.Itoa(pgPort) + " dbname=compass sslmode=disable"

	cfg, err := resolveConfig(configFlags{
		stateDir:   t.TempDir(),
		socket:     serverSock,
		listen:     "127.0.0.1:" + strconv.Itoa(listenPort),
		database:   dsn,
		image:      agentImage,
		runtimeDir: runtimeDir,
		// Headless T2 stack emits no OTLP; opt out of the bundled collector so
		// buildDeps wires no collector adapter (the real collector path is
		// covered by collector_podman_test.go). Without this, a struct-literal
		// configFlags bypasses newFlagSet's CollectorImage default, leaving both
		// CollectorImage and ExternalOTLPEndpoint empty -> an empty-image
		// `podman run` deep in the adapter.
		otelExternal: "127.0.0.1:4317",
	})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}

	deps, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	return stackFixture{
		cfg:    cfg,
		pgSock: filepath.Join(pgSockDir, ".s.PGSQL."+strconv.Itoa(pgPort)),
		socket: serverSock,
	}, deps
}

// shortRoot creates a short, unique, 0700 root under /tmp for one subtest and
// registers its RemoveAll. Short because everything sun_path-budgeted lives
// under it; unique off os.Getpid()+suffix so nothing collides with a concurrent
// or crashed run.
func shortRoot(t *testing.T, suffix string) string {
	t.Helper()
	root := filepath.Join("/tmp", "cs"+strconv.Itoa(os.Getpid())+suffix)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir short root: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort teardown guard: the state under here is this test's alone,
		// and the subtest's Down has already drained the children by now.
		_ = os.RemoveAll(root)
	})
	return root
}

// downGuard registers a best-effort Down so a t.Fatal before the explicit Down
// still drains the stack's children (Down is safe to call twice: drainChildren
// nils its handles and the lock release is idempotent, lockfile.go:137-147).
func downGuard(t *testing.T, s *stack.Stack) {
	t.Helper()
	t.Cleanup(func() {
		// Best-effort: the happy path asserts Down's error explicitly; this only
		// covers a failed/panicked subtest so no child process leaks.
		_ = s.Down(context.Background())
	})
}

// assertServerGone probes the server socket and requires it to NOT answer —
// proof the compass-server child is gone after Down. A short timeout keeps a
// failed assertion fast.
func assertServerGone(t *testing.T, deps stack.Deps, socketPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if _, err := deps.Prober.Probe(ctx, socketPath); err == nil {
		t.Fatal("server still answering GetServerInfo after Down; the compass-server child was not stopped")
	}
}

// assertPostgresGone dials the postgres unix socket and requires the dial to
// fail — proof the postgres child is gone after Down.
func assertPostgresGone(t *testing.T, pgSock string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", pgSock, probeTimeout)
	if err == nil {
		_ = conn.Close() // reachable socket means the child is still up — closing is only tidiness before we fail
		t.Fatal("postgres still accepting on its socket after Down; the postgres child was not stopped")
	}
}

func TestStackIntegration(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}

	// Build the three child binaries once and put them first on PATH for every
	// subtest. t.Setenv (parent scope, not parallel) so subtests inherit it.
	binDir := buildBinariesFromModuleRoot(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background() // test root context (rule://go-thread-context exemption)

	// 1. up -> Ready -> down. A cold up reaches Health=Ready (server answering,
	// and — since a spawned Ready stack means spawnChain ran to completion — the
	// runner spawned with the image pulled), then Down drains the children.
	t.Run("up_ready_down", func(t *testing.T) {
		fx, deps := newFixture(t, shortRoot(t, "-a"))

		s, err := stack.Up(ctx, fx.cfg, deps)
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		downGuard(t, s)

		st, err := s.Health(ctx)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if st.State != stack.StatusReady {
			t.Fatalf("Health state = %s (%q), want ready", st.State, st.Detail)
		}
		if st.Detail == "" {
			t.Fatal("Ready status carried an empty detail; want the server version")
		}
		if _, err := os.Stat(fx.socket); err != nil {
			t.Fatalf("server socket %q missing while Ready: %v", fx.socket, err)
		}

		if err := s.Down(ctx); err != nil {
			t.Fatalf("Down: %v", err)
		}
		assertServerGone(t, deps, fx.socket)
		assertPostgresGone(t, fx.pgSock)
	})

	// 2. A second, independent up ATTACHES to the live stack rather than
	// spawning a second one: the winner holds the O_EXCL lock, so the second Up
	// finds it held, probes the answering server, and attaches (owns no
	// children). Down on the attached stack is a no-op; Down on the spawner
	// tears the real stack down.
	t.Run("second_up_attaches", func(t *testing.T) {
		fx, deps1 := newFixture(t, shortRoot(t, "-b"))

		s1, err := stack.Up(ctx, fx.cfg, deps1)
		if err != nil {
			t.Fatalf("first Up: %v", err)
		}
		downGuard(t, s1)
		if st, err := s1.Health(ctx); err != nil || st.State != stack.StatusReady {
			t.Fatalf("first Up Health = %s (err %v), want ready", st.State, err)
		}

		// Fresh deps, same Config: a genuinely independent second up.
		deps2, err := buildDeps(fx.cfg)
		if err != nil {
			t.Fatalf("buildDeps (second): %v", err)
		}
		s2, err := stack.Up(ctx, fx.cfg, deps2)
		if err != nil {
			t.Fatalf("second Up: %v", err)
		}
		downGuard(t, s2)
		st2, err := s2.Health(ctx)
		if err != nil {
			t.Fatalf("second Up Health: %v", err)
		}
		if st2.State != stack.StatusAttached {
			t.Fatalf("second Up state = %s, want attached (it must NOT have spawned a second stack)", st2.State)
		}

		// The attached stack owns nothing: Down releases (a no-op) and must not
		// stop the still-live children.
		if err := s2.Down(ctx); err != nil {
			t.Fatalf("attached Down: %v", err)
		}
		if st, err := s1.Health(ctx); err != nil || st.State != stack.StatusReady {
			t.Fatalf("after attached Down, spawner Health = %s (err %v), want still ready", st.State, err)
		}

		// The spawner's Down performs the real teardown.
		if err := s1.Down(ctx); err != nil {
			t.Fatalf("spawner Down: %v", err)
		}
		assertServerGone(t, deps1, fx.socket)
		assertPostgresGone(t, fx.pgSock)
	})

	// 3. Two concurrent ups -> exactly one spawns. The O_EXCL lockfile closes the
	// probe->spawn TOCTOU: one goroutine wins the lock and spawns the chain; the
	// other, finding the lock held, either attaches (server already answering) or
	// returns a contended error (server not yet answering) — either outcome is
	// "did not double-spawn". Exactly one Up comes back a spawned Ready stack.
	t.Run("concurrent_ups_one_spawns", func(t *testing.T) {
		fx, _ := newFixture(t, shortRoot(t, "-c"))

		type result struct {
			s   *stack.Stack
			err error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				// Each goroutine gets its own real deps; the shared Config points
				// them at the same state dir / socket, so the lock arbitrates.
				deps, derr := buildDeps(fx.cfg)
				if derr != nil {
					results <- result{nil, derr}
					return
				}
				s, err := stack.Up(ctx, fx.cfg, deps)
				results <- result{s, err}
			}()
		}
		r1, r2 := <-results, <-results

		// Classify each outcome by its PUBLIC Health state: a spawner reads
		// Ready (it spawned the chain), an attacher reads Attached (it found the
		// lock held and attached to the live server), and a contended loser
		// returns an error and no stack. Exactly one may be a spawner.
		var spawners, others int
		var spawner *stack.Stack
		for _, r := range []result{r1, r2} {
			if r.err != nil {
				// A contended loser: server not yet answering when it found the
				// lock held. Legitimate "did not double-spawn" — but it must not
				// also hand back a stack.
				if r.s != nil {
					t.Fatalf("contended Up returned an error AND a stack: %v / %+v", r.err, r.s)
				}
				others++
				continue
			}
			if r.s == nil {
				t.Fatalf("Up returned nil stack and nil error")
			}
			downGuard(t, r.s)
			st, err := r.s.Health(ctx)
			if err != nil {
				t.Fatalf("classifying Up Health: %v", err)
			}
			switch st.State {
			case stack.StatusReady:
				spawners++
				spawner = r.s
			case stack.StatusAttached:
				others++
			default:
				t.Fatalf("Up produced state %s, want ready (spawned) or attached", st.State)
			}
		}
		if spawners != 1 {
			t.Fatalf("exactly one Up must spawn; got %d spawners, %d others", spawners, others)
		}

		// The single spawner is Ready and its server answers — exactly one stack.
		if st, err := spawner.Health(ctx); err != nil || st.State != stack.StatusReady {
			t.Fatalf("spawner Health = %s (err %v), want ready", st.State, err)
		}

		if err := spawner.Down(ctx); err != nil {
			t.Fatalf("spawner Down: %v", err)
		}
		goneDeps, err := buildDeps(fx.cfg)
		if err != nil {
			t.Fatalf("buildDeps (gone probe): %v", err)
		}
		assertServerGone(t, goneDeps, fx.socket)
		assertPostgresGone(t, fx.pgSock)
	})
}
