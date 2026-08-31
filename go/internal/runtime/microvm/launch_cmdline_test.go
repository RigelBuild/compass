//go:build unix

package microvm

import (
	"slices"
	"strings"
	"testing"
)

// cmdlineArg extracts the --cmdline value from a vmmArgs argv (the token
// following the "--cmdline" flag), failing the test if it is absent.
func cmdlineArg(t *testing.T, args []string) string {
	t.Helper()
	i := slices.Index(args, "--cmdline")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("argv %v carries no --cmdline value", args)
	}
	return args[i+1]
}

// TestVmmArgsAppendsGatewayPort pins the record §(b)/§(d) cmdline contract: a
// non-zero BootConfig.GatewayPort is appended as compass.gateway_port=<n>
// beside the existing compass.vsock_port append, so guestd's forwarder learns
// the host-served gateway port from /proc/cmdline the same way it learns the
// control port.
func TestVmmArgsAppendsGatewayPort(t *testing.T) {
	cfg := BootConfig{VsockPort: 1024, GatewayPort: 1025}
	cmdline := cmdlineArg(t, vmmArgs(cfg, "/tmp/console", launchOptions{}))
	if !strings.Contains(cmdline, "compass.gateway_port=1025") {
		t.Fatalf("cmdline = %q, want it to carry compass.gateway_port=1025", cmdline)
	}
	if !strings.Contains(cmdline, "compass.vsock_port=1024") {
		t.Fatalf("cmdline = %q, want the existing compass.vsock_port append preserved", cmdline)
	}
}

// TestVmmArgsOmitsZeroGatewayPort pins the harness-compatibility carve-out: a
// zero GatewayPort (a V2a-era boot or a hermetic harness that starts no
// gateway) is NOT appended, so guestd starts no proxy and the V2b/V3 suites
// keep booting unchanged (record §(b)/§(d)).
func TestVmmArgsOmitsZeroGatewayPort(t *testing.T) {
	cfg := BootConfig{VsockPort: 1024}
	cmdline := cmdlineArg(t, vmmArgs(cfg, "/tmp/console", launchOptions{}))
	if strings.Contains(cmdline, "compass.gateway_port") {
		t.Fatalf("cmdline = %q, want no compass.gateway_port token for a zero GatewayPort", cmdline)
	}
}
