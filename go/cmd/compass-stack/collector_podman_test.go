//go:build podman

package main

// T4 (RIG-2825) bundled Plane-B fan-in OTel Collector integration proof,
// mirroring container_postgres_podman_test.go's cross-process harness: it drives
// the REAL compass-stack binary as separate up/down processes against a REAL
// rootless podman collector container, and proves the T4 supervised-component
// path end to end:
//
//	up -> collector container exists + health :13133 answers 200 ->
//	fresh-process down -> collector container gone (`podman container exists`
//	false) -> pgid record removed.
//
// Plus the D3 --otel-external opt-out: `up --otel-external <endpoint>` brings the
// stack up WITHOUT starting a bundled collector component, and no collector
// container is ever created.
//
// Build-tagged `podman` (out of the hermetic unit lane) and podmanUsable()-guarded
// so a container-less sandbox skips rather than fails.
//
// PROCESS SAFETY (rule://process-safety): the collector container is torn down
// only by `compass-stack down` (reads the stack's OWN v2 pgid record, drives
// podman stop/rm by the recorded name) or by an explicit podman-rm of a
// container THIS test created by a unique derived name — never a pattern/name
// scan kill.
//
// DETERMINISM: no blind sleeps for readiness — up returns only after Ready, so
// its exit 0 is the gate; every post-condition polls an event (the container's
// presence, the health endpoint, the pgid file) under a bounded budget.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// collectorImagePinned is the same pinned collector digest the stack defaults to
// (stack.DefaultCollectorImage). Referenced by literal here because the test
// drives the CLI which resolves the default itself; kept equal so the test
// exercises the shipped image, not a drift.
const collectorImagePinned = "docker.io/otel/opentelemetry-collector-contrib@sha256:ffc818ee108d0b934fd14207fd87ff247c9b64f344ed349d0b66c166c18a2312"

// collectorHealthEndpoint is the host loopback endpoint the collector's
// health_check extension is published on by the T4 run spec (the fixed
// 127.0.0.1:13133). The post-up health poll GETs it.
const collectorHealthEndpoint = "http://127.0.0.1:13133/"

// collectorHealthyBudget bounds the post-up poll for the health endpoint to
// answer 200. up returns after Ready (which already gated on collector health),
// so this is a fast confirmation, not the readiness wait.
const collectorHealthyBudget = 30 * time.Second

// TestCollectorUpDown drives the T4 bundled-collector path end to end.
func TestCollectorUpDown(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background() // test root context (rule://go-thread-context exemption for a _test.go root)

	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(binDir)

	fx := newContainerFixture(t, shortRoot(t, "-col"))
	cfg := fx.cfg
	recordPath := filepath.Join(cfg.StateDir, pgidRecordName)
	pgName := derivedContainerName(cfg.StateDir)
	colName := derivedCollectorName(cfg.StateDir)

	// Cleanup guard: a fresh down (tears both containers down by recorded name),
	// then a belt-and-suspenders explicit rm of the two containers THIS test
	// would have created, by their exact unique names (never a scan). Both
	// best-effort: a guard failure must not fail an already-finished test.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), downBudget)
		defer cancel()
		if out, runErr, infraErr := runStack(cctx, t, stackBin, env, cfg.args("down")...); infraErr != nil {
			t.Logf("cleanup guard: down harness error (ignored): %v", infraErr)
		} else if runErr != nil {
			t.Logf("cleanup guard: down exited non-zero (ignored): %v\n%s", runErr, out)
		}
		for _, n := range []string{colName, pgName} {
			if out, err := exec.Command("podman", "rm", "--force", "--volumes", n).CombinedOutput(); err != nil {
				t.Logf("cleanup guard: podman rm %s (ignored): %v\n%s", n, err, out)
			}
		}
	})

	// up with the pinned container images (postgres + collector both bundled).
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env,
		cfg.args("up", "--postgres-image", pgImagePinned, "--collector-image", collectorImagePinned, "--linger")...)
	if err != nil {
		t.Fatalf("compass-stack up (bundled collector): %v\n%s", err, out)
	}

	// The stack is live and the collector container EXISTS under the derived
	// name, its health endpoint answering 200 (the exact readiness gate up ran).
	waitServerAnswering(t, fx.deps, cfg.SocketPath)
	if !containerExists(t, colName) {
		t.Fatalf("collector container %q not present after up", colName)
	}
	waitCollectorHealthy(t, collectorHealthEndpoint, collectorHealthyBudget)

	// The pgid record carries the collector as a container (ctr) entry keyed by
	// the derived name.
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("stack.pgids record %q missing after up: %v", recordPath, err)
	}
	assertRecordHasCollectorEntry(t, recordPath, colName)

	// Fresh-process down: reads the v2 record, tears the collector down by name.
	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, cfg.args("down")...)
	if err != nil {
		t.Fatalf("compass-stack down (bundled collector): %v\n%s", err, out)
	}

	// The collector container is gone and the record removed.
	waitContainerGone(t, colName, containerGoneBudget)
	assertServerGone(t, fx.deps, cfg.SocketPath)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("stack.pgids record %q still present after a full down: stat err = %v", recordPath, err)
	}
}

// TestExternalOTLPUpDown drives the D3 --otel-external opt-out: the stack comes
// up WITHOUT a bundled collector, and no collector container is ever created.
func TestExternalOTLPUpDown(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background() // test root context (rule://go-thread-context exemption for a _test.go root)

	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(binDir)

	fx := newContainerFixture(t, shortRoot(t, "-extotel"))
	cfg := fx.cfg
	pgName := derivedContainerName(cfg.StateDir)
	colName := derivedCollectorName(cfg.StateDir)

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), downBudget)
		defer cancel()
		if out, runErr, infraErr := runStack(cctx, t, stackBin, env, cfg.args("down")...); infraErr != nil {
			t.Logf("cleanup guard: down harness error (ignored): %v", infraErr)
		} else if runErr != nil {
			t.Logf("cleanup guard: down exited non-zero (ignored): %v\n%s", runErr, out)
		}
		for _, n := range []string{colName, pgName} {
			if out, err := exec.Command("podman", "rm", "--force", "--volumes", n).CombinedOutput(); err != nil {
				t.Logf("cleanup guard: podman rm %s (ignored): %v\n%s", n, err, out)
			}
		}
	})

	// up with --otel-external: postgres still bundled, collector NOT.
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env,
		cfg.args("up", "--postgres-image", pgImagePinned, "--otel-external", "127.0.0.1:4317", "--linger")...)
	if err != nil {
		t.Fatalf("compass-stack up (--otel-external): %v\n%s", err, out)
	}

	waitServerAnswering(t, fx.deps, cfg.SocketPath)
	// No bundled collector container was created.
	if containerExists(t, colName) {
		t.Fatalf("collector container %q present on the --otel-external path; want none", colName)
	}

	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, cfg.args("down")...)
	if err != nil {
		t.Fatalf("compass-stack down (--otel-external): %v\n%s", err, out)
	}
	assertServerGone(t, fx.deps, cfg.SocketPath)
}

// derivedCollectorName reproduces stack.collectorContainerName (package-internal
// to stack): sha256 of the cleaned state dir, first 6 bytes hex, with the
// compass-otel-collector- prefix. Kept in lockstep with the production
// derivation so the test asserts against the exact name a fresh down
// reconstructs.
func derivedCollectorName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-otel-collector-" + hex.EncodeToString(sum[:6])
}

// waitCollectorHealthy polls until the collector health endpoint answers 200 or
// the budget elapses — the event-gate for "the collector is receiving", never a
// blind sleep. up already gated on this, so it is a fast confirmation.
func waitCollectorHealthy(t *testing.T, url string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		if collectorHealthOK(client, url) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector health %q not 200 within %s", url, budget)
		}
		time.Sleep(answerPollInterval) //nolint:forbidigo // bounded poll tick, event-gated by the deadline above (rule://go-no-sleep-in-test poll-until exemption, mirroring waitDSNReachable)
	}
}

// collectorHealthOK reports whether a GET of url returns 200. The body is
// unused; it is closed so the probe leaks no connection.
func collectorHealthOK(client *http.Client, url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close() // health verdict is the status code; a close error on a probe is not actionable
	return resp.StatusCode == http.StatusOK
}

// assertRecordHasCollectorEntry parses the v2 pgid record and requires the
// collector line to be a container (ctr) entry with the derived name. The record
// grammar is stack-internal; the test parses the lines directly (read-only),
// reusing recordLines from the postgres integration test (same package).
func assertRecordHasCollectorEntry(t *testing.T, recordPath, name string) {
	t.Helper()
	for _, f := range recordLines(t, recordPath) {
		if len(f) >= 3 && f[0] == "ctr" && f[1] == "otel-collector" {
			if f[2] != name {
				t.Fatalf("collector entry name = %q, want %q", f[2], name)
			}
			return
		}
	}
	t.Fatalf("no `ctr otel-collector <name>` entry in %q", recordPath)
}
