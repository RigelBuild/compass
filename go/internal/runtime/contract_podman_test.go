//go:build podman

package runtime

// The podman leg runs the shared contract suite against rootless PodmanCLI when available.
// MicroVM-only divergences are off; the kill row preserves podman's *exec.ExitError path.

import (
	"errors"
	"testing"
)

// TestContractSuite_Podman builds the agent image once and runs shared rows against podman.
// Each session uses the production keep-alive command and uid 1000.
func TestContractSuite_Podman(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman not usable in this environment")
	}
	buildImage(t, t.Context(), t.TempDir())

	caps := backendCaps{
		name: "podman",
		makeSpec: func(t *testing.T, name string) WorkloadSpec {
			t.Helper()
			return WorkloadSpec{
				Image:   imageTag,
				Name:    name,
				Command: []string{"sleep", "infinity"},
				UID:     1000,
			}
		},
		// Divergences 1-4 are microVM-specific: podman's capture is unbounded,
		// it resolves image user names, MountLabel is the engine's real label,
		// and Command is the keep-alive entrypoint. All OFF → those rows skip.
		refusesRootExec:         false,
		numericUIDOnly:          false,
		emptyMountLabel:         false,
		ignoresCommandAndCapAdd: false,
		capsOutput:              false,
		// A `sleep infinity` PID 1 ignores SIGTERM, so Stop burns the full grace
		// on podman — the graceful-power-off row would prove nothing here.
		gracefulStopPowersOff: false,
		// Divergence 6: the podman deliberate-kill error is the byte-identical
		// *exec.ExitError (checked in deliberateKill), never the portable type.
		portableKillError: false,
		assertDuplicateName: func(t *testing.T, err error) {
			t.Helper()
			if _, ok := errors.AsType[*CommandError](err); !ok {
				t.Fatalf("duplicate-name Create error = %v (%T), want *CommandError (the engine's name-collision refusal)", err, err)
			}
		},
	}

	runContractSuite(t, func(t *testing.T) WorkloadRuntime {
		t.Helper()
		return NewPodmanCLI()
	}, caps)
}
