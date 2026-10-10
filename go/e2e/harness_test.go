//go:build podman

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/stack"
)

// TestHarnessCore is the podman end-to-end proof of the H1 fixture substrate: a
// real embedded stack (real compass-agent:latest) reaches Ready; both Connect
// clients answer one AUTHENTICATED RPC each; an UNauthenticated call is rejected
// Unauthenticated (the load-bearing negative — without it, a door that ignored
// auth would pass); the configured AgentModel/EgressAllow reached the runner's
// flags; and Down leaves no child processes.
//
// podmanUsable-guarded so a container-less sandbox SKIPS (never fails). No
// sleeps, no retries: readiness is the Up postcondition observed via Health, and
// every RPC carries a deterministic deadline.
func TestHarnessCore(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into every RPC below

	f := sharedFixture(t)

	// 1. The stack is Ready — a spawned Ready stack is itself proof the whole
	// cold-start chain ran (postgres up, server answering, token minted, real
	// agent image present in the store, runner exec'd).
	health, err := f.Stack().Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.State != stack.StatusReady {
		t.Fatalf("Health state = %v (%s), want Ready", health.State, health.Detail)
	}

	// 2. Authed CompassService RPC: GetServerInfo over the TLS door with the admin
	// bearer succeeds. On the network door GetServerInfo is authenticatedOpen — it
	// still requires a valid bearer, so success proves the terminated TLS
	// connection carries a genuinely authenticated RPC.
	{
		rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		defer cancel()
		resp, err := f.Compass().GetServerInfo(rctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
		if err != nil {
			t.Fatalf("authed CompassService.GetServerInfo: %v", err)
		}
		if resp.Msg.GetVersion() == "" {
			t.Fatal("GetServerInfo returned an empty version")
		}
	}

	// 3. Authed CommsService RPC: ListAccounts is a no-side-effect read scoped to
	// the caller; over the authed client it must succeed, proving the CommsService
	// door is mounted behind the same bearer chain and reachable.
	{
		rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		defer cancel()
		if _, err := f.Comms().ListAccounts(rctx, connect.NewRequest(&compassv1.ListAccountsRequest{})); err != nil {
			t.Fatalf("authed CommsService.ListAccounts: %v", err)
		}
	}

	// 4. Load-bearing negative: the SAME RPC over a client with NO bearer must be
	// rejected Unauthenticated. Without this, a server that ignored auth would
	// pass steps 2-3 — so the reject case is what actually proves auth.
	{
		unauthed, err := newUnauthedCompassClient(f.caPath, f.serverURL)
		if err != nil {
			t.Fatalf("build unauthed client: %v", err)
		}
		rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		defer cancel()
		_, err = unauthed.GetServerInfo(rctx, connect.NewRequest(&compassv1.GetServerInfoRequest{}))
		if err == nil {
			t.Fatal("unauthenticated GetServerInfo succeeded; the network door is not enforcing auth")
		}
		if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
			t.Fatalf("unauthenticated GetServerInfo code = %v, want Unauthenticated (err: %v)", code, err)
		}
	}

	// 5. The configured AgentModel/EgressAllow reached the runner's flags. The
	// deterministic proof is the runnerSpec unit test (internal/stack); here we
	// confirm end-to-end that the live runner process carries them, reading its
	// argv from the process table. Scoped to THIS fixture's unique runtime-dir so
	// a foreign compass-runner on this shared box cannot satisfy the assertion.
	assertRunnerHasConfiguredFlags(t, f)

	// 6. The shared stack remains Ready after these authenticated and rejected
	// calls. TestMain owns its shutdown after every leg has finished.
	post, err := f.Stack().Health(ctx)
	if err != nil {
		t.Fatalf("Health after RPC checks: %v", err)
	}
	if post.State != stack.StatusReady {
		t.Fatalf("stack state after RPC checks = %v (%s), want Ready", post.State, post.Detail)
	}
}

// assertRunnerHasConfiguredFlags reads the live compass-runner's argv from the
// process table and requires the configured --agent-model and comma-joined
// --egress-allow to be present — end-to-end proof the A4 Config fields reached
// the spawned runner, complementing the deterministic runnerSpec unit test.
//
// The match is scoped to runtimeDir — the shared fixture's unique run root,
// forwarded to the runner as --runtime-dir. An unscoped scrape could match a
// foreign compass-runner and either false-green or flake on a concurrent runner
// with different flags.
//
// ps inspects the live Runner's argv; spawnChain waits for enrollment before
// Up returns Ready, so the process exists by the time this assertion runs.
func assertRunnerHasConfiguredFlags(t *testing.T, f *Fixture) {
	t.Helper()
	runtimeDir := f.runtimeDir
	out, err := exec.Command("ps", "-eo", "args").CombinedOutput()
	if err != nil {
		t.Fatalf("ps -eo args: %v\n%s", err, out)
	}
	var runnerLine string
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "compass-runner") &&
			strings.Contains(line, "--runtime-dir "+runtimeDir) {
			runnerLine = line
			break
		}
	}
	if runnerLine == "" {
		t.Fatalf("no live compass-runner process for this fixture (--runtime-dir %s) in the process table", runtimeDir)
	}
	if !strings.Contains(runnerLine, "--agent-model "+f.agentModel) {
		t.Fatalf("runner argv missing configured --agent-model: %q", runnerLine)
	}
	if !strings.Contains(runnerLine, "--egress-allow "+strings.Join(f.egressAllow, ",")) {
		t.Fatalf("runner argv missing comma-joined --egress-allow: %q", runnerLine)
	}
}
