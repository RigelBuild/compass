//go:build linux

package microvm

import "syscall"

// orphanGuardSysProcAttr installs Linux PR_SET_PDEATHSIG as a best-effort
// SIGTERM guard. The signal is tied to the spawning thread, so startChild locks
// that thread across Start; Shutdown remains the teardown guarantee.
func orphanGuardSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
