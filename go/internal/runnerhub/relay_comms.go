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

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
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
	account, ok := h.containerAccounts[containerName]
	if !ok {
		h.mu.Unlock()
		return
	}
	h.sessionAccounts[sessionID] = account
	h.accountSessions[account] = sessionID
	// The container->account entry has served its purpose; the session binding
	// is now authoritative. Drop it so a container name reused across the
	// Runner's life cannot resurrect a stale account (reconnect clears both maps
	// anyway; this keeps the pre-Start map tight in the meantime).
	delete(h.containerAccounts, containerName)
	// Read both after-binding sinks under mu so a setter and this arm never race,
	// then release BEFORE firing either: each sink only enqueues into its own
	// consumer/component loop and returns promptly, so promoteSession never blocks
	// on store work and never holds h.mu across a sink call (mirrors the settle
	// edge at deliverSession). Both nil-safe (a hub with neither wired is today's
	// behavior — SEA-1569 T6 session-start, T8 presence).
	sessionStart := h.sessionStart
	presence := h.presence
	h.mu.Unlock()
	if sessionStart != nil {
		sessionStart.OnSessionStarted(sessionID, account)
	}

	// The reconciliation edge (SEA-1569 T8, design.md:494-503): a Runner
	// re-enroll clears bindings and each session re-promotes here, so presence is
	// reconstructed on this edge. Notify AFTER releasing the lock and only once
	// the binding is recorded, nil-safe; the sink enqueues into the component's
	// own loop and returns promptly (the loop resolves the live state via the
	// Status relay + the open-ask overlay, so it must not run under h.mu).
	if presence != nil {
		presence.OnSessionPromoted(account, sessionID)
	}
}

// unbindSession removes a session's account binding. Called from Stop, so a
// RelayCommsCall for a stopped session_id fails closed CodeNotFound — the same
// answer as a never-seen session, never a stale reuse.
//
// It also drives presence to OFFLINE (SEA-1569 T8): a clean Stop tears the
// session down, but a STOPPED/DISCONNECTED frame arriving after the unbind can
// no longer resolve the account at deliverSession, so without an edge here the
// account's presence would stay WORKING/IDLE/WAITING forever. Fire a terminal
// (DISCONNECTED → OFFLINE) presence edge — but ONLY when THIS session was the
// account's live session (the reverse entry actually got deleted). If
// promoteSession already re-pointed the account onto a NEWER session, the
// account is not offline and no edge fires. publishIfChanged dedups, so an
// OFFLINE already driven by a terminal frame makes this a no-op second publish.
// Capture the account under mu, release, then fire the sink — the exact
// lock-then-release-then-fire discipline promoteSession uses, so the sink (which
// enqueues into the presence loop) never runs under h.mu.
func (h *Hub) unbindSession(sessionID string) {
	h.mu.Lock()
	var (
		account     store.AccountID
		wentOffline bool
	)
	if a, ok := h.sessionAccounts[sessionID]; ok {
		// Drop the reverse entry only if it still points at THIS session — a
		// promoteSession for the account onto a newer session would have already
		// repointed it, and a stale delete would then unbind the live one.
		if h.accountSessions[a] == sessionID {
			delete(h.accountSessions, a)
			account = a
			wentOffline = true
		}
	}
	delete(h.sessionAccounts, sessionID)
	presence := h.presence
	h.mu.Unlock()

	// The account now has NO live session: drive its presence OFFLINE. Skipped
	// when the account was re-pointed to a newer session (wentOffline is false).
	//
	// Accepted race (RIG-1651): this edge and promoteSession's OnSessionPromoted
	// both enqueue onto the presence FIFO after releasing h.mu, so a CONCURRENT
	// same-account Stop(this)+Start(newer) could order the live promotion before
	// this DISCONNECTED and strand the account OFFLINE until the next
	// lifecycle/promotion edge repairs it. Left as-is: an account has at most one
	// live session (the orchestrator's operating invariant), so concurrent
	// same-account churn is unreachable, and any transient OFFLINE self-repairs on
	// the next edge. Do NOT "fix" the fire-after-unlock discipline blind to this —
	// the re-point guard above already covers every SEQUENTIAL ordering. If
	// concurrent same-account teardown/promote ever becomes reachable, stamp a
	// per-account generation under h.mu into each edge and drop a terminal edge
	// older than the last-applied promotion (RIG-1651 option b).
	if wentOffline && presence != nil {
		presence.OnSessionLifecycle(account, sessionID, compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED)
	}
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

// errTranscriptsUnavailable is the fail-closed cause when a hub with no
// TranscriptStore wired receives a CommitConversationFrame (a Deliver-only hub).
// It maps to CodeUnavailable — the durable transcript leg is not mounted, never
// a silent success.
var errTranscriptsUnavailable = errors.New("runnerhub: no transcript store wired to serve CommitConversationFrame")

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

// CommitConversationFrame durably commits one relayed transcript_entry frame to
// the transcript store, keyed at most once on the agent-minted idempotency_key —
// the DURABLE counterpart to the loss-tolerant Deliver/PublishEvents path (#24 /
// OQ-3, SEA-1667 T4). The Runner asserts no account; this resolves session_id ->
// account from the hub's own binding purely as the fail-closed liveness gate
// (exactly as RelayCommsCall does), then writes the entry to the transcript
// store under the session id. The transcript row is keyed by session_id, not by
// account — the resolved account is the "is this a live session bound to this
// Runner" check, not an attribution written into the row.
//
// The conversation_posted / conversation_updated write-through was removed with
// the Zulip threading model, so the durable lane now carries ONLY the SEA-1570
// transcript_entry variant — the exact frame the Runner's Gateway forwards
// (runner/gateway/post_conversation_frame.go). The method name and the request/
// response messages keep the frozen CommitConversationFrame shape.
//
// Contract (ratified — do not redesign):
//   - An unresolved session fails closed CodeNotFound — never a stale account,
//     never the bootstrap admin. Same fail-closed shape as RelayCommsCall.
//   - A hub with no transcript store wired fails CodeUnavailable (the durable
//     transcript leg is not mounted — a Deliver-only hub). Checked BEFORE
//     session resolution, so even a bound session gets Unavailable.
//   - committed=true on a fresh commit AND on an idempotent replay of an
//     already-committed key (the store dedups a duplicate idempotency_key as a
//     silent success). A non-commit is NEVER committed=false with a nil error —
//     it is ALWAYS a Connect status error, because the Runner drives
//     at-least-once purely off the Connect code (err==nil => committed).
//   - Store errors are mapped to Connect codes (transcriptCommitError, mirroring
//     the comms edgeError): ErrInvalidArgument -> InvalidArgument (a malformed or
//     unknown-session frame, terminal), ErrConflict -> AlreadyExists (a genuine
//     entry_seq collision, terminal), any other -> Internal (a transient store
//     fault the relay should retry). Never a bare error connect.CodeOf would read
//     as CodeUnknown — that would wrongly present as a retryable teardown.
//   - message_id and seq are not meaningful for a transcript entry (the Runner
//     reads neither): shipped as "" and 0.
func (h *Hub) CommitConversationFrame(
	ctx context.Context,
	req *compassv1internal.CommitConversationFrameRequest,
) (*compassv1internal.CommitConversationFrameResponse, error) {
	h.mu.Lock()
	transcripts := h.transcripts
	h.mu.Unlock()
	if transcripts == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errTranscriptsUnavailable)
	}
	sessionID := req.GetSessionId()
	if _, ok := h.accountForSession(sessionID); !ok {
		// Fail closed: no live session maps to this id. Never a stale account,
		// never the bootstrap admin — a hard CodeNotFound the Runner surfaces.
		return nil, connect.NewError(
			connect.CodeNotFound,
			errors.New("runnerhub: no agent account bound to session"),
		)
	}

	if err := h.commitFrame(ctx, transcripts, sessionID, req.GetFrame(), req.GetIdempotencyKey()); err != nil {
		// Already Connect-coded (commitFrame maps the store sentinel or minted the
		// malformed-frame status). Propagate as-is so the Runner's retryable/
		// terminal split reads the right code.
		return nil, err
	}
	return &compassv1internal.CommitConversationFrameResponse{
		Committed: true,
		MessageId: "", // No message id for a transcript entry; the Runner reads none.
		Seq:       0,  // Deferred (#24 contract): the consumer reads neither id nor seq today.
	}, nil
}

// commitFrame dispatches one durable frame to the transcript store by its set
// oneof variant. The durable lane carries only the SEA-1570 transcript_entry
// variant (the conversation_posted / conversation_updated write-through was
// removed with the Zulip threading model), so a frame with any other variant —
// or none — is CodeInvalidArgument, the terminal "malformed frame" the Runner
// does not retry. A store error is mapped through transcriptCommitError. entryJSON
// is opaque and forwarded verbatim (never parsed here).
func (h *Hub) commitFrame(
	ctx context.Context,
	transcripts TranscriptStore,
	sessionID string,
	frame *compassv1internal.AgentFrame,
	idempotencyKey string,
) error {
	switch f := frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_TranscriptEntry:
		te := f.TranscriptEntry
		if err := transcripts.AppendTranscriptEntry(
			ctx, sessionID, te.GetEntrySeq(), te.GetCheckpoint(), te.GetEntryJson(), idempotencyKey,
		); err != nil {
			return transcriptCommitError(err)
		}
		return nil
	default:
		return connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: durable frame has no transcript_entry variant set"),
		)
	}
}

// transcriptCommitError maps a transcript-store sentinel error onto the Connect
// code the durable lane's contract expects, mirroring the comms edgeError
// (comms/context.go): the store's sentinels are the vocabulary, and anything
// unrecognized is an internal fault the relay should retry, never leaked as a
// bare CodeUnknown. ErrInvalidArgument (a malformed or unknown-session entry) and
// ErrConflict (a genuine entry_seq collision) are terminal; any other store
// error is a transient fault (CodeInternal) the Runner retries under the same
// key.
func transcriptCommitError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
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
	case *compassv1internal.CommsCallRequest_Roster:
		resp, err := h.comms.RosterAsAccount(ctx, account, c.Roster)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_Roster{Roster: resp},
		}, nil
	case *compassv1internal.CommsCallRequest_SetStatus:
		// Ordered write-then-publish (design.md T3:473-486): the durable
		// Store.SetActivity COMMITS first (returning the server-truncated value
		// that landed in the table), THEN a best-effort PublishActivity fires the
		// live event carrying exactly that truncated string. A lost publish
		// self-heals on the next set_status; the table is the source of record,
		// so the publish is never gated on and never errors the call.
		truncated, err := h.comms.SetStatusAsAccount(ctx, account, c.SetStatus.GetActivity())
		if err != nil {
			return nil, err
		}
		h.PublishActivity(account, truncated)
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_SetStatus{SetStatus: &compassv1internal.SetAgentStatusResponse{}},
		}, nil
	case *compassv1internal.CommsCallRequest_Pin:
		resp, err := h.comms.UpdatePinnedBoardAsAccount(ctx, account, c.Pin)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_Pin{Pin: resp},
		}, nil
	default:
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: comms call has no recognized variant set (post/list/roster/set_status/pin)"),
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
