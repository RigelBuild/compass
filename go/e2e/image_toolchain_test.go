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
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	for _, tool := range []struct{ bin, want string }{
		{"jj", "jj "},
		{"jj-vine", "jj-vine "},
	} {
		t.Run(tool.bin, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "podman", "run", "--rm", agentImage, tool.bin, "--version").CombinedOutput()
			if err != nil {
				t.Fatalf("%s --version in %s: %v\n%s", tool.bin, agentImage, err, out)
			}
			if !strings.HasPrefix(strings.TrimSpace(string(out)), tool.want) {
				t.Fatalf("%s --version printed %q, want prefix %q", tool.bin, out, tool.want)
			}
		})
	}
}

// TestAgentImageGHTokenAuth verifies owner-aware Git and jj-vine credential routing.
func TestAgentImageGHTokenAuth(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the image toolchain check")
	}
	tokenA, tokenB := "owner-a-token", "owner-b-token"
	ownerFiles, err := runtime.GHOwnerTokensScript("/home/agent", []runtime.GHCredentials{
		{Host: "github.com", Owner: "owner-a", Token: tokenA},
		{Host: "github.com", Owner: "owner-b", Token: tokenB},
	})
	if err != nil {
		t.Fatalf("GHOwnerTokensScript: %v", err)
	}
	defaultHosts, err := runtime.GHHostsScript("/home/agent", []runtime.GHCredentials{{Host: "github.com", Owner: "owner-a", Token: tokenA}})
	if err != nil {
		t.Fatalf("GHHostsScript: %v", err)
	}
	checks := []struct {
		name  string
		setup string
		check string
	}{
		{name: "owner tokens", check: `for pair in owner-a/one:owner-a-token owner-b/two:owner-b-token; do
  repo=${pair%%:*}; want=${pair#*:}
  got=$(printf 'protocol=https\nhost=github.com\npath=%s\n\n' "$repo" | git credential fill | sed -n 's/^password=//p')
  test "$got" = "$want" || { echo "credential mismatch for $repo"; exit 1; }
done`},
		{name: "unknown owner refused", check: `out=$(printf 'protocol=https\nhost=github.com\npath=owner-c/three\n\n' | git credential fill 2>&1) && { echo "unexpected credential for owner-c"; exit 1; }
case "$out" in *"no credential for owner owner-c"*) ;; *) echo "$out"; exit 1 ;; esac`},
		{name: "default owner falls through; unknown owner refused", setup: `rm -f /home/agent/.compass/forge/github.com/owner-a
`, check: `got=$(printf 'protocol=https\nhost=github.com\npath=owner-a/one\n\n' | git credential fill | sed -n 's/^password=//p')
test "$got" = owner-a-token || { echo "default fallback mismatch"; exit 1; }
out=$(printf 'protocol=https\nhost=github.com\npath=owner-c/three\n\n' | git credential fill 2>&1) && { echo "wildcard owner unexpectedly accepted"; exit 1; }
case "$out" in *"no credential for owner owner-c"*) ;; *) echo "$out"; exit 1 ;; esac`},
		{name: "jj token command resolves project owner", check: `a=$(compass-forge-credential token --host github.com --repo owner-a/one)
	b=$(compass-forge-credential token --host github.com --repo owner-b/two)
	test "$a" = owner-a-token && test "$b" = owner-b-token
	if compass-forge-credential token --host github.com --repo owner-c/three; then echo "unknown owner token accepted"; exit 1; fi`},
		{name: "marker-free arbitrary owner falls through", setup: `rm -f /home/agent/.compass/forge/github.com/.default-owner
			printf '%s\n' fallback-token | gh auth login --with-token
`, check: `got=$(printf 'protocol=https\nhost=github.com\npath=owner-z/repo\n\n' | git credential fill | sed -n 's/^password=//p')
test "$got" = fallback-token || { echo "Git marker-free fallback mismatch"; exit 1; }
got=$(compass-forge-credential token --host github.com --repo owner-z/repo)
test "$got" = fallback-token || { echo "jj marker-free fallback mismatch"; exit 1; }`},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "podman", "run", "--rm", "-i", "-e", "HOME=/home/agent", "-e", "GH_TOKEN=fallback-token", agentImage, "sh", "-s")
			cmd.Stdin = strings.NewReader(ownerFiles + defaultHosts + "set -eu\n" + tc.setup + tc.check)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("owner credential check in %s: %v\n%s", agentImage, err, out)
			}
		})
	}
}
