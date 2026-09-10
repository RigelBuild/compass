//go:build unix && !linux && !darwin

package stack

import (
	"fmt"
	"runtime"
)

// readProcessStartTime refuses on a unix that is neither linux nor darwin.
//
// The seam must RESOLVE on every OS this package is built for: pgidfile.go is
// //go:build unix, so readProcessStartTime has to exist under every build
// constraint the package accepts or the package does not compile there at all.
// The BSDs and solaris satisfy `unix`, and each reads process start time its
// own way, so there is no reader here to share.
//
// A named refusal, not a build break: the supervised stack ships on linux and
// darwin, and a compile failure on an OS nobody targets is a worse outcome than
// a legible runtime error for anyone who tries. The refusal is also the
// fail-closed answer — an identity token that cannot be read must never come
// back as a value that might compare equal.
func readProcessStartTime(pid int) (uint64, error) {
	return 0, fmt.Errorf(
		"reading the start-time identity token for pid %d is not implemented on %s "+
			"(the supervised stack runs on linux and darwin)", pid, runtime.GOOS)
}
