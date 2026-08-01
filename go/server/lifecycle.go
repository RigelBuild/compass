//go:build unix

// The agent-initiated lifecycle leg: lifecycleService implements
// runnerhub.LifecycleCaller (relay_lifecycle.go), the seam the RunnerHub
// delegates a resolved-caller spawn/despawn into. It runs the SAME provisioning
// paths a human-initiated ProvisionAgentWorkspace/StartAgentSession takes
// (service.go), so authz, server-authoritative persona, and container placement
// are identical — the hub depends only on the narrow LifecycleCaller surface and
// never pulls the whole service in.
//
// Trust model (the load-bearing security leg). The caller AccountID is resolved
// Server-side by the hub from its own session binding and passed in; the Runner
// never asserts it. Two fail-closed authority rules follow (spawn/despawn record
// F2 ownership):
//
//   - Spawn creates the new peer under the CALLER'S OWNER — never the caller
//     agent itself, never the bootstrap admin — so a spawned peer shares the
//     human owner of the agent that spawned it.
//   - Despawn is same-owner only: a target owned by a different user (or unknown,
//     or not an agent) is an INDISTINGUISHABLE CodeNotFound, so a caller can
//     never probe a foreign peer's existence; despawning oneself is refused
//     CodeInvalidArgument.
//
// A tool-level failure (dup handle, foreign target, self-despawn) is returned as
// a Connect-coded error the hub renders IN-BAND (lifecycleCallError); only a
// resolution miss / no-caller is a transport error, and that is the hub's job.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// lifecycleService is the LifecycleCaller implementation. It holds the store (of
// record for accounts + placements) and the hub (Provision/Start/Stop/Remove
// relays to the owning Runner) — the same two dependencies the provision/start
// handlers use, wired here as the agent-initiated door.
type lifecycleService struct {
	store *store.Store
	hub   *runnerhub.Hub
}

// newLifecycleService constructs the lifecycle caller over the store and hub.
// Wired at serve assembly with hub.SetLifecycleCaller after both exist, breaking
// the hub<->lifecycleService construction cycle (serve.go).
func newLifecycleService(st *store.Store, hub *runnerhub.Hub) *lifecycleService {
	return &lifecycleService{store: st, hub: hub}
}

// Compile-time proof lifecycleService satisfies the seam the hub delegates into.
var _ runnerhub.LifecycleCaller = (*lifecycleService)(nil)

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
// or unknown target — unknown id, non-agent id, and foreign-owner peer all
// collapse to this one message so the caller can never distinguish a peer it may
// not touch from one that does not exist (the not-found/forbidden merge).
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

// SpawnAsAccount creates a peer agent owned by the caller's OWNER and brings it
// online, running the same provision->placement->start->session chain a human
// spawn takes. The new agent's owner is the caller agent's owner (F2), resolved
// from the store — never the caller itself, never admin, never a client value.
//
// Idempotency / resume on a taken handle. A duplicate handle is not blindly an
// error: if the existing agent is owned by the SAME owner and has a live
// placement, the spawn is an idempotent no-op returning the existing
// container/session (a completed-call retry); if it is same-owner but UNPLACED
// (a spawn that crashed after CreateAgent, or one rolled back), it is RESUMED —
// re-provisioned and started against the existing account rather than creating a
// second. A handle owned by a DIFFERENT user (or a non-agent account) is
// CodeAlreadyExists and never resumes/steals it.
func (l *lifecycleService) SpawnAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, spawnChainTimeout)
	defer cancel()

	// F2 ownership: the spawned peer inherits the CALLER'S OWNER. Resolve it
	// from the store — the caller is an agent account, and its owner is who the
	// new peer belongs to.
	callerOwner, err := l.store.AgentOwner(ctx, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, errCallerNotAgent)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller owner: %w", err))
	}

	// Persona is server-authoritative and empty on spawn (SpawnPeerRequest
	// carries none): the new account is created with no persona, and the value
	// threaded to the Runner comes from the store account below, never the
	// caller — a caller cannot inject a system prompt.
	created, err := l.store.CreateAgent(ctx, callerOwner, store.NewAgent{
		Handle:      req.GetHandle(),
		DisplayName: req.GetDisplayName(),
		Persona:     "",
	})
	switch {
	case err == nil:
		return l.provisionAndStart(ctx, created.ID, created.Agent.Persona, req)
	case errors.Is(err, store.ErrConflict):
		return l.resumeOrReject(ctx, callerOwner, req)
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("creating agent: %w", err))
	}
}

// DespawnAsAccount tears down a peer's compute (container + session), NOT its
// identity: the account row is durable. Authority is the OWNER's, not the
// spawner's — any agent may despawn a sibling its owner owns, but never a foreign
// peer. Guards, each fail-closed: self-despawn -> CodeInvalidArgument; unknown,
// non-agent, or foreign-owner target -> the SAME indistinguishable CodeNotFound.
// Idempotent past the guards: a target with no live placement is already torn
// down and succeeds without a Remove (the same already-stopped-succeeds contract
// StopAgentSession has).
func (l *lifecycleService) DespawnAsAccount(
	ctx context.Context,
	caller store.AccountID,
	req *compassv1internal.DespawnPeerRequest,
) (*compassv1internal.DespawnPeerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, spawnChainTimeout)
	defer cancel()

	target := store.AccountID(req.GetAgentAccountId())
	if target == caller {
		return nil, connect.NewError(connect.CodeInvalidArgument, errCannotDespawnSelf)
	}

	// Owner check, fail-closed and indistinguishable. An unknown or non-agent
	// target is ErrNotFound from AgentOwner; a foreign-owner target collapses to
	// the identical CodeNotFound so neither can be told apart from the other.
	targetOwner, err := l.store.AgentOwner(ctx, target)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errPeerNotFound)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving target owner: %w", err))
	}
	callerOwner, err := l.store.AgentOwner(ctx, caller)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInternal, errCallerNotAgent)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller owner: %w", err))
	}
	if targetOwner != callerOwner {
		// A peer the caller's owner does not own: indistinguishable from unknown.
		return nil, connect.NewError(connect.CodeNotFound, errPeerNotFound)
	}

	// Authorized. Stop the target's live session first (best-effort, bounded so a
	// wedged Runner cannot starve the Remove below); skip if none is live.
	if sessionID, ok := l.hub.SessionForAccount(target); ok {
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

// resumeOrReject handles a spawn whose handle is already taken. Same-owner with
// no live placement resumes the existing account; same-owner already placed is an
// idempotent success returning the existing container/session; a foreign owner
// (or a non-agent handle) is CodeAlreadyExists that never resumes or steals it.
func (l *lifecycleService) resumeOrReject(
	ctx context.Context,
	callerOwner store.AccountID,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	existing, err := l.store.AgentByHandle(ctx, req.GetHandle())
	if err != nil {
		// The handle is taken (CreateAgent conflicted) but does not resolve to an
		// agent — a non-agent account holds it. Collapse to already_exists; never
		// reveal what kind of account it is.
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errHandleTaken)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving existing handle: %w", err))
	}
	if existing.Agent.OwnerUserID != callerOwner {
		// A DIFFERENT owner's agent holds this handle: never resume or steal it,
		// and never reveal whose it is — the same already_exists a human gets.
		return nil, connect.NewError(connect.CodeAlreadyExists, errHandleTaken)
	}

	// Same owner: is it already placed (live), or unplaced (resumable)?
	_, container, err := l.store.PlacementForAgent(ctx, existing.ID)
	switch {
	case err == nil:
		// Already spawned and placed: idempotent success. Return the existing
		// container and its live session (if any) rather than provisioning a
		// second — a completed-call retry gets its original answer.
		sessionID, _ := l.hub.SessionForAccount(existing.ID)
		return &compassv1internal.SpawnPeerResponse{
			AgentAccountId: string(existing.ID),
			ContainerName:  container,
			SessionId:      sessionID,
		}, nil
	case errors.Is(err, store.ErrNotFound):
		// Unplaced: a spawn that crashed after CreateAgent (or was rolled back).
		// Resume — re-provision and start the existing account, not a second.
		return l.provisionAndStart(ctx, existing.ID, existing.Agent.Persona, req)
	default:
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving placement for existing agent: %w", err))
	}
}

// provisionAndStart runs the placement/session chain for agentID: Provision
// (threading the client_request_id idempotency key), record the durable
// placement, Start, record the session ownership. On any post-Provision failure
// it rolls the container back (bounded Stop + Remove + DeleteAgentPlacement) so
// the account is left UNPLACED and the handle is not burned — a re-spawn of the
// same handle then resumes. persona is the store's server-authoritative value
// for the account, threaded to the Runner so no caller value is trusted.
func (l *lifecycleService) provisionAndStart(
	ctx context.Context,
	agentID store.AccountID,
	persona string,
	req *compassv1internal.SpawnPeerRequest,
) (*compassv1internal.SpawnPeerResponse, error) {
	resp, runnerID, err := l.hub.Provision(ctx, req.GetClientRequestId(), &compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  string(agentID),
		ClientRequestId: req.GetClientRequestId(),
		Persona:         persona,
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
		InitialPrompt: req.GetInitialPrompt(),
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
