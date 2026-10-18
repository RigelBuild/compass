//go:build podman

package runtime

// This podman proof verifies the real agent image owns and can use its Nix store
// without a host /nix mount. The alpine lifecycle stand-in cannot prove image behavior.
// The test checks uid remapping and store activation to prevent a host store from masking failure.
// It skips when rootless podman or a real agent image is unavailable.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/agentuid"
)

// realImageEnv is the env var naming the real agent base image to prove against
// (the same var compass-runner and the embedded app honor for the image ref).
// Unset falls back to realImageDefault.
const realImageEnv = "COMPASS_AGENT_IMAGE"

// realImageDefault is the local tag used after builds and dogfood image loads.
const realImageDefault = "compass-agent:latest"

// bakedAgentUID is the agent uid that owns the real image's Nix store.
const bakedAgentUID uint32 = agentuid.AgentUID

// resolveRealImage returns the real agent image ref to prove against.
func resolveRealImage() string {
	if env := os.Getenv(realImageEnv); env != "" {
		return env
	}
	return realImageDefault
}

// imageHasNix checks local image presence and probes PATH plus the default profile.
// Either location can carry nix in the image.
func imageHasNix(ctx context.Context, ref string) bool {
	if exec.CommandContext(ctx, "podman", "image", "exists", ref).Run() != nil {
		return false
	}
	out, err := exec.CommandContext(ctx,
		"podman", "run", "--rm", "--entrypoint=", ref,
		"sh", "-c", "command -v nix || test -x /nix/var/nix/profiles/default/bin/nix && echo nix",
	).CombinedOutput()
	return err == nil && strings.Contains(string(out), "nix")
}

// requireRealImage skips unless a real, nix-carrying agent image is available.
// A missing/stand-in image is a skip (the proof cannot run here), never a fail —
// but it will NOT silently accept an image without nix as if it proved anything.
func requireRealImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	ref := resolveRealImage()
	if !imageHasNix(ctx, ref) {
		t.Skipf(
			"no real agent image with an in-image nix at %q (set %s to a built "+
				"compass-agent image); portability proof cannot run here",
			ref, realImageEnv)
	}
	return ref
}

// TestRealImagePortability is the RIG-1359 acceptance proof. It drives the
// production AgentRuntime.Launch against the REAL base image and, as the
// unprivileged agent user over the production ExecAsAgent path, proves the
// in-image store is agent-owned and self-sufficient and that direnv activates a
// workspace .envrc against it — with no host /nix mount.
func TestRealImagePortability(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman not usable in this environment")
	}
	image := requireRealImage(t, t.Context())

	name := "compass-portability-" + strconv.Itoa(os.Getpid())
	facade := NewAgentRuntime(NewPodmanCLI())
	ctx := t.Context()

	// Free the name from any crashed prior run, and guard against leaking a
	// container on a mid-test failure (Go has no Drop).
	cleanupCtx := context.WithoutCancel(ctx)
	_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", name).Run() })

	spec := AgentSpec{
		Name:  name,
		Image: image,
		Workspace: Workspace{
			// The agent's $HOME the image bakes at bakedAgentUID; the workspace
			// root the agent activates a devenv in.
			CheckoutDir: "/home/agent/work",
			HomeDir:     "/home/agent",
			UID:         bakedAgentUID,
			Credentials: nil,
		},
		// Default-open is not yet the landed default (pre-T2), so allowlist the
		// one host the arm step needs to succeed; egress policy is not what this
		// proof is about — lifecycle_test.go covers the firewall.
		Egress: MustAllowEgress(allowedHost),
	}

	// The production lifecycle brings the real-image container online.
	handle, err := facade.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("launch the real-image agent container: %v", err)
	}
	defer func() {
		if err := facade.Teardown(ctx, handle); err != nil {
			t.Errorf("teardown: %v", err)
		}
	}()

	// 1. The container runs as the baked agent uid over the production exec
	// path — the precondition that makes /nix ownership meaningful.
	assertAgentExec(t, ctx, facade, handle, "id -u", func(out string) bool {
		return strings.TrimSpace(out) == strconv.Itoa(int(bakedAgentUID))
	}, "agent must run as the baked uid %d", bakedAgentUID)

	// 2. The in-image Nix store must belong to the agent so it can write and rebuild devenv.
	// A root-owned store would violate the single-user ownership contract.
	assertAgentExec(t, ctx, facade, handle, "stat -c %u /nix/store", func(out string) bool {
		return strings.TrimSpace(out) == strconv.Itoa(int(bakedAgentUID))
	}, "/nix/store must be owned by the agent uid %d (agent-managed store)", bakedAgentUID)

	// No /nix mount may cover the image's store; this prevents a host store from masking failure.
	assertAgentExec(t, ctx, facade, handle,
		`awk '$5 ~ "^/nix" {print $5}' /proc/self/mountinfo`,
		func(out string) bool { return strings.TrimSpace(out) == "" },
		"no host mount may cover /nix (Decision 1: no host /nix mount, no overlay)")

	// nix-store --add proves the agent can write to the in-image store without network access.
	assertAgentExec(t, ctx, facade, handle,
		"printf 'compass-t8\\n' > \"$HOME/t8src\" && nix-store --add \"$HOME/t8src\"",
		func(out string) bool { return strings.HasPrefix(strings.TrimSpace(out), "/nix/store/") },
		"agent must be able to write to the in-image store (single-user rebuild capability)")

	// Direnv must evaluate nix against the in-image store when loading the workspace .envrc.
	// The quoted heredoc delays the nix expression until activation, and toFile exercises writes.
	activate := `set -e
cd ` + spec.Workspace.CheckoutDir + `
cat > .envrc <<'EOF'
export COMPASS_PORTABILITY="$(nix eval --raw --expr 'builtins.readFile (builtins.toFile "t8" "activated")')"
EOF
direnv allow .
direnv exec ` + spec.Workspace.CheckoutDir + ` sh -c 'printf %s "$COMPASS_PORTABILITY"'`
	assertAgentExec(t, ctx, facade, handle, activate, func(out string) bool {
		return strings.TrimSpace(out) == "activated"
	}, "direnv exec must activate the workspace .envrc (value computed by nix reading the in-image store at load time)")
}

// assertAgentExec runs command as the agent user over the production
// ExecAsAgent path and fails the test unless want(stdout) holds. The exec is
// `sh -c command` so a compound activation script runs as one agent-user shell.
func assertAgentExec(
	t *testing.T,
	ctx context.Context,
	facade *AgentRuntime,
	handle *AgentHandle,
	command string,
	want func(stdout string) bool,
	msgf string,
	args ...any,
) {
	t.Helper()
	out, err := facade.ExecAsAgent(ctx, handle, "sh", "-c", command)
	if err != nil {
		t.Fatalf(msgf+": exec %q: %v", append(args, command, err)...)
	}
	if !want(out.Stdout) {
		t.Fatalf(msgf+": got stdout %q, stderr %q",
			append(args, out.Stdout, out.Stderr)...)
	}
}
