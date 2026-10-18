//go:build unix && !linux && !darwin

package adapters

import (
	"fmt"
	"runtime"
)

// readGroupLeaderStartTime refuses unsupported Unix systems; the //go:build unix parser requires this symbol.
// Spawn refuses too, so no record with an unreadable identity token can be written.
func readGroupLeaderStartTime(pgid int) (uint64, error) {
	return 0, fmt.Errorf(
		"reading the start-time identity token for process group %d is not implemented on %s "+
			"(the supervised stack runs on linux and darwin)", pgid, runtime.GOOS)
}
