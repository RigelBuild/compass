//go:build podman

package gateway

// Podman integration verifies socket ownership and connection after :Z relabel.
// The current probe is skipped until a container image with a Unix-socket client
// is available; socket mount and relabel arguments are covered hermetically.

import (
	"context"
	"os/exec"
	"testing"
)

// podmanUsable reports whether rootless podman can run a container here. A
// missing binary or broken rootless setup means skip, not fail — same guard as
// internal/runtime/lifecycle_test.go.
func podmanUsable(ctx context.Context) bool {
	return exec.CommandContext(ctx, "podman", "run", "--rm", "docker.io/library/alpine:latest", "true").Run() == nil
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
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman not usable in this environment")
	}
	t.Skip("SKELETON: in-container connect()-after-relabel round-trip not yet wired (needs an image with a unix-socket client probe); Mount fields + :Z relabel are covered hermetically in socket_test.go and internal/runtime/podman_test.go")

	// The eventual probe mounts the socket into a keep-id container, checks the
	// mapped uid owns it, then connects through the :Z relabel.
}
