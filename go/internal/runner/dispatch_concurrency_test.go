//go:build unix

package runner

// Concurrent-dispatch tests cover in-flight request-id dedup and event-gated
// synchronization without sleeps. Real RunSessions tests cover lifecycle
// concurrency and provision limits.

import (
	"context"
	"testing"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
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
