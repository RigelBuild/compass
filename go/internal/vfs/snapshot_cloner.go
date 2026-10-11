package vfs

// snapshot_cloner.go confines snapshot reads and writes to opened roots.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

type cloner interface {
	cloneTree(ctx context.Context, src, dst string) error
	name() string
}

type copyCloner struct{}
type reflinkCloner struct{}

type snapshotDir struct {
	path    string
	mode    fs.FileMode
	modTime time.Time
}

func (copyCloner) name() string    { return "copy" }
func (reflinkCloner) name() string { return "reflink" }

func (copyCloner) cloneTree(ctx context.Context, src, dst string) error {
	return cloneTree(ctx, src, dst, false)
}

func (reflinkCloner) cloneTree(ctx context.Context, src, dst string) error {
	return cloneTree(ctx, src, dst, true)
}

func cloneTree(ctx context.Context, src, dst string, reflink bool) error {
	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		return fmt.Errorf("vfs: opening snapshot source root %q: %w", src, err)
	}
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		return errors.Join(fmt.Errorf("vfs: opening snapshot destination root %q: %w", dst, err), srcRoot.Close())
	}
	cloneErr := cloneTreeRoots(ctx, srcRoot, dstRoot, reflink)
	return errors.Join(cloneErr, srcRoot.Close(), dstRoot.Close())
}

func cloneTreeRoots(ctx context.Context, srcRoot, dstRoot *os.Root, reflink bool) error {
	var dirs []snapshotDir
	err := fs.WalkDir(srcRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("vfs: walking snapshot source %q: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := filepath.FromSlash(path)
		info, err := srcRoot.Lstat(rel)
		if err != nil {
			return fmt.Errorf("vfs: inspecting snapshot entry %q: %w", path, err)
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if path != "." {
				if err := dstRoot.Mkdir(rel, mode.Perm()|0o700); err != nil {
					return fmt.Errorf("vfs: creating snapshot directory %q: %w", path, err)
				}
			}
			dirs = append(dirs, snapshotDir{path: rel, mode: mode.Perm(), modTime: info.ModTime()})
		case mode.IsRegular():
			if err := cloneRegular(ctx, srcRoot, dstRoot, rel, mode.Perm(), info.ModTime(), reflink); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			link, err := srcRoot.Readlink(rel)
			if err != nil {
				return fmt.Errorf("vfs: reading snapshot symlink %q: %w", path, err)
			}
			if err := dstRoot.Symlink(link, rel); err != nil {
				return fmt.Errorf("vfs: creating snapshot symlink %q: %w", path, err)
			}
			if err := setSymlinkTimes(dstRoot, rel, info.ModTime()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("vfs: unsupported snapshot entry type at %q", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i].path) > len(dirs[j].path)
	})
	for _, dir := range dirs {
		if err := dstRoot.Chmod(dir.path, dir.mode.Perm()); err != nil {
			return fmt.Errorf("vfs: preserving snapshot directory mode %q: %w", dir.path, err)
		}
		if err := dstRoot.Chtimes(dir.path, dir.modTime, dir.modTime); err != nil {
			return fmt.Errorf("vfs: preserving snapshot directory time %q: %w", dir.path, err)
		}
	}
	return nil
}

func cloneRegular(ctx context.Context, srcRoot, dstRoot *os.Root, path string, mode fs.FileMode, modTime time.Time, reflink bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := srcRoot.Open(path)
	if err != nil {
		return fmt.Errorf("vfs: opening snapshot source %q: %w", path, err)
	}
	dst, err := dstRoot.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return errors.Join(fmt.Errorf("vfs: creating snapshot file %q: %w", path, err), src.Close())
	}
	var cloneErr error
	if reflink {
		cloneErr = reflinkFile(dst, src)
	} else {
		_, cloneErr = io.Copy(dst, src)
	}
	if cloneErr != nil {
		cloneErr = fmt.Errorf("vfs: cloning snapshot file %q: %w", path, cloneErr)
	}
	if cloneErr == nil {
		cloneErr = dst.Chmod(mode)
		if cloneErr != nil {
			cloneErr = fmt.Errorf("vfs: setting snapshot file mode %q: %w", path, cloneErr)
		}
	}
	if cloneErr == nil {
		cloneErr = dstRoot.Chtimes(path, modTime, modTime)
		if cloneErr != nil {
			cloneErr = fmt.Errorf("vfs: preserving snapshot file time %q: %w", path, cloneErr)
		}
	}
	return errors.Join(cloneErr, src.Close(), dst.Close())
}

func setSymlinkTimes(root *os.Root, path string, modTime time.Time) (resultErr error) {
	parent, err := root.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("vfs: opening snapshot symlink parent %q: %w", path, err)
	}
	dir, err := parent.Open(".")
	if err != nil {
		return errors.Join(fmt.Errorf("vfs: opening snapshot symlink directory %q: %w", path, err), parent.Close())
	}
	defer func() {
		resultErr = errors.Join(resultErr, dir.Close(), parent.Close())
	}()
	// os.Root has no no-follow Chtimes; use its opened parent FD and basename.
	times := []unix.Timespec{unix.NsecToTimespec(modTime.UnixNano()), unix.NsecToTimespec(modTime.UnixNano())}
	if err := unix.UtimesNanoAt(int(dir.Fd()), filepath.Base(path), times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("vfs: preserving snapshot symlink time %q: %w", path, err)
	}
	return nil
}

func probeCloner(storeDir string) (selected cloner, resultErr error) {
	staging := filepath.Join(storeDir, "staging")
	for _, dir := range []string{staging, filepath.Join(storeDir, "trees"), filepath.Join(storeDir, "index")} {
		if err := os.MkdirAll(dir, volumeDirMode); err != nil { //nolint:gosec // store subdirectories are fixed names below the operator-configured store root
			return nil, fmt.Errorf("vfs: creating snapshot store directory %q: %w", dir, err)
		}
	}
	stagingRoot, err := os.OpenRoot(staging)
	if err != nil {
		return nil, fmt.Errorf("vfs: opening snapshot staging root: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, stagingRoot.Close())
	}()
	src, err := os.CreateTemp(staging, "probe-source-")
	if err != nil {
		return nil, fmt.Errorf("vfs: creating reflink probe source: %w", err)
	}
	srcName := filepath.Base(src.Name())
	if _, err := src.WriteString("reflink probe"); err != nil {
		return nil, errors.Join(fmt.Errorf("vfs: writing reflink probe source: %w", err), src.Close(), removeProbe(stagingRoot, srcName))
	}
	if err := src.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("vfs: closing reflink probe source: %w", err), removeProbe(stagingRoot, srcName))
	}
	dst, err := os.CreateTemp(staging, "probe-destination-")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("vfs: creating reflink probe destination: %w", err), removeProbe(stagingRoot, srcName))
	}
	dstName := filepath.Base(dst.Name())
	srcFile, err := stagingRoot.Open(srcName)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("vfs: opening reflink probe source: %w", err), dst.Close(), removeProbe(stagingRoot, srcName), removeProbe(stagingRoot, dstName))
	}
	cloneErr := reflinkFile(dst, srcFile)
	closeErr := errors.Join(srcFile.Close(), dst.Close())
	cleanupErr := errors.Join(removeProbe(stagingRoot, srcName), removeProbe(stagingRoot, dstName))
	if cleanupErr != nil {
		return nil, errors.Join(fmt.Errorf("vfs: cleaning reflink probe: %w", cleanupErr), closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("vfs: closing reflink probe files: %w", closeErr)
	}
	if cloneErr == nil {
		return reflinkCloner{}, nil
	}
	return copyCloner{}, nil
}

func removeProbe(root *os.Root, path string) error {
	if err := root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("vfs: removing probe file %q: %w", path, err)
	}
	return nil
}
