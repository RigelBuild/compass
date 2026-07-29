//go:build unix

// The production SessionHost: the Runner's authoritative session set over the
// built AgentRuntime + StartAgent relay. It resolves a container by name, starts
// the first-party agent in it, and tracks the live session set — the Runner is
// authoritative for live session truth (OQ6), so Status answers from here and
// the Server reconciles to it on reattach.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strconv"
	"sync"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runner/gateway"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// agentSocketDir is the per-container subdirectory (under the Runner's runtime
// dir) that holds one container's agent socket, and agentSocketFile is the
// socket's fixed basename (OQ-5: RuntimeDir/containers/<container>/agent.sock,
// container-keyed per Decision #4, never session-keyed). agentSocketMountPath is
// the fixed in-container path the socket is bind-mounted to, so the agent needs
// no per-session configuration — it always dials the same path.
const (
	agentSocketDir       = "containers"
	agentSocketFile      = "agent.sock"
	agentSocketMountPath = "/run/compass/agent.sock"
)

// SpecBuilder maps a provision request to a complete runtime.AgentSpec — the
// image, per-agent workspace, and egress policy the container is launched with.
// It is the policy seam T4 keeps injectable: production derives the image +
// default-deny egress allowlist for the agent account; a test supplies a fake
// spec. Keeping it a seam means Provision is fully wired to AgentRuntime.Launch
// without T4 hard-coding image/egress derivation that later tiers own.
type SpecBuilder interface {
	BuildSpec(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error)
}

// agentHost is the production SessionHost. It owns the live session set and
// drives the container lifecycle through the AgentRuntime registry + the relay.
type agentHost struct {
	link       *ServerLink
	runtime    *runtime.AgentRuntime
	registry   *runtime.AgentRegistry
	engine     runtime.ContainerRuntime
	specs      SpecBuilder
	log        *slog.Logger
	runtimeDir string
	// model is the model selector handed to every agent this Runner starts;
	// empty leaves the agent on its own default.
	model string

	mu       sync.Mutex
	sessions map[string]*liveSession
	sockets  map[string]*gateway.SocketListener
	nextID   func() string
}

// liveSession is one running agent session: its container and the relay stream
// pumping its frames up PublishEvents.
type liveSession struct {
	sessionID     string
	containerName string
	containerID   runtime.ContainerID
	stream        *AgentStream
	state         compassv1.AgentSessionState
}

// AgentHostConfig is the SessionHost's own configuration, distinct from the
// collaborators it is built over. Two adjacent strings as positional params
// would be silently swappable at the call site; a struct makes each named.
type AgentHostConfig struct {
	// RuntimeDir is the Runner-owned base dir the per-container agent sockets
	// live under (RuntimeDir/containers/<container>/agent.sock).
	RuntimeDir string
	// AgentModel is the model selector every agent this host starts receives;
	// empty leaves the agent on its default.
	AgentModel string
}

// NewSessionHost builds the production SessionHost over the link, the agent
// runtime + registry (so a launched container resolves by name), the container
// engine, the spec builder Provision derives its AgentSpec from, and the host's
// own config. newID mints session ids; nil uses a monotonic counter.
func NewSessionHost(link *ServerLink, rt *runtime.AgentRuntime, registry *runtime.AgentRegistry, engine runtime.ContainerRuntime, specs SpecBuilder, cfg AgentHostConfig, log *slog.Logger, newID func() string) SessionHost {
	if log == nil {
		log = slog.Default()
	}
	if newID == nil {
		newID = monotonicIDs()
	}
	return &agentHost{
		link:       link,
		runtime:    rt,
		registry:   registry,
		engine:     engine,
		specs:      specs,
		log:        log,
		runtimeDir: cfg.RuntimeDir,
		model:      cfg.AgentModel,
		sessions:   map[string]*liveSession{},
		sockets:    map[string]*gateway.SocketListener{},
		nextID:     newID,
	}
}

// Provision derives the AgentSpec from the request, creates and serves the
// per-container agent socket (before `podman run`, so the bind-mount source is
// live), mounts it into the spec, and launches the isolated container through
// the AgentRuntime façade, returning its stable container name. The socket is
// the agent->Runner call transport (design SEA-1351 T5): it is served from
// Provision so a call arriving before Start binds a session fails closed rather
// than finding no listener. Launch registers the handle so a later Start
// resolves it by name. The dispatcher's request-id dedup makes a provision retry
// idempotent (no duplicate container) before this runs; a genuine spec/launch
// failure surfaces here, and a socket already serving that container name is
// reused rather than double-served (idempotent retry).
func (h *agentHost) Provision(ctx context.Context, req *compassv1.ProvisionAgentWorkspaceRequest) (string, error) {
	spec, err := h.specs.BuildSpec(req)
	if err != nil {
		return "", err
	}
	listener, err := h.serveSocket(ctx, spec.Name)
	if err != nil {
		return "", err
	}
	spec.Mounts = append(spec.Mounts, listener.Mount(agentSocketMountPath))
	handle, err := h.runtime.Launch(ctx, spec)
	if err != nil {
		// Launch failed, so no container will ever mount this socket; tear it
		// down rather than leak the listener + file until host shutdown.
		h.closeSocket(ctx, spec.Name)
		return "", err
	}
	return handle.Name(), nil
}

// Session resolves the one live session bound to a container (gateway's
// SessionForContainer): the Gateway serving that container's socket forwards a
// comms call under this session id. A container with no live session (socket
// served at Provision, before Start binds one, or after Stop) returns ok=false,
// which the Gateway turns into a fail-closed CodePermissionDenied — never a
// forward with an empty session id.
func (h *agentHost) Session(containerName string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.containerName == containerName {
			return s.sessionID, true
		}
	}
	return "", false
}

// Close tears down every live agent socket, draining in-flight calls under each
// listener's bounded deadline. It is the container-teardown symmetric point in
// the single-Runner MVP: there is no per-container Deprovision RPC (a session
// Stop/Reload reuses the container and its socket), so every container lives
// until the Runner process ends, and Close runs once on that shutdown. A crash
// instead leaves the socket files on disk, which the next Provision reclaims
// (gateway.reclaimStaleSocket).
func (h *agentHost) Close(ctx context.Context) {
	h.mu.Lock()
	listeners := make(map[string]*gateway.SocketListener, len(h.sockets))
	maps.Copy(listeners, h.sockets)
	h.sockets = map[string]*gateway.SocketListener{}
	h.mu.Unlock()
	for name, l := range listeners {
		if err := l.Close(ctx); err != nil {
			h.log.Warn("closing agent socket", slog.String("container", name), slog.Any("error", err))
		}
	}
}

// Start resolves the launched container by name and starts the agent relay in
// it. A container already hosting a live session returns errAlreadyRunning (a
// genuine double start; the dispatcher's request-id dedup handles idempotent
// retries before this is reached).
func (h *agentHost) Start(ctx context.Context, req *compassv1.StartAgentSessionRequest) (string, error) {
	name := req.GetContainerName()
	handle, ok := h.registry.Resolve(name)
	if !ok {
		return "", errSessionUnknown
	}

	h.mu.Lock()
	for _, s := range h.sessions {
		if s.containerName == name {
			h.mu.Unlock()
			return "", errAlreadyRunning
		}
	}
	sessionID := h.nextID()
	h.mu.Unlock()

	// The existing-session check releases h.mu before the slow StartAgent below
	// and re-acquires it to record. Two concurrent Starts for one container
	// could both pass the check in that window (TOCTOU). Unreachable in the
	// single-Runner MVP: the Sessions dispatch loop is strictly sequential
	// (dispatch.go — Receive→execute→Send, no per-command goroutine) and Run is
	// single-shot (run.go — one host, one RunSessions, no in-process reconnect),
	// so only one lifecycle op is ever in flight against this host. A per-session
	// transition lock is deferred to T9, where in-process reattach against a
	// persistent host first makes concurrent callers reachable (go-toolchain-default.md:979).

	stream, err := h.link.StartAgent(ctx, sessionID, handle.ID(), h.engine, h.agentEnv(handle), h.log)
	if err != nil {
		return "", err
	}

	h.mu.Lock()
	h.sessions[sessionID] = &liveSession{
		sessionID:     sessionID,
		containerName: name,
		containerID:   handle.ID(),
		stream:        stream,
		state:         compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
	}
	// Create the session's control state here, under the same lock that records
	// the session — the mirror of Stop's retirement. The Runner owning both ends
	// is what lets the agent-driven paths refuse an id they do not know: an
	// agent that subscribes or acks against a session the lifecycle never bound
	// (or already retired) is turned away, instead of minting state nothing
	// would ever reclaim.
	if listener, served := h.sockets[name]; served {
		listener.BindSession(sessionID)
	}
	h.mu.Unlock()
	return sessionID, nil
}

// Stop tears a session down. An unknown/already-stopped session succeeds
// (idempotent, matching the frozen StopAgentSession semantics).
func (h *agentHost) Stop(_ context.Context, sessionID string) error {
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	if ok {
		delete(h.sessions, sessionID)
		// The socket outlives the session (a Stop/Start reuses the container and
		// its socket), so the control producer keeps this session's retained ops
		// unless the teardown says otherwise. Retire under the same lock that
		// drops the session, so a concurrent Start on a fresh id cannot observe a
		// half-torn state.
		if listener, served := h.sockets[s.containerName]; served {
			listener.RetireSession(sessionID)
		}
	}
	h.mu.Unlock()
	if !ok {
		return nil
	}
	return s.stream.Stop()
}

// Reload restarts a session's agent in place, reusing the session id so the
// board entry is continuous.
func (h *agentHost) Reload(ctx context.Context, sessionID string) error {
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	h.mu.Unlock()
	// Reload reads the session under h.mu, releases, then runs the slow Stop +
	// StartAgent unlocked before re-locking to swap the stream. A concurrent Stop
	// could delete the session mid-interval, leaving Reload to relaunch through a
	// pointer the caller was told had stopped. Same MVP invariant as Start makes
	// this unreachable (sequential dispatch + single-shot Run); the per-session
	// transition lock that serializes Stop vs Reload is T9 (go-toolchain-default.md:979).
	if !ok {
		return errSessionUnknown
	}
	// Re-resolve the handle BEFORE stopping, so the relaunch carries the same
	// identity and configuration the original Start did and a session whose
	// container has since been dropped from the registry is rejected while its
	// agent is still running: the error path is a true no-op, never a stopped
	// agent left behind a session the live set still reports READY.
	handle, ok := h.registry.Resolve(s.containerName)
	if !ok {
		return errSessionUnknown
	}
	if err := s.stream.Stop(); err != nil {
		return err
	}
	stream, err := h.link.StartAgent(ctx, sessionID, s.containerID, h.engine, h.agentEnv(handle), h.log)
	if err != nil {
		return err
	}
	h.mu.Lock()
	s.stream = stream
	s.state = compassv1.AgentSessionState_AGENT_SESSION_STATE_READY
	h.mu.Unlock()
	return nil
}

// Status returns one session's status, or every live session's when id is empty
// — answered from the Runner's authoritative live set.
func (h *agentHost) Status(_ context.Context, sessionID string) ([]*compassv1.AgentSessionStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sessionID != "" {
		s, ok := h.sessions[sessionID]
		if !ok {
			return nil, errSessionUnknown
		}
		return []*compassv1.AgentSessionStatus{{SessionId: s.sessionID, State: s.state}}, nil
	}
	out := make([]*compassv1.AgentSessionStatus, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, &compassv1.AgentSessionStatus{SessionId: s.sessionID, State: s.state})
	}
	return out, nil
}

// agentEnv derives the agent exec's identity and configuration from the
// launched container's handle, so Start and Reload cannot drift apart. The
// model is Runner-wide config; everything else is per-container.
func (h *agentHost) agentEnv(handle *runtime.AgentHandle) AgentEnv {
	return AgentEnv{
		UID:     handle.WorkspaceUID(),
		HomeDir: handle.HomeDir(),
		Workdir: handle.CheckoutDir(),
		Model:   h.model,
	}
}

// serveSocket creates and serves the per-container agent socket for
// containerName, recording the listener so Provision can mount it and teardown
// can Close it. A container already serving a socket (an idempotent provision
// retry) is a no-op — the live listener is kept, never double-served. The
// Gateway forwards to the Server over the Runner's own RunnerService client
// (the link), resolving the container to its bound session via this host.
func (h *agentHost) serveSocket(ctx context.Context, containerName string) (*gateway.SocketListener, error) {
	h.mu.Lock()
	if listener, served := h.sockets[containerName]; served {
		h.mu.Unlock()
		return listener, nil
	}
	h.mu.Unlock()
	path := filepath.Join(h.runtimeDir, agentSocketDir, containerName, agentSocketFile)
	listener, err := gateway.Serve(ctx, path, containerName, h, h.link.client, h.link.client, h.link.client)
	if err != nil {
		return nil, fmt.Errorf("serving agent socket for container %q: %w", containerName, err)
	}
	h.mu.Lock()
	h.sockets[containerName] = listener
	h.mu.Unlock()
	return listener, nil
}

// closeSocket tears down and forgets the container's agent socket, draining any
// in-flight call under the listener's bounded deadline. A container with no
// recorded socket is a no-op.
func (h *agentHost) closeSocket(ctx context.Context, containerName string) {
	h.mu.Lock()
	listener, ok := h.sockets[containerName]
	if ok {
		delete(h.sockets, containerName)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	if err := listener.Close(ctx); err != nil {
		h.log.Warn("closing agent socket", slog.String("container", containerName), slog.Any("error", err))
	}
}

// monotonicIDs returns a session-id minter — a simple monotonic counter,
// sufficient for the single-Runner MVP where ids are Runner-local.
func monotonicIDs() func() string {
	var mu sync.Mutex
	var n uint64
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return "sess-" + strconv.FormatUint(n, 10)
	}
}
