//go:build podman

package main

// Cross-process teardown tests drive compass-stack up and down to prove a
// fresh process can stop the recorded stack. Teardown probes only recorded
// process groups and unique test paths; rootless-podman absence skips the test.

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

// downBudget bounds teardown above its escalation worst case while keeping a
// wedged down bounded.
const downBudget = 150 * time.Second

// answerPollInterval spaces socket probes without replacing the readiness event.
const answerPollInterval = 100 * time.Millisecond

// Postgres reports socket quiescence at SIGTERM entry, before exit; this bounds
// the remaining drain and still limits a wedged child.
const teardownConfirmBudget = 30 * time.Second

// TestCrossProcessTeardown proves a fresh down process stops a lingering stack
// through its persisted pgid record. It builds the binaries, runs up/down
// separately, checks group and socket teardown, then verifies down is idempotent.
func TestCrossProcessTeardown(t *testing.T) {
	if !podmanUsable(t) {
		t.Skip("rootless podman not usable in this environment")
	}

	ctx := t.Context()

	// Build child binaries together; subprocesses resolve them from PATH.
	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(t, binDir)

	// Resolve short paths so liveness probes fit system socket limits.
	fx, deps := newFixture(t, shortRoot(t, "-xp"))
	cfg := fx.cfg
	recordPath := filepath.Join(cfg.StateDir, pgidRecordName)

	// Cleanup runs down in a fresh process and is best-effort; the happy path
	// asserts explicit teardown results.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), downBudget)
		defer cancel()
		// Log cleanup harness failures instead of failing a completed test.
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

	// Up returns only after Ready, so exit 0 is the readiness gate.
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env, "up",
		"--state-dir", cfg.StateDir,
		"--socket", cfg.SocketPath,
		"--listen", cfg.ListenAddr,
		"--database", cfg.DatabaseDSN,
		"--image", cfg.AgentImage,
		"--runtime-dir", cfg.RuntimeDir,
		"--gateway-external", "http://127.0.0.1:4100",
		"--linger",
	)
	if err != nil {
		t.Fatalf("compass-stack up: %v\n%s", err, out)
	}

	// Up's success proves readiness; confirm cross-process socket access and record creation.
	waitServerAnswering(t, deps, cfg.SocketPath)
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("stack.pgids record %q missing after up (a lingering stack must leave a teardown record): %v", recordPath, err)
	}

	// Read recorded pgids and start times to detect dead or recycled processes.
	groups := readRecordedGroups(t, recordPath)
	if len(groups) == 0 {
		t.Fatal("stack.pgids recorded no process groups; a Ready stack spawned all three children")
	}

	// Down runs separately and must rely on the persisted record, not up handles.
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

	// Process-group death is authoritative: postgres may answer its probe at
	// SIGTERM before the postmaster exits. Wait for recorded groups to vanish
	// before socket checks; probes signal nothing and use only this stack's record.
	waitGroupsGone(t, groups, teardownConfirmBudget)

	// After groups exit, sockets are dark and successful teardown removes record.
	assertServerGone(t, t.Context(), deps, cfg.SocketPath)
	assertPostgresGone(t, fx.pgSock)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("stack.pgids record %q still present after a full down (full-success teardown removes it): stat err = %v", recordPath, err)
	}

	// A second down after teardown exercises the idempotent no-stack path.
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

// buildStackBinary builds the current compass-stack beside the child binaries,
// so the subprocess under test is not an installed command.
func buildStackBinary(t *testing.T, binDir string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	moduleRoot := filepath.Join(wd, "..", "..") // go/cmd/compass-stack -> go
	out := filepath.Join(binDir, "compass-stack")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", out, "./cmd/compass-stack")
	cmd.Dir = moduleRoot
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build compass-stack: %v\n%s", err, b)
	}
	return out
}

// stackEnv prepends built binaries and replaces inherited secret providers with
// a fixture-owned provider, avoiding dependence on the operator's host.
func stackEnv(t *testing.T, binDir string) []string {
	t.Helper()
	provider := seedMasterKeyProvider(t)
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	oldPath := ""
	for _, e := range base {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			oldPath = p
			continue
		}
		if strings.HasPrefix(e, "COMPASS_SECRET_PROVIDER=") {
			t.Logf("stackEnv: discarding the inherited COMPASS_SECRET_PROVIDER for the fixture's own throwaway dotenv provider")
			continue
		}
		out = append(out, e)
	}
	out = append(out, "COMPASS_SECRET_PROVIDER="+provider)
	return append(out, "PATH="+binDir+string(os.PathListSeparator)+oldPath)
}

// runStack returns combined output, the subprocess exit error, and a separate
// harness error for temp-file I/O. Output goes to a file rather than a pipe:
// lingering children inherit stdout, which would keep a pipe open and block
// cmd.Wait after the up process exits.
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
	ctx, cancel := context.WithTimeout(t.Context(), probeTimeout)
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

// recordedGroup pairs a child's pgid with its leader start time, preventing a
// recycled pid from being mistaken for the original process.
type recordedGroup struct {
	// component is the recorded component name, carried purely so a liveness
	// failure names which child survived instead of a bare pgid.
	component string
	pgid      int
	startTime uint64
}

// waitGroupsGone polls recorded process groups until teardown completes. A
// group is gone if absent or if its leader PID was recycled; the start-time
// check prevents a reused pid from appearing alive. Probes signal nothing and
// use only pgids from the stack's own record.
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
				t.Fatalf("recorded %s process group %d still alive %s after down; the cross-process teardown did not stop it", grp.component, grp.pgid, budget)
			}
			<-ticker.C
		}
	}
}

// ESRCH or a recycled leader means gone; a matching or unreadable leader may
// still have members. Probe with signal 0 and report unexpected errors.
func groupAlive(pgid int, startTime uint64) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return false, nil // gone
	case err != nil && !errors.Is(err, syscall.EPERM):
		return false, err // unexpected errno
	}
	got, rerr := readLeaderStartTime(pgid)
	if rerr != nil {
		// The group exists without a readable leader: orphaned members remain.
		return true, nil //nolint:nilerr // read failure on an existing group => orphaned => alive
	}
	return got == startTime, nil
}

// readLeaderStartTime reads field 22 after the last ')' in /proc stat because
// comm may contain spaces and parentheses; the offset is 22-3 after comm.
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

// readRecordedGroups reads process-group identities from v1 or v2 stack.pgids
// records. Container entries have no pgid; this test reads but never writes the
// stack-owned file because the production reader is package-private.
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
	containers := 0
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if line == 1 || text == "" {
			continue // header (line 1) or a blank line
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		// v2 records tag entries: proc <component> <pgid> <starttime>,
		// while ctr entries identify containers and have no process group.
		switch fields[0] {
		case "ctr":
			if len(fields) != 3 {
				t.Fatalf("stack.pgids container entry line %d malformed: %q", line, text)
			}
			containers++
			continue
		case "proc":
			if len(fields) != 4 {
				t.Fatalf("stack.pgids process entry line %d malformed: %q", line, text)
			}
			fields = fields[1:]
		default:
			// v1 records are untagged: <component> <pgid> <starttime>.
			if len(fields) != 3 {
				t.Fatalf("stack.pgids entry line %d malformed: %q", line, text)
			}
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("stack.pgids entry line %d has unparseable pgid %q: %v", line, fields[1], err)
		}
		startTime, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			t.Fatalf("stack.pgids entry line %d has unparseable start time %q: %v", line, fields[2], err)
		}
		groups = append(groups, recordedGroup{component: fields[0], pgid: pgid, startTime: startTime})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stack.pgids record %q: %v", recordPath, err)
	}
	// Explain container-only records here rather than implying a spawn failure.
	if len(groups) == 0 && containers > 0 {
		t.Fatalf("stack.pgids had %d entries, all container-kind; no process group to probe", containers)
	}
	return groups
}
