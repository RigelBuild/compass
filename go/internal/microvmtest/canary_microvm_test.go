//go:build microvm && unix

// The FIRST tagged microVM test. It exists so the CI KVM leg and its assert-ran
// guard have a real tagged test to compile and run NOW — the guard counts "the
// packages that call microvmtest.Require ran ok", vacuous until one such test
// exists. This is that test.

// A SMOKE TEST OF THE ENABLEMENT WAVE, not a boot. It proves the chain is wired:
// /dev/kvm openable by the uid (E5/E6, via Require's probe), the guest-image
// attrs realized and exported (E3), and the VMM binaries on PATH (E1).

// It deliberately does NOT boot the guest — that is V2a's job. Asserting the Env
// is populated and both image paths exist on disk is the strongest claim without
// a boot. Distinct from the V5 boot canary (runtime.BootCanary), which DOES boot;
// despite the shared word the two are unrelated (record §(g)).

// It lives in the EXTERNAL package `microvmtest_test` and calls the EXPORTED
// Require: the guard greps the qualified call (an in-package call would count
// zero), and it exercises Require as a real consumer through the public surface.
// The binary still compiles into internal/microvmtest, so `go test` reports ok.

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

	// Every field must be populated: an empty one means Require resolved a boot
	// path to nothing. Require already Fatalf's on the misconfiguration, so these
	// guard the contract — a future regression returning a partial Env is caught.
	if env.KernelImage == "" {
		t.Error("resolved Env.KernelImage is empty")
	}
	if env.InitrdImage == "" {
		t.Error("resolved Env.InitrdImage is empty")
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
	if env.PasstPath == "" {
		t.Error("resolved Env.PasstPath is empty")
	}

	// The two guest-image paths must exist on disk: this proves E3's attrs were
	// realized and exported, not merely that the env vars were set. The VMM paths
	// came from exec.LookPath inside Require, so they are known-present already.
	for _, img := range []struct {
		name string
		path string
	}{
		{"guest kernel", env.KernelImage},
		{"guest rootfs", env.RootfsImage},
		{"guest initrd", env.InitrdImage},
	} {
		if _, err := os.Stat(img.path); err != nil {
			t.Errorf("%s image %q does not exist on disk: %v", img.name, img.path, err)
		}
	}
}
