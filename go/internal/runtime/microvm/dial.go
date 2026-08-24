package microvm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

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
// ctx bounds the AF_UNIX dial and the preamble handshake (via a conn deadline).
// On any failure the connection is closed and a descriptive error is returned
// (no fd leak).
func DialGuest(ctx context.Context, vsockSocket string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", vsockSocket)
	if err != nil {
		return nil, fmt.Errorf("microvm: dialing hybrid-vsock socket %q: %w", vsockSocket, err)
	}

	// The write + ack read below run on an already-connected socket that
	// DialContext no longer governs. Without a bound, a guest that accepts the
	// connection but never acks (booted the socket, then wedged) would hang the
	// dial forever — and since this backs the GuestClient transport's
	// DialContext, the RPC ctx could not abort it. Bound the handshake to ctx's
	// deadline, then clear it before the transparent stream is handed off so the
	// caller (http2) owns all further deadlines. Callers that must abort a hung
	// handshake pass a ctx with a deadline; a bare cancel is not aborted
	// mid-handshake — adequate for the spike, where every dial is deadline-bound.
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			_ = conn.Close() // teardown on an already-failing path
			return nil, fmt.Errorf("microvm: setting handshake deadline for port %d: %w", port, err)
		}
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

	// Clear the handshake deadline so the returned stream carries none into the
	// transport (a no-op when ctx had no deadline).
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close() // teardown on an already-failing path
		return nil, fmt.Errorf("microvm: clearing handshake deadline for port %d: %w", port, err)
	}

	return conn, nil
}

// maxPreambleLen caps the muxer's ack line so a peer that streams bytes without
// ever sending '\n' cannot exhaust host memory. The real ack ("OK <port>") is a
// dozen-odd bytes; 256 is generous headroom.
const maxPreambleLen = 256

// readPreambleLine reads a single `\n`-terminated line from conn one byte at a
// time, so no application data past the newline is consumed. The returned string
// excludes the trailing `\n` (and a preceding `\r` if present). It fails once an
// unterminated line exceeds maxPreambleLen.
func readPreambleLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimSuffix(b.String(), "\r"), nil
			}
			if b.Len() == maxPreambleLen {
				return "", fmt.Errorf("microvm: vsock preamble ack exceeded %d bytes without a newline", maxPreambleLen)
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return "", err
		}
	}
}

// GuestClient returns a GuestControl Connect client whose transport dials the
// guest over the hybrid vsock at vsockSocket/port and speaks cleartext HTTP/2
// (h2c). The transport ignores the HTTP network/addr entirely — every dial goes
// to DialGuest — so the base URL host is a placeholder. It takes the socket and
// port directly (not a whole BootConfig) so a reconnect or a V2b caller holding
// only an endpoint need not fabricate one. h2c is enabled the same way the
// gateway server enables it (go/internal/runner/gateway/socket.go: http.Protocols
// + SetUnencryptedHTTP2), mirrored here on the client transport.
func GuestClient(vsockSocket string, port uint32) compassv1internalconnect.GuestControlClient {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	httpClient := &http.Client{
		Transport: &http.Transport{
			Protocols: protocols,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return DialGuest(ctx, vsockSocket, port)
			},
		},
	}
	return compassv1internalconnect.NewGuestControlClient(httpClient, "http://guest")
}
