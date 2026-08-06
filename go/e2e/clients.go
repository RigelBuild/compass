//go:build podman

// Package e2e is the dogfood end-to-end harness substrate: it stands up the real
// embedded Compass stack over stack.Up (the same composition the compass-stack
// CLI drives) and hands the harness's later legs (SEA-1785 H2-H6) a Fixture with
// authenticated Connect clients and the store DSN. It is podman-build-tagged —
// it drives real child processes and the real agent image, so it is out of the
// hermetic unit lane and runs only under `-tags podman`.
package e2e

import (
	"context"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/runner"
)

// compassServiceClient and commsServiceClient are short aliases for the two
// generated Connect client interfaces the Fixture exposes, so its accessors read
// cleanly without repeating the fully-qualified generated names.
type (
	compassServiceClient = compassv1connect.CompassServiceClient
	commsServiceClient   = compassv1connect.CommsServiceClient
)

// bearerToken stamps the admin bearer credential on every outbound RPC (unary
// and streaming) so the Server's network door authenticates it. It mirrors the
// CLI's client interceptor (cmd/compass/client.go:28-53); replicated rather than
// imported because that one lives in package main. Streaming is stamped too so a
// future streaming RPC cannot silently go out unauthenticated.
type bearerToken struct {
	token string
}

func (b *bearerToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b *bearerToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b *bearerToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// newAuthedClients dials the stack's loopback TLS network door with the state
// dir's self-signed anchor as the trust root and the admin bearer on every call,
// returning the two authenticated Connect clients the harness consumes. caPath
// anchors the self-signed dogfood cert (the server serves https on the door, so
// a CA-trust client is required, never plain http); serverURL is the https door
// URL; adminToken is the bootstrap-admin bearer read from disk. Both services
// live in the same generated compassv1connect package.
func newAuthedClients(caPath, serverURL, adminToken string) (compassv1connect.CompassServiceClient, compassv1connect.CommsServiceClient, error) {
	httpClient, err := runner.NewCATrustClient(caPath)
	if err != nil {
		return nil, nil, err
	}
	ic := connect.WithInterceptors(&bearerToken{token: adminToken})
	compass := compassv1connect.NewCompassServiceClient(httpClient, serverURL, ic)
	comms := compassv1connect.NewCommsServiceClient(httpClient, serverURL, ic)
	return compass, comms, nil
}

// newUnauthedCompassClient dials the SAME TLS door with the trust anchor but NO
// bearer interceptor, so its calls carry no credential. It exists solely for the
// negative auth assertion: an RPC over it must be rejected Unauthenticated,
// proving the door actually enforces auth rather than incidentally passing.
func newUnauthedCompassClient(caPath, serverURL string) (compassv1connect.CompassServiceClient, error) {
	httpClient, err := runner.NewCATrustClient(caPath)
	if err != nil {
		return nil, err
	}
	return compassv1connect.NewCompassServiceClient(httpClient, serverURL), nil
}
