package runtime

// The microVM backend-selection suite: hermetic, no subprocess, no build tag.
// It pins the backend-selection contract (the transitional podman default, the
// microVM opt-in, the loud rejection of an unknown backend) — the part of the
// microVM seam that must type-check and run on any platform. The lifecycle
// method behavior (spec→BootConfig, spec→ExecCall, the session table, mount
// refusal, idempotent Remove) lives in microvm_lifecycle_test.go behind
// //go:build unix, matching the unix-only lifecycle file it exercises.

import (
	"strings"
	"testing"
)

// SelectBackend defaults to podman: an empty or explicit "podman" backend must
// return the container path (a *PodmanCLI), never the unfinished microVM path.
// This is the transitional kill switch — an unset backend stays on the proven
// runtime. Surrounding whitespace (stray from env/CI wiring) is tolerated.
func TestSelectBackendDefaultsToPodman(t *testing.T) {
	for _, backend := range []string{"", "podman", "  podman  ", "\tpodman\n"} {
		rt, err := SelectBackend(BackendConfig{Backend: backend})
		if err != nil {
			t.Fatalf("SelectBackend(%q) err = %v, want nil", backend, err)
		}
		if _, ok := rt.(*PodmanCLI); !ok {
			t.Fatalf("SelectBackend(%q) = %T, want *PodmanCLI", backend, rt)
		}
	}
}

// SelectBackend("microvm") returns the microVM runtime.
func TestSelectBackendMicroVM(t *testing.T) {
	rt, err := SelectBackend(BackendConfig{Backend: "microvm"})
	if err != nil {
		t.Fatalf("SelectBackend(microvm) err = %v, want nil", err)
	}
	if _, ok := rt.(*MicroVMRuntime); !ok {
		t.Fatalf("SelectBackend(microvm) = %T, want *MicroVMRuntime", rt)
	}
}

// An unknown backend is a loud refusal: a nil runtime plus an error naming the
// bad value, never a silent fallback to a default backend.
func TestSelectBackendUnknown(t *testing.T) {
	rt, err := SelectBackend(BackendConfig{Backend: "bogus"})
	if rt != nil {
		t.Fatalf("SelectBackend(bogus) runtime = %v, want nil", rt)
	}
	if err == nil {
		t.Fatal("SelectBackend(bogus) err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("SelectBackend(bogus) err = %q, want it to name the bad value", err)
	}
}
