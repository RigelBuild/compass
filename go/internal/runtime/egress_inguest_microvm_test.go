//go:build microvm && unix

package runtime

// KVM-gated tests exercise guest egress through the real runtime API and agent-uid execs.
// Hermetic tests separately prove script delivery, netfilter setup, and fail-closed behavior.

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
)

const (
	// egressProbeTimeout bounds the host-side reachability exec.
	egressProbeTimeout = 25 * time.Second
	// guestConnectTimeout bounds the guest probe; it must fire before the host timeout.
	guestConnectTimeout = "10"
	// Raw IPs avoid DNS stalls; allowlisted addresses connect and denied addresses must block.
	allowedIP = "1.1.1.1"
	deniedIP  = "8.8.8.8"
	// guestGatewayIP is the gateway passt advertises (microvm launch's -g).
	guestGatewayIP = "10.0.2.2"
)

// startEgressSession boots a microVM with the policy and registers teardown.
// Start arms the in-guest firewall before returning.
func startEgressSession(t *testing.T, egress EgressPolicy, name string) (*MicroVMRuntime, WorkloadID) {
	t.Helper()
	env := microvmtest.Require(t)
	m := NewMicroVMRuntime(e2eConfig(t, env))

	workspace := t.TempDir()
	id, err := m.Create(t.Context(), WorkloadSpec{
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

	// Start's context owns the VM lifetime; a helper-local timeout cancellation would kill it.
	// Boot and test duration are already bounded by bootDeadline and the test timeout.
	if startErr := m.Start(t.Context(), id); startErr != nil {
		t.Fatalf("Start (in-guest egress arm must succeed): %v", startErr)
	}
	return m, id
}

// canReachIPv4 probes ip:443 as the agent user; guest timeout 124 means blocked.
// Transport errors fail the test because they are harness faults, not firewall verdicts.
func canReachIPv4(t *testing.T, m *MicroVMRuntime, id WorkloadID, ip string) bool {
	t.Helper()
	return canReachTCP(t, m, id, ip, "443")
}

// canReachTCP is canReachIPv4 for an arbitrary port.
func canReachTCP(t *testing.T, m *MicroVMRuntime, id WorkloadID, ip, port string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), egressProbeTimeout)
	defer cancel()
	script := "timeout " + guestConnectTimeout +
		" bash -c 'exec 3<>/dev/tcp/" + ip + "/" + port + " && echo connected'"
	out, err := m.Exec(ctx, id, NewExecSpec("sh", "-c", script).AsUser(strconv.Itoa(int(agentuid.AgentUID))))
	if err != nil {
		t.Fatalf("in-guest connect probe to %s errored (harness fault, not a firewall verdict): %v", ip, err)
	}
	reached := out.ExitCode == 0 && strings.Contains(out.Stdout, "connected")
	t.Logf("connect %s:%s -> reached=%v (exit=%d stdout=%q stderr=%q)", ip, port, reached, out.ExitCode, out.Stdout, out.Stderr)
	return reached
}

// TestInGuestEgressAllowlistAndDeny checks allowed and blocked IPv4 connections in the guest.
func TestInGuestEgressAllowlistAndDeny(t *testing.T) {
	m, id := startEgressSession(t, MustAllowEgress(allowedIP), "w3-allowdeny")

	if !canReachIPv4(t, m, id, allowedIP) {
		t.Errorf("allowlisted host %s must be reachable through the in-guest firewall", allowedIP)
	}
	if canReachIPv4(t, m, id, deniedIP) {
		t.Errorf("non-allowlisted host %s must be blocked by the default-deny in-guest firewall", deniedIP)
	}
}

// TestInGuestEgressAgentCannotAlterRuleset proves agent-uid exec cannot flush nft rules.
// It also confirms the firewall still allows and blocks the expected destinations afterward.
func TestInGuestEgressAgentCannotAlterRuleset(t *testing.T) {
	m, id := startEgressSession(t, MustAllowEgress(allowedIP), "w3-integrity")

	// Run nft directly so its own non-zero status is observed; a trailing echo would mask refusal.
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

// TestInGuestEgressAlwaysArmedDefaultDeny proves an empty policy still blocks external egress.
// A skipped arm would let deniedIP connect.
func TestInGuestEgressAlwaysArmedDefaultDeny(t *testing.T) {
	// The allowlist sibling is the positive control: it proves this block is the firewall,
	// not dead guest networking.
	m, id := startEgressSession(t, EgressPolicy{}, "w3-defaultdeny")

	if canReachIPv4(t, m, id, allowedIP) {
		t.Errorf("default-deny session must block %s: an always-armed empty policy allows no external egress", allowedIP)
	}
	if canReachIPv4(t, m, id, deniedIP) {
		t.Errorf("default-deny session must block %s: an always-armed empty policy allows no external egress", deniedIP)
	}
}

// TestInGuestEgressGatewayDoesNotReachHost pins passt's --no-map-gw: with the
// gateway allowlisted, a guest dial to it must not land on a host listener.
func TestInGuestEgressGatewayDoesNotReachHost(t *testing.T) {
	microvmtest.Require(t) // skip a KVM-less (or non-Linux) host before reading /proc
	// passt implies --no-map-gw when the host has no default-route gateway, so
	// there the test could not fail without the flag: skip as a declared gap.
	if !hostHasDefaultGateway(t) {
		t.Skip("host has no IPv4 default-route gateway: passt would not map the gateway even without --no-map-gw, so this test cannot discriminate here")
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		if c, acceptErr := ln.Accept(); acceptErr == nil {
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	m, id := startEgressSession(t, MustAllowEgress(guestGatewayIP, allowedIP), "v8-no-map-gw")
	// Positive control: the same armed session does reach an allowlisted host.
	if !canReachIPv4(t, m, id, allowedIP) {
		t.Fatalf("allowlisted host %s must be reachable, or the gateway verdict below proves nothing", allowedIP)
	}
	if canReachTCP(t, m, id, guestGatewayIP, port) {
		t.Errorf("guest reached the host listener through gateway %s:%s: passt maps the gateway to the host", guestGatewayIP, port)
	}
	select {
	case <-accepted:
		t.Errorf("host loopback listener accepted a connection from the guest")
	default:
	}
}

// hostHasDefaultGateway reports whether the IPv4 default route the kernel
// selects (lowest metric) has a gateway, the case where passt maps 10.0.2.2.
func hostHasDefaultGateway(t *testing.T) bool {
	t.Helper()
	raw, err := os.ReadFile("/proc/net/route")
	if err != nil {
		t.Fatalf("read /proc/net/route: %v", err)
	}
	found, hasGW, best := false, false, 0
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 7 || f[1] != "00000000" {
			continue
		}
		metric, convErr := strconv.Atoi(f[6])
		if convErr != nil {
			t.Fatalf("parse route metric %q: %v", f[6], convErr)
		}
		if !found || metric < best {
			found, best, hasGW = true, metric, f[2] != "00000000"
		}
	}
	return hasGW
}
