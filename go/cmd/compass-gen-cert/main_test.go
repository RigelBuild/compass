//go:build unix

package main

// Unit tests for the two pure seams of the cert-gen CLI: parseHosts (SAN list
// splitting) and shouldSkipGen (the skip-if-present idempotence gate). run()
// itself is a thin flag wrapper over certgen.Generate/WriteFiles, which are
// tested in internal/certgen; the gate is the one stateful decision the binary
// owns, so it is extracted and tested directly.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShouldSkipGen pins the skip-if-present truth table: without --force, both
// the cert AND the key must already exist to skip; any missing file (including a
// divergent pair from a half-finished prior run) regenerates, and --force always
// regenerates. Each case uses its own t.TempDir() so they are hermetic.
func TestShouldSkipGen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mkCert   bool
		mkKey    bool
		force    bool
		wantSkip bool
	}{
		{"neither file exists regenerates", false, false, false, false},
		{"only cert exists regenerates (divergent pair heals)", true, false, false, false},
		{"only key exists regenerates (divergent pair heals)", false, true, false, false},
		{"both exist skips", true, true, false, true},
		{"both exist but force regenerates", true, true, true, false},
		{"neither exists and force regenerates", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			certPath := filepath.Join(dir, "tls.crt")
			keyPath := filepath.Join(dir, "tls.key")
			if tc.mkCert {
				if err := os.WriteFile(certPath, []byte("cert"), 0o644); err != nil {
					t.Fatalf("seed cert: %v", err)
				}
			}
			if tc.mkKey {
				if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
					t.Fatalf("seed key: %v", err)
				}
			}
			if got := shouldSkipGen(certPath, keyPath, tc.force); got != tc.wantSkip {
				t.Errorf("shouldSkipGen(cert=%v, key=%v, force=%v) = %v, want %v",
					tc.mkCert, tc.mkKey, tc.force, got, tc.wantSkip)
			}
		})
	}
}

// TestParseHosts pins the SAN list splitting: comma-separated, blanks trimmed,
// empty entries (from a trailing comma or stray spaces) dropped so they never
// become an empty SAN.
func TestParseHosts(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"single host", "localhost", []string{"localhost"}},
		{"multiple hosts", "127.0.0.1,::1,localhost", []string{"127.0.0.1", "::1", "localhost"}},
		{"trailing comma dropped", "127.0.0.1,", []string{"127.0.0.1"}},
		{"surrounding spaces trimmed", " 127.0.0.1 , localhost ", []string{"127.0.0.1", "localhost"}},
		{"empty string yields none", "", nil},
		{"only commas yields none", ",,,", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseHosts(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseHosts(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseHosts(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}
