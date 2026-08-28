//go:build microvm && unix

package runtime

// The KVM-gated microVM lifecycle e2e suite (record §T4/U4): it drives a real
// MicroVMRuntime through Create→Start→Exec→Stop→Remove on live hardware, and
// asserts the fail-closed Start-teardown and graceful-Stop invariants. Every
// test calls microvmtest.Require(t) first, mirroring
// microvm/boot_microvm_test.go: on a KVM-less box it SKIPS (unless
// COMPASS_REQUIRE_MICROVM=1 forces a hard fail), so the suite is only real where
// /dev/kvm is openable and the guest images are exported into the env.

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// e2eConfig builds a MicroVMConfig from the resolved test env and a fresh, SHORT
// runroot, so every session's sockets live inside the test's own scratch tree
// and are removed with it. The runroot must be short because the per-session
// socket paths under it are AF_UNIX sun_path-budgeted: the widest is
// <RunRoot>/microvm/<32-hex session id>/virtiofsd.sock, a 56-byte tail, so a
// t.TempDir() root (which embeds the test-function name, e.g. the 39-char
// TestMicroVMExecStreamingKillSignalsExit → a 114-byte socket path) overflows
// the 107-byte Linux cap and virtiofsd's bind(2) fails with a bare EINVAL —
// the socket never appears and the boot times out. A short fixed root keeps the
// worst-case path well under the cap. Production runroots are short and
// startup-budget-checked (run.go validateRuntimeDir); this mirrors that.
func e2eConfig(t *testing.T, env microvmtest.Env) MicroVMConfig {
	t.Helper()
	//nolint:usetesting // t.TempDir embeds the long test-function name, which overflows the 107-byte AF_UNIX sun_path budget for the per-session virtiofsd/vsock/net sockets — the very failure this short root prevents.
	runRoot, err := os.MkdirTemp("", "cvm")
	if err != nil {
		t.Fatalf("creating short microvm runroot: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(runRoot); err != nil {
			t.Errorf("removing microvm runroot %s: %v", runRoot, err)
		}
	})
	return MicroVMConfig{
		VMMPath:         env.VMMPath,
		VirtiofsdPath:   env.VirtiofsdPath,
		KernelImage:     env.KernelImage,
		RootfsImage:     env.RootfsImage,
		InitrdImage:     env.InitrdImage,
		RunRoot:         runRoot,
		DefaultCPUs:     2,
		DefaultMemoryMB: 1024,
	}
}

// TestMicroVMLifecycleEndToEnd is the U4 deliverable: allocate a session
// (Create, no boot), boot + provision it (Start), run a command capturing its
// output (Exec echo), stop it gracefully (Stop), and remove it (Remove) — the
// full ContainerRuntime verb sequence against a live guest.
func TestMicroVMLifecycleEndToEnd(t *testing.T) {
	env := microvmtest.Require(t)
	m := NewMicroVMRuntime(e2eConfig(t, env))

	workspace := t.TempDir()
	spec := ContainerSpec{
		Name:   "e2e-agent",
		UID:    1000,
		Mounts: []Mount{{HostPath: workspace, ContainerPath: "/workspace"}},
	}

	id, err := m.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Remove is the backstop teardown even if a later step fails midway.
	t.Cleanup(func() {
		if rmErr := m.Remove(context.WithoutCancel(t.Context()), id); rmErr != nil {
			t.Errorf("Remove (cleanup): %v", rmErr)
		}
	})

	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	out, err := m.Exec(t.Context(), id, NewExecSpec("echo", "hello-guest").AsUser("1000"))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !out.Success() {
		t.Fatalf("Exec exit = %d, stderr = %q, want exit 0", out.ExitCode, out.Stderr)
	}
	if got := out.Stdout; got != "hello-guest\n" {
		t.Fatalf("Exec stdout = %q, want %q", got, "hello-guest\n")
	}

	if err := m.Stop(t.Context(), id, 10*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := m.session(id); err == nil {
		t.Fatal("session still in table after Remove")
	}
}

// TestMicroVMStartFailureLeavesNoState is the transactional-Start negative: a
// Start against a bad rootfs path must tear down its partial boot (no orphan
// processes) AND leave the session in a not-started state so a subsequent Remove
// still cleans up its runtime dir. The runtime dir itself is created by Create
// and removed by Remove; Start's teardown covers the booted VM, not the dir.
func TestMicroVMStartFailureLeavesNoState(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := e2eConfig(t, env)
	// A raw-sized but non-erofs rootfs: CH boots the kernel, then the initrd's
	// erofs mount fails-closed — the guest never reaches Health, so Start's boot
	// deadline elapses and it tears the partial boot down (mirrors
	// microvm/boot_microvm_test.go TestCorruptRootfsFailsClosed).
	corrupt := t.TempDir() + "/corrupt.erofs"
	f, createErr := os.Create(corrupt)
	if createErr != nil {
		t.Fatalf("creating corrupt rootfs: %v", createErr)
	}
	if truncErr := f.Truncate(16 << 20); truncErr != nil {
		_ = f.Close() // cleanup on an already-failing setup path
		t.Fatalf("sizing corrupt rootfs: %v", truncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		t.Fatalf("closing corrupt rootfs: %v", closeErr)
	}
	cfg.RootfsImage = corrupt
	m := NewMicroVMRuntime(cfg)

	workspace := t.TempDir()
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   "e2e-badboot",
		UID:    1000,
		Mounts: []Mount{{HostPath: workspace, ContainerPath: "/workspace"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A short deadline: the boot can never become healthy, so bound the wait
	// well under the -timeout rather than burning the full 60 s bootDeadline.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if startErr := m.Start(ctx, id); startErr == nil {
		t.Fatal("Start against a corrupt rootfs succeeded; want a fail-closed error")
	}

	// The session must not be marked started (no exec client), and Remove must
	// still clean it up idempotently.
	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session missing after a failed Start: %v", err)
	}
	if session.guestExec != nil {
		t.Fatal("session has an exec client after a failed Start; Start must not open the gate on a torn-down boot")
	}
	if rmErr := m.Remove(t.Context(), id); rmErr != nil {
		t.Fatalf("Remove after a failed Start: %v", rmErr)
	}
	if _, statErr := os.Stat(session.runtimeDir); !os.IsNotExist(statErr) {
		t.Fatalf("runtime dir %s not removed after Remove (stat err %v)", session.runtimeDir, statErr)
	}
}

// TestMicroVMExecStreamingKillSignalsExit exercises U4's ExecStreaming wiring
// live (M3's OQ-G contract end to end): start a long-running streaming command,
// Kill it via the ChildHandle, and assert Wait maps the guest's signalled exit
// onto a *ExitStatusError with a non-zero Signal — the kill/wait/stream path the
// hermetic TestExitErrorMapping cannot reach.
func TestMicroVMExecStreamingKillSignalsExit(t *testing.T) {
	env := microvmtest.Require(t)
	m := NewMicroVMRuntime(e2eConfig(t, env))

	workspace := t.TempDir()
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   "e2e-stream-kill",
		UID:    1000,
		Mounts: []Mount{{HostPath: workspace, ContainerPath: "/workspace"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = m.Remove(t.Context(), id) })

	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream, err := m.ExecStreaming(t.Context(), id, StreamingExecSpec{
		Command: []string{"sleep", "300"},
		User:    strPtr("1000"),
	})
	if err != nil {
		t.Fatalf("ExecStreaming: %v", err)
	}
	// Drain stdout/stderr so the stream is not blocked on a full pipe while we
	// wait for the kill to land.
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stderr) }()

	if err := stream.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	err = stream.Process.Wait()
	var exitStatus *ExitStatusError
	if !errors.As(err, &exitStatus) {
		t.Fatalf("Wait error = %v (%T), want *ExitStatusError", err, err)
	}
	if exitStatus.Signal == 0 {
		t.Fatalf("ExitStatusError.Signal = 0, want a non-zero kill signal (%+v)", exitStatus)
	}
}
