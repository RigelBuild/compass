//go:build pgtest && unix

package server

// P1 dev-door regression test (RIG-1195 T3b, security-critical) — pins the closed
// "Dev Door Can Mint Admin Tokens" hole. The --dev-http endpoint mounts
// CompassService behind AdminGate with NO bearer interceptor, so AdminGate
// fail-closes every adminOnly procedure (asserts IssueToken → PermissionDenied).

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
)

// devClients builds h2c CompassService and CommsService clients pointed at the
// dev-port TCP address (a plaintext loopback dev endpoint, so h2c — not TLS).
func devClients(t *testing.T, devAddr string) (compassv1connect.CompassServiceClient, compassv1connect.CommsServiceClient) {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	httpClient := &http.Client{Transport: tr}
	base := "http://" + devAddr
	return compassv1connect.NewCompassServiceClient(httpClient, base),
		compassv1connect.NewCommsServiceClient(httpClient, base)
}

// TestDevDoorGatesAdminOnlyRPCsWithoutBearer is the P1 regression: on the dev
// door an adminOnly RPC is denied (no bearer → no caller → fail-closed), while
// open CompassService and CommsService RPCs stay reachable for the dev client.
func TestDevDoorGatesAdminOnlyRPCsWithoutBearer(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	devAddr := freeLoopbackAddr(t)
	devAddrPort, err := netip.ParseAddrPort(devAddr)
	if err != nil {
		t.Fatalf("parsing dev loopback addr %q: %v", devAddr, err)
	}

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "dev-door-test",
		DevHTTP:     &devAddrPort,
	})
	// Serve binds the dev listener before the socket and serves both off the same
	// startup, so once the socket serves an RPC the dev port is bound and serving.
	waitServing(t, socketPath)

	compassClient, commsClient := devClients(t, devAddr)

	t.Run("IssueToken is PermissionDenied and mints no token", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		resp, err := compassClient.IssueToken(ctx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: "any-account"}))
		if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
			t.Fatalf("IssueToken on the dev door = %v, want CodePermissionDenied (no bearer → no caller → fail-closed)", code)
		}
		if resp != nil {
			t.Fatalf("IssueToken on the dev door returned a response %+v — a browser must NOT be able to mint an admin token here", resp.Msg)
		}
	})

	t.Run("SubscribeAgentSession reaches the authz gate under the dev-door ambient admin (NotFound not Unauthenticated)", func(t *testing.T) {
		// The dev door carries NO bearer, yet the ambient pair attaches
		// caller=bootstrap admin AFTER AdminGate. SubscribeAgentSession is
		// authenticatedOpen, so it passes the gate and proceeds PAST CallerFrom into
		// RequireAgentSessionSubscriber; the unknown id resolves to CodeNotFound.
		// Pre-fix (AdminGate only, no ambient) this returned CodeUnauthenticated.
		code := subscribeAgentSessionCode(t, compassClient, "no-such-session")
		if code == connect.CodeUnauthenticated {
			t.Fatalf("SubscribeAgentSession on the dev door = CodeUnauthenticated — the ambient caller is not attached; the ambient pair was dropped or ordered before AdminGate")
		}
		if code != connect.CodeNotFound {
			t.Fatalf("SubscribeAgentSession on the dev door for an unknown session = %v, want CodeNotFound (ambient admin reaches RequireAgentSessionSubscriber → unknown session)", code)
		}
	})

	t.Run("GetServerInfo is reachable", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		resp, err := compassClient.GetServerInfo(ctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
		if err != nil {
			t.Fatalf("GetServerInfo on the dev door (authenticatedOpen): %v", err)
		}
		if resp.Msg.GetVersion() != "dev-door-test" {
			t.Fatalf("Version = %q, want dev-door-test", resp.Msg.GetVersion())
		}
	})

	t.Run("CommsService ListAccounts is reachable", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		// The dev door serves comms under the ambient bootstrap admin, so this
		// read succeeds and returns at least the admin account (created at
		// startup). A gate wrongly applied to CommsService here would deny it.
		resp, err := commsClient.ListAccounts(ctx, connect.NewRequest(&compassv1.ListAccountsRequest{}))
		if err != nil {
			t.Fatalf("ListAccounts on the dev door: %v", err)
		}
		if len(resp.Msg.GetAccounts()) == 0 {
			t.Fatal("ListAccounts returned no accounts, want at least the bootstrap admin (ambient admin can read)")
		}
	})
}
