//go:build pgtest && unix

package server

// Test scaffolding used only by the store-gated serve tests (serve_pgtest_test.go,
// behind the `pgtest` tag): the socket-readiness gate and the two Unix-socket
// connect clients. These live behind the same tag as their only callers so the
// default `go test ./...` lane — which does not compile serve_pgtest_test.go —
// does not see them as unused (golangci-lint runs on the default build tags).
// They build on the tag-neutral scaffolding in helpers_test.go (h2cTransport,
// timeAfter), which the default lane's service tests also use.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
)

// socketReadyTimeout bounds the wait for Serve to bind its Unix socket. It is
// deliberately separate from — and larger than — testTimeout (the RPC/stream
// safety net, helpers_test.go): under full-suite -race load the Serve goroutine
// can be slow to get scheduled and bind, and a readiness wait sharing the 15s
// RPC budget false-fails with "socket never became connectable" on healthy code
// that is merely under load. This budget fails only a genuinely wedged bind.
const socketReadyTimeout = 60 * time.Second

// waitListening blocks until the Unix socket at path is connectable, or fails
// the test at socketReadyTimeout. net.Listen makes a stream socket connectable
// the instant it returns (the kernel accepts into the backlog before the server
// calls Accept), so this gates precisely on "Serve has bound" — a monotonic,
// deterministic readiness signal, not a flaky retry: it advances the moment the
// socket is bound and fails loud if it never binds.
//
// The poll is ticker-gated, not a busy-spin: each miss blocks on <-tick.C,
// yielding the CPU to the Serve goroutine this wait depends on. A tight
// runtime.Gosched loop here instead starves that goroutine under full-suite
// -race load — N parallel readiness loops hot-spinning across the runner's cores
// keep the bind goroutine off-CPU, so readiness overshoots its budget on healthy
// code (the load-dependent flake this replaces). <-tick.C is a bounded poll, not
// a wall-clock sleep: it never encodes a timing assumption about when the socket
// binds, only how often to re-probe.
func waitListening(t *testing.T, path string) {
	t.Helper()
	deadline := time.After(socketReadyTimeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			return
		}
		select {
		case <-deadline:
			t.Fatalf("socket %s never became connectable within %s", path, socketReadyTimeout)
		case <-tick.C:
		}
	}
}

// newUDSClient builds a connect client that speaks h2c over the Unix socket at
// socketPath, ignoring the URL host (the dialer routes every dial to the
// socket).
func newUDSClient(t *testing.T, socketPath string) compassv1connect.CompassServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCompassServiceClient(&http.Client{Transport: tr}, "http://unix")
}

// newUDSCommsClient builds a connect client for the CommsService that speaks
// h2c over the Unix socket at socketPath, ignoring the URL host (the dialer
// routes every dial to the socket). The server mounts both the CompassService
// and CommsService handlers on the one socket door, so this reaches the comms
// RPCs — including the SubscribeComms stream the shutdown-with-subscriber
// regression holds open — over the same transport newUDSClient uses.
func newUDSCommsClient(t *testing.T, socketPath string) compassv1connect.CommsServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCommsServiceClient(&http.Client{Transport: tr}, "http://unix")
}

// newH2CTestServerWithInterceptors stands up the compass.v1 handler on an h2c
// httptest server with a production interceptor chain mounted — the network
// door's bearer + admin-gate chain, driven over a real connect client so the
// AdminGate reads a populated Spec().Procedure (the integration seam a
// hand-built unary request cannot exercise). Returns the base URL; torn down via
// t.Cleanup. Lives in the pgtest lane because its only caller is the store-backed
// network_door_test.go; it builds on the tag-neutral cleartextHTTP2 helper.
func newH2CTestServerWithInterceptors(t *testing.T, svc *service, interceptors ...connect.Interceptor) string {
	t.Helper()
	path, handler := compassv1connect.NewCompassServiceHandler(svc, connect.WithInterceptors(interceptors...))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newSecretsH2CServer stands up the SecretsService handler on an h2c httptest
// server with the given interceptor chain (the network door's bearer + admin-gate
// chain), driven over a real connect client so the handler reads a populated
// caller identity the shipped door supplies. Returns the base URL; torn down via
// t.Cleanup.
func newSecretsH2CServer(t *testing.T, svc compassv1connect.SecretsServiceHandler, interceptors ...connect.Interceptor) string {
	t.Helper()
	path, handler := compassv1connect.NewSecretsServiceHandler(svc, connect.WithInterceptors(interceptors...))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newSecretsH2CClient builds a SecretsService connect client speaking h2c
// prior-knowledge to baseURL (a TCP address). Idle conns are closed via t.Cleanup.
func newSecretsH2CClient(t *testing.T, baseURL string) compassv1connect.SecretsServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewSecretsServiceClient(&http.Client{Transport: tr}, baseURL)
}
