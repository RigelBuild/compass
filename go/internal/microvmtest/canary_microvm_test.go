//go:build microvm && unix

// canary_microvm_test.go is the FIRST tagged microVM test. It exists so the CI
// KVM leg (the `gates` job's "microVM suites" step) and its assert-ran guard
// have a real tagged test to compile and run NOW — before V2a's boot spike
// grows the actual boot/integration suites. The guard counts "the packages that
// call microvmtest.Require ran ok"; that count is vacuous until at least one such
// test exists. This is that test.
//
// It is a SMOKE TEST OF THE ENABLEMENT WAVE, not a boot. It proves the whole
// chain the E-wave stands up is wired end to end:
//   - /dev/kvm is openable by the invoking uid — the E5 udev-enable step on the
//     GHA runner (and E6 on the dev box) — because Require probes it (and, under
//     COMPASS_REQUIRE_MICROVM=1, hard-fails rather than skips when it is not).
//   - the guest-image attrs (E3) were realized and exported — KernelImage /
//     RootfsImage point at store paths the CI step built via
//     `nix build -f guest-image/default.nix` and exported into the env.
//   - the VMM binaries (E1) are on PATH — VMMPath / VirtiofsdPath resolved,
//     which Require does with exec.LookPath.
//
// It deliberately does NOT spawn cloud-hypervisor or boot the guest: booting a
// microVM is V2a's job and needs the runtime this record does not own. Asserting
// the resolved Env is fully populated and the two image paths exist on disk is
// the strongest claim this slice can make WITHOUT a boot — and it is a real
// assertion, never a skip-always stub.
//
// It lives in the EXTERNAL test package `microvmtest_test` (not in-package) and
// calls the EXPORTED microvmtest.Require, for two reasons that both matter:
//   - the assert-ran guard finds Require callers with `grep 'microvmtest\.Require'`
//     (ci.yml), which only matches the qualified call an external package makes;
//     an in-package `Require(t)` would leave the guard counting zero packages and
//     reporting itself vacuous. This canary must be the thing that guard counts.
//   - it exercises Require exactly as a real consumer does — through the exported
//     surface — so it is a genuine contract test of the package's public API.
// The test binary still compiles into internal/microvmtest, so `go test` reports
// `ok github.com/RigelBuild/compass/go/internal/microvmtest`, which is what the
// guard asserts.

package microvmtest_test

import (
	"os"
	"testing"

	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// TestCanaryMicroVMEnv resolves the microVM test environment through the shared
// gate and asserts every field the boot suites will depend on is present. Require
// itself gates on /dev/kvm (skip when absent, hard-fail under
// COMPASS_REQUIRE_MICROVM=1) and hard-fails when a guest-image env var or a VMM
// binary is missing, so reaching the assertions below already proves the enable +
// substitute + PATH chain held; the assertions then confirm the resolved Env is
// complete and the image store paths actually exist on disk.
func TestCanaryMicroVMEnv(t *testing.T) {
	env := microvmtest.Require(t)

	// Every field must be populated: an empty one means Require resolved a path
	// this test would later boot with to nothing. Require already Fatalf's on the
	// underlying misconfiguration, so these guard the contract rather than the
	// environment — a future Require regression that returns a partial Env is
	// caught here.
	if env.KernelImage == "" {
		t.Error("resolved Env.KernelImage is empty")
	}
	if env.RootfsImage == "" {
		t.Error("resolved Env.RootfsImage is empty")
	}
	if env.VMMPath == "" {
		t.Error("resolved Env.VMMPath is empty")
	}
	if env.VirtiofsdPath == "" {
		t.Error("resolved Env.VirtiofsdPath is empty")
	}

	// The two guest-image paths must exist on disk: this is what proves E3's
	// attrs were realized and exported, not merely that the env vars were set to
	// some string. The VMM paths came from exec.LookPath inside Require, so they
	// are known-present already; the image paths came from the environment, so
	// they are verified here.
	for _, img := range []struct {
		name string
		path string
	}{
		{"guest kernel", env.KernelImage},
		{"guest rootfs", env.RootfsImage},
	} {
		if _, err := os.Stat(img.path); err != nil {
			t.Errorf("%s image %q does not exist on disk: %v", img.name, img.path, err)
		}
	}
}
