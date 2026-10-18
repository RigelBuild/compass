// The real host-preflight adapters for embedded mode: each is one genuine
// external effect the preflight core (go/internal/preflight) is inverted over —
// a rootless-podman probe, a podman-version floor probe, and an agent-image
// presence check. They are thin shells around os/exec and the runtime package,
// mirroring how go/internal/stack/adapters wires real effects behind the stack
// core seams; the pipeline's composition root (RealPreflight in embedded.go)
// supplies them.
package embedded

import (
	"context"
	"fmt"
	"os/exec" //nolint:depguard // preflight seam: fixed-arg podman probes and LookPath
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

// podmanVersionAtLeastFloor shares the runner's minimum and error copy.
func podmanVersionAtLeastFloor(ctx context.Context) error {
	return runtime.NewPodmanCLI().VerifyUsernsRemapSupport(ctx)
}

// imagePresent checks the local store; a miss is reported for stack up to pull.
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
