//go:build !linux

package vfs

import (
	"errors"
	"fmt"
	"os"
)

func reflinkFile(_, _ *os.File) error {
	return fmt.Errorf("vfs: reflink unavailable: %w", errors.ErrUnsupported)
}
