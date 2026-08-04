//go:build unix

package adapters

import (
	"context"
	"net"
	"net/http"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/stack"
)

// HealthProber is the real stack.HealthProber: it issues GetServerInfo over the
// server's unix socket via the generated compass.v1 client. A nil error means
// the server answered — the readiness signal, since the socket binds before
// migrations complete — and the response version feeds the attach version check.
type HealthProber struct{}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.HealthProber = (*HealthProber)(nil)

// NewHealthProber builds a HealthProber.
func NewHealthProber() *HealthProber {
	return &HealthProber{}
}

// Probe dials the unix socket at socketPath over prior-knowledge cleartext
// HTTP/2 (the same door compass-server serves) and calls GetServerInfo. On
// success it returns the server's version; on any dial or RPC error it returns
// the zero ServerInfo and the error, which the core's readiness poll reads as
// "not yet answering".
func (p *HealthProber) Probe(ctx context.Context, socketPath string) (stack.ServerInfo, error) {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()

	client := compassv1connect.NewCompassServiceClient(&http.Client{Transport: transport}, "http://unix")
	resp, err := client.GetServerInfo(ctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if err != nil {
		return stack.ServerInfo{}, err
	}
	return stack.ServerInfo{Version: resp.Msg.GetVersion()}, nil
}
