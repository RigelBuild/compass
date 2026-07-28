//go:build podman

package gateway

// Integration skeleton for the socket's uid-map + :Z-relabel invariant
// (SEA-1351 T2, design docs/designs/product/compass-agent-runner-transport/
// design.md:397-400): the host socket, bind-mounted via SocketListener.Mount and
// relabelled :Z by the runtime, must be OWNED by the mapped agent uid in-container
// and connect()-able from inside AFTER the relabel. This extends the
// container-runtime T8 agent-owns invariant onto an enforcing (SELinux) host.
//
// Build-tagged `podman` so it is not part of the hermetic gate; skipped (not
// failed) when rootless podman is unusable, mirroring
// internal/runtime/lifecycle_test.go. The hermetic half of this invariant — that
// Mount() targets the live socket read-write, so the runtime emits `host:container:Z`
// (never `:ro,Z`, which would block connect()) — is proven without a container in
// socket_test.go (TestMountTargetsLiveSocketReadWrite) and
// internal/runtime/podman_test.go (TestMountArgRelabel).
//
// STATUS: skeleton. The in-container connect()-after-relabel round-trip is not yet
// implemented — it needs a small devenv-equivalent image carrying a Connect/gRPC
// client binary (or a `socat`/`nc -U` probe against the socket) to exercise the
// door from inside the userns. Wiring that image + client is the remaining work;
// the structure below fixes the shape so filling it in is mechanical.

import (
	"os/exec"
	"testing"
)

// podmanUsable reports whether rootless podman can run a container here. A
// missing binary or broken rootless setup means skip, not fail — same guard as
// internal/runtime/lifecycle_test.go.
func podmanUsable() bool {
	return exec.Command("podman", "run", "--rm", "docker.io/library/alpine:latest", "true").Run() == nil
}

// TestSocketOwnedAndConnectableInContainer asserts the design's agent-owns +
// connect()-able-after-relabel invariant end to end: the Runner creates the
// listener on the host, bind-mounts it via l.Mount(containerPath) into a
// --userns=keep-id container relabelled :Z, and from INSIDE the container the
// mapped agent uid both OWNS the socket inode and can open a connection to the
// live door across the relabel boundary.
//
// A regression this must catch: a mount that drops :Z (connect() denied by
// SELinux on an enforcing host) or maps the socket to the wrong uid (agent can
// see but not own it), either of which silently breaks the local agent->Runner
// hop on production hosts while every hermetic test stays green.
func TestSocketOwnedAndConnectableInContainer(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	t.Skip("SKELETON: in-container connect()-after-relabel round-trip not yet wired (needs an image with a unix-socket client probe); Mount fields + :Z relabel are covered hermetically in socket_test.go and internal/runtime/podman_test.go")

	// Intended shape once the probe image lands (mirrors
	// internal/runtime/lifecycle_test.go's Launch/ExecAsAgent flow):
	//
	//   1. l, _ := listenAgentSocket(ctx, hostPath, stubHandler(t), func() {})
	//      defer l.Close(ctx)
	//   2. build a tiny image carrying a unix-socket client (a Connect/gRPC probe,
	//      or `socat - UNIX-CONNECT:<path>`), run it with --userns=keep-id and
	//      Mounts: []runtime.Mount{ l.Mount("/run/compass/agent.sock") } so the
	//      runtime emits `host:/run/compass/agent.sock:Z`.
	//   3. exec `stat -c %u /run/compass/agent.sock` as the agent uid inside →
	//      assert it equals the mapped agent uid (agent OWNS the socket).
	//   4. exec the client probe against /run/compass/agent.sock as the agent uid →
	//      assert the connection is accepted and the stub answers Unimplemented
	//      (connect() works ACROSS the :Z relabel on an enforcing host).
}
