//go:build !linux

package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("vfs: walking tree for sync %q: %w", path, walkErr)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("vfs: inspecting tree entry for sync %q: %w", path, err)
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path) //nolint:gosec // path is discovered under the internally-owned tree root.
			if err != nil {
				return fmt.Errorf("vfs: opening tree file %q for sync: %w", path, err)
			}
			syncErr := f.Sync()
			if syncErr != nil {
				syncErr = fmt.Errorf("vfs: syncing tree file %q: %w", path, syncErr)
			}
			if err := errors.Join(syncErr, f.Close()); err != nil {
				return err
			}
		}
		if info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		f, err := os.Open(dirs[i]) //nolint:gosec // path is discovered under the internally-owned tree root.
		if err != nil {
			return fmt.Errorf("vfs: opening tree directory %q for sync: %w", dirs[i], err)
		}
		syncErr := f.Sync()
		if syncErr != nil {
			syncErr = fmt.Errorf("vfs: syncing tree directory %q: %w", dirs[i], syncErr)
		}
		if err := errors.Join(syncErr, f.Close()); err != nil {
			return err
		}
	}
	return nil
}
