//go:build unix

// Package certgen generates the self-signed TLS keypair the local dogfood path
// uses for the Server's authenticated network door. The single generated cert
// is its own CA, so it serves as both the Server's --tls-cert/--tls-key and the
// Runner's --ca trust anchor — one artifact, no external CA, exercising the real
// production TLS enroll path locally (full local TLS, no relaxed
// loopback). It is the production sibling of the network-door test helper; the
// generation logic is identical (ECDSA P-256, 127.0.0.1/::1/localhost SANs) so
// the dogfood cert matches what the door's tests validate against.
package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultValidity is the certificate lifetime when none is given: long enough
// that a dogfood cert generated once serves for the life of a local bring-up,
// short enough that a leaked local cert is not a durable credential.
const DefaultValidity = 365 * 24 * time.Hour

// clockSkew backdates NotBefore so a freshly generated cert is already valid
// under a client whose clock trails the generating host's by up to this margin.
const clockSkew = time.Hour

// Keypair is a generated self-signed certificate and its private key, both PEM
// encoded — the bytes written to the --tls-cert / --tls-key (and --ca) files.
type Keypair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Generate creates a self-signed ECDSA P-256 certificate valid for hosts (IP
// literals bind as IP SANs, everything else as DNS SANs) over
// [now-clockSkew, now+validity]. The certificate is marked IsCA so it is its own
// trust anchor: the same PEM is the Server's leaf cert and the Runner's --ca.
// validity <= 0 uses DefaultValidity. At least one host is required — a cert
// with no SANs matches nothing.
func Generate(hosts []string, validity time.Duration) (Keypair, error) {
	if len(hosts) == 0 {
		return Keypair{}, errors.New("at least one host (IP or DNS name) is required")
	}
	if validity <= 0 {
		validity = DefaultValidity
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Keypair{}, fmt.Errorf("generating ECDSA key: %w", err)
	}
	// A random 128-bit serial, per CA/Browser Forum guidance — a fixed serial
	// collides if two generated certs ever land in the same trust store.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Keypair{}, fmt.Errorf("generating serial number: %w", err)
	}
	// Capture one instant so NotBefore and NotAfter derive from the same clock
	// read: the on-cert span is exactly validity+clockSkew, not two Now() calls
	// apart.
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "compass-network-door"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return Keypair{}, fmt.Errorf("creating self-signed certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return Keypair{}, fmt.Errorf("marshaling EC private key: %w", err)
	}
	return Keypair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// WriteFiles writes the certificate to certPath (0644) and the private key to
// keyPath (0600). The key is secret, so it is owner-only regardless of umask;
// the certificate is a public trust anchor (it is handed to every Runner as
// --ca), so it is world-readable. Parent directories must already exist.
//
// Each file is written atomically (temp file in the same directory, mode-pinned,
// synced, renamed into place), so a reader never sees a partial file and — since
// rename replaces rather than truncates-in-place — an existing target with loose
// permissions is superseded by a fresh file at the intended mode, never left at
// its old perms. The key is written first: if it cannot be written, no new
// certificate is stranded beside a missing key (the pair never diverges on a
// write failure).
func (k Keypair) WriteFiles(certPath, keyPath string) error {
	if err := atomicWrite(keyPath, k.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("writing private key to %q: %w", keyPath, err)
	}
	if err := atomicWrite(certPath, k.CertPEM, 0o644); err != nil {
		return fmt.Errorf("writing certificate to %q: %w", certPath, err)
	}
	return nil
}

// atomicWrite writes data to path at mode perm, atomically: a temp file in the
// same directory (mode-pinned against umask) is written, synced, and renamed
// over path. A reader never sees a partial file, and because rename replaces the
// target rather than truncating it in place, an existing file's stale mode never
// carries over. On any error the temp file is removed so no partial leaks.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod %o temp file: %w", perm, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file into place at %q: %w", path, err)
	}
	return nil
}
