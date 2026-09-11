//go:build podman

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// seamProbeMarker is this file's canned route, registered at init() the way
// every leg sharing the stack must. It exists to be found in the stub.
const seamProbeMarker = "seam-probe-marker"

func init() {
	// WithCannedScript is required, not decorative: only it sets fc.canned, the
	// flag NewFixture gates the stub on (fixture.go:472). Markers registered
	// without it are dropped and never serve a route.
	registerSharedFixtureOption(
		WithCannedScript(CannedText("seam positional turn")),
		WithCannedMarkerReply(seamProbeMarker, "seam probe ack"),
	)
}

// seamHandle is the account written once per run and read back by every leg —
// the cross-leg evidence that the legs share one stack.
const seamHandle = "shared-seam-owner"

var (
	seamOnce sync.Once
	seamErr  error
	seamURL  string
)

// seedSeamAccount creates the marker account exactly ONCE per test binary. That
// is what makes the read below load-bearing: if the legs ever stopped sharing a
// stack, the seed would land in the first leg's database and the later leg would
// query a different, empty one and find nothing.
func seedSeamAccount(t *testing.T, f *Fixture, ctx context.Context) {
	t.Helper()
	seamOnce.Do(func() {
		_, seamErr = f.CreateUser(ctx, seamHandle, "Shared Seam Owner")
		seamURL = f.ServerURL()
	})
	if seamErr != nil {
		t.Fatalf("seed seam account %q: %v", seamHandle, seamErr)
	}
}

// assertSeamVisible fails unless this leg's fixture resolves the seeded account
// and answers on the same door the seed used.
func assertSeamVisible(t *testing.T, f *Fixture, ctx context.Context) {
	t.Helper()
	if got := f.ServerURL(); got != seamURL {
		t.Fatalf("ServerURL = %q, want %q — this leg is on a different stack", got, seamURL)
	}
	acc, err := f.lookupAccount(ctx, seamHandle)
	if err != nil {
		t.Fatalf("seeded account %q is not visible to this leg: %v", seamHandle, err)
	}
	if acc.GetHandle() != seamHandle {
		t.Fatalf("resolved handle = %q, want %q", acc.GetHandle(), seamHandle)
	}
}

// TestSharedFixtureRootIsDisjointFromPerLegRoot pins the one property the two
// sharing legs above cannot see, because they never run beside a per-leg
// fixture: shortRoot keys its root on the PID alone, so a shared stack left on
// the default root sits on the exact path every NewFixture claims — and that
// leg's cleanup RemoveAll would delete this stack's live sockets mid-run.
func TestSharedFixtureRootIsDisjointFromPerLegRoot(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	f := sharedFixture(t)

	shared := filepath.Dir(f.runtimeDir)
	perLeg := filepath.Join("/tmp", "ce"+strconv.Itoa(os.Getpid())+"h1")
	if shared == perLeg {
		t.Fatalf("shared root %q is the path a per-leg NewFixture claims; a per-leg cleanup would delete this stack's sockets", shared)
	}
}

// TestSharedFixtureLegOne is the first of two legs sharing one stack.
func TestSharedFixtureLegOne(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background()
	f := sharedFixture(t)

	seedSeamAccount(t, f, ctx)
	assertSeamVisible(t, f, ctx)
}

// TestSharedFixtureLegTwo is the load-bearing half: whichever of the pair runs
// second reads the other's account out of the shared stack, which only a shared
// stack holds. seamOnce makes that order-independent — the pair is the unit of
// proof, so running one alone proves nothing.
func TestSharedFixtureLegTwo(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background()
	f := sharedFixture(t)

	seedSeamAccount(t, f, ctx)
	assertSeamVisible(t, f, ctx)
}

// TestSharedFixtureAppliesRegisteredOptions proves an option registered at
// init() by ANY leg reaches the shared backend — the property that makes the
// seam independent of which leg runs first. A dropped marker route would settle
// an agent turn on the wrong reply and still pass, so this is the vacuous-green
// guard for the registration path.
func TestSharedFixtureAppliesRegisteredOptions(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	f := sharedFixture(t)

	if f.stub == nil {
		t.Fatal("shared fixture has no canned stub; the registered canned option was dropped")
	}
	var got []string
	for _, m := range f.stub.markers {
		got = append(got, m.marker)
	}
	if !slices.Contains(got, seamProbeMarker) {
		t.Fatalf("registered marker %q absent from the shared stub (markers: %v); "+
			"an init()-registered option did not reach stand-up", seamProbeMarker, got)
	}
}

// TestSharedFixtureRejectsPostStandUpRegistration pins the misuse diagnostic:
// registering from a test body instead of an init() cannot be honoured, because
// canned routes freeze at construction. It must panic rather than drop silently.
func TestSharedFixtureRejectsPostStandUpRegistration(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	sharedFixture(t)

	defer func() {
		if recover() == nil {
			t.Fatal("registering an option after stand-up did not panic; it would be silently dropped")
		}
	}()
	registerSharedFixtureOption(WithCannedModel("never applied"))
}
