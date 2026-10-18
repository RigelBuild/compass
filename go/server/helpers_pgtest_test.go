//go:build pgtest && unix

package server

// Test scaffolding used only by the store-gated serve tests (serve_pgtest_test.go,
// behind the `pgtest` tag): the socket-readiness gate and the two Unix-socket connect
// clients. Behind the same tag as their only callers so the default lane does not see
// them as unused. Built on the tag-neutral scaffolding in helpers_test.go.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

func clientContextWithHeaders(ctx context.Context, headers http.Header) context.Context {
	ctx, info := connect.NewClientContext(ctx)
	for name, values := range headers {
		for _, value := range values {
			info.RequestHeader().Add(name, value)
		}
	}
	return ctx
}

func clientContextWithBearer(ctx context.Context, bearer string) context.Context {
	if bearer == "" {
		return ctx
	}
	return clientContextWithHeaders(ctx, http.Header{"Authorization": {bearer}})
}

// Bounds socket readiness for race scheduling; a wedged Serve bind still fails.
const socketReadyTimeout = 60 * time.Second

// Waits for the Unix socket to accept a connection; bind precedes Accept.
func waitListening(t *testing.T, path string) {
	t.Helper()
	if err := waitConnectable(t.Context(), path, socketReadyTimeout, (&net.Dialer{}).DialContext); err != nil {
		t.Fatal(err)
	}
}

// Dial retries wait on a ticker so the binding goroutine gets CPU.
func waitConnectable(ctx context.Context, path string, budget time.Duration, dial func(context.Context, string, string) (net.Conn, error)) error {
	deadline := time.After(budget)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		conn, err := dial(ctx, "unix", path)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-deadline:
			return fmt.Errorf("socket %s never became connectable within %s", path, budget)
		case <-ctx.Done():
			return fmt.Errorf("waiting for socket %s: %w", path, ctx.Err())
		case <-tick.C:
		}
	}
}

// Routes every h2c request through the Unix socket.
func newUDSClient(t *testing.T, socketPath string) compassv1connect.CompassServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCompassServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: tr}, "http://unix", connecthttp.WithReadMaxBytes(0))))
}

func newUDSCommsClient(t *testing.T, socketPath string) compassv1connect.CommsServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCommsServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: tr}, "http://unix", connecthttp.WithReadMaxBytes(0))))
}

// newH2CTestServerWithInterceptors stands up CompassService on h2c with its
// production interceptor chain. It returns the base URL for the wire test.
func newH2CTestServerWithInterceptors(t *testing.T, svc *service, interceptors ...connect.ServerInterceptor) string {
	t.Helper()
	server := connect.NewServer(interceptors...)
	compassv1connect.RegisterCompassServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newSecretsH2CServer stands up SecretsService on h2c with its interceptor chain.
func newSecretsH2CServer(t *testing.T, svc compassv1connect.SecretsServiceHandler, interceptors ...connect.ServerInterceptor) string {
	t.Helper()
	server := connect.NewServer(interceptors...)
	compassv1connect.RegisterSecretsServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
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
	return compassv1connect.NewSecretsServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: tr}, baseURL, connecthttp.WithReadMaxBytes(0))))
}
