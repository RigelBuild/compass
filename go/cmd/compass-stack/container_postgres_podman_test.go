//go:build podman

package main

// Container tests drive separate up/down processes against rootless podman.
// They cover bundled postgres and the external-database opt-out with a throwaway
// container; cleanup targets only test-owned or stack-recorded containers.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// pgImagePinned is the same pinned stock postgres:18 digest the stack defaults
// to (stack.DefaultPostgresImage). Referenced by literal here because the test
// drives the CLI which resolves the default itself; kept equal so the test
// exercises the shipped image, not a drift.
const pgImagePinned = "docker.io/library/postgres:18@sha256:1957b2ff3137e4ef7f3bc813e74fff50b1e1ffddc85c8b9d6f14ade972be8687"

// dsnReachableBudget bounds a post-up confirmation; Ready already checks the
// postgres connection.
const dsnReachableBudget = 30 * time.Second

// containerGoneBudget bounds the post-down container-disappearance poll.
const containerGoneBudget = 30 * time.Second

// TestContainerPostgresUpDown covers the bundled postgres container path.
func TestContainerPostgresUpDown(t *testing.T) {
	if !podmanUsable(t) {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := t.Context()

	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(t, binDir)

	fx := newContainerFixture(ctx, t, shortRoot(t, "-ctr"))
	cfg := fx.cfg
	recordPath := filepath.Join(cfg.StateDir, pgidRecordName)
	name := derivedContainerName(cfg.StateDir)

	// Cleanup guard: a fresh down (tears the container down by the recorded
	// name), then a belt-and-suspenders explicit rm of the container THIS test
	// would have created, by its exact unique name (never a scan). Both
	// best-effort: a guard failure must not fail an already-finished test.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), downBudget)
		defer cancel()
		if out, runErr, infraErr := runStack(cctx, t, stackBin, env, cfg.args("down")...); infraErr != nil {
			t.Logf("cleanup guard: down harness error (ignored): %v", infraErr)
		} else if runErr != nil {
			t.Logf("cleanup guard: down exited non-zero (ignored): %v\n%s", runErr, out)
		}
		if out, err := exec.CommandContext(cctx, "podman", "rm", "--force", "--volumes", name).CombinedOutput(); err != nil {
			t.Logf("cleanup guard: podman rm %s (ignored): %v\n%s", name, err, out)
		}
	})

	// up with the pinned container image.
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env, cfg.args("up", "--postgres-image", pgImagePinned, "--otel-external", "127.0.0.1:4317", "--gateway-external", "http://127.0.0.1:4100", "--linger")...)
	if err != nil {
		t.Fatalf("compass-stack up (container postgres): %v\n%s", err, out)
	}

	// The stack is live: the server answers, and the DSN is reachable over the
	// bind-mounted socket (the exact store.Open precondition).
	waitServerAnswering(t, fx.deps, cfg.SocketPath)
	waitDSNReachable(t, cfg.DatabaseDSN, dsnReachableBudget)

	// The postgres child is a container that EXISTS under the derived name, and
	// the pgid record carries it as a container (ctr) entry.
	if !containerExists(t, name) {
		t.Fatalf("postgres container %q not present after up", name)
	}
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("stack.pgids record %q missing after up: %v", recordPath, err)
	}
	assertRecordHasContainerEntry(t, recordPath, name)

	// Fresh-process down: reads the v2 record, tears the container down by name.
	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, cfg.args("down")...)
	if err != nil {
		t.Fatalf("compass-stack down (container postgres): %v\n%s", err, out)
	}

	// The container is gone and the record removed.
	waitContainerGone(t, name, containerGoneBudget)
	assertServerGone(t, t.Context(), fx.deps, cfg.SocketPath)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("stack.pgids record %q still present after a full down: stat err = %v", recordPath, err)
	}
}

// TestExternalDatabaseUpDown covers the external-DB opt-out with a throwaway
// container standing in for the operator's database.
func TestExternalDatabaseUpDown(t *testing.T) {
	if !podmanUsable(t) {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := t.Context()

	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(t, binDir)

	// The "external" postgres: a throwaway TCP-published container the test owns.
	externalDSN := startExternalPostgres(t)

	fx := newContainerFixture(ctx, t, shortRoot(t, "-ext"))
	cfg := fx.cfg
	cfg.DatabaseDSN = externalDSN // the stack probes THIS as-is; no compass postgres starts
	recordPath := filepath.Join(cfg.StateDir, pgidRecordName)

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), downBudget)
		defer cancel()
		if out, runErr, infraErr := runStack(cctx, t, stackBin, env, cfg.args("down")...); infraErr != nil {
			t.Logf("cleanup guard: down harness error (ignored): %v", infraErr)
		} else if runErr != nil {
			t.Logf("cleanup guard: down exited non-zero (ignored): %v\n%s", runErr, out)
		}
	})

	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env, cfg.args("up", "--database-external", "--otel-external", "127.0.0.1:4317", "--gateway-external", "http://127.0.0.1:4100", "--linger")...)
	if err != nil {
		t.Fatalf("compass-stack up (external db): %v\n%s", err, out)
	}

	// No postgres process is started for an external DB; the record has only
	// server and runner process entries.
	waitServerAnswering(t, fx.deps, cfg.SocketPath)
	assertRecordHasNoPostgresEntry(t, recordPath)

	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, cfg.args("down")...)
	if err != nil {
		t.Fatalf("compass-stack down (external db): %v\n%s", err, out)
	}
	assertServerGone(t, t.Context(), fx.deps, cfg.SocketPath)

	// The external postgres is UNTOUCHED by the stack's teardown — still up.
	if !externalPostgresReachable(t.Context(), externalDSN) {
		t.Fatal("external postgres unreachable after stack down; the stack must never tear down a DB it did not start")
	}
}

// containerFixture pairs a container-path config with wired dependencies.
// Its socket directory is a PGDATA sibling; nesting it makes initdb fail.
type containerFixture struct {
	cfg  containerCfg
	deps stack.Deps
}

// containerCfg holds shared stack flags so up and down identify the same stack.
type containerCfg struct {
	StateDir    string
	SocketPath  string
	ListenAddr  string
	DatabaseDSN string
	AgentImage  string
	RuntimeDir  string
}

// args builds a shared up/down flag set with optional extra arguments.
func (c containerCfg) args(sub string, extra ...string) []string {
	out := make([]string, 0, 13+len(extra))
	out = append(out,
		sub,
		"--state-dir", c.StateDir,
		"--socket", c.SocketPath,
		"--listen", c.ListenAddr,
		"--database", c.DatabaseDSN,
		"--image", c.AgentImage,
		"--runtime-dir", c.RuntimeDir,
	)
	return append(out, extra...)
}

// newContainerFixture builds short paths and real deps. Its DSN socket dir must
// be a PGDATA sibling; nesting makes PGDATA non-empty and initdb fails.
func newContainerFixture(ctx context.Context, t *testing.T, root string) containerFixture {
	t.Helper()
	ports := freePorts(ctx, t, 2)
	return newContainerFixtureOnPorts(t, root, ports[0], ports[1])
}

// newContainerFixtureOnPorts accepts ports from one reservation call; separate
// calls could reuse a port after it is released.
func newContainerFixtureOnPorts(t *testing.T, root string, listenPort, pgPort int) containerFixture {
	t.Helper()
	pgSockDir := filepath.Join(root, "pgsock")
	runtimeDir := filepath.Join(root, "rt")
	serverSock := filepath.Join(root, "s.sock")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{runtimeDir, stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", d, err)
		}
	}

	dsn := "host=" + pgSockDir + " port=" + strconv.Itoa(pgPort) + " dbname=compass sslmode=disable"

	cfg := containerCfg{
		StateDir:    stateDir,
		SocketPath:  serverSock,
		ListenAddr:  "127.0.0.1:" + strconv.Itoa(listenPort),
		DatabaseDSN: dsn,
		AgentImage:  agentImage,
		RuntimeDir:  runtimeDir,
	}
	deps, err := buildDeps(stack.Config{
		StateDir:           cfg.StateDir,
		SocketPath:         cfg.SocketPath,
		ListenAddr:         cfg.ListenAddr,
		DatabaseDSN:        cfg.DatabaseDSN,
		AgentImage:         cfg.AgentImage,
		RuntimeDir:         cfg.RuntimeDir,
		ExternalNatsURL:    "nats://127.0.0.1:4222",
		ExternalGatewayURL: "http://127.0.0.1:4100",
	})
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	return containerFixture{cfg: cfg, deps: deps}
}

// derivedContainerName reproduces stack's private name: the cleaned state dir's
// SHA-256 prefix with the compass-postgres- prefix. Down reconstructs this name.
func derivedContainerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-postgres-" + hex.EncodeToString(sum[:6])
}

// waitDSNReachable polls for a successful pgx ping under a budget; this is the
// event gate for postgres accepting on its bind-mounted socket.
func waitDSNReachable(t *testing.T, dsn string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if externalPostgresReachable(t.Context(), dsn) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("DSN %q not reachable within %s", dsn, budget)
		}
		time.Sleep(answerPollInterval) //nolint:forbidigo // bounded poll tick between reachability checks (event-gated by the deadline above; rule://go-no-sleep-in-test poll-until exemption)
	}
}

// containerExists reports whether a container of this name is present, via
// `podman container exists` (exit 0 present, 1 absent). A non-0/1 exit is a real
// engine failure and fails the test.
func containerExists(t *testing.T, name string) bool {
	t.Helper()
	err := exec.CommandContext(t.Context(), "podman", "container", "exists", name).Run()
	if err == nil {
		return true
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		return false
	}
	t.Fatalf("podman container exists %q: %v", name, err)
	return false
}

// waitContainerGone polls until the named container is absent or the budget
// elapses — the event-gate for the teardown, never a sleep.
//
//nolint:unparam // read-clarity signature: the budget stays visible at each teardown call site
func waitContainerGone(t *testing.T, name string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if !containerExists(t, name) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %q still present %s after down", name, budget)
		}
		time.Sleep(answerPollInterval) //nolint:forbidigo // bounded poll tick (event-gated by the deadline; rule://go-no-sleep-in-test poll-until exemption)
	}
}

// assertRecordHasContainerEntry parses the v2 pgid record and requires the
// postgres line to be a container (ctr) entry with the derived name. The record
// grammar is stack-internal; the test parses the lines directly (read-only).
func assertRecordHasContainerEntry(t *testing.T, recordPath, name string) {
	t.Helper()
	for _, f := range recordLines(t, recordPath) {
		if len(f) >= 3 && f[0] == "ctr" && f[1] == "postgres" {
			if f[2] != name {
				t.Fatalf("container entry name = %q, want %q", f[2], name)
			}
			return
		}
	}
	t.Fatalf("no `ctr postgres <name>` entry in %q", recordPath)
}

// assertRecordHasNoPostgresEntry requires the record to carry no postgres line
// at all — the external-DB path starts no postgres component.
func assertRecordHasNoPostgresEntry(t *testing.T, recordPath string) {
	t.Helper()
	for _, f := range recordLines(t, recordPath) {
		if len(f) >= 2 && f[1] == "postgres" {
			t.Fatalf("external path recorded a postgres entry %v, want none", f)
		}
	}
}

// recordLines reads the v2 pgid record and returns the per-entry field slices
// (skipping the header line). Read-only parse of a stack-owned file.
func recordLines(t *testing.T, recordPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read pgid record %q: %v", recordPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var out [][]string
	for i, ln := range lines {
		if i == 0 {
			continue // header: <version> <writerPid>
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, strings.Fields(ln))
	}
	return out
}

// startExternalPostgres launches a throwaway TCP-published postgres container,
// waits until it accepts connections, and returns its DSN. Cleanup removes the
// container and volume; setup failures are fatal rather than skipped.
func startExternalPostgres(t *testing.T) string {
	t.Helper()
	name := "compass-ext-pg-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	hostPort := freePorts(t.Context(), t, 1)[0]
	password := "external-test"
	out, err := exec.CommandContext(t.Context(), "podman", "run", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=compass",
		"-p", strconv.Itoa(hostPort)+":5432",
		pgImagePinned,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("start external postgres: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), downBudget)
		defer cancel()
		if out, err := exec.CommandContext(cleanupCtx, "podman", "rm", "--force", "--volumes", name).CombinedOutput(); err != nil {
			t.Logf("cleanup: podman rm external postgres %s (ignored): %v\n%s", name, err, out)
		}
	})
	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=%s dbname=compass sslmode=disable", hostPort, password)
	waitDSNReachable(t, dsn, dsnReachableBudget)
	return dsn
}

// externalPostgresReachable reports whether a pgx connection to dsn pings clean.
func externalPostgresReachable(ctx context.Context, dsn string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(ctx) }() // probe-only conn; close error is not the verdict (the ping is)
	return conn.Ping(ctx) == nil
}
