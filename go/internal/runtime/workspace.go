// Clone-per-container and the agent's scoped $HOME (compass.md §5.3).
//
// Each agent gets its own full git clone, created inside the container — not a
// shared checkout and not a host worktree (a worktree's .git link assumes a
// shared on-disk path that can't survive the container boundary). The clone's
// credentials live in the agent's $HOME/.gitconfig credential helper, never in
// the workspace .git/config, so one agent's token never leaks into another's
// tree and the host's own credentials never enter the container.

package runtime

import (
	"fmt"
	"strings"
)

// RepoSource is where an agent's repo is cloned from, inside the container.
// Exactly one of Remote or LocalPath is set.
//
// Remote is a URL cloned over the network (subject to the egress allowlist).
// LocalPath is a path inside the container — typically a bare mirror
// bind-mounted read-only from a host cache, cloned over file:// for a hermetic,
// network-free clone (node-local, never a network share).
type RepoSource struct {
	Remote    string
	LocalPath string
}

// RemoteSource clones from a network URL.
func RemoteSource(url string) RepoSource { return RepoSource{Remote: url} }

// LocalPathSource clones from a container-local path over file://.
func LocalPathSource(path string) RepoSource { return RepoSource{LocalPath: path} }

// cloneArg is the argument passed to `git clone`: the URL directly for a remote,
// or a file:// URL for a container-local path.
func (r RepoSource) cloneArg() string {
	if r.LocalPath != "" {
		return "file://" + r.LocalPath
	}
	return r.Remote
}

// Credentials are the agent's forge credentials — a dedicated machine user's
// token, never the human's (compass.md §5.3, §9). Written into the agent's
// $HOME, scoped to this container.
type Credentials struct {
	// Host is the forge host these credentials authenticate to (e.g. github.com
	// or a GitHub Enterprise / GitLab host). Must match the authority of a
	// Remote RepoSource URL, so the substrate stays forge-agnostic.
	Host string
	// Username is the forge username the token belongs to (the *-agent machine
	// user).
	Username string
	// Token is the access token. Held in memory only; written to $HOME inside the
	// container at provision time and never baked into an image layer.
	Token string
}

// String redacts the token so it never lands in a log line or a formatted error
// — only the username is shown.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{username: %q, token: <redacted>}", c.Username)
}

// GoString redacts the token under %#v as well, so a struct dump can't leak it.
func (c Credentials) GoString() string { return c.String() }

// Workspace is a per-agent workspace: the clone source, the branch to check out,
// the scoped $HOME, and the unprivileged uid the agent runs as.
type Workspace struct {
	Source RepoSource
	Branch string
	// CheckoutDir is the absolute path inside the container where the repo is
	// cloned.
	CheckoutDir string
	// HomeDir is the agent's $HOME inside the container (holds .gitconfig +
	// creds).
	HomeDir string
	// UID is the unprivileged uid the agent runs as. Never container-root — that
	// would let the agent tear down its own egress firewall (see egress.go).
	UID uint32
	// Credentials is optional forge credentials to install into $HOME.
	Credentials *Credentials
}

// CloneCommand is the `git clone` command an unprivileged exec runs to create
// the clone. HOME is set on the exec so git reads the scoped .gitconfig (and its
// credential helper), not any image-default config.
func (w Workspace) CloneCommand() []string {
	return []string{
		"git", "clone",
		"--branch", w.Branch,
		w.Source.cloneArg(),
		w.CheckoutDir,
	}
}

// CredentialSetupScript is the shell script that installs the scoped credential
// helper into $HOME/.gitconfig. It uses git's `store` helper pointed at a 0600
// $HOME/.git-credentials under the agent's own home — so the token lives in the
// agent's $HOME, never the workspace .git/config, and is unreadable by another
// agent's container.
//
// Meant to be fed to `sh -s` over stdin, never `sh -c <script>`: the token is in
// the script body, and an argv is visible in the container's process list while
// stdin is not.
//
// Returns ("", nil) when no credentials are configured (e.g. a file:// clone of
// a local mirror needs none), or an InvalidHostError when the credential host
// isn't a plain DNS name or IP literal — it is written into this root-adjacent
// script, so it is validated, not escaped.
func (w Workspace) CredentialSetupScript() (string, error) {
	if w.Credentials == nil {
		return "", nil
	}
	creds := w.Credentials
	if !isValidHost(creds.Host) {
		return "", &InvalidHostError{Host: creds.Host}
	}
	home := shellSingleQuote(w.HomeDir)
	// git's own credential-store URL-encodes the user and token when it writes
	// this file, so a token containing reserved characters (:@/?) round-trips.
	// Match that, or such a token mis-parses on read.
	// An IPv6 literal must be bracketed in a URL authority ([::1], not ::1), or
	// the ':' reads as a host:port separator and the URL is malformed. isValidHost
	// already rejected zone-scoped hosts, so any colon-bearing host here is a bare
	// IPv6 literal — safe to bracket unconditionally. IPv4/DNS carry no ':'.
	host := creds.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	entry := fmt.Sprintf("https://%s:%s@%s",
		percentEncode(creds.Username),
		percentEncode(creds.Token),
		host,
	)
	// `h` holds the (single-quoted) home dir once, so a path with a space or
	// metacharacter can't break the commands, and the store helper's --file
	// survives git's later re-shelling of the helper string. The heredoc keeps
	// the token out of any argv even within the script; 0600 on the credential
	// file.
	return fmt.Sprintf("set -eu\n"+
		"umask 077\n"+
		"h=%s\n"+
		"git config --file \"$h/.gitconfig\" credential.helper \"store --file=$h/.git-credentials\"\n"+
		"cat > \"$h/.git-credentials\" <<'CRED_EOF'\n"+
		"%s\n"+
		"CRED_EOF\n"+
		"chmod 600 \"$h/.git-credentials\"\n", home, entry), nil
}

// percentEncode percent-encodes a credential component per RFC 3986, keeping
// only the unreserved set (A–Z a–z 0–9 - . _ ~) literal — matching git's own
// credential-store, so a token with reserved characters survives the write/read
// round-trip through ~/.git-credentials.
func percentEncode(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := range len(value) {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
