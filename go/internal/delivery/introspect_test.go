//go:build unix

package delivery

// Test-only introspection into the consumer's registry + settle queue and the
// dispatcher's recorded set, so the acceptance cases event-gate on observed
// internal state (a message held, the settle queue drained) rather than sleeping
// (rule://no-retries). These reach unexported fields, so they live in-package.

import (
	"slices"
	"testing"
	"time"
)

// waitHeld blocks until authorSession holds at least n pending-deliver entries,
// or fails at the deadline. Event-gates on the registry, polled with a yielding
// ticker — no wall-clock synchronization, just a bounded observe-loop over an
// in-memory field the bus goroutine mutates.
func (c *Consumer) waitHeld(t *testing.T, authorSession string, n int) { //nolint:unparam // authorSession is the helper's read-clarity signature: each call site names the session whose held-queue it gates on (currently always the agent-author session), not dead code.
	t.Helper()
	deadline := time.After(testTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.Lock()
		got := len(c.held[authorSession])
		c.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("waited for %d held under %q, got %d", n, authorSession, got)
		}
	}
}

// isHeld reports whether messageID is currently held under authorSession.
func (c *Consumer) isHeld(authorSession, messageID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.held[authorSession], messageID)
}

// waitSettleDrained blocks until the settle queue is empty, or fails at the
// deadline. Paired with an OnSessionSettled for a throwaway session, it is a
// deterministic barrier that a prior settle edge was fully processed.
func (c *Consumer) waitSettleDrained(t *testing.T) {
	t.Helper()
	deadline := time.After(testTimeout)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.Lock()
		empty := len(c.settleQueue) == 0
		c.mu.Unlock()
		if empty {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("settle queue never drained")
		}
	}
}

// waitForMessage reports whether messageID was dispatched before the deadline.
func (d *fakeDispatcher) waitForMessage(t *testing.T, messageID string) bool {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		for _, rec := range d.snapshot() {
			if rec.messageID == messageID {
				return true
			}
		}
		select {
		case <-d.recorded:
		case <-deadline:
			return false
		}
	}
}
