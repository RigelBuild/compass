//go:build darwin

package stack

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readCurrentBootID renders kern.boottime, which is fixed for the life of one boot.
func readCurrentBootID() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", fmt.Errorf("sysctl kern.boottime: %w", err)
	}
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}
