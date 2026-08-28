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
// t.TempDir() root (which embeds the test-function name, e.g. the 36-char
// TestMicroVMStartFailureLeavesNoState → a ~111-byte socket path) overflows
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
