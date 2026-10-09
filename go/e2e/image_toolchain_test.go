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

// TestAgentImageGHTokenAuth installs fixture tokens with the production
// GHHostsScript, then checks Git and jj-vine's tokenCommand return only the
// github.com token, never another host's.
func TestAgentImageGHTokenAuth(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	const tok = "ghs_fixture"
	// tokenCommand is a TOML string array; $argv is unquoted so it splits into argv.
	const tokenCommand = `argv=$(jj config get jj-vine.github.tokenCommand | tr -d '[],"')
got=$($argv 2>/dev/null || true)
`
	for _, tc := range []struct {
		name  string
		creds []runtime.GHCredentials
		check string
	}{
		{
			name:  "github token",
			creds: []runtime.GHCredentials{{Host: "ghe.example.com", Token: "ghs_other"}, {Host: "github.com", Token: tok}},
			check: `credential=$(printf 'protocol=https\nhost=github.com\n\n' | git credential fill)
case "$credential" in *"password=` + tok + `"*) ;; *) echo "git credential: $credential"; exit 1 ;; esac
` + tokenCommand + `test "$got" = "` + tok + `" || { echo "tokenCommand returned $got"; exit 1; }
`,
		},
		{
			name:  "other host only",
			creds: []runtime.GHCredentials{{Host: "ghe.example.com", Token: "ghs_other"}},
			check: tokenCommand + `test -z "$got" || { echo "tokenCommand returned $got"; exit 1; }
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			install, err := runtime.GHHostsScript("/home/agent", tc.creds)
			if err != nil {
				t.Fatalf("GHHostsScript: %v", err)
			}
			cmd := exec.Command("podman", "run", "--rm", "-i", "-e", "HOME=/home/agent", agentImage, "sh", "-s")
			cmd.Stdin = strings.NewReader(install + "set -eu\n" + tc.check)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("GitHub token auth smoke in %s: %v\n%s", agentImage, err, out)
			}
		})
	}
}
