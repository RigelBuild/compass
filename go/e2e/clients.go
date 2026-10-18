//go:build podman

// Package e2e is the end-to-end harness substrate: it stands up the real
// embedded Compass stack over stack.Up (the same composition the compass-stack
// CLI drives) and hands the harness's later legs (RIG-1785 H2-H6) a Fixture with
// authenticated Connect clients and the store DSN. It is podman-build-tagged —
// it drives real child processes and the real agent image, so it is out of the
// hermetic unit lane and runs only under `-tags podman`. Which tier to test at,
// and sharedFixture vs NewFixture: docs/architecture/testing-tiers.md.
package e2e

import (
	"context"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/runner"
)

// compassServiceClient and commsServiceClient are short aliases for the two
// generated Connect client interfaces the Fixture exposes, so its accessors read
// cleanly without repeating the fully-qualified generated names.
type (
	compassServiceClient = compassv1connect.CompassServiceClient
	commsServiceClient   = compassv1connect.CommsServiceClient
)

// bearerToken stamps the admin bearer credential on every outbound RPC.
func bearerToken(token string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			info, ok := connect.CallInfoForClientContext(ctx)
			if !ok {
				return nil, connect.NewError(connect.CodeInternal, "bearer token: no client call info")
			}
			info.RequestHeader().Set("Authorization", "Bearer "+token)
			return next(ctx, spec)
		}
	}
}

// newAuthedClients trusts the state directory's self-signed CA and adds the
// bootstrap-admin bearer to both generated Connect clients. The server door is
// HTTPS, so plain HTTP is not a valid alternative.
func newAuthedClients(caPath, serverURL, adminToken string) (compassv1connect.CompassServiceClient, compassv1connect.CommsServiceClient, error) {
	httpClient, err := runner.NewCATrustClient(caPath)
	if err != nil {
		return nil, nil, err
	}
	compass := compassv1connect.NewCompassServiceClient(connect.NewClient(
		connecthttp.NewTransport(httpClient, serverURL, connecthttp.WithReadMaxBytes(0)),
		bearerToken(adminToken),
	))
	comms := compassv1connect.NewCommsServiceClient(connect.NewClient(
		connecthttp.NewTransport(httpClient, serverURL, connecthttp.WithReadMaxBytes(0)),
		bearerToken(adminToken),
	))
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
	return compassv1connect.NewCompassServiceClient(connect.NewClient(
		connecthttp.NewTransport(httpClient, serverURL, connecthttp.WithReadMaxBytes(0)),
	)), nil
}
