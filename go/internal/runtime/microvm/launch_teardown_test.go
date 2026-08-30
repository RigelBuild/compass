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

	// virtiofsd: record pid, touch its --socket-path=, then stay alive.
	writeFake(t, bin, "virtiofsd", `echo $$ > `+vfsPidFile+`
for a in "$@"; do case "$a" in --socket-path=*) : > "${a#--socket-path=}";; esac; done
sleep 30`)
	// passt: record pid, touch the path following --socket, then stay alive.
	writeFake(t, bin, "passt", `echo $$ > `+passtPidFile+`
p=""; for a in "$@"; do [ "$p" = --socket ] && : > "$a"; p="$a"; done
sleep 30`)
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

	// The daemons launch already started must have been reaped by the deferred
	// cleanup — no orphan left sleeping.
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
// fake vmm child plus a manually-started reaper mirroring launch's (the full
// launch needs passt/virtiofsd), since the assertion is purely about the
// reaper→vmmExited→WaitVMMExit path.
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
	if err := startChild(vm.vmm); err != nil {
		t.Fatalf("startChild(vmm fake): %v", err)
	}
	// The sole reaper, mirroring launch: it owns the single Wait and closes
	// vmmExited once the process has exited.
	vm.vmmExited = make(chan struct{})
	go func() {
		_ = vm.vmm.cmd.Wait() // reaper mirror: a fake VMM's exit is the expected outcome
		vm.vmm.waited.Store(true)
		close(vm.vmmExited)
	}()

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

// readPidFile reads a pid a fake wrote, retrying briefly since the fake writes
// it asynchronously after exec.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake never wrote its pid to %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
