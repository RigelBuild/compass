//go:build podman

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
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
	closed  bool
	stoodUp bool
	// pgDSN is the suite postgres TestMain started; the shared stack attaches
	// to it with ExternalDatabase instead of spawning its own.
	pgDSN string
	// env is the process environment before stand-up. The detached owner's
	// t.Setenv cleanups never run, so shutdownShared restores it by hand.
	env []string
	// tempDirs are the stand-up's TempDirs; shutdownShared removes them.
	tempDirs []string
}

var shared sharedState

// registerSharedFixtureOption collects routes before the one shared stack is
// built. TestMain registers every leg's options first, so filtering or reordering
// tests cannot drop a route. Marker routes are additive, but the positional
// script has one shared counter and must not be drawn concurrently by legs.
func registerSharedFixtureOption(opts ...fixtureOption) {
	if shared.stoodUp {
		// TestMain registers before any test runs; reaching here means a leg
		// body called it, whose option would be silently lost.
		panic("registerSharedFixtureOption called after the shared fixture stood up: " +
			"register it from TestMain, not from a test body — canned routes freeze at construction")
	}
	shared.opts = append(shared.opts, opts...)
}

// sharedFixture returns the ONE stack every leg in this package shares: a
// single stack.Up and a single root-supervisor seed per run rather than one per
// leg. The caller's podmanUsable() guard still gates reaching it; it adds no
// skip of its own. It must not be called after the final zz_legsix_test.go test.
func sharedFixture(t *testing.T) *Fixture {
	t.Helper()
	if shared.closed {
		t.Fatalf("shared e2e fixture is unavailable after TestLegSixTeardownIdempotence; zz_legsix_test.go must run last")
	}

	shared.once.Do(func() {
		shared.stoodUp = true
		shared.env = os.Environ()
		shared.f, shared.err = standUpShared(t.Context(), shared.opts...)
	})
	if shared.err != nil {
		t.Fatalf("shared e2e fixture stand-up failed (every leg in this package shares it): %v", shared.err)
	}
	return shared.f
}

// standUpShared uses a detached TB so one leg's cleanup cannot stop the stack
// while other legs use it. Detached cleanups cannot run, so shutdownShared owns
// teardown; this function also reaps a partial stand-up after Up.
// Its distinct site suffix avoids colliding with the default shortRoot path.
func standUpShared(ctx context.Context, opts ...fixtureOption) (*Fixture, error) {
	// run-root: the shared stack outlives every leg, so no leg's cancel can own it.
	ctx = context.WithoutCancel(ctx)

	site, err := newSharedSite(ctx)
	if err != nil {
		return nil, err
	}

	// Capture the stack immediately after Up so a later construction failure can
	// reap child processes despite the detached TB's unreachable cleanup.
	var live *stack.Stack
	observe := WithStackObserver(func(st *stack.Stack) { live = st })

	var f *Fixture
	opts = append([]fixtureOption{WithSite(site), observe}, opts...)
	if shared.pgDSN != "" {
		opts = append(opts, withExternalDatabase(shared.pgDSN))
	}
	tempDirs, err := runDetached(func(tb testing.TB) {
		tb.Helper()
		f = NewFixture(ctx, tb, opts...)
	})
	if err != nil {
		if live != nil {
			if derr := live.Down(ctx); derr != nil {
				fmt.Fprintf(os.Stderr, "e2e shared fixture: reaping a partial stand-up: %v\n", derr)
			}
		}
		// The site's dirs are ours alone and no cleanup is registered for them.
		_ = os.RemoveAll(site.root)
		_ = os.RemoveAll(site.stateDir)
		removeAll(tempDirs)
		return nil, err
	}
	shared.tempDirs = tempDirs
	return f, nil
}

func removeAll(dirs []string) {
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// runDetached runs fn against a detached testing.TB on its own goroutine, since
// a Fatalf calls runtime.Goexit. A detached T's log is unreadable, so failure
// text is captured and returned as the error every leg reports. It also returns
// the temp dirs fn made, which no cleanup will remove.
func runDetached(fn func(testing.TB)) (tempDirs []string, err error) {
	rec := &standUpRecorder{TB: &testing.T{}}
	completed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic during shared stand-up: %v", r)
			}
		}()
		fn(rec)
		completed = true
	}()
	<-done

	tempDirs = rec.dirs()
	if err != nil {
		return tempDirs, err
	}
	if msgs := rec.failures(); !completed || len(msgs) > 0 {
		if len(msgs) == 0 {
			msgs = []string{"goroutine exited with no recorded failure"}
		}
		return tempDirs, fmt.Errorf("NewFixture aborted during shared stand-up: %s", strings.Join(msgs, "; "))
	}
	return tempDirs, nil
}

// standUpRecorder keeps the detached T's failure messages; every other TB
// method falls through to the embedded T.
type standUpRecorder struct {
	testing.TB
	mu       sync.Mutex
	msgs     []string
	tempDirs []string
}

func (r *standUpRecorder) Error(args ...any)                 { r.record(fmt.Sprint(args...)) }
func (r *standUpRecorder) Errorf(format string, args ...any) { r.record(fmt.Sprintf(format, args...)) }
func (r *standUpRecorder) Fatal(args ...any)                 { r.record(fmt.Sprint(args...)); runtime.Goexit() }
func (r *standUpRecorder) FailNow()                          { r.record("FailNow called"); runtime.Goexit() }
func (r *standUpRecorder) Fail()                             { r.record("Fail called") }
func (r *standUpRecorder) Failed() bool                      { return len(r.failures()) > 0 }

// TempDir replaces the promoted one, whose failure path calls Fatal on the
// embedded T and so would bypass the recorder.
func (r *standUpRecorder) TempDir() string {
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "compass-e2e-shared-")
	if err != nil {
		r.Fatalf("TempDir: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tempDirs = append(r.tempDirs, dir)
	return dir
}

func (r *standUpRecorder) Fatalf(format string, args ...any) {
	r.record(fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func (r *standUpRecorder) record(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
}

func (r *standUpRecorder) failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.msgs)
}

func (r *standUpRecorder) dirs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.tempDirs)
}

// newSharedSite mints the shared stack's own root, state dir, and pg port.
// It mirrors newPersistentSite but is t-free and registers no cleanup, because
// shutdownShared owns this site's lifetime.
func newSharedSite(ctx context.Context) (fixtureSite, error) {
	root := filepath.Join("/tmp", "ce"+strconv.Itoa(os.Getpid())+"sh")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fixtureSite{}, fmt.Errorf("mkdir shared site root: %w", err)
	}
	stateDir, err := os.MkdirTemp("", "compass-e2e-shared-state-")
	if err != nil {
		return fixtureSite{}, fmt.Errorf("make shared state dir: %w", err)
	}
	ports, err := freeSharedPorts(ctx, 1)
	if err != nil {
		return fixtureSite{}, err
	}
	return fixtureSite{root: root, stateDir: stateDir, pgPort: ports[0]}, nil
}

// freeSharedPorts is freePorts without the *testing.T. Every listener is held
// open until all are chosen so the kernel cannot hand out the same port twice.
func freeSharedPorts(ctx context.Context, n int) ([]int, error) {
	lns := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for range n {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("reserve port: %w", err)
		}
		lns = append(lns, ln)
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			return nil, fmt.Errorf("reserved listener address %T, want *net.TCPAddr", ln.Addr())
		}
		ports = append(ports, addr.Port)
	}
	for _, ln := range lns {
		if err := ln.Close(); err != nil {
			return nil, fmt.Errorf("release reserved port: %w", err)
		}
	}
	return ports, nil
}

// closeSharedForLegSix disables the shared stack before the final restart test.
func closeSharedForLegSix(ctx context.Context) {
	shared.closed = true
	shutdownShared(ctx)
}

// shutdownShared tears the shared stack down. TestMain calls it after m.Run()
// returns; TestLegSixTeardownIdempotence calls it first, as the last test. It is
// a no-op when no leg stood the fixture up. Teardown failures are reported to
// stderr: there is no test left to fail.
func shutdownShared(ctx context.Context) {
	if !shared.stoodUp || shared.f == nil {
		return
	}
	f := shared.f
	shared.f = nil

	// The detached T's own cleanups are unreachable, so Down, the canned stub,
	// and the two directories NewFixture created are released by hand here.
	err := f.stack.Down(context.WithoutCancel(ctx))
	if f.stub != nil {
		err = errors.Join(err, f.stub.Close())
	}
	if f.forgeStub != nil {
		f.forgeStub.srv.Close()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e shared fixture teardown: %v\n", err)
	}
	restoreEnv(shared.env)

	// The site's root and state dir, neither of which registers a cleanup.
	// Down has drained the children, so these are this run's alone.
	_ = os.RemoveAll(filepath.Dir(f.runtimeDir))
	_ = os.RemoveAll(filepath.Dir(f.caPath))
	removeAll(shared.tempDirs)
	shared.tempDirs = nil
}

// restoreEnv resets the process environment to a snapshot from os.Environ.
func restoreEnv(env []string) {
	os.Clearenv()
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "e2e shared fixture env restore %s: %v\n", k, err)
		}
	}
}
