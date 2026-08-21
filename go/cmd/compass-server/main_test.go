//go:build unix

package main

// Unit test for resolveNetworkDoor: the all-or-none CLI validation of the three
// network-door flags (--listen, --tls-cert, --tls-key). No I/O, no store, no
// sockets — a pure input→output contract, so a plain table-driven test covers
// the whole truth table.

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/server"
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

// TestBuildServeConfigFlagRoundTrip pins the CLI flag→ServeConfig mapping: the
// three T1 network-door flags (--state-dir/--admin-handle/--cors-allowed-origin)
// carry into their ServeConfig fields, and omitting them leaves the shipped
// defaults. A round-trip is necessary-not-sufficient for --admin-handle (an inert
// knob round-trips too — the server-level TestServeMints* tests are the teeth for
// "takes effect"), but it is exactly what pins the flag NAME→field wiring: a
// --state-dir mistakenly feeding AdminHandle reddens here. --database is supplied
// throughout because buildServeConfig requires a DSN (flag or $COMPASS_DATABASE_DSN).
func TestBuildServeConfigFlagRoundTrip(t *testing.T) {
	t.Setenv("COMPASS_DATABASE_DSN", "") // isolate from an ambient env DSN

	t.Run("new flags round-trip into their fields", func(t *testing.T) {
		cfg, showVersion, err := buildServeConfig([]string{
			"--database", "postgres://x/db",
			"--socket", "/tmp/x.sock",
			"--state-dir", "/var/lib/compass",
			"--admin-handle", "matt",
			"--cors-allowed-origin", "https://ui.example.ts.net",
		})
		if err != nil {
			t.Fatalf("buildServeConfig = %v, want nil", err)
		}
		if showVersion {
			t.Fatal("showVersion = true, want false (no --version)")
		}
		if cfg.StateDir != "/var/lib/compass" {
			t.Errorf("StateDir = %q, want %q (--state-dir)", cfg.StateDir, "/var/lib/compass")
		}
		if cfg.AdminHandle != "matt" {
			t.Errorf("AdminHandle = %q, want %q (--admin-handle)", cfg.AdminHandle, "matt")
		}
		if cfg.CORSAllowedOrigin != "https://ui.example.ts.net" {
			t.Errorf("CORSAllowedOrigin = %q, want %q (--cors-allowed-origin)", cfg.CORSAllowedOrigin, "https://ui.example.ts.net")
		}
	})

	t.Run("omitted new flags leave the shipped defaults", func(t *testing.T) {
		cfg, _, err := buildServeConfig([]string{"--database", "postgres://x/db", "--socket", "/tmp/x.sock"})
		if err != nil {
			t.Fatalf("buildServeConfig = %v, want nil", err)
		}
		if cfg.StateDir != "" || cfg.AdminHandle != "" || cfg.CORSAllowedOrigin != "" {
			t.Errorf("unset flags = {StateDir:%q AdminHandle:%q CORSAllowedOrigin:%q}, want all empty (the socket-only shipped defaults)",
				cfg.StateDir, cfg.AdminHandle, cfg.CORSAllowedOrigin)
		}
	})

	t.Run("shipped listen+tls group still maps", func(t *testing.T) {
		cfg, _, err := buildServeConfig([]string{
			"--database", "postgres://x/db", "--socket", "/tmp/x.sock",
			"--listen", "0.0.0.0:8443", "--tls-cert", "/c.pem", "--tls-key", "/k.pem",
		})
		if err != nil {
			t.Fatalf("buildServeConfig = %v, want nil", err)
		}
		if cfg.Listen != "0.0.0.0:8443" {
			t.Errorf("Listen = %q, want %q", cfg.Listen, "0.0.0.0:8443")
		}
		if cfg.TLS == nil || cfg.TLS.CertPath != "/c.pem" || cfg.TLS.KeyPath != "/k.pem" {
			t.Errorf("TLS = %+v, want cert=/c.pem key=/k.pem", cfg.TLS)
		}
	})

	t.Run("shipped s3 + dev-http fields still map after the FlagSet move", func(t *testing.T) {
		// The flag.String->fs.String move is exactly where a copy-paste transposition
		// (e.g. --s3-bucket read into S3.Endpoint) would compile, vet, and ship green.
		// Pin the full field-assembly block, not just the three T1 fields.
		cfg, _, err := buildServeConfig([]string{
			"--database", "postgres://x/db", "--socket", "/tmp/x.sock",
			"--dev-http", "127.0.0.1:50051",
			"--s3-endpoint", "s3.example:9000",
			"--s3-bucket", "transcripts",
			"--s3-region", "us-east-1",
		})
		if err != nil {
			t.Fatalf("buildServeConfig = %v, want nil", err)
		}
		if cfg.DevHTTP == nil || cfg.DevHTTP.String() != "127.0.0.1:50051" {
			t.Errorf("DevHTTP = %v, want 127.0.0.1:50051 (--dev-http)", cfg.DevHTTP)
		}
		if cfg.S3.Endpoint != "s3.example:9000" {
			t.Errorf("S3.Endpoint = %q, want %q (--s3-endpoint)", cfg.S3.Endpoint, "s3.example:9000")
		}
		if cfg.S3.Bucket != "transcripts" {
			t.Errorf("S3.Bucket = %q, want %q (--s3-bucket)", cfg.S3.Bucket, "transcripts")
		}
		if cfg.S3.Region != "us-east-1" {
			t.Errorf("S3.Region = %q, want %q (--s3-region)", cfg.S3.Region, "us-east-1")
		}
	})
}

// TestBuildServeConfigVersion: --version returns showVersion=true and no config,
// so run() prints the version and exits without touching the store.
func TestBuildServeConfigVersion(t *testing.T) {
	_, showVersion, err := buildServeConfig([]string{"--version"})
	if err != nil {
		t.Fatalf("buildServeConfig(--version) = %v, want nil", err)
	}
	if !showVersion {
		t.Fatal("showVersion = false, want true for --version")
	}
}

// TestBuildServeConfigMissingDSN: with neither --database nor $COMPASS_DATABASE_DSN,
// buildServeConfig fails rather than returning a store-less config Serve would
// reject deep in startup.
func TestBuildServeConfigMissingDSN(t *testing.T) {
	t.Setenv("COMPASS_DATABASE_DSN", "")
	_, _, err := buildServeConfig([]string{"--socket", "/tmp/x.sock"})
	if err == nil {
		t.Fatal("buildServeConfig with no DSN = nil, want a 'DSN is required' error")
	}
	if !strings.Contains(err.Error(), "DSN is required") {
		t.Fatalf("error = %q, want a 'DSN is required' message", err.Error())
	}
}

// TestBuildServeConfigPartialNetworkDoorErrors: a partial --listen/--tls group is
// rejected at parse time (the resolveNetworkDoor guard), so the invalid combo
// never reaches Serve. Complements resolveNetworkDoor's own unit test by proving
// buildServeConfig surfaces that error rather than swallowing it.
func TestBuildServeConfigPartialNetworkDoorErrors(t *testing.T) {
	_, _, err := buildServeConfig([]string{
		"--database", "postgres://x/db", "--socket", "/tmp/x.sock",
		"--listen", "0.0.0.0:8443", // missing --tls-cert/--tls-key
	})
	if err == nil {
		t.Fatal("buildServeConfig with --listen and no TLS = nil, want the partial-flag error")
	}
	if !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("error = %q, want it to name the missing TLS flags", err.Error())
	}
}

// TestBuildServeConfigRejectsWildcardCORS: the network door's CORS contract is
// exactly one explicit origin, so a wildcard (the ubiquitous "*", or any '*'
// pattern rs/cors honors) is rejected up front rather than silently opening the
// internet-facing door to every origin. Table-driven over the wildcard shapes.
func TestBuildServeConfigRejectsWildcardCORS(t *testing.T) {
	t.Setenv("COMPASS_DATABASE_DSN", "")
	for _, origin := range []string{"*", "https://*.example.com", "*.ts.net"} {
		t.Run(origin, func(t *testing.T) {
			_, _, err := buildServeConfig([]string{
				"--database", "postgres://x/db", "--socket", "/tmp/x.sock",
				"--cors-allowed-origin", origin,
			})
			if err == nil {
				t.Fatalf("buildServeConfig(--cors-allowed-origin %q) = nil, want a wildcard rejection", origin)
			}
			if !strings.Contains(err.Error(), "wildcard") {
				t.Fatalf("error = %q, want it to name the wildcard rejection", err.Error())
			}
		})
	}
}

// TestBuildServeConfigBadFlagIsUsageError: an unknown flag is a CLI usage
// mistake, not a server crash — buildServeConfig tags the parse error errUsage
// so main() exits 2 without re-logging it through slog (the FlagSet already
// printed usage). flag.ErrHelp stays distinguishable (multi-%w), so run()'s
// clean-exit help path is unaffected.
func TestBuildServeConfigBadFlagIsUsageError(t *testing.T) {
	// The ContinueOnError FlagSet writes usage to stderr; the test only inspects
	// the returned error, so the stderr noise is harmless.
	_, _, err := buildServeConfig([]string{"--no-such-flag"})
	if err == nil {
		t.Fatal("buildServeConfig(--no-such-flag) = nil, want a usage error")
	}
	if !errors.Is(err, errUsage) {
		t.Fatalf("error %v is not errUsage; main() would re-log a usage mistake as a server crash", err)
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Fatalf("a bad flag must not read as ErrHelp (that is a clean help exit): %v", err)
	}
}
