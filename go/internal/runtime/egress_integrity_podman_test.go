//go:build podman

package runtime

// This real-podman proof checks that agent-uid exec loses CAP_NET_ADMIN in a privileged container.
// Hermetic tests pin that production exec call sites set --user; this verifies podman enforces it.
// The podman-tagged test skips when the engine is unavailable.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/agentuid"
)

// capEffOf runs `grep CapEff /proc/self/status` inside container id as the given
// ExecSpec identity and returns the hex effective-capability mask (the token
// after "CapEff:"). It drives the real PodmanCLI.Exec so the --user plumbing
// under test is the one exercised in production.
func capEffOf(t *testing.T, ctx context.Context, cli *PodmanCLI, id WorkloadID, spec ExecSpec) string {
	t.Helper()
	out, err := cli.Exec(ctx, id, spec)
	if err != nil {
		t.Fatalf("exec CapEff probe: %v", err)
	}
	if !out.Success() {
		t.Fatalf("CapEff probe exited %d: %s", out.ExitCode, out.Stderr)
	}
	// /proc/self/status line: "CapEff:\t0000000000000000"
	for line := range strings.SplitSeq(out.Stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "CapEff:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no CapEff line in /proc/self/status: %q", out.Stdout)
	return ""
}

// TestAgentExecDropsNetAdminInNetAdminContainer verifies agent execs lack CAP_NET_ADMIN
// while the container's uid-0 exec retains it; dropping --user makes the masks equal.
func TestAgentExecDropsNetAdminInNetAdminContainer(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := t.Context()
	cli := NewPodmanCLI()

	// A NET_ADMIN container remapped to the baked agent uid, mirroring a real
	// agent container's create (agent.go createAndStart).
	const agentUID uint32 = agentuid.AgentUID
	spec := WorkloadSpec{
		Image:   "docker.io/library/alpine:latest",
		Name:    "compass-egress-integrity-" + strconv.Itoa(os.Getpid()),
		UID:     agentUID,
		CapAdd:  []string{capNetAdmin},
		Command: []string{"sleep", "infinity"},
	}
	// Bring the NET_ADMIN container up, mirroring createStartExec's
	// create/start/force-rm-teardown (userns_remap_test.go) but holding the id so
	// we can exec against it twice with different identities.
	cleanupCtx := context.WithoutCancel(ctx)
	_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", spec.Name).Run()
	t.Cleanup(func() { _ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", spec.Name).Run() })
	cid, err := cli.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if err := cli.Start(ctx, cid); err != nil {
		t.Fatalf("start container: %v", err)
	}

	probe := []string{"grep", "CapEff", "/proc/self/status"}

	// The armEgress identity: --user 0, container root, holding the container's
	// CAP_NET_ADMIN so it can arm nft and write the /etc/hosts pins.
	privileged := capEffOf(t, ctx, cli, cid, NewExecSpec(probe...).AsUser("0"))
	// The agent-work identity: explicit --user <agent uid>, empty capability set.
	agent := capEffOf(t, ctx, cli, cid, NewExecSpec(probe...).AsUser(strconv.FormatUint(uint64(agentUID), 10)))

	if agent != "0000000000000000" {
		t.Fatalf("agent-uid exec CapEff = %q, want an empty set %q: an --user agent exec must hold no capabilities so it cannot alter the egress ruleset (egress.go)", agent, "0000000000000000")
	}
	if privileged == agent {
		t.Fatalf("uid-0 exec CapEff = %q equals the agent-uid exec's = %q: the NET_ADMIN the arm runs with must not be inherited by agent execs", privileged, agent)
	}
	mask, err := strconv.ParseUint(privileged, 16, 64)
	if err != nil {
		t.Fatalf("parse uid-0 exec CapEff %q: %v", privileged, err)
	}
	if mask&(1<<12) == 0 {
		t.Fatalf("uid-0 exec CapEff = %q, want the CAP_NET_ADMIN bit (0x1000) set — the armEgress identity must retain the capability it arms nft with", privileged)
	}
}
