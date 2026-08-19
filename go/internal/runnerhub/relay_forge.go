//go:build unix

// The agent-forge Server leg: the RelayForgeCall resolution edge the Runner
// forwards each agent-initiated forge call into (Compass forge write path T5).
// It is the forge sibling of the RelayBoardCall board leg (relay_board.go), the
// RelayLifecycleCall lifecycle leg (relay_lifecycle.go), and the RelayCommsCall
// comms leg (relay_comms.go), and shares their trust model exactly.
//
// Trust model (the load-bearing security leg). The Runner is a pure forwarder:
// it sends RelayForgeCall{session_id, call} and asserts NO account. The SERVER
// resolves session_id -> caller agent account from THIS hub's own binding (the
// same binding RelayBoardCall/RelayLifecycleCall/RelayCommsCall resolve against)
// and delegates the call to the ForgeCaller under that resolved caller account,
// passing the session id through (the ForgeCaller stamps the owner header from
// it). An unknown, stopped, or reconnect-dropped session fails closed
// CodeNotFound: never a stale account, never the bootstrap admin. A session_id
// on the wire selects an account, it never carries one.
package runnerhub

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// ForgeCaller executes an agent-initiated forge call (create/comment/get/list an
// issue or PR, submit a review) as a resolved caller agent account. The forge
// service (T4) implements it over the write chokepoint that stamps the owner
// header, dispatches the call oneof, and enforces the body limit — the hub
// depends only on this narrow surface so it does not pull the whole forge
// service in (pattern: BoardCaller / LifecycleCaller / CommsCaller). It is the
// safe Runner->Server leg: the caller account is resolved Server-side from the
// hub's own binding, never asserted by the Runner (Compass forge write path T5).
// The signature carries the resolved caller AccountID for attribution AND the
// session id through, because the chokepoint interpolates the session id into
// the owner header it stamps. MVP scope ships no scope rejection
// (single-trust-domain, Resolved decision 2). A tool-level failure is returned
// IN-BAND inside the ForgeCallResult error arm, never as a Go error torn down
// onto the transport.
type ForgeCaller interface {
	ExecuteForgeCallAsAccount(ctx context.Context, caller store.AccountID, sessionID string, call *compassv1internal.ForgeCallRequest) (*compassv1internal.ForgeCallResult, error)
}

// errForgeUnavailable is the fail-closed cause when a hub with no ForgeCaller
// wired receives a RelayForgeCall. It maps to CodeUnavailable — the forge write
// leg is not mounted, never a silent success.
var errForgeUnavailable = errors.New("runnerhub: no forge caller wired to serve RelayForgeCall")

// errForgeNoResult is the cause when a wired ForgeCaller returns a nil result on
// the nil-error arm — a malformed reply, surfaced in-band as CodeInternal rather
// than nil-dereferenced on the resolution edge.
var errForgeNoResult = errors.New("runnerhub: forge caller returned no result")

// SetForgeCaller wires the forge execution seam after construction, so no NewHub
// caller signature changes and the hub<->forgeService construction cycle (the
// service needs the store + provider registry; the hub needs the service to
// serve RelayForgeCall) is broken exactly as SetBoardCaller breaks the board
// cycle. A hub with none wired fails RelayForgeCall closed CodeUnavailable.
// Wired under mu; read under mu.
func (h *Hub) SetForgeCaller(c ForgeCaller) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgeCaller = c
}

// RelayForgeCall resolves the relayed session_id to its bound agent account (the
// CALLER) and delegates the forge call to the ForgeCaller under that caller
// account, passing the session id through. Guard order, each fail-closed: (1) no
// ForgeCaller wired -> CodeUnavailable, checked BEFORE session resolution; (2)
// session_id resolves to no live binding -> CodeNotFound (never a stale account,
// never the bootstrap admin); (3) delegate under the RESOLVED caller AccountID
// (never request-asserted, never admin), passing the session id through. A
// tool-level failure (unknown coordinate, rate limit, bad input) is returned
// IN-BAND as the ForgeCallError variant of the result — the agent renders it and
// the transport survives — exactly the RelayBoardCall split: only a resolution
// miss / no-caller is a Connect error.
func (h *Hub) RelayForgeCall(
	ctx context.Context,
	req *compassv1internal.RelayForgeCallRequest,
) (*compassv1internal.RelayForgeCallResponse, error) {
	h.mu.Lock()
	caller := h.forgeCaller
	h.mu.Unlock()
	if caller == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errForgeUnavailable)
	}
	sessionID := req.GetSessionId()
	account, ok := h.accountForSession(sessionID)
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
	result, err := caller.ExecuteForgeCallAsAccount(ctx, account, sessionID, call)
	if err != nil {
		// A tool-level failure (or a malformed call) is in-band: the agent gets a
		// ForgeCallError it renders to the model, and the transport survives. Only
		// a resolution miss / no-caller (above) is a Connect error.
		return &compassv1internal.RelayForgeCallResponse{
			Result: &compassv1internal.ForgeCallResult{
				CallId: callID,
				Result: &compassv1internal.ForgeCallResult_Error{Error: forgeCallError(err)},
			},
		}, nil
	}
	if result == nil {
		// Defensive: a ForgeCaller must return a non-nil result on the nil-error
		// arm (the sibling legs get this for free from their internal executor,
		// which always builds a fresh result; the forge leg calls the external
		// ForgeCaller directly). A (nil, nil) return is a malformed reply, not a
		// tool failure — surface it in-band as CodeInternal rather than nil-deref
		// on this security-critical resolution edge.
		return &compassv1internal.RelayForgeCallResponse{
			Result: &compassv1internal.ForgeCallResult{
				CallId: callID,
				Result: &compassv1internal.ForgeCallResult_Error{
					Error: forgeCallError(connect.NewError(connect.CodeInternal, errForgeNoResult)),
				},
			},
		}, nil
	}
	result.CallId = callID
	return &compassv1internal.RelayForgeCallResponse{Result: result}, nil
}

// forgeCallError maps a forge execution error onto the in-band ForgeCallError
// the agent renders. The code is the Connect status token (e.g. "not_found" for
// an unknown coordinate, "resource_exhausted" for a rate limit); the message is
// the error text. A non-Connect error is CodeUnknown's token.
func forgeCallError(err error) *compassv1internal.ForgeCallError {
	return &compassv1internal.ForgeCallError{
		Code:    connect.CodeOf(err).String(),
		Message: err.Error(),
	}
}
