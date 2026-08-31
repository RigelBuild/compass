//go:build linux

// Package guestd is the logic behind the compass-guestd binary — the real
// guest PID-1 init for the V2a microVM runtime spike (design
// docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-v2a-guest-image-boot-spike.md,
// §(d), RIG-2589 T2). It runs post-switch_root as PID 1 and executes a fixed,
// fail-closed boot sequence: mount the API filesystems, bring networking up via
// an in-process DHCP client (OQ-C), mount the virtio-fs workspace, then serve
// the GuestControl Connect/h2c Health handshake over AF_VSOCK, and idle.
//
// The fail-closed invariant is the whole point: Health is served ONLY after net
// and mount both succeed, so a successful handshake IS the proof that D6 bringup
// and the virtio-fs mount happened. Any step failing logs the cause to the
// console (stderr → ttyS0) and returns non-zero; PID 1 exiting panics the guest
// and the host's dial fails at its deadline. The in-VM behavior is proven by T4
// (KVM-gated); this package ships the binary logic plus hermetic unit tests for
// every piece that runs without a VM, behind the netProvisioner / workspaceMounter
// seams the tests fake.
package guestd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// Version is the guestd build version reported by Health.GuestdVersion. Override
// at build time with -ldflags "-X github.com/RigelBuild/compass/go/internal/guestd.Version=<v>".
var Version = "0.1.0"

// procCmdlinePath is the kernel cmdline pseudo-file the vsock port is parsed
// from (compass.vsock_port=<n>).
const procCmdlinePath = "/proc/cmdline"

// netProvisioner brings guest networking up: link-up, DHCP lease acquisition,
// address/route installation, and /etc/resolv.conf. The real implementation
// (linuxNetProvisioner) touches netlink and a raw DHCP socket; tests inject a
// fake so the boot-ordering gate is hermetic.
type netProvisioner interface {
	Provision(ctx context.Context) error
}

// workspaceMounter mounts the virtio-fs workspace tag at its stable path. The
// real implementation (virtioFSMounter) calls mount(2); tests inject a fake.
type workspaceMounter interface {
	Mount() error
}

// config carries the resolved boot parameters that are known before the boot
// sequence starts. The vsock port is NOT here: it is read from the kernel
// cmdline inside run(), after /proc is mounted (§(d) step 1).
type config struct {
	guestdVersion string
	// log is the guestd logger, threaded to run() so its diagnostics match the
	// handler Run configures rather than the package global. Optional: a nil log
	// (boot tests) falls back to slog.Default().
	log *slog.Logger
}

// bootSteps are the injectable seams the orchestrator (run) depends on. The
// production wiring (Run) supplies the real Linux implementations; the
// ordering/fail-closed tests supply fakes and assert the gate.
type bootSteps struct {
	mountAPIFilesystems func() error
	// readCmdline returns the kernel command line. It is a step, not a
	// pre-computed value, because /proc/cmdline is only readable AFTER
	// mountAPIFilesystems mounts /proc — §(d) step 1 fixes that order, and the
	// initramfs hands over a bare root with no /proc. Injectable so a test can
	// assert the read happens after the api-mount step, not before.
	readCmdline func() ([]byte, error)
	net         netProvisioner
	workspace   workspaceMounter
	// serve receives the fully-provisioned supervisor and serves it until ctx
	// is cancelled. It is the LAST step: reaching it is the proof that net and
	// mount both succeeded.
	serve func(ctx context.Context, port uint32, svc *supervisor) error
	// powerOff performs the PID-1-legal reboot(RB_POWER_OFF) that ends an
	// RPC-driven Stop (§(d)): a bare PID-1 exit panics the kernel, which
	// cloud-hypervisor never observes as a VMM exit, so guestd must power the
	// guest off explicitly. Injectable so the hermetic boot tests assert the
	// trigger without actually rebooting the test host.
	powerOff func() error
}

// Run is the production entry point: it wires the real Linux boot steps and
// drives the fail-closed sequence. The vsock port is read from the kernel
// cmdline inside the sequence, after /proc is mounted.
func Run(ctx context.Context, log *slog.Logger) error {
	steps := bootSteps{
		mountAPIFilesystems: mountAPIFilesystems,
		readCmdline:         func() ([]byte, error) { return os.ReadFile(procCmdlinePath) },
		net:                 &linuxNetProvisioner{iface: defaultNetIface, log: log},
		workspace:           &virtioFSMounter{tag: workspaceTag, target: workspaceTarget},
		serve:               serveVsock,
		powerOff:            powerOff,
	}
	return run(ctx, config{guestdVersion: Version, log: log}, steps)
}

// run executes the fail-closed boot sequence in the exact order §(d) fixes:
// (1) API filesystems, (2) read the vsock port + optional boot nonce from the
// now-readable kernel cmdline, (3) networking, (4) virtio-fs workspace,
// (5) serve the vsock GuestControl surface, (6) idle inside serve until ctx is
// cancelled — by a Unix signal (V2a path) or an RPC Stop (§(d)). Any step error
// aborts the sequence before the next one runs, so Health is served only after
// net and mount both succeed. When serve returns because an RPC Stop cancelled
// it, run ends in reboot(RB_POWER_OFF): a bare PID-1 exit panics the kernel,
// which the VMM never observes as a guest exit.
func run(ctx context.Context, cfg config, steps bootSteps) error {
	// log is the guestd logger threaded from Run; a nil (test-constructed config)
	// falls back to the default handler, which writes to os.Stderr (ttyS0) — the
	// same sink the production logger uses, so a boot test needs no logger.
	log := cfg.log
	if log == nil {
		log = slog.Default()
	}
	if err := steps.mountAPIFilesystems(); err != nil {
		return fmt.Errorf("mounting API filesystems: %w", err)
	}

	// /proc is mounted now, so the kernel cmdline is readable — §(d) step 1.
	cmdline, err := steps.readCmdline()
	if err != nil {
		return fmt.Errorf("reading %s: %w", procCmdlinePath, err)
	}
	port, err := parseVsockPort(string(cmdline))
	if err != nil {
		return err
	}
	bootNonce, err := parseBootNonce(string(cmdline))
	if err != nil {
		return err
	}
	gatewayPort, _, err := parseGatewayPort(string(cmdline))
	if err != nil {
		return err
	}
	if err := steps.net.Provision(ctx); err != nil {
		return fmt.Errorf("provisioning network: %w", err)
	}
	if err := steps.workspace.Mount(); err != nil {
		return fmt.Errorf("mounting workspace: %w", err)
	}

	// Both bringup steps passed, so the served state is unconditionally true —
	// a successful handshake is the proof of that. The supervisor starts in
	// stateReady (Health answers, exec refused until Provision opens the gate).
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	svc := &supervisor{
		version:          cfg.guestdVersion,
		netProvisioned:   true,
		workspaceMounted: true,
		bootNonce:        bootNonce,
		newCredential:    linuxCredential,
		armFunc:          runNftScript,
		dialGateway:      realDialGateway,
		chownSocket:      chownAgentSocket,
		gatewayPort:      gatewayPort,
		stopServing:      stopServing,
		serveCtx:         serveCtx,
		state:            stateReady,
		execs:            make(map[string]*childExec),
	}
	serveErr := steps.serve(serveCtx, port, svc)

	// An RPC-driven Stop (Signal("", ...)) cancels serveCtx from inside the
	// supervisor; ctx (the process signal context) is still live. In that case
	// the guest is going down UNCONDITIONALLY, so guestd must power the guest
	// off explicitly (§(d)) — a PID-1 exit would panic the kernel. The
	// power-off is gated on rpcStop alone, NOT on a clean serveErr: a graceful
	// drain that overran its deadline (e.g. a child that ignored SIGTERM)
	// returns a non-nil Shutdown error, but the guest must STILL power off so
	// the VMM observes a real shutdown within the host's timeout rather than
	// burning the full timeout to a hard kill. A serve fault or a Unix-signal
	// cancel (rpcStop false) returns as before, letting main exit and the host
	// observe the dial failure.
	svc.mu.Lock()
	rpcStop := svc.rpcStop
	svc.mu.Unlock()
	if rpcStop {
		if serveErr != nil {
			log.Error("guest serve drain returned an error on RPC stop; powering off anyway", "error", serveErr)
		}
		return steps.powerOff()
	}
	return serveErr
}
