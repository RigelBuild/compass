//go:build linux

package guestd

import (
	"fmt"
	"strconv"
	"strings"
)

// vsockPortKey is the kernel-cmdline parameter carrying the AF_VSOCK port guestd
// serves the GuestControl handshake on (§(e), T2). The host sets it in the VM's
// cmdline; guestd reads it from /proc/cmdline.
const vsockPortKey = "compass.vsock_port"

// parseVsockPort extracts compass.vsock_port=<n> from a /proc/cmdline string.
// The cmdline is a single line of space-separated tokens, each either a bare
// flag or key=value; the last occurrence of the key wins, matching how the
// kernel resolves duplicated parameters. A missing key, an empty value, a
// non-numeric value, or a value outside the valid non-zero uint32 port range is
// a fail-closed error — guestd cannot serve the handshake without a port.
func parseVsockPort(procCmdline string) (uint32, error) {
	raw := ""
	found := false
	for tok := range strings.FieldsSeq(procCmdline) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok || key != vsockPortKey {
			continue
		}
		raw = val
		found = true
	}
	if !found {
		return 0, fmt.Errorf("kernel cmdline is missing %s=<n>", vsockPortKey)
	}
	if raw == "" {
		return 0, fmt.Errorf("kernel cmdline %s has an empty value", vsockPortKey)
	}

	// A vsock port is a uint32; parse as such so an out-of-range value is
	// rejected here rather than silently truncated. Port 0 is not a valid
	// listen port for the handshake.
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("kernel cmdline %s=%q is not a valid port: %w", vsockPortKey, raw, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("kernel cmdline %s=%q is not a valid port: must be non-zero", vsockPortKey, raw)
	}
	return uint32(n), nil
}
