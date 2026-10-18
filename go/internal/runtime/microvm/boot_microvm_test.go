//go:build microvm && unix

// KVM-gated boot tests for cloud-hypervisor, guestd's hybrid-vsock handshake,
// process PSS, and clean teardown. The suite skips without /dev/kvm unless
// COMPASS_REQUIRE_MICROVM=1. The net-only smoke isolates vhost-user negotiation
// failures from full-stack boot timeouts; it runs first only in unshuffled order.

package microvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

const (
	// testVsockCID is the guest CID (must be >= 3; 0-2 are reserved).
	testVsockCID uint32 = 3
	// testVsockPort is the guest port guestd serves the handshake on. It is
	// carried into the guest via the appended compass.vsock_port= cmdline
	// parameter, which guestd reads from /proc/cmdline (T2).
	testVsockPort uint32 = 1024
	// fullBootDeadline bounds Launch→Health-OK for the full boot (record §T4:
	// 60 s).
	fullBootDeadline = 60 * time.Second
	// negativeBootDeadline bounds the corrupt-rootfs negative: the boot must
	// fail-closed inside this window, not hang to the outer -timeout.
	negativeBootDeadline = 60 * time.Second
	// dhcpLeaseDeadline bounds the net-only smoke's wait for the guest to
	// acquire its address (observable in the serial console).
	dhcpLeaseDeadline = 60 * time.Second
)

// bootConfig creates an isolated temporary tree for sockets, pidfiles, logs,
// and the shared dir. Tests assert only that the mount succeeded; V2b checks
// shared-file content.
func bootConfig(t *testing.T, env microvmtest.Env, cpus, memoryMB int) BootConfig {
	t.Helper()
	dir := t.TempDir()
	return BootConfig{
		Kernel:      env.KernelImage,
		Initrd:      env.InitrdImage,
		Rootfs:      env.RootfsImage,
		VsockCID:    testVsockCID,
		VsockPort:   testVsockPort,
		VsockSocket: dir + "/vsock.sock",
		FSTag:       "workspace",
		FSSocket:    dir + "/virtiofsd.sock",
		FSSharedDir: dir, // a throwaway shared tree; content round-trip is V2b's
		CPUs:        cpus,
		MemoryMB:    memoryMB,
		Net: NetConfig{
			VhostUserSocket: dir + "/net.sock",
			MAC:             "12:34:56:78:9a:bc",
		},
	}
}

// TestNetOnlyBootSmoke isolates OQ-G: it boots with the vhost-user-net device
// but WITHOUT virtio-fs or vsock, and asserts only that the passt×CH vhost-user
// negotiation succeeded and the guest acquired its DHCP-delivered address — read
// from the captured serial console (guestd logs "network provisioned … addr=…").
// The guest boot then fails-closed at the absent workspace mount; that is
// expected and irrelevant here. On failure the serial console + daemon logs are
// surfaced so the cause is visible, not an opaque timeout — the STOP-condition
// evidence the driver needs to decide the pre-authorized gvproxy fallback.
func TestNetOnlyBootSmoke(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := bootConfig(t, env, 1, 512)

	vm, err := LaunchNetOnly(t.Context(), cfg)
	if err != nil {
		t.Fatalf("LaunchNetOnly failed: %v", err)
	}
	t.Cleanup(func() {
		if shutErr := vm.Shutdown(context.WithoutCancel(t.Context())); shutErr != nil {
			t.Errorf("Shutdown: %v", shutErr)
		}
	})

	// Gate on the guest logging its acquired address to the serial console — the
	// proof the vhost-user link negotiated and DHCP delivered the host-controlled
	// lease. Poll the console (there is no in-band event to receive on: the guest
	// has no vsock here), bounded by dhcpLeaseDeadline.
	leased := waitForConsole(t, vm, dhcpLeaseDeadline, func(console string) bool {
		return strings.Contains(console, "network provisioned") && strings.Contains(console, guestAddr)
	})
	if !leased {
		t.Fatalf("guest did not acquire its address within %s — passt×cloud-hypervisor "+
			"vhost-user negotiation likely failed (OQ-G STOP CONDITION: report this to the driver).\n%s",
			dhcpLeaseDeadline, vm.Diagnostics())
	}
	t.Logf("net-only smoke: guest acquired %s (passt×CH vhost-user negotiation OK)", guestAddr)
}

// TestFullBoot is the milestone deliverable: boot the full stack (2 CPUs /
// 1024 MB), complete the Health handshake with net_provisioned &&
// workspace_mounted inside the 60 s deadline, record boot latency and
// per-process PSS, and verify Shutdown leaves no orphan process and removes the
// unix sockets and the three pidfiles.
func TestFullBoot(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := bootConfig(t, env, 2, 1024)

	start := time.Now()
	vm, err := Launch(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	t.Cleanup(func() {
		if shutErr := vm.Shutdown(context.WithoutCancel(t.Context())); shutErr != nil {
			t.Errorf("Shutdown: %v", shutErr)
		}
		// No orphans: all three processes gone after Shutdown.
		for _, name := range []string{"cloud-hypervisor", "virtiofsd", "passt"} {
			if vm.Running(name) {
				t.Errorf("%s still running after Shutdown", name)
			}
		}
		// Sockets removed.
		for _, s := range vm.sockets {
			if fileExists(s) {
				t.Errorf("socket %s not removed by Shutdown", s)
			}
		}
		// And the three pidfiles: a record must not outlive the session it
		// names, or the reaper would find a dir full of dead records for a
		// session that shut down cleanly.
		for _, p := range vm.pidfiles {
			if fileExists(p) {
				t.Errorf("pidfile %s not removed by Shutdown", p)
			}
		}
	})

	// Poll Health until the guest reports both flags true, bounded by the boot
	// deadline. Early Health calls fail (guest not yet serving the vsock); that
	// is the expected pre-ready state, so a failing call is retried, not fatal.
	var latency time.Duration
	ok := waitFor(t, fullBootDeadline, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		resp, healthErr := vm.Health(ctx)
		if healthErr != nil {
			return false
		}
		if resp.GetNetProvisioned() && resp.GetWorkspaceMounted() {
			latency = time.Since(start)
			return true
		}
		return false
	})
	if !ok {
		t.Fatalf("guest did not reach Health{net_provisioned && workspace_mounted} within %s.\n%s",
			fullBootDeadline, vm.Diagnostics())
	}
	t.Logf("full boot: Health OK (net_provisioned && workspace_mounted) in %s", latency)

	// PSS is informational spike data, not a pass/fail gate: log what is
	// readable and note what is not. A rootless harness cannot read a
	// sandboxed helper's smaps_rollup (passt sets PR_SET_DUMPABLE=0), so its
	// entry is legitimately absent — that is reported, never failed.
	pss, pssErr := vm.PSS()
	if pssErr != nil {
		t.Logf("reading PSS (best-effort): %v", pssErr)
	}
	for _, name := range []string{"cloud-hypervisor", "virtiofsd", "passt"} {
		if kb, present := pss[name]; present {
			t.Logf("PSS %s: %d kB", name, kb)
		} else {
			t.Logf("PSS %s: unavailable (sandboxed or exited)", name)
		}
	}
}

// TestFullBootRecordsSettledPidfiles is W1's KVM assertion (§(a)): after a real
// Launch the runtime dir must hold a SETTLED record for each of the three
// children — no intent line left behind — and each record must name a process
// that is actually live with a matching starttime. That is exactly the input
// the orphan reaper acts on, so the hermetic tier's coverage of the primitives
// is not enough: this pins that launch wires them to the REAL pids of the REAL
// children, under the real spawn ordering (virtiofsd, passt, then the VMM).
func TestFullBootRecordsSettledPidfiles(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := bootConfig(t, env, 2, 1024)

	vm, err := Launch(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}

	dir := filepath.Dir(cfg.Net.VhostUserSocket)
	// The pid each name is expected to carry, from the child handles launch
	// built — so a record naming SOME live process (or another child's pid)
	// cannot pass.
	wantPIDs := map[string]int{
		"vmm.pid":       vm.vmm.cmd.Process.Pid,
		"virtiofsd.pid": vm.virtiofsd.cmd.Process.Pid,
		"passt.pid":     vm.passt.cmd.Process.Pid,
	}
	paths := make([]string, 0, len(wantPIDs))
	for name, wantPID := range wantPIDs {
		path := filepath.Join(dir, name)
		paths = append(paths, path)

		rec, readErr := readPidfile(path)
		if readErr != nil {
			t.Errorf("readPidfile(%s): %v", name, readErr)
			continue
		}
		if rec.Intent {
			t.Errorf("%s still holds an intent record after Launch; the settled write did not land", name)
			continue
		}
		if rec.PID != wantPID {
			t.Errorf("%s records pid %d, want the child's %d", name, rec.PID, wantPID)
		}
		// Liveness via the recorded identity, not a bare signal: this is the
		// starttime comparison the reaper makes before it kills anything.
		alive, aliveErr := rec.alive()
		if aliveErr != nil {
			t.Errorf("%s alive(): %v", name, aliveErr)
			continue
		}
		if !alive {
			t.Errorf("%s does not name a live process with a matching starttime (%+v)", name, rec)
		}
	}

	if shutErr := vm.Shutdown(context.WithoutCancel(t.Context())); shutErr != nil {
		t.Fatalf("Shutdown: %v", shutErr)
	}
	for _, path := range paths {
		if fileExists(path) {
			t.Errorf("pidfile %s survived Shutdown", path)
		}
	}
}

// armRuleset exercises netfilter autoload through nfnetlink, nf_tables, and
// conntrack. Keep it local: importing internal/runtime would create a cycle.
const armRuleset = `set -eu
nft add table inet compass_egress
nft add chain inet compass_egress output '{ type filter hook output priority 0 ; policy drop ; }'
nft add rule inet compass_egress output ct state established,related accept`

// TestInGuestEgressArmAutoloadsNetfilter is the RIG-3028 proof: on a real guest
// boot, Provision with a NON-EMPTY nft_script (the §(d) in-guest arm W1 landed)
// must succeed. The arm runs `/bin/sh -c <script>` as guest root, and the first
// `nft` opens a NETLINK_NETFILTER socket — which requires the guest kernel to
// autoload nfnetlink/nf_tables on demand. The guest has no udev/systemd-modules-
// load (guestd is PID 1), so that autoload rides ENTIRELY on the kernel
// usermode-helper: /sbin/modprobe resolving modules from the shipped /lib/modules
// tree (guest-image/default.nix). Before that helper was staged, this arm failed
// with `mnl.c:66: Unable to initialize Netlink socket: Protocol not supported`
// (EPROTONOSUPPORT) and §(e) always-arm reddened every microVM Start; this test
// is the red-green guard for that fix. The arm script is `set -eu` and adds a
// table, a chain, and a `ct state` rule, so Provision succeeding proves the
// whole chain (nfnetlink, nf_tables, nf_conntrack, nft_ct) autoloaded — any
// missing module aborts an `nft add` non-zero and surfaces as a Provision error.
func TestInGuestEgressArmAutoloadsNetfilter(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := bootConfig(t, env, 2, 1024)

	vm, err := Launch(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	t.Cleanup(func() {
		if shutErr := vm.Shutdown(context.WithoutCancel(t.Context())); shutErr != nil {
			t.Errorf("Shutdown: %v", shutErr)
		}
	})

	// Wait for the guest to report ready (net + workspace), same gate as the
	// full-boot test: Provision is only valid once guestd is serving.
	ready := waitFor(t, fullBootDeadline, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		resp, healthErr := vm.Health(ctx)
		if healthErr != nil {
			return false
		}
		return resp.GetNetProvisioned() && resp.GetWorkspaceMounted()
	})
	if !ready {
		t.Fatalf("guest did not reach Health{net_provisioned && workspace_mounted} within %s.\n%s",
			fullBootDeadline, vm.Diagnostics())
	}

	client := GuestClient(vm.vsockSocket, vm.vsockPort)

	// A non-empty nft_script exercises the guest's module autoload path; failures
	// here (including EPROTONOSUPPORT) identify a broken netfilter chain. The
	// client bound is generous; guestd's armTimeout is the server-side ceiling.
	provCtx, provCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer provCancel()
	if _, provErr := client.Provision(provCtx, &compassv1.ProvisionRequest{
		NftScript:      armRuleset,
		DefaultExecUid: 1000,
	}); provErr != nil {
		t.Fatalf("Provision with non-empty nft_script failed — in-guest netfilter "+
			"autoload broken (RIG-3028): %v\n%s", provErr, vm.Diagnostics())
	}
	t.Logf("in-guest egress arm succeeded: nfnetlink/nf_tables/nf_conntrack autoloaded on demand")
}

// TestCorruptRootfsFailsClosed is the fail-closed negative: boot with a
// nonexistent rootfs path and assert the boot fails inside the deadline with the
// cause visible in the serial console (the initrd's erofs-mount failure), not as
// a silent hang. This exercises the fail-closed teardown path — the VM must
// still Shutdown cleanly.
func TestCorruptRootfsFailsClosed(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := bootConfig(t, env, 1, 512)
	// Use a raw-sized non-erofs image so CH boots the kernel and initrd fails the
	// mount. A missing or tiny file fails during CH disk detection, before serial
	// output can show the guest's fail-closed behavior.
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
	cfg.Rootfs = corrupt

	vm, err := Launch(t.Context(), cfg)
	if err != nil {
		// A pre-VM spawn failure is also a valid fail-closed outcome.
		t.Logf("Launch fail-closed before VM start: %v", err)
		return
	}
	t.Cleanup(func() {
		if shutErr := vm.Shutdown(context.WithoutCancel(t.Context())); shutErr != nil {
			t.Errorf("Shutdown: %v", shutErr)
		}
	})

	// The guest can never reach Health (the root never mounts → no switch_root →
	// no guestd), so assert the named fail-closed cause appears in the serial
	// console inside the deadline: the initrd's erofs-mount failure.
	failed := waitForConsole(t, vm, negativeBootDeadline, func(console string) bool {
		return strings.Contains(console, "mount erofs root") || strings.Contains(console, "boot aborted")
	})
	if !failed {
		t.Fatalf("corrupt-rootfs boot did not surface a named failure in the serial console within %s.\n%s",
			negativeBootDeadline, vm.Diagnostics())
	}
	// Health must NOT succeed on a fail-closed boot.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, healthErr := vm.Health(ctx); healthErr == nil {
		t.Fatalf("Health unexpectedly succeeded on a corrupt-rootfs boot")
	}
	t.Logf("corrupt-rootfs boot fail-closed with a named serial-console cause (as designed)")
}

// waitFor polls a condition until it succeeds or the deadline elapses.
// Readiness has no completion event, so a bounded poll is required.
func waitFor(t *testing.T, deadline time.Duration, cond func() bool) bool {
	t.Helper()
	timeout := time.NewTimer(deadline)
	defer timeout.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	if cond() {
		return true
	}
	for {
		select {
		case <-timeout.C:
			return false
		case <-tick.C:
			if cond() {
				return true
			}
		}
	}
}

// waitForConsole polls the full console because a later panic backtrace can
// evict the evidence line from the bounded diagnostic tail. The full file is
// the only signal for tests where the guest has no vsock to probe.
func waitForConsole(t *testing.T, vm *VM, deadline time.Duration, match func(string) bool) bool {
	t.Helper()
	return waitFor(t, deadline, func() bool { return match(readConsole(vm)) })
}

// readConsole returns the full captured serial console, falling back to the
// bounded tail if the file cannot be read yet (e.g. before the first byte is
// flushed).
func readConsole(vm *VM) string {
	raw, err := os.ReadFile(vm.consolePath)
	if err != nil {
		return vm.ConsoleTail()
	}
	return string(raw)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
