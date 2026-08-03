//go:build unix

package adapters

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/internal/certgen"
)

// writeAnchor generates an anchor with the given validity and writes it under
// dir, returning the resolved cert path.
func writeAnchor(t *testing.T, dir string, validity time.Duration) string {
	t.Helper()
	kp, err := certgen.Generate(certHosts, validity)
	if err != nil {
		t.Fatalf("certgen.Generate = %v", err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := kp.WriteFiles(certPath, keyPath); err != nil {
		t.Fatalf("WriteFiles = %v", err)
	}
	return certPath
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", path, err)
	}
	return b
}

func TestEnsureCertAbsentAnchorGenerates(t *testing.T) {
	dir := t.TempDir()
	e := NewCertEnsurer(DefaultRotateWindow)

	res, err := e.EnsureCert(t.Context(), dir, time.Now())
	if err != nil {
		t.Fatalf("EnsureCert = %v", err)
	}
	if !res.Rotated {
		t.Fatalf("Rotated = false, want true (absent anchor must generate)")
	}
	if res.CertPath != filepath.Join(dir, "tls.crt") || res.KeyPath != filepath.Join(dir, "tls.key") {
		t.Fatalf("paths = %q/%q, want tls.crt/tls.key under %q", res.CertPath, res.KeyPath, dir)
	}

	certInfo, err := os.Stat(res.CertPath)
	if err != nil {
		t.Fatalf("stat cert = %v", err)
	}
	if got := certInfo.Mode().Perm(); got != 0o644 {
		t.Fatalf("cert mode = %o, want 0644", got)
	}
	keyInfo, err := os.Stat(res.KeyPath)
	if err != nil {
		t.Fatalf("stat key = %v", err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key mode = %o, want 0600", got)
	}

	block, _ := pem.Decode(readFile(t, res.CertPath))
	if block == nil {
		t.Fatalf("generated cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("generated cert not parseable: %v", err)
	}
	var hasLoopback bool
	for _, ip := range cert.IPAddresses {
		if ip.String() == "127.0.0.1" {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Fatalf("generated cert missing 127.0.0.1 SAN, got IPs %v", cert.IPAddresses)
	}
}

func TestEnsureCertFreshAnchorUntouched(t *testing.T) {
	dir := t.TempDir()
	certPath := writeAnchor(t, dir, 365*24*time.Hour)
	before := readFile(t, certPath)

	e := NewCertEnsurer(30 * 24 * time.Hour)
	res, err := e.EnsureCert(t.Context(), dir, time.Now())
	if err != nil {
		t.Fatalf("EnsureCert = %v", err)
	}
	if res.Rotated {
		t.Fatalf("Rotated = true, want false (fresh anchor must not regenerate)")
	}
	if after := readFile(t, certPath); string(after) != string(before) {
		t.Fatalf("cert bytes changed on a fresh anchor")
	}
}

func TestEnsureCertNearExpiryRotates(t *testing.T) {
	dir := t.TempDir()
	// A 10-day cert; probing with a 30-day window puts NotAfter inside it.
	certPath := writeAnchor(t, dir, 10*24*time.Hour)
	before := readFile(t, certPath)

	e := NewCertEnsurer(30 * 24 * time.Hour)
	res, err := e.EnsureCert(t.Context(), dir, time.Now())
	if err != nil {
		t.Fatalf("EnsureCert = %v", err)
	}
	if !res.Rotated {
		t.Fatalf("Rotated = false, want true (near-expiry anchor must rotate)")
	}
	if after := readFile(t, certPath); string(after) == string(before) {
		t.Fatalf("cert bytes unchanged on a near-expiry anchor")
	}
}

func TestEnsureCertCorruptRotates(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, []byte("not a pem cert"), 0o644); err != nil {
		t.Fatalf("write corrupt cert = %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a pem key"), 0o600); err != nil {
		t.Fatalf("write corrupt key = %v", err)
	}

	e := NewCertEnsurer(30 * 24 * time.Hour)
	res, err := e.EnsureCert(t.Context(), dir, time.Now())
	if err != nil {
		t.Fatalf("EnsureCert on corrupt anchor = %v, want nil error", err)
	}
	if !res.Rotated {
		t.Fatalf("Rotated = false, want true (corrupt anchor must regenerate)")
	}
	block, _ := pem.Decode(readFile(t, res.CertPath))
	if block == nil {
		t.Fatalf("regenerated cert is not valid PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Fatalf("regenerated cert not parseable: %v", err)
	}
}
