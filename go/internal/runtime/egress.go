// Default-deny + allowlist egress firewall for an agent container (design:
// architecture-lineage). The container's own network namespace is firewalled with
// nftables, so a compromised agent can't exfiltrate to an arbitrary host — the
// structural floor beneath the hook-level output gate.
//
// The integrity model, verified against rootless podman: the container is
// granted NET_ADMIN only so a root entrypoint can arm nft; the agent then runs
// as a non-root user whose capability set is empty, so it cannot flush or edit
// the ruleset even though the container nominally holds the capability. Never
// run the agent as container-root.
//
// Two nft subtleties the design depends on:
//   - Dual-stack. A host resolves to A and AAAA records; allowlisting one family
//     lets the container prefer the other and reach a blocked host (or stall a
//     permitted one). Both families are resolved and allowlisted.
//   - Resolve after deny. DNS (port 53) is allowlisted, then names are resolved
//     from inside the container to populate the address sets — the ruleset is
//     armed first so resolution itself isn't what a compromised resolver could
//     exploit to widen the allowlist.

package runtime

import (
	"fmt"
	"sort"
	"strings"
)

// EgressPolicy is the set of destinations an agent container may reach. An empty
// host set is pure default-deny (only loopback, established flows, and DNS to
// the container's own resolver).
type EgressPolicy struct {
	hosts []string
}

// AllowEgress builds a policy allowing exactly hosts (deduplicated and
// sorted), each validated as a DNS name or IP literal. A host with shell-unsafe
// characters is rejected — it would otherwise be interpolated into the
// privileged firewall script.
func AllowEgress(hosts ...string) (EgressPolicy, error) {
	set := map[string]struct{}{}
	for _, host := range hosts {
		if !isValidHost(host) {
			return EgressPolicy{}, &InvalidHostError{Host: host}
		}
		set[host] = struct{}{}
	}
	deduped := make([]string, 0, len(set))
	for host := range set {
		deduped = append(deduped, host)
	}
	sort.Strings(deduped)
	return EgressPolicy{hosts: deduped}, nil
}

// MustAllowEgress is a convenience for known-good literals (tests, static config).
// It panics on an invalid host; use AllowEgress for untrusted input.
func MustAllowEgress(hosts ...string) EgressPolicy {
	policy, err := AllowEgress(hosts...)
	if err != nil {
		panic("allowlist hosts must be valid DNS names or IP literals: " + err.Error())
	}
	return policy
}

// Hosts returns the allowlisted hosts in sorted order.
func (e EgressPolicy) Hosts() []string {
	return e.hosts
}

// NftScript is the shell script an entrypoint runs (as root, with NET_ADMIN) to
// arm the firewall: default-drop output with loopback / established carve-outs,
// DNS restricted to the container's own resolver(s), then each allowlisted
// host's A and AAAA records resolved into the allow4 / allow6 sets.
//
// Requires nft, getent, and awk in the image; all ship in the devenv base. `set
// -eu` makes the base ruleset fail closed — if any table / set / chain /
// policy-drop rule fails to install, the script aborts non-zero and the caller
// tears the container down rather than running it unfirewalled. Only the
// per-address `nft add element` loop tolerates failure (a host may lack one
// address family, or a shared address may already be present), and it runs in a
// `set +e` subshell terminated with `; true` so a tolerated add failure neither
// aborts the loop nor pierces the base rules' `set -e`.
//
// Every interpolated host is pre-validated by isValidHost, so none can carry
// shell metacharacters into this root script.
func (e EgressPolicy) NftScript() string {
	var b strings.Builder
	b.WriteString(baseRuleset)
	for _, host := range e.hosts {
		// `getent ahostsv4/v6` is resolver-backed and present in glibc images;
		// each yields one address per line in column one. The `set +e` keeps a
		// failing `nft add element` (a host may lack a family, or two hosts may
		// resolve to a shared address whose second insert nft rejects) from
		// aborting the loop, and the trailing `; true` makes the subshell itself
		// exit 0 so that tolerated failure can't pierce the fail-closed base
		// ruleset's `set -e`.
		fmt.Fprintf(&b,
			"\n(set +e; for ip in $(getent ahostsv4 %s | awk '{print $1}' | sort -u); do "+
				"nft add element inet compass_egress allow4 \"{ $ip }\"; done; true)", host)
		fmt.Fprintf(&b,
			"\n(set +e; for ip in $(getent ahostsv6 %s | awk '{print $1}' | sort -u); do "+
				"nft add element inet compass_egress allow6 \"{ $ip }\"; done; true)", host)
	}
	b.WriteByte('\n')
	return b.String()
}

// baseRuleset is the base nftables ruleset: fail-closed (`set -eu`), a single
// table with dual-stack allow sets, and an output chain that drops by default.
// DNS (port 53) is allowed only to the container's own configured resolvers
// (parsed from /etc/resolv.conf at arm time), not to any host — so a compromised
// agent can't tunnel to an arbitrary port-53 listener. Ordering matters: accept
// rules precede the implicit drop from the chain policy.
const baseRuleset = `set -eu
nft add table inet compass_egress
nft add set inet compass_egress allow4 '{ type ipv4_addr ; flags interval ; }'
nft add set inet compass_egress allow6 '{ type ipv6_addr ; flags interval ; }'
nft add chain inet compass_egress output '{ type filter hook output priority 0 ; policy drop ; }'
nft add rule inet compass_egress output oif lo accept
nft add rule inet compass_egress output ct state established,related accept
for ns in $(awk '/^nameserver/ { print $2 }' /etc/resolv.conf); do \
  case "$ns" in \
    *:*) nft add rule inet compass_egress output ip6 daddr "$ns" udp dport 53 accept; \
         nft add rule inet compass_egress output ip6 daddr "$ns" tcp dport 53 accept;; \
    *)   nft add rule inet compass_egress output ip daddr "$ns" udp dport 53 accept; \
         nft add rule inet compass_egress output ip daddr "$ns" tcp dport 53 accept;; \
  esac; \
done
nft add rule inet compass_egress output ip daddr @allow4 accept
nft add rule inet compass_egress output ip6 daddr @allow6 accept`
