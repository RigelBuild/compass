//go:build darwin

package stack

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readProcessStartTime reads the Darwin creation timeval from sysctl; missing pids and zero timevals fail closed.
func readProcessStartTime(pid int) (uint64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.proc.pid.%d: %w", pid, err)
	}
	tv := kp.Proc.P_starttime
	// Sec must be positive: negatives become huge uint64 tokens; Usec may validly be zero.
	if tv.Sec <= 0 {
		return 0, fmt.Errorf("sysctl kern.proc.pid.%d: no usable start timeval (sec=%d usec=%d)",
			pid, tv.Sec, tv.Usec)
	}
	return packStartTimeval(tv), nil
}

// packStartTimeval flattens the creation timeval into the uint64 identity token persisted in the pgid record.
// The teardown adapter duplicates this rule; mirror tests prevent drift because liveness compares tokens exactly.
func packStartTimeval(tv unix.Timeval) uint64 {
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}
