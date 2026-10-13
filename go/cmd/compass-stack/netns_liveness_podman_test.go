//go:build podman

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNetnsConnLivenessRejectsNonPositivePid: kill(0) and kill(-1) probe a
// process group or every process, so they must never be reported as liveness.
func TestNetnsConnLivenessRejectsNonPositivePid(t *testing.T) {
	for _, pid := range []string{"0", "-1", "-42"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "rootless-netns-conn.pid"), []byte(pid+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := netnsConnLiveness(dir)
		if strings.Contains(got, "kill(0)") || !strings.Contains(got, "invalid") {
			t.Errorf("pid %s: got %q, want an invalid-pid report with no kill probe", pid, got)
		}
	}
}
