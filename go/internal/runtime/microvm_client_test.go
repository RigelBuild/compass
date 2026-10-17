//go:build unix

package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

func TestMicroVMConfigGuestClientInterceptors(t *testing.T) {
	var requests atomic.Int32
	handler := &runtimeHealthHandler{}
	socket := serveRuntimeGuest(t, handler)
	interceptor := connect.ClientInterceptor(func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			requests.Add(1)
			return next(ctx, spec)
		}
	})
	m := NewMicroVMRuntime(MicroVMConfig{
		RunRoot:                 t.TempDir(),
		GuestClientInterceptors: []connect.ClientInterceptor{interceptor},
	})

	client := m.newGuestClient(socket, 1024)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.Health(ctx, &compassv1.HealthRequest{}); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("interceptor calls = %d, want 1", got)
	}
	if got := handler.calls.Load(); got != 1 {
		t.Errorf("guest requests = %d, want 1", got)
	}
}

type runtimeHealthHandler struct {
	compassv1internalconnect.UnimplementedGuestControlHandler
	calls atomic.Int32
}

func (h *runtimeHealthHandler) Health(context.Context, *compassv1.HealthRequest) (*compassv1.HealthResponse, error) {
	h.calls.Add(1)
	return &compassv1.HealthResponse{GuestdVersion: "test"}, nil
}

func serveRuntimeGuest(t *testing.T, handler compassv1internalconnect.GuestControlHandler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "vsock.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("binding guest socket: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing guest listener: %v", err)
		}
	})

	mux := http.NewServeMux()
	connectServer := connect.NewServer()
	compassv1internalconnect.RegisterGuestControlHandler(connectServer, handler)
	connecthttp.Mount(mux, connectServer)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Handler: mux, Protocols: protocols}
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("closing guest HTTP server: %v", err)
		}
	})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		served := false
		defer func() {
			if !served {
				_ = conn.Close() // unblocks the client after a failed preamble
			}
		}()
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Errorf("reading guest preamble: %v", err)
			return
		}
		if got, want := strings.TrimRight(line, "\r\n"), "CONNECT 1024"; got != want {
			t.Errorf("guest preamble = %q, want %q", got, want)
			return
		}
		if _, err := fmt.Fprint(conn, "OK 1024\n"); err != nil {
			t.Errorf("writing guest ack: %v", err)
			return
		}
		served = true
		serveErr := server.Serve(&runtimeOneConnListener{conn: conn})
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			t.Errorf("serving guest HTTP connection: %v", serveErr)
		}
	}()
	return socket
}

type runtimeOneConnListener struct {
	conn net.Conn
	used bool
}

func (l *runtimeOneConnListener) Accept() (net.Conn, error) {
	if l.used {
		return nil, net.ErrClosed
	}
	l.used = true
	return l.conn, nil
}

func (*runtimeOneConnListener) Close() error     { return nil }
func (l *runtimeOneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
