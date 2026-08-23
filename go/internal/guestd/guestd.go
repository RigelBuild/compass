//go:build linux

// Package guestd is the logic behind the compass-guestd binary — the real
// guest PID-1 init for the V2a microVM runtime spike (design
// docs/designs/platform/compass-elastic-session-runtime/microvm-v2a-guest-image-boot-spike.md,
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

// config carries the resolved boot parameters.
type config struct {
	vsockPort     uint32
	guestdVersion string
}

// bootSteps are the injectable seams the orchestrator (run) depends on. The
// production wiring (Run) supplies the real Linux implementations; the
// ordering/fail-closed tests supply fakes and assert the gate.
type bootSteps struct {
	mountAPIFilesystems func() error
	net                 netProvisioner
	workspace           workspaceMounter
	// serve receives the fully-provisioned Health service and serves it until
	// ctx is cancelled. It is the LAST step: reaching it is the proof that net
	// and mount both succeeded.
	serve func(ctx context.Context, port uint32, svc *healthService) error
}

// Run is the production entry point: it reads the vsock port from the kernel
// cmdline, wires the real Linux boot steps, and drives the fail-closed sequence.
func Run(ctx context.Context, log *slog.Logger) error {
	cmdline, err := os.ReadFile(procCmdlinePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", procCmdlinePath, err)
	}
	port, err := parseVsockPort(string(cmdline))
	if err != nil {
		return err
	}

	steps := bootSteps{
		mountAPIFilesystems: mountAPIFilesystems,
		net:                 &linuxNetProvisioner{iface: defaultNetIface, log: log},
		workspace:           &virtioFSMounter{tag: workspaceTag, target: workspaceTarget},
		serve:               serveVsock,
	}
	return run(ctx, config{vsockPort: port, guestdVersion: Version}, steps)
}

// run executes the fail-closed boot sequence in the exact order §(d) fixes:
// (1) API filesystems, (2) networking, (3) virtio-fs workspace, (4) serve the
// vsock Health handshake, (5) idle inside serve until ctx is cancelled. Any step
// error aborts the sequence before the next one runs, so a failing provisioner
// never reaches the mount and a failing mount never reaches the server — Health
// is served only after net and mount both succeed.
func run(ctx context.Context, cfg config, steps bootSteps) error {
	if err := steps.mountAPIFilesystems(); err != nil {
		return fmt.Errorf("mounting API filesystems: %w", err)
	}
	if err := steps.net.Provision(ctx); err != nil {
		return fmt.Errorf("provisioning network: %w", err)
	}
	if err := steps.workspace.Mount(); err != nil {
		return fmt.Errorf("mounting workspace: %w", err)
	}

	// Both bringup steps passed, so the served state is unconditionally true —
	// a successful handshake is the proof of that. If the sequence ever grew a
	// step that could serve degraded state, these would reflect it.
	svc := &healthService{
		version:          cfg.guestdVersion,
		netProvisioned:   true,
		workspaceMounted: true,
	}
	return steps.serve(ctx, cfg.vsockPort, svc)
}
