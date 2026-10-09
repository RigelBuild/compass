//go:build unix

package main

// TestResolveNetworkDoor covers address and inherited-descriptor validation.

import (
	"errors"
	"flag"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/server"
)

func TestResolveNetworkDoor(t *testing.T) {
	const (
		listen = "0.0.0.0:8443"
		cert   = "/etc/compass/tls.crt"
		key    = "/etc/compass/tls.key"
	)
	tests := []struct {
		name              string
		listen, cert, key string
		listenFD          int
		wantListen        string
		wantFD            int
		wantErr           []string
	}{
		{name: "none set yields socket-only default"},
		{name: "listen enables network door", listen: listen, cert: cert, key: key, wantListen: listen},
		{name: "only listen misses both TLS flags", listen: listen, wantErr: []string{"--tls-cert", "--tls-key"}},
		{name: "only cert misses listen and key", cert: cert, wantErr: []string{"--listen", "--listen-fd", "--tls-key"}},
		{name: "only key misses listen and cert", key: key, wantErr: []string{"--listen", "--listen-fd", "--tls-cert"}},
		{name: "listen and cert miss key", listen: listen, cert: cert, wantErr: []string{"--tls-key"}},
		{name: "listen and key miss cert", listen: listen, key: key, wantErr: []string{"--tls-cert"}},
		{name: "both TLS flags miss listener", cert: cert, key: key, wantErr: []string{"--listen", "--listen-fd"}},
		{name: "inherited fd with TLS enables network door", listenFD: 3, cert: cert, key: key, wantFD: 3},
		{name: "inherited fd misses both TLS flags", listenFD: 3, wantErr: []string{"--tls-cert", "--tls-key"}},
		{name: "listen and inherited fd conflict", listen: listen, listenFD: 3, cert: cert, key: key, wantErr: []string{"--listen", "--listen-fd"}},
		{name: "negative inherited fd is rejected", listenFD: -1, cert: cert, key: key, wantErr: []string{"--listen-fd", "3"}},
		{name: "inherited fd below 3 is rejected", listenFD: 2, cert: cert, key: key, wantErr: []string{"--listen-fd", "3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotListen, gotFD, gotTLS, err := resolveNetworkDoor(tc.listen, tc.listenFD, tc.cert, tc.key)
			if len(tc.wantErr) > 0 {
				assertNetworkDoorError(t, gotListen, gotFD, gotTLS, err, tc.wantErr)
				return
			}
			assertNetworkDoorSuccess(t, gotListen, gotFD, gotTLS, err, tc.wantListen, tc.wantFD, cert, key)
		})
	}
}

func assertNetworkDoorError(t *testing.T, gotListen string, gotFD int, gotTLS *server.TLSConfig, err error, want []string) {
	t.Helper()
	if err == nil {
		t.Fatal("resolveNetworkDoor() error = nil, want error")
	}
	for _, part := range want {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not contain %q", err, part)
		}
	}
	if gotListen != "" || gotFD != 0 || gotTLS != nil {
		t.Errorf("resolveNetworkDoor() returned %q/%d/%+v on error", gotListen, gotFD, gotTLS)
	}
}

func assertNetworkDoorSuccess(t *testing.T, gotListen string, gotFD int, gotTLS *server.TLSConfig, err error, wantListen string, wantFD int, cert, key string) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveNetworkDoor() error = %v, want nil", err)
	}
	if gotListen != wantListen || gotFD != wantFD {
		t.Errorf("resolved listen/fd = %q/%d, want %q/%d", gotListen, gotFD, wantListen, wantFD)
	}
	if wantListen == "" && wantFD == 0 {
		if gotTLS != nil {
			t.Errorf("TLS = %+v, want nil in socket-only mode", gotTLS)
		}
		return
	}
	if gotTLS == nil || gotTLS.CertPath != cert || gotTLS.KeyPath != key {
		t.Errorf("TLS = %+v, want cert=%q key=%q", gotTLS, cert, key)
	}
}

// rawListenerFD returns a dup of l's descriptor that no *os.File owns, as a
// child inherits it; an *os.File here would close the number again when GC'd.
func rawListenerFD(t *testing.T, l *net.TCPListener) int {
	t.Helper()
	file, err := l.File()
	if err != nil {
		t.Fatalf("listener File: %v", err)
	}
	defer file.Close()
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		t.Fatalf("dup listener fd: %v", err)
	}
	return fd
}

// An inherited fd becomes ServeConfig.ListenListener, and the passed fd is
// closed (FileListener dups it) so the process holds exactly one descriptor.
func TestBuildServeConfigInheritedListener(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fd := rawListenerFD(t, listener)
	t.Setenv("COMPASS_NATS_URL", "nats://127.0.0.1:4222")
	cfg, _, err := buildServeConfig([]string{
		"--database", "postgres://x/db",
		"--socket", "/tmp/x.sock",
		"--listen-fd", strconv.Itoa(fd),
		"--tls-cert", "/c.pem",
		"--tls-key", "/k.pem",
	})
	if err != nil {
		t.Fatalf("buildServeConfig with inherited listener: %v", err)
	}
	t.Cleanup(func() {
		if cfg.ListenListener != nil {
			_ = cfg.ListenListener.Close()
		}
	})
	if cfg.Listen != "" || cfg.ListenListener == nil {
		t.Fatalf("listen config = %q/%v, want inherited listener only", cfg.Listen, cfg.ListenListener)
	}
	if got, want := cfg.ListenListener.Addr().String(), listener.Addr().String(); got != want {
		t.Fatalf("inherited listener address = %q, want %q", got, want)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("passed fd %d Fstat error = %v, want EBADF (closed after adoption)", fd, err)
	}
}

// A non-listener fd is rejected and left open (it may not be ours to close).
func TestBuildServeConfigRejectsNonTCPListenerFD(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer writer.Close()
	defer reader.Close()
	t.Setenv("COMPASS_NATS_URL", "nats://127.0.0.1:4222")
	_, _, err = buildServeConfig([]string{
		"--database", "postgres://x/db",
		"--socket", "/tmp/x.sock",
		"--listen-fd", strconv.Itoa(int(reader.Fd())),
		"--tls-cert", "/c.pem",
		"--tls-key", "/k.pem",
	})
	if err == nil || !strings.Contains(err.Error(), "TCP listener") {
		t.Fatalf("buildServeConfig with pipe fd = %v, want non-TCP listener error", err)
	}
	if _, err := reader.Stat(); err != nil {
		t.Fatalf("pipe fd was closed by a rejected adoption: %v", err)
	}
}

// A config error after the fd was adopted must close the adopted listener, or
// the failed process would keep the stack's port open until it exits.
func TestBuildServeConfigClosesAdoptedListenerOnLaterError(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	addr := listener.Addr().String()
	fd := rawListenerFD(t, listener)
	if err := listener.Close(); err != nil { // fd is now the only copy
		t.Fatalf("close original listener: %v", err)
	}
	t.Setenv("COMPASS_NATS_URL", "nats://127.0.0.1:4222")
	_, _, err = buildServeConfig([]string{
		"--database", "postgres://x/db",
		"--socket", "/tmp/x.sock",
		"--listen-fd", strconv.Itoa(fd),
		"--tls-cert", "/c.pem",
		"--tls-key", "/k.pem",
		"--cors-allowed-origin", "*",
	})
	if err == nil {
		t.Fatal("buildServeConfig with a wildcard origin = nil, want error")
	}
	if conn, err := net.Dial("tcp", addr); err == nil {
		_ = conn.Close()
		t.Fatal("adopted listener still accepts after a config error")
	}
}

// TestBuildServeConfigFlagRoundTrip pins the CLI flag→ServeConfig mapping: the
// three T1 network-door flags (--state-dir/--admin-handle/--cors-allowed-origin)
// carry into their ServeConfig fields, and omitting them leaves the shipped
// defaults. A round-trip is necessary-not-sufficient for --admin-handle (an inert
// knob round-trips too — the server-level TestServeMints* tests are the teeth for
// "takes effect"), but it is exactly what pins the flag NAME→field wiring: a
// --state-dir mistakenly feeding AdminHandle reddens here. --database is supplied
// throughout because buildServeConfig requires a DSN (flag or $COMPASS_DATABASE_DSN),
// and $COMPASS_NATS_URL is set because it requires a NATS URL the same way.
func TestBuildServeConfigFlagRoundTrip(t *testing.T) {
	t.Setenv("COMPASS_DATABASE_DSN", "")                  // isolate from an ambient env DSN
	t.Setenv("COMPASS_NATS_URL", "nats://127.0.0.1:4222") // required; the env path leaves the args under test unchanged

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

// TestBuildServeConfigNatsURL pins the event-fabric endpoint: --nats-url wins
// over $COMPASS_NATS_URL, the env is the fallback, and with neither the config
// is rejected, because a Server with no fabric commits posts it never delivers.
func TestBuildServeConfigNatsURL(t *testing.T) {
	t.Setenv("COMPASS_DATABASE_DSN", "")
	tests := []struct {
		name, flag, env, want string
	}{
		{name: "neither set is rejected"},
		{name: "env is the fallback", env: "nats://env.example:4222", want: "nats://env.example:4222"},
		{name: "flag wins over env", flag: "nats://flag.example:4222", env: "nats://env.example:4222", want: "nats://flag.example:4222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("COMPASS_NATS_URL", tt.env)
			args := []string{"--database", "postgres://x/db", "--socket", "/tmp/x.sock"}
			if tt.flag != "" {
				args = append(args, "--nats-url", tt.flag)
			}
			cfg, _, err := buildServeConfig(args)
			if tt.want == "" {
				if err == nil || !strings.Contains(err.Error(), "--nats-url") {
					t.Fatalf("buildServeConfig with no NATS URL = %v, want an error naming --nats-url", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildServeConfig = %v, want nil", err)
			}
			if cfg.NatsURL != tt.want {
				t.Errorf("NatsURL = %q, want %q", cfg.NatsURL, tt.want)
			}
		})
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

// TestBuildServeConfigUsageEventRetention pins the retention window's
// flag-then-env precedence, its 90-day default, 0 as the opt-out, and bad input.
func TestBuildServeConfigUsageEventRetention(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env string
		want            time.Duration
		wantErr         bool
	}{
		{name: "unset_keeps_90_days", want: 90 * 24 * time.Hour},
		{name: "flag_sets_the_window", flag: "720h", want: 720 * time.Hour},
		{name: "env_is_the_fallback", env: "48h", want: 48 * time.Hour},
		{name: "flag_beats_env", flag: "24h", env: "48h", want: 24 * time.Hour},
		{name: "flag_0_disables_the_sweep", flag: "0", env: "48h", want: 0},
		{name: "env_0_disables_the_sweep", env: "0", want: 0},
		{name: "day_unit_is_rejected", flag: "90d", wantErr: true},
		{name: "bad_env_is_rejected", env: "soon", wantErr: true},
		{name: "negative_is_rejected", flag: "-24h", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COMPASS_USAGE_EVENT_RETENTION", tc.env)
			t.Setenv("COMPASS_NATS_URL", "nats://127.0.0.1:4222") // required since the event fabric landed
			args := []string{"--database", "postgres://x/db", "--socket", "/tmp/x.sock"}
			if tc.flag != "" {
				args = append(args, "--usage-event-retention", tc.flag)
			}
			cfg, _, err := buildServeConfig(args)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--usage-event-retention") {
					t.Fatalf("buildServeConfig = %v, want an error naming --usage-event-retention", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildServeConfig = %v, want nil", err)
			}
			if cfg.UsageEventRetention != tc.want {
				t.Errorf("UsageEventRetention = %v, want %v", cfg.UsageEventRetention, tc.want)
			}
		})
	}
}
