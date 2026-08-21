//go:build unix

// Package runnerloop provides the Runner dispatch-loop test scaffolding that
// must import internal/runner (for ServerLink/SessionHost). It is used only by
// tests that already depend on runner from outside its own package
// (runnerhub_test, server) — never by package runner's own test — so no consumer
// creates a runner → runnerloop → runner import cycle in a test binary.
package runnerloop

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/runner"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// RunSessionsLoop starts the Runner's dispatch loop and registers the teardown
// that must bracket it. The ordering is load-bearing in both directions, which
// is why it lives beside the goroutine rather than beside context.WithCancel.
//
// It must run BEFORE httptest's srv.Close (registered inside mountRunnerServer,
// therefore earlier, therefore later under LIFO): Close waits on its handlers,
// and the live Sessions handler returns only once ctx is cancelled, so
// cancelling after Close deadlocks the entire cleanup stack — every later
// cleanup, the runtime dir removal included, never runs.
//
// It must run AFTER the runtime dir removal registered at the top of the test,
// so LIFO reclaims that tree only once the loop has left dispatch. Cancel alone
// would not do it: cancel signals and returns, so the WAIT is what makes the
// ordering mean anything.
//
// The wait is what orders the two; it is not a gate on the removal. If the
// drain times out, the later cleanups still run — a cleanup cannot cancel the
// ones registered before it. Neither `return` (it exits only its own closure)
// nor t.Fatalf (it marks the test and runs the rest of the stack anyway) skips
// them, so the timeout arm reports the collision rather than averting it. That
// is the right trade at this point: the test has already failed, and the
// alternative — a flag threaded into shortRuntimeDir to skip RemoveAll — leaks
// the tree and couples two independent helpers to buy nothing a red test needs.
func RunSessionsLoop(t *testing.T, ctx context.Context, cancel context.CancelFunc, link *runner.ServerLink, host runner.SessionHost, timeout time.Duration) <-chan error {
	t.Helper()
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- link.RunSessions(ctx, host, discardLog())
		// Closed as well as sent: the clean-teardown drain at the end of the test
		// takes the value, and the cleanup below must still observe the exit.
		close(loopDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-loopDone:
			// On the clean path assertCleanShutdown already took the value and
			// this reads the zero from a closed channel. On a failure path it
			// takes the real one, and a stream error that killed the loop must
			// surface here — otherwise the test reports only whatever Fatalf'd
			// first, with the actual cause discarded.
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("RunSessions ended with %v", err)
			}
		case <-time.After(timeout):
			t.Errorf("RunSessions still running %s after cancel; the runtime dir removal below runs anyway and will race an in-flight Provision", timeout)
		}
	})
	return loopDone
}
