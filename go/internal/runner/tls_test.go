//go:build unix

package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/RigelBuild/compass/go/internal/certgen"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// writeCertFile writes a certgen keypair's CertPEM to a temp file and returns
// the path — the --ca file NewCATrustClient reads.
func writeCertFile(t *testing.T, certPEM []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatalf("writing ca cert file: %v", err)
	}
	return path
}

func TestNewCATrustClient(t *testing.T) {
	kp, err := certgen.Generate([]string{"127.0.0.1"}, 0)
	if err != nil {
		t.Fatalf("certgen.Generate: %v", err)
	}

	t.Run("nonexistent path errors on reading the certificate", func(t *testing.T) {
		_, err := NewCATrustClient(filepath.Join(t.TempDir(), "does-not-exist.crt"))
		if err == nil {
			t.Fatal("NewCATrustClient(missing) = nil error, want error")
		}
		if !strings.Contains(err.Error(), "reading --ca certificate") {
			t.Errorf("error = %q, want mention of reading the certificate", err)
		}
	})

	t.Run("file with no PEM certificate errors", func(t *testing.T) {
		junk := filepath.Join(t.TempDir(), "junk.crt")
		if err := os.WriteFile(junk, []byte("not a pem certificate at all\n"), 0o600); err != nil {
			t.Fatalf("write junk: %v", err)
		}
		_, err := NewCATrustClient(junk)
		if err == nil {
			t.Fatal("NewCATrustClient(junk) = nil error, want error")
		}
		if !strings.Contains(err.Error(), "no PEM certificate") {
			t.Errorf("error = %q, want mention of no PEM certificate", err)
		}
	})

	t.Run("valid certgen cert yields a non-nil client", func(t *testing.T) {
		path := writeCertFile(t, kp.CertPEM)
		client, err := NewCATrustClient(path)
		if err != nil {
			t.Fatalf("NewCATrustClient(valid) = %v, want nil error", err)
		}
		if client == nil {
			t.Fatal("NewCATrustClient(valid) returned a nil client")
		}
	})
}
func TestNewServerClient(t *testing.T) {
	t.Run("system roots retain server name and TLS minimum", func(t *testing.T) {
		client, err := NewServerClient("", "compass.example.test")
		if err != nil {
			t.Fatalf("NewServerClient without CA: %v", err)
		}
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
		}
		if transport.TLSClientConfig.RootCAs != nil {
			t.Errorf("RootCAs = %v, want nil for system roots", transport.TLSClientConfig.RootCAs)
		}
		if transport.TLSClientConfig.ServerName != "compass.example.test" {
			t.Errorf("ServerName = %q, want compass.example.test", transport.TLSClientConfig.ServerName)
		}
		if transport.TLSClientConfig.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %v, want TLS 1.3", transport.TLSClientConfig.MinVersion)
		}
	})

	t.Run("missing CA keeps NewCATrustClient error", func(t *testing.T) {
		_, err := NewServerClient(filepath.Join(t.TempDir(), "missing.pem"), "")
		if err == nil || !strings.Contains(err.Error(), "reading --ca certificate") {
			t.Fatalf("NewServerClient(missing CA) error = %v, want NewCATrustClient read error", err)
		}
	})

	t.Run("non-PEM CA keeps NewCATrustClient error", func(t *testing.T) {
		caPath := filepath.Join(t.TempDir(), "junk.pem")
		if err := os.WriteFile(caPath, []byte("not a PEM certificate\n"), 0o600); err != nil {
			t.Fatalf("write junk CA: %v", err)
		}
		_, err := NewServerClient(caPath, "")
		if err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
			t.Fatalf("NewServerClient(non-PEM CA) error = %v, want NewCATrustClient PEM error", err)
		}
	})
}

func TestServerNameOverride(t *testing.T) {
	serverKP, err := certgen.Generate([]string{"compass.example.test"}, 0)
	if err != nil {
		t.Fatalf("certgen.Generate(server): %v", err)
	}
	srv := startTLSRunnerServer(t, serverKP)
	caPath := writeCertFile(t, serverKP.CertPEM)

	t.Run("override enrolls against the certificate name", func(t *testing.T) {
		httpClient, err := NewServerClient(caPath, "compass.example.test")
		if err != nil {
			t.Fatalf("NewServerClient: %v", err)
		}
		client := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(
			connecthttp.NewTransport(httpClient, srv.URL, connecthttp.WithReadMaxBytes(0)),
		))
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"}); err != nil {
			t.Fatalf("Enroll with server-name override: %v", err)
		}
	})

	t.Run("empty override checks the dialed host", func(t *testing.T) {
		httpClient, err := NewServerClient(caPath, "")
		if err != nil {
			t.Fatalf("NewServerClient: %v", err)
		}
		client := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(
			connecthttp.NewTransport(httpClient, srv.URL, connecthttp.WithReadMaxBytes(0)),
		))
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err = client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
		if err == nil {
			t.Fatal("Enroll without server-name override = nil error, want hostname failure")
		}
		if _, ok := errors.AsType[x509.HostnameError](err); !ok {
			t.Fatalf("Enroll error = %v, want x509.HostnameError", err)
		}
	})
}

// enrollStub is a RunnerService handler that answers Enroll with a minimal
// EnrollResponse. Only the TLS handshake + the unary Enroll round-trip are under
// test, so every other RPC is left unimplemented.
type enrollStub struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
}

func (enrollStub) Enroll(_ context.Context, _ *compassv1internal.EnrollRequest) (*compassv1internal.EnrollResponse, error) {
	return &compassv1internal.EnrollResponse{Reattached: false}, nil
}

// startTLSRunnerServer stands up an httptest TLS server on 127.0.0.1 serving the
// RunnerService over the given certgen keypair (its own leaf cert). The cert's
// 127.0.0.1 IP SAN matches the loopback host httptest binds, so a client that
// trusts the cert completes the handshake.
func startTLSRunnerServer(t *testing.T, kp certgen.Keypair) *httptest.Server {
	t.Helper()
	cert, err := tls.X509KeyPair(kp.CertPEM, kp.KeyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	server := connect.NewServer()
	compassv1internalconnect.RegisterRunnerServiceHandler(server, enrollStub{})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(0))
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestCATrustEnrollEndToEnd is the load-bearing model-(a) test: one certgen cert
// is both the server's leaf and the runner's trust anchor. A runner whose --ca
// is that cert completes the TLS handshake and enrolls; a runner trusting a
// DIFFERENT cert is rejected at the handshake.
func TestCATrustEnrollEndToEnd(t *testing.T) {
	serverKP, err := certgen.Generate([]string{"127.0.0.1", "::1", "localhost"}, 0)
	if err != nil {
		t.Fatalf("certgen.Generate(server): %v", err)
	}
	srv := startTLSRunnerServer(t, serverKP)

	t.Run("client trusting the server cert enrolls", func(t *testing.T) {
		caPath := writeCertFile(t, serverKP.CertPEM)
		httpClient, err := NewCATrustClient(caPath)
		if err != nil {
			t.Fatalf("NewCATrustClient: %v", err)
		}
		client := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, srv.URL, connecthttp.WithReadMaxBytes(0))))
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		resp, err := client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
		if err != nil {
			t.Fatalf("Enroll over trusted TLS = %v, want success", err)
		}
		if resp.GetReattached() {
			t.Errorf("Reattached = true, want false (stub returns a fresh enrollment)")
		}
	})

	t.Run("client trusting a different cert is rejected at TLS", func(t *testing.T) {
		// A distinct certgen cert: same SANs, different key — so the server's
		// leaf does not chain to this runner's trust anchor.
		otherKP, err := certgen.Generate([]string{"127.0.0.1", "::1", "localhost"}, 0)
		if err != nil {
			t.Fatalf("certgen.Generate(other): %v", err)
		}
		caPath := writeCertFile(t, otherKP.CertPEM)
		httpClient, err := NewCATrustClient(caPath)
		if err != nil {
			t.Fatalf("NewCATrustClient: %v", err)
		}
		client := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, srv.URL, connecthttp.WithReadMaxBytes(0))))
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err = client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
		if err == nil {
			t.Fatal("Enroll against an untrusted server cert = nil error, want a TLS trust failure")
		}
		// Prove the rejection is specifically an unknown-authority failure — the
		// server leaf does not chain to the runner's trust anchor — not merely
		// "some TLS error". Connect wraps the transport error, so try to unwrap to
		// the x509 sentinel; if the wrapping chain preserves it, assert exactly
		// that. Fall back to the substring check only when the chain has flattened
		// the error to a string (so the test still means something either way).
		if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
			// Exactly the failure we want: the anchor does not vouch for the leaf.
		} else if _, ok := errors.AsType[x509.HostnameError](err); ok {
			t.Errorf("rejected on hostname, not authority: %v — SANs should match; only the anchor differs", err)
		} else {
			msg := strings.ToLower(err.Error())
			if !strings.Contains(msg, "certificate") &&
				!strings.Contains(msg, "authority") &&
				!strings.Contains(msg, "verify") {
				t.Errorf("error = %q, want a certificate-verification/TLS failure", err)
			}
		}
	})
}
