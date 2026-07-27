package runtime

// Host validation and shell quoting: the gate that decides which caller-supplied
// strings may be interpolated into a root in-container shell script (nft rules,
// the credential helper). A false accept here is a shell-injection hole in a
// privileged script, so every metacharacter reject and every colon-vs-IP edge is
// pinned.

import "testing"

func TestIsValidHostRejectsShellMetacharacters(t *testing.T) {
	bad := []string{
		"github.com; rm -rf /",
		"$(id)",
		"a b",
		"host`whoami`",
		"x|y",
		"",
	}
	for _, host := range bad {
		t.Run(host, func(t *testing.T) {
			if isValidHost(host) {
				t.Fatalf("isValidHost(%q) = true, want false", host)
			}
		})
	}
}

func TestIsValidHostAcceptsDNSNamesAndIPLiterals(t *testing.T) {
	good := []string{
		"github.com",
		"api.anthropic.com",
		"10.0.0.1",
		"2606:4700::1",
		"host_1",
	}
	for _, host := range good {
		t.Run(host, func(t *testing.T) {
			if !isValidHost(host) {
				t.Fatalf("isValidHost(%q) = false, want true", host)
			}
		})
	}
}

func TestIsValidHostColonStringsMustParseAsIPLiterals(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		// A colon is allowed only in a real IPv6 literal.
		{"2606:4700::1", true},
		{"::1", true},
		// Not IP literals, not DNS names — rejected rather than passed through to
		// fail later in the container.
		{"github.com:443", false},
		{":", false},
		{"::::", false},
		{"a:b:c", false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			if got := isValidHost(tc.host); got != tc.want {
				t.Fatalf("isValidHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestIsValidHostRejectsZoneScopedIPv6(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		// Zone-scoped IPv6 (<ip>%<zone>): netip.ParseAddr keeps the zone text
		// verbatim, so every one of these parses today and is wrongly accepted.
		// The Rust reference parses via std::net::IpAddr, which rejects ALL zones
		// (host.rs:28), so a zone must always be refused — it is an unescaped
		// shell-injection channel into the root nft / credential scripts.
		{"benign zone", "fe80::1%eth0", false},
		{"command-substitution zone", "fe80::1%$(id)", false},
		{"semicolon-chain zone", "::1%a;rm -rf /", false},
		{"backtick zone", "fe80::1%`whoami`", false},
		{"whitespace zone", "fe80::1%a b", false},
		{"heredoc-escape newline zone", "::1%\nCRED_EOF\ntouch /tmp/pwned\n#", false},
		// Clean literals keep parsing — the zone reject must not regress a real
		// IPv6/IPv4 address.
		{"clean ipv6", "2606:4700::1", true},
		{"clean loopback ipv6", "::1", true},
		{"clean ipv4", "10.0.0.1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidHost(tc.host); got != tc.want {
				t.Fatalf("isValidHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestShellSingleQuoteEscapesEmbeddedQuote(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"plain path", "/home/agent", "'/home/agent'"},
		{"embedded quote uses the '\\'' idiom", "a'b", `'a'\''b'`},
		{"space stays inside the quotes", "with space", "'with space'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellSingleQuote(tc.value); got != tc.want {
				t.Fatalf("shellSingleQuote(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
