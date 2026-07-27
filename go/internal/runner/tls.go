//go:build unix

package runner

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"connectrpc.com/connect"
)

// NewCATrustClient builds an HTTP/2-over-TLS client that trusts exactly the CA
// in caPath (a PEM file), for dialing a Server whose network-door cert is not
// signed by a system root — the local dogfood path, where a self-signed
// 127.0.0.1 cert is both the server's --tls-cert and this runner's --ca (the
// self-signed leaf is its own CA). It is the seam RunnerConfig.HTTPClient
// consumes; production Runners talking to a Server behind a public CA leave --ca
// unset and Dial falls back to http.DefaultClient (system roots).
//
// The transport advertises HTTP/1.1 and HTTP/2 so ALPN negotiates h2 against the
// network door (which is HTTP/2-native over TLS via ALPN, mirroring the door's
// own client shape). RootCAs is set to ONLY the provided CA — the runner trusts
// that one cert, not the system pool, so a misconfigured or swapped server cert
// fails the handshake rather than silently trusting a wider set.
func NewCATrustClient(caPath string) (connect.HTTPClient, error) {
	pem, err := os.ReadFile(caPath) //nolint:gosec // caPath is an operator-provided CLI flag, the whole point of --ca
	if err != nil {
		return nil, fmt.Errorf("reading --ca certificate %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("--ca certificate %q contains no PEM certificate", caPath)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
				// The network door requires TLS 1.3 (network_door.go loadNetworkTLS);
				// match it so a downgrade cannot be negotiated from the client side.
				MinVersion: tls.VersionTLS13,
			},
			Protocols: protocols,
		},
	}, nil
}
