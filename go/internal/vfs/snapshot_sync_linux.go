//go:build linux

package vfs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func syncTree(root string) error {
	f, err := os.Open(root) //nolint:gosec // root is an internal snapshot or volume directory.
	if err != nil {
		return fmt.Errorf("vfs: opening tree %q for syncfs: %w", root, err)
	}
	syncErr := unix.Syncfs(int(f.Fd()))
	if syncErr != nil {
		syncErr = fmt.Errorf("vfs: syncing filesystem for tree %q: %w", root, syncErr)
	}
	return errors.Join(syncErr, f.Close())
}
