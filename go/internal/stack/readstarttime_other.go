//go:build unix && !linux && !darwin

package stack

import (
	"fmt"
	"runtime"
)

// readProcessStartTime refuses unsupported Unix systems; this symbol must exist under every unix build constraint.
// A runtime refusal is fail-closed and clearer than a package build failure.
func readProcessStartTime(pid int) (uint64, error) {
	return 0, fmt.Errorf(
		"reading the start-time identity token for pid %d is not implemented on %s "+
			"(the supervised stack runs on linux and darwin)", pid, runtime.GOOS)
}
