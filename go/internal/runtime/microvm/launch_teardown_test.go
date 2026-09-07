//go:build unix

package microvm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeFake writes an executable shell stub into dir under name.
func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// pidAlive reports whether pid names a live process (signal 0 probes without
// affecting it); a reaped process yields ESRCH.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestLaunchFailClosedTeardown pins the fail-closed teardown contract that CI
// surfaced (PR #574): when a later spawn fails after virtiofsd and passt are
// already up, launch must (1) return a nil VM and the error WITHOUT panicking,
// and (2) reap the daemons it already started, leaving no orphan. The original
// bug returned `nil, err` into the NAMED vm return, clobbering the handle before
// the deferred cleanup ran — so Shutdown nil-dereferenced (SIGSEGV) and the
// started daemons were orphaned. This runs with no KVM: the aux daemons are
// shell fakes that bind their sockets and sleep, and cloud-hypervisor is absent
// from PATH so LookPath fails after both aux daemons are up.
func TestLaunchFailClosedTeardown(t *testing.T) {
	bin := t.TempDir()
	run := t.TempDir()
	vfsPidFile := filepath.Join(run, "vfs.pid")
	passtPidFile := filepath.Join(run, "passt.pid")

	// The fakes must OUTLIVE launch, and the only absence this test wants is
	// cloud-hypervisor's — so `sleep` is resolved on the real PATH now, while it
	// is still intact, and burned into the stubs as an absolute path. Naming it
	// bare would make the stubs die on `sleep: command not found` the moment
	// PATH is narrowed below (there is no `sleep` builtin in /bin/sh), so both
	// daemons would exit on their own and "teardown reaped them" would be
	// trivially true — the RIG-3480 defect.
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("resolving sleep on PATH (the fakes need it to stay alive): %v", err)
	}
	// virtiofsd: record pid, touch its --socket-path=, then stay alive.
	writeFake(t, bin, "virtiofsd", `echo $$ > `+vfsPidFile+`
for a in "$@"; do case "$a" in --socket-path=*) : > "${a#--socket-path=}";; esac; done
`+sleepBin+` 30`)
	// passt: record pid, touch the path following --socket, then stay alive.
	writeFake(t, bin, "passt", `echo $$ > `+passtPidFile+`
p=""; for a in "$@"; do [ "$p" = --socket ] && : > "$a"; p="$a"; done
`+sleepBin+` 30`)
	// cloud-hypervisor deliberately absent → LookPath fails after aux are up.
	t.Setenv("PATH", bin)

	dir := t.TempDir()
	cfg := BootConfig{
		Kernel: "/nonexistent/kernel", Initrd: "/nonexistent/initrd", Rootfs: "/nonexistent/rootfs",
		VsockCID: 3, VsockPort: 1024, VsockSocket: dir + "/vsock.sock",
		FSTag: "workspace", FSSocket: dir + "/virtiofsd.sock", FSSharedDir: dir,
		CPUs: 2, MemoryMB: 1024,
		Net: NetConfig{VhostUserSocket: dir + "/net.sock", MAC: "12:34:56:78:9a:bc"},
	}

	vm, err := Launch(t.Context(), cfg) // must not panic
	if err == nil {
		t.Fatal("expected an error (cloud-hypervisor absent), got nil")
	}
	if vm != nil {
		t.Errorf("expected a nil VM on the error path, got %v", vm)
	}

	// The failure must be cloud-hypervisor's LookPath, which is the ONLY absence
	// this test arranges. Anything earlier — notably a fake that exited before
	// its socket was served — means the daemons never really came up, so the
	// no-orphan assertion below would pass vacuously. This control is what
	// caught RIG-3480: it fired on 39/40 runs of the merge result while the
	// assertion it guards stayed green.
	if !strings.Contains(err.Error(), "cloud-hypervisor") {
		t.Fatalf("launch failed before the cloud-hypervisor lookup (%v); the aux daemons never came up, "+
			"so the no-orphan assertion below would be vacuous", err)
	}

	// The daemons launch already started must have been reaped by the deferred
	// cleanup — no orphan left sleeping. Shutdown's reap Waits each child, so by
	// the time Launch has returned this is a settled fact, not a race to poll.
	for name, pidFile := range map[string]string{"virtiofsd": vfsPidFile, "passt": passtPidFile} {
		pid := readPidFile(t, pidFile)
		if pidAlive(pid) {
			t.Errorf("%s (pid %d) still alive after fail-closed launch — teardown orphaned it", name, pid)
		}
	}
}

// TestWaitVMMExitObservesPromptSelfExit pins M2's prompt-exit contract: a VMM
// that exits on its own (as the guest does on RB_POWER_OFF) is observed by
// WaitVMMExit via the sole reaper WELL UNDER the grace window — it must NOT burn
// the full timeout waiting on a zombie. It assembles a minimal VM with just a
// fake vmm child (the full launch needs passt/virtiofsd), since the assertion is
// purely about the startChild-reaper→exited→WaitVMMExit path.
func TestWaitVMMExitObservesPromptSelfExit(t *testing.T) {
	dir := t.TempDir()
	vm := &VM{
		vmm: &child{
			name:    "cloud-hypervisor",
			logPath: filepath.Join(dir, "cloud-hypervisor.log"),
			// A self-exiting VMM stand-in: sleep briefly, then exit 0.
			cmd: exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 0.1; exit 0"),
		},
	}
	// startChild installs the sole reaper itself, exactly as launch relies on;
	// the VM's channel is that child's, so there is no second Wait anywhere.
	if err := startChild(vm.vmm); err != nil {
		t.Fatalf("startChild(vmm fake): %v", err)
	}
	vm.vmmExited = vm.vmm.exited

	// A generous grace window: the fake exits in ~100ms, so WaitVMMExit must
	// return true well before this elapses. Measure to prove it did not burn
	// the timeout on a zombie.
	const grace = 10 * time.Second
	start := time.Now()
	if !vm.WaitVMMExit(grace) {
		t.Fatalf("WaitVMMExit returned false: a self-exiting VMM was not observed within %v", grace)
	}
	if elapsed := time.Since(start); elapsed >= grace {
		t.Fatalf("WaitVMMExit took %v (>= grace %v): the self-exit was not observed promptly", elapsed, grace)
	}
}

// TestWaitForSocketsFailsFastOnADeadDaemon is HIGH-2's regression lock: the
// readiness wait must be LIVENESS-aware, not path-existence-only.
//
// The fake reproduces virtiofsd's real, verified ordering — it BINDS its socket
// and only then hits the id-map step that can fail, exiting non-zero with its
// diagnostic on stderr. (Measured on virtiofsd 1.14.0: a bad gid base left
// `srwx------ .../sb.sock` on disk with the daemon exited 1.) A path-only poll
// therefore Stats a socket that exists, returns nil, and launch starts
// cloud-hypervisor against a corpse; the operator then sees a vhost-user
// negotiation error instead of "couldn't setup id mappings".
//
// Three properties, each of which the pre-fix implementation fails:
//  1. an ERROR rather than nil, even though the socket path exists;
//  2. the error NAMES the daemon and carries its log tail, so virtiofsd's own
//     line reaches the operator;
//  3. it returns FAST — well inside socketReadyTimeout, i.e. it short-circuits
//     on the exit rather than polling out the full window.
func TestWaitForSocketsFailsFastOnADeadDaemon(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "sb.sock")
	// virtiofsd's own line, apostrophe included. It travels as an ENV VAR, not
	// inline in the -c script: embedded in single quotes the apostrophe would
	// terminate the quoting and the fake would die before binding its socket,
	// silently defeating the whole reproduction.
	const diagnostic = "Couldn't setup id mappings: newgidmap failed"

	c := &child{
		name:    "virtiofsd",
		logPath: filepath.Join(dir, "virtiofsd.log"),
		// Bind first, THEN fail — virtiofsd's actual ordering.
		cmd: exec.CommandContext(t.Context(), "/bin/sh", "-c",
			": > \"$SOCKET\"; printf '%s\\n' \"$DIAGNOSTIC\" >&2; exit 1"),
	}
	c.cmd.Env = append(os.Environ(), "SOCKET="+socket, "DIAGNOSTIC="+diagnostic)
	if err := startChild(c); err != nil {
		t.Fatalf("startChild(dead-daemon fake): %v", err)
	}
	// Await the reaper so the poll enters with the exit already observable;
	// otherwise this test would race the fake's own exit.
	<-c.exited

	// The socket the dead daemon left behind: the exact condition that made the
	// old path-existence poll return nil.
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the fake did not leave its socket on disk (%v); the test would not reproduce the defect", err)
	}

	start := time.Now()
	err := waitForSockets(t.Context(), []string{socket}, []*child{c}, socketReadyTimeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("waitForSockets = nil for a daemon that exited after binding its socket; " +
			"launch would start cloud-hypervisor against a DEAD virtiofsd")
	}
	if !strings.Contains(err.Error(), "virtiofsd") {
		t.Errorf("error %q does not name the daemon", err.Error())
	}
	if !strings.Contains(err.Error(), diagnostic) {
		t.Errorf("error %q does not carry the daemon's own log tail (%q); the real cause would be lost", err.Error(), diagnostic)
	}
	// Fast: the exit short-circuits the poll instead of burning the window.
	if elapsed > socketReadyTimeout/2 {
		t.Errorf("waitForSockets took %v of a %v budget; a dead child must short-circuit the poll", elapsed, socketReadyTimeout)
	}
}

// TestWaitForSocketsSucceedsForALiveDaemon is the non-vacuity control for the
// liveness check: a child that binds its socket and STAYS UP must still be
// accepted. Without it the assertion above would pass on a waitForSockets that
// had simply started erroring unconditionally.
func TestWaitForSocketsSucceedsForALiveDaemon(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "live.sock")
	c := &child{
		name:    "virtiofsd",
		logPath: filepath.Join(dir, "virtiofsd.log"),
		cmd:     exec.CommandContext(t.Context(), "/bin/sh", "-c", ": > "+socket+"; sleep 30"),
	}
	if err := startChild(c); err != nil {
		t.Fatalf("startChild(live fake): %v", err)
	}
	t.Cleanup(func() {
		if err := reap(c); err != nil {
			t.Errorf("reaping the live fake: %v", err)
		}
	})

	if err := waitForSockets(t.Context(), []string{socket}, []*child{c}, socketReadyTimeout); err != nil {
		t.Fatalf("waitForSockets over a LIVE daemon that bound its socket = %v, want nil", err)
	}
}

// readPidFile reads the pid a fake recorded. It does NOT poll: each fake writes
// its pid BEFORE it touches its socket, and the caller has already confirmed
// launch got past the socket wait, which orders the write before this read. So a
// missing or malformed pidfile here is a real defect in that ordering rather
// than a race to wait out, and it fails immediately instead of behind a
// wall-clock budget that can only ever expire (RIG-3480).
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the pid the fake recorded at %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pidfile %s holds %q, not a pid: %v", path, string(raw), err)
	}
	return pid
}
