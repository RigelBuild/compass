//go:build unix

// The agent-board Server leg: the RelayBoardCall resolution edge, board sibling
// of the lifecycle and comms legs, sharing their trust model exactly.

// Trust model (load-bearing security leg): the Runner is a pure forwarder and
// asserts NO account. The SERVER resolves session_id -> caller account from this
// hub's binding and delegates under it. An unbound session fails closed with a
// generic FailedPrecondition so unknown and foreign session ids are identical.
package runnerhub

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

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

// RelayBoardCall resolves the caller account and delegates the issue-state write.
// Unbound sessions fail generically; unverifiable bindings are retryable.
func (h *Hub) RelayBoardCall( //nolint:dupl // mirrored security guard order in RelayLifecycleCall.
	ctx context.Context,
	runnerID string,
	req *compassv1internal.RelayBoardCallRequest,
) (*compassv1internal.RelayBoardCallResponse, error) {
	h.mu.Lock()
	caller := h.boardCaller
	h.mu.Unlock()
	if caller == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errBoardUnavailable.Error()).WithCause(errBoardUnavailable)
	}
	ctx, account, err := h.runnerSessionAccountError(ctx, runnerID, req.GetSessionId())
	if err != nil {
		return nil, err
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
			"runnerhub: board call has no set_issue_state variant set",
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
			"runnerhub: board call has no set_issue_state variant set",
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
