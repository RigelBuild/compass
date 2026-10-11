//go:build linux

package vfs

import (
	"os"

	"golang.org/x/sys/unix"
)

func reflinkFile(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
