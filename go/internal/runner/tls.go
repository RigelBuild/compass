//go:build unix

package runner

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// NewCATrustClient builds an HTTP/2-over-TLS client that trusts exactly the CA
// in caPath (a PEM file). It keeps the CA-only trust contract used by local
// dogfood clients.
func NewCATrustClient(caPath string) (*http.Client, error) {
	roots, err := readCATrust(caPath)
	if err != nil {
		return nil, err
	}
	return newTLSTransportClient(roots, ""), nil
}

// NewServerClient builds the Runner's HTTP/2-over-TLS client. An empty caPath
// uses the host roots, and serverName overrides the certificate name and SNI.
func NewServerClient(caPath, serverName string) (*http.Client, error) {
	var roots *x509.CertPool
	if caPath != "" {
		var err error
		roots, err = readCATrust(caPath)
		if err != nil {
			return nil, err
		}
	}
	return newTLSTransportClient(roots, serverName), nil
}

func readCATrust(caPath string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caPath) //nolint:gosec // caPath is an operator-provided CLI flag
	if err != nil {
		return nil, fmt.Errorf("reading --ca certificate %q: %w", caPath, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("--ca certificate %q contains no PEM certificate", caPath)
	}
	return roots, nil
}

func newTLSTransportClient(roots *x509.CertPool, serverName string) *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				ServerName: serverName,
				// The network door requires TLS 1.3; prevent a client-side downgrade.
				MinVersion: tls.VersionTLS13,
			},
			Protocols: protocols,
		},
	}
}
