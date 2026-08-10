//go:build unix

// The composite start: SpawnAgent runs ProvisionAgentWorkspace then
// StartAgentSession server-side under ONE client_request_id, so a UI brings an
// agent online in a single call (DL-166). It reuses the existing human-path
// handlers verbatim — it never reimplements provisioning — so persona/role
// authority, durable placement, session-ownership recording, and the
// anti-stranding rollback are identical to the two-call flow.
//
// Two behaviors the composite owns beyond calling the two handlers:
//
//   - End-to-end idempotency. A retry with the same client_request_id returns
//     the same session_id and provisions no second container. The three lower
//     primitives (provisionDedupID, the router in-flight join, the dispatcher
//     handled map) each dedup one relayed command, but they do NOT compose a
//     SEQUENTIAL completed retry (the router entry is deleted on complete, and
//     Start mints a fresh relay id). The composite span the server adds is a
//     client_request_id-keyed memo of the in-flight/completed spawn: a retry
//     joins it and returns the original result rather than re-running either
//     step. This is the "dedup-join lookup" the reject-on-live check is ordered
//     after.
//
//   - Pre-Provision reject-on-live. On a cache miss ONLY, before issuing any
//     Provision, scan the Runner's authoritative live-session set for one whose
//     agent_account_id matches the request; on a hit return CodeAlreadyExists.
//     Ordered after the dedup-join so a retry of an in-flight/completed spawn
//     rejoins the original instead of being bounced (one container per agent
//     account, DL-170). The scan reads the Runner (Hub.Status with an empty
//     session id — "empty = every live session"), never Server in-memory state,
//     which fails open after a Runner reconnect clears the bindings.
package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
)

// errAgentAlreadyLive is the reject-on-live cause: the target agent account
// already holds a live session, so a second spawn would collide on its one
// container (DL-170). CodeAlreadyExists, returned before any Provision.
var errAgentAlreadyLive = errors.New("agent already has a live session")

// spawnCall is one in-flight-or-completed composite spawn, keyed by
// client_request_id. done closes when the composite settles; resp/err then hold
// its outcome for every retry that joined. A SUCCESSFUL entry is retained so a
// later sequential retry returns the same session (the end-to-end idempotency
// contract); a FAILED entry is removed on settle so a retry re-attempts rather
// than replaying the failure (DL-169: retry a failed start is a re-attempt the
// server's dedup decides join-vs-reattempt on).
type spawnCall struct {
	done chan struct{}
	resp *compassv1.SpawnAgentResponse
	err  error
}

// SpawnAgent brings an agent online in one call: it provisions the agent's
// container and starts its session under the single client_request_id, owning
// end-to-end idempotency and the pre-Provision reject-on-live short-circuit. It
// reuses the ProvisionAgentWorkspace and StartAgentSession handlers, so a server
// built with no Runner door surfaces their Unavailable, and a mid-sequence
// failure surfaces their Connect status (the Start rollback already tears a
// stranded container back down).
func (s *service) SpawnAgent(
	ctx context.Context,
	req *connect.Request[compassv1.SpawnAgentRequest],
) (*connect.Response[compassv1.SpawnAgentResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}

	crid := req.Msg.GetClientRequestId()
	// The dedup-join lookup. A non-empty client_request_id memoizes the spawn:
	// the first caller runs it, every retry joins the same entry. An empty id is
	// not memoized (each call is a distinct spawn) but still runs reject-on-live.
	if crid != "" {
		if call, joined := s.joinOrBeginSpawn(crid); joined {
			// Joined an in-flight or completed spawn: wait for it to settle and
			// return its result — never a second Provision, never reject-on-live.
			select {
			case <-call.done:
				if call.err != nil {
					return nil, call.err
				}
				return connect.NewResponse(call.resp), nil
			case <-ctx.Done():
				return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
			}
		} else {
			resp, err := s.runSpawn(ctx, req.Msg)
			s.settleSpawn(crid, call, resp, err)
			if err != nil {
				return nil, err
			}
			return connect.NewResponse(resp), nil
		}
	}

	resp, err := s.runSpawn(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// runSpawn is the cache-miss body: reject-on-live, then Provision, then Start,
// under the one client_request_id. It reuses the existing handlers so the full
// orchestration (persona/role authority, placement, session ownership, rollback)
// is identical to the two-call human path.
func (s *service) runSpawn(ctx context.Context, msg *compassv1.SpawnAgentRequest) (*compassv1.SpawnAgentResponse, error) {
	// Pre-Provision reject-on-live: consult the Runner (authoritative for live
	// session truth) for every live session and reject if the target agent
	// already holds one. Runs on the cache-miss path only, BEFORE Provision, so a
	// rejected spawn churns no container. Sourced from the Runner's status scan,
	// never Server in-memory state (which fails open after a reconnect clears the
	// bindings and would let the spawn collide on the container name mid-Provision).
	if err := s.rejectIfAgentLive(ctx, msg.GetAgentAccountId()); err != nil {
		return nil, err
	}

	provResp, err := s.ProvisionAgentWorkspace(ctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  msg.GetAgentAccountId(),
		ClientRequestId: msg.GetClientRequestId(),
	}))
	if err != nil {
		return nil, err
	}
	container := provResp.Msg.GetContainerName()

	startResp, err := s.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: container,
		InitialPrompt: msg.GetInitialPrompt(),
	}))
	if err != nil {
		return nil, err
	}

	return &compassv1.SpawnAgentResponse{
		SessionId:     startResp.Msg.GetSessionId(),
		ContainerName: container,
	}, nil
}

// rejectIfAgentLive returns CodeAlreadyExists when agentAccountID already holds a
// live session, scanning the Runner's authoritative all-sessions status set (the
// GetAgentStatus arm answered by the Runner with an empty session id). The scan
// matches on AgentSessionStatus.agent_account_id (DL-167) — the request shape
// admits nothing else, since GetAgentStatusRequest carries only a session id. A
// relay failure propagates: reject-on-live must fail closed rather than let a
// second container collide on the one-per-account name.
func (s *service) rejectIfAgentLive(ctx context.Context, agentAccountID string) error {
	statuses, err := s.hub.Status(ctx, "", &compassv1.GetAgentStatusRequest{})
	if err != nil {
		return err
	}
	for _, st := range statuses.GetStatuses() {
		if st.GetAgentAccountId() == agentAccountID {
			return connect.NewError(connect.CodeAlreadyExists, errAgentAlreadyLive)
		}
	}
	return nil
}

// joinOrBeginSpawn is the memo lookup: it returns (existing, true) when a spawn
// for crid is already in flight or completed-successfully, or (fresh, false)
// after registering a new entry the caller must settle. Under s.spawnMu so two
// concurrent retries never both begin.
func (s *service) joinOrBeginSpawn(crid string) (*spawnCall, bool) {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	if s.spawns == nil {
		s.spawns = make(map[string]*spawnCall)
	}
	if existing, ok := s.spawns[crid]; ok {
		return existing, true
	}
	call := &spawnCall{done: make(chan struct{})}
	s.spawns[crid] = call
	return call, false
}

// settleSpawn records a fresh spawn's outcome and wakes every joined retry. A
// success is retained under crid so a later sequential retry returns the same
// session (end-to-end idempotency); a failure is removed so a retry re-attempts
// (DL-169) rather than replaying the failure.
func (s *service) settleSpawn(crid string, call *spawnCall, resp *compassv1.SpawnAgentResponse, err error) {
	s.spawnMu.Lock()
	call.resp = resp
	call.err = err
	if err != nil {
		delete(s.spawns, crid)
	}
	s.spawnMu.Unlock()
	close(call.done)
}
