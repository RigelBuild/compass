//go:build unix

package server

// Handler-level contracts for WhoAmI, the identity-reflection RPC (service.go
// GetServerInfo neighbor). It reflects the caller's own account id resolved
// server-side from the credential (DL-111), so both branches turn on the caller
// the door's interceptors attach: an authenticated caller reflects its own id,
// and a missing caller fails closed with Unauthenticated. Both run in the
// default lane — no store is reached — driven end to end through a real connect
// client over in-process h2c (the shipped door's protocol).
import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
)

// TestWhoAmIReflectsAmbientCallerIdentity pins the authenticated branch: the
// ambient-identity interceptor attaches a caller (the socket/dev door shape),
// and WhoAmI reflects exactly that account id in account_id — never a
// client-supplied value. A regression that read the id from the request, or
// returned the wrong caller, reddens this.
func TestWhoAmIReflectsAmbientCallerIdentity(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	const account = "acct-whoami-42"
	svc := newService("test", bus, nil, nil, nil, nil, nil)
	// Ambient identity attaches account as the caller AFTER any gate — the same
	// interceptor the socket and dev doors mount (serve.go). Built inline (rather
	// than via the pgtest-lane newH2CTestServerWithInterceptors) so this stays in
	// the default test lane.
	path, handler := compassv1connect.NewCompassServiceHandler(svc, connect.WithInterceptors(auth.AmbientIdentity(account)))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	client := newH2CClient(t, srv.URL)

	resp, err := client.WhoAmI(context.Background(), connect.NewRequest(&compassv1.WhoAmIRequest{}))
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if got := resp.Msg.GetAccountId(); got != account {
		t.Fatalf("AccountId = %q, want %q (the ambient caller's own id)", got, account)
	}
}

// TestWhoAmIWithoutCallerIsUnauthenticated pins the fail-closed branch: the
// default h2c door mounts no interceptor, so no caller is attached and
// CallerFrom returns !ok. WhoAmI fails with CodeUnauthenticated rather than
// reflecting an empty identity — the door-wiring-bug guard. A regression that
// treated a missing caller as an empty-string account reddens this.
func TestWhoAmIWithoutCallerIsUnauthenticated(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("test", bus, nil, nil, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	_, err := client.WhoAmI(context.Background(), connect.NewRequest(&compassv1.WhoAmIRequest{}))
	if err == nil {
		t.Fatal("WhoAmI with no caller returned nil error, want CodeUnauthenticated")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("WhoAmI with no caller = %v, want CodeUnauthenticated", code)
	}
}
