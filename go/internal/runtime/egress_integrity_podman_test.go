//go:build podman

package runtime

// Egress-integrity boundary proof against real rootless podman: an agent exec —
// run as the agent uid with an explicit --user — holds an EMPTY effective
// capability set inside a NET_ADMIN container, so it cannot alter the egress
// ruleset the container's privileged entrypoint armed (egress.go:6-10, "the
// agent then runs as a non-root user whose capability set is empty, so it cannot
// flush or edit the ruleset even though the container nominally holds the
// capability").
//
// The defect this pins: if an agent exec omitted --user it would run as the
// container's default user and inherit the container's added CAP_NET_ADMIN,
// letting a compromised agent `nft flush ruleset` to disarm its own egress. Every
// agent-controllable exec sets --user (agent_exec.go, agent.go's ExecAsAgent /
// credential / config / checkout execs); the only nil-user exec is the Runner's
// own armEgress provisioning step, which legitimately needs the capability. This
// test locks the two halves so a future refactor that drops an --user (e.g. a
// rename moving an exec-spec construction) reddens rather than silently
// reintroducing the escape.
//
// Skipped (not failed) when podman isn't usable, matching lifecycle_test /
// config_mount_test. Build-tagged (podman) so it is not part of the hermetic gate.

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// capEffOf runs `grep CapEff /proc/self/status` inside container id as the given
// ExecSpec identity and returns the hex effective-capability mask (the token
// after "CapEff:"). It drives the real PodmanCLI.Exec so the --user plumbing
// under test is the one exercised in production.
func capEffOf(t *testing.T, ctx context.Context, cli *PodmanCLI, id ContainerID, spec ExecSpec) string {
	t.Helper()
	out, err := cli.Exec(ctx, id, spec)
	if err != nil {
		t.Fatalf("exec CapEff probe: %v", err)
	}
	if !out.Success() {
		t.Fatalf("CapEff probe exited %d: %s", out.ExitCode, out.Stderr)
	}
	// /proc/self/status line: "CapEff:\t0000000000000000"
	for _, line := range strings.Split(out.Stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "CapEff:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no CapEff line in /proc/self/status: %q", out.Stdout)
	return ""
}

// TestAgentExecDropsNetAdminInNetAdminContainer is the egress-integrity
// regression: in a container granted CAP_NET_ADMIN (as every agent container
// is, agent.go createAndStart CapAdd), an exec pinned to the agent uid via
// --user must have an all-zero effective capability set, while the container's
// default-user exec (the armEgress identity) retains the capability. A regression
// that drops the --user on an agent exec makes the two masks equal and fails
// here.
func TestAgentExecDropsNetAdminInNetAdminContainer(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background()
	cli := NewPodmanCLI()

	// A NET_ADMIN container remapped to the baked agent uid, mirroring a real
	// agent container's create (agent.go createAndStart).
	const agentUID uint32 = 1000
	spec := ContainerSpec{
		Image:   "docker.io/library/alpine:latest",
		Name:    "compass-egress-integrity-" + strconv.Itoa(os.Getpid()),
		UID:     agentUID,
		CapAdd:  []string{capNetAdmin},
		Command: []string{"sleep", "infinity"},
	}
	// Bring the NET_ADMIN container up, mirroring createStartExec's
	// create/start/force-rm-teardown (userns_remap_test.go) but holding the id so
	// we can exec against it twice with different identities.
	_ = exec.Command("podman", "rm", "--force", spec.Name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", spec.Name).Run() })
	cid, err := cli.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if err := cli.Start(ctx, cid); err != nil {
		t.Fatalf("start container: %v", err)
	}

	probe := []string{"grep", "CapEff", "/proc/self/status"}

	// The armEgress identity: nil --user, runs as the image default user and
	// inherits the container's CAP_NET_ADMIN so it can arm nft.
	privileged := capEffOf(t, ctx, cli, cid, NewExecSpec(probe...))
	// The agent-work identity: explicit --user <agent uid>, empty capability set.
	agent := capEffOf(t, ctx, cli, cid, NewExecSpec(probe...).AsUser(strconv.FormatUint(uint64(agentUID), 10)))

	if agent != "0000000000000000" {
		t.Fatalf("agent-uid exec CapEff = %q, want an empty set %q: an --user agent exec must hold no capabilities so it cannot alter the egress ruleset (egress.go)", agent, "0000000000000000")
	}
	if privileged == agent {
		t.Fatalf("default-user exec CapEff = %q equals the agent-uid exec's = %q: the NET_ADMIN the entrypoint arms with must not be inherited by agent execs", privileged, agent)
	}
	if !strings.Contains(privileged, "1000") {
		t.Fatalf("default-user exec CapEff = %q, want the CAP_NET_ADMIN bit (0x1000) set — the armEgress identity must retain the capability it arms nft with", privileged)
	}
}
