package runtime

// These tests pin the fail-closed nft script, resolver-scoped DNS, dual-stack rules,
// and allowlist validation so regressions cannot open egress or stall allowed hosts.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	// Each family's lookup must feed its own set: allow6 alone also appears in
	// the base ruleset, so pin the lookup-to-set pairing.
	for _, want := range []string{
		"v4=\"$(getent ahostsv4 github.com | awk '{print $1}' | sort -u)\"",
		"v6=\"$(getent ahostsv6 github.com | awk '{print $1}' | sort -u)\"",
		"for ip in $v4; do nft add element inet compass_egress allow4",
		"for ip in $v6; do nft add element inet compass_egress allow6",
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

func TestConfiguredDistinguishesAnEmptyAllowlistFromNoPolicy(t *testing.T) {
	// An empty allowlist is the strictest posture, not the absence of a policy;
	// keying on len(Hosts()) would invert exactly the case this separates.
	empty := MustAllowEgress()
	if !empty.Configured() {
		t.Error("MustAllowEgress() with no hosts: Configured() = false, want true (empty is default-deny, not unset)")
	}
	if got := empty.Hosts(); len(got) != 0 {
		t.Errorf("MustAllowEgress().Hosts() = %q, want empty", got)
	}

	var unset EgressPolicy
	if unset.Configured() {
		t.Error("zero-value EgressPolicy: Configured() = true, want false")
	}
}

func TestRejectedHostYieldsAnUnconfiguredPolicy(t *testing.T) {
	policy, err := AllowEgress("github.com; rm -rf /")
	if err == nil {
		t.Fatal("AllowEgress with a shell-unsafe host: err = nil, want rejection")
	}
	if policy.Configured() {
		t.Error("rejected AllowEgress: Configured() = true, want false (the returned policy must not read as enforceable)")
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
			invalid, ok := errors.AsType[*InvalidHostError](err)
			if !ok {
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
			invalid, ok := errors.AsType[*InvalidHostError](err)
			if !ok {
				t.Fatalf("AllowEgress(%q) error = %v, want *InvalidHostError", host, err)
			}
			if invalid.Host != host {
				t.Fatalf("InvalidHostError.Host = %q, want %q", invalid.Host, host)
			}
		})
	}
}

func TestEgressSubshellToleratesFailingAddElement(t *testing.T) {
	// A set +e subshell tolerates duplicate or unavailable nft addresses without aborting the arm.
	// Its trailing true prevents that expected failure from escaping the fail-closed set -eu script.
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
		forced = strings.ReplaceAll(forced, egressHostsFile, filepath.Join(t.TempDir(), "hosts"))
		outer := "set -eu\n" + forced + "\necho REACHED_END"

		out, err := exec.CommandContext(t.Context(), "sh", "-c", outer).CombinedOutput()
		if err != nil {
			t.Fatalf("outer `set -eu` script aborted on a tolerated add failure (err=%v)\nsubshell under test:\n%s\nrewritten:\n%s\noutput:\n%s", err, sub, forced, out)
		}
		if !strings.Contains(string(out), "REACHED_END") {
			t.Fatalf("outer script did not reach its end past the failing subshell\nrewritten:\n%s\noutput:\n%s", forced, out)
		}
	}
}

func TestArmPinsResolvedAddressesInEtcHosts(t *testing.T) {
	// Arm resolves once; an agent that re-resolved later could get a rotated
	// address the firewall drops. Run the real script against fake getent/nft
	// and a temp hosts file, then check every resolved address is pinned.
	script := MustAllowEgress("github.com", "192.0.2.99").NftScript()
	if !strings.Contains(script, egressHostsFile) {
		t.Fatalf("NftScript does not write %s\nscript:\n%s", egressHostsFile, script)
	}

	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolv, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeExe := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExe("nft", "exit 0\n")
	writeExe("getent", `case "$1 $2" in
"ahostsv4 github.com") printf '140.82.112.3    STREAM github.com\n140.82.112.3    DGRAM\n140.82.114.5    STREAM\n' ;;
"ahostsv6 github.com") printf '::ffff:140.82.112.3 STREAM github.com\n2001:db8::5 STREAM\n' ;;
*) exit 2 ;;
esac
`)

	run := strings.ReplaceAll(script, egressHostsFile, hosts)
	run = strings.ReplaceAll(run, "/etc/resolv.conf", resolv)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", run)
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin:" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("arm script failed: %v\n%s", err, out)
	}

	got, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.1 localhost\n", "140.82.112.3 github.com\n", "140.82.114.5 github.com\n", "2001:db8::5 github.com\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("hosts file missing %q\nhosts:\n%s", want, got)
		}
	}
	// A v4-mapped v6 answer duplicates its v4 pin, and an IP literal is dialled
	// as-is; neither gets a hosts entry.
	for _, unwanted := range []string{"::ffff:", "192.0.2.99"} {
		if strings.Contains(string(got), unwanted) {
			t.Errorf("hosts file pins %q\nhosts:\n%s", unwanted, got)
		}
	}
}

func TestArmFailsWhenAPinCannotBeWritten(t *testing.T) {
	// An address allowed in nft but missing from the hosts file reproduces the
	// rotation outage, so a failed pin write must fail the whole arm.
	script := MustAllowEgress("github.com").NftScript()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"nft":    "exit 0\n",
		"getent": "printf '140.82.112.3 STREAM github.com\\n'\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resolv := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolv, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, probe, pin string }{
		// A hosts path inside a missing directory cannot be opened at all.
		{"probe fails", filepath.Join(dir, "missing", "hosts"), filepath.Join(dir, "missing", "hosts")},
		// The probe opens fine but the per-address append fails.
		{"append fails", filepath.Join(dir, "hosts"), filepath.Join(dir, "missing", "hosts")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := strings.Replace(script, egressHostsFile, tc.probe, 1)
			run = strings.ReplaceAll(run, egressHostsFile, tc.pin)
			run = strings.ReplaceAll(run, "/etc/resolv.conf", resolv)
			cmd := exec.CommandContext(t.Context(), "sh", "-c", run)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin:" + os.Getenv("PATH")}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("arm succeeded with an unwritable pin file\noutput:\n%s", out)
			}
		})
	}
}
