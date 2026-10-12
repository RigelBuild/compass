//go:build unix

package main

// Unit tests for the two pure seams of the cert-gen CLI: parseHosts (SAN list
// splitting) and shouldSkipGen (the skip-if-present idempotence gate). run()
// itself is a thin flag wrapper over certgen.Generate/WriteFiles, which are
// tested in internal/certgen; the gate is the one stateful decision the binary
// owns, so it is extracted and tested directly.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// TestCheckOutputPaths rejects a missing flag and one file named twice, which
// would let the certificate write overwrite the private key.
func TestCheckOutputPaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cert := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(cert, []byte("cert"), 0o644); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(cert, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	for _, tc := range []struct {
		name, cert, key string
		wantErr         bool
	}{
		{"distinct paths pass", "tls.crt", "tls.key", false},
		{"missing cert flag fails", "", "tls.key", true},
		{"missing key flag fails", "tls.crt", "", true},
		{"identical paths fail", "tls.crt", "tls.crt", true},
		{"relative and absolute spellings of one path fail", "tls.crt", cert, true},
		{"a symlink to the cert fails", cert, link, true},
		{"absent files through a symlinked parent fail", filepath.Join(realDir, "a.pem"), filepath.Join(linkDir, "a.pem"), true},
		{"distinct absent files through a symlinked parent pass", filepath.Join(realDir, "a.pem"), filepath.Join(linkDir, "b.pem"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkOutputPaths(tc.cert, tc.key); (err != nil) != tc.wantErr {
				t.Errorf("checkOutputPaths(%q, %q) = %v, wantErr %v", tc.cert, tc.key, err, tc.wantErr)
			}
		})
	}
}

// TestLooseMode flags any group or other bit on an existing file, and nothing else.
func TestLooseMode(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		mode os.FileMode
		want bool
	}{{0o600, false}, {0o400, false}, {0o640, true}, {0o604, true}, {0o602, true}} {
		path := filepath.Join(dir, tc.mode.String())
		if err := os.WriteFile(path, []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		if got := looseMode(path); got != tc.want {
			t.Errorf("looseMode(%v) = %v, want %v", tc.mode, got, tc.want)
		}
	}
	if looseMode(filepath.Join(dir, "absent")) {
		t.Error("looseMode(absent) = true, want false")
	}
}

// captureLog routes slog's default logger into a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestSkipExistingWarnsOnLooseKey checks the skip path warns on a loose key and
// leaves its mode, and stays quiet on a 0600 key.
func TestSkipExistingWarnsOnLooseKey(t *testing.T) {
	for _, tc := range []struct {
		mode     os.FileMode
		wantWarn bool
	}{{0o600, false}, {0o644, true}} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
			if err := os.WriteFile(cert, []byte("cert"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(key, tc.mode); err != nil {
				t.Fatal(err)
			}
			logs := captureLog(t)
			if !skipExisting(cert, key, false) {
				t.Fatal("skipExisting = false, want a skip with both files present")
			}
			if got := strings.Contains(logs.String(), "level=WARN"); got != tc.wantWarn {
				t.Errorf("warned = %v, want %v; logs:\n%s", got, tc.wantWarn, logs)
			}
			info, err := os.Stat(key)
			if err != nil {
				t.Fatalf("stat key: %v", err)
			}
			if info.Mode().Perm() != tc.mode {
				t.Errorf("key mode = %v, want %v unchanged", info.Mode().Perm(), tc.mode)
			}
		})
	}
}
