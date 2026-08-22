// Package microvmtest is the shared runtime gate for the compass microVM
// integration tests. A microVM boot is only proven against real virtualization,
// so every test that opens /dev/kvm, spawns cloud-hypervisor/virtiofsd/passt, or
// requires the guest image runs KVM-backed — there is no mock. This package owns
// the one copy of the KVM-availability probe and env-resolution those suites
// share: probe whether /dev/kvm is openable by the current uid, resolve the
// guest-image and VMM/virtiofsd paths from the environment (with PATH
// fallbacks), and SKIP (never fail) when KVM is absent, so the hermetic gate
// stays green on a KVM-less box while the assertions are real wherever KVM
// exists. When COMPASS_REQUIRE_MICROVM is set — the CI KVM leg — that skip
// becomes a hard failure instead, because a skip would silently pass the suite
// without exercising anything.
//
// It is deliberately runtime-agnostic: it hands back an Env of resolved paths,
// and each consuming test drives the VMM itself. That keeps this package free of
// any dependency on the packages it serves (no import cycle) and makes it
// reusable across the V2a+ integration suites.
//
// Unlike pgtest, this package's non-test files compile UNTAGGED, so the moon
// battery type-checks the gate everywhere — even on a box with no KVM. The
// two-tier tag rule lives in the CONSUMERS, not here: an integration test that
// touches KVM/cloud-hypervisor/virtiofsd/passt or the guest image carries
// `//go:build microvm && unix` and calls microvmtest.Require(t) first; an
// untagged test must run on a KVM-less box. This package is the untagged shared
// gate those tagged tests import.

package microvmtest

import (
	"os"
	"os/exec"
	"testing"
)

// RequireMicroVMEnvVar, when set to a non-empty value, turns the no-KVM SKIP
// path into a hard failure: with /dev/kvm not openable, Require would normally
// skip so the suite stays green on a KVM-less box, but where KVM is mandatory
// (the CI KVM leg sets this) a skip would silently pass the suite without
// exercising anything. Setting COMPASS_REQUIRE_MICROVM=1 makes that case fail
// loudly instead — the pgtest/COMPASS_REQUIRE_LIVE posture.
const RequireMicroVMEnvVar = "COMPASS_REQUIRE_MICROVM"

// KernelEnvVar points the harness at the guest kernel image to boot. It is
// supplied by the environment — the dev shell and the CI KVM leg export it
// pointing at the `nix build .#compass-guest-kernel` result — keeping this
// package agnostic about how the image is built (the pgtest posture: resolve a
// path, do not own its lifecycle). Require fails loudly when it is unset on a
// run that has already cleared the KVM gate.
const KernelEnvVar = "COMPASS_TEST_GUEST_KERNEL"

// RootfsEnvVar points the harness at the guest rootfs image to boot. Like
// KernelEnvVar it is supplied by the environment (the dev shell / CI KVM leg
// export it pointing at the `nix build .#compass-guest-rootfs` result).
const RootfsEnvVar = "COMPASS_TEST_GUEST_ROOTFS"

// vmmBinary is the VMM the microVM suites drive; virtiofsdBinary is the
// virtio-fs daemon they pair it with. Both are resolved from PATH (the dev shell
// and CI KVM leg put them there) when Require builds the Env.
const (
	vmmBinary       = "cloud-hypervisor"
	virtiofsdBinary = "virtiofsd"
)

// Env is the resolved microVM test environment Require hands back: the guest
// images to boot and the host binaries to drive them with.
type Env struct {
	KernelImage   string
	RootfsImage   string
	VMMPath       string
	VirtiofsdPath string
}

// kvmSource is which of the three KVM-availability paths Require takes.
type kvmSource int

const (
	sourceProceed     kvmSource = iota // /dev/kvm openable
	sourceSkipNoKVM                    // /dev/kvm not openable, require flag unset
	sourceFailRequire                  // /dev/kvm not openable, COMPASS_REQUIRE_MICROVM set
)

// decideKVMSource is the pure policy behind Require, split out so the dispatch is
// unit-testable without real KVM or a *testing.T. The /dev/kvm probe result is
// injected as kvmOpenable, exactly as decideDSNSource takes an injected cli/dsn
// string rather than probing itself.
func decideKVMSource(kvmOpenable bool, requireMicroVM string) kvmSource {
	if kvmOpenable {
		return sourceProceed
	}
	if requireMicroVM != "" {
		return sourceFailRequire
	}
	return sourceSkipNoKVM
}

// Require gates a microVM integration test on KVM availability and returns the
// resolved Env for the test to boot with. It probes whether /dev/kvm is openable
// by the current uid; when it is, it resolves the guest images from
// COMPASS_TEST_GUEST_KERNEL / COMPASS_TEST_GUEST_ROOTFS and the VMM/virtiofsd
// binaries from PATH, and returns the Env. When /dev/kvm is not openable it
// SKIPS the test so the suite stays green on a KVM-less box — unless
// COMPASS_REQUIRE_MICROVM is set, in which case it FAILS LOUDLY (see
// RequireMicroVMEnvVar), for contexts like the CI KVM leg where KVM is mandatory.
func Require(t *testing.T) Env {
	t.Helper()
	switch decideKVMSource(kvmOpenable(), os.Getenv(RequireMicroVMEnvVar)) {
	case sourceProceed:
		return resolveEnv(t)
	case sourceSkipNoKVM:
		t.Skip("/dev/kvm is not openable by the current uid; skipping microVM integration test")
	case sourceFailRequire:
		t.Fatalf("%s=%s requires KVM but /dev/kvm is not openable by the current uid: "+
			"run on a KVM-capable host or unset %s to allow the skip",
			RequireMicroVMEnvVar, os.Getenv(RequireMicroVMEnvVar), RequireMicroVMEnvVar)
	}
	return Env{}
}

// kvmOpenable reports whether /dev/kvm can be opened by the current uid. It opens
// (and immediately closes) the device rather than stat-ing it, because presence
// on the filesystem does not imply the uid has the read/write access a VMM needs;
// an open is the authoritative probe.
func kvmOpenable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	// Probe only; the VMM opens its own handle. Close error is not actionable.
	_ = f.Close()
	return true
}

// resolveEnv populates an Env from the guest-image env vars and PATH lookups for
// the VMM/virtiofsd binaries. A missing guest-image var or a binary absent from
// PATH is a hard failure on a run that has already cleared the KVM gate: the
// guest images and VMM are prerequisites of any boot, so their absence is a
// misconfigured environment, not a reason to silently skip into a green suite.
// The images are supplied by the environment (see KernelEnvVar/RootfsEnvVar), so
// their absence is caught here rather than defaulted to a build path this
// package does not own.
func resolveEnv(t *testing.T) Env {
	t.Helper()
	vmmPath, err := exec.LookPath(vmmBinary)
	if err != nil {
		t.Fatalf("microVM test requires %s on PATH: %v", vmmBinary, err)
	}
	virtiofsdPath, err := exec.LookPath(virtiofsdBinary)
	if err != nil {
		t.Fatalf("microVM test requires %s on PATH: %v", virtiofsdBinary, err)
	}
	kernelImage := os.Getenv(KernelEnvVar)
	if kernelImage == "" {
		t.Fatalf("microVM test requires %s to point at the guest kernel image "+
			"(exported by the dev shell / CI KVM leg from `nix build .#compass-guest-kernel`)", KernelEnvVar)
	}
	rootfsImage := os.Getenv(RootfsEnvVar)
	if rootfsImage == "" {
		t.Fatalf("microVM test requires %s to point at the guest rootfs image "+
			"(exported by the dev shell / CI KVM leg from `nix build .#compass-guest-rootfs`)", RootfsEnvVar)
	}
	return Env{
		KernelImage:   kernelImage,
		RootfsImage:   rootfsImage,
		VMMPath:       vmmPath,
		VirtiofsdPath: virtiofsdPath,
	}
}
