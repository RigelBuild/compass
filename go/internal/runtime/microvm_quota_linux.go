//go:build linux

package runtime

// The syscall half of V6's quota verification: the rootless statfs(2) probe
// microvm_quota.go's pure decision consumes. Mechanism and the rejected
// alternatives (quotactl, FS_IOC_FSGETXATTR): microvm_quota.go's header.

// Linux-only because the project-quota-projected statvfs behavior it reads is a
// Linux XFS/ext4 kernel property (xfs_qm_statvfs / ext4_statfs_project);
// microvm_quota_unsupported.go carries the named refusal for every other unix.

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// readVolumeQuota compares statfs totals for a path and its mount root.
// A project-quota projection rewrites the path totals but not the mount-root totals.
func readVolumeQuota(path string) (QuotaReading, error) {
	var at syscall.Statfs_t
	if err := syscall.Statfs(path, &at); err != nil {
		return QuotaReading{}, fmt.Errorf("statfs %q: %w", path, err)
	}
	root, distinct, err := mountRoot(path)
	if err != nil {
		return QuotaReading{}, err
	}
	if !distinct {
		// The volume root IS the mount point, so there is no UNPROJECTED
		// reference: both statfs calls target the same path and Active() is false
		// BY CONSTRUCTION, not by observation. Reporting "no quota" would refuse a
		// QuotaRequired startup, so this is an INCONCLUSIVE probe with the fix named.
		return QuotaReading{}, fmt.Errorf(
			"locating an unprojected reference for %q: that path IS the mount point of its filesystem (%q), "+
				"so there is no unquota'd ancestor to compare its statfs totals against and whether a project "+
				"quota scopes it is INDETERMINATE; point --microvm-volume-root (or "+
				"$COMPASS_MICROVM_VOLUME_ROOT) at a SUBDIRECTORY of the quota'd filesystem rather than at its "+
				"mount point — that subdirectory is also where per-session volumes are actually minted, and it "+
				"is the dir the project id and FS_XFLAG_PROJINHERIT belong on",
			path, root)
	}
	var atRoot syscall.Statfs_t
	if err := syscall.Statfs(root, &atRoot); err != nil {
		return QuotaReading{}, fmt.Errorf("statfs mount root %q: %w", root, err)
	}
	return QuotaReading{
		Path:      path,
		MountRoot: root,
		// Active distinguishes projected quota totals from whole-filesystem totals.
		LimitBytes:       blocksToBytes(at.Blocks, at.Bsize),
		UsedBytes:        blocksToBytes(at.Blocks-at.Bfree, at.Bsize),
		LimitInodes:      int64(at.Files), //nolint:gosec // G115: a statfs inode count is a kernel-reported magnitude, never near the int64 ceiling
		UsedInodes:       int64(at.Files - at.Ffree),
		FilesystemBytes:  blocksToBytes(atRoot.Blocks, atRoot.Bsize),
		FilesystemInodes: int64(atRoot.Files), //nolint:gosec // G115: as above — a kernel-reported inode count
	}, nil
}

// blocksToBytes converts a statfs block count at the reported fragment size to
// bytes. A non-positive Bsize (never produced by a healthy filesystem, but the
// field is signed) yields 0 rather than a nonsense product, so the decision
// reads it as "no limit observed" and fails a required check closed.
func blocksToBytes(blocks uint64, bsize int64) int64 {
	if bsize <= 0 {
		return 0
	}
	//nolint:gosec // G115: block counts × fragment size are filesystem-sized magnitudes; a real statfs cannot overflow int64 here
	return int64(blocks) * bsize
}

// mountRoot resolves symlinks, then walks to the containing mount by device number.
// Project quotas do not change st_dev, so an unprojected mount root is the reference.
// If the path is its own mount or an ancestor is unreadable, the probe is inconclusive;
// treating rewritten ancestor totals as a reference could falsely claim enforcement.
func mountRoot(path string) (root string, distinct bool, err error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, fmt.Errorf("resolving %q: %w", path, err)
	}
	dev, err := deviceOf(resolved)
	if err != nil {
		return "", false, err
	}
	current := resolved
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached "/" — the filesystem root is the mount root. It is a
			// distinct reference only if the walk moved off the given path.
			return current, current != resolved, nil
		}
		parentDev, err := deviceOf(parent)
		if err != nil {
			return "", false, fmt.Errorf(
				"locating the mount root of %q: ancestor %q is not statable (%w), so no unprojected "+
					"reference point could be reached and whether a project quota scopes the volume is "+
					"INDETERMINATE; make the ancestor path traversable by the Runner uid (chmod o+x) "+
					"or place the volume root on a path the Runner can walk to its mount point",
				path, parent, err)
		}
		if parentDev != dev {
			return current, current != resolved, nil
		}
		current = parent
	}
}

// deviceOf returns st_dev using syscall.Stat without allocating os.FileInfo.
func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return st.Dev, nil
}
