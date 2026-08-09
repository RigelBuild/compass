//go:build podman

package main

// T4.3 gate — the scripted, headless (no-webview), CROSS-PROCESS teardown proof
// the frozen native-app design requires (design.md:465-491: "Manual QA + a
// scripted compass-stack-level CI variant (headless, no webview)"). It closes
// the gap TestStackIntegration leaves: that test proves up→Ready→down but ONLY
// in-process (one process owns the child handles). The real app path is
// cross-process — the app shells out to a `compass-stack up` that spawns the
// three children in their own process groups and then EXITS (linger), and later
// shells out to a FRESH `compass-stack down` that holds NO in-memory handles and
// must tear the stack down by reading the persisted <StateDir>/stack.pgids record
// (stack.DownDetached, PR #248). This test drives the REAL compass-stack binary
// as SEPARATE processes end to end, exactly as the app does.
//
// It is a pure test-only addition: it changes no production code under
// internal/stack or cmd/compass-stack/main.go (the teardown mechanism is frozen
// and already reviewed in PR #248) and reuses the in-process test's helpers
// (shortRoot, freePorts, buildBinariesFromModuleRoot, podmanUsable, newFixture
// via resolveConfig/buildDeps, assertServerGone, assertPostgresGone,
// agentImage/probeTimeout) that live in integration_podman_test.go, same package.
//
// PROCESS SAFETY (this runs on a SHARED box; rule://process-safety): the only
// thing that ever STOPS a process here is `compass-stack down` (which signals
// only the exact process groups it reads from the stack's OWN state-dir
// stack.pgids record) — never a pkill/killall/pattern-kill/name-scan. The
// post-down liveness assertion (assertGroupESRCH) probes ONLY pgids read from
// that same record, with kill(-pgid, 0), and asserts ESRCH — it signals nothing.
// A t.Cleanup runs an idempotent `compass-stack down` so a t.Fatal mid-test still
// drains the children (the subprocess mirror of the in-process downGuard).
//
// AGENT IMAGE — docker.io/library/alpine:latest, the same pullable stand-in
// integration_podman_test.go documents and uses: the stack pulls it and hands it
// to the runner but never runs it as a container at up, so a pullable public
// image satisfies the whole up path (the local-only compass-agent:latest is not
// pullable, so EnsureImage's `podman pull` would fail).
//
// DETERMINISM: no sleeps. up returns only after the stack is Ready, so waiting
// for its exit 0 IS the readiness gate; every other wait polls an event (the
// server socket answering / going dark, the postgres socket going dark, the
// pgid file's presence/absence, group ESRCH) under a bounded timeout so a
// failure fails fast. Paths stay under the AF_UNIX sun_path budget via the
// shared shortRoot/freePorts fixture, and the per-test root is unique off the
// pid so a concurrent or crashed run cannot collide.

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/internal/stack"
)

// pgidRecordName is the state-dir teardown record a successful up persists and a
// fresh down reads (internal/stack/pgidfile.go pgidFileName). Referenced by name
// here because the const is package-internal to stack; the test only ever stats
// it or parses the pgids it recorded, never writes it.
const pgidRecordName = "stack.pgids"

// upBudget bounds the `compass-stack up` subprocess: cold-start runs initdb,
// starts postgres, migrates, pulls the agent image, and spawns the runner before
// up returns, so it is generous — but bounded, so a hung up fails the test fast
// rather than waiting out the whole `go test` deadline.
const upBudget = 3 * time.Minute

// downBudget bounds each `compass-stack down` subprocess. DownDetached's absolute
// worst case (every component escalating to SIGKILL) is ~65s; the SIGTERM-succeeds
// path is far quicker. Bounded so a wedged down fails fast.
const downBudget = 90 * time.Second

// answerPollInterval is the gap between server-socket readiness probes. Small
// enough to make the post-up live assertion prompt, an explicit event-gate
// rather than a sleep.
const answerPollInterval = 100 * time.Millisecond

// teardownConfirmBudget bounds the post-down poll for every recorded process
// group to reach ESRCH. down returns once its confirm channels are satisfied,
// but postgres's channel (socket quiescence) fires at SIGTERM smart-shutdown
// entry, before the postmaster process actually exits; this budget covers that
// residual drain-then-exit window. Comfortably larger than a healthy exit takes,
// but bounded so a genuinely stuck child fails the test fast.
const teardownConfirmBudget = 30 * time.Second

// TestCrossProcessTeardown drives the real compass-stack binary as separate up
// and down processes and proves the cross-process teardown mechanism (reading
// <StateDir>/stack.pgids from a fresh process) actually stops a lingering stack.
//
// Steps:
//  1. Build the three child binaries + the compass-stack binary itself into one
//     bin dir; the up/down subprocesses find the children on PATH there.
//  2. `compass-stack up --linger …` as a subprocess → exit 0 (up fire-and-returns
//     after Ready; the children linger in their own process groups).
//  3. Assert the stack is live (server socket answers) AND the pgid record exists.
//  4. Read the recorded pgids from the stack's own record (for the ESRCH check).
//  5. `compass-stack down …` as a SECOND, independent subprocess → exit 0.
//  6. Assert cross-process teardown happened: every recorded process group is
//     ESRCH (the authoritative proof, gated first), then the server and postgres
//     sockets are dark and the pgid record is removed.
//  7. A second `compass-stack down` after full teardown → exit 0 (idempotent
//     "no stack" branch), proving a retried down is safe.
func TestCrossProcessTeardown(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}

	ctx := context.Background() // test root context (rule://go-thread-context exemption for a _test.go root)

	// 1. Build the three stack child binaries AND the compass-stack binary itself
	// into one dir. The subprocesses resolve the children via exec.LookPath, so
	// the dir must be first on their PATH; compass-stack is invoked by full path.
	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(binDir)

	// A short-path, free-port config resolved through the SAME resolveConfig the
	// CLI uses (no duplicated config logic); the fixture's derived socket paths
	// are what the gone-assertions probe. deps gives us the real HealthProber for
	// the live/gone server probes. Unique per-pid short root so a concurrent or
	// crashed run cannot collide (shortRoot uses os.Getpid()+suffix).
	fx, deps := newFixture(t, shortRoot(t, "-xp"))
	cfg := fx.cfg
	recordPath := filepath.Join(cfg.StateDir, pgidRecordName)

	// The idempotent-down cleanup guard: a t.Fatal before the explicit downs
	// still drains the lingering children across the process boundary. Runs a
	// fresh `compass-stack down`, exactly as the app's teardown path does; its
	// result is best-effort (the happy path asserts each down's exit explicitly).
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), downBudget)
		defer cancel()
		// Best-effort teardown guard; output is only of interest if it fails, and
		// a failing guard must not itself fail an already-finished test.
		_, _ = runStack(cctx, t, stackBin, env, "down",
			"--state-dir", cfg.StateDir,
			"--socket", cfg.SocketPath,
			"--listen", cfg.ListenAddr,
			"--database", cfg.DatabaseDSN,
			"--image", cfg.AgentImage,
			"--runtime-dir", cfg.RuntimeDir,
		)
	})

	// 2. `compass-stack up --linger` as a subprocess. up returns (exit 0) only
	// after the stack is Ready, then the children linger — so waiting for exit 0
	// IS the readiness gate, no sleep.
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := runStack(upCtx, t, stackBin, env, "up",
		"--state-dir", cfg.StateDir,
		"--socket", cfg.SocketPath,
		"--listen", cfg.ListenAddr,
		"--database", cfg.DatabaseDSN,
		"--image", cfg.AgentImage,
		"--runtime-dir", cfg.RuntimeDir,
		"--linger",
	)
	if err != nil {
		t.Fatalf("compass-stack up: %v\n%s", err, out)
	}

	// 3. The stack is live across the process boundary: the server socket answers
	// GetServerInfo (poll it, event-gated, as a belt-and-suspenders confirmation
	// on top of up's exit-0 readiness gate), and the pgid teardown record exists.
	waitServerAnswering(t, deps, cfg.SocketPath)
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("stack.pgids record %q missing after up (a lingering stack must leave a teardown record): %v", recordPath, err)
	}

	// 4. Read the recorded process groups from the stack's OWN record — the only
	// pgids this test is ever permitted to probe (rule://process-safety). Reading
	// before the down lets step 6 assert each group is ESRCH afterward.
	pgids := readRecordedPgids(t, recordPath)
	if len(pgids) == 0 {
		t.Fatal("stack.pgids recorded no process groups; a Ready stack spawned all three children")
	}

	// 5. `compass-stack down` as a SECOND, fully independent subprocess: a fresh
	// process holding no in-memory child handles, tearing the stack down purely
	// from the persisted record (the stack.DownDetached path).
	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = runStack(downCtx, t, stackBin, env, "down",
		"--state-dir", cfg.StateDir,
		"--socket", cfg.SocketPath,
		"--listen", cfg.ListenAddr,
		"--database", cfg.DatabaseDSN,
		"--image", cfg.AgentImage,
		"--runtime-dir", cfg.RuntimeDir,
	)
	if err != nil {
		t.Fatalf("compass-stack down: %v\n%s", err, out)
	}

	// 6. Cross-process teardown actually happened. The AUTHORITATIVE proof is
	// process-group death, so gate on it FIRST: poll every recorded process group
	// until it is ESRCH (bounded, event-gated). This is load-bearing, not
	// belt-and-suspenders — DownDetached confirms postgres via its DBProber
	// socket-quiescence channel, which fires the moment postgres enters SIGTERM
	// "smart shutdown" and refuses NEW connections, i.e. while the postmaster
	// process is still alive draining compass-server's pooled connections and
	// still accept()ing on its unix socket. So down returns exit 0 (record
	// removed) before the postmaster has actually exited; a raw socket dial in
	// that window still connects. Waiting for group ESRCH is what makes the
	// socket gone-assertions below deterministic. Only pgids from the stack's OWN
	// record are ever probed (rule://process-safety); kill(-pgid, 0) signals
	// nothing.
	waitGroupsESRCH(t, pgids, teardownConfirmBudget)

	// With every recorded group gone, the sockets are deterministically dark and
	// the full-success teardown has removed its record.
	assertServerGone(t, deps, cfg.SocketPath)
	assertPostgresGone(t, fx.pgSock)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("stack.pgids record %q still present after a full down (full-success teardown removes it): stat err = %v", recordPath, err)
	}

	// 7. A retried down after a full teardown is safe: no record, no answering
	// server → the "no stack" branch → exit 0. Proves an idempotent down.
	down2Ctx, down2Cancel := context.WithTimeout(ctx, downBudget)
	defer down2Cancel()
	out, err = runStack(down2Ctx, t, stackBin, env, "down",
		"--state-dir", cfg.StateDir,
		"--socket", cfg.SocketPath,
		"--listen", cfg.ListenAddr,
		"--database", cfg.DatabaseDSN,
		"--image", cfg.AgentImage,
		"--runtime-dir", cfg.RuntimeDir,
	)
	if err != nil {
		t.Fatalf("second compass-stack down (idempotent no-stack path) must exit 0: %v\n%s", err, out)
	}
}

// buildStackBinary compiles the compass-stack command itself into binDir (beside
// the three child binaries buildBinariesFromModuleRoot built) and returns its
// path. Built from the module root the same way, so the subprocess under test is
// the current tree's compass-stack, not whatever happens to be installed.
func buildStackBinary(t *testing.T, binDir string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	moduleRoot := filepath.Join(wd, "..", "..") // go/cmd/compass-stack -> go
	out := filepath.Join(binDir, "compass-stack")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/compass-stack")
	cmd.Dir = moduleRoot
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build compass-stack: %v\n%s", err, b)
	}
	return out
}

// stackEnv returns the current environment with binDir prepended to PATH so a
// compass-stack subprocess resolves the compass-postgres/-server/-runner
// children (looked up by bare name via exec.LookPath) to the freshly built
// binaries. PATH is rebuilt (not merely re-appended) so there is exactly one
// PATH entry and binDir is unambiguously first.
func stackEnv(binDir string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	oldPath := ""
	for _, e := range base {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			oldPath = p
			continue
		}
		out = append(out, e)
	}
	return append(out, "PATH="+binDir+string(os.PathListSeparator)+oldPath)
}

// runStack runs the compass-stack binary with the given args and environment,
// bounded by ctx, and returns its combined stdout+stderr. The error is the
// process's exit status (nil == exit 0); callers assert on it, and the combined
// output is returned regardless so a failing caller can print it.
//
// Output goes to a temp FILE, never a pipe (CombinedOutput / a buffer-backed
// cmd.Stdout). That is load-bearing for the `up --linger` path: up spawns the
// three children — which INHERIT its stdout/stderr — and then exits. A pipe's
// read end only reaches EOF when every writer fd is closed, so the lingering
// children hold it open and cmd.Wait would block until they die, never
// returning even though up itself exited. A file fd is passed straight to the
// child; cmd.Wait returns on the process's own exit, and the inherited fd is
// harmless.
func runStack(ctx context.Context, t *testing.T, stackBin string, env []string, args ...string) (string, error) {
	t.Helper()
	// A plain os.CreateTemp (not t.TempDir) so this is safe to call from a
	// t.Cleanup, where registering a new t.TempDir cleanup would be too late; the
	// file is removed explicitly below.
	logFile, err := os.CreateTemp("", "compass-stack-out.*.log")
	if err != nil {
		t.Fatalf("create subprocess output file: %v", err)
	}
	logPath := logFile.Name()
	defer func() {
		// Best-effort cleanup of our own transient log file after reading it back;
		// a close/remove error here is not actionable.
		_ = logFile.Close()
		_ = os.Remove(logPath)
	}()

	cmd := exec.CommandContext(ctx, stackBin, args...)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()

	out, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read back subprocess output %q: %v", logPath, readErr)
	}
	return string(out), runErr
}

// waitServerAnswering polls the server socket until GetServerInfo answers,
// bounded by probeTimeout — an explicit readiness event-gate, never a sleep. up
// already returns only after Ready, so this confirms the lingering server is
// reachable across the process boundary rather than waiting for it to become so.
func waitServerAnswering(t *testing.T, deps stack.Deps, socketPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	ticker := time.NewTicker(answerPollInterval)
	defer ticker.Stop()
	for {
		if _, err := deps.Prober.Probe(ctx, socketPath); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("server socket %q did not answer within %s after up returned Ready", socketPath, probeTimeout)
		case <-ticker.C:
		}
	}
}

// waitGroupsESRCH polls every recorded process group until it is ESRCH (gone) or
// the budget elapses — the authoritative "the children are actually dead" proof
// after a cross-process down. It signals nothing: kill(-pgid, 0) is the existence
// probe (signal 0), and it only ever runs on pgids the test read from the
// stack's OWN stack.pgids record, never a scan (rule://process-safety). A pgid
// still alive at budget expiry fails the test — the teardown did not stop it.
func waitGroupsESRCH(t *testing.T, pgids []int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(answerPollInterval)
	defer ticker.Stop()
	for _, pgid := range pgids {
		if pgid <= 1 {
			t.Fatalf("refusing to probe degenerate pgid %d", pgid)
		}
		for {
			gone, err := groupGone(pgid)
			if err != nil {
				t.Fatalf("probing group %d after down: got %v, want ESRCH (gone) or a live group", pgid, err)
			}
			if gone {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("process group %d still alive %s after down; the cross-process teardown did not stop it", pgid, budget)
			}
			<-ticker.C
		}
	}
}

// groupGone reports whether the process group named by pgid is ESRCH (fully
// gone). kill(-pgid, 0) with nil or EPERM means the group still exists (EPERM =
// exists but not signalable by us); ESRCH means gone; any other errno is
// unexpected and surfaced. Probe only — signal 0 delivers nothing.
func groupGone(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	switch {
	case err == nil:
		return false, nil
	case err == syscall.ESRCH:
		return true, nil
	case err == syscall.EPERM:
		return false, nil
	default:
		return false, err
	}
}

// readRecordedPgids parses the stack.pgids record and returns the recorded
// process-group ids in start order. The record is
//
//	<version> <writerPid>
//	<component> <pgid> <starttime>
//	...
//
// (internal/stack/pgidfile.go writePgidFile). The const is package-internal to
// stack, so the test parses the pgid field directly rather than calling the
// unexported reader — a read-only parse of a stack-owned file, never a write.
func readRecordedPgids(t *testing.T, recordPath string) []int {
	t.Helper()
	f, err := os.Open(recordPath)
	if err != nil {
		t.Fatalf("open stack.pgids record %q: %v", recordPath, err)
	}
	defer func() {
		// Read-only handle in a test; a close error here is not actionable.
		_ = f.Close()
	}()

	var pgids []int
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if line == 1 || text == "" {
			continue // header (line 1) or a blank line
		}
		fields := strings.Fields(text)
		if len(fields) != 3 {
			t.Fatalf("stack.pgids entry line %d malformed: %q", line, text)
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("stack.pgids entry line %d has unparseable pgid %q: %v", line, fields[1], err)
		}
		pgids = append(pgids, pgid)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stack.pgids record %q: %v", recordPath, err)
	}
	return pgids
}
