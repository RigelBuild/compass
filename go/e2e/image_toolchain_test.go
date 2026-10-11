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

// TestAgentImageGHTokenAuth installs tokens with the production GHHostsScript and
// checks Git and jj-vine get only the requested host's token, never another's.
func TestAgentImageGHTokenAuth(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	const tok = "ghs_fixture"
	// tokenCommand is a TOML string array; $argv is unquoted so it splits into argv.
	// $host stands in for jj-vine's {host} substitution.
	const tokenCommand = `argv=$(jj config get jj-vine.github.tokenCommand | tr -d '[],"' | sed "s/{host}/$host/")
got=$($argv 2>/dev/null || true)
`
	// gitPassword prints the password Git's credential helpers return for $host.
	const gitPassword = `pw=$(printf 'protocol=https\nhost=%s\n\n' "$host" | GIT_TERMINAL_PROMPT=0 git credential fill 2>/dev/null | sed -n 's/^password=//p' || true)
`
	for _, tc := range []struct {
		name  string
		creds []runtime.GHCredentials
		check string
	}{
		{
			name:  "github token",
			creds: []runtime.GHCredentials{{Host: "github.example.com", Token: "ghs_other"}, {Host: "github.com", Token: tok}},
			check: `host=github.com
` + gitPassword + `test "$pw" = "` + tok + `" || { echo "git credential returned $pw"; exit 1; }
` + tokenCommand + `test "$got" = "` + tok + `" || { echo "tokenCommand returned $got"; exit 1; }
`,
		},
		{
			name:  "other host only",
			creds: []runtime.GHCredentials{{Host: "github.example.com", Token: "ghs_other"}},
			check: `host=github.com
` + gitPassword + `test -z "$pw" || { echo "git credential returned $pw"; exit 1; }
` + tokenCommand + `test -z "$got" || { echo "tokenCommand returned $got"; exit 1; }
host=github.example.com
` + gitPassword + `test "$pw" = ghs_other || { echo "git credential for GHE returned $pw"; exit 1; }
`,
		},
		{
			// jj-vine derives the GHE host from origin; the github.com token must not reach it.
			name:  "ghe remote with only a github token",
			creds: []runtime.GHCredentials{{Host: "github.com", Token: tok}},
			check: `cd "$(mktemp -d)" && jj git init . >/dev/null 2>&1 && jj git remote add origin https://github.example.com/owner/repo.git
out=$(jj-vine status 2>&1) && { echo "jj-vine status succeeded: $out"; exit 1; }
case "$out" in *"tokenCommand failed"*) ;; *) echo "jj-vine status: $out"; exit 1 ;; esac
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
