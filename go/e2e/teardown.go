//go:build podman

package e2e

import (
	"context"
	"fmt"
	"os/exec" //nolint:depguard // e2e teardown: exact-name `podman rm -f` container sweep (rule://process-safety)
)

// podmanRemoveForce removes only the exact named container, tolerating absence.
// Exact-name scope protects other containers on shared hosts; errors include
// command output so the caller can report a failed preflight or cleanup.
func podmanRemoveForce(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "podman", "rm", "-f", name) //nolint:gosec // G204: fixed podman binary and argument vector; name is an exact container id.
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("podman rm -f %q: %w: %s", name, err, out)
	}
	return nil
}
