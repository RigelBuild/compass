package adapters

import (
	"testing"
	"time"
)

// waitBudget bounds an event-gated wait by the test binary's own deadline, so a
// loaded host slows the test instead of failing it at a fixed wall-clock cap.
func waitBudget(t *testing.T) time.Duration {
	t.Helper()
	d, ok := t.Deadline()
	if !ok { // -timeout=0 disables the deadline
		return 2 * time.Minute
	}
	// Margin lets the test fail with its own diagnostic before the package timeout panics.
	remaining := time.Until(d)
	return max(remaining-min(5*time.Second, remaining/4), time.Millisecond)
}
