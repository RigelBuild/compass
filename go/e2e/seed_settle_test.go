//go:build podman

package e2e

import (
	"context"
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestSeedSettledBeforeFixtureReturns is the RIG-2403 regression: NewFixture must
// not return until the first-launch root-supervisor seed has finished
// provisioning its container. The seed (server/serve_seed.go) fires on the
// Runner Sessions-stream attach — the same event enrollment returns on — and
// drives its OWN Provision+Start of the supervisor on the hook goroutine, so
// before the fix a leg's first f.Provision raced the seed's in-flight Provision:
// two cold rootless-podman bring-ups contended on the engine storage lock and
// the pair overran the leg's 30s rpcTimeout under CI load, dying with
// deadline_exceeded (legfive_test.go:61 / legsix_test.go:94).
//
// The fix gates NewFixture on the seed's DURABLE PLACEMENT row (written by the
// ProvisionAgentWorkspace handler right after the Runner relay returns). So the
// observable post-condition, asserted here, is: the instant NewFixture returns,
// the supervisor account exists AND its placement resolves — the seed's
// container work is already DONE, with NO wait of our own. Each assertion is a
// single presence-NOW read (no poll loop, no "eventually"); a bare NewFixture
// with no gate would frequently fail this because the placement lands ~1.5s
// AFTER enrollment (the window the flake lived in). The reads carry the standard
// per-RPC-magnitude deadline so a wedged store fails loud rather than hanging.
//
// podmanUsable-guarded so a container-less sandbox SKIPS rather than fails. The
// only wait is NewFixture's own event-gated readiness; this test adds none.
func TestSeedSettledBeforeFixtureReturns(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + the store read

	f := NewFixture(ctx, t)

	st, err := store.Open(ctx, f.DSN())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// The supervisor account exists the moment NewFixture returns — the seed's
	// create half ran. A bare ErrNotFound here means the gate let NewFixture
	// return before the seed even created the supervisor.
	hctx, hcancel := context.WithTimeout(ctx, rpcTimeout)
	defer hcancel()
	sup, err := st.AgentByHandle(hctx, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("root supervisor not resolvable after NewFixture returned: %v (the seed-settle gate did not hold)", err)
	}

	// The placement resolves on a single read taken right after NewFixture
	// returned — the seed's Provision recorded it BEFORE the gate released. This
	// is the exact post-condition the gate guarantees and the race violated: a
	// bare NewFixture with no gate frequently leaves this unplaced, because the
	// placement lands ~1.5s after enrollment (the window the flake lived in). The
	// read carries the standard per-RPC-magnitude deadline so a wedged store fails
	// loud rather than hanging, but the assertion is presence-NOW, not eventual:
	// there is no poll loop.
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	runnerID, containerName, err := st.PlacementForAgent(rctx, sup.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			t.Fatalf("root supervisor is unplaced immediately after NewFixture returned: the seed's Provision had not recorded its placement, so a leg's first Provision would race it (RIG-2403)")
		}
		t.Fatalf("PlacementForAgent(supervisor): %v", err)
	}
	if runnerID == "" || containerName == "" {
		t.Fatalf("supervisor placement is incomplete after NewFixture: runner=%q container=%q, want both set", runnerID, containerName)
	}
}
