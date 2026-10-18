//go:build unix

package stack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
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
	// ListenAddr is the loopback TLS network door to bind (e.g.
	// 127.0.0.1:50052). Port 0 asks the stack to resolve an ephemeral port
	// while it holds the listener.
	ListenAddr string
	// DatabaseDSN is the postgres DSN compass-server opens the store of record
	// on.
	DatabaseDSN string
	// SecretProvider is the secretspec provider URI forwarded to compass-server.
	// Empty leaves the server's own env/default resolution unchanged.
	SecretProvider string
	// S3 fields configure the server-side transcript archive. Empty endpoint disables archiving.
	S3Endpoint                    string
	S3Bucket                      string
	S3AccessKey                   string
	S3SecretKey                   string
	S3Region                      string
	S3UseTLS                      bool
	TranscriptSafetyValveCapBytes int
	// PostgresImage selects the bundled image; empty keeps the dev wrapper path. Ignored with ExternalDatabase.
	PostgresImage string
	// ExternalDatabase skips bundled postgres and probes DatabaseDSN as provided.
	ExternalDatabase bool
	// CollectorImage selects the bundled collector image. Ignored when ExternalOTLPEndpoint is set.
	CollectorImage string
	// ExternalOTLPEndpoint skips the bundled collector and routes OTLP to the supplied endpoint.
	// Empty enables the collector's default receive-and-drop posture.
	ExternalOTLPEndpoint string
	// These are host loopback publish ports; container ports stay fixed and defaults are DefaultCollector*Port.
	CollectorGRPCPort   int
	CollectorHTTPPort   int
	CollectorHealthPort int
	// NatsImage selects the bundled NATS image. Ignored when ExternalNatsURL is set.
	NatsImage string
	// ExternalNatsURL skips bundled NATS and supplies the endpoint consumers use.
	ExternalNatsURL string
	// These are the published host loopback ports; container ports stay fixed.
	NatsClientPort  int
	NatsMonitorPort int
	// GatewayImage is the bundled LLM gateway image; required unless ExternalGatewayURL is set.
	GatewayImage string
	// ExternalGatewayURL opts out of starting the bundled gateway.
	ExternalGatewayURL string
	// AgentImage is the container image ref every agent runs; the runner requires it in the local store.
	AgentImage string
	// RuntimeDir is the base for per-container agent sockets; Validate budgets its length against AF_UNIX sun_path.
	RuntimeDir string
	// Linger records whether the CLI leaves the stack running after the driver exits; Down stops when called.
	Linger bool
	// AgentModel pins the runner's COMPASS_MODEL; empty leaves model selection to the caller and agent.
	AgentModel string
	// EgressAllow is the default-deny container allowlist; empty permits no host access.
	EgressAllow []string
	// CheckoutDir is the workspace cwd; the image requires a writable path under HOME.
	// Empty keeps the runner's non-writable /workspace default.
	CheckoutDir string
	// Mounts passes extra host bind mounts to the runner; nil adds none.
	Mounts []string
	// RuntimeBackend selects the runner session backend; empty leaves resolution to the runner.
	RuntimeBackend string
	// GuestArtifact is a digest-pinned image reference; it cannot be combined with GuestDir.
	GuestArtifact string
	// GuestDir uses an existing local artifact and skips fetching, including in air-gapped setups.
	GuestDir string
}

// sunPathMax derives the AF_UNIX path budget from sockaddr_un, excluding its NUL terminator.
const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1

// agentSocketTailWidth measures the widest runner socket suffix with the same filepath.Join construction.
var agentSocketTailWidth = len(filepath.Join(
	"containers", "compass-agent-"+strings.Repeat("0", 32), "agent.sock",
)) + 1 // +1 for the separator joining RuntimeDir to the tail

// Validate rejects settings that cause runtime failures or violate socket and guest-image constraints.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("stack config: ListenAddr is required (a loopback TLS door, e.g. 127.0.0.1:50052)")
	}
	// Both guest knobs identify the artifact through different paths; accepting both leaves precedence ambiguous.
	if c.GuestArtifact != "" && c.GuestDir != "" {
		return fmt.Errorf("stack config: GuestArtifact %q and GuestDir %q are mutually exclusive", c.GuestArtifact, c.GuestDir)
	}
	if !c.microVM() && (c.GuestArtifact != "" || c.GuestDir != "") {
		return fmt.Errorf("stack config: GuestArtifact/GuestDir require RuntimeBackend %q, got %q", runtimeBackendMicroVM, c.RuntimeBackend)
	}
	// Use the fetcher's parser so an accepted reference cannot fail later during startup.
	if c.GuestArtifact != "" {
		if _, err := parseGuestRef(c.GuestArtifact); err != nil {
			return fmt.Errorf("stack config: %w", err)
		}
	}
	if c.GuestDir != "" && !filepath.IsAbs(c.GuestDir) {
		return fmt.Errorf("stack config: GuestDir %q must be an absolute path", c.GuestDir)
	}
	if budget := sunPathMax - agentSocketTailWidth; len(c.RuntimeDir) > budget {
		return fmt.Errorf(
			"stack config: RuntimeDir %q (%d bytes) is too long: the per-container agent socket tail adds %d bytes, over this platform's AF_UNIX sun_path limit of %d; RuntimeDir must be at most %d bytes (shorten it by at least %d)",
			c.RuntimeDir, len(c.RuntimeDir), agentSocketTailWidth, sunPathMax, budget, len(c.RuntimeDir)-budget,
		)
	}
	return nil
}

// splitPort extracts a host:port tail without importing net's resolution machinery.
func splitPort(addr string) (host, port string, ok bool) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// loopbackEndpoint refuses unset or out-of-range ports before podman sees a publish to port zero.
func loopbackEndpoint(field string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("stack config: %s %d must be a host port in 1-65535", field, port)
	}
	return "127.0.0.1:" + strconv.Itoa(port), nil
}

// checkBundledPortsDistinct rejects conflicting host binds before any child starts; attach and down use no ports.
func (c Config) checkBundledPortsDistinct(ctx context.Context) error {
	type port struct {
		field string
		port  int
	}
	var ports []port
	add := func(field, addr string) {
		// LookupPort also maps a service name such as "http", which net.Listen accepts.
		if _, p, ok := splitPort(addr); ok {
			if n, err := net.DefaultResolver.LookupPort(ctx, "tcp", p); err == nil {
				ports = append(ports, port{field, n})
			}
		}
	}
	// Bundled ports publish on 127.0.0.1; ListenAddr conflicts only if its host covers loopback.
	if host, _, ok := splitPort(c.ListenAddr); ok && overlapsLoopbackPublish(host) {
		add("ListenAddr", c.ListenAddr)
	}
	if c.ExternalGatewayURL == "" {
		add("the bundled gateway", gatewayHostEndpoint)
	}
	if c.ExternalOTLPEndpoint == "" {
		ports = append(ports, port{"CollectorGRPCPort", c.CollectorGRPCPort},
			port{"CollectorHTTPPort", c.CollectorHTTPPort}, port{"CollectorHealthPort", c.CollectorHealthPort})
	}
	if c.ExternalNatsURL == "" {
		ports = append(ports, port{"NatsClientPort", c.NatsClientPort}, port{"NatsMonitorPort", c.NatsMonitorPort})
	}
	seen := make(map[int]string, len(ports))
	for _, p := range ports {
		if prev, ok := seen[p.port]; ok {
			return fmt.Errorf("stack config: %s and %s both use host port %d", prev, p.field, p.port)
		}
		seen[p.port] = p.field
	}
	return nil
}

// overlapsLoopbackPublish reports whether a bind may also take 127.0.0.1; a hostname could resolve there.
func overlapsLoopbackPublish(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip == nil || ip.IsUnspecified() || ip.Equal(net.IPv4(127, 0, 0, 1))
}
