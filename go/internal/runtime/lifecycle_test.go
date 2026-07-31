//go:build podman

package runtime

// End-to-end per-agent container lifecycle against real rootless podman
// (compass.md §5.3), driving the production AgentRuntime.Launch path:
//
//  1. the Runner creates + starts a per-agent container,
//  2. the agent's checkout dir is present inside the container, owned by the
//     unprivileged agent user (the runner no longer clones — the agent
//     self-clones into it post-launch),
//  3. a command execs in-container as the unprivileged agent user (uid 1000),
//  4. the default-deny egress firewall holds — a non-allowlisted host is
//     blocked while an allowlisted one is reachable, and the agent user can't
//     tear the firewall down,
//  5. teardown removes the container.
//
// Skipped (not failed) when podman isn't usable, so the suite still runs in a
// container-less CI sandbox; the assertions are real wherever podman exists.
//
// The image is built once from a tiny Containerfile (a stand-in for a repo's
// devenv image, which carries the same tools) so Launch runs exactly as it does
// in production: `sleep infinity`, then provision-by-exec. Build-tagged
// (`podman`) so it is not part of the hermetic gate.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// imageTag is the image tag the test builds and runs the agent container from.
const imageTag = "compass-lifecycle-test:latest"

// allowedHost is an allowlisted host the firewall must let through. Stable,
// low-traffic.
const allowedHost = "example.com"

// containerfile is the agent container image: alpine plus git, nftables, and the
// unprivileged `agent` user (uid 1000) with a HOME and a workspace dir. A real
// deployment gets these from the repo's devenv image; the test bakes an
// equivalent.
const containerfile = `FROM docker.io/library/alpine:latest
RUN apk add --no-cache git nftables \
 && adduser -D -u 1000 agent \
 && mkdir -p /home/agent /work \
 && chown agent:agent /home/agent /work
`

// podmanUsable reports whether rootless podman can run a container here. A
// missing binary or broken rootless setup means skip, not fail.
func podmanUsable() bool {
	out, err := exec.Command("podman", "run", "--rm", "docker.io/library/alpine:latest", "true").CombinedOutput()
	_ = out
	return err == nil
}

// buildImage builds the agent image from containerfile into local storage.
func buildImage(t *testing.T, dir string) {
	t.Helper()
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(containerfile), 0o644); err != nil {
		t.Fatalf("write Containerfile: %v", err)
	}
	out, err := exec.Command("podman", "build", "-t", imageTag, "-f", file, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("podman build failed: %v\n%s", err, out)
	}
}

// runtimeExists is a direct `podman container exists` check for the
// post-teardown assertion, independent of the runtime under test.
func runtimeExists(name string) bool {
	return exec.Command("podman", "container", "exists", name).Run() == nil
}

func TestPerAgentContainerLifecycle(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}

	tmp := t.TempDir()
	buildImage(t, tmp)

	name := "compass-lifecycle-" + strconv.Itoa(os.Getpid())
	facade := NewAgentRuntime(NewPodmanCLI())
	ctx := context.Background()

	// Force-remove any leftover from a previous crashed run so the name is free,
	// and register a cleanup guard (Go has no Drop): a panic/failure before
	// teardown must never leak a container that collides with the next run.
	_ = exec.Command("podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", name).Run() })

	spec := AgentSpec{
		Name:  name,
		Image: imageTag,
		Workspace: Workspace{
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
			Credentials: nil,
		},
		Egress: MustAllowEgress(allowedHost),
	}

	// 1. The production lifecycle: create + start + arm egress + create checkout dir.
	handle, err := facade.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("launch the agent container: %v", err)
	}

	// 2. The agent's checkout dir exists and is owned by the agent user — the
	// runner no longer clones; the agent self-clones into this owned dir
	// post-launch. `test -w` as the agent confirms it can write there.
	writable, err := facade.ExecAsAgent(ctx, handle, "sh", "-c", "test -w /work/repo; echo rc=$?")
	if err != nil {
		t.Fatalf("check checkout dir: %v", err)
	}
	if !strings.Contains(writable.Stdout, "rc=0") {
		t.Fatalf("checkout dir must exist and be writable by the agent, got: %s", writable.Stdout)
	}

	// 3. Exec runs as the unprivileged agent user (uid 1000), not root.
	uid, err := facade.ExecAsAgent(ctx, handle, "id", "-u")
	if err != nil {
		t.Fatalf("id -u: %v", err)
	}
	if got := strings.TrimSpace(uid.Stdout); got != "1000" {
		t.Fatalf("agent uid = %q, want 1000 (must run unprivileged)", got)
	}

	// 4a. Egress default-deny holds: a non-allowlisted destination is blocked. A
	// raw IP with a short timeout — no DNS, so this tests the address-level drop
	// directly. Blocked => wget times out (non-zero rc).
	denied, err := facade.ExecAsAgent(ctx, handle, "sh", "-c",
		"wget -q -T 6 -O /dev/null https://1.1.1.1; echo rc=$?")
	if err != nil {
		t.Fatalf("denied probe: %v", err)
	}
	if strings.Contains(denied.Stdout, "rc=0") {
		t.Fatalf("non-allowlisted host must be blocked, got: %s", denied.Stdout)
	}

	// 4b. The allowlisted host IS reachable through the firewall.
	allowed, err := facade.ExecAsAgent(ctx, handle, "sh", "-c",
		"wget -q -T 10 -O /dev/null https://"+allowedHost+"; echo rc=$?")
	if err != nil {
		t.Fatalf("allowed probe: %v", err)
	}
	if !strings.Contains(allowed.Stdout, "rc=0") {
		t.Fatalf("allowlisted host must be reachable, got: %s", allowed.Stdout)
	}

	// 4c. Integrity: the unprivileged agent cannot tear down its own firewall.
	flush, err := facade.ExecAsAgent(ctx, handle, "sh", "-c", "nft flush ruleset; echo rc=$?")
	if err != nil {
		t.Fatalf("flush attempt: %v", err)
	}
	if strings.Contains(flush.Stdout, "rc=0") {
		t.Fatalf("agent user must not be able to flush the firewall, got: %s", flush.Stdout)
	}

	// 5. Teardown removes the container.
	if err := facade.Teardown(ctx, handle); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if runtimeExists(name) {
		t.Fatal("container must be gone after teardown")
	}
}
