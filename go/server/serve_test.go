//go:build unix

package server

// DB-free Serve-loop tests: the up-front DevHTTP loopback guard, which returns
// before Serve opens the store, so it needs no Postgres and runs in the default
// `go test ./...` lane. The store-gated serve tests (which call store.Open and
// so require a real database) live in serve_pgtest_test.go behind the `pgtest`
// tag, alongside the socket-readiness gate (waitListening) they use.
//
// Hermetic: t.TempDir() socket paths, no fixed ports.

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeRejectsNonLoopbackDevHTTPUpFront(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")
	// A routable, non-loopback address (TEST-NET-3, RFC 5737): the guard must
	// reject it before binding anything, so nothing is ever dialed.
	devAddr := netip.MustParseAddrPort("203.0.113.1:12345")

	// No DatabaseDSN is set: the loopback guard returns before Serve reaches
	// store.Open, so this test stays DB-free in the default lane. That ordering
	// is itself part of the contract — an inverted guard would fall through to
	// the store open (or a bind) instead of failing here.
	err := Serve(context.Background(), ServeConfig{
		SocketPath: socketPath,
		Version:    "serve-test",
		DevHTTP:    &devAddr,
	})
	if err == nil {
		t.Fatal("Serve with non-loopback DevHTTP = nil, want an up-front error")
	}
	// The message must be the loopback guard's, not a downstream bind failure:
	// the guard rejects the address before net.Listen is ever attempted, so an
	// inverted or missing guard (which would instead fail when trying to bind
	// the unroutable address) is caught here.
	if msg := err.Error(); !strings.Contains(msg, "dev_http must be a loopback address") {
		t.Fatalf("Serve error = %q, want the up-front loopback-guard message", msg)
	}
	// The guard fires before any on-disk state: no socket must have been created.
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket created despite non-loopback DevHTTP rejection (stat err = %v)", statErr)
	}
}
