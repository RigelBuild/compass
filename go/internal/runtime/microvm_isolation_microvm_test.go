//go:build microvm && unix

package runtime

// The KVM-gated virtio-fs ISOLATION suite (record §Plan V6 Test cycle). This is
// the slice that PROVES inter-tenant isolation rather than exercising a happy
// path, so every assertion here drives a real MicroVMRuntime through
// Create→Start and then execs inside the live guest to ATTEMPT an escape,
// asserting confinement from both sides of the boundary (what the guest can
// name, and what actually landed on the host).
//
// Every test calls microvmtest.Require(t) FIRST, mirroring
// microvm_lifecycle_microvm_test.go: on a KVM-less box it SKIPS, and under
// COMPASS_REQUIRE_MICROVM=1 that skip becomes a hard failure — so a green run
// proves the suite really booted guests.
//
// The three legs, and what each actually proves:
//
//  1. Path traversal (TestMicroVMVolumeTraversalConfined) — the guest attempts
//     to read and write outside its volume via `..`, an absolute host path, and
//     a symlink planted inside the volume pointing at a host path outside it.
//     Confinement is asserted on BOTH sides: the guest cannot read the outside
//     canary's content, and the host-side outside tree is byte-for-byte
//     unchanged afterwards. The host-side half is the load-bearing one — a guest
//     read failing could be a missing file, but an unchanged host tree after a
//     write attempt is confinement.
//  2. Cross-session unreachability (TestMicroVMCrossSessionVolumeUnreachable) —
//     two sessions boot with distinct volumes; guest A cannot read B's secret by
//     any path, and nothing A writes appears in B's host-side volume.
//  3. Host-ownership parity (TestMicroVMHostOwnershipParity) — a file the guest
//     agent creates on the shared volume must land with the SAME host-side
//     (uid,gid) the podman `--userns=keep-id:uid=N,gid=N` path produces: the
//     INVOKING host user, not the in-guest agent id (podman.go createArgs).
//     This is the test that decided whether launch.go needed virtiofsd uid/gid
//     translation (record §(d)) — see parityTargetUID/GID below.
//
// The quota-enforcement-in-guest leg is gated separately and skips honestly; see
// TestMicroVMVolumeQuotaEnforcedInGuest.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// outsideCanaryBody is the content planted in a host file OUTSIDE the session
// volume. A guest exec that ever prints this string has escaped the volume
// subtree, so it is a distinctive sentinel rather than a generic word.
const outsideCanaryBody = "HOST-ONLY-CANARY-8f3ac1d0-must-never-be-readable-from-a-guest"

// isolationSession boots one session against a fresh volume dir and returns the
// runtime, its id, and the host-side volume path. Teardown is registered so a
// failed assertion still tears the VM down. The volume is a t.TempDir() child so
// it is removed with the test; the SHORT runroot comes from e2eConfig (the
// AF_UNIX sun_path budget, microvm_lifecycle_microvm_test.go).
func isolationSession(t *testing.T, env microvmtest.Env, name string) (*MicroVMRuntime, ContainerID, string) {
	t.Helper()
	m := NewMicroVMRuntime(e2eConfig(t, env))
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(volume, 0o700); err != nil {
		t.Fatalf("creating session volume %s: %v", volume, err)
	}
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   name,
		UID:    agentuid.AgentUID,
		Mounts: []Mount{{HostPath: volume, ContainerPath: workspaceMountPath}},
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	t.Cleanup(func() {
		if err := m.Remove(t.Context(), id); err != nil {
			t.Errorf("Remove(%s): %v", name, err)
		}
	})
	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	return m, id, volume
}

// guestSh runs a shell snippet in the guest as the agent uid and returns its
// combined stdout+stderr and exit code. A transport/refusal error is fatal; a
// NON-ZERO EXIT IS NOT — a denied escape attempt is expected to exit non-zero,
// and that is the outcome under test (mirrors rowExecExitCodes' posture).
func guestSh(t *testing.T, m *MicroVMRuntime, id ContainerID, script string) (string, int) {
	t.Helper()
	out, err := m.Exec(t.Context(), id,
		NewExecSpec("sh", "-s").WithStdin(script).AsUser("1000"))
	if err != nil {
		t.Fatalf("guest exec failed at the transport/refusal layer (not the escape itself): %v", err)
	}
	return out.Stdout + out.Stderr, out.ExitCode
}

// TestMicroVMVolumeTraversalConfined is the path-traversal leg: the guest tries
// to escape /workspace three ways and is confined every time, proven from both
// sides of the boundary.
func TestMicroVMVolumeTraversalConfined(t *testing.T) {
	env := microvmtest.Require(t)
	m, id, volume := isolationSession(t, env, "iso-traversal")

	// The host tree OUTSIDE the volume: a sibling of the volume dir, so a
	// `/workspace/..` that actually escaped the share would land right in it.
	outside := filepath.Join(filepath.Dir(volume), "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("creating outside dir: %v", err)
	}
	canary := filepath.Join(outside, "canary.txt")
	if err := os.WriteFile(canary, []byte(outsideCanaryBody), 0o600); err != nil {
		t.Fatalf("planting host canary: %v", err)
	}

	// A symlink INSIDE the volume pointing at the outside host path. This is the
	// sharpest probe: virtio-fs passes the link through verbatim, so if the
	// guest could resolve it against the host's namespace the escape would
	// succeed. It cannot — the target is resolved inside the guest's own mount
	// namespace, where that path does not exist.
	if err := os.Symlink(canary, filepath.Join(volume, "escape-link")); err != nil {
		t.Fatalf("planting escape symlink: %v", err)
	}
	// A symlink to the outside DIRECTORY too, so a traversal through a link
	// (rather than a direct read of one) is covered.
	if err := os.Symlink(outside, filepath.Join(volume, "escape-dir")); err != nil {
		t.Fatalf("planting escape dir symlink: %v", err)
	}

	// Read attempts. Each prints nothing on success-of-confinement; the
	// assertion is that the canary body never appears in ANY of them.
	reads := map[string]string{
		"dot-dot traversal":       "cat /workspace/../canary.txt /workspace/../outside/canary.txt",
		"absolute host path":      "cat " + canary,
		"symlink to host file":    "cat /workspace/escape-link",
		"symlink to host dir":     "cat /workspace/escape-dir/canary.txt",
		"deep dot-dot to fs root": "cat /workspace/../../../../../.." + canary,
	}
	for name, script := range reads {
		t.Run("read: "+name, func(t *testing.T) {
			out, code := guestSh(t, m, id, script)
			if strings.Contains(out, outsideCanaryBody) {
				t.Fatalf("guest READ a host file outside its volume via %s — VOLUME ESCAPE.\noutput: %q", name, out)
			}
			if code == 0 {
				t.Errorf("escape attempt %q exited 0 (output %q); a confined read must fail", name, out)
			}
			t.Logf("confined: %s -> exit %d, %q", name, code, strings.TrimSpace(out))
		})
	}

	// Write attempts. The guest's own root filesystem is writable (an overlay),
	// so a `/workspace/../pwned` may well succeed INSIDE the guest — that proves
	// nothing either way. The real question is whether any byte landed on the
	// HOST outside the volume, which the snapshot comparison below answers.
	before := snapshotTree(t, outside)
	writes := []string{
		"echo pwned > /workspace/../pwned.txt",
		"echo pwned > /workspace/../outside/pwned.txt",
		"echo pwned > " + filepath.Join(outside, "pwned-abs.txt"),
		"echo pwned > /workspace/escape-dir/pwned-link.txt",
		"echo overwritten > /workspace/escape-link",
		"rm -f " + canary,
		"rm -rf " + outside,
	}
	for _, script := range writes {
		out, code := guestSh(t, m, id, script)
		t.Logf("write attempt %q -> exit %d, %q", script, code, strings.TrimSpace(out))
	}
	after := snapshotTree(t, outside)
	if before != after {
		t.Fatalf("the host tree OUTSIDE the session volume changed after guest write attempts — VOLUME ESCAPE.\nbefore: %s\nafter:  %s", before, after)
	}
	// And the canary's content specifically: an in-place overwrite through the
	// symlink would keep the tree shape identical while corrupting the file.
	body, err := os.ReadFile(canary)
	if err != nil {
		t.Fatalf("reading the host canary after the guest write attempts: %v", err)
	}
	if string(body) != outsideCanaryBody {
		t.Fatalf("the host canary was REWRITTEN by the guest (now %q) — VOLUME ESCAPE", string(body))
	}
	t.Logf("host tree outside the volume unchanged after %d write attempts: %s", len(writes), after)
}

// TestMicroVMCrossSessionVolumeUnreachable is the multi-tenant leg: two live
// sessions, and guest A cannot reach guest B's volume by any path, nor write
// into it. This is the assertion that makes "another tenant's volume is not
// merely unreadable but unnameable" (record §(d)) a tested property.
func TestMicroVMCrossSessionVolumeUnreachable(t *testing.T) {
	env := microvmtest.Require(t)
	const tenantBSecret = "TENANT-B-SECRET-4b91e7c2-must-never-be-readable-from-tenant-A"

	mA, idA, volumeA := isolationSession(t, env, "iso-tenant-a")
	mB, idB, volumeB := isolationSession(t, env, "iso-tenant-b")

	// B's secret, planted host-side inside B's volume, and also written by B's
	// OWN guest — so the test covers both a host-authored and a guest-authored
	// file (the latter is what a real tenant's workspace actually contains).
	if err := os.WriteFile(filepath.Join(volumeB, "host-secret.txt"), []byte(tenantBSecret), 0o600); err != nil {
		t.Fatalf("planting tenant B's host secret: %v", err)
	}
	if out, code := guestSh(t, mB, idB, "printf '%s' '"+tenantBSecret+"' > /workspace/guest-secret.txt"); code != 0 {
		t.Fatalf("tenant B could not write its own workspace file: exit %d, %q", code, out)
	}
	// A's own volume is populated and readable by A. Without this the
	// cross-tenant negatives below could pass because A's share is broken
	// rather than because B's is unreachable.
	if err := os.WriteFile(filepath.Join(volumeA, "a-own.txt"), []byte("tenant-a-own"), 0o600); err != nil {
		t.Fatalf("planting tenant A's own file: %v", err)
	}
	if out, code := guestSh(t, mA, idA, "cat /workspace/a-own.txt"); code != 0 || !strings.Contains(out, "tenant-a-own") {
		t.Fatalf("tenant A cannot read its OWN volume (exit %d, %q); the cross-tenant negatives would be vacuous", code, out)
	}
	// Sanity: B can read its own secret. Without this, the cross-tenant
	// negative below could pass vacuously (e.g. if the share were broken for
	// everyone).
	if out, code := guestSh(t, mB, idB, "cat /workspace/host-secret.txt /workspace/guest-secret.txt"); code != 0 || !strings.Contains(out, tenantBSecret) {
		t.Fatalf("tenant B cannot read its OWN volume (exit %d, %q); the cross-tenant negative would be vacuous", code, out)
	}

	// Every way A could try to name B's volume.
	attempts := map[string]string{
		"B's absolute host volume path": "cat " + filepath.Join(volumeB, "host-secret.txt") + " " + filepath.Join(volumeB, "guest-secret.txt"),
		"B's volume dir listing":        "ls -la " + volumeB,
		"traversal toward B":            "cat /workspace/../volume/host-secret.txt; cat /workspace/../../*/volume/*secret*",
		"a symlink A plants to B":       "ln -sf " + volumeB + " /workspace/b-link && cat /workspace/b-link/host-secret.txt",
		"the whole host tmp tree":       "grep -rl 'TENANT-B-SECRET' / 2>/dev/null | head -5",
	}
	for name, script := range attempts {
		t.Run("A cannot reach "+name, func(t *testing.T) {
			out, code := guestSh(t, mA, idA, script)
			if strings.Contains(out, tenantBSecret) {
				t.Fatalf("tenant A READ tenant B's secret via %s — CROSS-TENANT ESCAPE.\noutput: %q", name, out)
			}
			t.Logf("unreachable: %s -> exit %d, %q", name, code, strings.TrimSpace(truncate(out)))
		})
	}

	// A's writes must not land in B's volume. Snapshot B's host-side tree,
	// let A try, and compare.
	before := snapshotTree(t, volumeB)
	for _, script := range []string{
		"echo from-a > " + filepath.Join(volumeB, "pwned.txt"),
		"echo from-a > /workspace/b-link/pwned-link.txt",
		"rm -f " + filepath.Join(volumeB, "host-secret.txt"),
		"rm -rf " + volumeB,
	} {
		out, code := guestSh(t, mA, idA, script)
		t.Logf("A write-into-B attempt %q -> exit %d, %q", script, code, strings.TrimSpace(out))
	}
	if after := snapshotTree(t, volumeB); before != after {
		t.Fatalf("tenant B's host-side volume changed after tenant A's write attempts — CROSS-TENANT ESCAPE.\nbefore: %s\nafter:  %s", before, after)
	}
	// B must still be able to read its own secret: a "confinement" that worked
	// by breaking B's share would otherwise pass the comparison above.
	if out, code := guestSh(t, mB, idB, "cat /workspace/host-secret.txt"); code != 0 || !strings.Contains(out, tenantBSecret) {
		t.Fatalf("tenant B's own volume is broken after A's attempts (exit %d, %q)", code, out)
	}
	t.Logf("tenant B's volume intact and unreachable from tenant A: %s", before)
}

// TestMicroVMHostOwnershipParity is the ownership leg (record §(d)): a file the
// guest agent creates on the shared volume must land with the same host-side
// (uid,gid) the podman backend's `--userns=keep-id:uid=N,gid=N` produces — the
// INVOKING HOST user's ids, so files stay identical between backends and the
// invoking user still owns its own workspace tree.
//
// The gid axis is the discriminating one on a uid-1000 dev box: the in-guest
// agent runs as uid==gid==1000 (guestd linuxCredential), while the invoking host
// user's gid is typically NOT 1000 (here: 100/users). So an untranslated
// virtiofsd stamps gid 1000 where podman's keep-id would stamp the host gid —
// which is exactly what this test detects, and why launch.go carries virtiofsd's
// uid/gid translation.
func TestMicroVMHostOwnershipParity(t *testing.T) {
	env := microvmtest.Require(t)
	m, id, volume := isolationSession(t, env, "iso-parity")

	// The podman keep-id target: the invoking host user's own ids
	// (podman.go createArgs `--userns=keep-id:uid=%d,gid=%d` maps the invoking
	// host user onto the baked agent id, so the host-side owner is the invoker).
	wantUID, wantGID := os.Getuid(), os.Getgid()

	if out, code := guestSh(t, m, id, "umask 022; echo guest-authored > /workspace/from-guest.txt && mkdir -p /workspace/from-guest-dir && id -u && id -g"); code != 0 {
		t.Fatalf("guest could not write its own workspace: exit %d, %q", code, out)
	} else {
		t.Logf("in-guest identity for the write: %q", strings.TrimSpace(out))
	}

	for _, entry := range []string{"from-guest.txt", "from-guest-dir"} {
		path := filepath.Join(volume, entry)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("guest-authored %s is absent host-side: %v", entry, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("stat %s: unexpected Sys type %T", entry, info.Sys())
		}
		gotUID, gotGID := int(stat.Uid), int(stat.Gid)
		t.Logf("host-side ownership of guest-authored %s: uid=%d gid=%d (podman keep-id target: uid=%d gid=%d)",
			entry, gotUID, gotGID, wantUID, wantGID)
		if gotUID != wantUID || gotGID != wantGID {
			t.Errorf("host-ownership PARITY BROKEN for %s: guest-authored file is %d:%d, "+
				"but the podman --userns=keep-id path yields %d:%d — virtiofsd's uid/gid translation "+
				"(launch.go --translate-uid/--translate-gid) must map the in-guest agent id to the invoking host user",
				entry, gotUID, gotGID, wantUID, wantGID)
		}
	}

	// The other direction: a file the HOST creates must be owned by the agent
	// in-guest, or the agent cannot write its own workspace tree — the same
	// property keep-id gives the container path.
	hostAuthored := filepath.Join(volume, "from-host.txt")
	if err := os.WriteFile(hostAuthored, []byte("host-authored"), 0o600); err != nil {
		t.Fatalf("writing host-authored file: %v", err)
	}
	out, code := guestSh(t, m, id, "stat -c '%u %g' /workspace/from-host.txt && echo appended >> /workspace/from-host.txt")
	if code != 0 {
		t.Fatalf("the guest agent cannot read/append a host-authored workspace file (exit %d, %q); "+
			"the translation must leave the invoking user's files owned by the in-guest agent", code, out)
	}
	t.Logf("in-guest view of a host-authored file (uid gid): %q", strings.TrimSpace(out))
	if want := strings.Fields(strings.TrimSpace(out)); len(want) >= 2 {
		agentID := strconv.Itoa(int(agentuid.AgentUID))
		if want[0] != agentID || want[1] != agentID {
			t.Errorf("a host-authored workspace file appears in-guest as %s:%s, want the agent id %s:%s — "+
				"the agent would not own its own checkout", want[0], want[1], agentID, agentID)
		}
	}
	// The host-side owner of that file must be unchanged by the guest's append:
	// a translation that rewrote ownership on write would silently reassign the
	// invoking user's files.
	info, err := os.Stat(hostAuthored)
	if err != nil {
		t.Fatalf("re-stat host-authored file: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("re-stat: unexpected Sys type %T", info.Sys())
	}
	if int(stat.Uid) != wantUID || int(stat.Gid) != wantGID {
		t.Errorf("a host-authored file changed owner to %d:%d after a guest append, want %d:%d",
			stat.Uid, stat.Gid, wantUID, wantGID)
	}
}

// requireQuotaFS gates the real in-guest quota-enforcement leg on a capability
// this box structurally cannot have: a prjquota-ACTIVE filesystem, whose
// provisioning needs root (loop file + `mkfs.xfs`/`mkfs.ext4 -O quota` + mount
// -o prjquota + a project id + limits — every step CAP_SYS_ADMIN, D7 / Global
// Constraint "Rootless is hard"). It mirrors microvmtest.Require's shape: SKIP
// with the reason named when absent, and HARD FAIL when the operator asserts the
// capability is present via COMPASS_REQUIRE_QUOTA_FS=1, so a managed/CI profile
// that is supposed to have it cannot silently stop exercising it.
//
// $COMPASS_TEST_QUOTA_VOLUME names a directory on a project-quota'd filesystem.
// Returning it only after verifyVolumeQuota confirms an ACTIVE bound is the
// honesty gate: a path on an unquota'd filesystem skips (or hard-fails under the
// require flag) rather than running a test that can never observe EDQUOT and
// passing as if it proved quota.
func requireQuotaFS(t *testing.T) string {
	t.Helper()
	required := os.Getenv("COMPASS_REQUIRE_QUOTA_FS") != ""
	refuse := func(format string, args ...any) string {
		t.Helper()
		if required {
			t.Fatalf("COMPASS_REQUIRE_QUOTA_FS is set but "+format, args...)
		}
		t.Skipf("no project-quota'd filesystem available: "+format+
			" — provisioning one needs root (loop + mkfs -o prjquota + project id + limits), which the rootless "+
			"Runner and this suite do not have (D7). The verification LOGIC is proven hermetically in "+
			"microvm_quota_test.go; set $COMPASS_TEST_QUOTA_VOLUME to a quota'd dir to run this leg for real.", args...)
		return ""
	}

	volume := os.Getenv("COMPASS_TEST_QUOTA_VOLUME")
	if volume == "" {
		return refuse("$COMPASS_TEST_QUOTA_VOLUME is unset")
	}
	reading, err := verifyVolumeQuota(volume, VolumeQuota{}, readVolumeQuota)
	if err != nil {
		return refuse("%v", err)
	}
	t.Logf("quota'd volume %s: %s (used ratio %.4f)", volume, reading, reading.UsedRatio())
	return volume
}

// TestMicroVMVolumeQuotaEnforcedInGuest is the real resource-exhaustion leg
// (record §Plan V6: "writes past the byte bound and creates past the inode bound
// fail inside the guest with ENOSPC/EDQUOT while the host filesystem stays
// healthy"). It runs ONLY on a genuinely project-quota'd filesystem and skips
// with the reason named otherwise — it never fake-proves quota.
func TestMicroVMVolumeQuotaEnforcedInGuest(t *testing.T) {
	env := microvmtest.Require(t)
	quotaRoot := requireQuotaFS(t)

	volume, err := os.MkdirTemp(quotaRoot, "iso-quota-") //nolint:usetesting // the volume MUST live on the operator-provided quota'd filesystem, not the test's own TMPDIR — that is the whole capability under test
	if err != nil {
		t.Fatalf("creating a session volume on the quota'd filesystem: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(volume); err != nil {
			t.Errorf("removing quota'd volume %s: %v", volume, err)
		}
	})

	before, err := readVolumeQuota(volume)
	if err != nil {
		t.Fatalf("reading the quota before the fill: %v", err)
	}

	m := NewMicroVMRuntime(e2eConfig(t, env))
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   "iso-quota",
		UID:    agentuid.AgentUID,
		Mounts: []Mount{{HostPath: volume, ContainerPath: workspaceMountPath}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Remove(t.Context(), id); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write past the byte bound: dd until it fails. The guest MUST hit
	// ENOSPC/EDQUOT rather than consuming the whole host filesystem.
	fill := "dd if=/dev/zero of=/workspace/fill bs=1M count=" +
		strconv.FormatInt(before.LimitBytes/(1<<20)+64, 10) + " 2>&1"
	out, code := guestSh(t, m, id, fill)
	if code == 0 {
		t.Fatalf("the guest wrote past the project byte bound (%d B) without failing — the quota is not enforced.\noutput: %q",
			before.LimitBytes, out)
	}
	if !strings.Contains(out, "No space left") && !strings.Contains(out, "Disk quota exceeded") {
		t.Errorf("the over-bound write failed with %q, want an ENOSPC/EDQUOT diagnostic", strings.TrimSpace(out))
	}
	t.Logf("over-bound write confined: exit %d, %q", code, strings.TrimSpace(truncate(out)))

	// The HOST filesystem must stay healthy: the mount root's free space is
	// still ample, i.e. the guest exhausted its project, not the filesystem.
	after, err := readVolumeQuota(volume)
	if err != nil {
		t.Fatalf("reading the quota after the fill: %v", err)
	}
	if after.FilesystemBytes-after.UsedBytes <= 0 {
		t.Fatalf("the host filesystem is exhausted after the guest fill (%s); the quota failed to contain it", after)
	}
	t.Logf("host filesystem healthy after the guest fill: %s (used ratio %.4f)", after, after.UsedRatio())
}

// snapshotTree renders a stable, comparable description of every entry under
// root: relative path, mode, size, and host uid/gid. It is the host-side half of
// each escape assertion — an unchanged snapshot after a batch of guest write
// attempts is what "confined" means, in a form a diff can show.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("stat %s: unexpected Sys type %T", path, info.Sys())
		}
		size := info.Size()
		if entry.IsDir() {
			// A directory's size is filesystem bookkeeping that shifts as
			// entries are added and removed and then re-added, so it is not a
			// stable identity — the entry SET (rendered by the walk itself) is.
			size = -1
		}
		fmt.Fprintf(&b, "%s mode=%s size=%d owner=%d:%d\n", rel, info.Mode(), size, stat.Uid, stat.Gid)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return b.String()
}

// truncate bounds a guest output for a log line, so a `grep -r /` that returned
// a lot does not flood the test log.
func truncate(out string) string {
	const max = 512
	if len(out) <= max {
		return out
	}
	return out[:max] + "…(truncated)"
}
