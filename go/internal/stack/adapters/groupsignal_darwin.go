//go:build darwin

package adapters

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readGroupLeaderStartTime reads the group leader's start time from the
// kernel's process table — the darwin half of the teardown-side identity read.
// There is no /proc on darwin, so the token comes from sysctl
// kern.proc.pid.<pgid>, whose KinfoProc carries the process's creation timeval
// (Proc.P_starttime). The leader's pid is the pgid, since pid == pgid for a
// Setpgid child.
//
// A pgid that names no process yields an error rather than a zero token: the
// kernel returns a short result for an unknown pid, which SysctlKinfoProc
// rejects, and the explicit zero-timeval guard closes the remaining case. Alive
// then reports not-alive, so the identity check fails closed.
func readGroupLeaderStartTime(pgid int) (uint64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pgid)
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.proc.pid.%d: %w", pgid, err)
	}
	tv := kp.Proc.P_starttime
	// Sec alone is the guard, matching the spawn side (stack.readProcessStartTime):
	// Sec is signed, so a negative would pack into a huge uint64 that looks like
	// a valid token, while Usec == 0 is a legitimate exact-second start and must
	// not be rejected.
	if tv.Sec <= 0 {
		return 0, fmt.Errorf("sysctl kern.proc.pid.%d: no usable start timeval (sec=%d usec=%d)",
			pgid, tv.Sec, tv.Usec)
	}
	return packGroupLeaderTimeval(tv), nil
}

// packGroupLeaderTimeval flattens a process-creation timeval into the uint64
// identity token the pgid record carries.
//
// It MUST stay byte-identical in effect to stack.packStartTimeval, which the
// spawn side uses to write the token this reads back. The two packages cannot
// import each other's internals, so the expression is duplicated for the same
// reason parseGroupLeaderStat duplicates stack.parseStatStartTime — and here the
// duplication is the load-bearing one: Alive compares this against a token the
// spawn side produced, for uint64 equality, so any drift would report every live
// child as not-alive and silently skip it at teardown. The mirror test
// (groupsignal_darwin_test.go) feeds one synthetic timeval through both packings
// and asserts the same uint64, so a one-sided change reds.
func packGroupLeaderTimeval(tv unix.Timeval) uint64 {
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}
