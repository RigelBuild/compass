//go:build unix

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// baseFlags returns a configFlags with the two required fields set to valid
// values rooted at dir, so each test can mutate exactly the field under test.
func baseFlags(dir string) configFlags {
	return configFlags{
		stateDir: dir,
		image:    "example.com/agent:latest",
	}
}

func TestResolveConfig(t *testing.T) {
	// Pin XDG_RUNTIME_DIR to a short, absolute dir so the RuntimeDir default is
	// deterministic and within the sun_path budget regardless of the host env.
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("COMPASS_DATABASE_DSN", "")

	t.Run("state-dir required", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.stateDir = ""
		if _, err := resolveConfig(f); err == nil {
			t.Fatal("expected error when --state-dir is absent, got nil")
		}
	})

	t.Run("image required", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.image = ""
		if _, err := resolveConfig(f); err == nil {
			t.Fatal("expected error when --image is absent, got nil")
		}
	})

	t.Run("defaults applied", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := resolveConfig(baseFlags(dir))
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.SocketPath == "" {
			t.Error("SocketPath default is empty")
		}
		if cfg.ListenAddr != defaultListenAddr {
			t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
		}
		if !strings.Contains(cfg.DatabaseDSN, dir) {
			t.Errorf("DatabaseDSN %q does not reference the state dir %q", cfg.DatabaseDSN, dir)
		}
		if cfg.RuntimeDir == "" {
			t.Error("RuntimeDir default is empty")
		}
		if cfg.AgentImage != "example.com/agent:latest" {
			t.Errorf("AgentImage = %q, want the flag value", cfg.AgentImage)
		}
		// Validate must pass with the defaults (RuntimeDir within the sun_path
		// budget); resolveConfig already ran it, so reaching here proves it.
	})

	t.Run("listen :0 rejected", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.listen = "127.0.0.1:0"
		if _, err := resolveConfig(f); err == nil {
			t.Fatal("expected Validate to reject an ephemeral :0 listen addr, got nil")
		}
	})

	t.Run("DSN default references state dir", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := resolveConfig(baseFlags(dir))
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if !strings.Contains(cfg.DatabaseDSN, dir) {
			t.Errorf("default DSN %q does not reference state dir %q", cfg.DatabaseDSN, dir)
		}
		if !strings.Contains(cfg.DatabaseDSN, "dbname=compass") {
			t.Errorf("default DSN %q is not the expected keyword/value form", cfg.DatabaseDSN)
		}
	})

	t.Run("COMPASS_DATABASE_DSN honored, flag wins", func(t *testing.T) {
		t.Setenv("COMPASS_DATABASE_DSN", "host=/env/sock dbname=compass")

		// Env used when the flag is empty.
		cfg, err := resolveConfig(baseFlags(t.TempDir()))
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.DatabaseDSN != "host=/env/sock dbname=compass" {
			t.Errorf("DatabaseDSN = %q, want the env value", cfg.DatabaseDSN)
		}

		// Flag wins over env.
		f := baseFlags(t.TempDir())
		f.database = "host=/flag/sock dbname=compass"
		cfg, err = resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.DatabaseDSN != "host=/flag/sock dbname=compass" {
			t.Errorf("DatabaseDSN = %q, want the flag value (flag wins over env)", cfg.DatabaseDSN)
		}
	})
}

// TestResolveConfigContainerFlags covers the S4 container/external flags,
// kept separate from TestResolveConfig to hold each test's cognitive complexity
// under the gate.
func TestResolveConfigContainerFlags(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("COMPASS_DATABASE_DSN", "")

	t.Run("postgres-image and database-external thread into config", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.postgresImage = "docker.io/library/postgres:18@sha256:abc"
		f.databaseExternal = true
		f.database = "host=/var/run/postgresql port=5432 dbname=compass sslmode=disable" // external requires an explicit DSN
		cfg, err := resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.PostgresImage != f.postgresImage {
			t.Errorf("PostgresImage = %q, want %q", cfg.PostgresImage, f.postgresImage)
		}
		if !cfg.ExternalDatabase {
			t.Error("ExternalDatabase = false, want true (flag set)")
		}
		if cfg.DatabaseDSN != f.database {
			t.Errorf("DatabaseDSN = %q, want the supplied external DSN %q", cfg.DatabaseDSN, f.database)
		}
	})

	t.Run("database-external without an explicit DSN is rejected", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.databaseExternal = true // no --database, no $COMPASS_DATABASE_DSN
		_, err := resolveConfig(f)
		if err == nil {
			t.Fatal("resolveConfig(external, no DSN) = nil error, want a rejection (would silently default to the private socket)")
		}
		if !strings.Contains(err.Error(), "--database-external requires an explicit --database DSN") {
			t.Errorf("error = %v, want it to name the missing external DSN", err)
		}
	})

	t.Run("empty postgres-image is the dev-path process spawn", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.postgresImage = ""
		cfg, err := resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.PostgresImage != "" {
			t.Errorf("PostgresImage = %q, want empty (dev-path)", cfg.PostgresImage)
		}
	})

	t.Run("default socket dir is a sibling of PGDATA, not nested", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := resolveConfig(baseFlags(dir))
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		// The container path bind-mounts <state>/postgres as PGDATA and requires
		// the socket dir NOT to nest under it (initdb refuses a non-empty PGDATA).
		pgdata := filepath.Join(dir, "postgres")
		if strings.Contains(cfg.DatabaseDSN, "host="+pgdata+"/") {
			t.Fatalf("default DSN %q nests the socket dir under PGDATA %q; container initdb would refuse it", cfg.DatabaseDSN, pgdata)
		}
	})
}

// TestResolveConfigCollectorFlags covers the T4 collector / --otel-external
// flags: the endpoint threads into ExternalOTLPEndpoint, the collector image
// defaults when unset, and an explicit-empty --otel-external is rejected.
func TestResolveConfigCollectorFlags(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("COMPASS_DATABASE_DSN", "")

	t.Run("otel-external threads into config", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.otelExternal = "otlp.example.com:4317"
		f.otelExternalSet = true
		cfg, err := resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.ExternalOTLPEndpoint != f.otelExternal {
			t.Errorf("ExternalOTLPEndpoint = %q, want %q", cfg.ExternalOTLPEndpoint, f.otelExternal)
		}
	})

	t.Run("otel-external explicit empty is rejected", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.otelExternal = ""
		f.otelExternalSet = true // the flag was passed with an empty value
		_, err := resolveConfig(f)
		if err == nil {
			t.Fatal("resolveConfig(--otel-external \"\") = nil error, want a rejection")
		}
		if !strings.Contains(err.Error(), "--otel-external requires an explicit OTLP endpoint") {
			t.Errorf("error = %v, want it to name the missing endpoint", err)
		}
	})

	t.Run("otel-external unset is the D3 default (empty endpoint, bundle)", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		// otelExternalSet stays false: the flag was never passed.
		cfg, err := resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.ExternalOTLPEndpoint != "" {
			t.Errorf("ExternalOTLPEndpoint = %q, want empty (bundle the collector)", cfg.ExternalOTLPEndpoint)
		}
	})

	t.Run("collector-image threads into config", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.collectorImage = "docker.io/otel/opentelemetry-collector-contrib@sha256:abc"
		cfg, err := resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.CollectorImage != f.collectorImage {
			t.Errorf("CollectorImage = %q, want %q", cfg.CollectorImage, f.collectorImage)
		}
	})

	t.Run("collector-image defaults to the pinned digest when flag-set via newFlagSet", func(t *testing.T) {
		// newFlagSet registers --collector-image with DefaultCollectorImage as
		// its default, so an unparsed flag set carries the pinned digest — the
		// resolve path passes it through unchanged.
		_, f := newFlagSet("up", true)
		f.stateDir = t.TempDir()
		f.image = "example.com/agent:latest"
		cfg, err := resolveConfig(*f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.CollectorImage != stack.DefaultCollectorImage {
			t.Errorf("CollectorImage = %q, want the pinned default %q", cfg.CollectorImage, stack.DefaultCollectorImage)
		}
	})
}

// TestResolveConfigNatsFlags covers the bundled NATS / --nats-external flags.
func TestResolveConfigNatsFlags(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("COMPASS_DATABASE_DSN", "")

	t.Run("nats-external threads into config", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.natsExternal = "nats.example.com:4222"
		f.natsExternalSet = true
		cfg, err := resolveConfig(f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.ExternalNatsURL != f.natsExternal {
			t.Errorf("ExternalNatsURL = %q, want %q", cfg.ExternalNatsURL, f.natsExternal)
		}
	})
	t.Run("nats-external explicit empty is rejected", func(t *testing.T) {
		f := baseFlags(t.TempDir())
		f.natsExternalSet = true
		_, err := resolveConfig(f)
		if err == nil {
			t.Fatal("resolveConfig(--nats-external \"\") = nil error, want a rejection")
		}
		if !strings.Contains(err.Error(), "--nats-external requires an explicit nats:// URL") {
			t.Errorf("error = %v, want it to name the missing URL", err)
		}
	})
	t.Run("nats-external unset leaves endpoint empty", func(t *testing.T) {
		cfg, err := resolveConfig(baseFlags(t.TempDir()))
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.ExternalNatsURL != "" {
			t.Errorf("ExternalNatsURL = %q, want empty (bundle NATS)", cfg.ExternalNatsURL)
		}
	})
	t.Run("nats-image defaults to the pinned digest via newFlagSet", func(t *testing.T) {
		_, f := newFlagSet("up", true)
		f.stateDir = t.TempDir()
		f.image = "example.com/agent:latest"
		cfg, err := resolveConfig(*f)
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.NatsImage != stack.DefaultNatsImage {
			t.Errorf("NatsImage = %q, want the pinned default %q", cfg.NatsImage, stack.DefaultNatsImage)
		}
	})
	t.Run("nats-external derives natsExternalSet through markExplicitFlags", func(t *testing.T) {
		fs, f := newFlagSet("up", true)
		f.stateDir = t.TempDir()
		f.image = "example.com/agent:latest"
		// Drive the real flag set end to end so the fs.Visit derivation runs.
		if err := fs.Parse([]string{"--nats-external", "", "--state-dir", f.stateDir, "--image", f.image}); err != nil {
			t.Fatalf("Parse: %v", err)
		}
		markExplicitFlags(fs, f)
		if !f.natsExternalSet {
			t.Fatalf("natsExternalSet = false after --nats-external \"\" parsed; want true")
		}
		if _, err := resolveConfig(*f); err == nil {
			t.Fatal("resolveConfig(--nats-external \"\") = nil error, want the naming rejection")
		}
	})
}

func TestRunDispatch(t *testing.T) {
	t.Run("unknown subcommand names the three", func(t *testing.T) {
		err := run([]string{"bogus"})
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got nil")
		}
		for _, want := range []string{"up", "down", "status"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name subcommand %q", err.Error(), want)
			}
		}
	})

	t.Run("empty args is a usage error", func(t *testing.T) {
		if err := run(nil); err == nil {
			t.Fatal("expected a usage error for no subcommand, got nil")
		}
	})

	t.Run("--version prints version", func(t *testing.T) {
		if err := run([]string{"--version"}); err != nil {
			t.Fatalf("run --version: %v", err)
		}
	})
}
