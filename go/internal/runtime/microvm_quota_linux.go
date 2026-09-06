//go:build linux

package runtime

// The syscall half of V6's quota verification: the rootless statfs(2) probe
// microvm_quota.go's pure decision consumes. Linux-only because the
// project-quota-projected statvfs behavior it reads is a Linux XFS/ext4 kernel
// property (xfs_qm_statvfs / ext4_statfs_project); microvm_quota_unsupported.go
// carries the named refusal for every other unix.

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// readVolumeQuota is the production quotaReadFn: it statfs(2)es the volume path
// and, separately, the mount root the path resolves into, so the pure decision
// can compare the two. On a project-quota'd subtree the kernel rewrites the
// path's block/inode totals to the project's limit and usage while the mount
// root's stay the filesystem's real size — so the divergence IS the proof an
// enforced quota is active, readable with no capability (see microvm_quota.go's
// header for why quotactl and FS_IOC_FSGETXATTR are both the wrong read).
func readVolumeQuota(path string) (QuotaReading, error) {
	var at syscall.Statfs_t
	if err := syscall.Statfs(path, &at); err != nil {
		return QuotaReading{}, fmt.Errorf("statfs %q: %w", path, err)
	}
	root, err := mountRoot(path)
	if err != nil {
		return QuotaReading{}, err
	}
	var atRoot syscall.Statfs_t
	if err := syscall.Statfs(root, &atRoot); err != nil {
		return QuotaReading{}, fmt.Errorf("statfs mount root %q: %w", root, err)
	}
	return QuotaReading{
		Path:             path,
		MountRoot:        root,
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

// mountRoot walks path's ancestors until the device number changes, returning
// the deepest ancestor still on the same filesystem — the mount point path
// belongs to. A project quota does NOT change st_dev (it is an accounting scope
// inside one filesystem, not a separate device), so this reliably reaches the
// unprojected reference point the comparison needs.
//
// Paths are resolved through symlinks first: a symlinked volume dir would
// otherwise walk the link's lexical parents, which may live on a different
// filesystem entirely and make the comparison meaningless.
func mountRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	dev, err := deviceOf(resolved)
	if err != nil {
		return "", err
	}
	current := resolved
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached "/" — the filesystem root is the mount root.
			return current, nil
		}
		parentDev, err := deviceOf(parent)
		if err != nil {
			// An unreadable ancestor (a 0711 parent an unprivileged Runner
			// cannot stat) is not a quota verdict: the deepest ancestor proven
			// to be on this device is the best reference available, and the
			// comparison against it stays sound.
			return current, nil //nolint:nilerr // a stat-blocked ancestor bounds the walk; `current` is still a same-device reference, so this is the answer, not a swallowed failure
		}
		if parentDev != dev {
			return current, nil
		}
		current = parent
	}
}

// deviceOf returns the st_dev of path, the identity the mount-root walk compares.
// syscall.Stat (not os.Stat) because st_dev is all this needs: it fills a
// caller-owned struct with no os.FileInfo allocation and no Sys() type
// assertion, and it is the same call the readVolumeQuota statfs pair already
// uses, keeping this file on one syscall surface.
func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	return st.Dev, nil
}
