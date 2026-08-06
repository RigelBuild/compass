//go:build unix

package stack

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
)

// Config is the caller-provided description of the embedded stack. Every field
// is resolved by the caller (the CLI slice) — the core applies no defaults, so
// there is exactly one source of truth for each path.
type Config struct {
	// StateDir is the app state directory: the O_EXCL lockfile, the private
	// postgres data dir, and the TLS anchor (tls.crt/tls.key) all live here.
	StateDir string
	// SocketPath is the server Unix socket the stack serves compass.v1 on and
	// probes GetServerInfo over. Caller-provided — default resolution (the
	// XDG_RUNTIME_DIR/HOME fallback) is the CLI slice's job, not the core's.
	SocketPath string
	// ListenAddr is the configured loopback TLS network door (e.g.
	// 127.0.0.1:50052). It must be a fixed port, never ":0": the server exposes
	// no bound-address discovery API, so an ephemeral port could not be handed
	// to the runner as its --server target.
	ListenAddr string
	// DatabaseDSN is the postgres DSN compass-server opens the store of record
	// on.
	DatabaseDSN string
	// AgentImage is the container image ref every agent workstream runs; the
	// runner refuses to boot without it present in the local store.
	AgentImage string
	// RuntimeDir is the runner-owned base dir for per-container agent sockets
	// (RuntimeDir/containers/compass-agent-<32hex>/agent.sock). Its length is
	// budgeted against the AF_UNIX sun_path cap by Validate.
	RuntimeDir string
	// Linger is the caller's Down policy: whether the CLI leaves the stack
	// running after the driving process exits. The Stack methods themselves
	// always stop when Down is invoked; this only records the caller's intent.
	Linger bool
	// AgentModel is the model selector every agent this stack's runner starts
	// is pinned to (the agent's COMPASS_MODEL). Empty leaves each agent on its
	// own default — the core applies no default, the caller (CLI slice) resolves
	// $COMPASS_AGENT_MODEL if it wants one.
	AgentModel string
	// EgressAllow is the default-deny egress allowlist (DNS names / IP literals)
	// handed to every agent container. Empty (nil) is a valid default-deny
	// policy — no host reachable. The core applies no default.
	EgressAllow []string
	// CheckoutDir is the in-container working directory the runner checks the
	// agent's workspace out into (the agent session's cwd). Empty leaves the
	// runner on its own default (/workspace) — the core applies no default. The
	// real agent image ships /workspace non-writable (only $HOME is agent-owned),
	// so a caller driving Provision against that image must set this to a path
	// under $HOME (e.g. /home/agent/repo); the embedded supervisor and the
	// compass-stack CLI leave it unset and keep the runner's default.
	CheckoutDir string
}

// sunPathMax is the longest NUL-terminated path an AF_UNIX address holds on this
// platform, derived from the kernel's own sockaddr_un (108 bytes on Linux, 104
// on the BSDs) minus the terminator — mirroring runner.run.go so the two budgets
// cannot drift onto different literals.
const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1

// agentSocketTailWidth is the fixed suffix the runner appends to RuntimeDir to
// form the widest per-container agent socket path:
// /containers/compass-agent-<32-hex account id>/agent.sock. It is 69 bytes
// (devenv.nix:255-263), so on Linux (sunPathMax 107) a RuntimeDir over 38 bytes
// overflows the cap. Built with the same filepath.Join the runner uses rather
// than hand-summed, so it tracks the real path construction.
var agentSocketTailWidth = len(filepath.Join(
	"containers", "compass-agent-"+strings.Repeat("0", 32), "agent.sock")) + 1 // +1 for the separator joining RuntimeDir to the tail

// Validate enforces the config invariants that would otherwise surface as opaque
// runtime failures far from the misconfiguration: an unbindable network door and
// an over-budget runner socket path.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("stack config: ListenAddr is required (a fixed loopback TLS door, e.g. 127.0.0.1:50052)")
	}
	// ":0" (and any ":0" host variant) requests an ephemeral port. The server
	// has no bound-address discovery API, so the runner could never be told the
	// real port — reject it up front rather than spawn an unreachable door.
	if _, port, ok := splitPort(c.ListenAddr); ok && port == "0" {
		return fmt.Errorf("stack config: ListenAddr %q must be a fixed port, not :0 (no bound-address discovery API exists)", c.ListenAddr)
	}
	// The runner builds agent sockets at RuntimeDir/containers/
	// compass-agent-<32hex>/agent.sock; the fixed tail is agentSocketTailWidth
	// bytes, so RuntimeDir may not exceed sunPathMax-tail. Name the budget so an
	// operator knows exactly how far to shorten it.
	if budget := sunPathMax - agentSocketTailWidth; len(c.RuntimeDir) > budget {
		return fmt.Errorf(
			"stack config: RuntimeDir %q (%d bytes) is too long: the per-container agent socket tail adds %d bytes, over this platform's AF_UNIX sun_path limit of %d; RuntimeDir must be at most %d bytes (shorten it by at least %d)",
			c.RuntimeDir, len(c.RuntimeDir), agentSocketTailWidth, sunPathMax, budget, len(c.RuntimeDir)-budget)
	}
	return nil
}

// splitPort extracts the port from a host:port authority without importing net's
// resolution machinery. It returns ok=false when there is no ":port" tail.
func splitPort(addr string) (host, port string, ok bool) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}
