//go:build darwin

package stack

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readProcessStartTime reads the process's start time from the kernel's process
// table — the darwin half of the readStartTime seam. There is no /proc on
// darwin, so the token comes from sysctl kern.proc.pid.<pid>, whose KinfoProc
// carries the process's creation timeval (Proc.P_starttime).
//
// A pid that does not exist yields an error rather than a zero token: the
// kernel returns a short (zero-length) result for an unknown pid, which
// SysctlKinfoProc rejects, and the explicit zero-timeval guard below closes the
// remaining case. This matters because the token feeds an equality check — a
// reader that quietly returned 0 for a dead pid would match any record that
// happened to carry 0, so the read must fail closed.
func readProcessStartTime(pid int) (uint64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.proc.pid.%d: %w", pid, err)
	}
	tv := kp.Proc.P_starttime
	// Sec alone is the guard. A real process start time is a wall-clock epoch
	// second, so any value <= 0 means the kernel gave us nothing usable — and
	// Sec is signed, so a negative would otherwise pack into a huge uint64 that
	// looks like a valid token. Usec is deliberately NOT part of the condition:
	// a process really can start on an exact second boundary, and rejecting
	// Usec == 0 would fail a legitimate read about once in a million.
	if tv.Sec <= 0 {
		return 0, fmt.Errorf("sysctl kern.proc.pid.%d: no usable start timeval (sec=%d usec=%d)",
			pid, tv.Sec, tv.Usec)
	}
	return packStartTimeval(tv), nil
}

// packStartTimeval flattens a process-creation timeval into the uint64 identity
// token the pgid record carries.
//
// This expression is DUPLICATED in adapters.packGroupLeaderTimeval, which the
// teardown side uses, for the same reason the /proc field-22 parse is
// duplicated there: the two are read-only leaf helpers and the packages cannot
// reach into each other. The duplication is load-bearing rather than incidental
// — GroupSignaller.Alive compares a spawn-side token against a down-side read
// for uint64 equality, so a drift between the two packings would report every
// live child as not-alive and silently skip it at teardown. Mirrored tests in
// both packages feed one synthetic timeval through both and assert the same
// uint64, so a change to one packing without the other reds.
//
// Microseconds since the epoch fits a uint64 for ~584,000 years, so the
// multiply cannot overflow for any real process.
func packStartTimeval(tv unix.Timeval) uint64 {
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}
