//go:build linux

package guestd

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// vsockPortKey is the kernel-cmdline parameter carrying the AF_VSOCK port guestd
// serves the GuestControl handshake on (§(e), T2). The host sets it in the VM's
// cmdline; guestd reads it from /proc/cmdline.
const vsockPortKey = "compass.vsock_port"

// vmaddrPortAny is AF_VSOCK's VMADDR_PORT_ANY wildcard (uint32 -1). A listen on
// it binds an auto-assigned port, so it is not a valid configured handshake port.
const vmaddrPortAny = 0xFFFFFFFF

// parseVsockPort reads the last compass.vsock_port token; missing, empty, non-numeric, zero, or out-of-range values fail closed.
// guestd cannot serve the handshake without a valid port.
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
	// rejected, not truncated. Port 0 is not a valid listen port, and 0xFFFFFFFF
	// is VMADDR_PORT_ANY — the wildcard that would auto-assign a port and silently
	// break the host handshake. Reject both.
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("kernel cmdline %s=%q is not a valid port: %w", vsockPortKey, raw, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("kernel cmdline %s=%q is not a valid port: must be non-zero", vsockPortKey, raw)
	}
	if n == vmaddrPortAny {
		return 0, fmt.Errorf("kernel cmdline %s=%q is reserved (VMADDR_PORT_ANY)", vsockPortKey, raw)
	}
	return uint32(n), nil
}

// bootNonceKey carries the host-generated session nonce, which guestd echoes to bind the response to this boot.
// It is optional for older guests; a present malformed value fails closed.
const bootNonceKey = "compass.boot_nonce"

// parseBootNonce follows parseVsockPort's last-occurrence rule; absence is optional, but empty or non-hex values fail.
func parseBootNonce(procCmdline string) ([]byte, error) {
	raw := ""
	found := false
	for tok := range strings.FieldsSeq(procCmdline) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok || key != bootNonceKey {
			continue
		}
		raw = val
		found = true
	}
	if !found {
		return nil, nil
	}
	if raw == "" {
		return nil, fmt.Errorf("kernel cmdline %s has an empty value", bootNonceKey)
	}
	nonce, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("kernel cmdline %s=%q is not valid hex: %w", bootNonceKey, raw, err)
	}
	return nonce, nil
}

// gatewayPortKey carries the optional host AgentGateway vsock port.
// Older cmdlines omit it and start no forwarder.
const gatewayPortKey = "compass.gateway_port"

// parseGatewayPort follows parseVsockPort's token and range checks but allows absence.
// A present malformed value fails boot because it came from host configuration.
func parseGatewayPort(procCmdline string) (uint32, bool, error) {
	raw := ""
	found := false
	for tok := range strings.FieldsSeq(procCmdline) {
		key, val, ok := strings.Cut(tok, "=")
		if !ok || key != gatewayPortKey {
			continue
		}
		raw = val
		found = true
	}
	if !found {
		return 0, false, nil
	}
	if raw == "" {
		return 0, false, fmt.Errorf("kernel cmdline %s has an empty value", gatewayPortKey)
	}

	// Same validation as parseVsockPort: reject an out-of-range value here
	// rather than truncating, reject port 0 (not a valid dial target), and
	// reject 0xFFFFFFFF (VMADDR_PORT_ANY, the AF_VSOCK wildcard sentinel).
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, false, fmt.Errorf("kernel cmdline %s=%q is not a valid port: %w", gatewayPortKey, raw, err)
	}
	if n == 0 {
		return 0, false, fmt.Errorf("kernel cmdline %s=%q is not a valid port: must be non-zero", gatewayPortKey, raw)
	}
	if n == vmaddrPortAny {
		return 0, false, fmt.Errorf("kernel cmdline %s=%q is reserved (VMADDR_PORT_ANY)", gatewayPortKey, raw)
	}
	return uint32(n), true, nil
}
