// Package pgshare acquires real Postgres and isolates tests with per-test
// schemas. It accepts shared DSNs or explicitly opted-in containers; missing
// runtimes skip unless live Postgres is required, and a present unconfigured
// runtime fails rather than silently selecting a slower container path.

//go:build (pgtest || podman) && unix

package pgshare

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // postgres test harness: LookPath + container-CLI (podman/docker) subprocess management
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgImage pins the major and digest so local and CI suites use the same Postgres
// behavior. Keep it equal to the pgtest service image in CI.
const pgImage = "docker.io/library/postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"

// dsnEnvVar points to shared Postgres; pgtest re-exports the same contract.
const dsnEnvVar = "COMPASS_TEST_DATABASE_DSN"

// useContainerEnvVar, when set to a non-empty value, opts the harness into
// starting a throwaway per-test Postgres container when COMPASS_TEST_DATABASE_DSN
// is unset. The container path is ~500x slower than a shared DSN and silently
// changes what the suite measures, so it is opt-in rather than a fallback.
const useContainerEnvVar = "COMPASS_TEST_USE_CONTAINER"

// requireLiveEnvVar turns the no-runtime skip into failure when live Postgres is
// mandatory, so CI cannot pass without exercising the database.
const requireLiveEnvVar = "COMPASS_REQUIRE_LIVE"

// dsnSource is which of the five database-acquisition paths AcquireDSN takes.
type dsnSource int

const (
	sourceSharedSchema      dsnSource = iota // COMPASS_TEST_DATABASE_DSN is set
	sourceSkipNoRuntime                      // no DSN, no container runtime
	sourceFailMisconfigured                  // no DSN, runtime present, no opt-in
	sourceContainer                          // no DSN, runtime present, opt-in
	sourceFailRequireLive                    // no DSN, no runtime, COMPASS_REQUIRE_LIVE set
)

// decideDSNSource is the pure policy behind AcquireDSN, split out so the dispatch
// is unit-testable without a real Postgres or *testing.T.
func decideDSNSource(dsn, useContainer, cli, requireLive string) dsnSource {
	if dsn != "" {
		return sourceSharedSchema
	}
	if cli == "" {
		if requireLive != "" {
			return sourceFailRequireLive
		}
		return sourceSkipNoRuntime
	}
	if useContainer == "" {
		return sourceFailMisconfigured
	}
	return sourceContainer
}

// AcquireDSN returns a DSN addressing a freshly-created, empty schema, ready for
// a caller to migrate by opening its store against it. It uses
// COMPASS_TEST_DATABASE_DSN when set — giving each test its OWN uniquely-named
// schema on that shared database (via search_path), dropped on cleanup, so
// parallel tests and packages are fully isolated. With no DSN set, it SKIPS the
// test when no container runtime is available, so the suite stays green in a
// container-less sandbox. When a runtime IS present but no DSN is set, it FAILS
// LOUDLY unless COMPASS_TEST_USE_CONTAINER is set: the ~500x-slower throwaway
// container path is opt-in, never a silent fallback that changes what the suite
// measures. When COMPASS_REQUIRE_LIVE is set and neither a DSN nor a container
// runtime is available, the skip becomes a hard failure instead (see
// requireLiveEnvVar) — for contexts like CI where a live database is mandatory.
func AcquireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnvVar)
	cli := containerCLI()
	switch decideDSNSource(dsn, os.Getenv(useContainerEnvVar), cli, os.Getenv(requireLiveEnvVar)) {
	case sourceSharedSchema:
		return IsolatedSchemaDSN(t, dsn)
	case sourceSkipNoRuntime:
		t.Skip("no COMPASS_TEST_DATABASE_DSN and no podman/docker; skipping real-Postgres test")
	case sourceFailMisconfigured:
		t.Fatalf("COMPASS_TEST_DATABASE_DSN is unset but a container runtime (%s) is present: "+
			"set COMPASS_TEST_DATABASE_DSN to a running Postgres for the fast shared-schema path, "+
			"or set COMPASS_TEST_USE_CONTAINER=1 to opt into a throwaway per-test container (~500x slower)", cli)
	case sourceContainer:
		return startContainer(t, cli)
	case sourceFailRequireLive:
		t.Fatalf("%s=%s requires a live Postgres but COMPASS_TEST_DATABASE_DSN is unset and no "+
			"container runtime (podman/docker) is present: set COMPASS_TEST_DATABASE_DSN to a "+
			"running Postgres", requireLiveEnvVar, os.Getenv(requireLiveEnvVar))
	}
	return ""
}

// schemaSeq makes each per-test schema and database name unique within a
// process, so tests sharing one COMPASS_TEST_DATABASE_DSN server never collide
// even when the go test runner runs their packages in parallel.
var schemaSeq atomic.Uint64

// IsolatedSchemaDSN creates a fresh, uniquely-named schema on the database dsn
// points at, registers its CASCADE drop on test cleanup, and returns a DSN whose
// search_path selects that schema so the caller's store.Open migrates into it in
// isolation. The migrations use unqualified table names, so a per-connection
// search_path fully scopes them to this test's schema.
func IsolatedSchemaDSN(t *testing.T, dsn string) string {
	t.Helper()
	schema := fmt.Sprintf("pgtest_%d_%d", os.Getpid(), schemaSeq.Add(1))
	ctx := t.Context()

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
		cctx := context.WithoutCancel(ctx)
		c, err := pgx.Connect(cctx, dsn)
		if err != nil {
			return // best-effort teardown; a dropped connection at exit is harmless
		}
		defer func() { _ = c.Close(cctx) }()
		_, _ = c.Exec(cctx, "DROP SCHEMA "+schema+" CASCADE")
	})

	return withSearchPath(dsn, schema)
}

// withSearchPath adds a per-test schema through libpq's options parameter.
// Reject an existing options value: libpq would discard one, breaking isolation.
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

// IsolatedDatabase creates a fresh, uniquely-named database on the server
// dsn points at, registers its forced drop on test cleanup, and returns its
// name. It isolates callers that cannot use IsolatedSchemaDSN, such as a
// connection pooler that rejects the libpq options startup parameter.
func IsolatedDatabase(t *testing.T, dsn string) string {
	t.Helper()
	name := fmt.Sprintf("pgtest_db_%d_%d", os.Getpid(), schemaSeq.Add(1))
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pgtest: connect to create database: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	// name is a fixed-format identifier, no user input.
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		// t.Context is canceled before Cleanup runs; keep its values, not its deadline.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return // best-effort teardown, as IsolatedSchemaDSN
		}
		defer func() { _ = c.Close(ctx) }()
		// FORCE ends any idle server connection a pooler still holds to it.
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return name
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

// startContainer launches opted-in disposable Postgres and removes its volume.
// Startup failures are infrastructure failures, not skips.
func startContainer(t *testing.T, cli string) string {
	t.Helper()
	ctx := t.Context()
	name := "compass-test-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const (
		password = "compass-test"
		port     = "5432"
	)

	// -P publishes to an ephemeral host port so parallel runs don't collide.
	//nolint:gosec // G204: containerCLI LookPath-resolves the runtime; all arguments are fixture-generated.
	out, err := exec.CommandContext(ctx, cli, "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_PASSWORD="+password,
		"-e", "POSTGRES_DB=compass",
		"-P",
		pgImage,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("pgtest: COMPASS_TEST_USE_CONTAINER is set but the postgres container failed to start (%s): %v\n%s", cli, err, out)
	}
	// removeContainerArgs adds --volumes so the container's anonymous data volume
	// is removed with it (see removeContainerArgs for why a bare rm --force
	// leaks).
	t.Cleanup(func() {
		// t.Context() is canceled before cleanup runs; the removal must still complete.
		//nolint:gosec // G204: runtime is LookPath-resolved; this removes only the fixture-minted container name.
		_ = exec.CommandContext(context.WithoutCancel(ctx), cli, removeContainerArgs(name)...).Run()
	})
	hostPort := publishedPort(t, ctx, cli, name, port)
	dsn := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/compass?sslmode=disable", password, hostPort)
	waitReady(t, ctx, dsn)
	return dsn
}

// removeContainerArgs removes the container and its anonymous data volume.
// Omitting --volumes leaks postgres volumes; keep independent runtime args in sync.
func removeContainerArgs(name string) []string {
	return []string{"rm", "--force", "--volumes", name}
}

// publishedPort reads the ephemeral host port the container's Postgres port maps
// to.
func publishedPort(t *testing.T, ctx context.Context, cli, name, containerPort string) string {
	t.Helper()
	//nolint:gosec // G204: containerCLI LookPath-resolves the runtime; the name and port are fixture-generated.
	out, err := exec.CommandContext(ctx, cli, "port", name, containerPort).CombinedOutput()
	if err != nil {
		t.Fatalf("pgtest: read published port: %v\n%s", err, out)
	}
	// Output like "0.0.0.0:49153" (possibly several lines); take the last field
	// of the first line after the colon.
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		t.Fatalf("pgtest: unexpected port mapping output: %q", line)
	}
	return line[idx+1:]
}

// waitReady polls until Postgres accepts a connection or the deadline passes.
func waitReady(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
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
		time.Sleep(250 * time.Millisecond) //nolint:forbidigo // bounded poll tick; event-gated on the postgres Ping above with a deadline (rule://go-no-sleep-in-test poll-until exemption)
	}
}

// suitePGBinary is the PATH-resolved wrapper used to start private test Postgres.
const suitePGBinary = "compass-postgres"

// The bounded connect poll allows cold init while exiting as soon as the child
// stops; readiness is based on a successful connection, not elapsed time.
const (
	suiteReadyBudget = 30 * time.Second
	suiteReadyTick   = 250 * time.Millisecond
)

// StartSuitePostgres brings up a private Postgres for a whole suite (TestMain /
// package-level fixture) by exec'ing the on-PATH compass-postgres wrapper on the
// socket described by (sockDir, port), waits for it to accept connections, and
// returns the base DSN plus a stop that SIGTERMs the child and waits it out. It
// fails the test loudly on any bring-up error — a suite fixture that cannot start
// its database has nothing to test. For a TestMain context with no *testing.TB,
// use StartSuitePostgresMain, which returns the error instead.
func StartSuitePostgres(tb testing.TB, stateDir, sockDir string, port int) (dsn string, stop func()) {
	tb.Helper()
	dsn, stop, err := startSuitePostgres(tb.Context(), stateDir, sockDir, port)
	if err != nil {
		tb.Fatalf("pgshare: start suite postgres: %v", err)
	}
	return dsn, stop
}

// StartSuitePostgresMain is the *testing.TB-free variant of StartSuitePostgres
// for TestMain, where there is no test to fail: it returns an error instead of
// failing a t, so the caller can os.Exit(1) after cleanup. On success it returns
// the base DSN and a stop that SIGTERMs the child and waits it; on failure it
// returns a non-nil error and a nil stop, having already reaped any child it
// started.
func StartSuitePostgresMain(ctx context.Context, stateDir, sockDir string, port int) (string, func(), error) {
	return startSuitePostgres(ctx, stateDir, sockDir, port)
}

// startSuitePostgres shares startup for TB and TestMain callers. It reaps a
// started child on failure and returns a stop function only after readiness.
func startSuitePostgres(ctx context.Context, stateDir, sockDir string, port int) (string, func(), error) {
	if stateDir == "" {
		return "", nil, errors.New("pgshare: a state directory is required")
	}
	bin, err := exec.LookPath(suitePGBinary)
	if err != nil {
		return "", nil, fmt.Errorf("pgshare: %s not on PATH: %w", suitePGBinary, err)
	}
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return "", nil, fmt.Errorf("pgshare: create socket dir %s: %w", sockDir, err)
	}

	// The supervisor's keyword/value DSN (cmd/compass-postgres/main.go): the host
	// is the socket DIRECTORY postgres -k listens on, and the wrapper binds
	// <sockDir>/.s.PGSQL.<port>.
	dsn := fmt.Sprintf("host=%s port=%d dbname=compass sslmode=disable", sockDir, port)

	// The wrapper outlives this startup call and is stopped explicitly by its owner.
	// Preserve caller values while detaching its lifetime from the startup context.
	//nolint:gosec // G204: the wrapper is LookPath-resolved; arguments are suite-fixture paths and a constructed DSN.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), bin, "--state-dir", stateDir, "--database", dsn)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("pgshare: start %s: %w", suitePGBinary, err)
	}

	// Reap the child in one place. done closes once Wait returns; waitErr is then
	// safe to read (the receive establishes happens-before). The stop we hand back
	// and the exit short-circuit in waitSuiteReady both synchronize on done, so
	// Wait is called exactly once.
	var waitErr error
	done := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
	}

	if err := waitSuiteReady(ctx, dsn, done, &waitErr); err != nil {
		stop() // idempotent SIGTERM; the child is already gone on the exit path
		return "", nil, err
	}
	return dsn, stop, nil
}

// waitSuiteReady polls for a successful connection, but returns promptly if the
// child exits. done synchronizes access to waitErr after Wait completes.
func waitSuiteReady(ctx context.Context, dsn string, done <-chan struct{}, waitErr *error) error {
	deadline := time.Now().Add(suiteReadyBudget)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			pingErr := conn.Ping(ctx)
			_ = conn.Close(ctx)
			if pingErr == nil {
				return nil
			}
		}
		select {
		case <-done:
			return fmt.Errorf("pgshare: %s exited before accepting connections: %w", suitePGBinary, *waitErr)
		case <-ctx.Done():
			return fmt.Errorf("pgshare: waiting for %s: %w", suitePGBinary, ctx.Err())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pgshare: %s not ready within %s: %w", suitePGBinary, suiteReadyBudget, err)
		}
		time.Sleep(suiteReadyTick) //nolint:forbidigo // bounded poll tick; event-gated on the suite postgres accepting connections above, with a deadline + done-channel (rule://go-no-sleep-in-test poll-until exemption)
	}
}
