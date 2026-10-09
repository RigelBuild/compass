//go:build podman

package e2e

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime"
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

// TestAgentImageGHTokenAuth installs a fixture token with the production
// GHHostsScript, then checks gh, Git, and jj-vine's tokenCommand all return it.
func TestAgentImageGHTokenAuth(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	const tok = "ghs_fixture"
	install, err := runtime.GHHostsScript("/home/agent", []runtime.GHCredentials{{Host: "github.com", Token: tok}})
	if err != nil {
		t.Fatalf("GHHostsScript: %v", err)
	}
	// tokenCommand is a TOML string array; run that argv exactly as configured.
	check := `set -eu
test "$(gh auth token)" = "` + tok + `"
credential=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill)
case "$credential" in *"password=` + tok + `"*) ;; *) echo "git credential: $credential"; exit 1 ;; esac
argv=$(jj config get jj-vine.github.tokenCommand | tr -d '[],"')
test "$($argv)" = "` + tok + `"
`
	cmd := exec.Command("podman", "run", "--rm", "-i", "-e", "HOME=/home/agent", agentImage, "sh", "-s")
	cmd.Stdin = strings.NewReader(install + check)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("GitHub token auth smoke in %s: %v\n%s", agentImage, err, out)
	}
}
