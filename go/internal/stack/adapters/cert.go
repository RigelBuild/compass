//go:build unix

package adapters

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sealedsecurity/compass/go/internal/certgen"
	"github.com/sealedsecurity/compass/go/internal/stack"
)

// DefaultRotateWindow is the lead time before a cert's NotAfter at which the
// anchor is proactively rotated: comfortably longer than any single local
// bring-up, so a live stack is never driven over a door about to expire.
const DefaultRotateWindow = 30 * 24 * time.Hour

// certHosts are the SANs the dogfood anchor binds — loopback plus localhost,
// matching what certgen's tests validate against.
var certHosts = []string{"127.0.0.1", "::1", "localhost"}

// CertEnsurer is the real stack.CertEnsurer: it ensures the TLS anchor (one PEM
// serving as both the server's --tls-cert and the runner's --ca) exists under
// the state dir and is valid well past now, rotating it via internal/certgen
// when it is missing, unparseable, or within rotateWindow of expiry.
type CertEnsurer struct {
	rotateWindow time.Duration
}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.CertEnsurer = (*CertEnsurer)(nil)

// NewCertEnsurer builds a CertEnsurer. A rotateWindow <= 0 uses
// DefaultRotateWindow; tests pass a short window to drive the near-expiry path.
func NewCertEnsurer(rotateWindow time.Duration) *CertEnsurer {
	if rotateWindow <= 0 {
		rotateWindow = DefaultRotateWindow
	}
	return &CertEnsurer{rotateWindow: rotateWindow}
}

// EnsureCert resolves the cert/key paths under stateDir and (re)generates the
// anchor when needed. It is expiry-aware: an existing, parseable anchor whose
// NotAfter is more than rotateWindow beyond now is left untouched
// (Rotated=false); anything else — a missing file, a corrupt/half-written cert,
// or one within rotateWindow of (or past) expiry — triggers a fresh generate
// (Rotated=true).
func (e *CertEnsurer) EnsureCert(_ context.Context, stateDir string, now time.Time) (stack.CertResult, error) {
	certPath := filepath.Join(stateDir, "tls.crt")
	keyPath := filepath.Join(stateDir, "tls.key")

	if !e.needsRotation(certPath, keyPath, now) {
		return stack.CertResult{CertPath: certPath, KeyPath: keyPath, Rotated: false}, nil
	}

	kp, err := certgen.Generate(certHosts, 0)
	if err != nil {
		return stack.CertResult{}, fmt.Errorf("generating TLS anchor: %w", err)
	}
	if err := kp.WriteFiles(certPath, keyPath); err != nil {
		return stack.CertResult{}, fmt.Errorf("writing TLS anchor: %w", err)
	}
	return stack.CertResult{CertPath: certPath, KeyPath: keyPath, Rotated: true}, nil
}

// needsRotation reports whether the anchor must be (re)generated. Any condition
// short of "both files present, cert parseable, and NotAfter safely beyond the
// window" means rotate — a corrupt or half-written cert is treated as needing
// rotation, never surfaced as an error.
func (e *CertEnsurer) needsRotation(certPath, keyPath string, now time.Time) bool {
	if _, err := os.Stat(keyPath); err != nil {
		return true
	}
	notAfter, ok := certNotAfter(certPath)
	if !ok {
		return true
	}
	return !notAfter.After(now.Add(e.rotateWindow))
}

// certNotAfter reads the leaf cert at path and returns its NotAfter. ok is false
// when the file is missing, not valid PEM, or not a parseable certificate — the
// caller treats every such case as "rotate".
func certNotAfter(certPath string) (time.Time, bool) {
	raw, err := os.ReadFile(certPath) //nolint:gosec // G304: certPath is the adapter-owned cert file under the stack state dir, not user input
	if err != nil {
		return time.Time{}, false
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return time.Time{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, false
	}
	return cert.NotAfter, true
}
