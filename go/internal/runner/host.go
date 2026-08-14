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
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
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

	mu           sync.Mutex
	sessions     map[string]*liveSession
	sockets      map[string]*gateway.SocketListener
	nextID       func() string
	materializer *runtime.SecretMaterializer
	// configVersions is the last config bundle version materialized into each
	// container's per-container root, keyed by container name — the
	// last-materialized-version tracking the ConfigVersion update path compares
	// against so it only Reloads an agent when the version actually moved.
	// Recorded at Provision (initial materialize) and, on a RefreshConfig pass,
	// only after the agent's Reload succeeds — so a swallowed Reload failure
	// leaves it unmoved and the next signal retries that container.
	// Keyed by container, not session: config lifecycle is container-scoped
	// (SEA-1659 per-container roots), and it must survive a Reload (which reuses
	// the session but keeps the container).
	configVersions map[string]string
	// containerLocks serializes the state-transitioning lifecycle ops per
	// container name (Provision/Start/Stop/Remove/Reload), so concurrent dispatch
	// (per-command goroutines) cannot interleave two transitions on one container
	// — closing the Start TOCTOU documented below and linearizing Stop/Reload/
	// Remove on a container. Guarded by h.mu for get-or-create ONLY; entries are
	// NEVER deleted (not even by Remove), which is load-bearing: a deleted entry
	// could race a concurrent op that already resolved the same *sync.Mutex, so
	// retaining it is what makes the resolve-then-lock protocol safe. Growth is
	// bounded by distinct container names ever provisioned — the same retention
	// class as handled/configVersions, and T9's bounded-eviction (SEA-1328)
	// covers them together. Status deliberately does NOT lock (it answers from
	// the session set under h.mu), so it never queues behind a slow Provision.
	// See docs/designs/platform/compass-runner-concurrent-dispatch/design.md.
	containerLocks map[string]*sync.Mutex
}

// liveSession is one running agent session: its container and the relay stream
// pumping its frames up PublishEvents.
type liveSession struct {
	sessionID     string
	containerName string
	containerID   runtime.ContainerID
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
// own config. newID mints session ids; nil uses a monotonic counter.
func NewSessionHost(link *ServerLink, rt *runtime.AgentRuntime, registry *runtime.AgentRegistry, engine runtime.ContainerRuntime, specs SpecBuilder, cfg AgentHostConfig, log *slog.Logger, newID func() string) SessionHost {
	if log == nil {
		log = slog.Default()
	}
	if newID == nil {
		newID = monotonicIDs()
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
	// Serialize all transitions on this container: a concurrent Remove/Start of
	// the same name cannot interleave with this provision. Resolved from the spec
	// name (the stable lifecycle key).
	unlock := h.lockContainer(spec.Name)
	defer unlock()
	listener, err := h.serveSocket(ctx, spec.Name)
	if err != nil {
		return "", err
	}
	spec.Mounts = append(spec.Mounts, listener.Mount(agentSocketMountPath))
	// Materialize the fleet config into a host tree and bind-mount it read-only.
	// The mount target is the PARENT config dir (not the resolved version dir),
	// so a later `current/` symlink flip by an update becomes visible inside the
	// live container with no remount. It is read-only because the agent only
	// reads config, never writes it. The mcsLabel is empty on this provision
	// path: Materialize runs before the container exists, so there is no MCS
	// category to target — the create-time :Z relabel covers the whole tree.
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
	// existing-session check releases h.mu before the slow StartAgent below and
	// re-acquires it to record, so two concurrent Starts for one container could
	// both pass the check in that window (a TOCTOU minting a duplicate session).
	// The per-container transition lock closes it: the second Start blocks here
	// until the first records its session, then sees it and returns
	// errAlreadyRunning. Concurrent dispatch (per-command goroutines) made this
	// reachable; T9's in-process reattach (SEA-1328) consumes the same lock — do
	// not reintroduce it. See docs/designs/platform/compass-runner-concurrent-dispatch/design.md.
	unlock := h.lockContainer(name)
	defer unlock()

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

	// Materialize the agent's secrets into the container BEFORE exec'ing the
	// agent, so its first provider/gh/env read never races an empty seed
	// (SEA-1327 T5: materialize before the agent runs). The fetch authorizes on
	// the container→account binding the Server recorded at Provision — not the
	// session, which is only being minted now — so it is FetchSecretsByContainer,
	// keyed on the container name. The frozen record placed this in the
	// provision step, but the Server binds the container→account entry only
	// after the relay-Provision returns, so the earliest point the by-container
	// fetch is authorizable is here in Start, still strictly before StartAgent.
	//
	// A Server built with NO secrets surface (nil resolver) answers
	// FetchSecrets with CodeFailedPrecondition — a legitimate deployment, not a
	// failure: such a server has no secrets to inject, so the agent must still
	// start. That one code is tolerated (skip the materialize, start the agent).
	// It is deliberately distinct from CodeUnavailable: connect-go synthesizes
	// CodeUnavailable for a transient transport fault (conn reset, GOAWAY, server
	// restart), and a blip on this fetch against a Server that DOES have secrets
	// must NOT be read as "no secrets" — the agent would otherwise come up
	// silently missing credentials that exist. So every other fetch error — a
	// transient Unavailable, an authz denial — fails the Start. Rotation (T6)
	// rides the async SecretsVersion signal.
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

	// On an authorized resume, materialize the server-reconstructed session
	// file into the container BEFORE exec'ing the agent, so the agent's first
	// read finds it (SEA-1570 T8). The absolute in-container path is exported to
	// the agent as COMPASS_RESUME_SESSION_FILE. A fresh (non-resume) start does
	// nothing here. The discriminator is a non-empty resume_session_id.
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
			// An authorized id with no reconstructed body is a Server-side skew
			// (authz passed but reconstruction produced nothing); materialize the
			// empty file per the id-is-the-discriminator contract, but surface it
			// so the reconstruction bug is visible here, not only as an agent that
			// resumes from an empty transcript.
			h.log.Warn("resume_session_id set with empty body; materializing empty resume file", "container", name, "resume_session_id", id)
		}
		h.log.Info("materializing resume session file", "container", name, "resume_session_id", id)
		if err := h.runtime.WriteAgentFile(ctx, handle.ID(), handle.WorkspaceUID(), handle.HomeDir(), relPath, resumeBody); err != nil {
			return "", fmt.Errorf("materializing resume session file for container %q: %w", name, err)
		}
		env.ResumeSessionFile = filepath.Join(handle.HomeDir(), relPath)
	} else if resumeBody != "" {
		// The inverse skew: a Server sent a reconstructed body with no
		// resume_session_id. The id is the sole discriminator, so this start is
		// treated as fresh and the body is dropped — surface it so the skew is
		// visible symmetrically with the empty-body warning above, rather than an
		// agent silently starting without the transcript the Server tried to send.
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
	// Create the session's control state here, under the same lock that records
	// the session — the mirror of Stop's retirement. The Runner owning both ends
	// is what lets the agent-driven paths refuse an id they do not know: an
	// agent that subscribes or acks against a session the lifecycle never bound
	// (or already retired) is turned away, instead of minting state nothing
	// would ever reclaim.
	listener, served := h.sockets[name]
	if served {
		listener.BindSession(sessionID)
	}
	h.mu.Unlock()

	// On a fresh (non-resume) start, lift the agent's replay barrier so the
	// first idle-deliver that starts the agent's turn is dispatched rather than
	// refused: the barrier defaults closed and only the arrival of
	// replay_complete lifts it. A resume start never sends it — the restart
	// replay path drives the barrier there. Sent after the h.mu release,
	// mirroring Deliver's resolve-under-lock / send-outside discipline; the
	// per-container transition lock held across Start guarantees no Stop/Retire
	// races between the bind above and this send. See
	// docs/designs/product/compass-system-sender-first-turn/design.md.
	if served && req.GetResumeSessionId() == "" {
		op := &compassv1internal.AgentControl{
			Control: &compassv1internal.AgentControl_ReplayComplete{
				ReplayComplete: &compassv1internal.ReplayComplete{},
			},
		}
		if err := listener.SendControl(sessionID, op); err != nil {
			// A served listener always has a wired producer (gateway.Serve wires
			// it), so this is an unreachable wiring fault in production, not a
			// reason to fail an already-recorded Start (recorded => success). Log
			// and continue, matching Start's other degraded-posture logging.
			h.log.Error("sending fresh-start replay_complete", slog.String("container", name), slog.String("session_id", sessionID), slog.Any("error", err))
		}
	}
	return sessionID, nil
}

// Stop tears a session down. An unknown/already-stopped session succeeds
// (idempotent, matching the frozen StopAgentSession semantics).
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
	// Stop's retirement — so a concurrent lifecycle op cannot observe a
	// half-torn state. The socket is closed unconditionally below, so it is not
	// retired here (RetireSession only quiesces the control producer for a live
	// session; a container with no bound session has nothing to retire).
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
// self-deadlock). See docs/designs/platform/compass-runner-concurrent-dispatch/design.md.
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
		return []*compassv1.AgentSessionStatus{{SessionId: s.sessionID, State: s.state, AgentAccountId: s.agentAccountID}}, nil
	}
	out := make([]*compassv1.AgentSessionStatus, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, &compassv1.AgentSessionStatus{SessionId: s.sessionID, State: s.state, AgentAccountId: s.agentAccountID})
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
		containerID   runtime.ContainerID
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
		// Materialize + relaunch run as one critical section, so a concurrent
		// dispatch-driven Stop/Remove/Reload of the same container cannot
		// interleave with the config update. The leg calls reloadLocked (NOT the
		// public Reload, which would re-take this same non-reentrant lock and
		// self-deadlock the worker forever, wedging every later transition on the
		// container behind it). See docs/designs/platform/compass-runner-concurrent-dispatch/design.md.
		if err := h.refreshOneContainer(ctx, t.sessionID, t.containerName, t.containerID, t.lastVersion); err != nil {
			continue
		}
	}
	// Always nil today: every per-container fault is swallowed above. The error
	// return is reserved for a future fleet-level fault (see the interface doc).
	return nil
}

// refreshOneContainer runs one container's config-update leg under its transition
// lock: read the live MCS label, re-materialize, and reloadLocked the agent iff
// the bundle version moved. A per-container fault (MountLabel, Materialize,
// Reload) is logged and returned so the caller skips this container this pass
// without aborting the fleet; the tracked version advances only after a
// successful reload. Split out of RefreshConfig so the container lock scopes to
// exactly one leg via defer.
func (h *agentHost) refreshOneContainer(ctx context.Context, sessionID, containerName string, containerID runtime.ContainerID, lastVersion string) error {
	unlock := h.lockContainer(containerName)
	defer unlock()

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
		// Reload failed: do NOT advance the tracked version, so the next
		// ConfigVersion signal still sees a version change and retries the
		// Reload. Materialize already flipped `current` to this version on
		// disk (idempotently), so the retry re-materializes for free and
		// only re-runs the Reload.
		h.log.ErrorContext(ctx, "reloading agent after config update failed; will retry on next signal",
			slog.String("container", containerName), slog.String("session_id", sessionID), slog.Any("error", err))
		return err
	}
	// Reload succeeded: record the version now so a same-version signal does
	// not Reload the agent again. Materialize re-flips `current` on every
	// pass, so tracking the version only after a successful Reload — not the
	// on-disk state — is what gates the mid-turn interrupt.
	h.mu.Lock()
	h.configVersions[containerName] = mount.Version
	h.mu.Unlock()
	return nil
}

// lockContainer acquires the per-container transition lock for name, creating it
// on first use, and returns the unlock. The get-or-create touches h.mu only
// briefly (never a container lock while holding h.mu, and never h.mu while
// holding a container lock — no lock is ever held across a slow call or a
// stream.Send). The entry is never removed, so a caller that resolves the mutex
// and a concurrent caller always contend on the same instance.
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

// reloadLocked performs the in-place agent relaunch assuming the caller holds
// the session's container transition lock. It re-resolves the session and its
// handle (a session dropped since the lock was taken is rejected as a true
// no-op), runs the slow Stop + StartAgent, then swaps the stream under h.mu.
// Callers: Reload (the public wrapper, which locks) and RefreshConfig's
// per-container fan-out leg (which takes the container lock ITSELF so its
// MountLabel + Materialize + relaunch run as one critical section).
func (h *agentHost) reloadLocked(ctx context.Context, sessionID string) error {
	h.mu.Lock()
	s, ok := h.sessions[sessionID]
	h.mu.Unlock()
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

// agentEnv derives the agent exec's identity and configuration from the
// launched container's handle, so Start and Reload cannot drift apart. The
// model is Runner-wide config; everything else is per-container.
func (h *agentHost) agentEnv(handle *runtime.AgentHandle) AgentEnv {
	return AgentEnv{
		UID:     handle.WorkspaceUID(),
		HomeDir: handle.HomeDir(),
		Workdir: handle.CheckoutDir(),
		Model:   h.model,
		Persona: handle.Persona(),
		Role:    handle.Role(),
	}
}

// configMaterializerFor builds a ConfigMaterializer rooted at the container's
// own config subtree, <RuntimeDir>/containers/<container>/config — mirroring
// the per-container agent-socket layout (serveSocket). A per-container root is
// required because every bind mount is relabeled into the container's PRIVATE
// SELinux MCS category (:Z, podman.go mountArg); a shared root would be
// re-stolen by each new container's relabel on an enforcing host.
func (h *agentHost) configMaterializerFor(containerName string) *ConfigMaterializer {
	return NewConfigMaterializer(filepath.Join(h.runtimeDir, agentSocketDir, containerName, "config"), h.link, h.log)
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
	listener, err := gateway.Serve(ctx, path, containerName, gateway.Deps{Sessions: h, Relay: h.link.client, Lifecycle: h.link.client, Events: h.link.client, Committer: h.link.client})
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
