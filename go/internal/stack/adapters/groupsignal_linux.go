//go:build linux

package adapters

import (
	"fmt"
	"os"
)

// readGroupLeaderStartTime reads /proc/<pgid>/stat; a missing process yields an error rather than an identity token.
// The pure parenthesized-comm parser is in parseGroupLeaderStat for cross-Unix tests.
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
