// Default-deny + allowlist egress firewall for an agent container (design:
// architecture-lineage). The container's own network namespace is firewalled with
// nftables, so a compromised agent can't exfiltrate to an arbitrary host — the
// structural floor beneath the hook-level output gate.

// Integrity model, verified against rootless podman: the container gets
// NET_ADMIN only so a root entrypoint can arm nft; the agent then runs as a
// non-root user with an empty capability set, so it cannot edit the ruleset.
// Never run the agent as container-root.

// Two nft subtleties: dual-stack (allowlist both A and AAAA or the container
// reaches a blocked host via the other family) and resolve-after-deny (arm the
// ruleset, allowlist DNS, then resolve names from inside, so a compromised
// resolver can't widen the allowlist).

package runtime

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
)

// EgressPolicy is the set of destinations an agent container may reach. An empty
// host set is pure default-deny (only loopback, established flows, and DNS to
// the container's own resolver).
//
// The zero value means no policy was configured at all, which is distinct from
// a configured-but-empty allowlist: empty is the strictest posture, so a tier
// that cannot enforce egress has to tell the two apart to refuse the former.
type EgressPolicy struct {
	hosts      []string
	configured bool
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
	return EgressPolicy{hosts: deduped, configured: true}, nil
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

// Configured reports whether this policy came from a constructor rather than
// being a zero value. Callers that arm a firewall ignore this; it exists for
// tiers that must reject a policy they cannot enforce.
func (e EgressPolicy) Configured() bool {
	return e.configured
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
	if slices.ContainsFunc(e.hosts, isDNSName) {
		// Fail closed if the pin file is not writable: an unpinned name would be
		// re-resolved by the agent and miss the allow sets once its DNS rotates.
		fmt.Fprintf(&b, "\n: >> %s", egressHostsFile)
	}
	for _, host := range e.hosts {
		// Resolve both families before pinning: a pinned v4 line would answer the
		// v6 lookup with ::ffff: addresses instead of AAAA. `set +e` tolerates a
		// failing `nft add element` (missing family, shared address); `; true`
		// keeps that from piercing the base ruleset's `set -e`.
		pin4, pin6 := "", ""
		if isDNSName(host) {
			// Pin the name so the agent dials exactly the addresses allowed here.
			// A failed pin write exits non-zero: an unpinned address can rotate away.
			pin4 = fmt.Sprintf(" echo \"$ip %s\" >> %s || exit 1;", host, egressHostsFile)
			pin6 = fmt.Sprintf(" case \"$ip\" in ::ffff:*) ;; *) echo \"$ip %s\" >> %s || exit 1;; esac;", host, egressHostsFile)
		}
		fmt.Fprintf(&b,
			"\n(set +e; v4=\"$(getent ahostsv4 %[1]s | awk '{print $1}' | sort -u)\"; "+
				"v6=\"$(getent ahostsv6 %[1]s | awk '{print $1}' | sort -u)\"; "+
				"for ip in $v4; do nft add element inet compass_egress allow4 \"{ $ip }\";%[2]s done; "+
				"for ip in $v6; do nft add element inet compass_egress allow6 \"{ $ip }\";%[3]s done; true)",
			host, pin4, pin6)
	}
	b.WriteByte('\n')
	return b.String()
}

// egressHostsFile is where the arm pins each allowlisted name's addresses. The
// agent image's glibc reads it before DNS, so a later lookup cannot return a
// rotated address the allow sets lack.
const egressHostsFile = "/etc/hosts"

// isDNSName reports whether host is a name rather than an IP literal; only a
// name needs pinning, since a literal is dialled as-is.
func isDNSName(host string) bool {
	_, err := netip.ParseAddr(host)
	return err != nil
}

// baseRuleset installs a fail-closed dual-stack firewall.
// DNS is limited to configured resolvers; accept rules precede the output-chain drop.
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
