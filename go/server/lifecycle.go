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

	"connectrpc.com/connect"
	"golang.org/x/sync/singleflight"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// dmOpener opens the manager<->peer DM at spawn time (R8). A narrow seam so
// lifecycleService does not pull the whole Comms handler in — satisfied by
// *comms.Comms via OpenDMAsAccount. It takes the PUBLIC compassv1 OpenDM types
// (the agent-caller adapter's signature).
type dmOpener interface {
	OpenDMAsAccount(ctx context.Context, account store.AccountID, req *compassv1.OpenDMRequest) (*compassv1.OpenDMResponse, error)
}

// lifecycleService is the LifecycleCaller implementation. It holds the store (of
// record for accounts + placements) and the hub (Provision/Start/Stop/Remove
// relays to the owning Runner) — the same two dependencies the provision/start
// handlers use, wired here as the agent-initiated door — plus the dmOpener seam
// that auto-opens the manager<->peer DM after a spawn (R8).
type lifecycleService struct {
	store *store.Store
	hub   *runnerhub.Hub
	// dm opens the manager<->new-peer DM at spawn time (R8). Nil for instances
	// that never spawn (the waker), in which case autoOpenSpawnDM returns an
	// empty name.
	dm dmOpener
	// wakeGroup coalesces concurrent WakeAgent calls for the SAME agent onto one
	// start (RIG-1641 T3 cost control, §Decisions OQ-2): a burst of messages at
	// one offline agent produces exactly one resume/Start, not a start-storm. A
	// zero-value Group is ready, so newLifecycleService needs no init.
	wakeGroup singleflight.Group
}

// newLifecycleService constructs the lifecycle caller over the store, hub, and
// the DM-opener seam. Wired at serve assembly with hub.SetLifecycleCaller after
// all three exist, breaking the hub<->lifecycleService construction cycle
// (serve.go).
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
//  1. Not-live pre-check: a live agent has nothing to wake — no-op.
//  2. Per-agent singleflight: N concurrent wakes for one agent coalesce onto one
//     start; the coalesced callers log outcome=coalesced.
//  3. LatestSessionForAccount: a prior session → the SYSTEM-AUTHORIZED internal
//     resume (a sibling of startResumeSession that SKIPS the caller-subscriber
//     gate — the wake IS the authorization, §Decisions OQ-2, so there is no
//     caller and no RequireAgentSessionSubscriber); no prior session → a fresh
//     hub.Start; no placement → a logged no-op.
func (l *lifecycleService) WakeAgent(ctx context.Context, agent store.AccountID) {
	// 1. Not-live pre-check (cost control): a live agent is already awake, so
	// there is nothing to resume. No-op, no log line — a wake is only an attempt
	// against an OFFLINE agent.
	if _, live := l.hub.SessionForAccount(ctx, agent); live {
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

// spawnChainTimeout bounds the whole spawn (and despawn) relay chain. Like
// rollbackStopTimeout (service.go) the runnerhub dispatch path has no deadline
// of its own, so a wedged-but-connected Runner that accepts a command but never
// answers would otherwise hang the agent's spawn/despawn call forever. A package
// var rather than a const only so a test can shorten it; never reassigned in
// production.
var spawnChainTimeout = 60 * time.Second

// errCannotDespawnSelf is the in-band cause for a self-despawn: an agent cannot
// tear down its own compute out from under itself. CodeInvalidArgument.
var errCannotDespawnSelf = errors.New("cannot despawn self")

// errPeerNotFound is the in-band cause a despawn returns for EVERY unauthorized
// or unknown target — unknown handle, non-agent handle, and foreign-owner peer
// all collapse to this one message so the caller can never distinguish a peer it
// may not touch from one that does not exist (the not-found/forbidden merge).
// CodeNotFound.
var errPeerNotFound = errors.New("peer not found")

// errHandleTaken is the in-band cause when a spawn handle is already taken by an
// agent the caller does not own (or by a non-agent account): the same
// already_exists collapse a human caller gets for a duplicate handle, and it
// never reveals whose it is. CodeAlreadyExists.
var errHandleTaken = errors.New("handle already taken")

// errCallerNotAgent is the fail-closed cause when the resolved caller does not
// resolve to an agent account. The hub only delegates for a caller it resolved
// from a live agent-session binding, so this is a wiring-invariant violation,
// never a normal outcome — CodeInternal, never a silent success.
var errCallerNotAgent = errors.New("resolved caller is not an agent account")

// errUnknownRole is the in-band cause when a spawn names a role outside the
// closed taxonomy (spawnableRoles), including an empty role: every spawned node
// carries a valid role, and the server is the authority on the set. The label,
// not the prompt text, is validated — prompt text still arrives only via the
// operator config bundle. CodeInvalidArgument.
var errUnknownRole = errors.New("unknown spawn role")

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

	// Role validation: every spawned node carries a role from the closed taxonomy,
	// and the server is the authority. First check in the chain, so it covers the
	// idempotent-resume branch too. The LABEL is validated, never the prompt text:
	// a valid label with an unshipped prompt degrades to default block-0 (a warn).
	if _, ok := spawnableRoles[req.GetRole()]; !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errUnknownRole)
	}

	// F2 ownership: the spawned peer inherits the CALLER'S OWNER, resolved from the
	// store (the caller is an agent account).
	callerOwner, err := l.store.AgentOwner(ctx, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, errCallerNotAgent)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller owner: %w", err))
	}

	// Shadow guard (defense in depth): an agent may never be spawned onto a handle
	// a user or system account already holds. Storage permits the overlap, but a
	// peer shadowing a human's handle could be addressed in its place. Refuse with
	// the same in-band already_exists, never revealing the holder's account kind.
	if _, err := l.store.UserByHandle(ctx, req.GetHandle()); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, errHandleTaken)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("checking handle for user shadow: %w", err))
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
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("creating agent: %w", err))
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
			return nil, connect.NewError(connect.CodeInternal, errCallerNotAgent)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller owner: %w", err))
	}
	target, err := l.resolveDespawnTarget(ctx, callerOwner, req.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	if target == caller {
		return nil, connect.NewError(connect.CodeInvalidArgument, errCannotDespawnSelf)
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
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving target placement: %w", err))
	}

	if _, err := l.hub.Remove(ctx, "", &compassv1.RemoveAgentWorkspaceRequest{ContainerName: container}); err != nil {
		// Already Connect-coded by the hub relay — return it for in-band render.
		return nil, err
	}
	if err := l.store.DeleteAgentPlacement(ctx, container); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("releasing agent placement: %w", err))
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
			return "", connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller owner handle: %w", err))
		}
		ownerMatches = qh.Owner == ownerHandle
	}
	acc, err := l.store.AgentByHandle(ctx, callerOwner, qh.Handle)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidArgument) {
			return "", connect.NewError(connect.CodeNotFound, errPeerNotFound)
		}
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("resolving target handle: %w", err))
	}
	if !ownerMatches || acc.Agent.OwnerUserID != callerOwner {
		return "", connect.NewError(connect.CodeNotFound, errPeerNotFound)
	}
	return acc.ID, nil
}

// autoOpenSpawnDM opens the manager<->new-peer DM (R8) and returns its channel
// name, or "" on any failure. caller is the MANAGER (the spawn's caller); peerID
// is the created/resolved peer's account id. OpenDM addresses the peer by HANDLE,
// so the peer's handle is resolved from its account. On ANY error — a nil opener
// (an unwired test), a handle resolve miss, or the open itself — it LOGS and
// returns an empty name rather than failing the spawn: the DM is recoverable next
// turn via comms_open_dm, never a spawn rollback (design.md T3:755-762).
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

// resumeOrReject handles a spawn whose handle is already taken in the CALLER'S
// OWN owner namespace (CreateAgent conflicted on the per-owner agent index). An
// already-placed agent is an idempotent success returning the existing
// container/session; an unplaced one is resumed. The handle is guaranteed to
// resolve to a same-owner agent here: the shadow guard already excluded
// user/system handles, and a foreign owner's same-name agent lives in a
// separate namespace partition that never conflicts on insert — so a miss is an
// invariant violation, not a foreign or non-agent handle to collapse.
func (l *lifecycleService) resumeOrReject(
	ctx context.Context,
	callerOwner store.AccountID,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	existing, err := l.store.AgentByHandle(ctx, callerOwner, req.GetHandle())
	if err != nil {
		// CreateAgent conflicted on the caller's own agent namespace, so the
		// handle MUST resolve to a same-owner agent here. A miss is an invariant
		// violation (a torn index or a concurrent delete), not a routine outcome
		// — surface it rather than masking a real fault as already_exists.
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving existing handle: %w", err))
	}

	// Same owner: is it already placed (live), or unplaced (resumable)?
	_, container, err := l.store.PlacementForAgent(ctx, existing.ID)
	switch {
	case err == nil:
		// Already spawned and placed: idempotent success. Return the existing
		// container and its live session (if any) rather than provisioning a
		// second — a completed-call retry gets its original answer.
		sessionID, _ := l.hub.SessionForAccount(ctx, existing.ID)
		return &compassv1internal.SpawnPeerResponse{
			AgentAccountId: string(existing.ID),
			ContainerName:  container,
			SessionId:      sessionID,
		}, nil
	case errors.Is(err, store.ErrNotFound):
		// Unplaced: a spawn that crashed after CreateAgent (or was rolled back).
		// Resume — re-provision and start the existing account, not a second.
		return l.provisionAndStart(ctx, existing.ID, existing.Agent.Persona, existing.Agent.Role, req)
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving placement for existing agent: %w", err))
	}
}

// provisionAndStart runs the placement/session chain for agentID: Provision
// (threading the client_request_id idempotency key), record the durable
// placement, Start, record the session ownership. On any post-Provision failure
// it rolls the container back (bounded Stop + Remove + DeleteAgentPlacement) so
// the account is left UNPLACED and the handle is not burned — a re-spawn of the
// same handle then resumes. persona and role are the store's server-
// authoritative values for the account, threaded to the Runner so no caller
// value is trusted.
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
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("recording agent placement: %w", err))
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
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("recording agent session: %w", err))
	}

	return &compassv1internal.SpawnPeerResponse{
		AgentAccountId: string(agentID),
		ContainerName:  container,
		SessionId:      sessionID,
	}, nil
}

// rollbackSpawn tears a partially-spawned container back down so the account is
// left UNPLACED and its handle is not burned — the resume path (no live
// placement) then re-spawns cleanly. Best-effort and bounded on
// context.WithoutCancel(ctx): the caller's context may already be cancelled (a
// client that gave up is one plausible reason the store write failed), and the
// container is live regardless — the same discipline abandonStartedSession uses.
// The container release order is Stop (if a session started) -> Remove ->
// DeleteAgentPlacement; each failure is logged, none masks the original cause the
// caller already returns.
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
	wakeOutcomeFreshStarted = "fresh-started"
	wakeOutcomeCoalesced    = "coalesced"
	wakeOutcomeNoPlacement  = "no-placement"
	wakeOutcomeFailed       = "failed"
)

// errWakeNoPlacement is the internal sentinel resumeSession returns when the
// agent has a prior session but no current placement (a despawned agent —
// despawn deletes the placement, agent_sessions rows are never deleted).
// wakeOnce maps it to the benign outcome=no-placement, the same disposition
// freshStart gives a never-provisioned agent (§Decisions OQ-7). It never
// escapes the wake path.
var errWakeNoPlacement = errors.New("agent wake: no placement for prior-sessioned agent")

// wakeOnce runs one non-coalesced wake attempt for agent and returns the outcome
// string the caller logs — one of "resumed", "fresh-started", "no-placement", or
// "failed". It never returns an error: the singleflight fn's error slot is
// unused because a wake is void/best-effort, so every fault is captured here as
// outcome=failed with the cause logged, not propagated.
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
			// A prior session with no current placement is a despawned agent
			// (despawn deletes the placement; agent_sessions rows are never deleted).
			// Benign and common — a logged no-op, NOT outcome=failed. Emitting ERROR
			// here would trip error-rate alerting on a routine path.
			return wakeOutcomeNoPlacement
		default:
			slog.ErrorContext(ctx, "agent wake: resume failed", "agent_account_id", agent, "session_id", sessionID, "error", err)
			return wakeOutcomeFailed
		}
	}
	return l.freshStart(ctx, agent)
}

// resumeSession runs the SYSTEM-AUTHORIZED internal resume of sessionID: the same
// ordered chain startResumeSession runs (BindLifetime → ReconstructSessionBody →
// hub.StartResume, service.go:603-616) MINUS RequireAgentSessionSubscriber — the
// wake holds an agent_account_id, not a caller, and the wake IS the authorization
// (§Decisions OQ-2). StartResume relays on the CONTAINER (resume_start.go:34), so
// the container is resolved from the durable placement while the bind/reconstruct
// legs key on the stable logical session_id.
func (l *lifecycleService) resumeSession(ctx context.Context, agent store.AccountID, sessionID string) error {
	// Resolve the container the resume relays on FIRST (StartResume keys on it),
	// so a placement miss fails before any bind/reconstruct work.
	_, container, err := l.store.PlacementForAgent(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Signal a benign no-placement to wakeOnce (mirrors freshStart): a
			// despawned prior-sessioned agent is a routine no-op, not a fault.
			return errWakeNoPlacement
		}
		return fmt.Errorf("resolving placement: %w", err)
	}
	if _, err := l.store.BindLifetime(ctx, sessionID); err != nil {
		return fmt.Errorf("binding resume lifetime: %w", err)
	}
	body, err := l.hub.ReconstructSessionBody(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("reconstructing session body: %w", err)
	}
	if _, err := l.hub.StartResume(ctx, "", &compassv1.StartAgentSessionRequest{
		ContainerName:   container,
		ResumeSessionId: sessionID,
	}, body); err != nil {
		return fmt.Errorf("relaying resume start: %w", err)
	}
	return nil
}

// freshStart is the no-prior-session fallback: an agent that has never had a
// session recorded has no session_id to reconstruct, so the wake mints a fresh
// one over the existing start chain (provisionAndStart's tail, lifecycle.go:323).
// A never-provisioned agent has no placement — a logged no-op (outcome
// "no-placement"), the owed row / cursor waits for any future natural start. It
// returns the outcome string the caller logs.
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
	if err != nil {
		slog.ErrorContext(ctx, "agent wake: fresh start failed", "agent_account_id", agent, "container_name", container, "error", err)
		return wakeOutcomeFailed
	}
	if err := l.store.RecordAgentSession(ctx, startResp.GetSessionId(), agent); err != nil {
		// The session is already live and started, and the pending message delivers
		// via the live sweep this Start fired — so we deliberately do NOT roll the
		// container back here. The only cost is that a FUTURE wake fresh-starts
		// again; acceptable for a best-effort void wake. outcome=failed flags it.
		slog.ErrorContext(ctx, "agent wake: recording fresh session failed", "agent_account_id", agent, "session_id", startResp.GetSessionId(), "error", err)
		return wakeOutcomeFailed
	}
	return wakeOutcomeFreshStarted
}
