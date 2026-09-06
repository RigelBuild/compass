//go:build podman

package main

// T3 (RIG-3107) bundled NATS integration proof, mirroring the cross-process
// harness used by the bundled OTel Collector leg: it drives the REAL
// compass-stack binary as separate up/down processes against REAL rootless
// podman containers and proves the supervised NATS path end to end:
//
//	up -> NATS container exists + health :8222 answers 200 -> JetStream posture
//	     matches the rendered store directory and sync interval -> fresh-process
//	down -> NATS container gone -> pgid record removed.
//
// Build-tagged `podman` (out of the hermetic unit lane) and podmanUsable()-guarded
// so a container-less sandbox skips rather than fails.
//
// PROCESS SAFETY (rule://process-safety): the NATS container is torn down only
// by `compass-stack down` (reads the stack's OWN v2 pgid record, drives podman
// stop/rm by the recorded name) or by an explicit podman-rm of a container THIS
// test created by a unique derived name — never a pattern/name scan.
//
// DETERMINISM: no blind sleeps for readiness — up returns only after Ready, so
// its exit 0 is the gate; every post-condition polls an event under a bounded
// budget.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// natsHealthEndpoint is the host loopback endpoint on the NATS monitor port
// used by the bundled server's /healthz readiness gate.
const natsHealthEndpoint = "http://127.0.0.1:8222/healthz"

// natsVarzEndpoint is the host loopback NATS /varz endpoint used to inspect
// the rendered JetStream configuration.
const natsVarzEndpoint = "http://127.0.0.1:8222/varz"

// natsHealthyBudget bounds the post-up health confirmation. up already gated on
// readiness, so this is a fast confirmation, not the readiness wait.
const natsHealthyBudget = 30 * time.Second

// TestNatsUpDown drives the bundled-NATS path end to end.
func TestNatsUpDown(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background() // test root context (rule://go-thread-context exemption for a _test.go root)

	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(binDir)

	fx := newContainerFixture(t, shortRoot(t, "-nats"))
	cfg := fx.cfg
	recordPath := filepath.Join(cfg.StateDir, pgidRecordName)
	pgName := derivedContainerName(cfg.StateDir)
	natsName := derivedNatsName(cfg.StateDir)

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
		for _, n := range []string{natsName, pgName} {
			if out, err := exec.Command("podman", "rm", "--force", "--volumes", n).CombinedOutput(); err != nil {
				t.Logf("cleanup guard: podman rm %s (ignored): %v\n%s", n, err, out)
			}
		}
	})

	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env,
		cfg.args("up", "--postgres-image", pgImagePinned, "--otel-external", "127.0.0.1:4317", "--linger")...)
	if err != nil {
		t.Fatalf("compass-stack up (bundled NATS): %v\n%s", err, out)
	}

	waitServerAnswering(t, fx.deps, cfg.SocketPath)
	if !containerExists(t, natsName) {
		t.Fatalf("NATS container %q not present after up", natsName)
	}
	waitNatsHealthy(t, natsHealthEndpoint, natsHealthyBudget)
	assertNatsJetStreamPosture(t, natsVarzEndpoint)
	if _, err := os.Stat(recordPath); err != nil {
		t.Fatalf("stack.pgids record %q missing after up: %v", recordPath, err)
	}
	assertRecordHasNatsEntry(t, recordPath, natsName)

	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, cfg.args("down")...)
	if err != nil {
		t.Fatalf("compass-stack down (bundled NATS): %v\n%s", err, out)
	}

	waitContainerGone(t, natsName, containerGoneBudget)
	assertServerGone(t, fx.deps, cfg.SocketPath)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("stack.pgids record %q still present after a full down: stat err = %v", recordPath, err)
	}
}

// TestExternalNatsUpDown drives the --nats-external opt-out against REAL podman:
// the stack comes up WITHOUT a bundled NATS, and no NATS container is ever
// created. The hermetic seam (internal/stack TestExternalNatsSkipsNats) already
// gates the spawn-chain decision; this is its real-container counterpart, the
// negative sibling of TestNatsUpDown, mirroring the collector's
// TestExternalOTLPUpDown.
func TestExternalNatsUpDown(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background() // test root context (rule://go-thread-context exemption for a _test.go root)

	binDir := buildBinariesFromModuleRoot(t)
	stackBin := buildStackBinary(t, binDir)
	env := stackEnv(binDir)

	fx := newContainerFixture(t, shortRoot(t, "-extnats"))
	cfg := fx.cfg
	pgName := derivedContainerName(cfg.StateDir)
	natsName := derivedNatsName(cfg.StateDir)

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), downBudget)
		defer cancel()
		if out, runErr, infraErr := runStack(cctx, t, stackBin, env, cfg.args("down")...); infraErr != nil {
			t.Logf("cleanup guard: down harness error (ignored): %v", infraErr)
		} else if runErr != nil {
			t.Logf("cleanup guard: down exited non-zero (ignored): %v\n%s", runErr, out)
		}
		for _, n := range []string{natsName, pgName} {
			if out, err := exec.Command("podman", "rm", "--force", "--volumes", n).CombinedOutput(); err != nil {
				t.Logf("cleanup guard: podman rm %s (ignored): %v\n%s", n, err, out)
			}
		}
	})

	// up with --nats-external: postgres still bundled, NATS NOT. --otel-external
	// too so the collector (not the subject here) also stays unbundled.
	upCtx, upCancel := context.WithTimeout(ctx, upBudget)
	defer upCancel()
	out, err := mustRunStack(upCtx, t, stackBin, env,
		cfg.args("up", "--postgres-image", pgImagePinned, "--otel-external", "127.0.0.1:4317", "--nats-external", "nats://127.0.0.1:4222", "--linger")...)
	if err != nil {
		t.Fatalf("compass-stack up (--nats-external): %v\n%s", err, out)
	}

	waitServerAnswering(t, fx.deps, cfg.SocketPath)
	// No bundled NATS container was created on the opt-out path.
	if containerExists(t, natsName) {
		t.Fatalf("NATS container %q present on the --nats-external path; want none", natsName)
	}

	downCtx, downCancel := context.WithTimeout(ctx, downBudget)
	defer downCancel()
	out, err = mustRunStack(downCtx, t, stackBin, env, cfg.args("down")...)
	if err != nil {
		t.Fatalf("compass-stack down (--nats-external): %v\n%s", err, out)
	}
	assertServerGone(t, fx.deps, cfg.SocketPath)
}

// derivedNatsName reproduces stack.natsContainerName (package-internal): the
// cleaned state directory's sha256 prefix determines the unique name. Kept in
// lockstep with production so fresh-process down and this test agree.
func derivedNatsName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-nats-" + hex.EncodeToString(sum[:6])
}

// waitNatsHealthy polls until the NATS health endpoint answers 200 or the
// budget elapses — the event gate for the running bundled server.
func waitNatsHealthy(t *testing.T, url string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		if collectorHealthOK(client, url) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("NATS health %q not 200 within %s", url, budget)
		}
		time.Sleep(answerPollInterval) //nolint:forbidigo // bounded poll tick, event-gated by deadline
	}
}

// assertNatsJetStreamPosture verifies the rendered NATS JetStream settings.
func assertNatsJetStreamPosture(t *testing.T, varzURL string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(varzURL)
	if err != nil {
		t.Fatalf("GET NATS varz %q: %v", varzURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET NATS varz %q: status = %s, want 200", varzURL, resp.Status)
	}
	var v struct {
		JetStream struct {
			Config struct {
				StoreDir     string `json:"store_dir"`
				SyncInterval int64  `json:"sync_interval"`
			} `json:"config"`
		} `json:"jetstream"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode NATS varz: %v", err)
	}
	if !strings.Contains(v.JetStream.Config.StoreDir, stack.NatsStoreDir) {
		t.Fatalf("NATS JetStream store_dir = %q, want substring %q", v.JetStream.Config.StoreDir, stack.NatsStoreDir)
	}
	want := int64(100 * time.Millisecond)
	if v.JetStream.Config.SyncInterval != want {
		t.Fatalf("NATS JetStream sync_interval = %s (%d), want %s (%d)", time.Duration(v.JetStream.Config.SyncInterval), v.JetStream.Config.SyncInterval, time.Duration(want), want)
	}
}

// assertRecordHasNatsEntry requires the v2 pgid record's NATS container entry
// to use the exact derived name.
func assertRecordHasNatsEntry(t *testing.T, recordPath, name string) {
	t.Helper()
	for _, f := range recordLines(t, recordPath) {
		if len(f) >= 3 && f[0] == "ctr" && f[1] == "nats" {
			if f[2] != name {
				t.Fatalf("NATS entry name = %q, want %q", f[2], name)
			}
			return
		}
	}
	t.Fatalf("no `ctr nats %s` entry in %q", name, recordPath)
}
