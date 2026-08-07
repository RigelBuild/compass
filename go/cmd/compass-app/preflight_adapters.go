//go:build unix && gtk3

// The real host-preflight adapters for embedded mode: each is one genuine
// external effect the preflight core (go/internal/preflight) is inverted over —
// a rootless-podman probe, an agent-image presence check, and a Postgres
// reachability probe. They are thin shells around os/exec and pgx, mirroring how
// go/internal/stack/adapters wires real effects behind the stack core seams; the
// pipeline's composition root (realPreflight in embedded.go) supplies them.
package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dbProbeTimeout bounds the embedded database reachability probe. A cheap
// connect+ping should answer well within this; the short window keeps a
// still-starting or wedged postgres from stalling the launch.
const dbProbeTimeout = 2 * time.Second

// podmanRootless probes that rootless podman is present and usable by running
// `podman info`. A nil error means it answered; a non-nil error wraps the
// captured stderr so the preflight failure copy names why podman is unusable.
func podmanRootless(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "podman", "info")
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("podman info: %w: %s", err, msg)
		}
		return fmt.Errorf("podman info: %w", err)
	}
	return nil
}

// imagePresent probes that the agent image ref is present in the local store via
// `podman image exists <image>` (exit 0 = present, non-zero = absent). A
// non-nil error means the image is not available locally (the preflight core
// reports it is pulled from GHCR at first run); it wraps the captured stderr for
// context.
func imagePresent(ctx context.Context, image string) error {
	//nolint:gosec // G204: image is an operator/env-resolved ref, argv is fixed.
	cmd := exec.CommandContext(ctx, "podman", "image", "exists", image)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("podman image exists %s: %w: %s", image, err, msg)
		}
		return fmt.Errorf("podman image exists %s: %w", image, err)
	}
	return nil
}

// dbReachable probes that Postgres is accepting connections on dsn by opening a
// short-lived pgx connection and pinging it, then closing immediately — the
// lightest genuine reachability check, mirroring
// go/internal/stack/adapters/dbprobe.go. A ~2s timeout keeps the probe cheap; a
// connect or ping failure means not-yet-reachable.
func dbReachable(ctx context.Context, dsn string) error {
	ctx, cancel := context.WithTimeout(ctx, dbProbeTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	// The probe's verdict is the ping result; a Close error on an already-broken
	// conn is not actionable here, so it is discarded on the deferred cleanup.
	defer func() { _ = conn.Close(ctx) }()

	if err := conn.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}
