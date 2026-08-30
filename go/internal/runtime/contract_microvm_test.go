//go:build microvm && unix

package runtime

// The microVM leg of the shared ContainerRuntime contract suite (record §U5):
// runs runContractSuite against a real MicroVMRuntime on live hardware, gated on
// microvmtest.Require(t) (skip-on-absent-KVM, hard-fail under
// COMPASS_REQUIRE_MICROVM=1). It supplies the microVM caps encoding all 6
// conceded divergences (record 580-593) as ON flags, so every divergence row
// runs here and a silent widening fails. It ALSO records the Q-budget numbers
// (record 832-834): boot latency (Start wall-clock) and per-process PSS, emitted
// via t.Logf as informational spike output per §(g) — NOT a boot gate.
//
// The Create→Start→Exec→Stop→Remove happy path and the ExecStreaming-kill path
// that used to live in microvm_lifecycle_microvm_test.go are now shared contract
// rows and were folded out of that file; e2eConfig and
// TestMicroVMStartFailureLeavesNoState (a microVM-backend-only negative with no
// podman analog) stay there.

import (
	"errors"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// TestContractSuite_MicroVM drives the shared contract rows against a live
// MicroVMRuntime through the ContainerRuntime interface. The factory builds a
// runtime from the resolved test env; sessions are created with a single
// /workspace virtio-fs share and uid 1000. All divergence caps are ON, so the
// microVM-specific rows (output cap, non-numeric user, empty MountLabel, ignored
// Command/CapAdd, graceful power-off, portable kill error) all run.
func TestContractSuite_MicroVM(t *testing.T) {
	env := microvmtest.Require(t)

	caps := backendCaps{
		name: "microvm",
		makeSpec: func(t *testing.T, name string) ContainerSpec {
			t.Helper()
			return ContainerSpec{
				Name:   name,
				UID:    1000,
				Mounts: []Mount{{HostPath: t.TempDir(), ContainerPath: "/workspace"}},
			}
		},
		// All 6 conceded divergences hold on the microVM backend (record
		// 580-593): each ON flag runs its row so a silent widening fails.
		refusesRootExec:         true,
		numericUIDOnly:          true,
		emptyMountLabel:         true,
		ignoresCommandAndCapAdd: true,
		capsOutput:              true,
		gracefulStopPowersOff:   true,
		portableKillError:       true,
		assertDuplicateName: func(t *testing.T, err error) {
			t.Helper()
			var dup *DuplicateNameError
			if !errors.As(err, &dup) {
				t.Fatalf("duplicate-name Create error = %v (%T), want *DuplicateNameError", err, err)
			}
		},
	}

	runContractSuite(t, func(t *testing.T) ContainerRuntime {
		t.Helper()
		return NewMicroVMRuntime(e2eConfig(t, env))
	}, caps)
}

// TestMicroVMQBudget records the boot-latency and per-process PSS numbers that
// feed the Q-budget (record 832-834, §(g), launch.go:450-455). It is
// INFORMATIONAL spike output, NOT a boot gate: it times Start (the full
// Launch→Health-OK→Provision window) and reads the session VM's PSS
// (proportional set size in kB, launch.go:456), emitting both via t.Logf. PSS is
// best-effort — a sandboxed helper (passt sets PR_SET_DUMPABLE=0) leaves no
// readable entry, which is reported, never failed.
func TestMicroVMQBudget(t *testing.T) {
	env := microvmtest.Require(t)
	m := NewMicroVMRuntime(e2eConfig(t, env))

	workspace := t.TempDir()
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   "qbudget-agent",
		UID:    1000,
		Mounts: []Mount{{HostPath: workspace, ContainerPath: "/workspace"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Remove(t.Context(), id); err != nil {
			t.Errorf("Remove (cleanup): %v", err)
		}
	})

	start := time.Now()
	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	bootLatency := time.Since(start)
	t.Logf("Q-budget: boot latency (Start Launch→Health-OK→Provision) = %s", bootLatency)

	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session after Start: %v", err)
	}
	pss, pssErr := session.vm.PSS()
	if pssErr != nil {
		t.Logf("Q-budget: reading PSS (best-effort): %v", pssErr)
	}
	for _, name := range []string{"cloud-hypervisor", "virtiofsd", "passt"} {
		if kb, present := pss[name]; present {
			t.Logf("Q-budget: PSS %s = %d kB", name, kb)
		} else {
			t.Logf("Q-budget: PSS %s unavailable (sandboxed or exited)", name)
		}
	}
}
