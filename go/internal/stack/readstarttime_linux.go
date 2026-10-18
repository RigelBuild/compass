//go:build linux

package stack

import (
	"fmt"
	"os"
)

// readProcessStartTime reads field 22 from /proc/<pid>/stat and fails closed when the pid is absent.
// parseStatStartTime finds the last ')' because comm can contain spaces and parentheses, then counts post-comm fields.
func readProcessStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	startTime, err := parseStatStartTime(string(data))
	if err != nil {
		return 0, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	}
	return startTime, nil
}
