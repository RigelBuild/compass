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

// TestAgentImageGHTokenAuth verifies gh and Git read the materialized token and
// jj-vine has its token command configured, without contacting GitHub.
func TestAgentImageGHTokenAuth(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	cmd := `set -eu
mkdir -p "$HOME/.config/gh"
printf 'github.com:\n    oauth_token: "ghs_fake"\n' > "$HOME/.config/gh/hosts.yml"
printf 'version: "1"\n' > "$HOME/.config/gh/config.yml"
test "$(gh auth token)" = "ghs_fake"
credential=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill)
case "$credential" in *"password=ghs_fake"*) ;; *) exit 1 ;; esac
jjconfig=$(jj config get jj-vine.github.tokenCommand)
case "$jjconfig" in *gh*) ;; *) exit 1 ;; esac
`
	out, err := exec.Command("podman", "run", "--rm", agentImage, "sh", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("GitHub token auth smoke in %s: %v\n%s", agentImage, err, out)
	}
}
