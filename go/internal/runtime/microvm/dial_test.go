//go:build unix

package microvm

// Hermetic suite for the host-side hybrid-vsock dialer and the GuestControl
// Connect client (RIG-2588 T3). No KVM, no `microvm` build tag: a fake muxer is
// a plain unix-socket listener that speaks the CH `CONNECT <port>`/`OK` preamble
// and then either echoes the byte stream (dialer tests) or hands the transparent
// connection to an in-process h2c Connect server (the Health round-trip).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// testTimeout bounds every blocking dial/RPC so a wedged fake fails the test
// fast instead of hanging the suite.
const testTimeout = 15 * time.Second

// guestPort is the guest vsock port the tests steer to; the exact value is
// asserted in the preamble.
const guestPort uint32 = 1234

// socketPath returns an AF_UNIX path under the test's temp dir short enough to
// fit sockaddr_un's sun_path.
func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "vsock.sock")
}

// handoff describes what a fake muxer does with a connection after it has read
// the CONNECT preamble: the preamble bytes it saw, and a function that takes
// over the (now transparent) connection.
type handoff struct {
	// ackLine is written back to the host as the muxer's response to CONNECT,
	// followed by "\n". "OK 1024" is the success ack; anything else is a refusal.
	ackLine string
	// afterAck runs with the connection once the ack has been written; nil closes
	// the connection immediately (a refusal that hangs up).
	afterAck func(conn net.Conn)
}

// fakeMuxer binds a unix listener at path, accepts one connection, reads its
// `CONNECT <port>\n` preamble line, records it, writes h.ackLine, and then runs
// h.afterAck. The captured preamble is returned via the gotPreamble channel.
func fakeMuxer(t *testing.T, path string, h handoff) <-chan string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding fake muxer: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // listener teardown

	gotPreamble := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			_ = conn.Close() // accept-path teardown
			return
		}
		gotPreamble <- strings.TrimRight(line, "\r\n")
		if _, err := io.WriteString(conn, h.ackLine+"\n"); err != nil {
			_ = conn.Close() // ack-path teardown
			return
		}
		if h.afterAck == nil {
			_ = conn.Close() // refusal hangup
			return
		}
		// The host consumes exactly the ack line off the raw socket (one byte at
		// a time), so any buffered-but-unconsumed bytes r holds are none here —
		// the CONNECT line was the only thing sent before the ack. Hand the raw
		// conn to afterAck.
		h.afterAck(conn)
	}()
	return gotPreamble
}

func TestDialGuest_Preamble(t *testing.T) {
	path := socketPath(t)
	// Echo the byte stream after acking, so the round-trip below proves the
	// connection is transparent past the preamble.
	got := fakeMuxer(t, path, handoff{
		ackLine: "OK 1024",
		afterAck: func(conn net.Conn) {
			_, _ = io.Copy(conn, conn) // echo until the host closes
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := DialGuest(ctx, path, guestPort)
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}
	defer conn.Close() // test cleanup

	if pre := <-got; pre != "CONNECT 1234" {
		t.Fatalf("preamble = %q, want %q", pre, "CONNECT 1234")
	}

	// The returned conn is transparent: a write echoes straight back.
	const payload = "ping"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("writing to guest: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading echo: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo = %q, want %q", buf, payload)
	}
}

func TestDialGuest_Refusal(t *testing.T) {
	path := socketPath(t)
	// The muxer answers with a non-OK line and hangs up: a refusal.
	fakeMuxer(t, path, handoff{ackLine: "ERR no such port", afterAck: nil})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	conn, err := DialGuest(ctx, path, guestPort)
	if err == nil {
		_ = conn.Close() // unexpected-success cleanup
		t.Fatal("DialGuest succeeded against a refusing muxer; want error")
	}
	if conn != nil {
		t.Fatalf("DialGuest returned a non-nil conn on refusal: %v", conn)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error = %q, want it to mention the refusal", err)
	}
}

// healthHandler is a canned GuestControl handler: every Health returns the same
// response, so the round-trip test asserts the fields survive the wire.
type healthHandler struct {
	resp *compassv1.HealthResponse
}

func (h healthHandler) Health(context.Context, *connect.Request[compassv1.HealthRequest]) (*connect.Response[compassv1.HealthResponse], error) {
	return connect.NewResponse(h.resp), nil
}

func TestGuestClient_HealthRoundTrip(t *testing.T) {
	path := socketPath(t)

	// h2c Connect server for the GuestControl handler.
	mux := http.NewServeMux()
	mux.Handle(compassv1internalconnect.NewGuestControlHandler(healthHandler{
		resp: &compassv1.HealthResponse{
			GuestdVersion:    "v2a-spike",
			NetProvisioned:   true,
			WorkspaceMounted: true,
		},
	}))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols}
	t.Cleanup(func() { _ = srv.Close() }) // server teardown

	// The fake muxer accepts each host dial, reads+asserts the CONNECT preamble,
	// acks OK, then hands the transparent conn to the h2c server via a one-shot
	// net.Listener. A new listener per accepted connection keeps the server's
	// Accept loop fed without a persistent shared listener.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding muxer: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // listener teardown

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil {
					_ = conn.Close() // accept-path teardown
					return
				}
				if strings.TrimRight(line, "\r\n") != "CONNECT 1234" {
					_ = conn.Close() // bad preamble
					return
				}
				if _, err := io.WriteString(conn, "OK 1024\n"); err != nil {
					_ = conn.Close() // ack-path teardown
					return
				}
				// Hand the connection to the h2c server. The host reads the ack
				// one byte at a time off the raw socket and buffers nothing, and
				// the muxer here sent nothing after the ack, so the server sees a
				// clean HTTP/2 preface as the client's first post-ack bytes.
				_ = srv.Serve(newOneConnListener(conn)) // serves one conn; errors on close
			}()
		}
	}()

	client := GuestClient(BootConfig{VsockSocket: path, VsockPort: guestPort})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	resp, err := client.Health(ctx, connect.NewRequest(&compassv1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := resp.Msg.GetGuestdVersion(); got != "v2a-spike" {
		t.Fatalf("guestd_version = %q, want %q", got, "v2a-spike")
	}
	if !resp.Msg.GetNetProvisioned() {
		t.Fatal("net_provisioned = false, want true")
	}
	if !resp.Msg.GetWorkspaceMounted() {
		t.Fatal("workspace_mounted = false, want true")
	}
}

// oneConnListener adapts a single already-accepted net.Conn into a net.Listener
// so http.Server.Serve can drive its HTTP/2 handshake on it. The first Accept
// yields the conn; the next returns an error, which ends Serve's Accept loop
// without disturbing the already-handed-off connection (Serve handles it in its
// own goroutine).
type oneConnListener struct {
	conn net.Conn
	done bool
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	return &oneConnListener{conn: conn}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, errors.New("microvm test: one-shot listener exhausted")
	}
	l.done = true
	return l.conn, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
