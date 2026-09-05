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
// post-down liveness assertion (waitGroupsGone) probes ONLY pgids read from
// that same record, with kill(-pgid, 0), and asserts each group is gone — ESRCH
// or a recycled-pid start-time mismatch — signalling nothing.
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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
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
// worst case (every component escalating to SIGKILL) is ~105s now that the nats
// drain budget joins the collector/postgres teardown; the SIGTERM-succeeds path
// is ~85s. Bounded generously above the escalation worst case so a `down` that
// legitimately escalates is never killed mid-teardown, while a genuinely wedged
// down still fails fast.
const downBudget = 150 * time.Second

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
//  4. Read the recorded pgids + start-time tokens from the stack's own record
//     (for the identity-checked liveness assertion).
//  5. `compass-stack down …` as a SECOND, independent subprocess → exit 0.
//  6. Assert cross-process teardown happened: every recorded process group is
//     gone — identity-checked ESRCH (the authoritative proof, gated first), then
//     the server and postgres
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
		// Best-effort teardown guard. It runs runStack directly (not mustRunStack)
		// so a harness/infra hiccup or a non-zero down here is only LOGGED, never
		// fatal: a failing guard must not fail an already-finished, passing test.
		out, runErr, infraErr := runStack(cctx, t, stackBin, env, "down",
			"--state-dir", cfg.StateDir,
			"--socket", cfg.SocketPath,
			"--listen", cfg.ListenAddr,
			"--database", cfg.DatabaseDSN,
			"--image", cfg.AgentImage,
			"--runtime-dir", cfg.RuntimeDir,
		)
		switch {
		case infraErr != nil:
			t.Logf("cleanup guard: compass-stack down harness error (ignored): %v", infraErr)
		case runErr != nil:
			t.Logf("cleanup guard: compass-stack down exited non-zero (ignored): %v\n%s", runErr, out)
		}
	})

	// 2. `compass-stack up --linger` as a subprocess. up returns (exit 0) only
	// after the stack is Ready, then the children linger — so waiting for exit 0
	// IS the readiness gate, no sleep.
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env, "up",
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
	// pgids this test is ever permitted to probe (rule://process-safety), plus
	// each group's recorded start-time identity token. Reading before the down
	// lets step 6 assert each group is gone afterward — identity-checked, so a
	// recycled pid can never masquerade as a still-alive child.
	groups := readRecordedGroups(t, recordPath)
	if len(groups) == 0 {
		t.Fatal("stack.pgids recorded no process groups; a Ready stack spawned all three children")
	}

	// 5. `compass-stack down` as a SECOND, fully independent subprocess: a fresh
	// process holding no in-memory child handles, tearing the stack down purely
	// from the persisted record (the stack.DownDetached path).
	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, "down",
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
	// until it is gone — ESRCH, or a leader start-time mismatch (a recycled pid),
	// the same identity gate production teardown uses (bounded, event-gated). This
	// is load-bearing, not
	// belt-and-suspenders — DownDetached confirms postgres via its DBProber
	// socket-quiescence channel, which fires the moment postgres enters SIGTERM
	// "smart shutdown" and refuses NEW connections, i.e. while the postmaster
	// process is still alive draining compass-server's pooled connections and
	// still accept()ing on its unix socket. So down returns exit 0 (record
	// removed) before the postmaster has actually exited; a raw socket dial in
	// that window still connects. Waiting for every recorded group to be gone
	// (identity-checked) is what makes the socket gone-assertions below
	// deterministic. Only pgids from the stack's OWN record are ever probed
	// (rule://process-safety); kill(-pgid, 0) signals nothing.
	waitGroupsGone(t, groups, teardownConfirmBudget)

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
	out, err = mustRunStack(down2Ctx, t, stackBin, env, "down",
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
// bounded by ctx. It returns the subprocess's combined stdout+stderr, the
// process exit error (runErr; nil == exit 0), and a separate infraErr that is
// non-nil only when the HARNESS itself failed — it could not create or read
// back the temp output file. Keeping the harness failure distinct from the
// subprocess exit status is what lets the best-effort cleanup guard log-and-
// ignore a transient tmp-I/O hiccup instead of failing an already-finished
// test; the happy-path steps call mustRunStack, which makes infraErr fatal.
//
// Output goes to a temp FILE, never a pipe (CombinedOutput / a buffer-backed
// cmd.Stdout). That is load-bearing for the `up --linger` path: up spawns the
// three children — which INHERIT its stdout/stderr — and then exits. A pipe's
// read end only reaches EOF when every writer fd is closed, so the lingering
// children hold it open and cmd.Wait would block until they die, never
// returning even though up itself exited. A file fd is passed straight to the
// child; cmd.Wait returns on the process's own exit, and the inherited fd is
// harmless.
func runStack(ctx context.Context, t *testing.T, stackBin string, env []string, args ...string) (out string, runErr, infraErr error) {
	t.Helper()
	// A plain os.CreateTemp (not t.TempDir) so this is safe to call from a
	// t.Cleanup, where registering a new t.TempDir cleanup would be too late; the
	// file is removed explicitly below.
	logFile, err := os.CreateTemp("", "compass-stack-out.*.log")
	if err != nil {
		return "", nil, fmt.Errorf("create subprocess output file: %w", err)
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
	runErr = cmd.Run()

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		return "", runErr, fmt.Errorf("read back subprocess output %q: %w", logPath, readErr)
	}
	return string(data), runErr, nil
}

// mustRunStack is runStack for the happy-path steps (up / down / retried down):
// a harness (infra) failure is fatal, and only the subprocess exit error is
// returned for the caller to assert on. The cleanup guard calls runStack
// directly so its infra errors are logged, not fatal.
func mustRunStack(ctx context.Context, t *testing.T, stackBin string, env []string, args ...string) (string, error) {
	t.Helper()
	out, runErr, infraErr := runStack(ctx, t, stackBin, env, args...)
	if infraErr != nil {
		t.Fatalf("compass-stack %v: harness error: %v", args, infraErr)
	}
	return out, runErr
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

// recordedGroup is one child's teardown identity as persisted in stack.pgids:
// its process-group id and the group leader's start-time token. The start time
// is what closes the pid-recycling window — the same (Pgid, StartTime) identity
// the production teardown checks (internal/stack/pgidfile.go pgidEntry,
// adapters/groupsignal.go Alive).
type recordedGroup struct {
	pgid      int
	startTime uint64
}

// waitGroupsGone polls every recorded process group until it is gone or the
// budget elapses — the authoritative "the children are actually dead" proof
// after a cross-process down. "Gone" is identity-checked, mirroring production's
// teardown gate (adapters/groupsignal.go Alive): a group is gone when it is
// ESRCH, OR it still exists but its leader's start time no longer equals the
// recorded token (the kernel recycled the pid to an unrelated leader). Without
// the identity check the probe is a false-FAILURE risk on the shared box — a
// dead child's pgid reused by another process would read as "still alive" and
// fail the test at budget even though teardown worked.
//
// It signals nothing: kill(-pgid, 0) is the existence probe (signal 0), run
// only on pgids read from the stack's OWN stack.pgids record, never a scan
// (rule://process-safety). A group genuinely still alive at budget expiry fails
// the test — the teardown did not stop it.
func waitGroupsGone(t *testing.T, groups []recordedGroup, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(answerPollInterval)
	defer ticker.Stop()
	for _, grp := range groups {
		if grp.pgid <= 1 {
			t.Fatalf("refusing to probe degenerate pgid %d", grp.pgid)
		}
		for {
			alive, err := groupAlive(grp.pgid, grp.startTime)
			if err != nil {
				t.Fatalf("probing group %d after down: got %v, want gone or a live group", grp.pgid, err)
			}
			if !alive {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("process group %d still alive %s after down; the cross-process teardown did not stop it", grp.pgid, budget)
			}
			<-ticker.C
		}
	}
}

// groupAlive reports whether the process group named by pgid still exists AND
// its leader's start time equals the recorded token — the same existence-then-
// identity gate production teardown uses (adapters/groupsignal.go Alive). A
// group that is ESRCH, or whose leader start time no longer matches (a recycled
// pid), or whose /proc entry cannot be read is reported not-alive: for a
// post-down liveness probe the safe verdict is "gone", never a false "alive"
// off a pid the kernel reused. It signals nothing — kill(-pgid, 0) is signal 0.
// An unexpected kill errno (not ESRCH/EPERM) is surfaced as an error.
func groupAlive(pgid int, startTime uint64) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return false, nil // gone
	case err != nil && !errors.Is(err, syscall.EPERM):
		return false, err // unexpected errno
	}
	// Exists (nil or EPERM). Confirm identity via the leader's start time; a read
	// failure means the leader vanished or /proc is unreadable — treat as gone.
	got, rerr := readLeaderStartTime(pgid)
	if rerr != nil {
		// Deliberate: a /proc read failure on an existing pgid means the leader
		// vanished between the two syscalls (or /proc is unreadable) — the safe
		// post-down verdict is "gone", never a false "alive". Mirrors production
		// adapters/groupsignal.go Alive, which also treats a read failure as
		// not-alive.
		return false, nil //nolint:nilerr // read failure => leader gone => not-alive (see comment)
	}
	return got == startTime, nil
}

// readLeaderStartTime reads field 22 (starttime) of /proc/<pgid>/stat — the
// group leader, since pid == pgid for a Setpgid child. It is a third read-only
// copy of the leaf parser the production teardown uses; the canonical
// explanation of the parenthesized-comm gotcha lives at stack.parseStatStartTime
// (duplicated again in adapters.parseGroupLeaderStat for the same reason).
func readLeaderStartTime(pgid int) (uint64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pgid) + "/stat")
	if err != nil {
		return 0, err
	}
	return parseLeaderStartTime(string(data))
}

// parseLeaderStartTime extracts field 22 (starttime) from a /proc/<pid>/stat
// line. comm (field 2) is parenthesized and may itself contain spaces and
// parens, so count fields from the LAST ')'; field[0] after it is state
// (field 3), so starttime (field 22) is index 22-3. See stack.parseStatStartTime.
func parseLeaderStartTime(line string) (uint64, error) {
	rparen := strings.LastIndexByte(line, ')')
	if rparen < 0 {
		return 0, errors.New("no comm terminator ')' in /proc stat line")
	}
	rest := strings.Fields(line[rparen+1:])
	const startTimeIndexAfterComm = 22 - 3
	if len(rest) <= startTimeIndexAfterComm {
		return 0, errors.New("too few fields after comm in /proc stat line")
	}
	return strconv.ParseUint(rest[startTimeIndexAfterComm], 10, 64)
}

// readRecordedGroups parses the stack.pgids record and returns the recorded
// process groups in start order, each with its pgid and start-time identity
// token. The record is
//
//	<version> <writerPid>
//	<component> <pgid> <starttime>
//	...
//
// (internal/stack/pgidfile.go writePgidFile). The const/type are package-
// internal to stack, so the test parses the fields directly rather than calling
// the unexported reader — a read-only parse of a stack-owned file, never a write.
func readRecordedGroups(t *testing.T, recordPath string) []recordedGroup {
	t.Helper()
	f, err := os.Open(recordPath)
	if err != nil {
		t.Fatalf("open stack.pgids record %q: %v", recordPath, err)
	}
	defer func() {
		// Read-only handle in a test; a close error here is not actionable.
		_ = f.Close()
	}()

	var groups []recordedGroup
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
		startTime, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			t.Fatalf("stack.pgids entry line %d has unparseable start time %q: %v", line, fields[2], err)
		}
		groups = append(groups, recordedGroup{pgid: pgid, startTime: startTime})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stack.pgids record %q: %v", recordPath, err)
	}
	return groups
}
