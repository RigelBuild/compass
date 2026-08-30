//go:build podman

package runtime

// Real-container substrate proof for config delivery (RIG-1675), against real
// rootless podman. This is the seam the config-in-place-update design rests on
// and that no other test exercises against a real container:
//
//  1. the materialized config tree is bind-mounted read-only at the production
//     path /run/compass/agent-config,
//  2. inside the container, current/ resolves to the active version dir and its
//     contents are readable by the unprivileged agent user,
//  3. a host-side current -> version symlink flip is visible LIVE inside the
//     already-running container with no remount and no restart — the Runner
//     mounts the PARENT dir (never the resolved version dir), so a later
//     ConfigMaterializer flip is picked up without re-mounting,
//  4. (only where SELinux labels mounts) the mount carries a per-container MCS label.
//
// The runner-half Go wiring (Provision -> Start -> RefreshConfig -> Reload) is
// already proven end-to-end by config_refresh_test with the podman binary
// stubbed; this test deliberately does NOT re-drive it. It builds the host
// config tree by hand (mirroring config_materialize.go's layout) rather than
// through ConfigMaterializer + the fetch stack, because the property under test
// is the runtime substrate, not materialization (config_materialize_test covers
// that). No agent binary is needed — a shell in the container observes the mount.
//
// Skipped (not failed) when podman isn't usable, matching lifecycle_test; the
// MCS-label assertion is additionally skipped where SELinux does not label
// mounts (e.g. a NixOS host with SELinux disabled), so it is real wherever it does.
// Build-tagged (podman) so it is not part of the hermetic gate.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeConfigTree lays out a config root exactly as config_materialize.go does:
//
//	root/<version>/config.txt   (contents = the version string)
//	root/current -> <version>   (relative symlink)
//
// for each version, then points current at initial. Returns the root, which is
// the bind-mount source (the PARENT dir, never a version dir).
func writeConfigTree(t *testing.T, versions []string, initial string) string {
	t.Helper()
	root := t.TempDir()
	for _, v := range versions {
		dir := filepath.Join(root, v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir version dir %q: %v", v, err)
		}
		// 0o666, not 0o644: the sentinel and the root mode must move together.
		// The root is pinned 0o755 below so the agent (uid 1000 under
		// --userns=keep-id) can traverse it regardless of the invoking host uid;
		// were the sentinel left 0o644, on a box where the invoker uid != 1000
		// the agent (mapped as "other") could not write it even absent the ro
		// mount, so assertion 2's write-rejection would come from file perms, not
		// the read-only mount — a vacuous green. 0o666 keeps the ro mount the sole
		// write barrier for any invoker uid.
		if err := os.WriteFile(filepath.Join(dir, "config.txt"), []byte(v), 0o666); err != nil {
			t.Fatalf("write config.txt for %q: %v", v, err)
		}
	}
	// Pin the root 0o755, mirroring ConfigMaterializer.ensureRoot
	// (config_materialize.go:180-186): t.TempDir defaults to 0o700, which only
	// the owner can traverse, so a confined agent whose in-userns uid differs
	// from the root owner could not resolve current/ into the tree. Production
	// pins 0o755 for exactly this reason.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("pin config root %q mode: %v", root, err)
	}
	flipCurrentSymlink(t, root, initial)
	return root
}

// flipCurrentSymlink atomically repoints root/current at version, mirroring
// ConfigMaterializer.flipCurrent: write a temp symlink then rename over current,
// so a reader inside the container never observes a torn intermediate state
// (never unlink-then-symlink). The link is relative (the version basename), as
// the materializer writes it, so it resolves the same way inside the container
// where root is mounted at agentConfigMountPath.
func flipCurrentSymlink(t *testing.T, root, version string) {
	t.Helper()
	tmp := filepath.Join(root, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		t.Fatalf("write temp current symlink -> %q: %v", version, err)
	}
	if err := os.Rename(tmp, filepath.Join(root, "current")); err != nil {
		t.Fatalf("atomic rename current -> %q: %v", version, err)
	}
}

// selinuxLabelsMounts reports whether this host runs SELinux in a mode where
// podman stamps bind mounts with a per-container MCS label. Both the kernel
// selinuxfs must be present AND podman must report SELinux enabled — a host with
// selinuxfs but podman's SELinux support off still yields an empty MountLabel.
// It detects SELinux *enabled* (not specifically enforcing) on purpose: podman
// labels mounts in permissive mode too, so "enabled" is the right predicate for
// the MountLabel assertion.
func selinuxLabelsMounts() bool {
	if _, err := os.Stat("/sys/fs/selinux"); err != nil {
		return false
	}
	out, err := exec.Command("podman", "info", "--format", "{{.Host.Security.SELinuxEnabled}}").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// TestConfigMountIsReadOnlyAndFlipVisibleLive proves the config-delivery
// substrate against a real container: read-only mount at the production path,
// current resolution inside, and a live current flip visible with no remount.
func TestConfigMountIsReadOnlyAndFlipVisibleLive(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}

	buildImage(t, t.TempDir())

	// Two versions, current -> v1 to start. root is the PARENT dir the Runner
	// bind-mounts (config_materialize.go: mounting the parent keeps a later
	// current flip visible inside a live container).
	root := writeConfigTree(t, []string{"v1", "v2"}, "v1")

	name := "compass-configmount-" + strconv.Itoa(os.Getpid())
	facade := NewAgentRuntime(NewPodmanCLI())
	ctx := context.Background()

	// Force-remove any leftover from a crashed run so the name is free, and
	// guard teardown (Go has no Drop): a failure before Teardown must not leak a
	// container that collides with the next run.
	_ = exec.Command("podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", name).Run() })

	// The exact production mount contract (host.go:152-168): parent root ->
	// agentConfigMountPath, read-only. Keep this literal in sync with
	// runner.agentConfigMountPath (host.go:37) — it is unexported across the
	// package boundary, so a production path change will not propagate here.
	const configMountPath = "/run/compass/agent-config"
	spec := AgentSpec{
		Name:  name,
		Image: imageTag,
		Workspace: Workspace{
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
		},
		Egress: MustAllowEgress(allowedHost),
		Mounts: []Mount{{HostPath: root, ContainerPath: configMountPath, ReadOnly: true}},
	}

	handle, err := facade.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("launch the agent container: %v", err)
	}

	// 1. current/ resolves to v1 inside the container and its contents are
	// readable by the unprivileged agent user.
	got := execAgentStdout(t, ctx, facade, handle, "cat "+configMountPath+"/current/config.txt")
	if got != "v1" {
		t.Fatalf("config current/config.txt inside container = %q, want %q", got, "v1")
	}

	// 2. The mount is read-only: a write into it is rejected. The agent is
	// unprivileged and the bind mount is ro, so the write must fail (EROFS).
	wrote := execAgentStdout(t, ctx, facade, handle,
		"sh -c 'echo tampered > "+configMountPath+"/current/config.txt 2>/dev/null; echo rc=$?'")
	if strings.Contains(wrote, "rc=0") {
		t.Fatalf("write into the read-only config mount must fail, got: %s", wrote)
	}

	// 3. LIVE flip: repoint current -> v2 on the HOST, atomically. No remount,
	// no restart. The already-running container must see the new version because
	// it mounts the PARENT dir. This is the load-bearing assertion: it fails if
	// the mount is regressed to the version dir instead of the parent.
	flipCurrentSymlink(t, root, "v2")
	afterFlip := execAgentStdout(t, ctx, facade, handle, "cat "+configMountPath+"/current/config.txt")
	if afterFlip != "v2" {
		t.Fatalf("after host-side current flip, config inside container = %q, want %q (live flip not visible — is the parent dir mounted?)", afterFlip, "v2")
	}

	// 4. SELinux MCS label: only meaningful where SELinux labels mounts. Where
	// it is disabled (e.g. this NixOS box) MountLabel is empty and :Z is a no-op,
	// so skip the sub-assertion with a note rather than fail. This branch is
	// compile-checked only in the disabled case; its runtime path (the
	// MountLabel read + non-empty check) is unverified pending an SELinux host.
	if selinuxLabelsMounts() {
		label, err := NewPodmanCLI().MountLabel(ctx, handle.ID())
		if err != nil {
			t.Fatalf("read MountLabel on an SELinux host: %v", err)
		}
		if strings.TrimSpace(label) == "" {
			t.Fatal("on an SELinux host the container must carry a non-empty MCS mount label")
		}
	} else {
		t.Log("SELinux does not label mounts here; skipping the MCS mount-label assertion (real only where SELinux is enabled)")
	}

	// 5. Teardown removes the container.
	if err := facade.Teardown(ctx, handle); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if runtimeExists(name) {
		t.Fatal("container must be gone after teardown")
	}
}

// execAgentStdout runs a shell command as the agent user inside the container
// and returns its trimmed stdout, failing the test on a runtime error.
func execAgentStdout(t *testing.T, ctx context.Context, facade *AgentRuntime, handle *AgentHandle, command string) string {
	t.Helper()
	out, err := facade.ExecAsAgent(ctx, handle, "sh", "-c", command)
	if err != nil {
		t.Fatalf("exec %q as agent: %v", command, err)
	}
	return strings.TrimSpace(out.Stdout)
}
