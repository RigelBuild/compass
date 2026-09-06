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

	root, err := mountRoot(volume)
	if err == nil {
		t.Fatalf("mountRoot(%q) = %q with no error; a stat-blocked ancestor must be an INCONCLUSIVE probe, "+
			"not a mount root — comparing against a possibly-projected ancestor can report a bogus active quota", volume, root)
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
// a path the Runner can traverse to its mount point, mountRoot still resolves a
// real reference. Without it the fail-closed assertion above could pass on a
// mountRoot that had simply stopped working.
//
// The ancestor-walk's own error branch is not reachable through file modes: an
// EACCES that blocks stat(parent) necessarily blocks resolving the leaf through
// that same parent, so EvalSymlinks refuses first (the case above). The branch
// stays because a non-permission stat failure — an ancestor unlinked mid-walk,
// an EIO — must fail closed rather than return a same-device guess.
func TestMountRootResolvesAnUnblockedPath(t *testing.T) {
	root, err := mountRoot(t.TempDir())
	if err != nil {
		t.Fatalf("mountRoot on an unblocked temp dir = %v, want a resolved mount root", err)
	}
	if root == "" {
		t.Fatal("mountRoot resolved an empty mount root on an unblocked path")
	}
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
