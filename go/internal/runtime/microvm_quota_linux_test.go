//go:build linux

package runtime

// Tests for the Linux-only statfs/mount-root half of V6's quota verification
// (microvm_quota_linux.go). Mechanism and the rejected alternatives:
// microvm_quota.go's header.
//
// These are separate from microvm_quota_test.go because mountRoot and deviceOf
// only exist under //go:build linux — the pure decision they feed is covered
// there, on every GOOS.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMountRootUnreadableAncestorIsInconclusive pins the fail-CLOSED semantics
// for a stat-blocked ancestor: the walk cannot reach an UNPROJECTED reference,
// and same-device is not the same as unprojected — FS_XFLAG_PROJINHERIT means a
// halted walk may stop inside the same project (or a larger enclosing one),
// whose totals are themselves rewritten. Comparing against that would report a
// bogus ACTIVE quota, the fail-open direction. So mountRoot returns an error
// naming the blocking path instead of a mount root, and no verdict is rendered.
//
// A 0000 ancestor is refused at path resolution rather than at the ancestor
// walk, since an EACCES that blocks stat(parent) also blocks resolving the leaf
// through it — both routes are errors, which is the point: the ONE thing that
// must never happen is a QuotaReading built against an unproven reference.
func TestMountRootUnreadableAncestorIsInconclusive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: a 0000 dir is still traversable, so the EACCES this test needs cannot be produced")
	}
	base := t.TempDir()
	blocked := filepath.Join(base, "blocked")
	volume := filepath.Join(blocked, "volume")
	if err := os.MkdirAll(volume, 0o700); err != nil {
		t.Fatalf("creating volume tree: %v", err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("blocking ancestor: %v", err)
	}
	// Restore the mode so t.TempDir's cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	root, distinct, err := mountRoot(volume)
	if err == nil {
		t.Fatalf("mountRoot(%q) = %q (distinct=%v) with no error; a stat-blocked ancestor must be an INCONCLUSIVE probe, "+
			"not a mount root — comparing against a possibly-projected ancestor can report a bogus active quota", volume, root, distinct)
	}
	if root != "" {
		t.Errorf("mountRoot returned the reference %q alongside its error; an inconclusive probe must yield no reference", root)
	}
	// The error must name the path an operator has to fix.
	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error %q does not name the blocking path %q", err.Error(), blocked)
	}
}

// TestMountRootResolvesAnUnblockedPath is the positive control for the walk: on
// a path the Runner can traverse to its mount point, mountRoot resolves a real
// reference AND reports that it crossed a device boundary to get there.
//
// It asserts the walk did real WORK, not merely that it returned something: the
// root must be a strict prefix ANCESTOR of the input, and a nested subdirectory
// must resolve to the SAME root. Both fail on an identity-returning mountRoot —
// which is exactly what the real one does for a path that IS a mount point, the
// degeneracy the old non-empty-and-no-error assertion could not catch.
func TestMountRootResolvesAnUnblockedPath(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("creating the nested tree: %v", err)
	}
	// t.TempDir resolves through symlinks the same way mountRoot does, so the
	// prefix comparison below is against the resolved form on a box where
	// $TMPDIR is a symlink.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}

	root, distinct, err := mountRoot(base)
	if err != nil {
		t.Fatalf("mountRoot on an unblocked temp dir = %v, want a resolved mount root", err)
	}
	if !distinct {
		t.Fatalf("mountRoot(%q) reported distinct=false; a temp dir is never its own mount point, so the "+
			"device-boundary walk did not run", base)
	}
	// STRICT ancestor: an identity-returning implementation returns the input
	// itself, which this rejects.
	if root == resolvedBase {
		t.Fatalf("mountRoot(%q) = %q — the input itself. The walk resolved no unprojected reference; an "+
			"identity implementation would pass a mere non-empty check", base, root)
	}
	if !strings.HasPrefix(resolvedBase, strings.TrimSuffix(root, "/")+"/") {
		t.Fatalf("mountRoot(%q) = %q, which is not a prefix ancestor of the input", base, root)
	}
	// A NESTED path must land on the SAME mount root: the walk climbs to a
	// device boundary, not to some depth-relative ancestor.
	nestedRoot, nestedDistinct, err := mountRoot(nested)
	if err != nil {
		t.Fatalf("mountRoot(%q) = %v, want the same mount root as its ancestor", nested, err)
	}
	if !nestedDistinct {
		t.Errorf("mountRoot(%q) reported distinct=false for a deeply nested path", nested)
	}
	if nestedRoot != root {
		t.Fatalf("mountRoot(%q) = %q but mountRoot(%q) = %q; a device-boundary walk must reach the same "+
			"mount root from both (an identity implementation returns each input instead)", nested, nestedRoot, base, root)
	}
	t.Logf("mount-root walk: %q and %q both resolve to %q", base, nested, root)
}

// TestMountRootRecognizesASelfReferentialPath is MED-3's core: a path that IS
// its own mount point must be reported as NON-distinct, because there is then no
// unprojected reference to compare statfs totals against.
//
// /tmp is a real mount on this box (a separate st_dev from /), so mountRoot
// returns /tmp itself — verified by instrumented run. Accepting that as a
// reference makes LimitBytes == FilesystemBytes identically, Active() false BY
// CONSTRUCTION, and a QuotaRequired startup a refusal on a host whose volume
// root is (as production layouts naturally do) the mount point of a dedicated
// quota'd filesystem.
func TestMountRootRecognizesASelfReferentialPath(t *testing.T) {
	const mountPoint = "/tmp"
	if _, err := os.Stat(mountPoint); err != nil {
		t.Skipf("%s is not present: %v", mountPoint, err)
	}
	resolved, err := filepath.EvalSymlinks(mountPoint)
	if err != nil {
		t.Skipf("resolving %s: %v", mountPoint, err)
	}

	root, distinct, err := mountRoot(mountPoint)
	if err != nil {
		t.Fatalf("mountRoot(%q) = %v", mountPoint, err)
	}
	if root != resolved {
		t.Skipf("%s is not its own mount point on this box (mountRoot = %q); the self-referential case "+
			"cannot be exercised here", mountPoint, root)
	}
	if distinct {
		t.Fatalf("mountRoot(%q) = %q with distinct=true, but the resolved path IS the returned root: no "+
			"device boundary was crossed, so there is NO unprojected reference and the comparison would be "+
			"self-referential (LimitBytes == FilesystemBytes identically, Active() false by construction)",
			mountPoint, root)
	}
}

// TestReadVolumeQuotaRefusesASelfReferentialVolumeRoot is MED-3 one layer up:
// the self-referential case must propagate as a DISTINCT inconclusive error
// naming the volume-root knob and the subdirectory fix, not collapse into the
// generic "no enforced project quota" verdict — which would refuse startup on a
// correctly provisioned host and send the operator chasing a quota that is
// already there.
func TestReadVolumeQuotaRefusesASelfReferentialVolumeRoot(t *testing.T) {
	const mountPoint = "/tmp"
	root, distinct, err := mountRoot(mountPoint)
	if err != nil {
		t.Skipf("mountRoot(%q) = %v", mountPoint, err)
	}
	if distinct {
		t.Skipf("%s is not its own mount point on this box (root %q); the self-referential case cannot be "+
			"exercised here", mountPoint, root)
	}

	reading, readErr := readVolumeQuota(mountPoint)
	if readErr == nil {
		t.Fatalf("readVolumeQuota(%q) = %s with no error; a volume root that IS its own mount point has no "+
			"unprojected reference, so it must be INCONCLUSIVE rather than a verdict", mountPoint, reading)
	}
	for _, part := range []string{
		mountPoint, "IS the mount point", "INDETERMINATE", "--microvm-volume-root", "SUBDIRECTORY",
	} {
		if !strings.Contains(readErr.Error(), part) {
			t.Errorf("error %q does not name %q", readErr.Error(), part)
		}
	}
	t.Logf("self-referential volume root refused: %v", readErr)
}

// TestReadVolumeQuotaPropagatesInconclusiveMountRoot is the same posture one
// layer up: readVolumeQuota must NOT swallow the inconclusive mount-root probe
// into a reading, because a reading is what Active() renders a verdict from. An
// error here is what makes a QuotaRequired startup fail closed.
func TestReadVolumeQuotaPropagatesInconclusiveMountRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: a 0000 dir is still traversable, so the EACCES this test needs cannot be produced")
	}
	base := t.TempDir()
	blocked := filepath.Join(base, "blocked")
	volume := filepath.Join(blocked, "volume")
	if err := os.MkdirAll(volume, 0o700); err != nil {
		t.Fatalf("creating volume tree: %v", err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("blocking ancestor: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	reading, err := readVolumeQuota(volume)
	if err == nil {
		t.Fatalf("readVolumeQuota(%q) = %s with no error; an unreachable mount root must propagate so a "+
			"required-quota startup fails closed with the real cause", volume, reading)
	}
}
