//go:build linux

package adapters

import (
	"fmt"
	"os"
)

// readGroupLeaderStartTime reads field 22 (starttime, in clock ticks since
// boot) of /proc/<pgid>/stat — the Linux half of the teardown-side identity
// read. The parenthesized-comm parse rule lives in parseGroupLeaderStat
// (groupsignal.go) so it is unit-testable without a live process.
//
// A pgid that names no process has no /proc entry, so the read fails rather
// than returning a token that could match a recorded identity.
func readGroupLeaderStartTime(pgid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pgid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/stat: %w", pgid, err)
	}
	startTime, err := parseGroupLeaderStat(string(data))
	if err != nil {
		return 0, fmt.Errorf("/proc/%d/stat: %w", pgid, err)
	}
	return startTime, nil
}
