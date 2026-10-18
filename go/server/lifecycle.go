//go:build unix

// The agent-initiated lifecycle leg: lifecycleService implements
// runnerhub.LifecycleCaller, running the SAME provisioning paths as service.go
// with a Server-resolved caller. Fail-closed: spawn creates the peer under the
// CALLER'S OWNER; despawn is same-owner only (foreign target = CodeNotFound).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"
	"golang.org/x/sync/singleflight"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// dmOpener is the narrow Comms seam lifecycleService uses to open a
// manager-to-peer DM after spawn; *comms.Comms satisfies it.
type dmOpener interface {
	OpenDMAsAccount(ctx context.Context, account store.AccountID, req *compassv1.OpenDMRequest) (*compassv1.OpenDMResponse, error)
}

// lifecycleService handles LifecycleCaller requests with the store, runner hub,
// and DM opener used after spawn to open the manager-peer DM.
type lifecycleService struct {
	store *store.Store
	hub   *runnerhub.Hub
	// dm opens the post-spawn manager-peer DM; nil for the waker, which never
	// spawns and therefore has no channel name to return.
	dm dmOpener
	// wakeGroup coalesces concurrent WakeAgent calls per agent into one start.
	// The zero-value group needs no initialization.
	wakeGroup singleflight.Group
}

// newLifecycleService constructs the caller. Serve wires it after hub creation
// to break the hub-service cycle.
func newLifecycleService(st *store.Store, hub *runnerhub.Hub, dm dmOpener) *lifecycleService {
	return &lifecycleService{store: st, hub: hub, dm: dm}
}

// Compile-time proof lifecycleService satisfies the seam the hub delegates into.
var _ runnerhub.LifecycleCaller = (*lifecycleService)(nil)

// WakeAgent best-effort resumes an offline agent's most recent session so an
// owed mention or a subscribed deliver reaches it promptly (RIG-1641 T3,
// §Decisions OQ-7). It implements delivery.AgentWaker — void and best-effort:
// every fault is logged with outcome=failed and NEVER surfaced, so mention
// routing can never fail a post (the established "mention routing can never fail a
// post" contract, design.md:521-523).
//
// The chain, all cost-controlled:
//
//  1. Not-live pre-check: a live agent has nothing to wake — a logged no-op.
//  2. Per-agent singleflight: N concurrent wakes for one agent coalesce onto one
//     start; the coalesced callers log outcome=coalesced.
//  3. LatestSessionForAccount: a prior session → the SYSTEM-AUTHORIZED internal
//     resume (a sibling of startResumeSession that SKIPS the caller-subscriber
//     gate — the wake IS the authorization, §Decisions OQ-2, so there is no
//     caller and no RequireAgentSessionSubscriber); no prior session → a fresh
//     hub.Start; no placement → a logged no-op.
func (l *lifecycleService) WakeAgent(ctx context.Context, agent store.AccountID) {
	ctx, ok := l.wakeCtx(ctx, agent)
	if !ok {
		return
	}
	// 1. Not-live pre-check (cost control): a live agent is already awake, so
	// there is nothing to resume. Cache-only, like the delivery consumer's own
	// check: a durable row this hub never promoted may name a dead session, and
	// trusting it would drop the wake silently.
	if sessionID, live := l.hub.CachedSessionForAccount(agent); live {
		slog.InfoContext(ctx, "agent wake", "outcome", wakeOutcomeAlreadyLive, "agent_account_id", agent, "session_id", sessionID)
		return
	}

	// 2. Per-agent singleflight: the first caller runs the start, concurrent
	// callers block and share its result (shared==true), so a burst at one offline
	// agent produces exactly one resume/Start. A shared caller logs
	// outcome=coalesced so the coalescing is visible.
	outcome, _, shared := l.wakeGroup.Do(string(agent), func() (any, error) {
		return l.wakeOnce(ctx, agent), nil
	})
	if shared {
		slog.InfoContext(ctx, "agent wake", "outcome", wakeOutcomeCoalesced, "agent_account_id", agent)
		return
	}
	slog.InfoContext(ctx, "agent wake", "outcome", outcome, "agent_account_id", agent)
}

// spawnChainTimeout bounds spawn and despawn relay chains; RunnerHub calls have
// no deadline, so a stalled Runner could hang either request. Tests may shorten it.
var spawnChainTimeout = 60 * time.Second

// errCannotDespawnSelf is the in-band cause for a self-despawn: an agent cannot
// tear down its own compute out from under itself. CodeInvalidArgument.
var errCannotDespawnSelf = errors.New("cannot despawn self")

// errPeerNotFound collapses unauthorized and unknown despawn targets so callers
// cannot distinguish inaccessible peers from missing ones.
var errPeerNotFound = errors.New("peer not found")

// errHandleTaken hides whether another owner or a non-agent holds the handle.
// The caller sees the same already_exists error as for its own duplicate.
var errHandleTaken = errors.New("handle already taken")

// errCallerNotAgent is the fail-closed cause when the resolved caller does not
// resolve to an agent account. The hub only delegates for a caller it resolved
// from a live agent-session binding, so this is a wiring-invariant violation,
// never a normal outcome — CodeInternal, never a silent success.
var errCallerNotAgent = errors.New("resolved caller is not an agent account")

// SpawnAsAccount creates a peer agent owned by the caller's OWNER and brings it
// online, running the same provision->placement->start->session chain a human
// spawn takes. The new agent's owner is the caller agent's owner (F2), resolved
// from the store — never the caller itself, never admin, never a client value.
//
// Idempotency / resume on a taken handle. A handle already taken in the CALLER'S
// OWN owner namespace is not blindly an error: if that existing agent has a live
// placement, the spawn is an idempotent no-op returning the existing
// container/session (a completed-call retry); if it is UNPLACED (a spawn that
// crashed after CreateAgent, or one rolled back), it is RESUMED — re-provisioned
// and started against the existing account rather than creating a second. A
// handle owned by a DIFFERENT user is a distinct namespace and spawns a distinct
// peer (DL-271/OQ-7: two owners may each hold an agent named `compass-ux`); the
// caller never touches, resumes, or steals the other owner's agent.
//
// Shadow guard. Storage lets an agent handle overlap a user/system handle (the
// two partial-unique indexes never contend), but a spawned peer must never
// SHADOW a human or the system sender, so a spawn whose handle names an existing
// user/system account is refused CodeAlreadyExists before any create.
//
// Concurrent-spawn window. Two truly-concurrent same-handle+same-owner spawns
// bearing DISTINCT client_request_ids can both reach provisionAndStart before
// either records placement. The window is bounded to at most a redundant session
// row: NO duplicate container (the container name is derived from the accountID,
// so both target the same idempotent name) and NO authz breach (both spawns
// carry the same store-resolved owner). Serializing this further is a design
// decision tied to this PR's parked Open Question.
func (l *lifecycleService) SpawnAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, spawnChainTimeout)
	defer cancel()

	// First check in the chain, so it covers the idempotent-resume branch too.
	if !store.IsManagerRole(req.GetRole()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, store.ErrUnknownRole.Error()).WithCause(store.ErrUnknownRole)
	}
	if len(req.GetPersona()) > store.MaxPersonaBytes {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "persona is %d bytes; the limit is %d", len(req.GetPersona()), store.MaxPersonaBytes)
	}
	// Before CreateAgent: a later Provision refusal would leave the handle taken.
	if err := runnerhub.CheckClientRequestID(req.GetClientRequestId()); err != nil {
		return nil, err
	}

	// F2 ownership: the spawned peer inherits the CALLER'S OWNER, resolved from the
	// store (the caller is an agent account).
	callerOwner, err := l.store.AgentOwner(ctx, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, errCallerNotAgent.Error()).WithCause(errCallerNotAgent)
		}
		wrappedErr := fmt.Errorf("resolving caller owner: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}

	// Shadow guard (defense in depth): an agent may never be spawned onto a handle
	// a user or system account already holds. Storage permits the overlap, but a
	// peer shadowing a human's handle could be addressed in its place. Refuse with
	// the same in-band already_exists, never revealing the holder's account kind.
	if _, err := l.store.UserByHandle(ctx, req.GetHandle()); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, errHandleTaken.Error()).WithCause(errHandleTaken)
	} else if !errors.Is(err, store.ErrNotFound) {
		wrappedErr := fmt.Errorf("checking handle for user shadow: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}

	// Persona and role are set-at-creation from the spawn request: under the D9
	// owner-acts model the caller's OWNER is the authority. Role is caller-SELECTED
	// but server-VALIDATED (above); persona is free-text. Both stored via
	// CreateAgent and threaded to the Runner from the CREATED account, never the request.
	created, err := l.store.CreateAgent(ctx, callerOwner, store.NewAgent{
		Handle:      req.GetHandle(),
		DisplayName: req.GetDisplayName(),
		Persona:     req.GetPersona(),
		Role:        req.GetRole(),
		// Set-at-creation: the spawned peer's parent is its spawner (§T3). A new
		// account has no descendants, so this edge cannot form a cycle — the cycle
		// check lives only on the mutable ReparentAgent.
		ParentAgentID: caller,
	})
	var resp *compassv1internal.SpawnPeerResponse
	switch {
	case err == nil:
		resp, err = l.provisionAndStart(ctx, created.ID, created.Agent.Persona, created.Agent.Role, req)
	case errors.Is(err, store.ErrConflict):
		resp, err = l.resumeOrReject(ctx, callerOwner, req)
	default:
		wrappedErr := fmt.Errorf("creating agent: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	if err != nil {
		return nil, err
	}

	// Spawn auto-open (R8): after the spawn chain succeeds, open the
	// manager<->new-peer DM and set DmChannelName. OpenDM is resolve-or-create, so
	// fresh/resume/already-placed all yield the SAME name idempotently. An open
	// FAILURE is logged and returned with an EMPTY name, NEVER a spawn rollback.
	if resp != nil && resp.GetAgentAccountId() != "" {
		resp.DmChannelName = l.autoOpenSpawnDM(ctx, caller, store.AccountID(resp.GetAgentAccountId()))
	}
	return resp, nil
}

// DespawnAsAccount tears down a peer's compute (container + session), NOT its
// identity: the account row is durable. Authority is the OWNER's, not the
// spawner's — any agent may despawn a sibling its owner owns, but never a foreign
// peer. The target is an agent handle, bare (the caller owner's namespace) or
// `owner/agent`. Guards, each fail-closed: self-despawn -> CodeInvalidArgument;
// unknown, non-agent, or foreign-owner target -> the SAME indistinguishable
// CodeNotFound. Idempotent past the guards: a target with no live placement is
// already torn down and succeeds without a Remove (the same
// already-stopped-succeeds contract StopAgentSession has).
func (l *lifecycleService) DespawnAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1internal.DespawnPeerRequest,
) (*compassv1internal.DespawnPeerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, spawnChainTimeout)
	defer cancel()

	// Caller-FIRST closes a latency side-channel: every target outcome within one
	// input form then runs the same query count and differs only by O(1) compares.
	callerOwner, err := l.store.AgentOwner(ctx, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, errCallerNotAgent.Error()).WithCause(errCallerNotAgent)
		}
		wrappedErr := fmt.Errorf("resolving caller owner: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	target, err := l.resolveDespawnTarget(ctx, callerOwner, req.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	if target == caller {
		return nil, connect.NewError(connect.CodeInvalidArgument, errCannotDespawnSelf.Error()).WithCause(errCannotDespawnSelf)
	}

	// Authorized. Stop the target's live session first (best-effort, bounded so a
	// wedged Runner cannot starve the Remove below); skip if none is live.
	if sessionID, ok := l.hub.SessionForAccount(ctx, target); ok {
		stopCtx, stopCancel := context.WithTimeout(ctx, rollbackStopTimeout)
		if _, err := l.hub.Stop(stopCtx, "", &compassv1.StopAgentSessionRequest{SessionId: sessionID}); err != nil {
			slog.ErrorContext(ctx, "despawn: stopping target session failed; continuing to remove", "session_id", sessionID, "error", err)
		}
		stopCancel()
	}

	// Resolve the container to tear down. An unplaced target is already torn
	// down -> idempotent success, no Remove (mirrors StopAgentSession's
	// already-stopped contract).
	_, container, err := l.store.PlacementForAgent(ctx, target)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &compassv1internal.DespawnPeerResponse{}, nil
		}
		wrappedErr := fmt.Errorf("resolving target placement: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}

	if _, err := l.hub.Remove(ctx, "", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: container}); err != nil {
		// Already Connect-coded by the hub relay — return it for in-band render.
		return nil, err
	}
	if err := l.store.DeleteAgentPlacement(ctx, container); err != nil {
		wrappedErr := fmt.Errorf("releasing agent placement: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return &compassv1internal.DespawnPeerResponse{}, nil
}

// resolveDespawnTarget resolves a despawn agent handle to an agent id the caller's
// owner owns. Each input form runs a constant query shape whatever the outcome,
// and a foreign qualifier is never looked up, since that would reveal whether the
// foreign user exists. Every miss is the one errPeerNotFound.
func (l *lifecycleService) resolveDespawnTarget(ctx context.Context, callerOwner store.AccountID, raw string) (store.AccountID, error) {
	qh := store.ParseQualifiedHandle(raw)
	ownerMatches := true
	if qh.Qualified() {
		ownerHandle, err := l.store.AccountHandle(ctx, callerOwner)
		if err != nil {
			wrappedErr := fmt.Errorf("resolving caller owner handle: %w", err)
			return "", connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
		}
		ownerMatches = qh.Owner == ownerHandle
	}
	acc, err := l.store.AgentByHandle(ctx, callerOwner, qh.Handle)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidArgument) {
			return "", connect.NewError(connect.CodeNotFound, errPeerNotFound.Error()).WithCause(errPeerNotFound)
		}
		wrappedErr := fmt.Errorf("resolving target handle: %w", err)
		return "", connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	if !ownerMatches || acc.Agent.OwnerUserID != callerOwner {
		return "", connect.NewError(connect.CodeNotFound, errPeerNotFound.Error()).WithCause(errPeerNotFound)
	}
	return acc.ID, nil
}

// autoOpenSpawnDM opens the manager-peer DM after spawn. Failures are logged,
// never roll back the spawn, and can be recovered through comms_open_dm.
func (l *lifecycleService) autoOpenSpawnDM(ctx context.Context, caller, peerID store.AccountID) string {
	if l.dm == nil {
		return ""
	}
	peer, err := l.store.GetAccount(ctx, peerID)
	if err != nil {
		slog.WarnContext(ctx, "spawn: auto-open manager<->peer DM failed; recoverable via comms_open_dm",
			"error", err.Error(), "peer", string(peerID))
		return ""
	}
	resp, err := l.dm.OpenDMAsAccount(ctx, caller, &compassv1.OpenDMRequest{PeerHandle: peer.Handle})
	if err != nil {
		slog.WarnContext(ctx, "spawn: auto-open manager<->peer DM failed; recoverable via comms_open_dm",
			"error", err.Error(), "peer", string(peerID))
		return ""
	}
	return resp.GetChannel().GetName()
}

// resumeOrReject handles a same-owner collision: return the placed agent or
// resume it if unplaced. The owner-scoped index guarantees identity; a miss is
// an invariant violation, not an authorization case.
func (l *lifecycleService) resumeOrReject(
	ctx context.Context,
	callerOwner store.AccountID,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	existing, err := l.store.AgentByHandle(ctx, callerOwner, req.GetHandle())
	if err != nil {
		// CreateAgent conflicted in the caller's namespace, so the handle must
		// resolve to a same-owner agent. A miss is an invariant violation.
		wrappedErr := fmt.Errorf("resolving existing handle: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}

	// Same owner: return a placed agent or resume it if unplaced.
	_, container, err := l.store.PlacementForAgent(ctx, existing.ID)
	switch {
	case err == nil:
		// Return the existing placement and live session on retry; do not provision
		// a second container.
		sessionID, _ := l.hub.SessionForAccount(ctx, existing.ID)
		return &compassv1internal.SpawnPeerResponse{
			AgentAccountId: string(existing.ID),
			ContainerName:  container,
			SessionId:      sessionID,
		}, nil
	case errors.Is(err, store.ErrNotFound):
		// Unplaced agent: re-provision and start the existing account.
		return l.provisionAndStart(ctx, existing.ID, existing.Agent.Persona, existing.Agent.Role, req)
	default:
		wrappedErr := fmt.Errorf("resolving placement for existing agent: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
}

// provisionAndStart records placement, starts the session, and records
// ownership. Failures roll back; persona and role come from the store.
func (l *lifecycleService) provisionAndStart(
	ctx context.Context,
	agentID store.AccountID,
	persona string,
	role string,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	resp, runnerID, err := l.hub.Provision(ctx, req.GetClientRequestId(), agentID, &compassv1.ProvisionAgentWorkspaceRequest{
		ClientRequestId: req.GetClientRequestId(),
		Persona:         persona,
		Role:            role,
	})
	if err != nil {
		// Already Connect-coded by the hub relay — return it for in-band render.
		return nil, err
	}
	container := resp.GetContainerName()

	if err := l.store.RecordAgentPlacement(ctx, agentID, runnerID, container); err != nil {
		l.rollbackSpawn(ctx, container, "")
		wrappedErr := fmt.Errorf("recording agent placement: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}

	startResp, err := l.hub.Start(ctx, "", &compassv1.StartAgentSessionRequest{
		ContainerName: container,
	})
	if err != nil {
		l.rollbackSpawn(ctx, container, "")
		return nil, err
	}
	sessionID := startResp.GetSessionId()

	if err := l.store.RecordAgentSession(ctx, sessionID, agentID); err != nil {
		l.rollbackSpawn(ctx, container, sessionID)
		wrappedErr := fmt.Errorf("recording agent session: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}

	return &compassv1internal.SpawnPeerResponse{
		AgentAccountId: string(agentID),
		ContainerName:  container,
		SessionId:      sessionID,
	}, nil
}

// rollbackSpawn best-effort stops, removes, and unplaces under bounded
// context.WithoutCancel; caller cancellation does not stop a live container.
// Cleanup failures are logged and do not mask the original cause.
func (l *lifecycleService) rollbackSpawn(ctx context.Context, container, sessionID string) {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spawnChainTimeout)
	defer cancel()

	if sessionID != "" {
		if _, err := l.hub.Stop(tctx, "", &compassv1.StopAgentSessionRequest{SessionId: sessionID}); err != nil {
			slog.ErrorContext(ctx, "spawn rollback: stopping started session failed", "session_id", sessionID, "error", err)
		}
	}
	if _, err := l.hub.Remove(tctx, "", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: container}); err != nil {
		slog.ErrorContext(ctx, "spawn rollback: removing container failed", "container_name", container, "error", err)
	}
	if err := l.store.DeleteAgentPlacement(tctx, container); err != nil {
		slog.ErrorContext(ctx, "spawn rollback: releasing placement failed; handle may stay burned", "container_name", container, "error", err)
	}
}

// Wake outcomes (§Decisions OQ-7): the structured-log outcome vocabulary a wake
// attempt terminates in. WakeAgent logs exactly one per attempt; the string
// values are the literal outcome= log field.
const (
	wakeOutcomeResumed      = "resumed"
	wakeOutcomeAlreadyLive  = "already-live"
	wakeOutcomeFreshStarted = "fresh-started"
	wakeOutcomeCoalesced    = "coalesced"
	wakeOutcomeNoPlacement  = "no-placement"
	wakeOutcomeFailed       = "failed"
)

// errWakeNoPlacement marks a prior-session agent with no placement after
// despawn; wakeOnce maps it to benign no-placement rather than returning it.
var errWakeNoPlacement = errors.New("agent wake: no placement for prior-sessioned agent")

// wakeOnce returns a logged outcome rather than an error; faults become failed.
func (l *lifecycleService) wakeOnce(ctx context.Context, agent store.AccountID) string {
	sessionID, ok, err := l.store.LatestSessionForAccount(ctx, agent)
	if err != nil {
		slog.ErrorContext(ctx, "agent wake: resolving latest session failed", "agent_account_id", agent, "error", err)
		return wakeOutcomeFailed
	}
	if ok {
		switch err := l.resumeSession(ctx, agent, sessionID); {
		case err == nil:
			return wakeOutcomeResumed
		case errors.Is(err, errWakeNoPlacement):
			// A prior session without placement is a routine no-op: despawn removes
			// placement but keeps session rows. Do not emit an error-rate alert.
			return wakeOutcomeNoPlacement
		default:
			slog.ErrorContext(ctx, "agent wake: resume failed", "agent_account_id", agent, "session_id", sessionID, "error", err)
			return wakeOutcomeFailed
		}
	}
	return l.freshStart(ctx, agent)
}

// resumeSession performs the system-authorized wake resume: agent_account_id
// authorizes it, placement locates the container, and session ID reconstructs it.
func (l *lifecycleService) resumeSession(ctx context.Context, agent store.AccountID, sessionID string) error {
	// Resolve placement first; StartResume requires its container, so a miss must
	// fail before transcript reconstruction.
	_, container, err := l.store.PlacementForAgent(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Despawned prior-session agents without placement are benign no-ops.
			return errWakeNoPlacement
		}
		return fmt.Errorf("resolving placement: %w", err)
	}
	body, err := l.hub.ReconstructSessionBody(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("reconstructing session body: %w", err)
	}
	req := &compassv1.StartAgentSessionRequest{ContainerName: container, ResumeSessionId: sessionID}
	_, err = l.hub.StartResume(ctx, "", req, body)
	if connect.CodeOf(err) == connect.CodeNotFound {
		if req.ContainerName, err = l.reprovision(ctx, agent, container); err != nil {
			return err
		}
		_, err = l.hub.StartResume(ctx, "", req, body)
	}
	if err != nil {
		return fmt.Errorf("relaying resume start: %w", err)
	}
	return nil
}

// reprovision recreates a missing Runner container and updates its placement.
func (l *lifecycleService) reprovision(ctx context.Context, agent store.AccountID, stale string) (string, error) {
	acct, err := l.store.GetAccount(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("reading account to re-provision: %w", err)
	}
	if acct.Agent == nil {
		return "", fmt.Errorf("%w: account %q is not an agent", store.ErrNotFound, agent)
	}
	resp, runnerID, err := l.hub.Provision(ctx, "", agent, &compassv1.ProvisionAgentWorkspaceRequest{
		AgentHandle: string(agent),
		Persona:     acct.Agent.Persona,
		Role:        acct.Agent.Role,
	})
	if err != nil {
		return "", fmt.Errorf("re-provisioning lost container %q: %w", stale, err)
	}
	container := resp.GetContainerName()
	// No rollback: releasing the stale placement could leave the account unplaced.
	if err := l.store.RecordAgentPlacement(ctx, agent, runnerID, container); err != nil {
		return "", fmt.Errorf("recording re-provisioned placement: %w", err)
	}
	slog.InfoContext(ctx, "agent wake: re-provisioned lost container", "agent_account_id", agent, "stale_container", stale, "container_name", container)
	return container, nil
}

// freshStart mints a session for an agent without prior session history. With no
// placement it returns no-placement for a future natural start.
func (l *lifecycleService) freshStart(ctx context.Context, agent store.AccountID) string {
	_, container, err := l.store.PlacementForAgent(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return wakeOutcomeNoPlacement
		}
		slog.ErrorContext(ctx, "agent wake: resolving placement failed", "agent_account_id", agent, "error", err)
		return wakeOutcomeFailed
	}
	startResp, err := l.hub.Start(ctx, "", &compassv1.StartAgentSessionRequest{ContainerName: container})
	if connect.CodeOf(err) == connect.CodeNotFound {
		if container, err = l.reprovision(ctx, agent, container); err == nil {
			startResp, err = l.hub.Start(ctx, "", &compassv1.StartAgentSessionRequest{ContainerName: container})
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "agent wake: fresh start failed", "agent_account_id", agent, "container_name", container, "error", err)
		return wakeOutcomeFailed
	}
	if err := l.store.RecordAgentSession(ctx, startResp.GetSessionId(), agent); err != nil {
		// The live sweep already delivers the pending message. Keep the container;
		// a later wake may retry. Log failure without treating it as recoverable.
		slog.ErrorContext(ctx, "agent wake: recording fresh session failed", "agent_account_id", agent, "session_id", startResp.GetSessionId(), "error", err)
		return wakeOutcomeFailed
	}
	return wakeOutcomeFreshStarted
}

// wakeCtx assigns the agent's tenant to system-role wake writes; without it,
// they would use an empty tenant_id and be invisible to every tenant.
func (l *lifecycleService) wakeCtx(ctx context.Context, agent store.AccountID) (context.Context, bool) {
	if !store.IsSystemRole(ctx) {
		return ctx, true
	}
	tenant, err := l.store.AccountTenant(ctx, agent)
	if err != nil {
		slog.ErrorContext(ctx, "agent wake: resolving tenant failed", "agent_account_id", agent, "error", err)
		return ctx, false
	}
	return store.WithTenant(store.WithoutSystemRole(ctx), tenant), true
}
