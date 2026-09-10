//go:build linux

package stack

import (
	"fmt"
	"os"
)

// readProcessStartTime reads field 22 (starttime, in clock ticks since boot) of
// /proc/<pid>/stat — the Linux half of the readStartTime seam.
//
// The parse gotcha: field 2 (comm) is the executable name wrapped in
// parentheses and MAY itself contain spaces AND parentheses (e.g. a process
// named "(ec) foo"), so splitting the whole line on whitespace miscounts. The
// robust parse the kernel documents (proc(5)) is to find the LAST ')' — comm is
// the only parenthesized field and everything after it is space-separated
// fixed-position fields — then count fields from there. After the last ')':
// field[0] is state (field 3), so starttime (field 22) is field[22-3] = index
// 19 of the post-comm split. That rule lives in parseStatStartTime (pgidfile.go)
// so it is unit-testable without a live process.
//
// A pid that does not exist has no /proc entry, so the read fails and the
// identity check fails closed — it never matches a recorded token.
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
