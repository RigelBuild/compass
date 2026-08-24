//go:build podman

package runtime

// SEA-1359 portability proof (SEA-1327 Decision 1): the REAL agent base image
// carries a self-contained, single-user Nix store the agent owns and can
// activate a devenv from — with NO host /nix mount, no overlay, and no
// assumption the host has Nix. This is the "T8 agent-owns-/nix test" the
// container-runtime record pins
// (docs/designs/product/compass-agent-container-runtime.md:640).
//
// Distinct from lifecycle_test.go: that one bakes a tiny alpine Containerfile
// stand-in and proves the Launch/egress/teardown control flow. This one runs
// the ACTUAL containers.agent image and proves the in-image toolchain +
// store + direnv activation — the half a stand-in cannot exercise.
//
// THE LOAD-BEARING CONDITION — do not weaken. A Nix host would pass this
// VACUOUSLY: the host's own /nix could mask whether the in-image store works.
// So every store/eval assertion below is written to be meaningful ONLY because
// the container has no host /nix mounted (Launch adds no such mount — podman.go
// Create mounts only spec.Mounts, and this spec sets none), which assertion 3
// checks structurally, so a green proves the store operations below run against
// the in-image store, not a host one. The two ways this could go vacuously
// green instead of honestly are guarded explicitly:
//
//  1. Wrong image (the alpine stand-in has no nix) — requireRealImage skips
//     unless a real compass-agent image is named AND it actually carries nix.
//  2. Wrong in-container uid — the image bakes the agent user + /nix ownership
//     at uid 1000, and Launch creates with
//     --userns=keep-id:uid=1000,gid=1000 (podman.go:389), which maps WHATEVER
//     host uid the runner has to the baked in-container uid 1000 — so this runs
//     on a stock GitHub-hosted runner (host uid 1001) exactly as on a uid-1000
//     host. The test does NOT gate on host uid; assertion 1 verifies the
//     in-container agent actually IS uid 1000 at runtime, so a broken remap is a
//     red, not a vacuous pass.
//
// Build-tagged (podman) so it is out of the hermetic gate; podmanUsable()- and
// image-guarded so a container-less or image-less sandbox skips, never fails.
// Wherever the real image + rootless podman line up, the assertions are real
// regardless of host uid.

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

// realImageDefault is the local tag `agent-image` builds and the dogfood load
// produces. Used when COMPASS_AGENT_IMAGE is unset so a local run against a
// freshly-built image needs no env wiring.
const realImageDefault = "compass-agent:latest"

// bakedAgentUID is the uid the real image bakes the agent user, /nix ownership,
// and $HOME as. It is the shared agent-uid invariant (agentuid.AgentUID); the
// runtime package hardcodes this same uid throughout (podman.go:103,156,388 "the
// image bakes gid==uid==1000"). The whole point of the proof is that the
// in-container agent IS this uid and therefore owns /nix.
const bakedAgentUID uint32 = agentuid.AgentUID

// resolveRealImage returns the real agent image ref to prove against.
func resolveRealImage() string {
	if env := os.Getenv(realImageEnv); env != "" {
		return env
	}
	return realImageDefault
}

// imageHasNix reports whether ref exists in local storage AND actually carries a
// nix binary — the discriminator between the real base image and a stand-in
// (alpine has neither). The probe is PATH-independent: a non-login `sh -c` may
// not see a nix exposed only via profile.d, so it also checks the default
// single-user profile's absolute bin path. Either hit counts; both misses mean
// no nix. Skip-not-fail semantics stay (a false skip disables the whole proof),
// so the probe is deliberately generous about where nix may live.
func imageHasNix(ref string) bool {
	if exec.Command("podman", "image", "exists", ref).Run() != nil {
		return false
	}
	out, err := exec.Command(
		"podman", "run", "--rm", "--entrypoint=", ref,
		"sh", "-c", "command -v nix || test -x /nix/var/nix/profiles/default/bin/nix && echo nix",
	).CombinedOutput()
	return err == nil && strings.Contains(string(out), "nix")
}

// requireRealImage skips unless a real, nix-carrying agent image is available.
// A missing/stand-in image is a skip (the proof cannot run here), never a fail —
// but it will NOT silently accept an image without nix as if it proved anything.
func requireRealImage(t *testing.T) string {
	t.Helper()
	ref := resolveRealImage()
	if !imageHasNix(ref) {
		t.Skipf(
			"no real agent image with an in-image nix at %q (set %s to a built "+
				"compass-agent image); portability proof cannot run here",
			ref, realImageEnv)
	}
	return ref
}

// TestRealImagePortability is the SEA-1359 acceptance proof. It drives the
// production AgentRuntime.Launch against the REAL base image and, as the
// unprivileged agent user over the production ExecAsAgent path, proves the
// in-image store is agent-owned and self-sufficient and that direnv activates a
// workspace .envrc against it — with no host /nix mount.
func TestRealImagePortability(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	image := requireRealImage(t)

	name := "compass-portability-" + strconv.Itoa(os.Getpid())
	facade := NewAgentRuntime(NewPodmanCLI())
	ctx := context.Background()

	// Free the name from any crashed prior run, and guard against leaking a
	// container on a mid-test failure (Go has no Drop).
	_ = exec.Command("podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", name).Run() })

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

	// 2. The in-image Nix store is OWNED BY THE AGENT — the precondition for the
	// agent writing to it and rebuilding its own devenv (Decision 1: single-user,
	// /nix owned by the agent uid). `stat -c %u` gives the numeric owner uid; it
	// must be the agent's, not root's.
	assertAgentExec(t, ctx, facade, handle, "stat -c %u /nix/store", func(out string) bool {
		return strings.TrimSpace(out) == strconv.Itoa(int(bakedAgentUID))
	}, "/nix/store must be owned by the agent uid %d (agent-managed store)", bakedAgentUID)

	// 3. NO HOST /nix MOUNT — the core Decision 1 invariant, checked
	// structurally: /proc/self/mountinfo lists every mount in the container's
	// namespace, and none may cover /nix. A host-store leak (the rejected overlay
	// / bind-mount design) would show a /nix mount here; its absence proves the
	// store below is the in-image one, which is what makes assertions 4–5
	// non-vacuous rather than possibly reading a host store.
	assertAgentExec(t, ctx, facade, handle,
		`awk '$5 ~ "^/nix" {print $5}' /proc/self/mountinfo`,
		func(out string) bool { return strings.TrimSpace(out) == "" },
		"no host mount may cover /nix (Decision 1: no host /nix mount, no overlay)")

	// 4. The store is FUNCTIONAL AND WRITABLE by the agent: `nix-store --add`
	// writes a new path into the in-image store and returns it. This exercises
	// the store database + write path (not just the nix binary), needs no
	// network (a local file add) — the single-user rebuild-own-devenv capability
	// Decision 1 requires. A read-only store would fail here; assertion 3 already
	// established the store is the in-image one, not a host mount.
	assertAgentExec(t, ctx, facade, handle,
		"printf 'compass-t8\\n' > \"$HOME/t8src\" && nix-store --add \"$HOME/t8src\"",
		func(out string) bool { return strings.HasPrefix(strings.TrimSpace(out), "/nix/store/") },
		"agent must be able to write to the in-image store (single-user rebuild capability)")

	// 5. THE ACCEPTANCE CRITERION: direnv activates a workspace-root .envrc that
	// runs nix against the in-image store AT ACTIVATION TIME — the Runner-side
	// `direnv exec <WorkDir>` contract (Decision 1). The .envrc is written with a
	// quoted-delimiter heredoc ('EOF') so the $(...) stays LITERAL in the file
	// and runs when direnv loads it, not when the file is written; and the
	// expression is `readFile (toFile ...)`, which writes a path into /nix/store
	// and reads it back — so it exercises the store, not a pure builtin. A green
	// therefore ties activation to the in-image store working: if the store were
	// unwritable at activation the eval errors and the export is empty (verified:
	// an unwritable NIX_STORE_DIR makes toFile fail). Assertion 3 already
	// established the store is the in-image one, so this is activation against
	// that store specifically. CheckoutDir is a fixed test constant, not input.
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
