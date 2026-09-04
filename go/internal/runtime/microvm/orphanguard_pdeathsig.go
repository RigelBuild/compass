//go:build linux

package microvm

import "syscall"

// orphanGuardSysProcAttr returns the SysProcAttr that installs the child's
// best-effort orphan guard. On Linux that is PR_SET_PDEATHSIG (SIGTERM): the
// signal fires on the spawning THREAD's death, so startChild holds the OS thread
// across cmd.Start for it to bind reliably. The real teardown guarantee is
// Shutdown, not Pdeathsig (record §(g)); this only shortens the window a wedged
// host leaves a VMM orphaned.
//
// Pdeathsig is a Linux-only field of syscall.SysProcAttr, so naming it directly
// under //go:build unix would fail the darwin compile — this helper is the
// per-platform seam that keeps the unix-tagged launcher portable.
func orphanGuardSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
