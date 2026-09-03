//go:build unix && !linux

package microvm

import "syscall"

// orphanGuardSysProcAttr returns nil on non-Linux unix (darwin): there is no
// PR_SET_PDEATHSIG equivalent, so the child gets no parent-death guard. The
// microVM launcher only runs for real on Linux (KVM); this build exists so the
// unix-tagged package cross-compiles on darwin — e.g. when `go build
// ./cmd/compass-app` pulls microvm into a darwin compile — without naming the
// Linux-only Pdeathsig field. Teardown correctness does not depend on the guard
// (Shutdown is the real guarantee, record §(g)).
func orphanGuardSysProcAttr() *syscall.SysProcAttr {
	return nil
}
