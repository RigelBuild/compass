//go:build podman

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// sharedState holds the one package-scoped fixture and the outcome of its
// single stand-up attempt. err is retained so a failed stand-up is re-reported
// to every later caller instead of handing out a nil *Fixture.
type sharedState struct {
	once    sync.Once
	f       *Fixture
	err     error
	opts    []fixtureOption
	stoodUp bool
}

var shared sharedState

// registerSharedFixtureOption declares an option the shared stack must be built
// with. Call it from a leg's init(), NOT from the leg body:
//
//	func init() { registerSharedFixtureOption(WithCannedMarkerScript("leg-b", ...)) }
//
// Every init() in the package runs before the first test, so the full option
// set is known before stand-up no matter which leg reaches sharedFixture first,
// and no matter how `go test -run` filters or reorders the legs.
//
// This indirection exists because canned model routes FREEZE at construction —
// startCannedModelServer sizes its per-marker counters from the marker slice it
// is handed (cannedmodel.go:281) and the handler then reads that slice without
// a lock. So an option arriving after stand-up cannot be honoured, and the only
// two alternatives are worse: taking options at the first sharedFixture call
// makes the routes depend on test ORDER (a `-run` filter silently changes which
// leg is first, and the losing leg's marker is dropped), while mutating the
// running stub would be an unsynchronised write beside a live reader.
//
// Marker routes are keyed by a body substring and are additive, so legs do not
// contend. The POSITIONAL script is the shared resource: two legs drawing
// unmarked turns concurrently race one counter, which is why every leg past the
// first routes its turns by marker.
func registerSharedFixtureOption(opts ...fixtureOption) {
	if shared.stoodUp {
		// init() ordering guarantees this cannot happen from an init(); reaching
		// here means a leg body called it, whose option would be silently lost.
		panic("registerSharedFixtureOption called after the shared fixture stood up: " +
			"call it from an init(), not from a test body — canned routes freeze at construction")
	}
	shared.opts = append(shared.opts, opts...)
}

// sharedFixture returns the ONE stack every leg in this package shares: a
// single stack.Up and a single root-supervisor seed per run rather than one per
// leg. The caller's podmanUsable() guard still gates reaching it; it adds no
// skip of its own.
//
// It takes no options by design — see registerSharedFixtureOption. A leg
// needing routes nobody else may see calls NewFixture for its own stack.
func sharedFixture(t *testing.T) *Fixture {
	t.Helper()

	shared.once.Do(func() {
		shared.stoodUp = true
		shared.f, shared.err = standUpShared(shared.opts...)
	})
	if shared.err != nil {
		t.Fatalf("shared e2e fixture stand-up failed (every leg in this package shares it): %v", shared.err)
	}
	return shared.f
}

// standUpShared builds the shared fixture against a DETACHED *testing.T instead
// of a leg's own t. NewFixture is *testing.T-bound throughout — TempDir, four
// Cleanup registrations, and 13 Fatalf sites — so hanging it off whichever leg
// asked first would let that leg's cleanup Down the stack while the other legs
// are still using it, and would blame that leg for a shared setup failure.
//
// Nothing can drain a detached T's cleanups (they are unexported, and it never
// completes), so teardown is explicit and split: shutdownShared handles a
// fully-constructed fixture, and this function reaps a stand-up that aborted
// after Up. A Fatalf on that T calls runtime.Goexit, so the stand-up runs on
// its own goroutine and the goroutine ending without a fixture reads as failure.
//
// The site is supplied rather than defaulted because shortRoot keys its root on
// the PID alone: the default "h1" root is the very path every per-leg
// NewFixture claims, so a per-leg cleanup would RemoveAll this stack's live
// sockets mid-run. A distinct suffix keeps them disjoint, as the H6 site does.
func standUpShared(opts ...fixtureOption) (f *Fixture, err error) {
	owner := &testing.T{}
	ctx := context.Background() // run-root: the shared stack outlives every leg, so no leg's ctx can own it

	site, err := newSharedSite()
	if err != nil {
		return nil, err
	}

	// NewFixture returns its *Fixture only at the very end, but Up spawns the
	// children well before that and registers its Down on the detached t we
	// cannot drain. So capture the stack at Up and reap it here if a later gate
	// aborts — otherwise a failed enrollment or seed-settle orphans a postgres,
	// a server, and a rootless container for the rest of the run.
	var live *stack.Stack
	observe := WithStackObserver(func(st *stack.Stack) { live = st })

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic during shared stand-up: %v", r)
			}
		}()
		f = NewFixture(ctx, owner, append([]fixtureOption{WithSite(site), observe}, opts...)...)
	}()
	<-done

	if err == nil && f == nil {
		err = errors.New("NewFixture aborted during shared stand-up (a t.Fatalf inside it); " +
			"re-run a single leg to see the underlying stack error")
	}
	if err != nil {
		if live != nil {
			if derr := live.Down(ctx); derr != nil {
				fmt.Fprintf(os.Stderr, "e2e shared fixture: reaping a partial stand-up: %v\n", derr)
			}
		}
		// The site's dirs are ours alone and no cleanup is registered for them.
		_ = os.RemoveAll(site.root)
		_ = os.RemoveAll(site.stateDir)
		return nil, err
	}
	return f, nil
}

// newSharedSite mints the shared stack's own root, state dir, and port pair.
// It mirrors newPersistentSite but is t-free and registers no cleanup, because
// shutdownShared owns this site's lifetime.
func newSharedSite() (fixtureSite, error) {
	root := filepath.Join("/tmp", "ce"+strconv.Itoa(os.Getpid())+"sh")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fixtureSite{}, fmt.Errorf("mkdir shared site root: %w", err)
	}
	stateDir, err := os.MkdirTemp("", "compass-e2e-shared-state-")
	if err != nil {
		return fixtureSite{}, fmt.Errorf("make shared state dir: %w", err)
	}
	ports, err := freeSharedPorts(2)
	if err != nil {
		return fixtureSite{}, err
	}
	return fixtureSite{root: root, stateDir: stateDir, listenPort: ports[0], pgPort: ports[1]}, nil
}

// freeSharedPorts is freePorts without the *testing.T. Every listener is held
// open until all are chosen so the kernel cannot hand out the same port twice.
func freeSharedPorts(n int) ([]int, error) {
	lns := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve port: %w", err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		if err := ln.Close(); err != nil {
			return nil, fmt.Errorf("release reserved port: %w", err)
		}
	}
	return ports, nil
}

// shutdownShared tears the shared stack down. It MUST be called from TestMain
// after m.Run() returns — the only lifetime that outlives every leg. It is a
// no-op when no leg ever stood the fixture up. Teardown failures are reported
// to stderr: the run's tests are already over, so there is nothing left to fail.
func shutdownShared() {
	if !shared.stoodUp || shared.f == nil {
		return
	}
	f := shared.f
	shared.f = nil

	// The detached T's own cleanups are unreachable, so Down, the canned stub,
	// and the two directories NewFixture created are released by hand here.
	ctx := context.Background() // run-root: m.Run has returned, so no test ctx survives to inherit
	err := f.stack.Down(ctx)
	if f.stub != nil {
		err = errors.Join(err, f.stub.Close())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e shared fixture teardown: %v\n", err)
	}

	// The site's root and state dir, neither of which registers a cleanup.
	// Down has drained the children, so these are this run's alone.
	_ = os.RemoveAll(filepath.Dir(f.runtimeDir))
	_ = os.RemoveAll(filepath.Dir(f.caPath))
}
