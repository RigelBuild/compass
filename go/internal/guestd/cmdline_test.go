//go:build linux

package guestd

// Hermetic table-driven suite for parseVsockPort — the pure kernel-cmdline
// parser (§(e), T2). It defends the fail-closed contract: guestd cannot serve
// the handshake without a valid non-zero vsock port, so every malformed cmdline
// is an error and only a well-formed compass.vsock_port=<n> yields a port.

import (
	"bytes"
	"testing"
)

func TestParseVsockPort(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    uint32
		wantErr bool
	}{
		{
			name:    "single param among others",
			cmdline: "console=ttyS0 compass.vsock_port=1024 ro",
			want:    1024,
		},
		{
			name:    "only param",
			cmdline: "compass.vsock_port=5000",
			want:    5000,
		},
		{
			name:    "trailing newline (as /proc/cmdline yields)",
			cmdline: "console=ttyS0 compass.vsock_port=42\n",
			want:    42,
		},
		{
			name:    "max valid port (just below VMADDR_PORT_ANY)",
			cmdline: "compass.vsock_port=4294967294",
			want:    4294967294,
		},
		{
			name:    "last occurrence wins",
			cmdline: "compass.vsock_port=1 compass.vsock_port=2",
			want:    2,
		},
		{
			name:    "missing key",
			cmdline: "console=ttyS0 ro quiet",
			wantErr: true,
		},
		{
			name:    "empty cmdline",
			cmdline: "",
			wantErr: true,
		},
		{
			name:    "empty value",
			cmdline: "compass.vsock_port=",
			wantErr: true,
		},
		{
			name:    "non-numeric value",
			cmdline: "compass.vsock_port=abc",
			wantErr: true,
		},
		{
			name:    "zero is not a valid port",
			cmdline: "compass.vsock_port=0",
			wantErr: true,
		},
		{
			name:    "VMADDR_PORT_ANY sentinel is reserved",
			cmdline: "compass.vsock_port=4294967295",
			wantErr: true,
		},
		{
			name:    "overflows uint32",
			cmdline: "compass.vsock_port=4294967296",
			wantErr: true,
		},
		{
			name:    "negative value",
			cmdline: "compass.vsock_port=-1",
			wantErr: true,
		},
		{
			// A bare "compass.vsock_port" token (no '=') is not the key=value
			// form and must not be mistaken for a value.
			name:    "key without value token",
			cmdline: "compass.vsock_port ro",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVsockPort(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseVsockPort(%q) = %d, nil; want error", tt.cmdline, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVsockPort(%q) unexpected error: %v", tt.cmdline, err)
			}
			if got != tt.want {
				t.Fatalf("parseVsockPort(%q) = %d, want %d", tt.cmdline, got, tt.want)
			}
		})
	}
}

// TestParseBootNonce defends the boot-nonce contract (§(e)): the nonce is
// OPTIONAL hardening, so an absent key is (nil, nil) and Health still answers;
// a present key must be valid hex; an empty or non-hex value is a malformed
// boot config and fail-closes.
func TestParseBootNonce(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    []byte
		wantErr bool
	}{
		{
			name:    "absent key echoes empty nonce",
			cmdline: "console=ttyS0 compass.vsock_port=1024",
			want:    nil,
		},
		{
			name:    "valid hex nonce",
			cmdline: "compass.vsock_port=1024 compass.boot_nonce=deadbeef",
			want:    []byte{0xde, 0xad, 0xbe, 0xef},
		},
		{
			name:    "trailing newline as /proc/cmdline yields",
			cmdline: "compass.boot_nonce=00ff\n",
			want:    []byte{0x00, 0xff},
		},
		{
			name:    "last occurrence wins",
			cmdline: "compass.boot_nonce=aa compass.boot_nonce=bb",
			want:    []byte{0xbb},
		},
		{
			name:    "empty value is an error",
			cmdline: "compass.boot_nonce=",
			wantErr: true,
		},
		{
			name:    "non-hex value is an error",
			cmdline: "compass.boot_nonce=zzzz",
			wantErr: true,
		},
		{
			name:    "odd-length hex is an error",
			cmdline: "compass.boot_nonce=abc",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBootNonce(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseBootNonce(%q) = %x, nil; want error", tt.cmdline, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBootNonce(%q) unexpected error: %v", tt.cmdline, err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("parseBootNonce(%q) = %x, want %x", tt.cmdline, got, tt.want)
			}
		})
	}
}

// TestParseGatewayPort defends the §(d) gateway-port contract: the parameter is
// OPTIONAL (an absent key ⇒ (0, false, nil), no forwarder), but a
// present-but-malformed value fail-closes the boot with the parseVsockPort
// validation (non-zero uint32, VMADDR_PORT_ANY reserved, non-numeric/overflow).
func TestParseGatewayPort(t *testing.T) {
	tests := []struct {
		name      string
		cmdline   string
		want      uint32
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "absent key is (0, false, nil)",
			cmdline:   "console=ttyS0 compass.vsock_port=1024 ro",
			want:      0,
			wantFound: false,
		},
		{
			name:      "empty cmdline is (0, false, nil)",
			cmdline:   "",
			want:      0,
			wantFound: false,
		},
		{
			name:      "valid port among others",
			cmdline:   "console=ttyS0 compass.gateway_port=1025 ro",
			want:      1025,
			wantFound: true,
		},
		{
			name:      "only param",
			cmdline:   "compass.gateway_port=5000",
			want:      5000,
			wantFound: true,
		},
		{
			name:      "trailing newline (as /proc/cmdline yields)",
			cmdline:   "compass.gateway_port=42\n",
			want:      42,
			wantFound: true,
		},
		{
			name:      "max valid port (just below VMADDR_PORT_ANY)",
			cmdline:   "compass.gateway_port=4294967294",
			want:      4294967294,
			wantFound: true,
		},
		{
			name:      "last occurrence wins",
			cmdline:   "compass.gateway_port=1 compass.gateway_port=2",
			want:      2,
			wantFound: true,
		},
		{
			name:    "present-but-empty value is an error",
			cmdline: "compass.gateway_port=",
			wantErr: true,
		},
		{
			name:    "non-numeric value is an error",
			cmdline: "compass.gateway_port=abc",
			wantErr: true,
		},
		{
			name:    "zero is not a valid port",
			cmdline: "compass.gateway_port=0",
			wantErr: true,
		},
		{
			name:    "VMADDR_PORT_ANY sentinel is reserved",
			cmdline: "compass.gateway_port=4294967295",
			wantErr: true,
		},
		{
			name:    "overflows uint32",
			cmdline: "compass.gateway_port=4294967296",
			wantErr: true,
		},
		{
			name:    "negative value is an error",
			cmdline: "compass.gateway_port=-1",
			wantErr: true,
		},
		{
			// A bare "compass.gateway_port" token (no '=') is not the key=value
			// form, so it is skipped like any non-matching token — for an OPTIONAL
			// key that means "absent", (0, false, nil), NOT an error.
			name:      "key without value token is treated as absent",
			cmdline:   "compass.gateway_port ro",
			want:      0,
			wantFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := parseGatewayPort(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseGatewayPort(%q) = %d, %v, nil; want error", tt.cmdline, got, found)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGatewayPort(%q) unexpected error: %v", tt.cmdline, err)
			}
			if got != tt.want || found != tt.wantFound {
				t.Fatalf("parseGatewayPort(%q) = %d, %v; want %d, %v", tt.cmdline, got, found, tt.want, tt.wantFound)
			}
		})
	}
}
