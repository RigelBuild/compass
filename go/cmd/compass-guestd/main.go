//go:build linux

// Command compass-guestd is the Compass V2a microVM guest init: it runs as guest
// PID 1 (post-switch_root), installed at /sbin/init, and drives the fail-closed
// boot sequence — mount API filesystems, bring networking up via an in-process
// DHCP client, mount the virtio-fs workspace, then serve the GuestControl
// Connect/h2c Health handshake over AF_VSOCK, and idle until the host tears the
// VMM down (design docs/designs/platform/compass-elastic-session-runtime/
// microvm-v2a-guest-image-boot-spike.md, §(d), RIG-2589 T2). All boot logic
// lives in internal/guestd; this binary is a thin wrapper that assembles the
// process root context, wires signal-driven shutdown, and exits non-zero on any
// boot failure — PID 1 death panics the guest, so the host's handshake dial
// fails at its deadline and tears down.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/RigelBuild/compass/go/internal/guestd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "compass-guestd:", err)
		os.Exit(1)
	}
}

func run() error {
	// The process root context (main is the exempt place for a fresh root).
	// SIGTERM/SIGINT cancel it, driving the vsock server's graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Console is ttyS0; guestd logs boot progress and failures to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	return guestd.Run(ctx, log)
}
