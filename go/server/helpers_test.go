//go:build unix

package server

// Shared test scaffolding for the server package tests: in-process h2c
// transports (the same cleartext-HTTP/2 door the server ships) so the
// SubscribeEvents / GetServerInfo handlers are exercised through a real
// connect-go client rather than called directly. White-box (package server)
// so tests can construct the unexported service and drive the socket-door
// helpers.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// testTimeout bounds every RPC/stream wait so a broken handler fails fast
// instead of hanging the suite. It is a deadline safety net, never a
// synchronization device: tests event-gate on stream receives and channel
// sends, not on elapsed time.
const testTimeout = 15 * time.Second

// timeAfter is the deadline channel every blocking test wait selects on. It is
// a safety net that turns a wedged handler into a fast failure, never a
// synchronization device.
func timeAfter() <-chan time.Time { return time.After(testTimeout) }

// newH2CTestServer stands up the compass.v1 handler on an httptest server that
// speaks cleartext HTTP/2 (h2c) — the shipped socket door's protocol, minus the
// Unix socket. It returns the base URL; the server is torn down via t.Cleanup.
func newH2CTestServer(t *testing.T, svc *service) string {
	t.Helper()
	path, handler := compassv1connect.NewCompassServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newH2CClient builds a connect client that speaks h2c prior-knowledge to
// baseURL (a TCP address). Idle conns are closed via t.Cleanup.
func newH2CClient(t *testing.T, baseURL string) compassv1connect.CompassServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCompassServiceClient(&http.Client{Transport: tr}, baseURL)
}

// h2cTransport is a stdlib HTTP transport that speaks prior-knowledge cleartext
// HTTP/2 (h2c) and routes every dial through the supplied plaintext dialer — the
// native client shape (http.Protocols with UnencryptedHTTP2 only), matching the
// server's own cleartext-HTTP/2 door.
func h2cTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Protocols:   p,
		DialContext: dial,
	}
}
