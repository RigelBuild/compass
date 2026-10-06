//go:build podman

package e2e

import (
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// TestExecStreamingStopKillsInContainerProcess pins that terminating a podman
// streaming exec ends the processes inside the container, not only the host-side
// `podman exec -i` client. The exec forks a background child, so a fix that
// kills only the direct exec process still leaves a survivor.
func TestExecStreamingStopKillsInContainerProcess(t *testing.T) {
	cli, id, name := startKeepAlive(t)

	script := "trap '' TERM HUP; sleep 3001 & exec sleep 3000"
	stream, err := cli.ExecStreaming(t.Context(), id, agentExecSpec("sh", "-c", script))
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}
	waitForTop(t, name, func(procs []string) bool {
		return hasProcess(procs, "sleep 3000") && hasProcess(procs, "sleep 3001")
	})

	if err := stream.Process.Terminate(); err != nil && !isClientKill(err) {
		t.Fatalf("Terminate: unexpected error %v", err)
	}
	waitForTop(t, name, func(procs []string) bool {
		return !hasProcess(procs, "sleep 3000") && !hasProcess(procs, "sleep 3001")
	})
}

// TestExecStreamingStopSweepsDetachedSession verifies that Stop kills work in
// a new session without killing the container's PID 1 keep-alive.
func TestExecStreamingStopSweepsDetachedSession(t *testing.T) {
	cli, id, name := startKeepAlive(t)

	script := `bun -e 'require("child_process").spawn("sleep", ["3002"], {detached: true, stdio: "ignore"}).unref()'; exec sleep 3000`
	stream, err := cli.ExecStreaming(t.Context(), id, agentExecSpec("sh", "-c", script))
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}
	waitForTop(t, name, func(procs []string) bool {
		return hasProcess(procs, "sleep 3000") && hasProcess(procs, "sleep 3002")
	})

	if err := stream.Process.Terminate(); err != nil && !isClientKill(err) {
		t.Fatalf("Terminate: unexpected error %v", err)
	}
	waitForTop(t, name, func(procs []string) bool {
		return !hasProcess(procs, "sleep 3000") && hasProcess(procs, "sleep 3002")
	})

	sweeper, ok := any(cli).(runtime.SessionSweeper)
	if !ok {
		t.Fatal("PodmanCLI does not implement runtime.SessionSweeper")
	}
	if err := sweeper.SweepExecSessions(t.Context(), id, strconv.FormatUint(uint64(agentuid.AgentUID), 10)); err != nil {
		t.Fatalf("SweepExecSessions: %v", err)
	}
	waitForTop(t, name, func(procs []string) bool {
		return !hasProcess(procs, "sleep 3000") && !hasProcess(procs, "sleep 3002") && hasProcess(procs, "sleep infinity")
	})
	running, err := cli.Running(t.Context(), name)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !running {
		t.Fatal("container stopped during exec-session sweep")
	}
}

// TestExecStreamingNaturalExitKeepsStatus pins that an exec which exits on its
// own, with stdin still open, returns promptly with its own exit code.
func TestExecStreamingNaturalExitKeepsStatus(t *testing.T) {
	cli, id, _ := startKeepAlive(t)

	stream, err := cli.ExecStreaming(t.Context(), id, agentExecSpec("sh", "-c", "exit 3"))
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- stream.Process.Wait() }()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("exec did not return after its command exited")
	}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 3 {
		t.Fatalf("Wait = %v, want exit status 3", err)
	}
}

func agentExecSpec(command ...string) runtime.StreamingExecSpec {
	return runtime.NewStreamingExecSpec(command...).AsUser(strconv.FormatUint(uint64(agentuid.AgentUID), 10))
}

// startKeepAlive starts an agent-image container running the production
// `sleep infinity` keep-alive and removes it at cleanup.
func startKeepAlive(t *testing.T) (*runtime.PodmanCLI, runtime.WorkloadID, string) {
	t.Helper()
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}
	cli := runtime.NewPodmanCLI()
	name := "compass-e2e-exec-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		if out, err := exec.Command("podman", "rm", "--force", "--time", "0", name).CombinedOutput(); err != nil {
			t.Logf("cleanup podman rm %s: %v: %s", name, err, out)
		}
	})
	id, err := cli.Create(t.Context(), runtime.WorkloadSpec{
		Image:   agentImage,
		Name:    name,
		UID:     agentuid.AgentUID,
		Command: []string{"sleep", "infinity"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cli.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return cli, id, name
}

// isClientKill reports the deliberate SIGKILL of the host-side exec client.
func isClientKill(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	return ok && exitErr.ExitCode() == -1
}

func hasProcess(procs []string, args string) bool {
	return slices.ContainsFunc(procs, func(p string) bool { return strings.Contains(p, args) })
}

// listProcs prints "pid state args" per container process from /proc. The
// agent image has no ps, and `podman top` exits 125 in the CI e2e container.
const listProcs = `for d in /proc/[0-9]*; do read -r s < "$d/stat" || continue; set -f -- $s; echo "${d#/proc/} $3 $(tr '\0' ' ' < "$d/cmdline")"; done 2>/dev/null`

// waitForTop polls the container's process list until done accepts the live
// processes. Zombies are excluded: the keep-alive never reaps an orphan.
func waitForTop(t *testing.T, name string, done func([]string) bool) {
	t.Helper()
	var procs []string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		out, err := exec.Command("podman", "exec", name, "sh", "-c", listProcs).CombinedOutput()
		if err != nil {
			t.Fatalf("list processes in %s: %v: %s", name, err, out)
		}
		procs = liveProcesses(string(out))
		if done(procs) {
			return
		}
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // bounded poll tick on the process list with a deadline (rule://go-no-sleep-in-test poll-until exemption)
	}
	t.Fatalf("container %s: live processes %q never reached the expected state", name, procs)
}

func liveProcesses(list string) []string {
	var procs []string
	for line := range strings.Lines(list) {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] == "Z" {
			continue
		}
		procs = append(procs, strings.TrimSpace(line))
	}
	return procs
}
