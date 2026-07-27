package runtime

// The default-deny + allowlist egress firewall. These pin the generated nft
// script's structural contract (fail-closed base ruleset, DNS scoped to the
// container's own resolver, dual-stack resolution per host) and the allowlist's
// validation/dedup — a regression here either opens an exfiltration path or
// silently stalls a permitted host.

import (
	"errors"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestDefaultPolicyDeniesEverythingButCarveouts(t *testing.T) {
	script := EgressPolicy{}.NftScript()

	// The base ruleset is always present: fail-closed default-drop plus the
	// fixed loopback / established carve-outs.
	for _, want := range []string{"set -eu", "policy drop", "oif lo accept"} {
		if !strings.Contains(script, want) {
			t.Errorf("default nft script missing %q\nscript:\n%s", want, script)
		}
	}
	// No host allowlisted -> no element-add lines.
	if strings.Contains(script, "add element") {
		t.Errorf("default nft script has an add-element line but no host is allowlisted\nscript:\n%s", script)
	}
}

func TestDNSIsScopedToTheContainersResolverNotAnyHost(t *testing.T) {
	script := EgressPolicy{}.NftScript()

	// DNS is allowed only to resolvers parsed from /etc/resolv.conf, so there is
	// no blanket "any host on port 53" rule to tunnel through.
	for _, want := range []string{"/etc/resolv.conf", "nameserver"} {
		if !strings.Contains(script, want) {
			t.Errorf("nft script missing resolver-scoping token %q\nscript:\n%s", want, script)
		}
	}
	for _, forbidden := range []string{"output udp dport 53 accept", "output tcp dport 53 accept"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("nft script has a blanket DNS rule %q — DNS must be scoped to the resolver\nscript:\n%s", forbidden, script)
		}
	}
}

func TestAllowlistedHostPopulatesBothFamilies(t *testing.T) {
	script := MustAllowEgress("github.com").NftScript()

	// Dual-stack: both A and AAAA resolution must be wired, or the container
	// reaches a blocked host over the un-allowlisted family.
	for _, want := range []string{
		"getent ahostsv4 github.com",
		"getent ahostsv6 github.com",
		"allow4",
		"allow6",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("allowlisted-host nft script missing %q\nscript:\n%s", want, script)
		}
	}
}

func TestHostsAreDeduplicatedAndOrdered(t *testing.T) {
	policy := MustAllowEgress("b.example", "a.example", "b.example")

	if got := policy.Hosts(); !slices.Equal(got, []string{"a.example", "b.example"}) {
		t.Fatalf("Hosts() = %q, want [a.example b.example] (deduped + sorted)", got)
	}
}

func TestEveryAllowlistedHostIsResolved(t *testing.T) {
	script := MustAllowEgress("github.com", "api.anthropic.com").NftScript()

	for _, host := range []string{"github.com", "api.anthropic.com"} {
		for _, family := range []string{"ahostsv4", "ahostsv6"} {
			want := "getent " + family + " " + host
			if !strings.Contains(script, want) {
				t.Errorf("nft script missing resolution %q\nscript:\n%s", want, script)
			}
		}
	}
}

func TestShellUnsafeHostsAreRejected(t *testing.T) {
	bad := []string{
		"github.com; rm -rf /",
		"$(id)",
		"a b",
		"host`whoami`",
		"x|y",
	}
	for _, host := range bad {
		t.Run(host, func(t *testing.T) {
			_, err := AllowEgress(host)
			var invalid *InvalidHostError
			if !errors.As(err, &invalid) {
				t.Fatalf("AllowEgress(%q) error = %v, want *InvalidHostError", host, err)
			}
			if invalid.Host != host {
				t.Fatalf("InvalidHostError.Host = %q, want %q", invalid.Host, host)
			}
		})
	}
}

func TestValidDNSAndIPLiteralsAreAccepted(t *testing.T) {
	good := []string{
		"github.com",
		"api.anthropic.com",
		"10.0.0.1",
		"2606:4700::1",
		"host_1",
	}
	for _, host := range good {
		t.Run(host, func(t *testing.T) {
			if _, err := AllowEgress(host); err != nil {
				t.Fatalf("AllowEgress(%q) error = %v, want nil", host, err)
			}
		})
	}
}

func TestZoneScopedHostIsRejectedAtTheEgressSink(t *testing.T) {
	// Defense-in-depth: the isValidHost fix must close this real sink, not just
	// the predicate. A zone-scoped IPv6 host carrying a command substitution
	// must never reach the root nft script — AllowEgress gates on isValidHost
	// (egress.go:43-45), so a zone'd host must surface an *InvalidHostError.
	bad := []string{
		"fe80::1%$(id)",
		"fe80::1%eth0",
		"::1%a;rm -rf /",
	}
	for _, host := range bad {
		t.Run(host, func(t *testing.T) {
			_, err := AllowEgress(host)
			var invalid *InvalidHostError
			if !errors.As(err, &invalid) {
				t.Fatalf("AllowEgress(%q) error = %v, want *InvalidHostError", host, err)
			}
			if invalid.Host != host {
				t.Fatalf("InvalidHostError.Host = %q, want %q", invalid.Host, host)
			}
		})
	}
}

func TestEgressSubshellToleratesFailingAddElement(t *testing.T) {
	// The per-address loops run in a `set +e` subshell terminated with `; true`.
	// `set +e` keeps a failing `nft add element` (a host may lack an address
	// family, or two allowlisted hosts may share a CDN address whose second
	// insert nft's interval sets reject with "File exists") from aborting the
	// loop; the trailing `; true` makes the subshell's OWN exit status 0 so that
	// tolerated failure can't pierce the fail-closed base ruleset's `set -eu` and
	// abort the whole arm-egress script — which would tear down a legit
	// container. This drives that exact vector at the shell level.
	script := MustAllowEgress("github.com", "example.com").NftScript()

	subshellRe := regexp.MustCompile(`(?m)^\(set \+e;.*\)$`)
	subshells := subshellRe.FindAllString(script, -1)
	if len(subshells) == 0 {
		t.Fatalf("NftScript emitted no per-address subshells to exercise\nscript:\n%s", script)
	}

	// Rewrite the real subshell for hermetic execution: a fixed iteration list
	// (no resolver) and a body that always fails (no nft), preserving the
	// subshell SHAPE — crucially the trailing `; true)` that is the fix.
	getentRe := regexp.MustCompile(`\$\(getent[^)]*\)`)
	nftAddRe := regexp.MustCompile(`nft add element[^;]*`)

	for _, sub := range subshells {
		// Structural guard: the emitted subshell must end with the tolerance
		// tail, or a failing add leaks its non-zero status to the base ruleset.
		if !strings.HasSuffix(sub, "; done; true)") {
			t.Errorf("per-address subshell does not end with `; done; true)`, so a failing add-element pierces the base rules:\n%s", sub)
		}

		// Behavioral: force the loop body to fail on every iteration and embed
		// the subshell under the same fail-closed `set -eu` base the container
		// entrypoint runs, then assert the outer script still reaches its end.
		forced := getentRe.ReplaceAllString(sub, "a b c")
		forced = nftAddRe.ReplaceAllString(forced, "false")
		outer := "set -eu\n" + forced + "\necho REACHED_END"

		out, err := exec.Command("sh", "-c", outer).CombinedOutput()
		if err != nil {
			t.Fatalf("outer `set -eu` script aborted on a tolerated add failure (err=%v)\nsubshell under test:\n%s\nrewritten:\n%s\noutput:\n%s", err, sub, forced, out)
		}
		if !strings.Contains(string(out), "REACHED_END") {
			t.Fatalf("outer script did not reach its end past the failing subshell\nrewritten:\n%s\noutput:\n%s", forced, out)
		}
	}
}
