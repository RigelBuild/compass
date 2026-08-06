//go:build podman

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/stack"
)

// rpcTimeout bounds a single authed RPC over the loopback door: generous for a
// local call, short enough that a wedged connection fails visibly rather than
// hanging the test. Deterministic per-call deadline, never a retry loop.
const rpcTimeout = 30 * time.Second

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

	ctx := context.Background() // test root: NewFixture threads this into Up + teardown Down

	f := NewFixture(ctx, t)

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
	assertRunnerHasConfiguredFlags(t, f.runtimeDir)

	// 6. Down drains cleanly and leaves no child processes. A successful Down is
	// the explicit teardown (the t.Cleanup guard only covers a failed test); after
	// it, no compass-server/-runner/-postgres child spawned from binDir survives.
	if err := f.Stack().Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// Health after Down must no longer be Ready — the server child is gone, so the
	// GetServerInfo probe fails, which Health reports as Failed.
	post, err := f.Stack().Health(ctx)
	if err != nil {
		t.Fatalf("Health after Down: %v", err)
	}
	if post.State == stack.StatusReady {
		t.Fatal("stack still Ready after Down; the compass-server child was not stopped")
	}
}

// assertRunnerHasConfiguredFlags reads the live compass-runner's argv from the
// process table and requires the configured --agent-model and comma-joined
// --egress-allow to be present — end-to-end proof the A4 Config fields reached
// the spawned runner, complementing the deterministic runnerSpec unit test.
//
// The match is scoped to runtimeDir — this fixture's unique
// shortRoot(t,"h1")/rt path, forwarded to the runner as --runtime-dir. On a
// shared box running a compass fleet, an unscoped scrape could match a foreign
// compass-runner and either false-green (masking a real forward-path break in
// THIS stack) or flaky-red on a concurrent runner with different flags; with
// no-retries a flake has no net. Requiring the runtime-dir on the matched line
// makes a foreign runner unable to satisfy it.
//
// It reads argv via `ps` rather than racing the runner's async enrollment; the
// runner process exists as soon as spawnChain's final step returned, which is
// before Up returned Ready, so the process is present by the time this runs.
func assertRunnerHasConfiguredFlags(t *testing.T, runtimeDir string) {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "args").CombinedOutput()
	if err != nil {
		t.Fatalf("ps -eo args: %v\n%s", err, out)
	}
	var runnerLine string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "compass-runner") &&
			strings.Contains(line, "--runtime-dir "+runtimeDir) {
			runnerLine = line
			break
		}
	}
	if runnerLine == "" {
		t.Fatalf("no live compass-runner process for this fixture (--runtime-dir %s) in the process table", runtimeDir)
	}
	if !strings.Contains(runnerLine, "--agent-model anthropic/claude-opus") {
		t.Fatalf("runner argv missing configured --agent-model: %q", runnerLine)
	}
	if !strings.Contains(runnerLine, "--egress-allow api.anthropic.com,10.0.0.1") {
		t.Fatalf("runner argv missing comma-joined --egress-allow: %q", runnerLine)
	}
}
