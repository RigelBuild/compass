//go:build unix

package bridge

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tlsStubServer starts an httptest TLS server (self-signed cert) running handler
// and returns it plus its cert as a PEM the client can pin as caPEM. The server
// is torn down via t.Cleanup.
func tlsStubServer(t *testing.T, handler http.HandlerFunc) (srv *httptest.Server, certPEM []byte) {
	t.Helper()
	srv = httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	leaf := srv.Certificate()
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	return srv, certPEM
}

// tlsTarget builds a TLS Target against srv, failing the test on constructor error.
func tlsTarget(t *testing.T, srv *httptest.Server, caPEM []byte) *Target {
	t.Helper()
	target, err := NewTLSTarget(srv.URL, caPEM)
	if err != nil {
		t.Fatalf("NewTLSTarget: %v", err)
	}
	return target
}

// (a) dial succeeds when the server's cert PEM is pinned as caPEM.
func TestTLSTargetDialTrustedCA(t *testing.T) {
	var protoMajor int
	srv, certPEM := tlsStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		protoMajor = r.ProtoMajor
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	frames := collect(ctx, tlsTarget(t, srv, certPEM), Call{Path: "/rpc"})

	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}
	if ef, ok := frames[0].(ErrorFrame); ok {
		t.Fatalf("dial failed with trusted CA: %s", ef.Message)
	}
	head, ok := frames[0].(HeadFrame)
	if !ok {
		t.Fatalf("frame[0] = %T, want HeadFrame", frames[0])
	}
	if head.Status != http.StatusOK {
		t.Errorf("head status = %d, want %d", head.Status, http.StatusOK)
	}
	if protoMajor != 2 {
		t.Errorf("negotiated HTTP/%d, want HTTP/2 (encrypted h2 over TLS)", protoMajor)
	}
}

// (a) dial fails with a tls.CertificateVerificationError when caPEM is empty
// (system roots do not trust the self-signed cert).
func TestTLSTargetDialUntrustedCA(t *testing.T) {
	srv, _ := tlsStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	frames := collect(ctx, tlsTarget(t, srv, nil), Call{Path: "/rpc"})

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 ErrorFrame: %#v", len(frames), frames)
	}
	ef, ok := frames[0].(ErrorFrame)
	if !ok {
		t.Fatalf("frame[0] = %T, want ErrorFrame", frames[0])
	}

	// The pump surfaces the transport error's string; drive the same dial
	// directly to assert the typed cause via errors.As.
	resp, err := tlsTarget(t, srv, nil).client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("dial error = %v (%T), want tls.CertificateVerificationError", err, err)
	}
	if ef.Message == "" {
		t.Error("ErrorFrame message empty")
	}
}

// NewTLSTarget fails closed when a non-empty caPEM yields no usable certificate:
// a garbage trust anchor is a config error, never a silent fall-through to the
// system roots.
func TestTLSTargetRejectsUnusableCA(t *testing.T) {
	for _, tc := range []struct {
		name  string
		caPEM []byte
	}{
		{"not pem", []byte("this is not a PEM block")},
		{"wrong block type", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("nope")})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := NewTLSTarget("https://example.invalid", tc.caPEM)
			if err == nil {
				t.Fatalf("NewTLSTarget with %s caPEM = nil error, want failure", tc.name)
			}
			if target != nil {
				t.Errorf("target = %v, want nil on error", target)
			}
		})
	}
}

// (b) every forwarded request carries exactly one Authorization: Bearer <token>
// header when the target is armed.
func TestTLSTargetArmedBearer(t *testing.T) {
	var gotAuth []string
	srv, certPEM := tlsStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Values("Authorization")
		w.WriteHeader(http.StatusOK)
	})

	target := tlsTarget(t, srv, certPEM)
	target.SetBearer("s3cr3t-token")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collect(ctx, target, Call{Path: "/rpc"})

	if len(gotAuth) != 1 {
		t.Fatalf("Authorization headers = %v, want exactly one", gotAuth)
	}
	if gotAuth[0] != "Bearer s3cr3t-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth[0], "Bearer s3cr3t-token")
	}
}

// (c) a caller-supplied authorization header is stripped/overwritten by the
// armed bearer (DL-107 shell-injection point).
func TestTLSTargetOverwritesCallerAuthorization(t *testing.T) {
	var gotAuth []string
	srv, certPEM := tlsStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Values("Authorization")
		w.WriteHeader(http.StatusOK)
	})

	target := tlsTarget(t, srv, certPEM)
	target.SetBearer("armed-token")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collect(ctx, target, Call{
		Path:    "/rpc",
		Headers: [][2]string{{"Authorization", "Bearer attacker-supplied"}},
	})

	if len(gotAuth) != 1 {
		t.Fatalf("Authorization headers = %v, want exactly one (armed overwrite)", gotAuth)
	}
	if gotAuth[0] != "Bearer armed-token" {
		t.Errorf("Authorization = %q, want the armed token to overwrite the caller's", gotAuth[0])
	}
}

// (d) an empty bearer injects nothing: no Authorization header reaches the
// server, and a caller-supplied one is left untouched (unarmed = no rewrite).
func TestTLSTargetEmptyBearerInjectsNothing(t *testing.T) {
	var authPresent bool
	srv, certPEM := tlsStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, authPresent = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	})

	target := tlsTarget(t, srv, certPEM)
	target.SetBearer("") // unarmed

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collect(ctx, target, Call{Path: "/rpc"})

	if authPresent {
		t.Error("Authorization header reached server with empty bearer, want none")
	}
}

// unarmed still strips a caller-supplied Authorization header unconditionally:
// the UI must never smuggle a bearer to the server, even when no shell bearer is
// armed (DL-107 defense-in-depth).
func TestTLSTargetUnarmedStripsCallerAuthorization(t *testing.T) {
	var authPresent bool
	srv, certPEM := tlsStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, authPresent = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	})

	target := tlsTarget(t, srv, certPEM)
	target.SetBearer("") // unarmed

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	collect(ctx, target, Call{
		Path:    "/rpc",
		Headers: [][2]string{{"Authorization", "Bearer attacker-supplied"}},
	})

	if authPresent {
		t.Error("caller-supplied Authorization reached server while unarmed, want stripped")
	}
}
