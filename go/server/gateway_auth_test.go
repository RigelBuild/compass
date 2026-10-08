//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

type fakeGatewayVerifier map[string]store.GatewayCaller

var errStoreDown = errors.New("store down")

func (f fakeGatewayVerifier) Verify(_ context.Context, token string) (store.GatewayCaller, error) {
	if token == "store-fault" {
		return store.GatewayCaller{}, fmt.Errorf("verifying gateway token: %w", errStoreDown)
	}
	caller, ok := f[token]
	if !ok {
		return store.GatewayCaller{}, auth.ErrGatewayTokenInvalid
	}
	return caller, nil
}

func newGatewayAuthClient(t *testing.T, v gatewayTokenVerifier) compassv1internalconnect.GatewayAuthClient {
	t.Helper()
	path, handler := compassv1internalconnect.NewGatewayAuthHandler(newGatewayAuthService(v))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1internalconnect.NewGatewayAuthClient(&http.Client{Transport: tr}, srv.URL)
}

func TestVerifyAgentTokenResolvesCaller(t *testing.T) {
	client := newGatewayAuthClient(t, fakeGatewayVerifier{
		"tok-a": {AgentAccountID: "agent-a", OwnerUserID: "owner-a"},
	})
	resp, err := client.VerifyAgentToken(t.Context(), connect.NewRequest(&compassv1internal.VerifyAgentTokenRequest{Token: "tok-a"}))
	if err != nil {
		t.Fatalf("VerifyAgentToken: %v", err)
	}
	if resp.Msg.GetAgentAccountId() != "agent-a" || resp.Msg.GetOwnerUserId() != "owner-a" {
		t.Fatalf("response = %v, want agent-a/owner-a", resp.Msg)
	}
}

// Any invalid token is UNAUTHENTICATED with one fixed message.
func TestVerifyAgentTokenInvalidIsUnauthenticated(t *testing.T) {
	client := newGatewayAuthClient(t, fakeGatewayVerifier{})
	var first string
	for _, token := range []string{"unknown", ""} {
		_, err := client.VerifyAgentToken(t.Context(), connect.NewRequest(&compassv1internal.VerifyAgentTokenRequest{Token: token}))
		if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
			t.Fatalf("token %q: code = %v, want Unauthenticated (err %v)", token, code, err)
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("error text differs: %q vs %q", err.Error(), first)
		}
	}
}

// A store fault is not reported as a bad token.
func TestVerifyAgentTokenStoreFaultIsInternal(t *testing.T) {
	client := newGatewayAuthClient(t, fakeGatewayVerifier{})
	_, err := client.VerifyAgentToken(t.Context(), connect.NewRequest(&compassv1internal.VerifyAgentTokenRequest{Token: "store-fault"}))
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err %v)", code, err)
	}
}
