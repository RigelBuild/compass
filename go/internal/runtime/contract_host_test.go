package runtime

// The untagged host leg runs the shared WorkloadRuntime contract suite without an engine.
// It covers the host's euid-only execution and permanent resize-unsupported behavior.

import (
	"os"
	"strconv"
	"testing"
)

// TestContractSuite_Host runs shared contract rows against HostRuntime on every platform.
// It uses a fresh temp root and a name-only spec because host Create spawns no process.
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
		// MicroVM-only divergences are off; host behavior is covered by other rows.
		refusesRootExec:       false,
		numericUIDOnly:        false,
		emptyMountLabel:       false,
		capsOutput:            false,
		gracefulStopPowersOff: false,
		portableKillError:     false,
		assertDuplicateName: func(t *testing.T, err error) {
			t.Helper()
			// Host duplicate-name Create returns a plain error, so any non-nil error satisfies the contract.
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
