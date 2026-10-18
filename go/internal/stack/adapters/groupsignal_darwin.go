//go:build darwin

package adapters

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readGroupLeaderStartTime reads the Darwin process creation timeval via sysctl; Darwin has no /proc.
// Missing processes and zero timevals fail closed, so no invalid token can match a recorded identity.
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

// packGroupLeaderTimeval duplicates stack.packStartTimeval because the packages cannot share internals.
// Liveness compares these tokens for equality; mirrored tests prevent drift.
func packGroupLeaderTimeval(tv unix.Timeval) uint64 {
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec)
}
