//go:build podman

package runtime

// End-to-end per-agent container lifecycle against real rootless podman
// (compass.md §5.3), driving the production AgentRuntime.Launch path:
//
//  1. the Runner creates + starts a per-agent container,
//  2. its own git clone is present inside the container,
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

// git runs a git command in cwd, failing the test on a non-zero exit.
func git(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// makeBareRepo builds a bare git repo on the host with one commit on `main`.
// Bind-mounted read-only and cloned over file:// — hermetic, needs no network
// (so it clones fine under the egress firewall).
func makeBareRepo(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(dir, "demo.git")
	work := filepath.Join(dir, "work")
	git(t, dir, "init", "--quiet", "--bare", bare)
	git(t, dir, "-C", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, dir, "clone", "--quiet", bare, work)
	git(t, work, "config", "user.email", "test@compass.local")
	git(t, work, "config", "user.name", "compass-test")
	git(t, work, "checkout", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("compass-lifecycle\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "seed")
	git(t, work, "push", "--quiet", "origin", "main")
	return bare
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
	bare := makeBareRepo(t, tmp)

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
			Source:      LocalPathSource("/src/demo.git"),
			Branch:      "main",
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
			Credentials: nil,
		},
		Egress: MustAllowEgress(allowedHost),
		Mounts: []Mount{{
			HostPath:      bare,
			ContainerPath: "/src/demo.git",
			ReadOnly:      true,
		}},
	}

	// 1. The production lifecycle: create + start + arm egress + clone.
	handle, err := facade.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("launch the agent container: %v", err)
	}

	// 2. The agent's own clone is present with the seeded content.
	readme, err := facade.ExecAsAgent(ctx, handle, "cat", "/work/repo/README.md")
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	if !readme.Success() {
		t.Fatalf("clone missing: %s", readme.Stderr)
	}
	if got := strings.TrimSpace(readme.Stdout); got != "compass-lifecycle" {
		t.Fatalf("cloned README = %q, want %q", got, "compass-lifecycle")
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
