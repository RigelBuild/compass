//go:build linux

package guestd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// dialFromPipe builds a dialGateway seam whose every call returns one end of a
// fresh net.Pipe, recording the other (server) end on the returned channel so a
// test can drive the upstream side without AF_VSOCK. It is the §(d) hermetic
// seam for the forward loop.
func dialFromPipe(t *testing.T) (func(port uint32) (net.Conn, error), <-chan net.Conn) {
	t.Helper()
	servers := make(chan net.Conn, 8)
	return func(uint32) (net.Conn, error) {
		client, server := net.Pipe()
		servers <- server
		return client, nil
	}, servers
}

// dialFromUnixSocket is the CloseWrite-capable analogue of dialFromPipe: every
// call returns one end of a fresh AF_UNIX socketpair (a *net.UnixConn, which
// implements CloseWrite) and records the other end on the returned channel, so
// a test can exercise the halfCloseWrite branch that net.Pipe cannot reach.
func dialFromUnixSocket(t *testing.T) (func(port uint32) (net.Conn, error), <-chan net.Conn) {
	t.Helper()
	servers := make(chan net.Conn, 8)
	return func(uint32) (net.Conn, error) {
		client, server := unixSocketPair(t)
		servers <- server
		return client, nil
	}, servers
}

// unixSocketPair returns the two connected ends of an AF_UNIX SOCK_STREAM
// socketpair as *net.UnixConn values (which implement CloseWrite), for tests
// that need a real half-close, not net.Pipe's full-duplex-only behaviour.
func unixSocketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	return fileConn(t, fds[0]), fileConn(t, fds[1])
}

// fileConn adopts a raw socket fd as a net.Conn. net.FileConn dups the fd, so
// the original os.File is closed immediately to avoid leaking it.
func fileConn(t *testing.T, fd int) net.Conn {
	t.Helper()
	f := os.NewFile(uintptr(fd), "socketpair")
	conn, err := net.FileConn(f)
	_ = f.Close() // net.FileConn dups the fd; drop the original
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	return conn
}

// dialClientSocket connects to the proxy's AF_UNIX rendezvous, failing the test
// on error.
func dialClientSocket(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial proxy socket %s: %v", socketPath, err)
	}
	return conn
}

// noopChown is the hermetic chownSocket seam: the suite runs non-root and cannot
// chown a socket to a foreign uid (exactly why testCredential exists), so the
// forwarder tests inject this no-op. The 0600 mode — the security property the
// CI gate can enforce hermetically — is still set by the real Chmod.
func noopChown(string, uint32) error { return nil }

func TestGatewayProxySplicesBothWays(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "agent.sock")
	dial, servers := dialFromPipe(t)
	svc := &supervisor{dialGateway: dial, chownSocket: noopChown}

	closer, err := svc.startGatewayProxy(t.Context(), socketPath, uint32(os.Getuid()), 1025)
	if err != nil {
		t.Fatalf("startGatewayProxy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	client := dialClientSocket(t, socketPath)
	defer func() { _ = client.Close() }()

	// The forward loop dials the seam lazily on the first accepted connection.
	var upstream net.Conn
	select {
	case upstream = <-servers:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never dialed the gateway seam")
	}
	defer func() { _ = upstream.Close() }()

	// client -> upstream.
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(upstream, got); err != nil {
		t.Fatalf("upstream read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("upstream got %q, want ping", got)
	}

	// upstream -> client.
	if _, err := upstream.Write([]byte("pong")); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	got = make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("client got %q, want pong", got)
	}
}

func TestGatewayProxyDialErrorClosesOnlyThatConnection(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "agent.sock")

	// First dial fails; every later dial succeeds via a net.Pipe. This proves a
	// per-connection dial error closes ONLY that accepted connection and the
	// accept loop survives to serve a subsequent connection.
	var mu sync.Mutex
	calls := 0
	servers := make(chan net.Conn, 8)
	dial := func(uint32) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return nil, errors.New("host gateway not up yet")
		}
		client, server := net.Pipe()
		servers <- server
		return client, nil
	}
	svc := &supervisor{dialGateway: dial, chownSocket: noopChown}

	closer, err := svc.startGatewayProxy(t.Context(), socketPath, uint32(os.Getuid()), 1025)
	if err != nil {
		t.Fatalf("startGatewayProxy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	// First connection: dial fails, so the proxy closes it. A read returns EOF.
	failed := dialClientSocket(t, socketPath)
	buf := make([]byte, 1)
	_ = failed.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := failed.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("failed-dial connection read = %v, want EOF (proxy should close it)", err)
	}
	_ = failed.Close()

	// Second connection: the accept loop must still be running and splice it.
	ok := dialClientSocket(t, socketPath)
	defer func() { _ = ok.Close() }()
	var upstream net.Conn
	select {
	case upstream = <-servers:
	case <-time.After(2 * time.Second):
		t.Fatal("accept loop did not survive the dial error; second connection was never dialed")
	}
	defer func() { _ = upstream.Close() }()
	if _, err := ok.Write([]byte("x")); err != nil {
		t.Fatalf("second client write: %v", err)
	}
	got := make([]byte, 1)
	if _, err := io.ReadFull(upstream, got); err != nil {
		t.Fatalf("second upstream read: %v", err)
	}
	if got[0] != 'x' {
		t.Fatalf("second upstream got %q, want x", got)
	}
}

func TestGatewayProxyCloseTearsDownCleanly(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "agent.sock")
	dial, servers := dialFromPipe(t)
	svc := &supervisor{dialGateway: dial, chownSocket: noopChown}

	ctx, cancel := context.WithCancel(t.Context())
	closer, err := svc.startGatewayProxy(ctx, socketPath, uint32(os.Getuid()), 1025)
	if err != nil {
		t.Fatalf("startGatewayProxy: %v", err)
	}

	// Open one in-flight splice so ctx cancel must tear down both copy goroutines.
	client := dialClientSocket(t, socketPath)
	select {
	case <-servers:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never dialed the gateway seam")
	}

	// ctx cancel closes both conns so neither io.Copy leaks; Close then returns
	// once the accept loop has drained. A returned Close and a removed socket are
	// the hermetic no-leak assertion (the package does not use goleak).
	cancel()
	done := make(chan error, 1)
	go func() { done <- closer.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after ctx cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return; a copy goroutine leaked past ctx cancel")
	}
	_ = client.Close()

	// The listener is closed, so a fresh dial to the socket must fail.
	if c, err := net.Dial("unix", socketPath); err == nil {
		_ = c.Close()
		t.Fatal("proxy socket still accepts after Close; listener leaked")
	}
}

// newGatewaySupervisor builds a stateReady supervisor wired for a hermetic
// Provision-starts-proxy test: the gateway port is set, the socket path points
// at a tempdir, dialGateway is the net.Pipe seam, and chownSocket is a recording
// no-op (the suite is non-root). The returned *uint32 captures the uid the
// forwarder requested the chown for, so the owner-posture contract is asserted
// without privilege. It mirrors the &supervisor{} literal pattern the suite uses.
func newGatewaySupervisor(t *testing.T, socketPath string, port uint32) (*supervisor, *uint32) {
	t.Helper()
	dial, _ := dialFromPipe(t)
	var chownedTo uint32
	return &supervisor{
		version:       "v-test",
		newCredential: testCredential,
		dialGateway:   dial,
		chownSocket: func(_ string, uid uint32) error {
			chownedTo = uid
			return nil
		},
		gatewayPort:       port,
		gatewaySocketPath: socketPath,
		serveCtx:          t.Context(),
		state:             stateReady,
		execs:             map[string]*childExec{},
	}, &chownedTo
}

func TestProvisionStartsGatewayProxy(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "agent.sock")
	svc, chownedTo := newGatewaySupervisor(t, socketPath, 1025)

	const uid = 1000
	_, err := svc.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		DefaultExecUid: uid,
	}))
	if err != nil {
		t.Fatalf("Provision with gateway port: %v", err)
	}
	svc.mu.Lock()
	state := svc.state
	closer := svc.gatewayProxyCloser
	svc.mu.Unlock()
	if state != stateProvisioned {
		t.Fatalf("state after Provision = %d, want provisioned", state)
	}
	if closer == nil {
		t.Fatal("Provision did not record a gateway proxy closer")
	}
	t.Cleanup(func() { _ = closer.Close() })

	fi, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat proxy socket: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("proxy socket mode = %o, want 0600", fi.Mode().Perm())
	}
	// The forwarder requested the chown to the session's default_exec_uid — the
	// owner posture (§(d)). The suite runs non-root, so the real os.Chown is
	// stubbed by newGatewaySupervisor; the requested uid is the assertable
	// contract, and the real chown-to-uid is proven on real boot.
	if *chownedTo != uid {
		t.Fatalf("forwarder chowned socket to uid %d, want %d", *chownedTo, uid)
	}
}

func TestProvisionGatewayListenFailureKeepsGateClosed(t *testing.T) {
	dir := t.TempDir()
	// Pre-occupy the socket path with a directory: net.Listen("unix", …) cannot
	// bind over a directory, and os.Remove of a non-empty dir also fails, so the
	// listen step fails deterministically without a race.
	socketPath := filepath.Join(dir, "agent.sock")
	if err := os.Mkdir(socketPath, 0o755); err != nil {
		t.Fatalf("pre-occupy socket path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(socketPath, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed occupying dir: %v", err)
	}
	svc, _ := newGatewaySupervisor(t, socketPath, 1025)

	_, err := svc.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
	}))
	if err == nil {
		t.Fatal("Provision with a pre-occupied socket path returned nil, want CodeInternal")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("Provision listen failure = %v, want CodeInternal", connect.CodeOf(err))
	}
	svc.mu.Lock()
	state := svc.state
	closer := svc.gatewayProxyCloser
	svc.mu.Unlock()
	if state != stateReady {
		t.Fatalf("state after failed Provision = %d, want stateReady (gate closed)", state)
	}
	if closer != nil {
		t.Fatal("failed Provision recorded a proxy closer; want none")
	}
}

func TestProvisionNoGatewayPortStartsNoProxy(t *testing.T) {
	svc := &supervisor{
		version:       "v-test",
		newCredential: testCredential,
		serveCtx:      t.Context(),
		state:         stateReady,
		execs:         map[string]*childExec{},
	}
	_, err := svc.Provision(t.Context(), connect.NewRequest(&compassv1internal.ProvisionRequest{
		DefaultExecUid: 1000,
	}))
	if err != nil {
		t.Fatalf("Provision with no gateway port: %v", err)
	}
	svc.mu.Lock()
	state := svc.state
	closer := svc.gatewayProxyCloser
	svc.mu.Unlock()
	if state != stateProvisioned {
		t.Fatalf("state after Provision = %d, want provisioned", state)
	}
	if closer != nil {
		t.Fatal("Provision with no gateway port started a proxy; want none")
	}
}

// TestGatewayProxyHalfCloseOnEOF asserts the §(d) half-close-on-either-EOF
// contract on an upstream conn that actually implements CloseWrite — a real
// AF_UNIX socketpair, unlike the net.Pipe the other forward tests use, whose
// end has no CloseWrite so halfCloseWrite silently no-ops. When the client
// finishes writing and half-closes its write direction, the upstream read side
// must observe EOF while the reverse (upstream->client) direction still
// delivers its response. A regression turning halfCloseWrite into a full Close
// would tear the whole connection down and fail the reverse-direction read.
func TestGatewayProxyHalfCloseOnEOF(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "agent.sock")
	dial, servers := dialFromUnixSocket(t)
	svc := &supervisor{dialGateway: dial, chownSocket: noopChown}

	closer, err := svc.startGatewayProxy(t.Context(), socketPath, uint32(os.Getuid()), 1025)
	if err != nil {
		t.Fatalf("startGatewayProxy: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	client := dialClientSocket(t, socketPath)
	defer func() { _ = client.Close() }()
	unixClient, ok := client.(*net.UnixConn)
	if !ok {
		t.Fatalf("proxy client conn is %T, want *net.UnixConn (needed for CloseWrite)", client)
	}

	var upstream net.Conn
	select {
	case upstream = <-servers:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy never dialed the gateway seam")
	}
	defer func() { _ = upstream.Close() }()

	// client -> upstream, then the client half-closes only its write direction.
	if _, err := client.Write([]byte("req")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(upstream, got); err != nil {
		t.Fatalf("upstream read request: %v", err)
	}
	if string(got) != "req" {
		t.Fatalf("upstream got %q, want req", got)
	}
	if err := unixClient.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}

	// The proxy propagates the client's write-EOF to upstream as a half-close,
	// so upstream's read side observes EOF — not a full connection reset.
	_ = upstream.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := upstream.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("upstream read after client half-close = %v, want io.EOF", err)
	}
	_ = upstream.SetReadDeadline(time.Time{})

	// The reverse direction is still open: upstream -> client still delivers,
	// proving the half-close did not tear the whole connection down.
	if _, err := upstream.Write([]byte("resp")); err != nil {
		t.Fatalf("upstream write after half-close: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, 4)
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatalf("client read response after half-close: %v", err)
	}
	if string(resp) != "resp" {
		t.Fatalf("client got %q, want resp", resp)
	}
}
