//go:build linux

package guestd

import (
	"fmt"
	"syscall"
)

// powerOff performs the PID-1-legal guest power-off that ends an RPC-driven Stop
// (§(d)). guestd is guest PID 1, and a PID-1 process *exit* panics the kernel
// (main.go:11-13), which cloud-hypervisor never observes as a VMM exit — the
// vCPU wedges. reboot(RB_POWER_OFF) IS legal for PID 1 and the VMM observes it
// as guest shutdown and exits on it, so the host's Stop sees a real VMM exit
// within its timeout instead of always falling through to the hard kill.
//
// It is deliberately tiny and obviously-correct: the real power-off is
// KVM-integration-proven in U4's Stop-grace row (it requires PID 1 in a real
// guest), not unit-tested here. On success the call does not return; a returned
// error means the syscall was refused (guestd is not actually PID 1).
func powerOff() error {
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		return fmt.Errorf("reboot(RB_POWER_OFF): %w", err)
	}
	return nil
}
