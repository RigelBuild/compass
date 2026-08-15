//go:build unix

package bridge

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"sync/atomic"
)

// NewTLSTarget builds a Target that dials the daemon's TLS network door
// (native-client mode) over HTTP/2-over-TLS, mirroring the drop-in note in
// pump.go: a client with a TLS-dialing transport and an https base URL, leaving
// Pump.Do unchanged.
//
// serverURL is the https-absolute base URL, already validated upstream by
// validateServerURL (appconfig.go); it is used verbatim as the base URL and its
// scheme is not re-validated here. caPEM pins the trust anchor: when empty the
// system roots are used (RootCAs left nil); otherwise a fresh CertPool is seeded
// with the PEM (the appconfig ca_cert anchor). A CA PEM that yields no usable
// certificate is a configuration error and is returned as such.
//
// The TLS transport requires TLS 1.3 to mirror the server's network door
// (network_door.go:115) and enables HTTP/1.1 and HTTP/2 explicitly via
// http.Protocols, so ALPN negotiates encrypted h2 over the real TLS dial. The
// explicit opt-in is required: a custom TLSClientConfig otherwise makes
// net/http conservatively disable HTTP/2 (transport.go), silently downgrading
// to HTTP/1.1. This is the encrypted counterpart to NewUnixTarget's h2c
// (SetUnencryptedHTTP2) opt-in — never the cleartext path here.
//
// The returned Target carries a bearer-injecting RoundTripper (see SetBearer):
// it starts unarmed, so until SetBearer is called it forwards requests as-is.
func NewTLSTarget(serverURL string, caPEM []byte) (*Target, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("bridge: CA PEM contained no usable certificate")
		}
		cfg.RootCAs = pool
	}

	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	base := &http.Transport{TLSClientConfig: cfg, Protocols: p}
	rt := &bearerRoundTripper{base: base}

	return &Target{
		client:  &http.Client{Transport: rt},
		baseURL: serverURL,
	}, nil
}

// SetBearer arms (or disarms) the target's Authorization injection. When token
// is non-empty every forwarded request carries exactly "Authorization: Bearer
// <token>", overwriting any caller-supplied authorization header (the DL-107
// shell-injection point). An empty token disarms injection: no bearer is added,
// but a caller-supplied Authorization header is still stripped unconditionally
// (a UI-supplied bearer is always illegitimate in client mode).
//
// SetBearer is safe to call concurrently with request forwarding: T5.3's Connect
// arms the token from a different goroutine than the pump's forwarding goroutine.
func (t *Target) SetBearer(token string) {
	rt, ok := t.client.Transport.(*bearerRoundTripper)
	if !ok {
		return
	}
	if token == "" {
		rt.token.Store(nil)
		return
	}
	rt.token.Store(&token)
}

// bearerRoundTripper wraps a base RoundTripper. It strips any caller-supplied
// Authorization header on every request (unconditionally — the UI must never
// supply a bearer, DL-107) and, when armed, injects "Authorization: Bearer
// <token>". The token is held in an atomic.Pointer so SetBearer
// (T5.3's goroutine) and the pump's forwarding goroutine can access it without a
// lock. A nil pointer means unarmed.
type bearerRoundTripper struct {
	base  http.RoundTripper
	token atomic.Pointer[string]
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	token := b.token.Load()
	// Fast path: unarmed with no caller-supplied Authorization header at all —
	// forward as-is, no allocation. Keyed on true absence (Values == nil), not
	// Get == "", so a present-but-empty "Authorization:" header still falls
	// through to the unconditional strip below (DL-107, even by bug).
	if token == nil && req.Header.Values("Authorization") == nil {
		return b.base.RoundTrip(req)
	}

	// Clone before mutating: RoundTrippers must not modify the caller's request
	// (net/http contract). Strip any caller-supplied Authorization header
	// UNCONDITIONALLY: in client mode the bearer is only ever shell-injected, so
	// a UI-supplied header is always illegitimate and must not reach the server
	// even by bug (DL-107). Re-add the armed bearer when present.
	clone := req.Clone(req.Context())
	clone.Header.Del("Authorization")
	if token != nil {
		clone.Header.Set("Authorization", "Bearer "+*token)
	}
	return b.base.RoundTrip(clone)
}
