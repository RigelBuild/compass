//go:build darwin

package stack

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readCurrentBootID reads kern.bootsessionuuid, a read-only UUID minted per boot.
// Unlike kern.boottime it does not move when the wall clock steps.
func readCurrentBootID() (string, error) {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", fmt.Errorf("sysctl kern.bootsessionuuid: %w", err)
	}
	return id, nil
}
