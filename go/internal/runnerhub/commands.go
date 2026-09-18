//go:build unix

// The Server-facing command surface: the CompassService session RPCs route here,
// each dispatching to the owning Runner over the Sessions relay and mapping the
// RunnerError result to a Connect status. A client_request_id is reused so a
// timeout-retry dedupes to the original result (no duplicate container).
package runnerhub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// Provision relays a ProvisionAgentWorkspace command to the owning Runner and
// returns the container name it created plus the id of the Runner that served
// the call. The Runner id is returned rather than looked up afterwards so the
// caller records the agent's durable placement against the Runner that ACTUALLY
// ran the provision — re-reading the registry after the round trip could name a
// different Runner if one re-enrolled in the meantime, and a placement pointing
// at the wrong Runner is worse than none (reattach would re-drive the wrong
// set). requestID is the client_request_id idempotency key: a timeout-retry with
// the same id returns the same container (no duplicate); empty mints a fresh id
// (no dedup).
//
// The client_request_id alone must NOT be the dedup key: it is a client-chosen
// string, and two provisions that reuse one value for DIFFERENT agent accounts
// are distinct operations — joining them would hand the second caller a
// container provisioned for the first's account. So a non-empty id is scoped to
// the agent account: a genuine retry resends the identical request and dedups,
// while a reused id for a different account derives a distinct correlation id
// and does not join. The cross-account boundary is the invariant that remains.
// This mirrors the comms store's (author_account_id, client_request_id)
// idempotency scoping (store/migrations/0001_init.sql), keyed to the agent
// account the provision creates an isolated container for.
func (h *Hub) Provision(ctx context.Context, requestID string, req *compassv1.ProvisionAgentWorkspaceRequest) (*compassv1.ProvisionAgentWorkspaceResponse, string, error) {
	result, runnerID, err := h.relay(ctx, "", &compassv1internal.SessionsResponse{
		RequestId: provisionDedupID(requestID, req),
		Command:   &compassv1internal.SessionsResponse_Provision{Provision: req},
	})
	if err != nil {
		return nil, "", err
	}
	resp := result.GetProvision()
	// Record which agent account this container was provisioned for, so a later
	// Start can promote it to a session binding RelayCommsCall resolves against.
	// The Runner never asserts this account; it is the Server's own record, keyed
	// by the container name. Live comms binding only, cleared on re-enroll.
	h.bindContainer(resp.GetContainerName(), store.AccountID(req.GetAgentHandle()))
	return resp, runnerID, nil
}

// Start relays a StartAgentSession command to the owning Runner.
func (h *Hub) Start(ctx context.Context, requestID string, req *compassv1.StartAgentSessionRequest) (*compassv1.StartAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetContainerName(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Start{Start: req},
	})
	if err != nil {
		return nil, err
	}
	resp := result.GetStart()
	// Promote the container's provisioned account binding onto the live session
	// id the Runner minted, so RelayCommsCall for this session resolves the
	// agent account (comms-tools design T2). A container with no recorded
	// account leaves no session binding, and its comms calls fail closed.
	//
	// RIG-3696: a REFUSED promotion (the session id is durably owned by another
	// account) leaves the session started on the Runner but bindable to nobody,
	// so it is rolled back rather than left running unbound — the alternative is
	// a live agent whose comms calls resolve nowhere and which no Stop reaps.
	if promoteErr := h.promoteSession(ctx, req.GetContainerName(), resp.GetSessionId()); promoteErr != nil {
		return nil, h.rollbackStartedSession(ctx, resp.GetSessionId(), promoteErr)
	}
	// The initial secret materialize no longer rides a signal: the Runner
	// materializes the container's set pre-exec at Start, before the agent runs.
	// The SecretsVersion signal is now the T6 ROTATION path only.
	return resp, nil
}

// Stop relays a StopAgentSession command to the owning Runner.
func (h *Hub) Stop(ctx context.Context, requestID string, req *compassv1.StopAgentSessionRequest) (*compassv1.StopAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetSessionId(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Stop{Stop: req},
	})
	if err != nil {
		return nil, err
	}
	// Drop the session's account binding: a RelayCommsCall for a stopped session
	// fails closed CodeNotFound, the same answer as a never-seen session — never
	// a stale reuse.
	h.unbindSession(ctx, req.GetSessionId())
	return result.GetStop(), nil
}

// Remove relays a RemoveAgentWorkspace command to the owning Runner — the
// teardown counterpart to Provision, tearing down the per-agent container the
// container_name names. Idempotent: removing an unknown/already-removed
// container succeeds (the Runner's contract). The request id is the caller's
// client_request_id (empty mints a fresh id, no dedup) — Remove is idempotent
// Runner-side, so it needs no cross-account dedup derivation (unlike Provision).
func (h *Hub) Remove(ctx context.Context, requestID string, req *compassv1.RemoveAgentWorkspaceRequest) (*compassv1.RemoveAgentWorkspaceResponse, error) {
	result, _, err := h.relay(ctx, req.GetContainerName(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Remove{Remove: req},
	})
	if err != nil {
		return nil, err
	}
	// Drop the container's provisioned account binding — Provision bound it and a
	// Remove that never went through Start (promoteSession clears it there) would
	// otherwise leave a stale binding authorizing a pre-exec secrets materialize.
	h.unbindContainer(req.GetContainerName())
	return result.GetRemove(), nil
}

// Reload relays a ReloadAgentSession command to the owning Runner.
func (h *Hub) Reload(ctx context.Context, requestID string, req *compassv1.ReloadAgentSessionRequest) (*compassv1.ReloadAgentSessionResponse, error) {
	result, _, err := h.relay(ctx, req.GetSessionId(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Reload{Reload: req},
	})
	if err != nil {
		return nil, err
	}
	return result.GetReload(), nil
}

// Status relays a GetAgentStatus command to the owning Runner — the Runner is
// authoritative for live session truth, so the Server reconciles to its answer.
func (h *Hub) Status(ctx context.Context, requestID string, req *compassv1.GetAgentStatusRequest) (*compassv1.GetAgentStatusResponse, error) {
	result, _, err := h.relay(ctx, req.GetSessionId(), &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(requestID),
		Command:   &compassv1internal.SessionsResponse_Status{Status: req},
	})
	if err != nil {
		return nil, err
	}
	return result.GetStatus(), nil
}

// SessionState resolves a live session's lifecycle state through the Runner
// Status relay (GetAgentStatus) — the reconciliation input the RIG-1569 T8
// presence projection rebuilds from at a session promotion (design.md:494-503).
// The Runner is authoritative for live session truth, so a restart reconstructs
// presence from its answer rather than from any lost in-memory state. ok is
// false when the relay fails or returns no status for the session (the
// reconstruction falls to OFFLINE): a reconciliation edge must never tear
// anything down, so a relay error is a soft "unknown", not a propagated failure.
// Satisfies presence.LifecycleStatusResolver.
func (h *Hub) SessionState(ctx context.Context, sessionID string) (compassv1.AgentSessionState, bool) {
	resp, err := h.Status(ctx, "", &compassv1.GetAgentStatusRequest{SessionId: sessionID})
	if err != nil {
		return compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED, false
	}
	for _, st := range resp.GetStatuses() {
		if st.GetSessionId() == sessionID {
			return st.GetState(), true
		}
	}
	// A single-session Status request returns that session's status; adopt the
	// sole status ONLY when it carries no session id. A sole status with a
	// non-empty MISMATCHED id is a Runner bug and must not reconstruct a wrong
	// presence, so it is unresolved (ok=false → OFFLINE). Absent status likewise.
	if s := resp.GetStatuses(); len(s) == 1 && s[0].GetSessionId() == "" {
		return s[0].GetState(), true
	}
	return compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED, false
}

// relay dispatches one built command through the owning Runner's router and maps
// the outcome to a Connect status: a RunnerError result becomes the mapped
// Connect code; a transport failure (no Runner, stream drop) becomes
// Unavailable. sessionKey selects the owning Runner (single-Runner MVP: any
// non-empty key resolves the one Runner). The served Runner's id is returned
// alongside the result for the one caller that must attribute the command to a
// Runner (Provision, recording a durable placement); the rest discard it.
func (h *Hub) relay(ctx context.Context, sessionKey string, cmd *compassv1internal.SessionsResponse) (*compassv1internal.SessionsRequest, string, error) {
	router, runnerID, err := h.routerFor(sessionKey)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeUnavailable, err)
	}
	result, err := router.dispatch(ctx, cmd)
	if err != nil {
		return nil, "", connect.NewError(connect.CodeUnavailable, err)
	}
	if runnerErr := result.GetError(); runnerErr != nil {
		return nil, "", runnerErrorToConnect(runnerErr)
	}
	return result, runnerID, nil
}

// rollbackStartedSession tears down a session the Runner already started but
// whose account binding was REFUSED (RIG-3696), and returns the error the Start
// fails with. Used by both start legs (Start and StartResume): a started session
// no binding can authorize is a live agent whose comms calls resolve nowhere,
// so it is stopped rather than stranded.
//
// It relays the Stop command DIRECTLY rather than calling Hub.Stop, and that is
// the load-bearing choice. Hub.Stop follows its relay with unbindSession, which
// DELETES the durable session_bindings row by session id — and on this path that
// row belongs to the OTHER account, the durable owner whose ownership is the
// exact reason the promotion was refused. Stopping through Hub.Stop would
// therefore destroy the binding the conflict protected, converting a fail-closed
// refusal into the silent unbind it exists to prevent. The refused promotion
// wrote nothing to this hub's maps either, so there is no cache state to evict:
// a bare relay is the whole rollback.
//
// The Stop runs on context.WithoutCancel(ctx) bounded by rollbackStopTimeout:
// the session is live regardless of the caller's context, and the dispatch path
// has no deadline of its own, so an unbounded Stop against a Runner that accepts
// the command but never answers would hang the Start
// (server/service.go abandonStartedSession takes the same posture for the same
// reason). A Stop failure is folded into the returned error with errors.Join
// rather than masking the conflict: the session is then running unreapable, and
// only an operator who can see BOTH causes can clean it up.
func (h *Hub) rollbackStartedSession(ctx context.Context, sessionID string, cause error) error {
	if sessionID == "" {
		// The Runner answered with no session id, so there is nothing to stop —
		// and nothing was started under an id anyone can name.
		return connect.NewError(connect.CodeInternal, cause)
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackStopTimeout)
	defer cancel()
	_, _, stopErr := h.relay(stopCtx, sessionID, &compassv1internal.SessionsResponse{
		RequestId: orNewRequestID(""),
		Command: &compassv1internal.SessionsResponse_Stop{
			Stop: &compassv1.StopAgentSessionRequest{SessionId: sessionID},
		},
	})
	if stopErr != nil {
		h.log.Error("session binding was refused and the started session could not be stopped; it is running unreapable",
			"session_id", sessionID, "cause", cause, "stop_error", stopErr)
		return connect.NewError(connect.CodeInternal, errors.Join(cause, fmt.Errorf("stopping the started session %q: %w", sessionID, stopErr)))
	}
	h.log.Error("session binding was refused; stopped the started session to avoid stranding it",
		"session_id", sessionID, "cause", cause)
	return connect.NewError(connect.CodeInternal, cause)
}

// rollbackStopTimeout bounds the best-effort Stop in rollbackStartedSession. The
// runnerhub dispatch path has no deadline of its own, so without this bound a
// Runner that accepts the Stop command and never answers would hang the Start
// that is rolling back. Mirrors server/service.go's rollbackStopTimeout, the
// same bound on the same class of teardown. A var so a test can shorten it.
var rollbackStopTimeout = 30 * time.Second

// runnerErrorToConnect maps a RunnerError to the Connect status the client sees.
func runnerErrorToConnect(e *compassv1internal.RunnerError) error {
	var code connect.Code
	switch e.GetCode() {
	case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_ALREADY_RUNNING:
		code = connect.CodeAlreadyExists
	case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND:
		code = connect.CodeNotFound
	case compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_FAILED_PRECONDITION:
		code = connect.CodeFailedPrecondition
	default:
		code = connect.CodeInternal
	}
	return connect.NewError(code, fmt.Errorf("runner: %s", e.GetMessage()))
}

// orNewRequestID returns id when non-empty, else a fresh random correlation id.
func orNewRequestID(id string) string {
	if id != "" {
		return id
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// provisionDedupID derives the correlation/dedup id for a provision. An empty
// client_request_id mints a fresh random id (no dedup, per the contract). A
// non-empty id is bound to the agent account, so a retry of the SAME provision
// dedups while the same client_request_id reused for a DIFFERENT agent account
// derives a distinct id and does not join. The derivation is a domain-separated
// SHA-256 over length-prefixed fields, so no field's value can be shifted into
// another to forge a collision.
func provisionDedupID(clientRequestID string, req *compassv1.ProvisionAgentWorkspaceRequest) string {
	if clientRequestID == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
	h := sha256.New()
	for _, field := range []string{
		"compass.provision.v1", // domain separator
		clientRequestID,
		req.GetAgentHandle(),
	} {
		var lp [8]byte
		binary.BigEndian.PutUint64(lp[:], uint64(len(field)))
		h.Write(lp[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}
