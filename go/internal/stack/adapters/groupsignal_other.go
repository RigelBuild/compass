//go:build unix && !linux && !darwin

package adapters

import (
	"fmt"
	"runtime"
)

// readGroupLeaderStartTime refuses on a unix that is neither linux nor darwin,
// for the same reason as the spawn-side reader (stack.readProcessStartTime):
// groupsignal.go is //go:build unix, so this symbol must exist under every
// build constraint the package accepts.
//
// Refusing is fail-closed here too: the spawn side refuses as well, so no
// record carrying a token for this host is ever written.
func readGroupLeaderStartTime(pgid int) (uint64, error) {
	return 0, fmt.Errorf(
		"reading the start-time identity token for process group %d is not implemented on %s "+
			"(the supervised stack runs on linux and darwin)", pgid, runtime.GOOS)
}
