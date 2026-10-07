//go:build podman

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAgentImageVCSTools checks the agent image ships jj and jj-vine, the
// version-control tools every Rigel lane uses. Each binary must run, not only exist.
func TestAgentImageVCSTools(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	for _, tool := range []struct{ bin, want string }{
		{"jj", "jj "},
		{"jj-vine", "jj-vine "},
	} {
		t.Run(tool.bin, func(t *testing.T) {
			out, err := exec.Command("podman", "run", "--rm", agentImage, tool.bin, "--version").CombinedOutput()
			if err != nil {
				t.Fatalf("%s --version in %s: %v\n%s", tool.bin, agentImage, err, out)
			}
			if !strings.HasPrefix(strings.TrimSpace(string(out)), tool.want) {
				t.Fatalf("%s --version printed %q, want prefix %q", tool.bin, out, tool.want)
			}
		})
	}
}
