//go:build linux

package stack

import (
	"fmt"
	"os"
	"strings"
)

// readCurrentBootID reads the kernel's per-boot UUID, which changes on every boot.
func readCurrentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
