//go:build podman

package runtime

// Arbitrary-host-uid remap proof against real rootless podman
// (docs/designs/infra/runtime/compass-runner-arbitrary-uid/design.md §P3). The GA
// contract: a launch whose remap target differs from the invoking host uid
// still yields an agent that is the baked agent uid in-container and owns
// /nix-equivalent paths.
//
// The dev box runs as uid 1000, so the mapping test inverts the probe — remap
// to a NON-host uid and assert the mapping — which exercises the identical
// keep-id:uid= mechanism an arbitrary-host-uid deployment relies on. Against
// bare --userns=keep-id the in-container uid is the host uid (the defect); the
// remap flip makes it the spec'd uid.
//
// Cases 1-2 are alpine-based (no compass-agent image dependency); case 3 runs
// against the real compass-agent:latest when present, skipped otherwise.
// Skipped (not failed) when podman isn't usable, matching lifecycle_test /
// config_mount_test. Build-tagged (podman) so it is not part of the hermetic
// gate.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// agentRemapImage is the real agent image case 3 probes for /nix ownership.
// Mirrors config_delivery_e2e_test.go's agentImage: a missing image means skip,
// not fail.
const agentRemapImage = "compass-agent:latest"

// agentRemapImageExists reports whether the real agent image is present in
// local storage, mirroring config_delivery_e2e_test.go's agentImageExists.
func agentRemapImageExists() bool {
	return exec.Command("podman", "image", "exists", agentRemapImage).Run() == nil
}

// createStartExec creates + starts a container from spec, execs command inside
// it (as the container's default user), and returns the trimmed stdout. It
// registers teardown so a leaked container never collides with the next run.
func createStartExec(t *testing.T, ctx context.Context, cli *PodmanCLI, spec ContainerSpec, command ...string) ExecOutput {
	t.Helper()

	// Force-remove any leftover from a crashed run so the name is free, then
	// guard teardown (Go has no Drop). Deferred-cleanup discards in test code:
	// the rm is best-effort — a failure means nothing to clean, not a test
	// failure.
	_ = exec.Command("podman", "rm", "--force", spec.Name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", spec.Name).Run() })

	id, err := cli.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if err := cli.Start(ctx, id); err != nil {
		t.Fatalf("start container: %v", err)
	}
	out, err := cli.Exec(ctx, id, NewExecSpec(command...))
	if err != nil {
		t.Fatalf("exec %q: %v", command, err)
	}
	if !out.Success() {
		t.Fatalf("exec %q exited %d: %s", command, out.ExitCode, out.Stderr)
	}
	return out
}

// TestKeepIDRemapMapsHostUIDToSpecUID is the red/green heart of the slice: with
// a remap target distinct from the invoking host uid, `id -u` inside the
// container must equal the spec'd UID, not the host uid. RED against bare
// --userns=keep-id (in-container uid == host uid); GREEN once createArgs emits
// keep-id:uid=<UID>.
func TestKeepIDRemapMapsHostUIDToSpecUID(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background()

	hostUID := os.Getuid()
	// A target distinct from the host uid so the mapping is observable. 2000 is
	// arbitrary and differs from the dev-box host uid (1000).
	const targetUID uint32 = 2000
	if int(targetUID) == hostUID {
		t.Fatalf("target uid %d must differ from the host uid %d for the mapping to be observable", targetUID, hostUID)
	}

	spec := ContainerSpec{
		Image:   "docker.io/library/alpine:latest",
		Name:    "compass-usernsremap-map-" + strconv.Itoa(os.Getpid()),
		UID:     targetUID,
		Command: []string{"sleep", "infinity"},
	}
	out := createStartExec(t, ctx, NewPodmanCLI(), spec, "id", "-u")

	got := strings.TrimSpace(out.Stdout)
	want := strconv.FormatUint(uint64(targetUID), 10)
	if got != want {
		t.Fatalf("in-container uid = %q, want %q (the remap must map host uid %d to the spec'd uid %d, not pass the host uid through)",
			got, want, hostUID, targetUID)
	}
}

// TestKeepIDRemapBindMountRoundTrip proves §(c): a file the container user
// writes into a bind-mounted host dir lands on the host owned by the INVOKING
// host uid, not the mapped container uid. keep-id:uid=N maps the invoking host
// user to container uid N, so container-N writes still round-trip back to the
// invoker on the host.
func TestKeepIDRemapBindMountRoundTrip(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background()

	dir := t.TempDir()
	spec := ContainerSpec{
		Image:   "docker.io/library/alpine:latest",
		Name:    "compass-usernsremap-mount-" + strconv.Itoa(os.Getpid()),
		UID:     2000,
		Mounts:  []Mount{{HostPath: dir, ContainerPath: "/mnt", ReadOnly: false}},
		Command: []string{"sleep", "infinity"},
	}
	// Write the probe file as the container user (default user under the remap),
	// then read back the host-side owner.
	createStartExec(t, ctx, NewPodmanCLI(), spec, "touch", "/mnt/probe")

	info, err := os.Stat(dir + "/probe")
	if err != nil {
		t.Fatalf("stat host-side probe file: %v", err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat_t unavailable on this platform: %T", info.Sys())
	}
	if int(sys.Uid) != os.Getuid() {
		t.Fatalf("host probe file owned by uid %d, want the invoking host uid %d (the bind-mount round-trip must land back on the invoker)",
			sys.Uid, os.Getuid())
	}
}

// TestKeepIDRemapAgentOwnsNix is the GA contract against the real image: launch
// the compass-agent image under the remap and assert /nix inside is owned by
// the baked agent uid (1000). Skipped when the image is absent (it is the heavy
// dogfood nix build, not an in-test one), mirroring config_delivery_e2e_test.
func TestKeepIDRemapAgentOwnsNix(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	if !agentRemapImageExists() {
		t.Skip(agentRemapImage + " not present in local storage")
	}
	ctx := context.Background()

	spec := ContainerSpec{
		Image:   agentRemapImage,
		Name:    "compass-usernsremap-nix-" + strconv.Itoa(os.Getpid()),
		UID:     1000,
		Command: []string{"sleep", "infinity"},
	}
	out := createStartExec(t, ctx, NewPodmanCLI(), spec, "stat", "-c", "%u", "/nix")

	got := strings.TrimSpace(out.Stdout)
	if got != "1000" {
		t.Fatalf("/nix owner uid inside container = %q, want %q (the baked agent uid must own /nix under the remap)", got, "1000")
	}
}
