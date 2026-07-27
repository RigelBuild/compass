//go:build unix

package main

// Unit test for resolveNetworkDoor: the all-or-none CLI validation of the three
// network-door flags (--listen, --tls-cert, --tls-key). No I/O, no store, no
// sockets — a pure input→output contract, so a plain table-driven test covers
// the whole truth table.

import (
	"strings"
	"testing"

	"github.com/sealedsecurity/compass/go/server"
)

func TestResolveNetworkDoor(t *testing.T) {
	const (
		listen = "0.0.0.0:8443"
		cert   = "/etc/compass/tls.crt"
		key    = "/etc/compass/tls.key"
	)

	// The two non-error rows: none set (socket-only default) and all three set
	// (network door enabled). Asserted individually below because the success
	// case checks the returned TLSConfig, which the error rows don't have.
	t.Run("none set yields socket-only default", func(t *testing.T) {
		assertSocketOnlyDefault(t)
	})

	t.Run("all three set enables the network door", func(t *testing.T) {
		assertNetworkDoorEnabled(t, listen, cert, key)
	})

	// The partial rows: every meaningful proper subset of the three flags is a
	// startup error whose message names precisely the missing flag(s). wantMissing
	// lists the flag names the error MUST mention; wantAbsent lists the flag names
	// it MUST NOT mention (the ones that were supplied), so a message that named
	// every flag regardless of input would still fail these rows.
	partials := []struct {
		name                    string
		listen, cert, key       string
		wantMissing, wantAbsent []string
	}{
		{
			name:        "only --listen set (missing both TLS flags)",
			listen:      listen,
			wantMissing: []string{"--tls-cert", "--tls-key"},
			wantAbsent:  []string{"--listen"},
		},
		{
			name:        "only --tls-cert set (missing listen and key)",
			cert:        cert,
			wantMissing: []string{"--listen", "--tls-key"},
			wantAbsent:  []string{"--tls-cert"},
		},
		{
			name:        "only --tls-key set (missing listen and cert)",
			key:         key,
			wantMissing: []string{"--listen", "--tls-cert"},
			wantAbsent:  []string{"--tls-key"},
		},
		{
			name:        "--listen and --tls-cert set (missing --tls-key)",
			listen:      listen,
			cert:        cert,
			wantMissing: []string{"--tls-key"},
			wantAbsent:  []string{"--listen", "--tls-cert"},
		},
		{
			name:        "--listen and --tls-key set (missing --tls-cert)",
			listen:      listen,
			key:         key,
			wantMissing: []string{"--tls-cert"},
			wantAbsent:  []string{"--listen", "--tls-key"},
		},
		{
			name:        "--tls-cert and --tls-key set (missing --listen)",
			cert:        cert,
			key:         key,
			wantMissing: []string{"--listen"},
			wantAbsent:  []string{"--tls-cert", "--tls-key"},
		},
	}

	for _, tc := range partials {
		t.Run(tc.name, func(t *testing.T) {
			assertPartialFlagError(t, tc.listen, tc.cert, tc.key, tc.wantMissing, tc.wantAbsent)
		})
	}
}

// assertSocketOnlyDefault verifies that supplying none of the three network-door
// flags yields the socket-only default: no listen address and no TLS config.
func assertSocketOnlyDefault(t *testing.T) {
	t.Helper()
	gotListen, gotTLS, err := resolveNetworkDoor("", "", "")
	if err != nil {
		t.Fatalf("resolveNetworkDoor(\"\",\"\",\"\") error = %v, want nil", err)
	}
	if gotListen != "" {
		t.Errorf("listen = %q, want \"\"", gotListen)
	}
	if gotTLS != nil {
		t.Errorf("tlsConfig = %+v, want nil", gotTLS)
	}
}

// assertNetworkDoorEnabled verifies that supplying all three flags enables the
// network door: the listen string is passed through unchanged and the TLS config
// carries the cert/key paths on their correct fields (the arg-swap guard).
func assertNetworkDoorEnabled(t *testing.T, listen, cert, key string) {
	t.Helper()
	gotListen, gotTLS, err := resolveNetworkDoor(listen, cert, key)
	if err != nil {
		t.Fatalf("resolveNetworkDoor(%q,%q,%q) error = %v, want nil", listen, cert, key, err)
	}
	// Same listen string, unchanged — guards against the function mangling
	// or dropping the address.
	if gotListen != listen {
		t.Errorf("listen = %q, want %q", gotListen, listen)
	}
	if gotTLS == nil {
		t.Fatal("tlsConfig = nil, want non-nil *server.TLSConfig")
	}
	// Exact cert/key paths on the right fields — this is the arg-swap guard:
	// if the impl passed tlsKey into CertPath (or vice versa) these fail.
	if gotTLS.CertPath != cert {
		t.Errorf("tlsConfig.CertPath = %q, want %q", gotTLS.CertPath, cert)
	}
	if gotTLS.KeyPath != key {
		t.Errorf("tlsConfig.KeyPath = %q, want %q", gotTLS.KeyPath, key)
	}
	// Belt-and-suspenders on the whole struct value.
	want := server.TLSConfig{CertPath: cert, KeyPath: key}
	if *gotTLS != want {
		t.Errorf("tlsConfig = %+v, want %+v", *gotTLS, want)
	}
}

// assertPartialFlagError verifies that a proper subset of the three flags is a
// startup error that surfaces no usable config and whose "(missing ...)" clause
// names precisely the wantMissing flags and none of the wantAbsent ones.
func assertPartialFlagError(t *testing.T, listen, cert, key string, wantMissing, wantAbsent []string) {
	t.Helper()
	gotListen, gotTLS, err := resolveNetworkDoor(listen, cert, key)
	if err == nil {
		t.Fatalf("resolveNetworkDoor(%q,%q,%q) error = nil, want a partial-flag error",
			listen, cert, key)
	}
	// A partial (error) case must not smuggle out a usable config.
	if gotListen != "" {
		t.Errorf("listen = %q, want \"\" on error", gotListen)
	}
	if gotTLS != nil {
		t.Errorf("tlsConfig = %+v, want nil on error", gotTLS)
	}
	msg := err.Error()
	// The error's preamble names all three flags ("needs --listen,
	// --tls-cert, and --tls-key together"), so BOTH the presence and the
	// absence assertions must read only the "(missing ...)" clause —
	// checking the whole message would pass regardless of which flags the
	// impl actually detected as missing.
	missingClause := extractMissingClause(t, msg)
	for _, flag := range wantMissing {
		if !strings.Contains(missingClause, flag) {
			t.Errorf("error %q does not report %q as missing in %q",
				msg, flag, missingClause)
		}
	}
	for _, flag := range wantAbsent {
		if strings.Contains(missingClause, flag) {
			t.Errorf("error %q lists %q as missing in %q, but it was supplied",
				msg, flag, missingClause)
		}
	}
}

// extractMissingClause returns the contents of the "(missing ...)" parenthetical
// from a resolveNetworkDoor error message. The message names all three flags in
// its preamble, so asserting which flags were reported missing requires reading
// only this clause. Fails the test if the clause is absent — a message shape the
// contract does not permit.
func extractMissingClause(t *testing.T, msg string) string {
	t.Helper()
	const marker = "(missing "
	_, rest, found := strings.Cut(msg, marker)
	if !found {
		t.Fatalf("error %q has no %q clause", msg, marker)
	}
	clause, _, terminated := strings.Cut(rest, ")")
	if !terminated {
		t.Fatalf("error %q has an unterminated %q clause", msg, marker)
	}
	return clause
}
