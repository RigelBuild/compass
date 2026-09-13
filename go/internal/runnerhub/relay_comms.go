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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
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
//
// RIG-3108: the maps are a read-through cache, so the durable binding is written
// FIRST (RecordSessionBinding, on the request ctx so it lands tenant-scoped),
// and only then are the maps updated under h.mu. The store returns the session
// this account DISPLACED — the prior session now resolving nowhere — which is
// evicted from the forward map here (without it, sess-old would keep resolving
// to the account it no longer speaks for) and invalidated on peer instances via
// a BindingUnbound publish. A BindingBound publish for the new session lets peer
// instances drop any stale cache entry for it. The store write and the publish
// both run with h.mu RELEASED — never hold the lock across a store call or a
// sink — exactly the lock-then-store-then-map discipline the design requires.
func (h *Hub) promoteSession(ctx context.Context, containerName, sessionID string) {
	if containerName == "" || sessionID == "" {
		return
	}
	h.mu.Lock()
	account, ok := h.containerAccounts[containerName]
	if !ok {
		h.mu.Unlock()
		return
	}
	// Capture the store handle, the routing fabric, and the enrolled Runner id
	// under the lock, then release BEFORE the store write: the binding row names
	// the Runner the session is attached to (the sweep key a re-enroll retires
	// it by), and a promote always follows a relay through that Runner, so one is
	// enrolled. A nil store or an (unexpected) empty runner id keeps the maps as
	// truth — today's behaviour — writing no durable row.
	bindings := h.bindings
	routing := h.routing
	var runnerID string
	if h.runner != nil {
		runnerID = h.runner.id
	}
	h.mu.Unlock()

	// Write the durable binding first (h.mu released). The store upsert is keyed
	// on the account, so it lands whether or not the account held a prior
	// session, and it returns the displaced session id.
	var displaced string
	tenant := ""
	if bindings != nil && runnerID != "" {
		d, err := bindings.RecordSessionBinding(ctx, sessionID, account, runnerID)
		if err != nil {
			// A durable-write fault must not fail the Start that already
			// succeeded on the Runner: log it and fall back to the in-RAM cache
			// so the session resolves at least on this instance. The next
			// re-enroll sweep or a cache-miss re-read reconciles against the
			// table.
			h.log.Error("record session binding failed; falling back to in-RAM cache",
				"session_id", sessionID, "account", string(account), "error", err)
		} else {
			displaced = d
			tenant = string(bindings.EffectiveTenant(ctx))
		}
	}

	// Now update the maps under h.mu (store already written).
	h.mu.Lock()
	h.sessionAccounts[sessionID] = account
	h.accountSessions[account] = sessionID
	// The container->account entry has served its purpose; the session binding
	// is now authoritative. Drop it so a container name reused across the
	// Runner's life cannot resurrect a stale account (reconnect clears both maps
	// anyway; this keeps the pre-Start map tight in the meantime).
	delete(h.containerAccounts, containerName)
	// Evict the displaced session from the forward map: the account moved off it,
	// so it now resolves nowhere. Guard displaced != sessionID for the rebind
	// case (an account re-pointed onto the SAME session displaces itself, and
	// dropping the entry just written would unbind the live session).
	if displaced != "" && displaced != sessionID {
		delete(h.sessionAccounts, displaced)
	}
	// Read both after-binding sinks under mu so a setter and this arm never race,
	// then release BEFORE firing either: each sink only enqueues into its own
	// consumer/component loop and returns promptly, so promoteSession never blocks
	// on store work and never holds h.mu across a sink call (mirrors the settle
	// edge at deliverSession). Both nil-safe (a hub with neither wired is today's
	// behavior — RIG-1569 T6 session-start, T8 presence).
	sessionStart := h.sessionStart
	presence := h.presence
	h.mu.Unlock()

	// Invalidate peer instances' caches (h.mu released, nil-safe, best-effort):
	// the displaced session has no row any more (BindingUnbound), and the new
	// session's binding changed (BindingBound). A single-instance hub wires no
	// routing fabric, so this is a no-op there.
	if routing != nil && tenant != "" {
		if displaced != "" && displaced != sessionID {
			h.publishBindingChange(ctx, routing, tenant, displaced, fabric.BindingUnbound)
		}
		h.publishBindingChange(ctx, routing, tenant, sessionID, fabric.BindingBound)
	}

	if sessionStart != nil {
		sessionStart.OnSessionStarted(sessionID, account)
	}

	// The reconciliation edge (RIG-1569 T8, design.md:494-503): a Runner
	// re-enroll clears bindings and each session re-promotes here, so presence is
	// reconstructed on this edge. Notify AFTER releasing the lock and only once
	// the binding is recorded, nil-safe; the sink enqueues into the component's
	// own loop and returns promptly (the loop resolves the live state via the
	// Status relay + the open-ask overlay, so it must not run under h.mu).
	if presence != nil {
		presence.OnSessionPromoted(account, sessionID)
	}
}

// publishBindingChange fans one binding invalidation to peer instances over the
// routing fabric (RIG-3108 §T4), logging a publish failure rather than
// propagating it: the fabric rides core NATS (at-most-once), and a dropped
// invalidation degrades to a peer's cache-miss re-read against Postgres — the
// arbiter — so a publish failure never fails the operation that caused it.
func (h *Hub) publishBindingChange(ctx context.Context, routing RoutingFabric, tenant, sessionID string, op fabric.BindingOp) {
	if err := routing.PublishBindingChange(ctx, tenant, fabric.BindingChange{
		Tenant:    tenant,
		SessionID: sessionID,
		Op:        op,
	}); err != nil {
		h.log.Warn("publish binding change failed (peers re-read on cache miss)",
			"tenant", tenant, "session_id", sessionID, "op", string(op), "error", err)
	}
}

// unbindSession removes a session's account binding. Called from Stop, so a
// RelayCommsCall for a stopped session_id fails closed CodeNotFound — the same
// answer as a never-seen session, never a stale reuse.
//
// It also drives presence to OFFLINE (RIG-1569 T8): a clean Stop tears the
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
func (h *Hub) unbindSession(ctx context.Context, sessionID string) {
	// RIG-3108: the maps are a cache, so the durable row is deleted FIRST
	// (DeleteSessionBinding, on the request ctx so it stays tenant-scoped), then
	// the maps are evicted under h.mu. DeleteSessionBinding is by session id and
	// idempotent — a session promoteSession already displaced has no row (the
	// account row now names the newer session), so a stale release matches
	// nothing and leaves the live binding alone, exactly the re-point guard the
	// map eviction below keeps.
	h.mu.Lock()
	bindings := h.bindings
	routing := h.routing
	h.mu.Unlock()

	tenant := ""
	if bindings != nil {
		if err := bindings.DeleteSessionBinding(ctx, sessionID); err != nil {
			// A durable-delete fault must not fail the Stop that already
			// succeeded on the Runner: log and continue to evict the cache. The
			// next re-enroll sweep retires any surviving row.
			h.log.Error("delete session binding failed; evicting cache anyway",
				"session_id", sessionID, "error", err)
		} else {
			tenant = string(bindings.EffectiveTenant(ctx))
		}
	}

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

	// Invalidate peer instances' caches (h.mu released, nil-safe, best-effort):
	// this session has no binding any more (BindingUnbound), so a peer can drop
	// its entry outright. A single-instance hub wires no routing fabric.
	if routing != nil && tenant != "" {
		h.publishBindingChange(ctx, routing, tenant, sessionID, fabric.BindingUnbound)
	}

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
//
// RIG-3108: the map is a read-through cache. A hit returns immediately. A miss
// falls through to the durable binding table ONLY when a Runner is currently
// enrolled AND the ctx is request-scoped — so a Server restart resolves a
// pre-restart session from the durable row, while a miss after a reconnect
// (which durably reaps every binding at enroll) stays a fail-closed miss. The
// enrolled-Runner gate is what keeps fail-closed correct across a reconnect: the
// reap deletes the rows, so even the table read would miss, but the gate makes
// the miss free of a table round-trip. store.ErrNotFound (and any store fault)
// maps to ok=false, so CodeNotFound behaviour is byte-identical to today.
//
// The read-through is refused under a system-role ctx: SessionBindingStore's
// reads are single-valued only because RLS narrows them to the acting tenant,
// and a BYPASSRLS read could return a plausible row from an ARBITRARY tenant
// (store/session_bindings_pgtest_test.go::TestSessionForAccountUnderSystemRoleIsUnscoped).
// The ack arms resolve BEFORE escalating to the system role, so they never reach
// this refusal; the guard is defence in depth against a future system-role
// caller.
func (h *Hub) accountForSession(ctx context.Context, sessionID string) (store.AccountID, bool) {
	h.mu.Lock()
	if account, ok := h.sessionAccounts[sessionID]; ok {
		h.mu.Unlock()
		return account, true
	}
	bindings := h.bindings
	enrolled := h.runner != nil
	reapStale := h.reapStale
	h.mu.Unlock()

	if !h.readThroughAllowed(ctx, bindings, enrolled, reapStale) {
		return "", false
	}
	account, err := bindings.ResolveSessionAccount(ctx, sessionID)
	if err != nil {
		// store.ErrNotFound (an unbound session) and any store fault both fail
		// closed — the same CodeNotFound the caller mints today.
		return "", false
	}
	// Populate the forward cache so a subsequent comms call for this restarted
	// session hits without a table round-trip. Re-check under the lock: a
	// concurrent promote/unbind may have run between the release and here, so a
	// live map entry wins over the row just read (avoids clobbering a fresher
	// binding with a staler one).
	h.mu.Lock()
	if live, ok := h.sessionAccounts[sessionID]; ok {
		h.mu.Unlock()
		return live, true
	}
	h.sessionAccounts[sessionID] = account
	h.mu.Unlock()
	return account, true
}

// readThroughAllowed reports whether a cache-miss binding read may fall through
// to the durable table: a store must be wired, a Runner must be currently
// enrolled (a miss with none enrolled means the reconnect reap cleared every
// binding — fail closed), the ctx must be request-scoped (a system-role read
// is the unscoped-row hazard, refused), and the last re-enroll's durable reap
// must not have faulted — rows it failed to delete name sessions this hub has
// already declared dead, so reading them back would resurrect them.
func (h *Hub) readThroughAllowed(ctx context.Context, bindings SessionBindingStore, enrolled, reapStale bool) bool {
	return bindings != nil && enrolled && !reapStale && !store.IsSystemRole(ctx)
}

// SessionForAccount resolves the LIVE session bound to an agent account — the
// REVERSE of accountForSession, the direction the delivery consumer (RIG-1569
// T3) needs to dispatch a deliver to a resolved subscriber. The bool is false
// when the account has no live session (never started, stopped, or dropped on a
// Runner reconnect): the consumer pushes nothing now and lets the D2 cursor
// sweep deliver on the recipient's next start (design.md:137). It satisfies the
// delivery.SessionResolver interface the consumer holds, kept separate from the
// ControlDispatcher (DispatchControl) so that stays the established dispatch-only
// shape.
//
// RIG-3108: a read-through cache exactly as accountForSession is. A miss falls
// through to the durable table only when a Runner is enrolled AND the ctx is
// request-scoped. The delivery consumer's loop runs under the system role
// (delivery/consumer.go Run), so its resolve REFUSES the read-through and falls
// to the D2 cursor sweep — the consumer's own miss contract, unchanged. A
// request-scoped caller (a Server restart resolving a pre-restart recipient)
// resolves from the row. store.ErrNotFound and any store fault map to ok=false.
func (h *Hub) SessionForAccount(ctx context.Context, account store.AccountID) (string, bool) {
	h.mu.Lock()
	if sessionID, ok := h.accountSessions[account]; ok {
		h.mu.Unlock()
		return sessionID, true
	}
	bindings := h.bindings
	enrolled := h.runner != nil
	reapStale := h.reapStale
	h.mu.Unlock()

	if !h.readThroughAllowed(ctx, bindings, enrolled, reapStale) {
		return "", false
	}
	sessionID, err := bindings.SessionForAccount(ctx, account)
	if err != nil {
		return "", false
	}
	h.mu.Lock()
	if live, ok := h.accountSessions[account]; ok {
		h.mu.Unlock()
		return live, true
	}
	h.accountSessions[account] = sessionID
	h.mu.Unlock()
	return sessionID, true
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

// OnBindingChange evicts this instance's cache entry for the session named in a
// binding change received from a PEER instance over the routing fabric (RIG-3108
// §T4). It is the subscribe-side counterpart to promoteSession/unbindSession's
// PublishBindingChange: a peer that re-pointed or released a session tells every
// other Server to drop its now-stale cache, and the next resolution re-reads the
// durable truth (Postgres is the arbiter).
//
// It NEVER trusts the change's contents as data (reference-never-payload): the
// change carries only a session id and an op, never the resolved account, so
// this drops the cached entry and lets the next accountForSession/SessionForAccount
// cache-miss re-read the table. BindingOp is an OPEN SET, so this handles ANY op
// — known or not — as the same invalidate-and-re-read: an unrecognized op still
// means the binding genuinely changed, and on a plane with no ack a drop would
// leave the entry stale with nothing to reveal it (fabric.BindingOp). There is
// no switch on the op here precisely because every value means the same thing.
//
// It never publishes: this is the RECEIVE side, so re-publishing would loop an
// invalidation around the fabric forever.
func (h *Hub) OnBindingChange(change fabric.BindingChange) {
	sessionID := change.SessionID
	if sessionID == "" {
		// A change naming no session would invalidate cache key "" and read as
		// "nothing changed"; the fabric decode already rejects this, so it is
		// defence in depth.
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Evict the forward entry and, if it still points back at this session, the
	// reverse entry too — so neither direction serves a stale binding a peer just
	// changed. The re-point guard mirrors unbindSession: a reverse entry the
	// account has already moved onto a newer session is left alone.
	if account, ok := h.sessionAccounts[sessionID]; ok {
		if h.accountSessions[account] == sessionID {
			delete(h.accountSessions, account)
		}
		delete(h.sessionAccounts, sessionID)
	}
}

// AccountForLiveSession returns the agent account bound to sessionID in the hub,
// with false when no live binding exists. It mirrors accountForSession's lock
// discipline but skips the durable read-through: FetchSecrets authorizes a
// re-fetch for a session the hub currently holds, and the returned account is
// the identity the A9 scoped resolve reads. Under the inject-all + single-Runner
// MVP a live binding in the hub IS a session bound to this Runner (there is
// exactly one), so membership in sessionAccounts is the whole session-binding
// authz; the per-Runner differentiation is the future multi-Runner seam
// (record §761-762).
func (h *Hub) AccountForLiveSession(sessionID string) (store.AccountID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	account, ok := h.sessionAccounts[sessionID]
	return account, ok
}

// AccountForContainer returns the agent account bound to containerName in the
// Provision..Start window (bindContainer, cleared by promoteSession at Start and
// by clear() on re-enroll), with false when none is recorded. It is the
// PROVISION-time analogue of AccountForLiveSession: FetchSecrets authorizes an
// initial pre-exec materialize against it (no live session exists until Start)
// and reads the returned account for the A9 scoped resolve. Under the inject-all
// + single-Runner MVP a recorded binding IS a container provisioned on the one
// enrolled Runner, so membership is the whole authz check (the per-Runner
// differentiation is the same future multi-Runner seam, record §761-762).
func (h *Hub) AccountForContainer(containerName string) (store.AccountID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	account, ok := h.containerAccounts[containerName]
	return account, ok
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
// OQ-3, RIG-1667 T4). The Runner asserts no account; this resolves session_id ->
// account from the hub's own binding purely as the fail-closed liveness gate
// (exactly as RelayCommsCall does), then writes the entry to the transcript
// store under the session id. The transcript row is keyed by session_id, not by
// account — the resolved account is the "is this a live session bound to this
// Runner" check, not an attribution written into the row.
//
// The conversation_posted / conversation_updated write-through was removed with
// the Zulip threading model, so the durable lane now carries ONLY the RIG-1570
// transcript_entry variant — the exact frame the Runner's Gateway forwards
// (runner/gateway/post_conversation_frame.go). The method name and the request/
// response messages keep the established CommitConversationFrame shape.
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
	if _, ok := h.accountForSession(ctx, sessionID); !ok {
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
// oneof variant. The durable lane carries only the RIG-1570 transcript_entry
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
	oneof := frame.GetFrame()
	if oneof == nil {
		return connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: durable frame has no transcript_entry variant set"),
		)
	}
	switch f := oneof.(type) {
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

// linkKindAttr marks the link linkTrigger adds, so a reader selects the
// cross-turn causal edge by attribute instead of by position — the span also
// carries otelconnect's transport link. The mark lives in the link's
// ATTRIBUTES, so a deployment that zeroes OTEL_LINK_ATTRIBUTE_COUNT_LIMIT
// exports the edge but strips its kind — present, and unselectable.
var linkKindAttr = attribute.String("compass.link.kind", "cross_turn_trigger")

// linkTrigger records the cross-turn causal edge from the agent's reply to the
// turn that triggered it: a span LINK on the current RelayCommsCall origin span
// (the otelconnect handler span mounted in handler.go) pointing at the remote
// span context tp names. A LINK, never a parent — the reply is a fresh root that
// REFERENCES its trigger instead of nesting under it, which is what lets a
// conversation's traces terminate rather than growing one unbounded tree.
//
// An empty tp adds no link at all. A malformed tp adds none either: parsing goes
// through the one shipped W3C helper (otelx.ContextWithTraceparent), and because
// that helper returns its INPUT context unchanged on a parse failure — whose span
// context here would be the local handler span — the parse is run against a
// context.Context deliberately stripped of any span. That keeps a bad
// traceparent from linking the span to itself, and keeps one traceparent parser
// in the tree.
//
// Only the traceparent crosses this seam: the proto carries no trigger_tracestate
// half, so any vendor tracestate the trigger held is not on the link.
//
// The link is STAMPED with linkKindAttr so a reader can find it by attribute
// rather than by position. It is not the only link on this span: otelconnect's
// server branch mints one from the inbound transport context whenever the
// caller propagated a traceparent, which the production Runner's client
// interceptor always does (internal/runner/runner.go:112-115, otelconnect
// interceptor.go:110-117). So the trigger link is neither the only nor
// reliably the first element of Links, and a positional or count-based read of
// it is wrong.
func linkTrigger(ctx context.Context, tp string) {
	if tp == "" {
		return
	}
	bare := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	remote := trace.SpanContextFromContext(otelx.ContextWithTraceparent(bare, tp))
	if !remote.IsValid() {
		return
	}
	trace.SpanFromContext(ctx).AddLink(trace.Link{
		SpanContext: remote,
		Attributes:  []attribute.KeyValue{linkKindAttr},
	})
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
	oneof := call.GetCall()
	if oneof == nil {
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: comms call has no recognized variant set (post/list/roster/set_status/pin/create_channel/update_members/create_channel_group/open_dm)"),
		)
	}
	switch c := oneof.(type) {
	case *compassv1internal.CommsCallRequest_Post:
		linkTrigger(ctx, call.GetTriggerTraceparent())
		resp, err := h.comms.PostAsAccountByName(ctx, account, c.Post)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_Post{Post: resp},
		}, nil
	case *compassv1internal.CommsCallRequest_List:
		resp, err := h.comms.ListAsAccountByName(ctx, account, c.List)
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
	case *compassv1internal.CommsCallRequest_CreateChannel:
		resp, err := h.comms.CreateChannelAsAccount(ctx, account, c.CreateChannel)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_CreateChannel{CreateChannel: resp},
		}, nil
	case *compassv1internal.CommsCallRequest_UpdateMembers:
		resp, err := h.comms.UpdateChannelMembersAsAccount(ctx, account, c.UpdateMembers)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_UpdateMembers{UpdateMembers: resp},
		}, nil
	case *compassv1internal.CommsCallRequest_CreateChannelGroup:
		resp, err := h.comms.CreateChannelGroupAsAccount(ctx, account, c.CreateChannelGroup)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_CreateChannelGroup{CreateChannelGroup: resp},
		}, nil
	case *compassv1internal.CommsCallRequest_OpenDm:
		resp, err := h.comms.OpenDMAsAccount(ctx, account, c.OpenDm)
		if err != nil {
			return nil, err
		}
		return &compassv1internal.CommsCallResult{
			Result: &compassv1internal.CommsCallResult_OpenDm{OpenDm: resp},
		}, nil
	default:
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("runnerhub: comms call has no recognized variant set (post/list/roster/set_status/pin/create_channel/update_members/create_channel_group/open_dm)"),
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
