//go:build unix

package microvm

import (
	"context"
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

// TestLaunchFailClosedTeardown ensures a later spawn failure returns an error
// without panicking and reaps already-started daemons. Shell fakes provide the
// daemons; cloud-hypervisor is absent so the failure occurs after both start.
func TestLaunchFailClosedTeardown(t *testing.T) {
	bin := t.TempDir()
	run := t.TempDir()
	vfsPidFile := filepath.Join(run, "vfs.pid")
	passtPidFile := filepath.Join(run, "passt.pid")

	// Resolve sleep before narrowing PATH so the fake daemons stay alive until
	// launch's deferred teardown; otherwise their early exit makes the no-orphan
	// assertion pass vacuously.
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("resolving sleep on PATH (the fakes need it to stay alive): %v", err)
	}
	// Each fake records its pid, binds its socket, then execs sleep so teardown
	// signals the recorded process rather than leaving a child behind.
	writeFake(t, bin, "virtiofsd", longLivedFakeBody(vfsPidFile,
		`for a in "$@"; do case "$a" in --socket-path=*) : > "${a#--socket-path=}";; esac; done`,
		sleepBin, "30"))
	writeFake(t, bin, "passt", longLivedFakeBody(passtPidFile,
		`p=""; for a in "$@"; do [ "$p" = --socket ] && : > "$a"; p="$a"; done`,
		sleepBin, "30"))
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

	// Require the cloud-hypervisor lookup failure. A broader error match could
	// pass when a fake exits before serving its socket, making the no-orphan
	// assertion vacuous.
	if !strings.Contains(err.Error(), "resolving cloud-hypervisor on PATH") {
		t.Fatalf("launch failed before the cloud-hypervisor lookup (%v); the aux daemons never came up, "+
			"so the no-orphan assertion below would be vacuous", err)
	}

	// Deferred Shutdown waits for each child's sole reaper, so the no-orphan
	// check below observes settled process state rather than racing a poll.
	for name, pidFile := range map[string]string{"virtiofsd": vfsPidFile, "passt": passtPidFile} {
		pid := readPidFile(t, pidFile)
		if pidAlive(pid) {
			t.Errorf("%s (pid %d) still alive after fail-closed launch — teardown orphaned it", name, pid)
		}
	}
}

// longLivedStayAlive builds shell text that runs setup and execs its tail, so
// teardown signals the process Go started instead of orphaning a shell child.
func longLivedStayAlive(setup, tailBin string, tailArgs ...string) string {
	tail := strings.Join(append([]string{tailBin}, tailArgs...), " ")
	return setup + "\nexec " + tail
}

// longLivedFakeBody is longLivedStayAlive plus the pid record the fail-closed
// teardown test reads back.
func longLivedFakeBody(pidFile, setup, tailBin string, tailArgs ...string) string {
	return "echo $$ > " + pidFile + "\n" + longLivedStayAlive(setup, tailBin, tailArgs...)
}

// TestLongLivedStayAliveExecsItsTail pins the property above at the point it is
// decided, for both constructors.
//
// It cannot be asserted on a running stub without a poll. The only observable
// difference is /proc/<pid>/comm after the exec, and there is no happens-before
// edge to that moment: a readiness signal a stub emits necessarily precedes its
// own exec. A post-teardown residue scan can see the leak itself, but it
// reports on whichever stubs a test happens to start; this reports on the
// string every long-lived stub in the file is built from, which is the property
// that actually has to hold. Both constructors are covered so neither can drift
// away from the other.
func TestLongLivedStayAliveExecsItsTail(t *testing.T) {
	for name, body := range map[string]string{
		"longLivedStayAlive": longLivedStayAlive(": > /tmp/sock", "/bin/sleep", "30"),
		"longLivedFakeBody":  longLivedFakeBody("/tmp/x.pid", ": > /tmp/sock", "/bin/sleep", "30"),
	} {
		lines := strings.Split(body, "\n")
		tail := lines[len(lines)-1]
		if !strings.HasPrefix(tail, "exec ") {
			t.Errorf("%s tail is %q, want an `exec ` prefix — without it the shell forks the tail "+
				"and teardown cannot reach the process that stays alive", name, tail)
		}
	}
	// The pid record must come first, since the teardown test reads it back.
	if body := longLivedFakeBody("/tmp/x.pid", ": > /tmp/sock", "/bin/sleep", "30"); !strings.HasPrefix(
		body, "echo $$ > /tmp/x.pid\n") {
		t.Errorf("longLivedFakeBody must record its pid first, got %q", body)
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

// A zero-timeout poll is how liveness is read; a select racing an already
// closed exit channel against time.After(0) would intermittently say "alive".
func TestWaitVMMExitZeroTimeoutSeesExitedVMM(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	vm := &VM{vmm: &child{name: "cloud-hypervisor"}, vmmExited: exited}
	for i := range 1000 {
		if !vm.WaitVMMExit(0) {
			t.Fatalf("WaitVMMExit(0) on an exited VMM = false at iteration %d", i)
		}
	}
	live := &VM{vmm: &child{name: "cloud-hypervisor"}, vmmExited: make(chan struct{})}
	if live.WaitVMMExit(0) {
		t.Fatal("WaitVMMExit(0) on a live VMM = true")
	}
}

// TestWaitForSocketsFailsFastOnADeadDaemon is HIGH-2's regression lock: the
// readiness wait must be LIVENESS-aware, not path-existence-only.
//
// The fake reproduces virtiofsd's real, verified ordering — it BINDS its socket
// and only then hits the sandbox setup that can fail, exiting non-zero with its
// diagnostic on stderr. (Measured on virtiofsd 1.14.0: a failed sandbox setup
// left `srwx------ .../sb.sock` on disk with the daemon exited 1.) A path-only
// poll therefore Stats a socket that exists, returns nil, and launch starts
// cloud-hypervisor against a corpse; the operator then sees a vhost-user
// negotiation error instead of virtiofsd's own sandbox error.
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
	const diagnostic = "Couldn't set up the sandbox: Operation not permitted"

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
	// Check the error's cause slot too: daemon name and log tail can both be
	// present around a broken %w(<nil>) marker. Startup ExitError must retain its
	// exit status instead of being suppressed as expected teardown.
	if strings.Contains(err.Error(), "%!") {
		t.Errorf("error %q carries a Go formatting-verb error; the death cause was fed a nil", err.Error())
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("error %q does not report the daemon's exit status; an operator cannot tell "+
			"a non-zero exit from a clean one", err.Error())
	}
	// A dead child must short-circuit the poll instead of burning the window.
	if elapsed > socketReadyTimeout/2 {
		t.Errorf("waitForSockets took %v of a %v budget; a dead child must short-circuit the poll", elapsed, socketReadyTimeout)
	}
}

// TestWaitForSocketsSucceedsForALiveDaemon prevents the liveness check from
// passing vacuously if waitForSockets starts rejecting every child.
func TestWaitForSocketsSucceedsForALiveDaemon(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "live.sock")
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("resolving sleep on PATH (the fake needs it to stay alive): %v", err)
	}
	// Use longLivedStayAlive so reap signals the actual process, not a shell
	// parent that would orphan its child when killed.
	c := &child{
		name:    "virtiofsd",
		logPath: filepath.Join(dir, "virtiofsd.log"),
		cmd:     exec.CommandContext(t.Context(), "/bin/sh", "-c", longLivedStayAlive(": > "+socket, sleepBin, "30")),
	}
	if err := startChild(c); err != nil {
		t.Fatalf("startChild(live fake): %v", err)
	}
	t.Cleanup(func() {
		if err := reap(context.WithoutCancel(t.Context()), c); err != nil {
			t.Errorf("reaping the live fake: %v", err)
		}
	})

	if err := waitForSockets(t.Context(), []string{socket}, []*child{c}, socketReadyTimeout); err != nil {
		t.Fatalf("waitForSockets over a LIVE daemon that bound its socket = %v, want nil", err)
	}
}

// A done ctx must cut reap's SIGTERM grace short, not be ignored: a daemon that
// ignores SIGTERM is SIGKILLed at once and Shutdown returns well inside reapGrace.
func TestShutdownHonorsDoneContextOverReapGrace(t *testing.T) {
	dir := t.TempDir()
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("resolving sleep on PATH: %v", err)
	}
	// The fake reports through a FIFO once its trap is set, so SIGTERM cannot
	// land before the trap does. An ignored signal stays ignored across exec.
	ready := filepath.Join(dir, "ready")
	if err := syscall.Mkfifo(ready, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	c := &child{
		name:    "virtiofsd",
		logPath: filepath.Join(dir, "virtiofsd.log"),
		cmd: exec.CommandContext(t.Context(), "/bin/sh", "-c",
			longLivedStayAlive(`trap '' TERM; echo > "$READY"`, sleepBin, "300")),
	}
	c.cmd.Env = append(os.Environ(), "READY="+ready)
	if err := startChild(c); err != nil {
		t.Fatalf("startChild: %v", err)
	}
	if _, err := os.ReadFile(ready); err != nil {
		t.Fatalf("awaiting the fake's trap: %v", err)
	}
	vm := &VM{virtiofsd: c}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	if err := vm.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown under a done ctx = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed >= reapGrace {
		t.Fatalf("Shutdown under a done ctx took %v (>= reapGrace %v): it ignored ctx", elapsed, reapGrace)
	}
	select {
	case <-c.exited:
	default:
		t.Fatal("Shutdown returned before the daemon was reaped")
	}
}

// t.Context() is cancelled before cleanups run, so a Shutdown in t.Cleanup
// reaps a daemon whose launch ctx is gone. A clean exit then yields ctx.Err().
func TestReapToleratesACancelledLaunchContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	c := &child{
		name:    "virtiofsd",
		logPath: filepath.Join(dir, "virtiofsd.log"),
		cmd:     exec.CommandContext(ctx, "/bin/sh", "-c", "exec sleep 0.2"),
	}
	// A no-op Cancel lets the daemon exit 0 after ctx is done; Wait then reports
	// ctx.Err() (the os/exec Cmd.Cancel contract).
	c.cmd.Cancel = func() error { return nil }
	if err := startChild(c); err != nil {
		t.Fatalf("startChild: %v", err)
	}
	cancel()
	<-c.exited
	if err := reap(t.Context(), c); err != nil {
		t.Fatalf("reap after the launch ctx was cancelled = %v, want nil", err)
	}
}

// TestDeathErrorReportsTheCauseForEveryStartupPhase locks the death-cause slot
// in the shared BUILDER, for every phase the callers pass. It does not prove a
// callsite still delegates to the builder — the socket site is covered by
// TestWaitForSocketsFailsFastOnADeadDaemon and the pidfile site by
// TestStartRecordedChildNamesADeadChildNotAProcPath, both of which assert the
// slot through production. The VMM site at launch.go has no test of its own.
func TestDeathErrorReportsTheCauseForEveryStartupPhase(t *testing.T) {
	for _, phase := range allStartupPhases() {
		t.Run(string(phase), func(t *testing.T) {
			dir := t.TempDir()
			const diagnostic = "Couldn't set up the sandbox: Operation not permitted"
			c := &child{
				name:    "virtiofsd",
				logPath: filepath.Join(dir, "virtiofsd.log"),
				cmd: exec.CommandContext(t.Context(), "/bin/sh", "-c",
					"printf '%s\\n' \"$DIAGNOSTIC\" >&2; exit 1"),
			}
			c.cmd.Env = append(os.Environ(), "DIAGNOSTIC="+diagnostic)
			if err := startChild(c); err != nil {
				t.Fatalf("startChild(dead-daemon fake): %v", err)
			}
			<-c.exited // the cause is only settled once the reaper has run

			got := deathError(c, phase).Error()
			if strings.Contains(got, "%!") {
				t.Errorf("error %q carries a Go formatting-verb error; the death cause was fed a nil", got)
			}
			if !strings.Contains(got, "exit status 1") {
				t.Errorf("error %q does not report the daemon's exit status; an operator cannot "+
					"tell a non-zero exit from a clean one", got)
			}
			if !strings.Contains(got, "virtiofsd") {
				t.Errorf("error %q does not name the daemon", got)
			}
			if !strings.Contains(got, string(phase)) {
				t.Errorf("error %q does not name the phase it failed to reach (%q)", got, phase)
			}
			if !strings.Contains(got, diagnostic) {
				t.Errorf("error %q does not carry the daemon's own log tail (%q)", got, diagnostic)
			}
		})
	}
}

// readPidFile does not poll: fakes record their pid before touching the socket,
// and launch passing the socket wait orders that write before this read. A
// missing or malformed file therefore indicates a real ordering defect.
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
