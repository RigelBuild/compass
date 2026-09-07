//go:build microvm && unix

package runtime

// The RIG-3020 (V3 W3) KVM-gated in-guest egress integration suite: the
// milestone acceptance gate for egress-in-guest, driving a real MicroVMRuntime
// through Create→Start→Exec on live hardware so the egress firewall is proven
// where it actually runs — inside the guest netns, armed by guestd as guest
// root before the exec gate opens (design §(a)/(c)/(e)). Every test opens with
// microvmtest.Require(t): on a KVM-less box it SKIPS (unless
// COMPASS_REQUIRE_MICROVM=1 forces a hard fail), so the suite is only real where
// /dev/kvm is openable and the guest images are exported into the env.
//
// It complements, not duplicates, the hermetic W2 suite (microvm_start_test.go)
// and the direct-guestd arm proofs (microvm/boot_microvm_test.go
// TestInGuestEgressArmAutoloadsNetfilter, microvm/egress_arm_microvm_test.go):
// those prove script delivery, netfilter autoload, and the fail-closed arm in
// isolation; this proves the whole path end-to-end at the runtime API the
// Runner calls, with real agent-uid execs hitting the armed ruleset.
//
// Probe mechanism: reachability rides a bash `/dev/tcp` connect to a RAW IP
// (the guest ships bash+sh+nft+coreutils only — no wget/curl — and a DNS name
// stalls the harness resolver), bounded by a guest-side `timeout`; see
// canReachIPv4 and the const block for the full dropped-SYN → exit-124 rationale
// and the timeout ordering it depends on. Mirrors the podman lifecycle proof's
// raw-IPv4 allow/deny (lifecycle_test.go:137-166), inside the guest netns.
//
// IPv6 / dual-stack: the microVM guest network (passt) and the CI runners
// provide NO IPv6 route, so a live v6 connect fails ENETUNREACH regardless of
// the firewall — a live v6 assertion would pass vacuously (it cannot fail on a
// removed v6 rule), which is worse than none. The dual-stack property (every
// allowlisted host populates BOTH the allow4 and allow6 nft sets) is proven
// hermetically at the script level (egress_test.go
// TestAllowlistedHostPopulatesBothFamilies / TestEveryAllowlistedHostIsResolved),
// so it is not re-asserted live here. See the PR's Open Questions.
//
// V8 alignment (record §W3(5), microvm-runner.md:609-621): this suite satisfies
// V8 row (2) — "egress fail-closed asserted inside the guest netns" is exactly
// what runs here under the full backend. V8 row (8)'s re-arm half (an agent-uid
// process cannot re-arm/alter the ruleset) is covered by
// TestInGuestEgressAgentCannotAlterRuleset below plus W1's already-provisioned
// refusal and the peer-CID listener V2b built. No new V8 scope lands here.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

const (
	// egressProbeTimeout bounds one in-guest reachability exec (the host-side
	// ctx). It must exceed the guest-side `timeout` below so the guest's own
	// bounded connect reports (exit 124) rather than the host ctx firing first.
	egressProbeTimeout = 25 * time.Second
	// guestConnectTimeout is the guest-side `timeout N` (seconds, as a string for
	// direct interpolation into the probe script) wrapping the /dev/tcp connect:
	// a dropped SYN hangs, so this is what turns a blocked host into a bounded
	// exit-124 rather than an indefinite hang. It MUST be less than
	// egressProbeTimeout so the guest's own timeout fires first (a blocked host
	// then reports exit-124, not a host-ctx DeadlineExceeded read as a fault).
	guestConnectTimeout = "10"
	// allowedIP is the raw IPv4 the firewall must let through; deniedIP is a
	// different globally-reachable raw IPv4 the default-deny policy must block.
	// Both are stable anycast hosts reachable from CI when NOT firewalled, so a
	// blocked deniedIP proves the firewall (not an unreachable host). Raw IPs,
	// never names — DNS resolution stalls the harness (see the file header).
	allowedIP = "1.1.1.1"
	deniedIP  = "8.8.8.8"
)

// startEgressSession boots a real microVM session with the given egress policy
// and a throwaway workspace, returning the runtime + id ready for agent-uid
// execs. It registers teardown. The session is armed in-guest by guestd during
// Start (the Provision RPC carries the recorded nft_script), so by the time this
// returns the firewall is live.
func startEgressSession(t *testing.T, egress EgressPolicy, name string) (*MicroVMRuntime, ContainerID) {
	t.Helper()
	env := microvmtest.Require(t)
	m := NewMicroVMRuntime(e2eConfig(t, env))

	workspace := t.TempDir()
	id, err := m.Create(t.Context(), ContainerSpec{
		Name:   name,
		UID:    agentuid.AgentUID,
		Egress: egress,
		Mounts: []Mount{{HostPath: workspace, ContainerPath: "/workspace"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = m.Stop(context.WithoutCancel(t.Context()), id, 5*time.Second)
		_ = m.Remove(context.WithoutCancel(t.Context()), id)
	})

	// Start with t.Context(), NOT a WithTimeout/defer-cancel ctx: microvm.Launch
	// spawns the VMM (+virtiofsd/passt) via exec.CommandContext bound to this
	// ctx, so the Start ctx is the VM's LIFETIME — cancelling it kills the guest.
	// A helper-local `defer cancel()` fires on return, before any exec, and would
	// tear the VM down mid-session (guestd dies → vsock reset). The boot is
	// already bounded internally (bootDeadline) and the whole run by the KVM
	// -timeout, so t.Context() is both correct and sufficient (the contract-suite
	// pattern, contract_suite_test.go).
	if startErr := m.Start(t.Context(), id); startErr != nil {
		t.Fatalf("Start (in-guest egress arm must succeed): %v", startErr)
	}
	return m, id
}

// canReachIPv4 execs an agent-uid bash /dev/tcp connect to ip:443 inside the
// guest, bounded by a guest-side `timeout`, and reports whether the handshake
// completed. A blocked host's SYN is dropped by the default-deny policy, so the
// connect hangs until the guest `timeout` fires (exit 124) — reported as
// unreachable; an allowed host completes (exit 0, "connected"). A non-zero exit
// with no "connected" is unreachable; any transport/exec error fails the test
// (that is a harness fault, not a firewall verdict).
func canReachIPv4(t *testing.T, m *MicroVMRuntime, id ContainerID, ip string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), egressProbeTimeout)
	defer cancel()
	script := "timeout " + guestConnectTimeout +
		" bash -c 'exec 3<>/dev/tcp/" + ip + "/443 && echo connected'"
	out, err := m.Exec(ctx, id, NewExecSpec("sh", "-c", script).AsUser(strconv.Itoa(int(agentuid.AgentUID))))
	if err != nil {
		t.Fatalf("in-guest connect probe to %s errored (harness fault, not a firewall verdict): %v", ip, err)
	}
	reached := out.ExitCode == 0 && strings.Contains(out.Stdout, "connected")
	t.Logf("connect %s:443 -> reached=%v (exit=%d stdout=%q stderr=%q)", ip, reached, out.ExitCode, out.Stdout, out.Stderr)
	return reached
}

// TestInGuestEgressAllowlistAndDeny is W3(1): an allowlisted raw IPv4 connects
// from inside the guest and a non-allowlisted raw IPv4 is blocked — the live
// firewall proof at the runtime API, armed in-guest by guestd. This is the
// microVM analog of the podman lifecycle firewall proof (lifecycle_test.go:
// 137-166), run inside the guest netns instead of the container netns.
func TestInGuestEgressAllowlistAndDeny(t *testing.T) {
	m, id := startEgressSession(t, MustAllowEgress(allowedIP), "w3-allowdeny")

	if !canReachIPv4(t, m, id, allowedIP) {
		t.Errorf("allowlisted host %s must be reachable through the in-guest firewall", allowedIP)
	}
	if canReachIPv4(t, m, id, deniedIP) {
		t.Errorf("non-allowlisted host %s must be blocked by the default-deny in-guest firewall", deniedIP)
	}
}

// TestInGuestEgressAgentCannotAlterRuleset is W3(3) (and the V8 row-(8) re-arm
// half): after the arm, an agent-uid exec cannot tear down or alter the ruleset
// — `nft flush ruleset` fails because a non-root exec runs with an empty
// capability set (guestd builds the child credential via linuxCredential(uid),
// supervisor.go:59-61, dropping caps for a non-zero uid), and guestd never runs
// an exec as root (resolveUID refuses uid 0). The allow/deny behavior still
// holds afterward. A regression that ran the agent privileged, or armed with a
// flushable ruleset, fails here.
func TestInGuestEgressAgentCannotAlterRuleset(t *testing.T) {
	m, id := startEgressSession(t, MustAllowEgress(allowedIP), "w3-integrity")

	// Run `nft flush ruleset` BARE (no `; echo rc=$?` wrapper): the exec's
	// ExitCode is then nft's own exit, so a refusal surfaces directly as a
	// non-zero ExitCode. A trailing `echo` would make the shell exit 0 and mask
	// the refusal. A non-zero exit here is a SUCCESSFUL exec call carrying a
	// failed command (guest exec model), never a transport error.
	ctx, cancel := context.WithTimeout(t.Context(), egressProbeTimeout)
	defer cancel()
	flush, err := m.Exec(ctx, id, NewExecSpec("nft", "flush", "ruleset").AsUser(strconv.Itoa(int(agentuid.AgentUID))))
	if err != nil {
		t.Fatalf("nft flush probe errored (harness fault, not a firewall verdict): %v", err)
	}
	if flush.ExitCode == 0 {
		t.Fatalf("agent uid must NOT be able to flush the in-guest firewall; got exit=0 stdout=%q stderr=%q",
			flush.Stdout, flush.Stderr)
	}
	t.Logf("nft flush refused for agent uid: exit=%d stderr=%q", flush.ExitCode, flush.Stderr)

	// The firewall must still hold after the refused flush attempt.
	if !canReachIPv4(t, m, id, allowedIP) {
		t.Errorf("allowlisted host %s must stay reachable after a refused flush", allowedIP)
	}
	if canReachIPv4(t, m, id, deniedIP) {
		t.Errorf("non-allowlisted host %s must stay blocked after a refused flush", deniedIP)
	}
}

// TestInGuestEgressAlwaysArmedDefaultDeny is W3(4) / the §(e)/OQ-3 always-arm
// verification: a session created with the ZERO-VALUE egress policy still boots
// armed default-deny, so external egress is blocked even though no allowlist was
// set. This is the load-bearing always-arm claim — every ContainerSpec-created
// microVM session is firewalled at Start whether or not a caller set Egress —
// proven live. A regression that skipped the arm on an empty policy (a silent
// open-egress VM) fails here: the deniedIP would become reachable.
func TestInGuestEgressAlwaysArmedDefaultDeny(t *testing.T) {
	// Zero-value EgressPolicy: no allowlist, pure default-deny. The positive
	// control for this test is its sibling TestInGuestEgressAllowlistAndDeny
	// (where allowedIP DOES connect on an armed guest): together they prove the
	// block here is the firewall, not dead guest networking. This test alone is
	// not self-validating (both-blocked would also pass if all egress were down).
	m, id := startEgressSession(t, EgressPolicy{}, "w3-defaultdeny")

	if canReachIPv4(t, m, id, allowedIP) {
		t.Errorf("default-deny session must block %s: an always-armed empty policy allows no external egress", allowedIP)
	}
	if canReachIPv4(t, m, id, deniedIP) {
		t.Errorf("default-deny session must block %s: an always-armed empty policy allows no external egress", deniedIP)
	}
}
