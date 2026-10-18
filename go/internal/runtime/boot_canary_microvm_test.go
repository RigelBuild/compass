//go:build microvm && unix

package runtime

// This KVM-gated e2e runs BootCanary on live hardware and verifies boot and teardown.
// It uses the existing microVM CI leg and fails when KVM is required but unavailable.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

// TestBootCanary boots a real canary VM through BootCanary and asserts the report
// is populated (BootLatency in (0, canaryDeadline], GuestRSSBytes > 0) and that
// BootCanary tore down its own VM and runtime dir — nothing left in the session
// table and no leftover <RunRoot>/microvm/* dir. Unlike TestMicroVMQBudget (which
// Removes in cleanup), the canary owns its teardown, so this asserts it happened.
func TestBootCanary(t *testing.T) {
	env := microvmtest.Require(t)
	cfg := e2eConfig(t, env)
	m := NewMicroVMRuntime(cfg)

	report, err := m.BootCanary(t.Context())
	if err != nil {
		t.Fatalf("BootCanary = %v, want nil", err)
	}
	if report.BootLatency <= 0 {
		t.Errorf("BootLatency = %v, want > 0", report.BootLatency)
	}
	if report.BootLatency > canaryDeadline {
		t.Errorf("BootLatency = %v, want <= the %v canary deadline", report.BootLatency, canaryDeadline)
	}
	if report.GuestRSSBytes <= 0 {
		t.Errorf("GuestRSSBytes = %d, want > 0", report.GuestRSSBytes)
	}
	t.Logf("BootCanary: boot latency = %s, guest PSS = %d bytes", report.BootLatency, report.GuestRSSBytes)

	// BootCanary owns its teardown: no session leaked in the table.
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("session table has %d entries after BootCanary, want 0 (canary must tear down its own session)", n)
	}

	// And no per-session runtime dir left under <RunRoot>/microvm.
	microvmDir := filepath.Join(cfg.RunRoot, "microvm")
	entries, statErr := os.ReadDir(microvmDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return // never created, or fully removed — both fine
		}
		t.Fatalf("reading %s: %v", microvmDir, statErr)
	}
	if len(entries) != 0 {
		t.Errorf("%s has %d leftover session dirs after BootCanary, want 0", microvmDir, len(entries))
	}
}
