//go:build unix && !linux

package microvm

import "syscall"

// orphanGuardSysProcAttr returns nil on non-Linux Unix: there is no equivalent
// to PR_SET_PDEATHSIG. This stub keeps the unix-tagged package portable; normal
// teardown still relies on Shutdown.
func orphanGuardSysProcAttr() *syscall.SysProcAttr {
	return nil
}
