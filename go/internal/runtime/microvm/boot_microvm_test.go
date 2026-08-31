//go:build microvm && unix

// boot_microvm_test.go is T4's KVM-gated boot spike: it boots a real session
// guest under cloud-hypervisor (rootless, as the invoking user), completes the
// GuestControl.Health handshake over the hybrid vsock, measures per-process
// PSS, and tears the stack down with no orphans — the deliverable of the V2a
// milestone (record §T4, §(g)). Every test calls microvmtest.Require(t) first:
// on a KVM-less box it SKIPS (unless COMPASS_REQUIRE_MICROVM=1 forces a hard
// fail), so the suite is only real where /dev/kvm is openable.
//
// The tests run in a deliberate order via subtests off one parent, but each is
// independent (its own VM, its own temp dir). The net-only smoke is first
// because it isolates OQ-G — the passt×cloud-hypervisor vhost-user-net
// negotiation, the spike's primary unknown — so a negotiation failure surfaces
// as itself (no address in the serial console) rather than as an ambiguous
// full-stack timeout.

package microvm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

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

// bootConfig builds a BootConfig from the resolved test env and a fresh temp
// dir. The temp dir holds every AF_UNIX socket, the passt pidfile, the captured
// logs, and the virtio-fs shared dir, so a test leaves nothing outside its own
// t.TempDir(). FSSharedDir is a throwaway dir here (V2b supplies the per-session
// checkout); the spike only asserts the mount happened via Health's flag, not
// content.
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
// unix sockets.
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

// armRuleset is a minimal but representative in-guest egress arm: it creates the
// inet table + a conntrack-stateful output rule, forcing a representative slice
// of the netfilter autoload chain — the NETLINK_NETFILTER socket (nfnetlink),
// the nf_tables subsystem, and the `ct state` expression (nf_conntrack +
// nft_ct). Once the usermode helper is staged, request_module resolves ANY
// module from the full /lib/modules tree, so this slice proves the mechanism the
// real base ruleset relies on (its interval sets and DNS rules pull further
// modules the same way). It is `set -eu` so any nft failing aborts non-zero,
// like EgressPolicy.NftScript()'s base ruleset. Kept as a local literal, not a
// runtime.EgressPolicy call, because this package must not import
// internal/runtime (config.go: no runtime dep, no cycle).
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

	// The load-bearing assertion: Provision with a non-empty nft_script arms
	// egress in-guest. This is the exact path that failed EPROTONOSUPPORT before
	// /sbin/modprobe was staged. A non-nil error here (especially one carrying
	// mnl.c:66 / "Protocol not supported") means the netfilter autoload chain is
	// broken again.
	// 30s is a deliberately generous CLIENT bound: the arm (module autoload + a
	// few nft adds) is sub-second, and the real ceiling is the server's 120s
	// armTimeout (guestd/supervisor.go), not this deadline.
	provCtx, provCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer provCancel()
	if _, provErr := client.Provision(provCtx, connect.NewRequest(&compassv1.ProvisionRequest{
		NftScript:      armRuleset,
		DefaultExecUid: 1000,
	})); provErr != nil {
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
	// A raw-sized but non-erofs rootfs (16 MiB of zeros), NOT a missing or tiny
	// path: CH must be able to detect a raw image and boot the kernel, so the
	// initrd's `mount -t erofs /dev/vda` then fails-closed with a named cause on
	// the serial console — the GUEST fail-closed teardown the design exercises.
	// A missing path (or a byte-sized garbage file) instead trips CH's disk-open
	// / image-type detection before the kernel ever runs, leaving no serial log.
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

// waitFor polls cond every 250 ms until it returns true or the deadline elapses,
// returning whether it became true. It is a gated poll-until (not a blind
// time.Sleep): there is no single in-band completion event for a guest boot —
// readiness is only observable by probing Health — so a bounded poll is the
// correct wait, and it fails loudly at a real timeout.
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

// waitForConsole polls the captured serial console until match reports the
// expected content or the deadline elapses. Used where the guest has no vsock to
// probe (the net-only smoke and the corrupt-rootfs negative), so the console is
// the only observable signal.
//
// It matches against the WHOLE console file, not VM.ConsoleTail's bounded
// diagnostic tail: a fail-closed boot prints a kernel panic backtrace AFTER the
// evidence line, and a dump larger than the 8 KiB tail window would evict the
// evidence and turn a real failure into a false-negative timeout. The bounded
// tail stays where it belongs — the failure MESSAGE (Diagnostics) — not the poll
// predicate.
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
