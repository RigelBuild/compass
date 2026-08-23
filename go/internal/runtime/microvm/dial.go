package microvm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	compassv1internalconnect "github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// DialGuest opens the host end of a cloud-hypervisor hybrid vsock and steers a
// connection to the guest's `port`, returning the now-transparent stream.
//
// CH's vsock is hybrid: the host end is the AF_UNIX socket passed to
// `--vsock cid=<cid>,socket=<path>`, and a connection is routed to a guest port
// by writing a `CONNECT <port>\n` preamble line before any application data.
// The muxer acknowledges with `OK <assigned_port>\n` before the byte stream
// goes transparent; anything else (an error line, a short read, a close) is a
// refusal. CH's implementation is a copy of Firecracker's — protocol detail per
// CH docs/vsock.md and Firecracker docs/vsock.md.
//
// ctx bounds the AF_UNIX dial. On any failure the connection is closed and a
// descriptive error is returned (no fd leak).
func DialGuest(ctx context.Context, vsockSocket string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", vsockSocket)
	if err != nil {
		return nil, fmt.Errorf("microvm: dialing hybrid-vsock socket %q: %w", vsockSocket, err)
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close() // teardown on an already-failing path
		return nil, fmt.Errorf("microvm: writing CONNECT preamble for port %d: %w", port, err)
	}

	// The muxer answers with a single line. Read exactly that line and no more:
	// a buffered reader would over-read into the application stream, so read a
	// byte at a time, stop at the newline, and leave the rest on the wire.
	line, err := readPreambleLine(conn)
	if err != nil {
		_ = conn.Close() // teardown on an already-failing path
		return nil, fmt.Errorf("microvm: reading vsock preamble ack for port %d: %w", port, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		_ = conn.Close() // teardown on a refused connection
		return nil, fmt.Errorf("microvm: hybrid-vsock refused connection to port %d: muxer answered %q, want \"OK <port>\"", port, line)
	}

	return conn, nil
}

// readPreambleLine reads a single `\n`-terminated line from conn one byte at a
// time, so no application data past the newline is consumed. The returned string
// excludes the trailing `\n` (and a preceding `\r` if present).
func readPreambleLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(b.String(), "\r"), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return "", err
		}
	}
}

// GuestClient returns a GuestControl Connect client whose transport dials the
// guest over the hybrid vsock described by cfg and speaks cleartext HTTP/2
// (h2c). The transport ignores the HTTP network/addr entirely — every dial goes
// to DialGuest(cfg.VsockSocket, cfg.VsockPort) — so the base URL host is a
// placeholder. h2c is enabled the same way the gateway server enables it
// (go/internal/runner/gateway/socket.go: http.Protocols + SetUnencryptedHTTP2),
// mirrored here on the client transport.
func GuestClient(cfg BootConfig) compassv1internalconnect.GuestControlClient {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{
		Transport: &http.Transport{
			Protocols: protocols,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return DialGuest(ctx, cfg.VsockSocket, cfg.VsockPort)
			},
		},
	}
	return compassv1internalconnect.NewGuestControlClient(httpClient, "http://guest")
}
