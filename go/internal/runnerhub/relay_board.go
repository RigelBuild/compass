//go:build unix

// The agent-board Server leg: the RelayBoardCall resolution edge, board sibling
// of the lifecycle and comms legs, sharing their trust model exactly.

// Trust model (load-bearing security leg): the Runner is a pure forwarder and
// asserts NO account. The SERVER resolves session_id -> caller account from this
// hub's binding and delegates under it. Unknown/stopped/dropped session fails
// closed CodeNotFound — never a stale account. A session_id selects, never carries.
package runnerhub

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// BoardCaller executes an agent-initiated board call (write an issue's canonical
// lifecycle state) as a resolved caller agent account. The server package
// implements it over the same transition executor a tracker- or auto-sourced
// transition takes, so the compare-and-transition, record+publish, and outbound
// mirror are identical — the hub depends only on this narrow surface so it does
// not pull the whole board service in (pattern: CommsCaller / LifecycleCaller).
// It is the safe Runner->Server leg: the caller account is resolved Server-side
// from the hub's own binding, never asserted by the Runner (agent primary
// lifecycle T3-a). The caller AccountID is recorded for attribution; MVP scope
// ships no scope rejection (single-trust-domain, Resolved decision 2).
type BoardCaller interface {
	SetIssueStateAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.SetIssueStateRequest) (*compassv1internal.SetIssueStateResponse, error)
}

// errBoardUnavailable is the fail-closed cause when a hub with no BoardCaller
// wired receives a RelayBoardCall. It maps to CodeUnavailable — the board write
// leg is not mounted, never a silent success.
var errBoardUnavailable = errors.New("runnerhub: no board caller wired to serve RelayBoardCall")

// RelayBoardCall resolves the relayed session_id to its bound agent account (the
// CALLER) and delegates the issue-state write to the BoardCaller under that
// caller account. Guard order, each fail-closed: (1) no BoardCaller wired ->
// CodeUnavailable, checked BEFORE session resolution; (2) session_id resolves to
// no live binding -> CodeNotFound (never a stale account, never the bootstrap
// admin); (3) delegate under the RESOLVED caller AccountID (never request-
// asserted, never admin). A tool-level failure (unknown issue, invalid target
// state) is returned IN-BAND as the BoardCallError variant of the result — the
// agent renders it and the transport survives — exactly the RelayLifecycleCall
// split: only a resolution miss / no-caller is a Connect error.
func (h *Hub) RelayBoardCall( //nolint:dupl // deliberate structural mirror of RelayLifecycleCall (relay_lifecycle.go): the sibling relay legs each spell out the same fail-closed guard order (nil-caller CodeUnavailable, unbound-session CodeNotFound, in-band tool error) so the security-critical resolution edge reads identically per leg — collapsing them into one generic helper is the "second convention" this leg is required not to invent.
	ctx context.Context,
	req *compassv1internal.RelayBoardCallRequest,
) (*compassv1internal.RelayBoardCallResponse, error) {
	h.mu.Lock()
	caller := h.boardCaller
	h.mu.Unlock()
	if caller == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errBoardUnavailable)
	}
	account, ok := h.accountForSession(ctx, req.GetSessionId())
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
	result, err := h.executeBoardCall(ctx, caller, account, call)
	if err != nil {
		// A tool-level failure (or a malformed call) is in-band: the agent gets a
		// BoardCallError it renders to the model, and the transport survives. Only
		// a resolution miss / no-caller (above) is a Connect error.
		return &compassv1internal.RelayBoardCallResponse{
			Result: &compassv1internal.BoardCallResult{
				CallId: callID,
				Result: &compassv1internal.BoardCallResult_Error{Error: boardCallError(err)},
			},
		}, nil
	}
	result.CallId = callID
	return &compassv1internal.RelayBoardCallResponse{Result: result}, nil
}

// executeBoardCall dispatches one board call to the BoardCaller under account,
// returning the typed success result (call_id unset — the caller stamps it) or a
// non-nil error to be rendered in-band. An unset or unrecognized call oneof is
// an invalid request.
func (h *Hub) executeBoardCall(
	ctx context.Context,
	caller BoardCaller,
	account store.AccountID,
	call *compassv1internal.BoardCallRequest,
) (*compassv1internal.BoardCallResult, error) {
	oneof := call.GetCall()
	if oneof == nil {
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: board call has no set_issue_state variant set"),
		)
	}
	switch c := oneof.(type) {
	case *compassv1internal.BoardCallRequest_SetIssueState:
		resp, err := caller.SetIssueStateAsAccount(ctx, account, c.SetIssueState)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.BoardCallResult{
			Result: &compassv1internal.BoardCallResult_SetIssueState{SetIssueState: resp},
		}, nil
	default:
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: board call has no set_issue_state variant set"),
		)
	}
}

// boardCallError maps a board execution error onto the in-band BoardCallError
// the agent renders. The code is the Connect status token (e.g. "not_found" for
// an unknown issue, "invalid_argument" for an UNSPECIFIED target); the message
// is the error text. A non-Connect error is CodeUnknown's token.
func boardCallError(err error) *compassv1internal.BoardCallError {
	return &compassv1internal.BoardCallError{
		Code:    connect.CodeOf(err).String(),
		Message: err.Error(),
	}
}
