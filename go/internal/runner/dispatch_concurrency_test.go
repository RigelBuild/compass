//go:build unix

package runner

// Concurrent-dispatch tests for the dispatcher's in-flight-sentinel idempotency
// (docs/designs/platform/compass-runner-concurrent-dispatch/design.md, Approach
// (b) / Plan T2). Under per-command goroutine dispatch the request-id dedup
// window is open concurrently: a retry of an IN-FLIGHT id must JOIN the running
// execution rather than double-execute it. Every test is event-gated on the
// fake host's park channels — no sleeps, no polling.
//
// T1 harness split (per the record's T1 note — implementer's choice, documented
// here): the same-id single-execution case (test 2, below) drives the
// dispatcher's handle directly against the extended fakeSessionHost
// (dispatch_test.go), the dedup layer that owns the invariant — no wire needed.
// The broken-Send loop-classification case (test 6) drives an unexported
// runSessions seam with a scripted fake stream, in run_classification_test.go
// (package runner) — it is a loop-error unit test, not integration. The
// slow-Provision/Stop isolation and leak-free-join cases (tests 1, 4) and the
// Provision-cap case (T-cap) drive the real RunSessions over the in-memory wire
// in package runnerhub. The per-container serialization and no-self-deadlock
// cases (tests 3, 5) drive the real agentHost in host_concurrency_test.go, since
// the transition lock lives there.

import (
	"context"
	"testing"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// Same-id concurrent retry executes the host EXACTLY ONCE: a second push of an
// id whose execution is still in flight joins that execution and returns the one
// recorded result, rather than starting a second (Approach (b), the in-flight
// sentinel mirroring runnerhub/router.go's pendingCall join). Deterministic via
// the fake's provision park gate: the first handle is held mid-Provision while
// the second is dispatched, so the second can only observe an in-flight entry —
// never a completed or absent one.
//
// RED before T2 (the pre-T2 handled map records only COMPLETED results): the
// held-open first execution records nothing, so the concurrent second push
// misses the map and calls Provision a second time — provisionCalls == 2.
func TestHandleConcurrentSameIDExecutesHostOnce(t *testing.T) {
	host := &fakeSessionHost{
		containerName:    "cont-1",
		provisionEntered: make(chan struct{}, 2),
		provisionRelease: make(chan struct{}),
	}
	d := newDispatcher(host, discardLoggerRunner())
	ctx := context.Background()

	got := make(chan *compassv1internal.SessionsRequest, 2)
	for range 2 {
		go func() {
			got <- d.handle(ctx, provisionCommand("req-dup"))
		}()
	}

	// Gate on the first (and, under the sentinel, only) execution reaching
	// Provision and parking there, so the second push is guaranteed to observe an
	// in-flight id rather than an unstarted one.
	select {
	case <-host.provisionEntered:
	case <-timeAfter():
		t.Fatal("no Provision reached the host; the first same-id dispatch never executed")
	}

	// Release the held execution; both dispatches now complete.
	close(host.provisionRelease)
	for range 2 {
		select {
		case <-got:
		case <-timeAfter():
			t.Fatal("a same-id dispatch never completed after release")
		}
	}

	if host.provisionCalls != 1 {
		t.Fatalf("host.Provision called %d times for a concurrent repeated id, want 1 (the in-flight sentinel joins the second push)", host.provisionCalls)
	}
}
