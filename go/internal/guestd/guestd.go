//go:build linux

// Package guestd is the logic behind the compass-guestd binary — the real
// guest PID-1 init for the V2a microVM runtime spike (design
// RigelBuild/docs:docs/designs/compass/infra/runtime/compass-elastic-session-runtime/microvm-v2a-guest-image-boot-spike.md,
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
	// readCmdline reads /proc only after mountAPIFilesystems mounts it; fakes verify this order.
	readCmdline func() ([]byte, error)
	net         netProvisioner
	workspace   workspaceMounter
	// serve is last; reaching it proves networking and mount succeeded.
	serve func(ctx context.Context, port uint32, svc *supervisor) error
	// powerOff ends RPC-driven Stop without PID-1 exit, which panics the guest kernel.
	// Tests inject it instead of rebooting the host.
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

// run aborts setup errors before Health is served; serving faults can still occur afterward.
// RPC Stop powers off because exiting PID 1 panics the guest kernel.
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

	// RPC Stop powers off even if the graceful serve drain hits its deadline; the VMM must observe shutdown.
	// Serve faults and Unix-signal cancellation still surface to the host.
	svc.mu.Lock()
	rpcStop := svc.rpcStop
	svc.mu.Unlock()
	if rpcStop {
		if serveErr != nil {
			log.ErrorContext(ctx, "guest serve drain returned an error on RPC stop; powering off anyway", "error", serveErr)
		}
		return steps.powerOff()
	}
	return serveErr
}
