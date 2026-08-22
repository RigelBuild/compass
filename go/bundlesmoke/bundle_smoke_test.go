//go:build unix && bundlesmoke

// Package bundlesmoke is the automated packaged-artifact bring-up gate for the
// Compass native-app release tarball (T6.5). It is compiled ONLY under the
// `bundlesmoke` build tag, so the normal Go battery (compass-go:test, go build
// ./...) never touches it — exactly as go/e2e is fenced behind `podman`. The
// app-bundle:smoke moon task builds this commit's tarball, points
// COMPASS_BUNDLE_DIR at the app-bundle directory holding it (resolveTarball
// then globs the one artifact there), and runs this gate with the tag set.
//
// What it proves: the PACKAGED postgres + server binaries — resolved off the
// unpacked bundle's bin/ (the /nix/store symlinks the tarball ships), never a
// `go build` output and never the devenv PATH — bring the embedded stack to
// GetServerInfo-ready. That is stack.spawnChain steps 1-4 (private postgres
// up+reachable -> compass-server -> GetServerInfo readiness), the exact T4
// embedded smoke MINUS the two podman legs (agent image present + compass-runner
// enroll). Those legs need rootless podman + GHCR, which on this repo's CI live
// only in the privileged dogfood-e2e job, not the bare gates runner this bundle
// task rides; wiring the bundle there would be a ci.yml edit the frozen
// packaging design forbids (A4: the bundle rides the existing gates
// affected/full split, no workflow change). So the runner/agent-image/board
// legs stay the trimmed manual residue in app-bundle/SMOKE.md.
//
// Why this catches what dogfood-e2e cannot: dogfood-e2e brings the stack up from
// devenv-PATH binaries, where postgres's share/ and lib/ sit beside bin/ in one
// realized nix store — so a bundle that stages only bin/ still passes there. The
// packaged bundle stages share/postgresql + lib as separate store symlinks, and
// a staging gap (the defect T6.5 found, fixed in the postgres-staging PR) fails
// ONLY the packaged path. This gate drives that exact path from the tarball, so
// a staging regression reds it.
package bundlesmoke

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
	"github.com/RigelBuild/compass/go/internal/stack/adapters"
)

// bundleDirEnv names the env var the app-bundle:smoke moon task points at the
// app-bundle project directory holding THIS build's freshly built release
// tarball (compass-app-<version>-linux-amd64.tar.gz). The gate hard-fails (never
// skips) when it is unset: the bundlesmoke tag exists solely for that task,
// which always supplies the directory, so a missing value is a wiring bug, not
// an absent substrate — and a silent skip is the exact false-green class this
// repo's other gates guard against. The task passes the directory rather than
// the versioned tarball path so its command stays static (no git-sha in the
// moon file); resolveTarball globs the one artifact inside.
const bundleDirEnv = "COMPASS_BUNDLE_DIR"

// Budgets. Cold postgres init + createdb dominates (~10s observed on a dev box);
// the readiness probe answers a couple of seconds later. smokeBudget is generous
// headroom for a loaded CI runner; the moon task's -timeout exceeds it so a
// wedge surfaces as this budget's legible error, not an opaque test-binary kill.
const (
	smokeBudget  = 120 * time.Second
	drainBudget  = 30 * time.Second
	pollInterval = 500 * time.Millisecond
)

func TestBundleSmoke(t *testing.T) {
	bundleDir := os.Getenv(bundleDirEnv)
	if bundleDir == "" {
		t.Fatalf("%s is unset: this gate must be handed the app-bundle directory holding the freshly built tarball; run it via `moon run compass-app-bundle:smoke`, never a bare `go test -tags bundlesmoke`", bundleDirEnv)
	}
	tarball := resolveTarball(t, bundleDir)

	root := shortRoot(t)
	binDir, wantVersion := unpackBundle(t, tarball, root)

	// The packaged compass-postgres resolves initdb/postgres/createdb via
	// exec.LookPath, and the ProcessSupervisor resolves compass-postgres and
	// compass-server the same way, so the unpacked bundle's bin/ must lead PATH —
	// exactly the PATH="$BUNDLE/bin:$PATH" of SMOKE.md §5. This is a packaging
	// gate: nothing here may resolve a devenv-PATH or go-built binary.
	prependPATH(t, binDir)

	stateDir := filepath.Join(root, "state")
	sockDir := filepath.Join(stateDir, "pg") // the DSN host: postgres -k listens here
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("create postgres socket dir: %v", err)
	}
	// host is the unix-socket dir, so port=5432 only names the socket file
	// (.s.PGSQL.5432) — it never binds TCP, so no port contention on a shared runner.
	dsn := fmt.Sprintf("host=%s port=5432 dbname=compass sslmode=disable", sockDir)
	serverSock := filepath.Join(root, "server.sock")

	ctx, cancel := context.WithTimeout(context.Background(), smokeBudget)
	defer cancel()

	sup := adapters.NewProcessSupervisor()

	// Step 1 — private postgres, the same argv the stack supervisor spawns it
	// with (--state-dir / --database). Cleanups run LIFO, so registering pg
	// before the server drains them server-then-postgres (reverse start order,
	// matching stack.drainChildren).
	pg, err := sup.Start(ctx, stack.ProcessSpec{
		Component: stack.ComponentPostgres,
		Args:      []string{"--state-dir", stateDir, "--database", dsn},
	})
	if err != nil {
		t.Fatalf("start packaged compass-postgres: %v", err)
	}
	t.Cleanup(func() { drain(t, pg, "compass-postgres") })

	// Step 1b — wait until postgres accepts on the FULL DSN (dbname=compass): the
	// real DBProber, the exact precondition spawnChain enforces before the server
	// (compass-server's store.Open pings once, with no retry).
	dbProber := adapters.NewDBProber()
	if err := pollUntil(ctx, func(ctx context.Context) error {
		return dbProber.ProbeDB(ctx, dsn)
	}); err != nil {
		t.Fatalf("packaged postgres never accepted connections on the compass DSN within %s: %v", smokeBudget, err)
	}

	// Step 3 — compass-server, socket-only. No --listen/--tls-*: resolveNetworkDoor
	// is all-or-none, so the shipped socket-only door needs no TLS anchor. The
	// embedded stack also raises the TLS door for the runner to dial, but the
	// door binding is not packaging-sensitive (same server binary, cert minted at
	// runtime, nothing packaged), and readiness is defined over the SOCKET
	// regardless — so a socket-only bring-up is the faithful packaging reduction.
	srv, err := sup.Start(ctx, stack.ProcessSpec{
		Component: stack.ComponentServer,
		Args:      []string{"--socket", serverSock, "--database", dsn},
	})
	if err != nil {
		t.Fatalf("start packaged compass-server: %v", err)
	}
	t.Cleanup(func() { drain(t, srv, "compass-server") })

	// Step 4 — GetServerInfo readiness: the real HealthProber, the same probe
	// stack.waitReady polls. An answering probe means migrations ran and the
	// socket is serving compass.v1 — the T4 "ready" signal, from the packaged
	// server binary.
	healthProber := adapters.NewHealthProber()
	var info stack.ServerInfo
	if err := pollUntil(ctx, func(ctx context.Context) error {
		var perr error
		info, perr = healthProber.Probe(ctx, serverSock)
		return perr
	}); err != nil {
		t.Fatalf("packaged compass-server never answered GetServerInfo within %s: %v", smokeBudget, err)
	}

	// The served version is the packaged server's -ldflags stamp; it must equal
	// the bundle's own version (parsed from the tarball's dir name). Equality
	// proves the PACKAGED server answered, not an ambient compass-server a broken
	// PATH prepend could have resolved instead.
	if info.Version != wantVersion {
		t.Fatalf("GetServerInfo version = %q, want the bundle's stamped version %q (did an ambient binary answer?)", info.Version, wantVersion)
	}
	t.Logf("packaged bundle reached ready: server version %s (postgres cluster init + compass DB + migrations + compass.v1 socket, all from the tarball binaries)", info.Version)
}

// resolveTarball finds the single release tarball inside the app-bundle dir the
// moon task points at. Exactly-one is asserted: build.sh rm's the prior tarball
// before taring and stamps the name with the git sha, so a clean build leaves
// one — zero means the build dep did not run (a wiring bug), and more than one
// means a stale artifact from an earlier commit lingers, which would let the
// gate smoke the WRONG bundle. Both are hard failures, never a quiet pick.
func resolveTarball(t *testing.T, bundleDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(bundleDir, "compass-app-*-linux-amd64.tar.gz"))
	if err != nil {
		t.Fatalf("glob release tarball in %q: %v", bundleDir, err)
	}
	switch len(matches) {
	case 1:
		return matches[0]
	case 0:
		t.Fatalf("no release tarball in %q: the app-bundle:build dep must produce compass-app-<version>-linux-amd64.tar.gz before this gate runs", bundleDir)
	default:
		t.Fatalf("%d release tarballs in %q (%v): a stale artifact from an earlier commit lingers — the gate must smoke exactly one build", len(matches), bundleDir, matches)
	}
	return "" // unreachable: every switch arm above returns or t.Fatalf's
}

// shortRoot creates a short, unique, 0700 root under /tmp and reaps it. Short
// because the postgres socket dir and the server socket under it are AF_UNIX
// sun_path-budgeted; unique off the pid so a crashed run cannot collide (mirrors
// go/e2e's shortRoot).
func shortRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("/tmp", "cb"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir short root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// unpackBundle extracts the tarball under root and returns the unpacked bundle's
// bin/ directory and its version. System `tar -xzf` (not archive/tar) is
// deliberate: the bundle ships bin/{postgres,initdb,createdb}, share/postgresql,
// and lib as /nix/store SYMLINKS, and tar preserves them natively — exactly the
// extraction SMOKE.md §2 documents. The version is parsed from the single
// compass-app-<version>-linux-amd64 dir the tarball unpacks to. The staged
// postgres tree (share + lib beside bin, the staging fix) is asserted present so
// a staging regression fails HERE with a legible reason, before bring-up, rather
// than as an opaque initdb error mid-bringup.
func unpackBundle(t *testing.T, tarball, root string) (binDir, version string) {
	t.Helper()
	dest := filepath.Join(root, "unpack")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir unpack dir: %v", err)
	}
	if out, err := exec.Command("tar", "-xzf", tarball, "-C", dest).CombinedOutput(); err != nil {
		t.Fatalf("untar %q: %v\n%s", tarball, err, out)
	}

	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read unpack dir: %v", err)
	}
	var bundleDir string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "compass-app-") {
			bundleDir = e.Name()
			break
		}
	}
	if bundleDir == "" {
		t.Fatalf("tarball %q did not unpack a compass-app-* directory (got %v)", tarball, entries)
	}
	version = strings.TrimSuffix(strings.TrimPrefix(bundleDir, "compass-app-"), "-linux-amd64")
	if version == bundleDir || version == "" {
		t.Fatalf("bundle dir %q does not match compass-app-<version>-linux-amd64", bundleDir)
	}

	bundleRoot := filepath.Join(dest, bundleDir)
	binDir = filepath.Join(bundleRoot, "bin")

	// Layout assertions: the five app binaries + the three staged postgres tools.
	for _, b := range []string{
		"compass-app", "compass-stack", "compass-server", "compass-runner", "compass-postgres",
		"postgres", "initdb", "createdb",
	} {
		if _, err := os.Stat(filepath.Join(binDir, b)); err != nil {
			t.Fatalf("bundle missing bin/%s: %v", b, err)
		}
	}
	// The staged postgres support tree (share + lib beside bin/): the staging fix
	// this smoke exists to defend. A miss here is the exact defect T6.5 found.
	for _, d := range []string{"share/postgresql", "lib"} {
		if _, err := os.Stat(filepath.Join(bundleRoot, d)); err != nil {
			t.Fatalf("bundle missing %s (postgres staging regressed — the fix stages share+lib beside bin/): %v", d, err)
		}
	}
	return binDir, version
}

// prependPATH puts dir at the front of PATH for this test via t.Setenv, which
// restores the prior value on cleanup automatically. The package's only test is
// non-parallel, so t.Setenv (which forbids use after t.Parallel) is safe.
func prependPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// pollUntil calls probe every pollInterval until it returns nil or ctx is done,
// mirroring stack.waitPostgres/waitReady. The last probe error is wrapped into
// the timeout so the caller can name the phase that never became ready.
func pollUntil(ctx context.Context, probe func(context.Context) error) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var last error
	for {
		if last = probe(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last probe: %v)", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// drain stops a smoke child gracefully via the real Process seam (SIGTERM to its
// group, then Wait), as stack.drainChildren does. Best-effort: teardown of the
// smoke's own children never fails the test, but a stop error is logged.
func drain(t *testing.T, p stack.Process, name string) {
	t.Helper()
	if err := p.Signal(stack.SignalTerm); err != nil {
		t.Logf("drain %s: signal: %v", name, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainBudget)
	defer cancel()
	if err := p.Wait(ctx); err != nil {
		t.Logf("drain %s: wait: %v", name, err)
	}
}
