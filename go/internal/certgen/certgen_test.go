//go:build unix

package certgen

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseCert decodes the single CERTIFICATE PEM block in certPEM and returns the
// parsed x509 certificate — the exact bytes a trust store and a TLS peer parse,
// so asserting on this is asserting on what the wire sees.
func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("CertPEM contained no PEM block")
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("CertPEM block type = %q, want CERTIFICATE", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing generated certificate: %v", err)
	}
	return cert
}

func TestGenerate(t *testing.T) {
	t.Run("SANs, IsCA, and algorithm", func(t *testing.T) {
		assertSANsIsCAAndAlgorithm(t)
	})

	t.Run("custom validity is honored", func(t *testing.T) {
		assertCustomValidityHonored(t)
	})

	t.Run("validity <= 0 uses DefaultValidity", func(t *testing.T) {
		assertNonPositiveValidityUsesDefault(t)
	})

	t.Run("empty hosts is an error", func(t *testing.T) {
		assertEmptyHostsIsError(t)
	})
}

// assertSANsIsCAAndAlgorithm verifies the shape of a cert generated from a mix of
// IP literals and a DNS name: it is a self-signed CA, uses ECDSA, binds IP
// literals as IP SANs and DNS names as DNS SANs (with no cross-classification),
// and has a validity window backdated to now and ~DefaultValidity ahead.
func assertSANsIsCAAndAlgorithm(t *testing.T) {
	t.Helper()
	before := time.Now()
	kp, err := Generate([]string{"127.0.0.1", "::1", "localhost"}, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert := parseCert(t, kp.CertPEM)

	if !cert.IsCA {
		t.Error("IsCA = false, want true (the self-signed cert is its own trust anchor)")
	}
	if !cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	// The cert is simultaneously the CA that signs itself (KeyUsageCertSign) and
	// the server leaf presented on the TLS handshake (ExtKeyUsageServerAuth) —
	// both are load-bearing for the one-cert model, so a regression that dropped
	// either would break signing or serverAuth validation.
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("KeyUsage missing KeyUsageCertSign; a self-signed CA must be able to sign certificates")
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("KeyUsage missing KeyUsageDigitalSignature")
	}
	foundServerAuth := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			foundServerAuth = true
		}
	}
	if !foundServerAuth {
		t.Errorf("ExtKeyUsage %v missing ServerAuth; the cert is the Server's TLS leaf", cert.ExtKeyUsage)
	}
	if cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("PublicKeyAlgorithm = %v, want ECDSA", cert.PublicKeyAlgorithm)
	}

	// IP literals bind as IP SANs, DNS names as DNS SANs.
	wantIPs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, want := range wantIPs {
		found := false
		for _, got := range cert.IPAddresses {
			if got.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("IPAddresses %v missing %v", cert.IPAddresses, want)
		}
	}
	if len(cert.IPAddresses) != 2 {
		t.Errorf("IPAddresses = %v, want exactly the two IP literals", cert.IPAddresses)
	}
	wantDNS := "localhost"
	foundDNS := false
	for _, got := range cert.DNSNames {
		if got == wantDNS {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Errorf("DNSNames %v missing %q", cert.DNSNames, wantDNS)
	}
	// localhost must NOT have been mis-classified as an IP SAN.
	if len(cert.DNSNames) != 1 {
		t.Errorf("DNSNames = %v, want exactly [localhost]", cert.DNSNames)
	}

	// Validity window: NotBefore is backdated (already valid now), NotAfter
	// is roughly DefaultValidity ahead. Bounds are certificate fields, not a
	// timing sync, so a wide tolerance keeps this deterministic.
	if cert.NotBefore.After(before) {
		t.Errorf("NotBefore = %v is after generation start %v; cert not yet valid", cert.NotBefore, before)
	}
	wantNotAfter := before.Add(DefaultValidity)
	if diff := cert.NotAfter.Sub(wantNotAfter); diff < -time.Minute || diff > time.Minute {
		t.Errorf("NotAfter = %v, want ~%v (diff %v)", cert.NotAfter, wantNotAfter, diff)
	}
}

// assertCustomValidityHonored verifies that an explicit validity duration is
// honored: the cert's validity span equals the requested duration plus the
// clock-skew backdate.
func assertCustomValidityHonored(t *testing.T) {
	t.Helper()
	const custom = 48 * time.Hour
	kp, err := Generate([]string{"127.0.0.1"}, custom)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert := parseCert(t, kp.CertPEM)
	// span = validity + clockSkew backdate. Compare span so wall-clock drift
	// between the two time.Now() calls inside Generate cancels out.
	span := cert.NotAfter.Sub(cert.NotBefore)
	want := custom + clockSkew
	if diff := span - want; diff < -time.Minute || diff > time.Minute {
		t.Errorf("validity span = %v, want ~%v (diff %v)", span, want, diff)
	}
}

// assertNonPositiveValidityUsesDefault verifies that a zero or negative validity
// falls back to DefaultValidity.
func assertNonPositiveValidityUsesDefault(t *testing.T) {
	t.Helper()
	for _, v := range []time.Duration{0, -time.Hour} {
		kp, err := Generate([]string{"127.0.0.1"}, v)
		if err != nil {
			t.Fatalf("Generate(validity=%v): %v", v, err)
		}
		cert := parseCert(t, kp.CertPEM)
		span := cert.NotAfter.Sub(cert.NotBefore)
		want := DefaultValidity + clockSkew
		if diff := span - want; diff < -time.Minute || diff > time.Minute {
			t.Errorf("validity=%v: span = %v, want ~%v (diff %v)", v, span, want, diff)
		}
	}
}

// assertEmptyHostsIsError verifies that generating with no SANs (nil or empty
// slice) is an error — a cert with no SANs matches nothing.
func assertEmptyHostsIsError(t *testing.T) {
	t.Helper()
	if _, err := Generate(nil, 0); err == nil {
		t.Error("Generate(nil) = nil error, want error (a cert with no SANs matches nothing)")
	}
	if _, err := Generate([]string{}, 0); err == nil {
		t.Error("Generate([]) = nil error, want error")
	}
}

func TestWriteFiles(t *testing.T) {
	kp, err := Generate([]string{"127.0.0.1", "localhost"}, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := kp.WriteFiles(certPath, keyPath); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	t.Run("cert is 0644, key is 0600", func(t *testing.T) {
		// The cert is a public trust anchor handed to every Runner as --ca, so
		// it is world-readable; the private key is a secret and must not be.
		for _, tc := range []struct {
			path string
			want os.FileMode
		}{
			{certPath, 0o644},
			{keyPath, 0o600},
		} {
			info, err := os.Stat(tc.path)
			if err != nil {
				t.Fatalf("stat %q: %v", tc.path, err)
			}
			if mode := info.Mode() & 0o777; mode != tc.want {
				t.Errorf("%q mode = %o, want %o", tc.path, mode, tc.want)
			}
		}
	})

	t.Run("cert PEM round-trips through x509 parse", func(t *testing.T) {
		onDisk, err := os.ReadFile(certPath)
		if err != nil {
			t.Fatalf("read cert: %v", err)
		}
		got := parseCert(t, onDisk)
		want := parseCert(t, kp.CertPEM)
		if got.SerialNumber.Cmp(want.SerialNumber) != 0 {
			t.Errorf("on-disk cert serial %v != in-memory serial %v", got.SerialNumber, want.SerialNumber)
		}
	})

	t.Run("tls.X509KeyPair loads the written pair", func(t *testing.T) {
		// This is exactly what compass-server does to serve the cert; if the two
		// files don't pair, the server can't start TLS.
		certBytes, err := os.ReadFile(certPath)
		if err != nil {
			t.Fatalf("read cert: %v", err)
		}
		keyBytes, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		if _, err := tls.X509KeyPair(certBytes, keyBytes); err != nil {
			t.Fatalf("tls.X509KeyPair: %v", err)
		}
	})

	t.Run("overwriting a loose-perm key tightens it to 0600", func(t *testing.T) {
		assertLoosePermKeyTightened(t, kp)
	})
	t.Run("a failed key write does not strand a new cert beside a missing key", func(t *testing.T) {
		assertFailedKeyWriteStrandsNothing(t, kp)
	})
}

// assertLoosePermKeyTightened verifies WriteFiles lands the key at 0600 even when
// the key path already holds a world-readable file. os.WriteFile would truncate
// in place and keep the stale mode; the atomic temp+rename must supersede it.
func assertLoosePermKeyTightened(t *testing.T, kp Keypair) {
	t.Helper()
	d := t.TempDir()
	cp := filepath.Join(d, "tls.crt")
	kmp := filepath.Join(d, "tls.key")
	if err := os.WriteFile(kmp, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed loose key: %v", err)
	}
	if err := os.WriteFile(cp, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	if err := kp.WriteFiles(cp, kmp); err != nil {
		t.Fatalf("WriteFiles over existing files: %v", err)
	}
	info, err := os.Stat(kmp)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if mode := info.Mode() & 0o777; mode != 0o600 {
		t.Errorf("key mode after overwrite = %o, want 600 (a private key must not stay world-readable)", mode)
	}
}

// assertFailedKeyWriteStrandsNothing verifies a failed key write leaves no new
// cert beside a missing key (the pair never diverges on failure) and no leftover
// temp file. The key failure is injected by making the key path a directory, so
// the rename-into-place cannot clobber it.
func assertFailedKeyWriteStrandsNothing(t *testing.T, kp Keypair) {
	t.Helper()
	d := t.TempDir()
	cp := filepath.Join(d, "tls.crt")
	kmp := filepath.Join(d, "tls.key")
	if err := os.Mkdir(kmp, 0o755); err != nil {
		t.Fatalf("seed key-path dir: %v", err)
	}
	if err := kp.WriteFiles(cp, kmp); err == nil {
		t.Fatal("WriteFiles with a key path that is a directory = nil error, want failure")
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Errorf("cert was written despite the key write failing (stat err = %v); want no stranded cert", err)
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tls.crt.") || strings.HasPrefix(e.Name(), "tls.key.") {
			t.Errorf("leftover temp file %q not cleaned up", e.Name())
		}
	}
}
