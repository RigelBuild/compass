//go:build unix

// The agent-comms Server leg: the session->account binding lifecycle and the
// RelayCommsCall handler the Runner forwards each agent-initiated comms call
// into (transport design T3 -> comms-tools design T2).
//
// Trust model (OQ-2, ratified — the load-bearing security leg). The Runner is a
// pure forwarder: it sends RelayCommsCall{session_id, call} and asserts NO
// account. The SERVER resolves session_id -> agent account from THIS hub's own
// binding — recorded from the Provision request's agent_account_id, promoted to
// the minted session_id at Start — and executes the call under that account via
// the CommsCaller (which sets comms.WithActor in-process). An unknown, stopped,
// or reconnect-dropped session fails closed CodeNotFound: never a stale account,
// never the bootstrap-admin fallback. The binding is authoritative Server-side
// state; a session_id on the wire selects an account, it never carries one.
package runnerhub

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// bindContainer records that container_name was provisioned for agentAccountID.
// Called from Provision with the request's agent_account_id. Start later
// promotes this to a session binding under the minted session_id. An empty
// account or container is ignored — a provision that named no account cannot
// bind one (the comms call it would later serve fails closed instead).
func (h *Hub) bindContainer(containerName string, agentAccountID store.AccountID) {
	if containerName == "" || agentAccountID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.containerAccounts[containerName] = agentAccountID
}

// promoteSession moves the container's provisioned account binding onto the live
// session_id Start minted. Called from Start after the Runner returns the
// session id. If the container had no recorded account (a provision that named
// none, or a container from before this leg existed), no session binding is
// created and a later comms call for that session fails closed CodeNotFound.
func (h *Hub) promoteSession(containerName, sessionID string) {
	if containerName == "" || sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	account, ok := h.containerAccounts[containerName]
	if !ok {
		return
	}
	h.sessionAccounts[sessionID] = account
	// The container->account entry has served its purpose; the session binding
	// is now authoritative. Drop it so a container name reused across the
	// Runner's life cannot resurrect a stale account (reconnect clears both maps
	// anyway; this keeps the pre-Start map tight in the meantime).
	delete(h.containerAccounts, containerName)
}

// unbindSession removes a session's account binding. Called from Stop, so a
// RelayCommsCall for a stopped session_id fails closed CodeNotFound — the same
// answer as a never-seen session, never a stale reuse.
func (h *Hub) unbindSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessionAccounts, sessionID)
}

// accountForSession resolves the agent account bound to sessionID. The bool is
// false when no live binding exists (never provisioned, stopped, or dropped on a
// Runner reconnect) — the fail-closed signal RelayCommsCall turns into
// CodeNotFound.
func (h *Hub) accountForSession(sessionID string) (store.AccountID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	account, ok := h.sessionAccounts[sessionID]
	return account, ok
}

// errCommsUnavailable is the fail-closed cause when a hub with no CommsCaller
// wired receives a RelayCommsCall (a Deliver-only hub). It maps to
// CodeUnavailable — the comms leg is not mounted, never a silent success.
var errCommsUnavailable = errors.New("runnerhub: no comms caller wired to serve RelayCommsCall")

// RelayCommsCall executes one agent-initiated comms call under the agent account
// the relayed session resolves to. The Runner asserts no account; this resolves
// session_id -> account from the hub's own binding and runs the call through the
// CommsCaller under that account. An unresolved session fails closed
// CodeNotFound. A comms failure (non-member channel, bad input, transport) is
// mapped to the in-band CommsCallError variant of CommsCallResult — a tool
// error the agent renders, NOT a Connect stream error that would tear the
// transport down.
func (h *Hub) RelayCommsCall(
	ctx context.Context,
	req *compassv1internal.RelayCommsCallRequest,
) (*compassv1internal.RelayCommsCallResponse, error) {
	if h.comms == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errCommsUnavailable)
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
	result, err := h.executeCall(ctx, account, call)
	if err != nil {
		// A tool-level failure is in-band: the agent gets a CommsCallError it
		// renders to the model, and the transport survives. Only a resolution
		// miss (above) is a Connect error.
		return &compassv1internal.RelayCommsCallResponse{
			Result: &compassv1internal.CommsCallResult{
				CallId: callID,
				Result: &compassv1internal.CommsCallResult_Error{Error: commsCallError(err)},
			},
		}, nil
	}
	result.CallId = callID
	return &compassv1internal.RelayCommsCallResponse{Result: result}, nil
}

// executeCall dispatches one comms call to the CommsCaller under account,
// returning the typed success result (call_id unset — the caller stamps it) or a
// non-nil error to be rendered in-band. An unset or unrecognized call oneof is an
// invalid request.
func (h *Hub) executeCall(
	ctx context.Context,
	account store.AccountID,
	call *compassv1internal.CommsCallRequest,
) (*compassv1internal.CommsCallResult, error) {
	switch c := call.GetCall().(type) {
	case *compassv1internal.CommsCallRequest_Post:
		resp, err := h.comms.PostAsAccount(ctx, account, c.Post)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_Post{Post: resp},
		}, nil
	case *compassv1internal.CommsCallRequest_List:
		resp, err := h.comms.ListAsAccount(ctx, account, c.List)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_List{List: resp},
		}, nil
	default:
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: comms call has no post/list variant set"),
		)
	}
}

// commsCallError maps a comms execution error onto the in-band CommsCallError
// the agent renders. The code is the Connect status token (e.g. "not_found" for
// a non-member channel — the D9 collapse a human caller also gets); the message
// is the error text. A non-Connect error is CodeUnknown's token.
func commsCallError(err error) *compassv1internal.CommsCallError {
	return &compassv1internal.CommsCallError{
		Code:    connect.CodeOf(err).String(),
		Message: err.Error(),
	}
}
