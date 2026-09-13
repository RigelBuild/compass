package runtime

// The host leg of the shared WorkloadRuntime contract suite (record §U5): runs
// runContractSuite against a real HostRuntime — direct host child processes
// under the Runner's own uid, with no container engine, VM, or KVM. It is
// UNTAGGED (no //go:build line) precisely because it needs none of those: the
// podman and microVM legs are build-tagged and gated on an engine/KVM being
// present, so in most CI jobs the shared contract runs against NOTHING. This leg
// gives the frozen 9-method contract actual continuous coverage on every runner.
//
// The host backend's uid rule is its one structural divergence from the engine
// legs, and it drives every caps choice here. A host child cannot switch user:
// it runs as the Runner's own effective uid, and HostRuntime.checkUser accepts
// ONLY that euid (host_backend.go:436-445). So the exec/stream rows run as
// os.Geteuid(), NOT the engine legs' baked "1000" — which is not even the host
// uid on a stock GitHub-hosted runner (host uid 1001, portability_test.go). The
// euidOnly cap points rowUIDEnforcement at that rule (an exec as the euid runs;
// an exec naming any other uid is refused), and resizeErr carries the host's
// distinct permanent ErrResizeUnsupportedOnHost (not the engine legs' C3-reserved
// ErrResizeNotImplemented). The microVM-specific divergence caps (output cap,
// numeric-uid-only, empty MountLabel, graceful power-off, portable kill error)
// are OFF: their rows self-skip. ignoresCommandAndCapAdd is ON because the host
// backend ALSO ignores both — Create/Start spawn no process (Command is not a
// keep-alive), and an unprivileged host child has an all-zero CapEff — so that
// row runs here and proves it truthfully.

import (
	"os"
	"strconv"
	"testing"
)

// TestContractSuite_Host drives the shared contract rows against a real
// HostRuntime through the WorkloadRuntime interface. It needs no engine and runs
// as an ordinary unprivileged user, so it is untagged and always on. The factory
// roots each runtime at a fresh t.TempDir(); makeSpec builds a name-only spec —
// no Image, no keep-alive Command (host Create spawns nothing) — the fields the
// handle model actually consumes.
func TestContractSuite_Host(t *testing.T) {
	euid := strconv.Itoa(os.Geteuid())

	caps := backendCaps{
		name: "host",
		makeSpec: func(t *testing.T, name string) WorkloadSpec {
			t.Helper()
			// No Image (nothing to run) and no `sleep infinity` keep-alive: a
			// host handle is bookkeeping until ExecStreaming spawns the agent, so
			// Create/Start launch no process. Name is the Exists lookup key.
			return WorkloadSpec{Name: name}
		},
		// The exec/stream rows must run as the Runner's own euid — the only uid
		// checkUser accepts — never the engine legs' baked "1000".
		execUID: euid,
		// rowUIDEnforcement's host branch: an exec as the euid runs, an exec
		// naming euid+1 (a uid that is NOT the Runner's, whatever the euid is) is
		// refused.
		euidOnly:    true,
		rejectedUID: strconv.Itoa(os.Geteuid() + 1),
		// Resize is a PERMANENT unsupported on host (no cgroup ownership), a
		// deliberately distinct sentinel from the engine legs' C3-reserved one.
		resizeErr: ErrResizeUnsupportedOnHost,
		// The host backend ignores spec.Command (Create/Start spawn nothing) and
		// spec.CapAdd (an unprivileged host child carries no added capability), so
		// this divergence row runs here and proves both truthfully.
		ignoresCommandAndCapAdd: true,
		// A host child inherits the Runner's capabilities; "CapAdd added nothing"
		// means the child's set equals the Runner's, not that it is empty.
		inheritsRunnerCaps: true,
		// microVM-specific divergences: all OFF, so those rows self-skip. Host
		// capture is unbounded (no 8 MiB cap), it does not resolve user names
		// (checkUser is euid-only, covered by euidOnly above), MountLabel is a
		// no-error read, no guest powers off, and its deliberate-kill error is the
		// byte-identical *exec.ExitError (ExitCode -1), never the portable type.
		refusesRootExec:       false,
		numericUIDOnly:        false,
		emptyMountLabel:       false,
		capsOutput:            false,
		gracefulStopPowersOff: false,
		portableKillError:     false,
		assertDuplicateName: func(t *testing.T, err error) {
			t.Helper()
			// Host Create refuses a duplicate name with a plain error keyed on
			// spec.Name (host_backend.go Create); it has no typed collision error,
			// so a non-nil error IS the contract. rowDuplicateName already
			// asserted non-nil before calling this.
			if err == nil {
				t.Fatal("duplicate-name Create must be refused on host; got no error")
			}
		},
	}

	runContractSuite(t, func(t *testing.T) WorkloadRuntime {
		t.Helper()
		return NewHostRuntime(t.TempDir())
	}, caps)
}
