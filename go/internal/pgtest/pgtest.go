// Package pgtest is the shared real-Postgres test harness for the compass
// backend. A store of record is only proven against the database it targets, so
// every store/handler/serve integration test runs against an actual Postgres
// (design.md:1188-1190) — there is no mock. This package owns the one copy of
// the container orchestration those suites share: acquire a database (an
// already-running one via COMPASS_TEST_DATABASE_DSN, else a throwaway container
// started through the container CLI), reset its schema between tests, and SKIP
// (never fail) when no container runtime is usable, so the hermetic gate stays
// green in a container-less sandbox while the assertions are real wherever a
// runtime exists.
//
// It is deliberately store-agnostic: it hands back a DSN, and each package opens
// its own store against it. That keeps this package free of any dependency on
// the packages it serves (no import cycle) and makes it reusable by the store,
// comms, and server suites alike.
//
// It is build-tagged `pgtest` so it is compiled only into that opt-in test lane,
// never the default `go test` gate. It is a non-test file (not `_test.go`) so it
// is importable across packages; the build tag keeps it out of production
// binaries all the same.
//go:build pgtest

package pgtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgImage is the Postgres image the harness runs. A pinned major keeps the
// generated-tsvector + websearch_to_tsquery behavior the tests assert stable.
const pgImage = "docker.io/library/postgres:16-alpine"

// DSNEnvVar, when set, points the harness at an already-running Postgres and
// suppresses container management — the CI-service path (design.md:1188).
const DSNEnvVar = "COMPASS_TEST_DATABASE_DSN"

// schemaSeq makes each per-test schema name unique within a process, so tests
// that share one COMPASS_TEST_DATABASE_DSN database never collide even when the
// go test runner runs their packages in parallel.
var schemaSeq atomic.Uint64

// RequireDSN returns a DSN addressing a freshly-created, empty schema, ready for
// a caller to migrate by opening its store against it. It uses
// COMPASS_TEST_DATABASE_DSN when set — giving each test its OWN uniquely-named
// schema on that shared database (via search_path), dropped on cleanup, so
// parallel tests and packages are fully isolated — else starts a throwaway
// container (already private, no sharing). It SKIPS the test when neither a DSN
// nor a container runtime is available, so the suite stays green in a
// container-less sandbox.
//
// Per-test schema isolation (not a shared "public" reset) is what lets the
// default `go test ./...` run every package's real-Postgres suite concurrently
// against one database without one test's schema teardown wiping another's
// tables mid-run.
func RequireDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv(DSNEnvVar); dsn != "" {
		return isolatedSchemaDSN(t, dsn)
	}
	cli := containerCLI()
	if cli == "" {
		t.Skip("no COMPASS_TEST_DATABASE_DSN and no podman/docker; skipping real-Postgres test")
	}
	return startContainer(t, cli)
}

// isolatedSchemaDSN creates a fresh, uniquely-named schema on the database dsn
// points at, registers its CASCADE drop on test cleanup, and returns a DSN whose
// search_path selects that schema so the caller's store.Open migrates into it in
// isolation. The migrations use unqualified table names, so a per-connection
// search_path fully scopes them to this test's schema.
func isolatedSchemaDSN(t *testing.T, dsn string) string {
	t.Helper()
	schema := fmt.Sprintf("pgtest_%d_%d", os.Getpid(), schemaSeq.Add(1))
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: connect to create schema: %v", err)
	}
	// quoteIdent is unnecessary here: schema is a fixed-format identifier
	// (pgtest_<pid>_<seq>), no user input, no special chars.
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("pgtest: create schema %s: %v", schema, err)
	}
	_ = conn.Close(ctx)

	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return // best-effort teardown; a dropped connection at exit is harmless
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	})

	return withSearchPath(dsn, schema)
}

// withSearchPath returns dsn with its connection search_path set to schema, so
// every pooled connection store.Open makes resolves unqualified names to that
// schema. It threads the setting through the libpq `options` parameter
// (-c search_path=…), appended to any existing query string.
//
// A DSN that already carries an `options` parameter is rejected (panic): libpq
// keeps only one `options`, so appending a second would silently drop the
// per-test search_path and cross-contaminate parallel tests. A loud failure at
// setup beats a silent isolation loss that surfaces as a flaky, cross-talking
// suite. The DSNs this harness builds never set `options`, so this guards
// against an operator-supplied COMPASS_TEST_DATABASE_DSN that does.
func withSearchPath(dsn, schema string) string {
	if strings.Contains(dsn, "options=") {
		panic("pgtest: COMPASS_TEST_DATABASE_DSN already sets an 'options' parameter; " +
			"per-test schema isolation threads search_path through 'options' and cannot merge with an existing one")
	}
	opt := "options=-c%20search_path%3D" + schema
	if strings.Contains(dsn, "?") {
		return dsn + "&" + opt
	}
	return dsn + "?" + opt
}

// containerCLI returns the container runtime binary to drive (podman preferred,
// docker fallback), or "" if neither is on PATH.
func containerCLI() string {
	for _, c := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return ""
}

// startContainer launches a throwaway Postgres container, waits for it to accept
// connections, and returns its DSN. The container is force-removed on cleanup.
func startContainer(t *testing.T, cli string) string {
	t.Helper()
	name := "compass-test-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const (
		password = "compass-test"
		port     = "5432"
	)

	// -P publishes to an ephemeral host port so parallel runs don't collide.
	out, err := exec.Command(cli, "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=compass",
		"-P",
		pgImage,
	).CombinedOutput()
	if err != nil {
		t.Skipf("cannot start postgres container (%s): %v\n%s", cli, err, out)
	}
	t.Cleanup(func() { _ = exec.Command(cli, "rm", "--force", name).Run() })

	hostPort := publishedPort(t, cli, name, port)
	dsn := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/compass?sslmode=disable", password, hostPort)
	waitReady(t, dsn)
	return dsn
}

// publishedPort reads the ephemeral host port the container's Postgres port maps
// to.
func publishedPort(t *testing.T, cli, name, containerPort string) string {
	t.Helper()
	out, err := exec.Command(cli, "port", name, containerPort).CombinedOutput()
	if err != nil {
		t.Fatalf("pgtest: read published port: %v\n%s", err, out)
	}
	// Output like "0.0.0.0:49153" (possibly several lines); take the last field
	// of the first line after the colon.
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		t.Fatalf("pgtest: unexpected port mapping output: %q", line)
	}
	return line[idx+1:]
}

// waitReady polls until Postgres accepts a connection or the deadline passes.
func waitReady(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			pingErr := conn.Ping(ctx)
			_ = conn.Close(ctx)
			if pingErr == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pgtest: postgres not ready within deadline: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
