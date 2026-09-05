//go:build (linux && gtk4) || darwin

// The real host-preflight adapters for embedded mode: each is one genuine
// external effect the preflight core (go/internal/preflight) is inverted over —
// a rootless-podman probe, a podman-version floor probe, and an agent-image
// presence check. They are thin shells around os/exec and the runtime package,
// mirroring how go/internal/stack/adapters wires real effects behind the stack
// core seams; the pipeline's composition root (realPreflight in embedded.go)
// supplies them.
package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

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

// podmanVersionAtLeastFloor probes that the host podman is new enough for the
// userns remap the runner depends on (podman >= 4.3). It reuses
// runtime.(*PodmanCLI).VerifyUsernsRemapSupport so the app's front-door gate and
// the runner's startup gate share one floor and one error copy ("podman N.N or
// newer is required …"). A nil error means the floor is met; a non-nil error
// carries that copy verbatim so preflight surfaces it before `compass-stack up`
// (design §A3 delta 4).
func podmanVersionAtLeastFloor(ctx context.Context) error {
	return runtime.NewPodmanCLI().VerifyUsernsRemapSupport(ctx)
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
