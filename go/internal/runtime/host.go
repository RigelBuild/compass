// Host validation and shell quoting shared by the egress allowlist and the
// credential script. Both interpolate caller-supplied strings into shell
// scripts that run as root inside the container (arming nftables, writing the
// git credential helper), so untrusted values are validated or quoted here
// rather than trusted.

package runtime

import (
	"fmt"
	"net/netip"
	"strings"
)

// InvalidHostError is a rejected host: not a plain DNS name or IP literal. Such
// a value would be interpolated into a privileged in-container shell script, so
// it is refused rather than escaped — a host field is config, not a command
// channel.
type InvalidHostError struct {
	Host string
}

func (e *InvalidHostError) Error() string {
	return fmt.Sprintf("invalid host %q: must be a DNS name or IP literal", e.Host)
}

// isValidHost reports whether host is a plain DNS name or IP literal — the only
// shapes allowed into a root shell script. It rejects anything with shell
// metacharacters, whitespace, or control characters.
//
// A value containing ':' must parse as an IP address (an IPv6 literal), so
// github.com:443, ":", and "::::" are rejected — they are neither DNS names nor
// IP literals and would only fail later inside the container. A colon-free value
// is validated as a DNS name: labels of A–Z a–z 0–9 - _ and dots.
func isValidHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if strings.Contains(host, ":") {
		// Must be a bare IP literal. Reject a zone-scoped address (fe80::1%zone):
		// netip.ParseAddr takes the zone text verbatim, so an attacker-controlled
		// zone smuggles shell metacharacters and newlines into the root egress
		// script and the credential heredoc (a dual sink). Rust's
		// parse::<IpAddr>() rejects every zone; a.Zone() == "" restores that.
		a, err := netip.ParseAddr(host)
		return err == nil && a.Zone() == ""
	}
	for i := range len(host) {
		b := host[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		case b == '-' || b == '_' || b == '.':
		default:
			return false
		}
	}
	return true
}

// shellSingleQuote wraps value in single quotes for safe use in a POSIX shell,
// escaping any embedded single quote via the '\” idiom. Used for $HOME paths
// written into the credential script so a path with a space or metacharacter
// can't break the surrounding commands.
func shellSingleQuote(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('\'')
	for _, ch := range value {
		if ch == '\'' {
			b.WriteString("'\\''")
		} else {
			b.WriteRune(ch)
		}
	}
	b.WriteByte('\'')
	return b.String()
}
