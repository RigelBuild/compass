//go:build unix

// Command compass-gen-cert generates the self-signed TLS keypair the local
// dogfood path uses for the Server's authenticated network door. It writes a
// PEM certificate (0644, a public trust anchor) and private key (0600) to
// operator-specified paths; the single cert is its own CA, so the same file is
// the Server's --tls-cert and the Runner's --ca
// trust anchor — one artifact exercising the real production TLS enroll path
// locally (no external CA, no relaxed loopback). All generation logic lives in
// internal/certgen; this binary is a thin flag wrapper, mirroring
// cmd/compass-server and cmd/compass-runner.
//
// Typical local bring-up:
//
//	compass-gen-cert --cert-out $STATE/tls.crt --key-out $STATE/tls.key
//	compass-server --listen 127.0.0.1:8443 --tls-cert $STATE/tls.crt --tls-key $STATE/tls.key ...
//	compass-runner --ca $STATE/tls.crt --server https://127.0.0.1:8443 ...
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/sealedsecurity/compass/go/internal/certgen"
)

// version is the build version; override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

// defaultHosts are the SANs a local dogfood cert needs: the loopback IPs the
// network door binds and the localhost name, matching the network door's own
// test cert so the dogfood artifact is what the door's tests validate against.
const defaultHosts = "127.0.0.1,::1,localhost"

func main() {
	if err := run(); err != nil {
		slog.Error("compass-gen-cert exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	certOut := flag.String("cert-out", "",
		"Path to write the PEM certificate to (0644, a public trust anchor). "+
			"Required. This file is both the Server's --tls-cert and the Runner's "+
			"--ca trust anchor.")
	keyOut := flag.String("key-out", "",
		"Path to write the PEM private key to (0600). Required.")
	hosts := flag.String("hosts", defaultHosts,
		"Comma-separated SANs to issue the cert for; IP literals bind as IP SANs, "+
			"names as DNS SANs.")
	validity := flag.Duration("validity", certgen.DefaultValidity,
		"Certificate lifetime (e.g. 720h). The cert is backdated one hour for clock skew.")
	force := flag.Bool("force", false,
		"Regenerate even when both output files already exist. Default is "+
			"skip-if-present, so re-running (e.g. a devenv restart) does not "+
			"replace a cert a running Server/Runner is already using and force a "+
			"re-enroll.")
	showVersion := flag.Bool("version", false, "Print the version and exit.")
	flag.Parse()

	if *showVersion {
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if *certOut == "" || *keyOut == "" {
		return errors.New("both --cert-out and --key-out are required")
	}
	hostList := parseHosts(*hosts)
	if len(hostList) == 0 {
		return errors.New("at least one --hosts entry is required")
	}

	// Idempotent by default: when both files already exist, leave them be so a
	// restart does not swap the cert out from under a live Server/Runner pair
	// mid-session. --force overrides to regenerate unconditionally.
	if shouldSkipGen(*certOut, *keyOut, *force) {
		slog.Info("network-door certificate already present; skipping (pass --force to regenerate)",
			"cert", *certOut, "key", *keyOut)
		return nil
	}

	kp, err := certgen.Generate(hostList, *validity)
	if err != nil {
		return err
	}
	if err := kp.WriteFiles(*certOut, *keyOut); err != nil {
		return err
	}
	// Generate replaces a non-positive --validity with certgen.DefaultValidity,
	// so log the value actually baked into the cert, not the raw flag.
	actualValidity := *validity
	if actualValidity <= 0 {
		actualValidity = certgen.DefaultValidity
	}
	slog.Info("generated self-signed network-door certificate",
		"cert", *certOut, "key", *keyOut, "hosts", strings.Join(hostList, ","),
		"validity", actualValidity.String())
	return nil
}

// parseHosts splits the comma-separated SAN list, trimming blanks and dropping
// empties so a trailing comma or stray space does not become an empty SAN.
func parseHosts(csv string) []string {
	parts := strings.Split(csv, ",")
	hosts := make([]string, 0, len(parts))
	for _, p := range parts {
		if h := strings.TrimSpace(p); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// fileExists reports whether path names an existing file. A stat error other
// than not-exist (e.g. a permission problem) counts as "not present" so the
// generate path runs and surfaces the real write error, rather than silently
// skipping on an ambiguous stat.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// shouldSkipGen reports whether generation should be skipped: without --force,
// both the cert and the key must already exist. Requiring BOTH means a prior run
// that wrote one file but not the other (a divergent pair) is NOT skipped — the
// next run regenerates both and heals it. --force always regenerates.
func shouldSkipGen(certPath, keyPath string, force bool) bool {
	return !force && fileExists(certPath) && fileExists(keyPath)
}
