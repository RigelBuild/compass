package runtime

// The microVM backend seam suite: hermetic, no subprocess. These pin the
// backend-selection contract (the transitional podman default, the microVM
// opt-in, the loud rejection of an unknown backend) and the not-implemented
// posture every MicroVMRuntime method holds until the in-guest control plane
// lands.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// Every MicroVMRuntime method returns ErrMicroVMNotImplemented until the
// in-guest control plane lands. This pins the not-implemented contract so the
// task filling in each verb deliberately deletes its row here rather than
// silently regressing past a faked container operation.
func TestMicroVMRuntimeNotImplemented(t *testing.T) {
	ctx := context.Background()
	m := NewMicroVMRuntime(MicroVMConfig{})

	tests := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { _, err := m.Create(ctx, ContainerSpec{}); return err }},
		{"Start", func() error { return m.Start(ctx, ContainerID("c")) }},
		{"Exec", func() error { _, err := m.Exec(ctx, ContainerID("c"), ExecSpec{}); return err }},
		{"ExecStreaming", func() error { _, err := m.ExecStreaming(ctx, ContainerID("c"), StreamingExecSpec{}); return err }},
		{"Stop", func() error { return m.Stop(ctx, ContainerID("c"), time.Second) }},
		{"Remove", func() error { return m.Remove(ctx, ContainerID("c")) }},
		{"Exists", func() error { _, err := m.Exists(ctx, "c"); return err }},
		{"MountLabel", func() error { _, err := m.MountLabel(ctx, ContainerID("c")); return err }},
		{"Resize", func() error { return m.Resize(ctx, ContainerID("c"), ResourceLimits{}) }},
	}
	for _, tt := range tests {
		if err := tt.call(); !errors.Is(err, ErrMicroVMNotImplemented) {
			t.Errorf("%s err = %v, want ErrMicroVMNotImplemented", tt.name, err)
		}
	}
}
