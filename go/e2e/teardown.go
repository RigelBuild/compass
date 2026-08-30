//go:build podman

package e2e

import (
	"context"
	"fmt"
	"os/exec" //nolint:depguard // e2e teardown: exact-name `podman rm -f` container sweep (rule://process-safety)
)

// podmanRemoveForce force-removes a container by its EXACT name, tolerating a
// missing container (a fresh slate is the goal, not a guaranteed prior
// existence). This is the RIG-1790 A6 preflight: before a re-run Provisions the
// same deterministic name, the leaked container from a prior lifetime is swept
// so `podman create --name` does not collide.
//
// EXACT-NAME ONLY — never a name filter/glob. This runs on a box that may host a
// live agent fleet, and a blanket `--filter name=compass-agent-` removal would
// reap real containers (rule://process-safety). ctx-bounded, and it returns an
// error rather than panicking (go-no-panic-in-lib) so the caller decides
// fatality.
//
// `podman rm -f` exits 0 for an absent container (the -f/ignore semantics), so
// absence is not an error. A non-zero exit is a real removal failure and is
// wrapped with the combined output for a legible diagnostic.
func podmanRemoveForce(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "podman", "rm", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("podman rm -f %q: %w: %s", name, err, out)
	}
	return nil
}
