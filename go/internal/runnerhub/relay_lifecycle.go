//go:build unix

// The agent-lifecycle Server leg: the RelayLifecycleCall resolution edge the
// Runner forwards each agent-initiated spawn/despawn call into (spawn/despawn
// record T4). It is the lifecycle sibling of the RelayCommsCall comms leg
// (relay_comms.go) and shares its trust model exactly.
//
// Trust model (the load-bearing security leg). The Runner is a pure forwarder:
// it sends RelayLifecycleCall{session_id, call} and asserts NO account. The
// SERVER resolves session_id -> caller agent account from THIS hub's own binding
// (the same binding RelayCommsCall resolves against) and delegates the call to
// the LifecycleCaller under that resolved caller account. An unknown, stopped,
// or reconnect-dropped session fails closed CodeNotFound: never a stale account,
// never the bootstrap admin. A session_id on the wire selects an account, it
// never carries one.
package runnerhub

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
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

// RelayLifecycleCall resolves the relayed session_id to its bound agent account
// (the CALLER) and delegates the spawn/despawn to the LifecycleCaller under that
// caller account. Guard order, each fail-closed: (1) no LifecycleCaller wired ->
// CodeUnavailable, checked BEFORE session resolution; (2) session_id resolves to
// no live binding -> CodeNotFound (never a stale account, never the bootstrap
// admin); (3) delegate under the RESOLVED caller AccountID (never request-
// asserted, never admin). A tool-level failure (unknown target, dup handle,
// self-despawn) is returned IN-BAND as the LifecycleCallError variant of the
// result — the agent renders it and the transport survives — exactly the
// RelayCommsCall split: only a resolution miss / no-caller is a Connect error.
func (h *Hub) RelayLifecycleCall(
	ctx context.Context,
	req *compassv1internal.RelayLifecycleCallRequest,
) (*compassv1internal.RelayLifecycleCallResponse, error) {
	h.mu.Lock()
	caller := h.lifecycleCaller
	h.mu.Unlock()
	if caller == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errLifecycleUnavailable)
	}
	account, ok := h.accountForSession(req.GetSessionId())
	if !ok {
		// Fail closed: no live session maps to this id. Never a stale account,
		// never the bootstrap admin — a hard CodeNotFound the Runner surfaces.
		return nil, connect.NewError(
			connect.CodeNotFound,
			errors.New("runnerhub: no agent account bound to session"),
		)
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
	switch c := call.GetCall().(type) {
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
			errors.New("runnerhub: lifecycle call has no spawn/despawn variant set"),
		)
	}
}

// lifecycleCallError maps a lifecycle execution error onto the in-band
// LifecycleCallError the agent renders. The code is the Connect status token
// (e.g. "already_exists" for a duplicate handle — the same collapse a human
// caller gets); the message is the error text. A non-Connect error is
// CodeUnknown's token.
func lifecycleCallError(err error) *compassv1internal.LifecycleCallError {
	return &compassv1internal.LifecycleCallError{
		Code:    connect.CodeOf(err).String(),
		Message: err.Error(),
	}
}
