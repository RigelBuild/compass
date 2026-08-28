//go:build podman

package runtime

// The podman leg of the shared ContainerRuntime contract suite (record §U5):
// runs runContractSuite against a real rootless-podman PodmanCLI, gated on
// podmanUsable() (skip-not-fail where podman is absent, the existing suite's
// pattern, lifecycle_test.go:54-60). It supplies the podman caps: the
// divergences that are microVM-specific (output cap, numeric-uid-only, empty
// MountLabel, ignored Command/CapAdd, graceful power-off) are OFF here, so those
// rows self-skip; the deliberate-kill row asserts the byte-identical
// *exec.ExitError path so this leg proves the podman byte-path stays
// unregressed (divergence 6, OQ-G/U3b).

import (
	"errors"
	"testing"
)

// TestContractSuite_Podman drives the shared contract rows against rootless
// podman through the ContainerRuntime interface. It builds the agent image once
// (buildImage) and hands runContractSuite a factory minting a fresh PodmanCLI,
// with containers created from that image running `sleep infinity` as uid 1000
// — the production keep-alive-plus-exec shape (lifecycle_test.go).
func TestContractSuite_Podman(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	buildImage(t, t.TempDir())

	caps := backendCaps{
		name: "podman",
		makeSpec: func(t *testing.T, name string) ContainerSpec {
			t.Helper()
			return ContainerSpec{
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
			var cmdErr *CommandError
			if !errors.As(err, &cmdErr) {
				t.Fatalf("duplicate-name Create error = %v (%T), want *CommandError (the engine's name-collision refusal)", err, err)
			}
		},
	}

	runContractSuite(t, func(t *testing.T) ContainerRuntime {
		t.Helper()
		return NewPodmanCLI()
	}, caps)
}
