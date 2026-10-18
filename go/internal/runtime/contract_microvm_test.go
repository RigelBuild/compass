//go:build microvm && unix

package runtime

// The KVM-gated microVM leg runs the shared contract suite with all microVM divergences on.
// It also records boot latency and PSS as informational Q-budget data, not a boot gate.

import (
	"errors"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// TestContractSuite_MicroVM runs shared rows against a live MicroVMRuntime.
// Its factory uses one /workspace virtio-fs mount and enables all divergence checks.
func TestContractSuite_MicroVM(t *testing.T) {
	env := microvmtest.Require(t)

	caps := backendCaps{
		name: "microvm",
		makeSpec: func(t *testing.T, name string) WorkloadSpec {
			t.Helper()
			return WorkloadSpec{
				Name:   name,
				UID:    1000,
				Mounts: []Mount{{HostPath: t.TempDir(), ContainerPath: "/workspace"}},
			}
		},
		// All 7 conceded divergences hold on the microVM backend (record
		// 580-593): each ON flag runs its row so a silent widening fails.
		refusesRootExec:         true,
		numericUIDOnly:          true,
		emptyMountLabel:         true,
		ignoresCommandAndCapAdd: true,
		capsOutput:              true,
		gracefulStopPowersOff:   true,
		portableKillError:       true,
		armedDefaultDenyAtStart: true,
		assertDuplicateName: func(t *testing.T, err error) {
			t.Helper()
			if _, ok := errors.AsType[*DuplicateNameError](err); !ok {
				t.Fatalf("duplicate-name Create error = %v (%T), want *DuplicateNameError", err, err)
			}
		},
	}

	runContractSuite(t, func(t *testing.T) WorkloadRuntime {
		t.Helper()
		return NewMicroVMRuntime(e2eConfig(t, env))
	}, caps)
}

// TestMicroVMQBudget records Start latency and per-process PSS as informational output.
// PSS is best-effort because sandboxed helpers may hide their process data.
func TestMicroVMQBudget(t *testing.T) {
	env := microvmtest.Require(t)
	m := NewMicroVMRuntime(e2eConfig(t, env))

	workspace := t.TempDir()
	id, err := m.Create(t.Context(), WorkloadSpec{
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
