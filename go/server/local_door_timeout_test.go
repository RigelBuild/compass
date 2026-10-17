package server

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A local process that opens a connection and never sends a request must not
// hold it open: the local doors' header timeout closes it.
func TestLocalDoorClosesStalledConnection(t *testing.T) {
	t.Parallel()
	conn := dialLocalDoor(t)
	expectServerClose(t, conn)
}

// An h2c prior-knowledge connection that sends its preface and then idles must
// also be closed: the HTTP/2 path takes the server's IdleTimeout.
func TestLocalDoorClosesIdleH2CConnection(t *testing.T) {
	t.Parallel()
	conn := dialLocalDoor(t)
	// Client preface plus an empty SETTINGS frame (length 0, type 4, flags 0, stream 0).
	preface := append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), 0, 0, 0, 4, 0, 0, 0, 0, 0)
	if _, err := conn.Write(preface); err != nil {
		t.Fatalf("write preface: %v", err)
	}
	expectServerClose(t, conn)
}

func dialLocalDoor(t *testing.T) net.Conn {
	t.Helper()
	srv := newLocalDoorServer(http.NotFoundHandler(), 200*time.Millisecond, 200*time.Millisecond)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// expectServerClose drains whatever the server sends and requires EOF well
// before the 10s backstop, which only stops a hung test.
func expectServerClose(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	start := time.Now()
	_, err := io.Copy(io.Discard, conn)
	if err != nil {
		t.Fatalf("read on stalled conn = %v, want EOF from the server closing it", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("server closed the stalled conn after %v, want near the 200ms timeout", elapsed)
	}
}
