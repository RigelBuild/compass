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
	"runtime"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
)

// waitListening blocks until the Unix socket at path is connectable, or fails
// the test at the deadline. net.Listen makes a stream socket connectable the
// instant it returns (the kernel accepts into the backlog before the server
// calls Accept), so this gates precisely on "Serve has bound" — a monotonic,
// deterministic readiness signal, not a flaky retry: it advances the moment the
// socket is bound and fails loud if it never binds. runtime.Gosched yields to
// the Serve goroutine between probes; no wall-clock sleep is used for timing.
func waitListening(t *testing.T, path string) {
	t.Helper()
	deadline := timeAfter()
	for {
		select {
		case <-deadline:
			t.Fatalf("socket %s never became connectable", path)
		default:
		}
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			return
		}
		runtime.Gosched()
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
