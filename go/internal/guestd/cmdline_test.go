//go:build linux

package guestd

// Hermetic table-driven suite for parseVsockPort — the pure kernel-cmdline
// parser (§(e), T2). It defends the fail-closed contract: guestd cannot serve
// the handshake without a valid non-zero vsock port, so every malformed cmdline
// is an error and only a well-formed compass.vsock_port=<n> yields a port.

import "testing"

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
