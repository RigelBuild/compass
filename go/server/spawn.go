//go:build unix

// The composite start: SpawnAgent runs provisionAgent then the StartAgentSession
// handler under ONE client_request_id (DL-166). It owns end-to-end idempotency (a
// client_request_id-keyed memo, since the lower primitives don't compose a
// sequential retry) and pre-Provision reject-on-live.
package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// errAgentAlreadyLive is the reject-on-live cause: the target agent account
// already holds a live session, so a second spawn would collide on its one
// container (DL-170). CodeAlreadyExists, returned before any Provision.
var errAgentAlreadyLive = errors.New("agent already has a live session")

// spawnMemoTTL bounds how long a settled SUCCESS spawn entry is retained for
// idempotent replay before eviction. It covers the real retry window — a UI
// double-click or a blip retry, seconds to minutes — not a server restart: the
// server is single-instance and re-derives live truth from the Runner on
// restart, so a memo need not survive one. Without this bound the map would
// grow one permanent entry per successful spawn for the process lifetime.
const spawnMemoTTL = 10 * time.Minute

// spawnKey identifies a composite spawn in the memo. It binds the
// client_request_id to the agent account — matching provisionDedupID's account
// binding (runnerhub/commands.go): the same client_request_id reused for a
// DIFFERENT account derives a distinct entry, never a join that would hand back
// the first account's session and provision nothing for the second.
type spawnKey struct {
	account string
	crid    string
}

// spawnCall is one in-flight-or-completed composite spawn, keyed by
// (agent_account_id, client_request_id). done closes when the composite
// settles; resp/err then hold its outcome for every retry that joined. A
// SUCCESSFUL entry is retained for spawnMemoTTL so a later sequential retry
// returns the same session (the end-to-end idempotency contract) then evicted;
// a FAILED entry is removed on settle so a retry re-attempts rather than
// replaying the failure (DL-169: retry a failed start is a re-attempt the
// server's dedup decides join-vs-reattempt on).
type spawnCall struct {
	done chan struct{}
	resp *compassv1.SpawnAgentResponse
	err  error
}

// SpawnAgent brings an agent online in one call: it provisions the agent's
// container and starts its session under the single client_request_id, owning
// end-to-end idempotency and the pre-Provision reject-on-live short-circuit. It
// reuses provisionAgent and the StartAgentSession handler; a server built with
// no Runner door gets Unavailable from its own hub check, and a mid-sequence
// failure surfaces the relay's Connect status (the Start rollback already tears
// a stranded container back down).
func (s *service) SpawnAgent(
	ctx context.Context,
	req *connect.Request[compassv1.SpawnAgentRequest],
) (*connect.Response[compassv1.SpawnAgentResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}

	// Resolved before the memo so a retry keys on the account, not the spelling:
	// a rename between attempts still joins the first spawn.
	acc, err := s.resolveQualifiedAgent(ctx, req.Msg.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	return s.spawnAccount(ctx, acc, req.Msg.GetClientRequestId())
}

// spawnAccount is SpawnAgent past the wire edge, for an already-resolved agent
// acc: the memo, reject-on-live, and runSpawn. The root-supervisor seed calls it
// directly with the account it already holds. The caller has checked s.hub.
func (s *service) spawnAccount(ctx context.Context, acc store.Account, crid string) (*connect.Response[compassv1.SpawnAgentResponse], error) {
	// The dedup-join lookup. A non-empty client_request_id memoizes the spawn, keyed
	// by (account, id): the first caller runs it, every retry for the SAME account
	// joins the entry. An empty id is not memoized. Keying on the account matches
	// provisionDedupID: an id reused across accounts is a distinct spawn.
	if crid != "" {
		key := spawnKey{account: string(acc.ID), crid: crid}
		call, joined := s.joinOrBeginSpawn(key)
		if call == nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("spawn: join returned no memo entry"))
		}
		if joined {
			// Joined an in-flight or completed spawn: wait for it to settle and
			// return its result — never a second Provision, never reject-on-live.
			return awaitSpawn(ctx, call)
		}
		resp, err := s.runSpawn(ctx, acc, crid)
		s.settleSpawn(key, call, resp, err)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(resp), nil
	}

	resp, err := s.runSpawn(ctx, acc, crid)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// awaitSpawn blocks until the joined composite spawn settles, returning its
// memoized result — or CodeCanceled if the joining caller's context is done
// first (the original beginner keeps running and still settles the entry). The
// read of call.resp/err is safe without the lock: it happens-after the
// close(call.done) settleSpawn issues, per the Go memory model.
func awaitSpawn(ctx context.Context, call *spawnCall) (*connect.Response[compassv1.SpawnAgentResponse], error) {
	select {
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		return connect.NewResponse(call.resp), nil
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	}
}

// runSpawn is the cache-miss body for the resolved agent acc: reject-on-live,
// then Provision, then Start, under the one client_request_id. It reuses the
// provision and start paths so the full orchestration (persona/role authority,
// placement, session ownership, rollback) is identical to the two-call human path.
func (s *service) runSpawn(ctx context.Context, acc store.Account, crid string) (*compassv1.SpawnAgentResponse, error) {
	// Pre-Provision reject-on-live: consult the Runner (authoritative for live
	// session truth) and reject if the target agent already holds one. Cache-miss
	// path only, BEFORE Provision, so a rejected spawn churns no container. Sourced
	// from the Runner, never Server in-memory state (which fails open after reconnect).
	if err := s.rejectIfAgentLive(ctx, string(acc.ID)); err != nil {
		return nil, err
	}

	provResp, err := s.provisionAgent(ctx, acc, &compassv1.ProvisionAgentWorkspaceRequest{
		ClientRequestId: crid,
	})
	if err != nil {
		return nil, err
	}
	container := provResp.GetContainerName()

	startResp, err := s.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: container,
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
// for key is already in flight or completed-successfully, or (fresh, false)
// after registering a new entry the caller must settle. Under s.spawnMu so two
// concurrent retries never both begin.
func (s *service) joinOrBeginSpawn(key spawnKey) (*spawnCall, bool) {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	if s.spawns == nil {
		s.spawns = make(map[spawnKey]*spawnCall)
	}
	if existing, ok := s.spawns[key]; ok {
		return existing, true
	}
	call := &spawnCall{done: make(chan struct{})}
	s.spawns[key] = call
	return call, false
}

// settleSpawn records a fresh spawn's outcome and wakes every joined retry. A
// FAILED entry is removed immediately so a retry re-attempts (DL-169) rather
// than replaying the failure; a SUCCESS is retained for spawnMemoTTL so a later
// sequential retry returns the same session (end-to-end idempotency) then
// evicted, bounding the memo to the in-flight + recent-retry window.
func (s *service) settleSpawn(key spawnKey, call *spawnCall, resp *compassv1.SpawnAgentResponse, err error) {
	s.spawnMu.Lock()
	call.resp = resp
	call.err = err
	if err != nil {
		delete(s.spawns, key)
	}
	s.spawnMu.Unlock()
	close(call.done)
	if err != nil {
		return
	}
	// Retain the success for the idempotency-replay window, then evict so the
	// memo does not grow one entry per successful spawn for the process lifetime.
	sched := s.scheduleAfter
	if sched == nil {
		sched = time.AfterFunc
	}
	sched(spawnMemoTTL, func() { s.evictSpawn(key, call) })
}

// evictSpawn drops a settled success entry once its idempotency-replay window
// has elapsed, removing only the exact call it settled (identity guard) so a
// distinct later spawn that re-registered the same key is never clobbered.
func (s *service) evictSpawn(key spawnKey, call *spawnCall) {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	if s.spawns[key] == call {
		delete(s.spawns, key)
	}
}
