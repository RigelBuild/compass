//go:build unix

// The production SessionHost: the Runner's authoritative session set over the
// AgentRuntime + StartAgent relay. It resolves a container by name, starts the
// agent in it, and tracks the live session set — the Runner is authoritative for
// live session truth (OQ6), so Status answers here and the Server reconciles on reattach.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runner/gateway"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// Per-container agent socket layout: RuntimeDir/containers/<container>/agent.sock
// (container-keyed per Decision #4, never session-keyed). The mount path is fixed
// in-container so the agent always dials the same path with no per-session config.
const (
	agentSocketDir       = "containers"
	agentSocketFile      = "agent.sock"
	agentSocketMountPath = "/run/compass/agent.sock"
	agentConfigMountPath = "/run/compass/agent-config"
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

// vsockGatewayEngine is the unexported backend probe the microVM runtime
// satisfies: a session's AgentGateway is served host-side over a per-session
// vsock AF_UNIX path resolved AFTER Launch, not the pre-Launch bind-mounted
// socket the podman path uses. Provision/RefreshConfig type-assert to gate it.
type vsockGatewayEngine interface {
	AgentGatewayEndpoint(name string) (endpoint string, ok bool)
}

// hostStateEngine is the unexported backend probe the host-process runtime
// satisfies: each agent runs as a direct host child with no bind mounts, so its
// gateway socket and config tree live in the handle's private state dir, threaded
// as env vars. Provision type-asserts to gate the host-specific leg.
type hostStateEngine interface {
	AgentStateDir(id runtime.WorkloadID) (dir string, ok bool)
}

// hostAgentTransport is the per-agent socket + config-root paths the host leg
// serves inside the handle's state dir, keyed by container name. Its presence
// reroutes configMaterializerFor to the state-dir root so a later refresh
// materializes where the agent actually reads.
type hostAgentTransport struct {
	socketPath string
	configRoot string
}

// agentHost is the production SessionHost. It owns the live session set and
// drives the container lifecycle through the AgentRuntime registry + the relay.
type agentHost struct {
	link       *ServerLink
	runtime    *runtime.AgentRuntime
	registry   *runtime.AgentRegistry
	engine     runtime.WorkloadRuntime
	specs      SpecBuilder
	log        *slog.Logger
	runtimeDir string
	// model is the model selector handed to every agent this Runner starts;
	// empty leaves the agent on its own default.
	model string

	mu           sync.Mutex
	sessions     map[string]*liveSession
	sockets      map[string]*gateway.SocketListener
	nextID       func() string
	materializer *runtime.SecretMaterializer
	// configVersions is the last config bundle version materialized per container,
	// keyed by container name — compared on a ConfigVersion update so an agent is
	// Reloaded only when the version moved. Recorded after Reload succeeds, so a
	// swallowed failure leaves it unmoved and the next signal retries.
	configVersions map[string]string
	// containerLocks serializes state-transitioning lifecycle ops per container name
	// so concurrent dispatch cannot interleave two transitions on one container.
	// Guarded by h.mu for get-or-create only; entries are NEVER deleted — a deleted
	// entry could race an op holding the same *sync.Mutex, breaking resolve-then-lock.
	containerLocks map[string]*sync.Mutex
	// hostTransports records the per-agent socket + config-root paths the host
	// backend's Provision leg serves in each handle's state dir, keyed by container
	// name. Empty for podman/microVM (fixed mount paths). Set at Provision, removed
	// at teardown (closeSocket).
	hostTransports map[string]hostAgentTransport
}

// liveSession is one running agent session: its container and the relay stream
// pumping its frames up PublishEvents.
type liveSession struct {
	sessionID     string
	containerName string
	containerID   runtime.WorkloadID
	stream        *AgentStream
	state         compassv1.AgentSessionState
	// agentAccountID is the owned agent account this session belongs to, copied
	// from the resolved handle at Start so Status can attribute the session's
	// AgentSessionStatus to its account (DL-167) — the field the Server's
	// reject-on-live scan matches against.
	agentAccountID string
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
// own config. newID mints session ids; nil uses a crypto-random allocator.
func NewSessionHost(link *ServerLink, rt *runtime.AgentRuntime, registry *runtime.AgentRegistry, engine runtime.WorkloadRuntime, specs SpecBuilder, cfg AgentHostConfig, log *slog.Logger, newID func() string) SessionHost {
	if log == nil {
		log = slog.Default()
	}
	if newID == nil {
		newID = randomIDs()
	}
	return &agentHost{
		link:           link,
		runtime:        rt,
		registry:       registry,
		engine:         engine,
		specs:          specs,
		log:            log,
		runtimeDir:     cfg.RuntimeDir,
		model:          cfg.AgentModel,
		sessions:       map[string]*liveSession{},
		sockets:        map[string]*gateway.SocketListener{},
		nextID:         newID,
		materializer:   runtime.NewSecretMaterializer(engine, log),
		configVersions: map[string]string{},
		containerLocks: map[string]*sync.Mutex{},
		hostTransports: map[string]hostAgentTransport{},
	}
}

// Provision derives the AgentSpec from the request, creates and serves the
// per-container agent socket (before `podman run`, so the bind-mount source is
// live), mounts it into the spec, and launches the isolated container through
// the AgentRuntime façade, returning its stable container name. The socket is
// the agent->Runner call transport (design RIG-1351 T5): it is served from
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
	// Serialize all transitions on this container: a concurrent Remove/Start of
	// the same name cannot interleave with this provision. Resolved from the spec
	// name (the stable lifecycle key).
	unlock := h.lockContainer(spec.Name)
	defer unlock()
	// microVM serves the AgentGateway over a per-session vsock path not knowable
	// until Launch mints the session runtime dir, so it inverts order: Launch first,
	// then serve — and refuses the socket/config bind mounts (record §(c)/§(f)).
	// Probe absent (podman, every fake) keeps today's body byte-identical.
	if vsockEngine, ok := h.engine.(vsockGatewayEngine); ok {
		return h.provisionVsockGateway(ctx, spec, vsockEngine)
	}
	// The host-process backend runs each agent as a direct child with no bind mounts,
	// so its socket + config live in the handle's state dir, threaded as env vars.
	// The dir is minted by Create inside Launch, so this leg also inverts order.
	// Probe absent (podman, every fake) keeps today's body byte-identical.
	if hostEngine, ok := h.engine.(hostStateEngine); ok {
		return h.provisionHostGateway(ctx, spec, hostEngine)
	}
	listener, err := h.serveSocket(ctx, spec.Name)
	if err != nil {
		return "", err
	}
	spec.Mounts = append(spec.Mounts, listener.Mount(agentSocketMountPath))
	// Materialize the fleet config and bind-mount it read-only. Target is the PARENT
	// config dir, so a later `current/` symlink flip is visible with no remount.
	// mcsLabel empty — Materialize runs before the container exists, so the
	// create-time :Z relabel covers the whole tree.
	mount, err := h.configMaterializerFor(spec.Name).Materialize(ctx, "")
	if err != nil {
		// Config could not be materialized; abort provision rather than launch a
		// container with no config. Tear the socket down (mirror the Launch-
		// failure cleanup) so it does not leak until host shutdown.
		h.closeSocket(ctx, spec.Name)
		return "", fmt.Errorf("materializing agent config: %w", err)
	}
	spec.Mounts = append(spec.Mounts, runtime.Mount{HostPath: mount.HostPath, ContainerPath: agentConfigMountPath, ReadOnly: true})
	handle, err := h.runtime.Launch(ctx, spec)
	if err != nil {
		// Launch failed, so no container will ever mount this socket; tear it
		// down rather than leak the listener + file until host shutdown.
		h.closeSocket(ctx, spec.Name)
		return "", err
	}
	// Record the version materialized into this container's root, so the first
	// ConfigVersion signal only Reloads the agent if the bundle actually moved
	// past what Provision already installed. Container-keyed and set under h.mu
	// so a concurrent RefreshConfig pass observes a consistent map.
	h.mu.Lock()
	h.configVersions[spec.Name] = mount.Version
	h.mu.Unlock()
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

// Close drains every container this Runner is hosting on process shutdown: it
// stops and removes each one through the same teardown path Remove uses (session
// retire + AgentRuntime.Teardown + socket close), so no agent container outlives
// the Runner. It is the container-teardown symmetric point in the single-Runner
// MVP: there is no per-container Deprovision RPC (a session Stop/Reload reuses
// the container and its socket), so every container lives until the Runner
// process ends, and Close runs once on that shutdown.
//
// The listener-only teardown this replaces closed just the agent sockets, which
// left the podman containers running unsupervised: conmon double-forks them out
// of the Runner's process group, so the stack's group-signal on shutdown never
// reached them either. Close now enumerates the provisioned-container set (the
// h.sockets keys — Provision serves a socket keyed by container name before
// `podman run` and teardown removes the key, so the keys are exactly the
// containers provisioned and not yet removed, a superset of the started
// sessions) and tears each down.
//
// The teardowns run concurrently, so total shutdown is ~one stop grace regardless
// of container count, not N × the grace. Close runs under
// context.WithoutCancel(ctx) (run.go) precisely so the stop gets its full grace
// even though the parent ctx is already cancelled by the shutdown signal. Every
// teardown is joined before Close returns — a leaked goroutine plus an un-reaped
// container is the exact failure this closes. A per-container teardown error is
// logged, never fatal: a best-effort drain is correct here (the stack lingers
// safe on a failed teardown), matching drainChildren's attempt-every-child
// best-effort posture — though the error handling differs: drainChildren joins
// its children's errors and returns the aggregate, whereas Close, returning void
// on the process-shutdown path, logs each per-container error and discards it.
//
// Remove closes each container's socket itself (its deferred closeSocket), so
// Close delegates entirely to Remove and never double-closes: after Close
// returns there is no running container and no socket left, each closed once. A
// crash instead leaves the socket files on disk, which the next Provision
// reclaims (gateway.reclaimStaleSocket).
func (h *agentHost) Close(ctx context.Context) {
	h.mu.Lock()
	names := make([]string, 0, len(h.sockets))
	for name := range h.sockets {
		names = append(names, name)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			if err := h.Remove(ctx, name); err != nil {
				h.log.Warn("tearing down agent container", slog.String("container", name), slog.Any("error", err))
			}
		})
	}
	wg.Wait()
}

// Start resolves the launched container by name and starts the agent relay in
// it. A container already hosting a live session returns errAlreadyRunning (a
// genuine double start; the dispatcher's request-id dedup handles idempotent
// retries before this is reached).
func (h *agentHost) Start(ctx context.Context, req *compassv1.StartAgentSessionRequest, resumeBody string) (string, error) {
	name := req.GetContainerName()
	// Serialize transitions on this container across the whole Start: the
	// existing-session check releases h.mu before the slow StartAgent, so two
	// concurrent Starts could both pass it (a TOCTOU minting a duplicate session).
	// The per-container lock closes it; T9's reattach (RIG-1328) reuses this lock.
	unlock := h.lockContainer(name)
	defer unlock()

	handle, ok := h.registry.Resolve(name)
	if !ok {
		return "", errSessionUnknown
	}
	if handle == nil {
		return "", errSessionUnknown
	}

	h.mu.Lock()
	for _, s := range h.sessions {
		if s.containerName == name {
			h.mu.Unlock()
			return "", errAlreadyRunning
		}
	}
	// A resume REUSES the logical session id as the live id, so resumed transcript
	// frames commit under the SAME session key — one durable lineage, BindLifetime's
	// entry_seq rebase continues under that key. A fresh start mints a new id. The
	// Server skips RecordAgentSession on resume (row already exists).
	sessionID := req.GetResumeSessionId()
	if sessionID == "" {
		sessionID = h.nextID()
	} else if _, live := h.sessions[sessionID]; live {
		// Reusing the logical id as the map key means a resume racing a still-live
		// prior lifetime would clobber its liveSession entry, orphaning its stream.
		// The single-orchestrator precondition keeps this unreachable; guard so a
		// violation fails loud rather than leaking the prior stream.
		h.mu.Unlock()
		return "", errAlreadyRunning
	}
	h.mu.Unlock()

	// Materialize the agent's secrets BEFORE exec'ing, so its first read never races
	// an empty seed (RIG-1327 T5). CodeFailedPrecondition means no secrets surface and
	// is tolerated (start anyway); every other error — including the transient
	// CodeUnavailable against a Server that HAS secrets — fails the Start.
	resolved, err := h.link.FetchSecretsByContainer(ctx, name)
	switch {
	case err == nil:
		if err := h.materializer.Install(ctx, handle.ID(), handle.HomeDir(), handle.WorkspaceUID(), resolved); err != nil {
			return "", fmt.Errorf("materializing secrets before agent start for container %q: %w", name, err)
		}
	case connect.CodeOf(err) == connect.CodeFailedPrecondition:
		// No secrets surface on this Server: nothing to materialize. Start the
		// agent anyway. Logged at Warn so a secrets-less start is visible — it is
		// a degraded posture even when intended.
		h.log.Warn("no secrets surface; starting agent without materialized secrets", "container", name)
	default:
		return "", fmt.Errorf("fetching secrets before agent start for container %q: %w", name, err)
	}

	// On an authorized resume, materialize the server-reconstructed session file
	// into the container BEFORE exec'ing the agent, so the agent's first read finds
	// it (RIG-1570 T8). Exported as COMPASS_RESUME_SESSION_FILE. A fresh start does
	// nothing here; the discriminator is a non-empty resume_session_id.
	env := h.agentEnv(handle)
	if id := req.GetResumeSessionId(); id != "" {
		// The resume_session_id becomes a filename component below. The Server
		// authz-gates it and never sends a locator, but the Runner is a distinct
		// trust boundary: reject anything that is not a bare path element so a
		// crafted id can never redirect the write outside .compass/resume/.
		if id != filepath.Base(id) || strings.ContainsRune(id, filepath.Separator) {
			return "", fmt.Errorf("materializing resume session file for container %q: invalid resume_session_id", name)
		}
		resumeDir := filepath.Join(".compass", "resume")
		relPath := filepath.Join(resumeDir, id+".jsonl")
		// Belt-and-suspenders on the guard above: a bare "." or ".." passes the
		// element check and is neutralized only by the ".jsonl" suffix, so assert
		// the cleaned path still lands directly in the resume dir. This pins that
		// invariant explicitly rather than leaving it emergent from the suffix.
		if filepath.Dir(relPath) != resumeDir {
			return "", fmt.Errorf("materializing resume session file for container %q: invalid resume_session_id", name)
		}
		if resumeBody == "" {
			// An authorized id with no reconstructed body is Server-side skew (authz
			// passed but reconstruction produced nothing); materialize the empty file
			// per the id-is-the-discriminator contract, but surface the reconstruction
			// bug here rather than only as an agent resuming from an empty transcript.
			h.log.Warn("resume_session_id set with empty body; materializing empty resume file", "container", name, "resume_session_id", id)
		}
		h.log.Info("materializing resume session file", "container", name, "resume_session_id", id)
		if err := h.runtime.WriteAgentFile(ctx, handle.ID(), handle.WorkspaceUID(), handle.HomeDir(), relPath, resumeBody); err != nil {
			return "", fmt.Errorf("materializing resume session file for container %q: %w", name, err)
		}
		env.ResumeSessionFile = filepath.Join(handle.HomeDir(), relPath)
	} else if resumeBody != "" {
		// The inverse skew: a body with no resume_session_id. The id is the sole
		// discriminator, so this start is treated as fresh and the body dropped —
		// surfaced symmetrically with the empty-body warning above.
		h.log.Warn("resume body supplied without resume_session_id; dropping", "container", name)
	}

	stream, err := h.link.StartAgent(ctx, sessionID, handle.ID(), h.engine, env, h.log)
	if err != nil {
		return "", err
	}

	h.mu.Lock()
	h.sessions[sessionID] = &liveSession{
		sessionID:      sessionID,
		containerName:  name,
		containerID:    handle.ID(),
		stream:         stream,
		state:          compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
		agentAccountID: handle.AgentAccountID(),
	}
	// Create the session's control state here, under the same lock that records the
	// session — the mirror of Stop's retirement. Owning both ends lets agent-driven
	// paths refuse an id they do not know, instead of minting unreclaimable state.
	listener, served := h.sockets[name]
	if served {
		listener.BindSession(sessionID)
	}
	h.mu.Unlock()

	// Lift the agent's replay barrier so the first idle-deliver is dispatched rather
	// than refused: the barrier defaults closed and only replay_complete lifts it.
	// Sent on EVERY served start (a file-based resume loads its transcript before
	// subscribing); sent after the h.mu release, the per-container lock rules out races.
	if served {
		op := &compassv1internal.AgentControl{
			Control: &compassv1internal.AgentControl_ReplayComplete{
				ReplayComplete: &compassv1internal.ReplayComplete{},
			},
		}
		if err := listener.SendControl(sessionID, op); err != nil {
			// A served listener always has a wired producer (gateway.Serve), so this
			// is an unreachable wiring fault in production, not a reason to fail an
			// already-recorded Start. Log and continue.
			h.log.Error("sending replay_complete", slog.String("container", name), slog.String("session_id", sessionID), slog.Any("error", err))
		}
	}
	return sessionID, nil
}

// Stop tears a session down. An unknown/already-stopped session succeeds
// (idempotent, matching the established StopAgentSession semantics).
func (h *agentHost) Stop(_ context.Context, sessionID string) error {
	// Resolve session→container under h.mu first, release, then take the
	// container lock and re-check — the resolve-then-lock protocol (the lock key
	// is the container, but the caller names a session). A session that vanished
	// in the gap re-checks to not-found below and returns nil (idempotent).
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	h.mu.Unlock()
	if !ok {
		return nil
	}
	unlock := h.lockContainer(s.containerName)
	defer unlock()

	h.mu.Lock()
	s, ok = h.sessions[sessionID]
	if ok {
		delete(h.sessions, sessionID)
		// The socket outlives the session (Stop/Start reuses the container and its
		// socket), so retained ops survive unless teardown says otherwise. Retire
		// under the same lock that drops the session, so a concurrent Start on a
		// fresh id cannot observe a half-torn state.
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

// Remove tears a container down and everything bound to it: it retires the live
// session the container hosts (if any), stops that session's agent exec, tears
// the container down through the AgentRuntime (stop + remove + deregister), and
// closes the container's agent socket. It is the teardown-symmetric counterpart
// to Provision (which serves the socket and launches the container).
//
// Idempotent, like Stop: an unknown container — never provisioned, or already
// removed — is a no-op success. A container whose registry handle is already
// gone but whose socket still lingers (a crash-reclaimed or post-Stop container)
// still has its socket closed, so a Remove always leaves no socket behind.
func (h *agentHost) Remove(ctx context.Context, containerName string) error {
	// Serialize all transitions on this container across the whole teardown, so a
	// concurrent Provision/Start/Stop/Reload of the same name cannot interleave.
	unlock := h.lockContainer(containerName)
	defer unlock()
	// Retire the live session bound to this container under h.mu — the mirror of
	// Stop's retirement — so a concurrent lifecycle op cannot observe a half-torn
	// state. The socket is closed unconditionally below, so it is not retired here
	// (a container with no bound session has nothing to retire).
	h.mu.Lock()
	var retired *liveSession
	for id, s := range h.sessions {
		if s.containerName == containerName {
			delete(h.sessions, id)
			if listener, served := h.sockets[containerName]; served {
				listener.RetireSession(id)
			}
			retired = s
			break
		}
	}
	// Forget this container's materialized config version — the container is
	// being torn down, so its per-container root and its version tracking go
	// with it. A later re-Provision of the name re-records from a fresh
	// materialize.
	delete(h.configVersions, containerName)
	h.mu.Unlock()

	// Close the container's agent socket once teardown returns, whatever the
	// outcome, so a Remove never leaves a listener behind — not even when the
	// container teardown below fails. Deferred, so it still runs last (after the
	// container is stopped and removed) on the success path.
	defer h.closeSocket(ctx, containerName)

	// Stop the retired session's agent exec outside the lock (it terminates a
	// child and joins its drains). A stop error is logged, never returned: the
	// container teardown below is the operation that must complete, and a failed
	// exec-stop must not leave the container running.
	if retired != nil {
		if err := retired.stream.Stop(); err != nil {
			h.log.Warn("stopping agent exec during container remove",
				slog.String("container", containerName), slog.String("session_id", retired.sessionID), slog.Any("error", err))
		}
	}

	// Tear the container down through the AgentRuntime (stop + remove +
	// deregister). A container the registry no longer resolves is already gone —
	// an idempotent no-op; its socket is still closed by the deferred close above.
	if handle, ok := h.registry.Resolve(containerName); ok {
		if handle == nil {
			return fmt.Errorf("tearing down container %q: registry resolved a nil handle", containerName)
		}
		if err := h.runtime.Teardown(ctx, handle); err != nil {
			return fmt.Errorf("tearing down container %q: %w", containerName, err)
		}
	}
	return nil
}

// Reload restarts a session's agent in place, reusing the session id so the
// board entry is continuous. This is the public, dispatch-facing entry: it takes
// the session's container transition lock and delegates to reloadLocked. It must
// NOT be called from a caller that already holds the container lock — the
// config worker's RefreshConfig leg calls reloadLocked directly for exactly that
// reason (the lock is non-reentrant; calling Reload while holding it would
// self-deadlock). See docs/designs/infra/runtime/compass-runner-concurrent-dispatch/design.md.
func (h *agentHost) Reload(ctx context.Context, sessionID string) error {
	// Resolve session→container under h.mu, release, take the container lock,
	// then delegate — the resolve-then-lock protocol, mirroring Stop. A session
	// that vanished before the lock is rejected in reloadLocked's re-check.
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	h.mu.Unlock()
	if !ok {
		return errSessionUnknown
	}
	unlock := h.lockContainer(s.containerName)
	defer unlock()
	return h.reloadLocked(ctx, sessionID)
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
		return []*compassv1.AgentSessionStatus{h.statusOf(s)}, nil
	}
	out := make([]*compassv1.AgentSessionStatus, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, h.statusOf(s))
	}
	return out, nil
}

// Deliver writes a server-relayed control op to sessionID's container socket —
// the receive arm of the Server's send-only DeliverControl dispatch. It
// resolves session→container→socket under h.mu, releases, then delegates to the
// listener's SendControl (whose control producer has its own locking).
//
// It deliberately does NOT take the container transition lock: a control-send is
// not a container transition (Start/Reload/Stop/Remove), so serializing delivers
// behind those would add latency for no safety gain — the producer is already
// concurrency-safe. Resolving under h.mu and returning errSessionUnknown when
// the session is already gone is the guard against a deliver racing a Stop: Stop
// removes the session from h.sessions (and retires its control state) under
// h.mu before the socket goes away, so a deliver that observes the session still
// present resolved a live socket, and one that races past removal returns
// errSessionUnknown rather than delivering into a retired session.
func (h *agentHost) Deliver(_ context.Context, sessionID string, op *compassv1internal.AgentControl) error {
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	if !ok {
		h.mu.Unlock()
		return errSessionUnknown
	}
	listener, served := h.sockets[s.containerName]
	h.mu.Unlock()
	if !served {
		return errSessionUnknown
	}
	return listener.SendControl(sessionID, op)
}

// RefreshSecrets re-fetches the resolved secret set bound to sessionID and
// materializes it into the session's container over the stdin-exec channel — the
// SecretsVersion-driven ROTATION path (T6). The initial materialize is done
// synchronously in Start before the agent exec (by-container fetch); this signal
// path only re-materializes a live session after a rotation. An unknown session
// errors; the fetch is authz'd server-side on the live session binding, so it is
// only ever driven for a bound session. The container's $HOME and agent uid come
// from its resolved handle, so the materialize runs as the agent user in its own
// home, the git-credential posture.
//
// Rotation reach differs by delivery. Provider-seed, gh, and generic file
// secrets rotate into a live agent for any consumer that re-reads the file per
// use — git's credential helper is invoked per git op, so it does; the provider
// seed rotates live only if its consumer re-reads it rather than caching at
// startup (that is a property of the consumer, which lives in a sibling repo).
// ENV-delivery secrets never rotate live: the agent sources
// $HOME/.compass/env once at startup, and a process's environment is fixed at
// exec, so a rewritten env file never reaches a running agent — an env-secret
// rotation is picked up only on the agent's next start. This re-materialize
// keeps the on-disk file current for that next start; it does not, and cannot,
// mutate the live process env.
func (h *agentHost) RefreshSecrets(ctx context.Context, sessionID string) error {
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	h.mu.Unlock()
	if !ok {
		return errSessionUnknown
	}
	// The map read above and the Resolve/FetchSecrets below are deliberately not
	// atomic: holding h.mu across the network FetchSecrets would serialize every
	// session. A session torn down in the gap resolves to errSessionUnknown here,
	// which the best-effort dispatch hook logs and recovers on the next signal.
	handle, ok := h.registry.Resolve(s.containerName)
	if !ok {
		return errSessionUnknown
	}
	if handle == nil {
		return errSessionUnknown
	}
	resolved, err := h.link.FetchSecrets(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("fetching secrets for session %q: %w", sessionID, err)
	}
	if err := h.materializer.Install(ctx, handle.ID(), handle.HomeDir(), handle.WorkspaceUID(), resolved); err != nil {
		return fmt.Errorf("materializing secrets for session %q: %w", sessionID, err)
	}
	return nil
}

// RefreshConfig re-materializes the current fleet config bundle into every live
// session's per-container root and Reloads each agent whose config version
// actually moved — the fleet-wide ConfigVersion-driven update path (contrast
// RefreshSecrets, which is per-session). Unlike a secret rotation, config is
// visible to a live agent only through the read-only parent-dir mount and the
// `current` symlink, so the update is: read the container's live MCS label,
// re-materialize (which unpacks the new version, chcon's it into that label, and
// flips `current`), and Reload the agent so it re-reads current — but only when
// the version changed, since Reload interrupts the agent mid-turn.
//
// The pass snapshots the live set under h.mu, releases the lock, then does the
// per-container slow work (a `podman inspect` exec, a fetch+unpack, a Reload)
// unlocked — never holding h.mu across an exec/network call. A per-session
// error (MountLabel, Materialize, or Reload) is logged and swallowed so one bad
// container never blocks the rest of the fleet; the pass always completes. A
// container's tracked version advances only after its Reload SUCCEEDS (or when
// the version was already current), so a swallowed Reload failure leaves the
// version unmoved and the next signal retries that container — the "will retry
// on next signal" the error log promises is real, not aspirational.
//
// The fetch is per-container, not per-fleet: each configMaterializerFor issues
// its own FetchAgentConfig, so one pass makes N round-trips for one fleet bundle
// and is not atomic across the fleet — a bundle bump mid-pass can hand two
// containers different versions within the same pass. Each container is still
// self-consistent and the next coalesced signal reconciles the laggard; the
// N-fetch cost is the accepted MVP trade (fleet sizes are small), matching the
// per-provision re-fetch the config-delivery design already accepts.
//
// Reload here runs off the config worker goroutine, so this pass is a SECOND
// source of a lifecycle op against a session, concurrent with the dispatch
// receive loop. That weakens the "only one lifecycle op is ever in flight"
// MVP invariant Reload/Start document (host.go — sequential dispatch +
// single-shot Run): a ConfigVersion Reload can now overlap a dispatch-driven
// Stop/Remove/Reload of the same session in the window each holds h.mu open.
// Accepted for the MVP (ruled 2026-08-03): the Server does not issue a
// ConfigVersion pass and a per-session Stop/Reload for the same session
// concurrently, and the per-session transition lock that closes the window is
// the same T9 work the Reload/Start comments already defer to.
func (h *agentHost) RefreshConfig(ctx context.Context) error {
	type target struct {
		sessionID     string
		containerName string
		containerID   runtime.WorkloadID
		lastVersion   string
	}
	h.mu.Lock()
	targets := make([]target, 0, len(h.sessions))
	for _, s := range h.sessions {
		targets = append(targets, target{
			sessionID:     s.sessionID,
			containerName: s.containerName,
			containerID:   s.containerID,
			lastVersion:   h.configVersions[s.containerName],
		})
	}
	h.mu.Unlock()

	for _, t := range targets {
		// Take THIS container's transition lock for the whole leg — MountLabel +
		// Materialize + relaunch run as one critical section. Calls reloadLocked, NOT
		// public Reload (which would re-take this non-reentrant lock and self-deadlock).
		if err := h.refreshOneContainer(ctx, t.sessionID, t.containerName, t.containerID, t.lastVersion); err != nil {
			continue
		}
	}
	// Always nil today: every per-container fault is swallowed above. The error
	// return is reserved for a future fleet-level fault (see the interface doc).
	return nil
}

// statusOf stamps a live session with the tier and egress posture of the
// backend this Runner resolved. Both are Runner-wide, not per-session: the
// Runner reports them because it is the component that picked the backend, so a
// client never has to infer containment from a tier name.
func (h *agentHost) statusOf(s *liveSession) *compassv1.AgentSessionStatus {
	return &compassv1.AgentSessionStatus{
		SessionId:      s.sessionID,
		State:          s.state,
		AgentAccountId: s.agentAccountID,
		RuntimeTier:    runtimeTierProto(runtime.TierOf(h.engine)),
		EgressPosture:  egressPostureProto(h.runtime.EgressPosture()),
	}
}

// provisionVsockGateway is Provision's microVM leg: launch with NO agent-socket or
// config mount (§(c)/§(f)), resolve the per-session host gateway path, and serve the
// same handler there (gateway.Serve, §(b)). A Serve failure after Launch tears the
// session down; the config-version seed is skipped, so RefreshConfig skips it (§(f)).
func (h *agentHost) provisionVsockGateway(ctx context.Context, spec runtime.AgentSpec, engine vsockGatewayEngine) (string, error) {
	handle, err := h.runtime.Launch(ctx, spec)
	if err != nil {
		return "", err
	}
	name := handle.Name()
	endpoint, ok := engine.AgentGatewayEndpoint(name)
	if !ok {
		h.teardownContainer(ctx, name)
		return "", fmt.Errorf("resolving vsock gateway endpoint for container %q: backend reports no session", name)
	}
	deps := gateway.Deps{Sessions: h, Relay: h.link.client, Lifecycle: h.link.client, Events: h.link.client, Committer: h.link.client, Forge: h.link.client}
	h.log.InfoContext(ctx, "serving agent gateway over vsock path",
		slog.String("container", name), slog.String("path", endpoint))
	listener, err := gateway.Serve(ctx, endpoint, name, deps)
	if err != nil {
		// The gateway never came up; tear the launched session down so no VM
		// outlives a session with no reachable Runner.
		h.teardownContainer(ctx, name)
		return "", fmt.Errorf("serving agent gateway for container %q at %q: %w", name, endpoint, err)
	}
	h.mu.Lock()
	h.sockets[name] = listener
	h.mu.Unlock()
	return name, nil
}

// provisionHostGateway is Provision's host-process leg: the agent runs as a direct
// host child with NO bind mounts, so its socket and config tree live in the handle's
// private 0700 state dir, threaded as env vars. Like the vsock leg it inverts order.
// A serve/materialize failure after Launch tears both socket and container down.
func (h *agentHost) provisionHostGateway(ctx context.Context, spec runtime.AgentSpec, engine hostStateEngine) (string, error) {
	handle, err := h.runtime.Launch(ctx, spec)
	if err != nil {
		return "", err
	}
	name := handle.Name()
	stateDir, ok := engine.AgentStateDir(handle.ID())
	if !ok {
		h.teardownContainer(ctx, name)
		return "", fmt.Errorf("resolving host state dir for container %q: backend reports no handle", name)
	}
	// The socket lands in the handle's 0700 socket subdir (Create mints it); the config
	// tree is rooted under the same state dir (root 0755 so the agent can traverse it,
	// the 0700 parent keeps it private). Record both paths BEFORE materializing.
	transport := hostAgentTransport{
		socketPath: filepath.Join(stateDir, "socket", agentSocketFile),
		configRoot: filepath.Join(stateDir, "config"),
	}
	h.mu.Lock()
	h.hostTransports[name] = transport
	h.mu.Unlock()
	if _, err := h.serveSocketAt(ctx, name, transport.socketPath); err != nil {
		// The socket never came up; forget the transport (closeSocket) and tear
		// the launched container down so no agent runs with no reachable Runner.
		h.closeSocket(ctx, name)
		h.teardownContainer(ctx, name)
		return "", err
	}
	// Materialize the fleet config into the state-dir root. mcsLabel empty: no
	// container, no MCS category (host MountLabel returns ""), so Materialize skips
	// chcon and the agent reads the tree as the same uid that wrote it.
	mount, err := h.configMaterializerFor(name).Materialize(ctx, "")
	if err != nil {
		// Config could not be materialized: tear the socket down (mirror the
		// container leg's Launch-failure cleanup) so it does not leak, and tear
		// the launched container down so no agent comes up with no config.
		h.closeSocket(ctx, name)
		h.teardownContainer(ctx, name)
		return "", fmt.Errorf("materializing agent config: %w", err)
	}
	h.mu.Lock()
	h.configVersions[name] = mount.Version
	h.mu.Unlock()
	return name, nil
}

// teardownContainer tears a just-launched container down through runtime Teardown
// (the exact leg Remove uses) for provision-failure cleanup. A resolve miss or
// teardown error is logged, not returned — the caller returns the provision error,
// and a best-effort teardown matches Remove's posture.
func (h *agentHost) teardownContainer(ctx context.Context, containerName string) {
	handle, ok := h.registry.Resolve(containerName)
	if !ok || handle == nil {
		return
	}
	if err := h.runtime.Teardown(ctx, handle); err != nil {
		h.log.Warn("tearing down container after failed vsock gateway provision",
			slog.String("container", containerName), slog.Any("error", err))
	}
}

// refreshOneContainer runs one container's config-update leg under its transition
// lock: read the live MCS label, re-materialize, reloadLocked iff the version moved.
// A per-container fault is logged and returned so the caller skips it without aborting
// the fleet; the tracked version advances only after a successful reload.
func (h *agentHost) refreshOneContainer(ctx context.Context, sessionID, containerName string, containerID runtime.WorkloadID, lastVersion string) error {
	unlock := h.lockContainer(containerName)
	defer unlock()

	// A vsock-gateway backend serves config through no mount this refresh can touch:
	// Provision skipped the config materialize+mount (§(f)), so h.configVersions was
	// never seeded and every pass would churn the session mid-turn delivering nothing.
	// Skip until the config-delivery slice gives the refresh something real (§(f), OQ-3).
	if _, ok := h.engine.(vsockGatewayEngine); ok {
		return nil
	}

	mcsLabel, err := h.engine.MountLabel(ctx, containerID)
	if err != nil {
		h.log.ErrorContext(ctx, "reading container mount label on ConfigVersion signal failed; skipping container this pass",
			slog.String("container", containerName), slog.Any("error", err))
		return err
	}
	mount, err := h.configMaterializerFor(containerName).Materialize(ctx, mcsLabel)
	if err != nil {
		h.log.ErrorContext(ctx, "re-materializing agent config on ConfigVersion signal failed; will retry on next signal",
			slog.String("container", containerName), slog.Any("error", err))
		return err
	}
	if mount.Version == lastVersion {
		// Version unchanged: the re-materialize was idempotent and the agent
		// already reads this config. Do not Reload — it would interrupt the
		// agent mid-turn for no config change. The tracked version already
		// matches, so there is nothing to record.
		return nil
	}
	if err := h.reloadLocked(ctx, sessionID); err != nil {
		// Reload failed: do NOT advance the tracked version, so the next signal still
		// sees a change and retries. Materialize already flipped `current` on disk
		// (idempotently), so the retry re-materializes for free and re-runs Reload.
		h.log.ErrorContext(ctx, "reloading agent after config update failed; will retry on next signal",
			slog.String("container", containerName), slog.String("session_id", sessionID), slog.Any("error", err))
		return err
	}
	// Reload succeeded: record the version now so a same-version signal does not
	// Reload again. Materialize re-flips `current` every pass, so tracking the version
	// only after a successful Reload — not the on-disk state — gates the interrupt.
	h.mu.Lock()
	h.configVersions[containerName] = mount.Version
	h.mu.Unlock()
	return nil
}

// lockContainer acquires the per-container transition lock for name, creating it on
// first use, and returns the unlock. Get-or-create touches h.mu only briefly (never
// a container lock while holding h.mu, nor the reverse). The entry is never removed,
// so resolving callers always contend on the same instance.
func (h *agentHost) lockContainer(name string) (unlock func()) {
	h.mu.Lock()
	lock, ok := h.containerLocks[name]
	if !ok {
		lock = &sync.Mutex{}
		h.containerLocks[name] = lock
	}
	h.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// reloadLocked performs the in-place agent relaunch assuming the caller holds the
// session's container transition lock. It re-resolves the session and handle (a
// session dropped since the lock was taken is a true no-op), runs Stop + StartAgent,
// then swaps the stream under h.mu. Callers: Reload (locks) and refreshOneContainer.
func (h *agentHost) reloadLocked(ctx context.Context, sessionID string) error {
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	h.mu.Unlock()
	if !ok {
		return errSessionUnknown
	}
	// Re-resolve the handle BEFORE stopping, so the relaunch carries the same identity
	// the original Start did and a session whose container was dropped is rejected
	// while its agent still runs — a true no-op, never a stopped agent left behind a
	// session the live set still reports READY.
	handle, ok := h.registry.Resolve(s.containerName)
	if !ok {
		return errSessionUnknown
	}
	if handle == nil {
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

// agentEnv derives the agent exec's identity and configuration from the
// launched container's handle, so Start and Reload cannot drift apart. The
// model is Runner-wide config; everything else is per-container.
func (h *agentHost) agentEnv(handle *runtime.AgentHandle) AgentEnv {
	env := AgentEnv{
		UID:     handle.WorkspaceUID(),
		HomeDir: handle.HomeDir(),
		Workdir: handle.CheckoutDir(),
		Model:   h.model,
		Persona: handle.Persona(),
		Role:    handle.Role(),
	}
	// On the host tier the socket and config live in the handle's state dir, not at
	// the frozen /run/compass paths (no mounts). Thread those overrides so the agent
	// dials/reads where the host leg served them. Absent for container tiers, which
	// resolve the frozen defaults.
	h.mu.Lock()
	transport, ok := h.hostTransports[handle.Name()]
	h.mu.Unlock()
	if ok {
		env.SocketPath = transport.socketPath
		env.ConfigMountPath = transport.configRoot
	}
	return env
}

// configMaterializerFor builds a ConfigMaterializer rooted at the container's own
// config subtree, mirroring the per-container agent-socket layout. A per-container
// root is required because every bind mount is relabeled into the container's PRIVATE
// SELinux MCS category (:Z); a shared root would be re-stolen by each new relabel.
func (h *agentHost) configMaterializerFor(containerName string) *ConfigMaterializer {
	// On the host tier the config tree lives in the handle's state dir (recorded in
	// hostTransports), not under RuntimeDir/containers — no mounts, so a refresh must
	// re-materialize where the agent reads. Absent an entry (container tiers), root at
	// the per-container RuntimeDir subtree as before.
	h.mu.Lock()
	transport, ok := h.hostTransports[containerName]
	h.mu.Unlock()
	if ok {
		return NewConfigMaterializer(transport.configRoot, h.link, h.log)
	}
	return NewConfigMaterializer(filepath.Join(h.runtimeDir, agentSocketDir, containerName, "config"), h.link, h.log)
}

// serveSocket creates and serves the per-container agent socket for containerName,
// recording the listener so Provision can mount it and teardown can Close it. A
// container already serving is a no-op (idempotent retry). The Gateway forwards to
// the Server over the Runner's own RunnerService client, resolving container→session.
func (h *agentHost) serveSocket(ctx context.Context, containerName string) (*gateway.SocketListener, error) {
	return h.serveSocketAt(ctx, containerName, filepath.Join(h.runtimeDir, agentSocketDir, containerName, agentSocketFile))
}

// serveSocketAt is serveSocket with an explicit socket path: container tiers pass the
// fixed RuntimeDir socket, the host tier a path in the handle's state dir (no mount).
// Idempotency is identical: a container already serving keeps its live listener.
func (h *agentHost) serveSocketAt(ctx context.Context, containerName, path string) (*gateway.SocketListener, error) {
	h.mu.Lock()
	if listener, served := h.sockets[containerName]; served {
		h.mu.Unlock()
		return listener, nil
	}
	h.mu.Unlock()
	listener, err := gateway.Serve(ctx, path, containerName, gateway.Deps{Sessions: h, Relay: h.link.client, Lifecycle: h.link.client, Events: h.link.client, Committer: h.link.client, Forge: h.link.client})
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
	// Forget the host-tier transport paths alongside the socket: they are the
	// same per-container lifetime, so a re-Provision re-records fresh ones.
	delete(h.hostTransports, containerName)
	h.mu.Unlock()
	if !ok {
		return
	}
	if err := listener.Close(ctx); err != nil {
		h.log.Warn("closing agent socket", slog.String("container", containerName), slog.Any("error", err))
	}
}

// randomIDs returns a session-id minter backed by the OS CSPRNG. The sess-
// prefix preserves the operator-facing session shape; hex encoding keeps the
// id safe as a path element and fabric subject while making separate Runner
// lifetimes overwhelmingly unlikely to collide.
func randomIDs() func() string {
	return func() string {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			// crypto/rand.Read never returns an error on the supported platforms
			// (it reads from the OS RNG); a failure here means the OS entropy source
			// is unavailable, which is unrecoverable for a session ID.
			panic("runner: OS RNG for a fresh session ID: " + err.Error())
		}
		return "sess-" + hex.EncodeToString(b[:])
	}
}
