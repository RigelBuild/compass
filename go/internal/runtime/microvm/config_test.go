package microvm

import "testing"

// TestGatewaySocketPath pins the CH guest→host suffix contract (record §(a)):
// the host-side AF_UNIX listener path is the launch-time --vsock socket path
// with an appended "_" and the guest-side port, decimal, no zero-padding.
func TestGatewaySocketPath(t *testing.T) {
	tests := []struct {
		name  string
		vsock string
		port  uint32
		want  string
	}{
		{
			name:  "the fixed agent gateway port",
			vsock: "/run/compass/microvm/abc/vsock.sock",
			port:  1025,
			want:  "/run/compass/microvm/abc/vsock.sock_1025",
		},
		{
			name:  "the CH docs example",
			vsock: "/tmp/ch.vsock",
			port:  1234,
			want:  "/tmp/ch.vsock_1234",
		},
		{
			name:  "a port with all digits significant (no zero-padding)",
			vsock: "/tmp/s.sock",
			port:  907,
			want:  "/tmp/s.sock_907",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GatewaySocketPath(tt.vsock, tt.port); got != tt.want {
				t.Fatalf("GatewaySocketPath(%q, %d) = %q, want %q", tt.vsock, tt.port, got, tt.want)
			}
		})
	}
}
