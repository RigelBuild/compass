//go:build linux

package guestd

import (
	"fmt"
	"syscall"
)

// powerOff uses reboot(RB_POWER_OFF) because exiting guest PID 1 panics the kernel and leaves the VMM wedged.
// Success does not return; an error means the syscall was refused.
func powerOff() error {
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		return fmt.Errorf("reboot(RB_POWER_OFF): %w", err)
	}
	return nil
}
