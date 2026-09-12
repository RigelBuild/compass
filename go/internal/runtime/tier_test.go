package runtime

import "testing"

// TestEveryBackendNamesItsOwnTier: the tier is reported on a security surface,
// so each production backend must name itself rather than fall through to the
// empty tier. This catches a regression on a listed backend; a NEW backend is
// only covered once it is added here and to the SelectBackend cases below.
func TestEveryBackendNamesItsOwnTier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine WorkloadRuntime
		want   WorkloadTier
	}{
		{"podman", NewPodmanCLI(), WorkloadTierPodman},
		{"microvm", NewMicroVMRuntime(MicroVMConfig{}), WorkloadTierMicroVM},
		{"apple-container", NewAppleContainerCLI(AppleContainerConfig{}), WorkloadTierAppleContainer},
		{"host", NewHostRuntime(t.TempDir()), WorkloadTierHost},
	} {
		if got := TierOf(tc.engine); got != tc.want {
			t.Errorf("TierOf(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTierOfReportsUnknownRatherThanGuessing: a backend that does not name its
// tier reports the empty tier, which the wire renders as unspecified. Defaulting
// to a plausible tier would mislabel a session on a surface an operator trusts.
func TestTierOfReportsUnknownRatherThanGuessing(t *testing.T) {
	if got := TierOf(newFakeRuntime(t)); got != "" {
		t.Errorf("TierOf(a backend with no Tier method) = %q, want the empty tier", got)
	}
}

// TestSelectBackendTierMatchesTheRequestedBackend closes the loop the two tests
// above leave open: each is correct in isolation, but a mis-wired SelectBackend
// arm would hand back a healthy backend of the WRONG tier and both would still
// pass. The empty string is podman's documented default.
func TestSelectBackendTierMatchesTheRequestedBackend(t *testing.T) {
	for _, tc := range []struct {
		backend string
		want    WorkloadTier
	}{
		{"", WorkloadTierPodman},
		{"podman", WorkloadTierPodman},
		{"microvm", WorkloadTierMicroVM},
		{"apple-container", WorkloadTierAppleContainer},
		{"host", WorkloadTierHost},
	} {
		engine, err := SelectBackend(BackendConfig{Backend: tc.backend})
		if err != nil {
			t.Fatalf("SelectBackend(%q) error = %v", tc.backend, err)
		}
		if got := TierOf(engine); got != tc.want {
			t.Errorf("SelectBackend(%q) resolved a backend of tier %q, want %q", tc.backend, got, tc.want)
		}
	}
}
