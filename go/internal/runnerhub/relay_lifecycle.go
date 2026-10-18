//go:build unix

// The agent-lifecycle Server leg: the RelayLifecycleCall resolution edge, sibling
// of the comms leg, sharing its trust model exactly.

// Trust model (load-bearing security leg): the Runner is a pure forwarder and
// asserts NO account. The SERVER resolves session_id -> caller account and
// fails closed with a generic FailedPrecondition; unverifiable bindings are
// Unavailable, which the agent may retry.
package runnerhub

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// LifecycleCaller executes an agent-initiated lifecycle call (spawn/despawn a
// peer) as a resolved caller agent account. The server package implements it
// over the same provisioning paths a human-initiated spawn takes, so authz,
// naming, and container placement are identical — the hub depends only on this
// narrow surface so it does not pull the whole lifecycle service in (pattern:
// CommsCaller). It is the safe Runner->Server leg: the caller account is
// resolved Server-side from the hub's own binding, never asserted by the Runner
// (spawn/despawn record T4, mirroring the comms-tools OQ-2 ratification). T5
// implements it in the server package.
type LifecycleCaller interface {
	SpawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
	DespawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.DespawnPeerRequest) (*compassv1internal.DespawnPeerResponse, error)
}

// errLifecycleUnavailable is the fail-closed cause when a hub with no
// LifecycleCaller wired receives a RelayLifecycleCall. It maps to
// CodeUnavailable — the lifecycle leg is not mounted, never a silent success.
var errLifecycleUnavailable = errors.New("runnerhub: no lifecycle caller wired to serve RelayLifecycleCall")

// RelayLifecycleCall resolves the caller account and delegates the lifecycle call.
// Unbound sessions fail generically; unverifiable bindings are retryable.
func (h *Hub) RelayLifecycleCall( //nolint:dupl // mirrored security guard order in RelayBoardCall.
	ctx context.Context,
	runnerID string,
	req *compassv1internal.RelayLifecycleCallRequest,
) (*compassv1internal.RelayLifecycleCallResponse, error) {
	h.mu.Lock()
	caller := h.lifecycleCaller
	h.mu.Unlock()
	if caller == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errLifecycleUnavailable.Error()).WithCause(errLifecycleUnavailable)
	}
	ctx, account, err := h.runnerSessionAccountError(ctx, runnerID, req.GetSessionId())
	if err != nil {
		return nil, err
	}

	call := req.GetCall()
	callID := call.GetCallId()
	result, err := h.executeLifecycleCall(ctx, caller, account, call)
	if err != nil {
		// A tool-level failure (or a malformed call) is in-band: the agent gets a
		// LifecycleCallError it renders to the model, and the transport survives.
		// Only a resolution miss / no-caller (above) is a Connect error.
		return &compassv1internal.RelayLifecycleCallResponse{
			Result: &compassv1internal.LifecycleCallResult{
				CallId: callID,
				Result: &compassv1internal.LifecycleCallResult_Error{Error: lifecycleCallError(err)},
			},
		}, nil
	}
	result.CallId = callID
	return &compassv1internal.RelayLifecycleCallResponse{Result: result}, nil
}

// executeLifecycleCall dispatches one lifecycle call to the LifecycleCaller
// under account, returning the typed success result (call_id unset — the caller
// stamps it) or a non-nil error to be rendered in-band. An unset or unrecognized
// call oneof is an invalid request.
func (h *Hub) executeLifecycleCall(
	ctx context.Context,
	caller LifecycleCaller,
	account store.AccountID,
	call *compassv1internal.LifecycleCallRequest,
) (*compassv1internal.LifecycleCallResult, error) {
	oneof := call.GetCall()
	if oneof == nil {
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			"runnerhub: lifecycle call has no spawn/despawn variant set",
		)
	}
	switch c := oneof.(type) {
	case *compassv1internal.LifecycleCallRequest_Spawn:
		resp, err := caller.SpawnAsAccount(ctx, account, c.Spawn)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.LifecycleCallResult{
			Result: &compassv1internal.LifecycleCallResult_Spawn{Spawn: resp},
		}, nil
	case *compassv1internal.LifecycleCallRequest_Despawn:
		resp, err := caller.DespawnAsAccount(ctx, account, c.Despawn)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.LifecycleCallResult{
			Result: &compassv1internal.LifecycleCallResult_Despawn{Despawn: resp},
		}, nil
	default:
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			"runnerhub: lifecycle call has no spawn/despawn variant set",
		)
	}
}

// lifecycleCallError maps execution failures to the in-band error rendered by the agent.
// Connect failures retain their status token; other errors use Unknown.
func lifecycleCallError(err error) *compassv1internal.LifecycleCallError {
	return &compassv1internal.LifecycleCallError{
		Code:    connect.CodeOf(err).String(),
		Message: err.Error(),
	}
}
