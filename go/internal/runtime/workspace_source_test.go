package runtime

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// podmanTierFakeRuntime reports a tier that applies mounts.
type podmanTierFakeRuntime struct{ *fakeRuntime }

func (podmanTierFakeRuntime) Tier() WorkloadTier { return WorkloadTierPodman }

func volumeSpec(mounts ...Mount) AgentSpec {
	spec := specWithCreds(false)
	spec.Workspace.Source = SourceVolume
	spec.Mounts = mounts
	return spec
}

func TestLaunchSourceVolumeMountsCheckoutDirReadWrite(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(podmanTierFakeRuntime{fake})
	spec := volumeSpec(Mount{HostPath: "/var/lib/compass/vol/a", ContainerPath: "/work/repo"})

	if _, err := rt.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch error = %v", err)
	}
	fake.mu.Lock()
	created := fake.created.Mounts
	fake.mu.Unlock()
	if !slices.Contains(created, Mount{HostPath: "/var/lib/compass/vol/a", ContainerPath: "/work/repo"}) {
		t.Fatalf("WorkloadSpec.Mounts = %v, want the writable volume mount at the checkout dir", created)
	}
	// The checkout dir is still created on the mounted path, so a fresh volume
	// gets an agent-owned working dir exactly like the clone-dir path.
	calls := fake.callsSnapshot()
	if !slices.Contains(calls, "exec:mkdir -p /work/repo") {
		t.Fatalf("calls = %v, want mkdir -p on the mounted checkout dir", calls)
	}
}

func TestLaunchSourceVolumeRejectsSpecWithoutWritableCheckoutMount(t *testing.T) {
	tests := []struct {
		name   string
		mounts []Mount
	}{
		{name: "no mount"},
		{name: "read-only mount", mounts: []Mount{{HostPath: "/v", ContainerPath: "/work/repo", ReadOnly: true}}},
		{name: "mount elsewhere", mounts: []Mount{{HostPath: "/v", ContainerPath: "/work/other"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRuntime(t)
			rt := NewAgentRuntime(podmanTierFakeRuntime{fake})
			_, err := rt.Launch(t.Context(), volumeSpec(tc.mounts...))
			if !errors.Is(err, ErrVolumeMountMissing) {
				t.Fatalf("Launch error = %v, want ErrVolumeMountMissing", err)
			}
			// Rejected before any container exists, so nothing needs cleanup.
			if calls := fake.callsSnapshot(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "create:") }) {
				t.Fatalf("calls = %v, want no create", calls)
			}
		})
	}
}

func TestLaunchZeroSourceIsCloneDir(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)
	spec := specWithCreds(false)
	if spec.Workspace.Source != SourceCloneDir {
		t.Fatalf("zero-value Source = %v, want SourceCloneDir", spec.Workspace.Source)
	}
	if _, err := rt.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch error = %v", err)
	}
}

// hostTierFakeRuntime reports the host tier, whose backend ignores mounts.
type hostTierFakeRuntime struct{ *fakeRuntime }

func (hostTierFakeRuntime) Tier() WorkloadTier { return WorkloadTierHost }

// A tier that ignores mounts, or one that does not name itself (a wrapper that
// drops Tier), would run the checkout outside the volume, so both are refused.
func TestLaunchSourceVolumeRefusedOnTierWithoutMounts(t *testing.T) {
	tests := map[string]func(*fakeRuntime) WorkloadRuntime{
		"host tier":    func(f *fakeRuntime) WorkloadRuntime { return hostTierFakeRuntime{f} },
		"unknown tier": func(f *fakeRuntime) WorkloadRuntime { return f },
	}
	for name, wrap := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeRuntime(t)
			rt := NewAgentRuntime(wrap(fake))
			spec := volumeSpec(Mount{HostPath: "/v", ContainerPath: "/work/repo"})
			if _, err := rt.Launch(t.Context(), spec); !errors.Is(err, ErrVolumeMountMissing) {
				t.Fatalf("Launch error = %v, want ErrVolumeMountMissing", err)
			}
			if calls := fake.callsSnapshot(); len(calls) != 0 {
				t.Fatalf("calls = %v, want none", calls)
			}
		})
	}
}

func TestLaunchUnknownSourceRefused(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(podmanTierFakeRuntime{fake})
	spec := specWithCreds(false)
	spec.Workspace.Source = SourceVolume + 1
	if _, err := rt.Launch(t.Context(), spec); !errors.Is(err, ErrUnknownWorkspaceSource) {
		t.Fatalf("Launch error = %v, want ErrUnknownWorkspaceSource", err)
	}
	if calls := fake.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("calls = %v, want none", calls)
	}
}
