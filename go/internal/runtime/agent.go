// The agent lifecycle façade the Runner drives (compass.md §5.3, §7.1): build
// the image, create and start the container, arm the egress firewall as root,
// install scoped credentials, create the agent's checkout dir as the unprivileged
// agent user, and tear it all down. Composed from the podman, egress, and
// workspace pieces of this package.
//
// Deliberately stateless about container existence: the container engine is the
// source of truth for what exists, so there's no in-memory registry to keep in
// sync with reality for lifecycle decisions. Each call resolves the container by
// its stable name. The optional AgentRegistry is a separate concern — a handle
// cache the session RPCs resolve through, not lifecycle state.

package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

// capNetAdmin is the capability the container is granted so its entrypoint can
// arm the egress firewall. The agent user itself holds no capabilities.
const capNetAdmin = "NET_ADMIN"

// stopTimeout is how long to wait for graceful container stop before podman
// kills it.
const stopTimeout = 10 * time.Second

// AgentSpec is everything needed to bring one agent workstream online.
type AgentSpec struct {
	// Name is the stable container name — the Runner's handle for this
	// workstream.
	Name string
	// Image is the agent base image ref, supplied by the Runner's config
	// (--image / $COMPASS_AGENT_IMAGE) — never a per-repo build result.
	Image string
	// Workspace is the per-agent clone + scoped $HOME.
	Workspace Workspace
	// Egress is default-deny egress with this allowlist.
	Egress EgressPolicy
	// Mounts is read-only host mounts (e.g. a host cache mounted read-only).
	Mounts []Mount
	// Persona is the server-authoritative identity overlay for this agent,
	// appended to the agent's system prompt at boot. Empty means no overlay.
	Persona string
	// Role is the server-authoritative operator-set block-0 selector for this
	// agent, delivered as the container's customSystemPrompt at boot. Empty
	// means no role (default OMP block-0).
	Role string
	// AgentAccountID is the owned agent account this workstream belongs to (the
	// AgentAccount id from the provision request). Carried onto the spec so the
	// Runner can attribute a session's AgentSessionStatus to its account (DL-167)
	// without parsing it back out of the derived container name.
	AgentAccountID string
}

// AgentHandle is a live agent container: the resolved id plus the spec it was
// created from, so callers can exec as the agent user without re-deriving the
// workspace.
type AgentHandle struct {
	id   ContainerID
	spec AgentSpec
}

// ID returns the resolved container id.
func (h *AgentHandle) ID() ContainerID { return h.id }

// Name returns the stable container name.
func (h *AgentHandle) Name() string { return h.spec.Name }

// WorkspaceUID returns the agent user's uid — the --user for exec-ing agent
// work.
func (h *AgentHandle) WorkspaceUID() uint32 { return h.spec.Workspace.UID }

// CheckoutDir returns the in-container checkout directory (the agent session's
// cwd).
func (h *AgentHandle) CheckoutDir() string { return h.spec.Workspace.CheckoutDir }

// HomeDir returns the agent's scoped $HOME.
func (h *AgentHandle) HomeDir() string { return h.spec.Workspace.HomeDir }

// Persona returns the server-authoritative identity overlay for this agent, or
// empty for none.
func (h *AgentHandle) Persona() string { return h.spec.Persona }

// Role returns the server-authoritative operator-set block-0 selector for this
// agent, or empty for none.
func (h *AgentHandle) Role() string { return h.spec.Role }

// AgentAccountID returns the owned agent account this workstream belongs to,
// the attribution key the Runner stamps onto each session's status (DL-167).
func (h *AgentHandle) AgentAccountID() string { return h.spec.AgentAccountID }

// StageError wraps a container runtime error with the lifecycle stage it
// failed at, so a failure is diagnosable without a container inspect.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("container runtime error while %s: %v", e.Stage, e.Err)
}

// Unwrap exposes the underlying runtime error for errors.Is/As.
func (e *StageError) Unwrap() error { return e.Err }

// InContainerError is an in-container exec that ran but exited non-zero, tagged
// with the lifecycle stage and carrying the captured stderr.
type InContainerError struct {
	Stage    string
	ExitCode int
	Stderr   string
}

func (e *InContainerError) Error() string {
	return fmt.Sprintf("%s failed inside the container (exit %d): %s", e.Stage, e.ExitCode, e.Stderr)
}

// InvalidConfigError is an agent configuration the lifecycle rejected before
// touching the container (e.g. a credential host that isn't a DNS name or IP
// literal).
type InvalidConfigError struct {
	Err error
}

func (e *InvalidConfigError) Error() string {
	return fmt.Sprintf("invalid agent configuration: %v", e.Err)
}

// Unwrap exposes the underlying config error for errors.Is/As.
func (e *InvalidConfigError) Unwrap() error { return e.Err }

// atStage tags a container runtime error with the lifecycle stage it failed at.
func atStage(stage string, err error) error {
	return &StageError{Stage: stage, Err: err}
}

// requireSuccess turns a non-zero in-container exec into an InContainerError
// tagged with the stage, surfacing its captured stderr.
func requireSuccess(stage string, out ExecOutput) error {
	if out.Success() {
		return nil
	}
	return &InContainerError{Stage: stage, ExitCode: out.ExitCode, Stderr: out.Stderr}
}

// AgentRuntime drives the per-agent container lifecycle over a ContainerRuntime.
//
// When constructed with an AgentRegistry via NewAgentRuntimeWithRegistry, a
// successful Launch registers the handle and Teardown deregisters it, so the
// Runner's session RPCs can resolve a launched container by name.
type AgentRuntime struct {
	runtime  ContainerRuntime
	registry *AgentRegistry
}

// NewAgentRuntime builds a lifecycle façade with no registry: Launch/Teardown
// manage containers but register nothing.
func NewAgentRuntime(runtime ContainerRuntime) *AgentRuntime {
	return &AgentRuntime{runtime: runtime}
}

// NewAgentRuntimeWithRegistry builds a façade that registers each launched
// handle in registry so StartAgentSession can resolve the container by name, and
// deregisters it on teardown.
func NewAgentRuntimeWithRegistry(runtime ContainerRuntime, registry *AgentRegistry) *AgentRuntime {
	return &AgentRuntime{runtime: runtime, registry: registry}
}

// Launch brings an agent online: create + start the container, arm egress as
// root, install scoped credentials, and create the checkout dir as the agent
// user. On any failure after the container exists, the partial container is
// removed so a retry starts clean rather than colliding on the name.
func (r *AgentRuntime) Launch(ctx context.Context, spec AgentSpec) (*AgentHandle, error) {
	id, err := r.createAndStart(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := r.provision(ctx, id, spec); err != nil {
		// Best-effort cleanup: the launch already failed, so a remove error must
		// not mask the original cause. Detach cancellation (WithoutCancel) so the
		// cleanup still runs when the caller's context is already cancelled — the
		// per-command timeout inside the runtime still bounds it, and a leaked
		// container would otherwise collide with the next launch of the name.
		_ = r.runtime.Remove(context.WithoutCancel(ctx), id)
		return nil, err
	}
	handle := &AgentHandle{id: id, spec: spec}
	// Register so the Runner's session RPCs can resolve this container by name
	// (no-op when constructed without a registry).
	if r.registry != nil {
		r.registry.Register(handle)
	}
	return handle, nil
}

// ExecAsAgent runs a command as the agent user inside a live container (the
// Runner's handle for exec-ing agent work / probing state).
func (r *AgentRuntime) ExecAsAgent(ctx context.Context, handle *AgentHandle, command ...string) (ExecOutput, error) {
	spec := NewExecSpec(command...).
		AsUser(strconv.FormatUint(uint64(handle.spec.Workspace.UID), 10)).
		InDir(handle.spec.Workspace.CheckoutDir)
	spec.Env["HOME"] = handle.spec.Workspace.HomeDir
	out, err := r.runtime.Exec(ctx, handle.id, spec)
	if err != nil {
		return ExecOutput{}, atStage("exec", err)
	}
	return out, nil
}

// Teardown stops and removes an agent container, releasing the workstream's
// isolation.
func (r *AgentRuntime) Teardown(ctx context.Context, handle *AgentHandle) error {
	if err := r.runtime.Stop(ctx, handle.id, stopTimeout); err != nil {
		return atStage("stop", err)
	}
	if err := r.runtime.Remove(ctx, handle.id); err != nil {
		return atStage("remove", err)
	}
	// Deregister LAST — only after the container is stopped and removed. A
	// Teardown that fails partway leaves the handle resolvable, so the caller's
	// idempotency gate (agentHost.Remove resolves the handle before tearing
	// down) re-runs teardown on retry rather than answering a lying success over
	// a container that leaked. Deregister-first would orphan the container: the
	// handle would be gone but the engine container still present and now
	// unreachable. The "stop resolving an about-to-be-gone container" window is
	// moot under the sequential dispatch that is the only registry resolver.
	// No-op without a registry.
	if r.registry != nil {
		r.registry.Deregister(handle.Name())
	}
	return nil
}

// WriteAgentFile materializes a Runner-supplied file into the agent's $HOME as
// the agent user, creating parent dirs. Two invariants match every sibling
// materializer in this package (installCredentials, the secrets materializers):
// the body is fed over stdin, never argv — argv is visible in the container's
// process list while stdin is not; and the file lands 0600 under umask 077 (the
// created dir 0700), because a reconstructed session transcript is as sensitive
// as the aggregate env file. The path components are positional args to a fixed
// sh script, never interpolated into the script text, so a crafted path cannot
// inject shell.
func (r *AgentRuntime) WriteAgentFile(ctx context.Context, id ContainerID, uid uint32, homeDir, relPath, body string) error {
	script := `set -eu; umask 077; dir=$(dirname "$1"); mkdir -p "$dir"; cat > "$1"; chmod 600 "$1"`
	spec := NewExecSpec("sh", "-c", script, "sh", filepath.Join(homeDir, relPath)).
		AsUser(strconv.FormatUint(uint64(uid), 10)).
		InDir(homeDir).
		WithStdin(body)
	out, err := r.runtime.Exec(ctx, id, spec)
	if err != nil {
		return atStage("write agent file", err)
	}
	return requireSuccess("write agent file", out)
}

// createAndStart creates then starts the container, cleaning up a created but
// unstarted container so a retry with the same name starts clean.
func (r *AgentRuntime) createAndStart(ctx context.Context, spec AgentSpec) (ContainerID, error) {
	container := ContainerSpec{
		Image:  spec.Image,
		Name:   spec.Name,
		CapAdd: []string{capNetAdmin},
		Mounts: spec.Mounts,
		UID:    spec.Workspace.UID,
		// Keep the container alive so the Runner can exec into it; the agent is
		// driven via exec, not as the container's main process.
		Command: []string{"sleep", "infinity"},
	}

	id, err := r.runtime.Create(ctx, container)
	if err != nil {
		return "", atStage("create", err)
	}
	// `create` succeeded, so a `start` failure leaves a container behind. Remove
	// it (best-effort) so a retry with the same name starts clean rather than
	// colliding on an orphan.
	if err := r.runtime.Start(ctx, id); err != nil {
		_ = r.runtime.Remove(context.WithoutCancel(ctx), id)
		return "", atStage("start", err)
	}
	return id, nil
}

// provision runs the post-start steps, all inside the running container:
// firewall (root), credentials (agent user), checkout dir (agent user).
func (r *AgentRuntime) provision(ctx context.Context, id ContainerID, spec AgentSpec) error {
	if err := r.armEgress(ctx, id, spec.Egress); err != nil {
		return err
	}
	if err := r.installCredentials(ctx, id, spec.Workspace); err != nil {
		return err
	}
	return r.ensureCheckoutDir(ctx, id, spec.Workspace)
}

// armEgress arms the egress firewall as the image's default user (uid 1000)
// with CAP_NET_ADMIN. After this, an agent exec — run as the agent uid with no
// capabilities — cannot alter the ruleset.
func (r *AgentRuntime) armEgress(ctx context.Context, id ContainerID, egress EgressPolicy) error {
	out, err := r.runtime.Exec(ctx, id, NewExecSpec("sh", "-c", egress.NftScript()))
	if err != nil {
		return atStage("arm egress", err)
	}
	return requireSuccess("arm egress", out)
}

// installCredentials installs the scoped git credential helper into the agent's
// $HOME, as the agent user. A no-op when the workspace has no credentials.
func (r *AgentRuntime) installCredentials(ctx context.Context, id ContainerID, workspace Workspace) error {
	script, err := workspace.CredentialSetupScript()
	if err != nil {
		return &InvalidConfigError{Err: err}
	}
	if script == "" {
		return nil
	}
	// Feed the script over stdin to `sh -s`, never `sh -c <script>`: the token is
	// in the script body, and argv is visible in the container's process list
	// while stdin is not.
	spec := NewExecSpec("sh", "-s").
		AsUser(strconv.FormatUint(uint64(workspace.UID), 10)).
		InDir(workspace.HomeDir).
		WithStdin(script)
	out, err := r.runtime.Exec(ctx, id, spec)
	if err != nil {
		return atStage("install credentials", err)
	}
	return requireSuccess("install credentials", out)
}

// ensureCheckoutDir creates the in-container checkout directory as the agent
// user, so an agent that self-clones post-launch has an owned working dir. Run
// as the agent uid (not root) so the directory is owned by the agent. Its
// precondition: CheckoutDir's parent must be writable by the agent uid.
func (r *AgentRuntime) ensureCheckoutDir(ctx context.Context, id ContainerID, workspace Workspace) error {
	spec := NewExecSpec("mkdir", "-p", workspace.CheckoutDir).
		AsUser(strconv.FormatUint(uint64(workspace.UID), 10))
	out, err := r.runtime.Exec(ctx, id, spec)
	if err != nil {
		return atStage("create checkout dir", err)
	}
	return requireSuccess("create checkout dir", out)
}
