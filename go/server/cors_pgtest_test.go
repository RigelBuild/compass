//go:build pgtest && unix

package server

// Integration test for the network door's CORS policy (SEA-1195 T3b), pinning
// the spec Requirement "The network door defaults closed to browser origins"
// (docs/specs/product/compass.md:693-707): unless --cors-allowed-origin names a
// single explicit browser origin the door applies NO CORS at all; when set it
// allows exactly that one origin (never a wildcard), never enables credentialed
// CORS, and does not reflect a preflight from any other origin.
//
// It drives the REAL wiring: buildNetworkServer constructs the door and decides
// whether to wrap the mux in networkCORS (network_door.go:197-202), so the test
// asserts against buildNetworkServer's returned handler rather than networkCORS
// in isolation — a dropped guard (always-on CORS) is only observable through the
// build step. No TLS termination or serving is needed: the CORS layer is an
// HTTP middleware in front of the mux, so the preflight is exercised in-process
// with an httptest recorder against the returned handler.
//
// Store-gated (pgtest lane): buildNetworkServer mints and writes the bootstrap
// admin token against the Postgres store of record, so it needs a real database
// via the shared harness (newNetworkStore → an isolated-schema DSN, or t.Skip
// when no runtime). Hermetic: the token file lives under t.TempDir().

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/comms"
)

// corsAllowOrigin is the response header the CORS middleware echoes the allowed
// origin into; corsAllowCredentials is the credentialed-CORS opt-in the door
// must never emit.
const (
	corsAllowOrigin      = "Access-Control-Allow-Origin"
	corsAllowMethods     = "Access-Control-Allow-Methods"
	corsAllowCredentials = "Access-Control-Allow-Credentials"
)

// buildDoorHandler stands up the authenticated network door via the production
// buildNetworkServer (the same call serve.go makes) with corsOrigin as
// --cors-allowed-origin, and returns its HTTP handler — the netRoot that is
// either the bare mux (corsOrigin == "") or the mux wrapped in networkCORS. A
// nil netTLS is fine: buildNetworkServer only stores it on the http.Server for
// later serving, and this test never serves — it invokes the handler directly.
func buildDoorHandler(t *testing.T, corsOrigin string) http.Handler {
	t.Helper()
	ctx := context.Background()
	st, admin, _ := newNetworkStore(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("cors-test", bus, st, nil, nil, nil)

	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, admin)

	cfg := ServeConfig{
		StateDir:          t.TempDir(), // bootstrap-admin token file lands here (0600)
		CORSAllowedOrigin: corsOrigin,
	}
	secretsSvc := newSecretsService(st, nil, nil)
	srv, err := buildNetworkServer(ctx, cfg, svc, commsSvc, secretsSvc, nil, st, admin, nil, nil)
	if err != nil {
		t.Fatalf("buildNetworkServer: %v", err)
	}
	return srv.Handler
}

// preflight sends a CORS preflight (OPTIONS carrying Origin +
// Access-Control-Request-Method, the two headers rs/cors keys the preflight
// path on) from origin to a real procedure path on handler and returns the
// recorded response headers.
func preflight(handler http.Handler, origin string) http.Header {
	req := httptest.NewRequest(http.MethodOptions, compassv1connect.CompassServiceGetServerInfoProcedure, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result().Header
}

// TestNetworkDoorCORSPolicy pins the four halves of the network-door CORS
// contract against the enforced spec Scenario. Each case defends one clause, so
// a distinct regression reddens a distinct case: a wildcard origin fails
// "configured origin echoed exactly", credentialed CORS fails the
// AllowCredentials clause, a reflected foreign origin fails "foreign origin not
// reflected", and a dropped always-on guard fails "empty config emits no CORS".
func TestNetworkDoorCORSPolicy(t *testing.T) {
	const configured = "https://app.example.com"
	const foreign = "https://evil.example.com"

	t.Run("configured origin preflight is answered with exactly that origin, no wildcard", func(t *testing.T) {
		h := buildDoorHandler(t, configured)
		got := preflight(h, configured)

		if origin := got.Get(corsAllowOrigin); origin != configured {
			t.Fatalf("Access-Control-Allow-Origin = %q, want exactly %q (never a wildcard)", origin, configured)
		}
		// Belt-and-braces against a wildcard regression that also happened to
		// echo the origin: the single-origin policy never emits "*".
		if origin := got.Get(corsAllowOrigin); origin == "*" {
			t.Fatal("Access-Control-Allow-Origin is the wildcard '*'; the network door must expose exactly one origin")
		}
		// A granted preflight names the requested method — proves the preflight
		// was actually handled (not silently dropped), so the origin assertion
		// above is not vacuously green on an unhandled request.
		if methods := got.Get(corsAllowMethods); methods == "" {
			t.Fatal("granted preflight set no Access-Control-Allow-Methods; the preflight was not handled")
		}
	})

	t.Run("credentialed CORS is never enabled", func(t *testing.T) {
		h := buildDoorHandler(t, configured)
		got := preflight(h, configured)

		if creds := got.Get(corsAllowCredentials); creds != "" {
			t.Fatalf("Access-Control-Allow-Credentials = %q, want unset (the door must not enable cookie CORS)", creds)
		}
	})

	t.Run("foreign origin preflight is not reflected", func(t *testing.T) {
		h := buildDoorHandler(t, configured)
		got := preflight(h, foreign)

		if origin := got.Get(corsAllowOrigin); origin != "" {
			t.Fatalf("foreign-origin preflight got Access-Control-Allow-Origin = %q, want none (must not reflect a foreign origin)", origin)
		}
	})

	t.Run("empty config adds no CORS headers at all", func(t *testing.T) {
		h := buildDoorHandler(t, "")
		got := preflight(h, configured)

		// The guard (network_door.go:198) leaves netRoot as the bare mux when
		// no origin is configured, so the CORS middleware is entirely absent.
		// The middleware's fingerprint is present on EVERY preflight it touches
		// (even a rejected one): it echoes the allowed origin, names the method,
		// and — because it always sets Vary — adds a Vary header. Asserting all
		// three are absent reddens a dropped guard (always-on CORS), which would
		// re-introduce at least the Vary header even for an unmatched origin.
		for _, header := range []string{corsAllowOrigin, corsAllowMethods, corsAllowCredentials, "Vary"} {
			if v := got.Get(header); v != "" {
				t.Fatalf("empty --cors-allowed-origin still set %s = %q; the network door must add no CORS headers when closed", header, v)
			}
		}
	})
}
