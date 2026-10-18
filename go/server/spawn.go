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

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// errAgentAlreadyLive rejects a second spawn before provisioning an account
// that already owns a live session.
var errAgentAlreadyLive = errors.New("agent already has a live session")

// errAgentErrored requires explicit recovery before the account can be spawned again.
var errAgentErrored = errors.New("agent errored; reload or stop it")

// spawnMemoTTL bounds retry retention and prevents unbounded memo growth.
const spawnMemoTTL = 10 * time.Minute

// spawnKey binds a request ID to an agent account, preventing cross-agent joins.
type spawnKey struct {
	account string
	crid    string
}

// spawnCall stores the joined spawn outcome. Success supports retries; failure
// is removed so a retry attempts the operation again.
type spawnCall struct {
	done chan struct{}
	resp *compassv1.SpawnAgentResponse
	err  error
}

// SpawnAgent provisions an agent and starts its session under one request ID.
// It owns end-to-end idempotency and rejects live agents before provisioning;
// failures retain their Connect status and use Start's rollback path.
func (s *service) SpawnAgent(
	ctx context.Context,
	req *compassv1.SpawnAgentRequest,
) (*compassv1.SpawnAgentResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}

	// Resolved before the memo so a retry keys on the account, not the spelling:
	// a rename between attempts still joins the first spawn.
	acc, err := resolveQualifiedAgent(ctx, s.store, req.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	return s.spawnAccount(ctx, acc, req.GetClientRequestId())
}

// spawnAccount is SpawnAgent past the wire edge, for an already-resolved agent
// acc: the memo, reject-on-live, and runSpawn. The root-supervisor seed calls it
// directly with the account it already holds. The caller has checked s.hub.
func (s *service) spawnAccount(ctx context.Context, acc store.Account, crid string) (*compassv1.SpawnAgentResponse, error) {
	if err := runnerhub.CheckClientRequestID(crid); err != nil {
		return nil, err
	}
	// The dedup-join lookup. A non-empty client_request_id memoizes the spawn, keyed
	// by (account, id): the first caller runs it, every retry for the SAME account
	// joins the entry. An empty id is not memoized. Keying on the account matches
	// provisionDedupID: an id reused across accounts is a distinct spawn.
	if crid != "" {
		key := spawnKey{account: string(acc.ID), crid: crid}
		call, joined := s.joinOrBeginSpawn(key)
		if call == nil {
			return nil, connect.NewError(connect.CodeInternal, "spawn: join returned no memo entry")
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
		return resp, nil
	}

	resp, err := s.runSpawn(ctx, acc, crid)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// awaitSpawn returns the joined result or CodeCanceled if this caller leaves
// first; the original spawn continues. Closing done safely publishes resp and err.
func awaitSpawn(ctx context.Context, call *spawnCall) (*compassv1.SpawnAgentResponse, error) {
	select {
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		return call.resp, nil
	case <-ctx.Done():
		wrappedErr := ctx.Err()
		return nil, connect.NewError(connect.CodeCanceled, wrappedErr.Error()).WithCause(wrappedErr)
	}
}

// runSpawn is the cache-miss path: reject a live session, then provision and
// start under one request ID, reusing the existing placement and rollback paths.
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

	startResp, err := s.StartAgentSession(ctx, &compassv1.StartAgentSessionRequest{
		ContainerName: container,
	})
	if err != nil {
		return nil, err
	}

	return &compassv1.SpawnAgentResponse{
		SessionId:     startResp.GetSessionId(),
		ContainerName: container,
	}, nil
}

// rejectIfAgentLive returns CodeAlreadyExists when agentAccountID already holds a
// live session, or FailedPrecondition when its only matching sessions are errored.
// It scans the Runner's authoritative all-sessions status set, returned by
// GetAgentStatus with an empty session id. Relay failures fail closed.
func (s *service) rejectIfAgentLive(ctx context.Context, agentAccountID string) error {
	statuses, err := s.hub.Status(ctx, "", &compassv1.GetAgentStatusRequest{})
	if err != nil {
		return err
	}
	errored := false
	for _, st := range statuses.GetStatuses() {
		if st.GetAgentAccountId() != agentAccountID {
			continue
		}
		if st.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
			return connect.NewError(connect.CodeAlreadyExists, errAgentAlreadyLive.Error()).WithCause(errAgentAlreadyLive)
		}
		errored = true
	}
	if errored {
		return connect.NewError(connect.CodeFailedPrecondition, errAgentErrored.Error()).WithCause(errAgentErrored)
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

// settleSpawn publishes the result and wakes joined retries. Failed entries are
// removed for retry; successful entries expire after spawnMemoTTL.
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
	// Retain success through the idempotency-replay window, then evict it.
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
