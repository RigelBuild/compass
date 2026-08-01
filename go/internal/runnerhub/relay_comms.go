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
	"maps"

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
	h.accountSessions[account] = sessionID
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
	if account, ok := h.sessionAccounts[sessionID]; ok {
		// Drop the reverse entry only if it still points at THIS session — a
		// promoteSession for the account onto a newer session would have already
		// repointed it, and a stale delete would then unbind the live one.
		if h.accountSessions[account] == sessionID {
			delete(h.accountSessions, account)
		}
	}
	delete(h.sessionAccounts, sessionID)
}

// unbindContainer drops a container's provisioned account binding. Called from
// Remove, the teardown counterpart to Provision (which binds via bindContainer):
// on a Provision->Remove path that never reached Start, promoteSession never
// cleared the entry, so without this a stale container->account binding would
// linger and keep authorizing a pre-exec FetchSecrets materialize
// (HasContainerBinding) against a container that no longer exists. A no-op on
// the normal lifecycle (Start's promoteSession already cleared it).
func (h *Hub) unbindContainer(containerName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.containerAccounts, containerName)
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

// SessionForAccount resolves the LIVE session bound to an agent account — the
// REVERSE of accountForSession, the direction the delivery consumer (SEA-1569
// T3) needs to dispatch a deliver to a resolved subscriber. The bool is false
// when the account has no live session (never started, stopped, or dropped on a
// Runner reconnect): the consumer pushes nothing now and lets the D2 cursor
// sweep deliver on the recipient's next start (design.md:137). It satisfies the
// delivery.SessionResolver interface the consumer holds, kept separate from the
// ControlDispatcher (DispatchControl) so that stays the frozen dispatch-only
// shape.
func (h *Hub) SessionForAccount(account store.AccountID) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessionID, ok := h.accountSessions[account]
	return sessionID, ok
}

// LiveAgentSessions snapshots every live (agent account -> session) binding — the
// set the delivery consumer's lag-resync sweep iterates to redeliver owed
// messages to every live recipient (design.md:227-231). A copy under the lock,
// so the caller iterates without holding hub state; the map is empty when no
// session is live.
func (h *Hub) LiveAgentSessions() map[store.AccountID]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[store.AccountID]string, len(h.accountSessions))
	maps.Copy(out, h.accountSessions)
	return out
}

// HasLiveSession reports whether sessionID names a live session bound in the
// hub. It mirrors accountForSession's lock discipline but discards the account —
// the FetchSecrets authz check only needs "is this a session bound to the (one)
// enrolled Runner", not whose session it is. Under the inject-all + single-Runner
// MVP, a live binding in the hub IS a session bound to this Runner (there is
// exactly one), so this is the whole session-binding authz. The per-Runner
// differentiation — verifying the session belongs to THIS Runner among several —
// is the future multi-Runner seam (record §761-762); today there is one Runner,
// so membership in sessionAccounts is that check.
func (h *Hub) HasLiveSession(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessionAccounts[sessionID]
	return ok
}

// HasContainerBinding reports whether containerName has a recorded
// container→account binding — the Provision..Start window binding (bindContainer,
// cleared by promoteSession at Start and by clear() on re-enroll). It is the
// PROVISION-time analogue of HasLiveSession: FetchSecrets authorizes an initial
// pre-exec materialize against it, because no live session exists until Start.
// Under the inject-all + single-Runner MVP a recorded binding IS a container
// provisioned on the one enrolled Runner, so membership is the whole authz check
// (the per-Runner differentiation is the same future multi-Runner seam,
// record §761-762).
func (h *Hub) HasContainerBinding(containerName string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.containerAccounts[containerName]
	return ok
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

// CommitConversationFrame durably commits one agent-authored conversation frame
// under the agent account the relayed session resolves to, at most once keyed on
// the agent-minted idempotency_key — the DURABLE counterpart to the loss-
// tolerant Deliver/PublishEvents path (#24 / OQ-3). The Runner asserts no
// account; this resolves session_id -> account from the hub's own binding,
// exactly as RelayCommsCall does, and runs the commit through the CommsCaller
// under that account.
//
// Contract (ratified — do not redesign):
//   - An unresolved session fails closed CodeNotFound — never a stale account,
//     never the bootstrap admin. Same fail-closed shape as RelayCommsCall.
//   - A hub with no CommsCaller wired fails CodeUnavailable (a Deliver-only hub).
//   - committed=true on a fresh commit AND on an idempotent replay of an
//     already-committed key; the returned message_id is the ORIGINAL row's id,
//     stable across the replay. A non-commit is NEVER committed=false with a nil
//     error — it is ALWAYS a Connect status error, because the Runner drives
//     at-least-once purely off the Connect code (err==nil => committed).
//   - Comms errors are propagated AS-IS: the *AsAccount/CommitAgent* methods
//     already map through edgeError to proper Connect codes (InvalidArgument /
//     NotFound / FailedPrecondition), and the Runner splits retryable
//     (transient) from terminal (permanent) on that code. Never return a bare
//     error connect.CodeOf would report as CodeUnknown — that would wrongly read
//     as a retryable teardown.
//   - seq is deferred: shipped as 0 (the downstream consumer reads neither
//     message_id nor seq today), never widened into the store write path.
func (h *Hub) CommitConversationFrame(
	ctx context.Context,
	req *compassv1internal.CommitConversationFrameRequest,
) (*compassv1internal.CommitConversationFrameResponse, error) {
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

	key := req.GetIdempotencyKey()
	messageID, err := h.commitFrame(ctx, account, req.GetFrame(), key)
	if err != nil {
		// Already Connect-coded by the comms layer (edgeError). Propagate as-is
		// so the Runner's retryable/terminal split reads the right code.
		return nil, err
	}
	return &compassv1internal.CommitConversationFrameResponse{
		Committed: true,
		MessageId: messageID,
		Seq:       0, // Deferred (#24 contract): the consumer reads neither id nor seq today.
	}, nil
}

// commitFrame dispatches one conversation frame to the keyed commit path by its
// set oneof variant, returning the committed row's id (original and stable on an
// idempotent replay). A frame with neither the posted nor updated variant set is
// CodeInvalidArgument — the terminal "malformed frame" the Runner does not
// retry. Any comms error is returned as-is (already Connect-coded).
func (h *Hub) commitFrame(
	ctx context.Context,
	account store.AccountID,
	frame *compassv1internal.AgentFrame,
	idempotencyKey string,
) (string, error) {
	switch frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_ConversationPosted:
		resp, err := h.comms.CommitAgentPostKeyed(ctx, account, frame.GetConversationPosted(), idempotencyKey)
		if err != nil {
			return "", err
		}
		return resp.GetMessage().GetId(), nil
	case *compassv1internal.AgentFrame_ConversationUpdated:
		updated, err := h.comms.CommitAgentUpdateKeyed(ctx, account, frame.GetConversationUpdated(), idempotencyKey)
		if err != nil {
			return "", err
		}
		return updated.GetMessage().GetId(), nil
	default:
		return "", connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: conversation frame has no conversation_posted/conversation_updated variant set"),
		)
	}
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
