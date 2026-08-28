//go:build unix && gtk4

package main

// Native-client launch gate. runClient builds the ONE TLS-anchored bridge target
// and shares it across the pump and the bridge service (design §T5.6); the
// startup-JS injection and the state-dir resolver are the shell's other
// client-launch seams. Embedded mode was retired in RIG-2554, so there is no
// stack pipeline to exercise here.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
)

// clientConfig is a representative valid native-client config. ServerURL is an
// absolute https URL (validated upstream by appconfig); runClient never dials it.
var clientConfig = appconfig.Config{
	Mode:      appconfig.ModeClient,
	ServerURL: "https://remote.example:8443",
}

// TestRunClientBuildsServiceWithoutPipelineEffect: a valid client config yields a
// bridge service with a non-nil TLS target + tokenstore and an empty accountID.
// The assertion is structural: the service it returns is wired for Connect
// (target + tokens non-nil). No network dial.
func TestRunClientBuildsServiceWithoutPipelineEffect(t *testing.T) {
	svc, err := runClient(clientConfig, t.TempDir())
	if err != nil {
		t.Fatalf("runClient err = %v, want nil for a valid client config", err)
	}
	if svc.target == nil {
		t.Error("client service target is nil, want a TLS target wired for Connect")
	}
	if svc.tokens == nil {
		t.Error("client service tokenstore is nil, want one wired for Connect")
	}
	if svc.accountID != "" {
		t.Errorf("client service accountID = %q, want empty (identity is UI-resolved, OQ-7)", svc.accountID)
	}
}

// TestRunClientBadCACertNamesThePath: an unreadable ca_cert path aborts launch
// with a legible error that NAMES the path (not a panic, not a bare os error).
func TestRunClientBadCACertNamesThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")
	cfg := clientConfig
	cfg.CACert = missing

	svc, err := runClient(cfg, t.TempDir())
	if err == nil {
		t.Fatal("runClient with a missing ca_cert returned nil error, want a legible read error")
	}
	if svc != nil {
		t.Error("runClient returned a non-nil service alongside an error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("CA read error %q does not name the ca_cert path %q", err.Error(), missing)
	}
}

// TestRunClientBadCAPEMNamesTheCause: a ca_cert file that is not a usable PEM
// aborts launch with a legible error surfacing the TLS-target cause (NewTLSTarget
// rejects a PEM yielding no certificate).
func TestRunClientBadCAPEMNamesTheCause(t *testing.T) {
	badPEM := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPEM, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("writing bad PEM fixture: %v", err)
	}
	cfg := clientConfig
	cfg.CACert = badPEM

	svc, err := runClient(cfg, t.TempDir())
	if err == nil {
		t.Fatal("runClient with an unusable CA PEM returned nil error, want a legible target error")
	}
	if svc != nil {
		t.Error("runClient returned a non-nil service alongside an error")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("CA PEM error %q does not surface the no-usable-certificate cause", err.Error())
	}
}

// TestRunClientSharesOneTargetAcrossPumpAndService is the load-bearing wiring
// invariant (design §T5.6): the client pump and the bridge service MUST share ONE
// *bridge.Target, or a bearer armed on the service's target (Connect →
// SetBearer) never reaches the pump's forwarded requests (CompassRPC → pump →
// target.client.Do).
//
// The assertion is behavioral, not a private-field pointer peek: arm a bearer on
// the service's target, then run a real gRPC-Web forward through the service's
// pump against an in-process TLS server that records the Authorization header it
// receives. If the pump forwarded through the SAME target, the armed bearer
// arrives; if runClient had built two targets, the pump's target would be unarmed
// and no bearer would arrive. Non-vacuous: mutating runClient to pass a second
// bridge.NewTLSTarget(...) to NewPump reddens this (the recorded header is empty).
func TestRunClientSharesOneTargetAcrossPumpAndService(t *testing.T) {
	const bearer = "arm-me-9f3c"

	gotAuth := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotAuth <- r.Header.Get("Authorization"):
		default:
		}
		// Minimal well-formed response so the pump emits head→end, not an error
		// frame; the body is irrelevant to the header assertion.
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	caPEM := pemEncodeCert(t, srv.Certificate())
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("writing CA fixture: %v", err)
	}
	cfg := appconfig.Config{Mode: appconfig.ModeClient, ServerURL: srv.URL, CACert: caFile}

	svc, err := runClient(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("runClient err = %v, want nil", err)
	}

	// Arm the bearer on the SERVICE's target (the point Connect arms it).
	svc.target.SetBearer(bearer)

	// Forward one call through the SERVICE's pump; block until the terminal
	// frame so the request has completed (no time.Sleep — the pump's Do is
	// synchronous and signals completion by emitting a terminal frame).
	done := make(chan struct{})
	svc.pump.Do(context.Background(), bridge.Call{Path: "/probe"}, func(f bridge.Frame) {
		switch f.(type) {
		case bridge.EndFrame, bridge.ErrorFrame:
			close(done)
		default:
		}
	})
	<-done

	select {
	case auth := <-gotAuth:
		if auth != "Bearer "+bearer {
			t.Errorf("pump forwarded Authorization = %q, want %q — the pump's target is NOT the service's armed target",
				auth, "Bearer "+bearer)
		}
	default:
		t.Fatal("server received no request through the pump")
	}
}

// pemEncodeCert PEM-encodes a certificate for use as a CA anchor fixture.
func pemEncodeCert(t *testing.T, cert *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// TestShellStartupJS covers the OQ-8 startup-global injection: the client mode
// token, __COMPASS_SERVER_URL__ present in client mode, and JSON-escaping of a
// hostile server URL so it cannot break out of the script. Client is now the
// only mode (embedded was retired in RIG-2554).
func TestShellStartupJS(t *testing.T) {
	t.Run("client injects mode and server url", func(t *testing.T) {
		js, err := shellStartupJS("https://remote.example:8443")
		if err != nil {
			t.Fatalf("shellStartupJS err = %v, want nil", err)
		}
		if !strings.Contains(js, `window.__COMPASS_MODE__="client";`) {
			t.Errorf("client JS = %q, want the client mode global", js)
		}
		if !strings.Contains(js, `window.__COMPASS_SERVER_URL__="https://remote.example:8443";`) {
			t.Errorf("client JS = %q, want the server-url global", js)
		}
	})

	t.Run("hostile server url is JSON-escaped, not a breakout", func(t *testing.T) {
		hostile := `https://x/"+alert(1)+"</script><script>`
		js, err := shellStartupJS(hostile)
		if err != nil {
			t.Fatalf("shellStartupJS err = %v, want nil", err)
		}

		// The raw hostile string must NOT appear verbatim — the double-quote and
		// the </script> would break out of the script if concatenated unescaped.
		if strings.Contains(js, hostile) {
			t.Fatalf("JS contains the raw hostile URL verbatim (breakout): %q", js)
		}
		// The value must be a valid JS/JSON string literal: parse back the
		// assigned literal and confirm it round-trips to the original URL.
		assign := "window.__COMPASS_SERVER_URL__="
		i := strings.Index(js, assign)
		if i < 0 {
			t.Fatalf("JS missing the server-url assignment: %q", js)
		}
		literal := strings.TrimSuffix(js[i+len(assign):], ";")
		var decoded string
		if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
			t.Fatalf("server-url literal %q is not a valid JSON string: %v", literal, err)
		}
		if decoded != hostile {
			t.Errorf("decoded server url = %q, want the original %q", decoded, hostile)
		}
	})
}

// TestResolveStateDir: flag wins, then $COMPASS_STATE_DIR, then an ABSOLUTE
// $XDG_STATE_HOME/compass. A RELATIVE $XDG_STATE_HOME is treated as unset and
// falls through to $HOME/.compass — the load-bearing determinism guard.
func TestResolveStateDir(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "/env/state")
		if got := resolveStateDir("/flag/state"); got != "/flag/state" {
			t.Errorf("got %q, want the flag value", got)
		}
	})
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "/env/state")
		t.Setenv("XDG_STATE_HOME", "/xdg/state")
		if got := resolveStateDir(""); got != "/env/state" {
			t.Errorf("got %q, want the env value", got)
		}
	})
	t.Run("absolute XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "")
		xdg := t.TempDir()
		t.Setenv("XDG_STATE_HOME", xdg)
		if got := resolveStateDir(""); got != filepath.Join(xdg, "compass") {
			t.Errorf("got %q, want %q", got, filepath.Join(xdg, "compass"))
		}
	})
	t.Run("relative XDG_STATE_HOME falls through to HOME/.compass", func(t *testing.T) {
		t.Setenv("COMPASS_STATE_DIR", "")
		t.Setenv("XDG_STATE_HOME", "rel/state")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if got := resolveStateDir(""); got != filepath.Join(home, ".compass") {
			t.Errorf("got %q, want %q (relative XDG_STATE_HOME must fall through)", got, filepath.Join(home, ".compass"))
		}
	})
}
