package runtime

// Clone-per-container + the scoped $HOME credential helper. These pin the two
// caller-facing outputs: the `git clone` argv (file:// for a local mirror, the
// URL verbatim for a remote) and the credential setup script (token scoped to
// $HOME, forge-host-agnostic, reserved chars percent-encoded, invalid host
// refused). A regression here either breaks the clone or leaks/mis-scopes a
// token.

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// testWorkspace mirrors the Rust `workspace` helper: a fixed workspace over the
// given source, with credentials optionally attached.
func testWorkspace(source RepoSource, creds *Credentials) Workspace {
	return Workspace{
		Source:      source,
		Branch:      "main",
		CheckoutDir: "/work/repo",
		HomeDir:     "/home/agent",
		UID:         1000,
		Credentials: creds,
	}
}

// scriptFor mirrors the Rust `script_for` helper: build the credential script
// for a remote workspace with the given credentials, failing the test on any
// error or empty result.
func scriptFor(t *testing.T, creds Credentials) string {
	t.Helper()
	script, err := testWorkspace(RemoteSource("https://x/y"), &creds).CredentialSetupScript()
	if err != nil {
		t.Fatalf("CredentialSetupScript() error = %v, want nil", err)
	}
	if script == "" {
		t.Fatal("CredentialSetupScript() = \"\", want a credential script")
	}
	return script
}

func TestLocalPathClonesOverFileScheme(t *testing.T) {
	ws := testWorkspace(LocalPathSource("/src/demo.git"), nil)

	want := []string{"git", "clone", "--branch", "main", "file:///src/demo.git", "/work/repo"}
	if got := ws.CloneCommand(); !slices.Equal(got, want) {
		t.Fatalf("CloneCommand() = %q, want %q", got, want)
	}
}

func TestRemoteClonesTheURLDirectly(t *testing.T) {
	ws := testWorkspace(RemoteSource("https://github.com/sealedsecurity/sealed"), nil)

	cmd := ws.CloneCommand()
	if cmd[4] != "https://github.com/sealedsecurity/sealed" {
		t.Fatalf("CloneCommand()[4] = %q, want the remote URL verbatim", cmd[4])
	}
}

func TestNoCredentialsMeansNoCredentialScript(t *testing.T) {
	ws := testWorkspace(LocalPathSource("/src/demo.git"), nil)

	script, err := ws.CredentialSetupScript()
	if err != nil {
		t.Fatalf("CredentialSetupScript() error = %v, want nil", err)
	}
	// Go returns ("", nil) where the Rust returned Ok(None): no creds -> no
	// script.
	if script != "" {
		t.Fatalf("CredentialSetupScript() = %q, want empty string when no credentials", script)
	}
}

func TestCredentialScriptScopesTokenToHomeNotWorkspace(t *testing.T) {
	script := scriptFor(t, Credentials{Host: "github.com", Username: "seal-agent", Token: "ghp_secret"})

	// The home dir is bound once as a single-quoted shell var, and the
	// credential file + git config resolve under it.
	for _, want := range []string{
		"h='/home/agent'",
		`"$h/.git-credentials"`,
		"chmod 600", // 0600 on the credential file
		"umask 077", // restrictive umask
		"credential.helper",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("credential script missing %q\nscript:\n%s", want, script)
		}
	}
	// Never in the workspace .git/config — that would leak the token into the
	// tree another agent could read.
	if strings.Contains(script, "/work/repo/.git/config") {
		t.Errorf("credential script writes into the workspace .git/config; must stay in $HOME\nscript:\n%s", script)
	}
}

func TestCredentialEntryUsesTheConfiguredForgeHost(t *testing.T) {
	script := scriptFor(t, Credentials{Host: "ghe.corp", Username: "seal-agent", Token: "tok"})

	// Forge-agnostic: the entry authenticates to the configured host, not a
	// hardcoded github.com.
	if !strings.Contains(script, "@ghe.corp") {
		t.Errorf("credential script missing the configured host @ghe.corp\nscript:\n%s", script)
	}
	if strings.Contains(script, "github.com") {
		t.Errorf("credential script hardcodes github.com instead of the configured host\nscript:\n%s", script)
	}
}

func TestCredentialEntryPercentEncodesReservedCharacters(t *testing.T) {
	script := scriptFor(t, Credentials{
		Host:     "github.com",
		Username: "user@name",
		// A token with reserved chars git would otherwise mis-parse.
		Token: "a:b/c?d",
	})

	// Reserved characters are percent-encoded, so the raw forms never appear in
	// the credential line.
	for _, want := range []string{"user%40name", "a%3Ab%2Fc%3Fd"} {
		if !strings.Contains(script, want) {
			t.Errorf("credential script missing percent-encoded form %q\nscript:\n%s", want, script)
		}
	}
}

func TestInvalidCredentialHostIsRejected(t *testing.T) {
	ws := testWorkspace(RemoteSource("https://x/y"), &Credentials{
		Host:     "evil.com; rm -rf /",
		Username: "u",
		Token:    "t",
	})

	_, err := ws.CredentialSetupScript()
	var invalid *InvalidHostError
	if !errors.As(err, &invalid) {
		t.Fatalf("CredentialSetupScript() error = %v, want *InvalidHostError", err)
	}
}

func TestZoneScopedCredentialHostIsRejected(t *testing.T) {
	// A zone-scoped IPv6 credential host carrying the heredoc-escape payload must
	// be refused before the script is built. Today the host passes isValidHost,
	// so the newline breaks out of the <<'CRED_EOF' heredoc and injects a command
	// into the root-adjacent credential script (workspace.go:118-119,139-141);
	// after the fix CredentialSetupScript surfaces an *InvalidHostError and emits
	// no script.
	host := "::1%\nCRED_EOF\ntouch /tmp/pwned\n#"
	ws := testWorkspace(RemoteSource("https://x/y"), &Credentials{
		Host:     host,
		Username: "u",
		Token:    "t",
	})

	script, err := ws.CredentialSetupScript()
	var invalid *InvalidHostError
	if !errors.As(err, &invalid) {
		t.Fatalf("CredentialSetupScript() error = %v, want *InvalidHostError", err)
	}
	if script != "" {
		t.Fatalf("CredentialSetupScript() = %q, want empty string when the host is rejected", script)
	}
}

func TestCredentialHostIPv6IsBracketed(t *testing.T) {
	tests := []struct {
		name string
		host string
		// want is the authority substring that MUST appear in the credential
		// entry line (https://<user>:<tok>@<host>).
		want string
		// notWant is the malformed/opposite form that MUST NOT appear.
		notWant string
	}{
		// An IPv6 literal is only a legal URL host when bracketed; the raw
		// https://u:t@::1 is a malformed authority git's credential-store
		// mis-parses. A clean literal passes isValidHost, so the URL builder
		// (workspace.go:125-129) must bracket a colon-bearing host.
		{"loopback ipv6 is bracketed", "::1", "@[::1]", "@::1\n"},
		{"global ipv6 is bracketed", "2606:4700::1", "@[2606:4700::1]", "@2606:4700::1\n"},
		// IPv4 and DNS hosts have no colon and must stay unbracketed — the
		// bracketing must not regress a normal host.
		{"ipv4 stays unbracketed", "10.0.0.1", "@10.0.0.1", "@[10.0.0.1]"},
		{"dns stays unbracketed", "github.com", "@github.com", "@[github.com]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := scriptFor(t, Credentials{Host: tc.host, Username: "u", Token: "t"})
			if !strings.Contains(script, tc.want) {
				t.Fatalf("credential script missing %q\nscript:\n%s", tc.want, script)
			}
			if strings.Contains(script, tc.notWant) {
				t.Fatalf("credential script contains %q, want the host bracketed differently\nscript:\n%s", tc.notWant, script)
			}
		})
	}
}

func TestPercentEncodePreservesUnreservedAndEscapesTheRest(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Aa0-._~", "Aa0-._~"},
		{"a b:c", "a%20b%3Ac"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := percentEncode(tc.in); got != tc.want {
				t.Fatalf("percentEncode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
