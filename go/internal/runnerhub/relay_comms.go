//go:build unix

// The agent-comms Server leg: the session->account binding lifecycle and the
// RelayCommsCall handler the Runner forwards each agent comms call into.

// Trust model (load-bearing security leg): the Runner is a pure forwarder and
// asserts NO account. The SERVER resolves session_id -> agent account from this
// hub's binding and executes under it. Unknown/stopped/dropped session fails
// closed CodeNotFound — never a stale account. A session_id selects, never carries.
package runnerhub

import (
	"context"
	"errors"
	"maps"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// bindContainer records the resolved account and Runner that provisioned
// containerName. Every Start on the container reads it; Remove and re-enroll
// clear it. Empty values fail closed.
func (h *Hub) bindContainer(containerName string, agentAccountID store.AccountID, runnerID string) {
	if containerName == "" || agentAccountID == "" || runnerID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.containerAccounts[containerName] = sessionBinding{account: agentAccountID, runnerID: runnerID}
}

// promoteSession binds the container's provisioned account to the live
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
// bindingWriteMu spans the lookup, write and map update, so a whole enroll orders
// before or after this promotion, never inside it.
func (h *Hub) promoteSession(ctx context.Context, containerName, sessionID string) {
	if containerName == "" || sessionID == "" {
		return
	}
	h.bindingWriteMu.Lock()
	h.mu.Lock()
	binding, ok := h.containerAccounts[containerName]
	account := binding.account
	if !ok {
		h.mu.Unlock()
		h.bindingWriteMu.Unlock()
		return
	}
	// Capture the store handle, routing fabric, and enrolled Runner id under the
	// lock, then release BEFORE the store write: the binding row names the Runner
	// the session is attached to (the sweep key a re-enroll retires it by). A nil
	// store or empty runner id keeps the maps as truth, writing no durable row.
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
	var displaced, version string
	tenant := ""
	if bindings != nil && runnerID != "" {
		d, v, err := bindings.RecordSessionBinding(ctx, sessionID, account, runnerID)
		if err != nil {
			// A durable-write fault must not fail the Start that already succeeded
			// on the Runner: log it and fall back to the in-RAM cache so the session
			// resolves at least on this instance. The next re-enroll sweep or a
			// cache-miss re-read reconciles against the table.
			h.log.Error("record session binding failed; falling back to in-RAM cache",
				"session_id", sessionID, "account", string(account), "error", err)
		} else {
			displaced, version = d, v
			tenant = string(bindings.EffectiveTenant(ctx))
		}
	}

	// Now update the maps under h.mu (store already written).
	h.mu.Lock()
	// Keep the container->account entry: a resume Start on this container fetches
	// secrets by container_name before exec. Remove and re-enroll clear it.
	h.sessionAccounts[sessionID] = h.newBindingLocked(account, runnerID, version)
	h.accountSessions[account] = sessionID
	// Evict the displaced session from the forward map: the account moved off it,
	// so it now resolves nowhere. Guard displaced != sessionID for the rebind
	// case (an account re-pointed onto the SAME session displaces itself, and
	// dropping the entry just written would unbind the live session).
	if displaced != "" && displaced != sessionID {
		delete(h.sessionAccounts, displaced)
	}
	// Read both after-binding sinks under mu so a setter and this arm never race,
	// then release BEFORE firing either: each sink enqueues into its own loop and
	// returns promptly, so promoteSession never blocks on store work nor holds h.mu
	// across a sink call. Both nil-safe.
	sessionStart := h.sessionStart
	presence := h.presence
	h.mu.Unlock()
	h.bindingWriteMu.Unlock()

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

	// The reconciliation edge: a Runner re-enroll clears bindings and each session
	// re-promotes here, so presence is reconstructed on this edge. Notify AFTER
	// releasing the lock and only once the binding is recorded; the sink enqueues
	// into its own loop, which resolves live state and must not run under h.mu.
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
	h.releaseSession(ctx, sessionID, nil)
}

// releaseSession is unbindSession limited, when only is non-nil, to that one
// binding of sessionID, so a release from an older lifetime leaves a re-bind
// alone. It reports whether the binding was released.
func (h *Hub) releaseSession(ctx context.Context, sessionID string, only *sessionBinding) bool {
	// The maps are a cache, so the durable row is deleted FIRST (on the request ctx
	// so it stays tenant-scoped), then the maps are evicted under h.mu. Delete is
	// by session id and idempotent — a session already displaced has no row, so a
	// stale release matches nothing and leaves the live binding alone.
	h.mu.Lock()
	bindings := h.bindings
	routing := h.routing
	if only != nil {
		if live, ok := h.sessionAccounts[sessionID]; ok && !sameBinding(live, *only) {
			h.mu.Unlock()
			return false
		}
	}
	h.mu.Unlock()

	tenant := ""
	if bindings != nil {
		version := ""
		if only != nil {
			version = only.version
		}
		removed, err := bindings.DeleteSessionBinding(ctx, sessionID, version)
		switch {
		case err != nil:
			// A durable-delete fault must not fail the Stop that already
			// succeeded on the Runner: log and continue to evict the cache. The
			// next re-enroll sweep retires any surviving row.
			h.log.Error("delete session binding failed; evicting cache anyway",
				"session_id", sessionID, "error", err)
		case only != nil && version != "" && !removed:
			// The row was re-bound since only was read: it is not ours to release.
			return false
		default:
			tenant = string(bindings.EffectiveTenant(ctx))
		}
	}

	h.mu.Lock()
	var (
		account     store.AccountID
		wentOffline bool
	)
	binding, ok := h.sessionAccounts[sessionID]
	if ok && only != nil && !sameBinding(binding, *only) {
		// Re-bound in the cache while the durable delete ran.
		h.mu.Unlock()
		return false
	}
	if ok {
		// Drop the reverse entry only if it still points at THIS session — a
		// promoteSession for the account onto a newer session would have already
		// repointed it, and a stale delete would then unbind the live one.
		if h.accountSessions[binding.account] == sessionID {
			delete(h.accountSessions, binding.account)
			account = binding.account
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

	// Accepted race (RIG-1651): this edge and OnSessionPromoted both enqueue onto
	// the presence FIFO after releasing h.mu, but an account has at most one live
	// session (orchestrator invariant), so concurrent same-account churn is
	// unreachable and any transient OFFLINE self-repairs on the next edge.
	if wentOffline && presence != nil {
		presence.OnSessionLifecycle(account, sessionID, compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED)
	}
	return true
}

// newBindingLocked builds a sessionAccounts entry with a fresh lifetime. Caller holds mu.
func (h *Hub) newBindingLocked(account store.AccountID, runnerID, version string) sessionBinding {
	h.lastLifetime++
	return sessionBinding{account: account, runnerID: runnerID, version: version, lifetime: h.lastLifetime}
}

// unbindContainer drops a container's provisioned account binding. Remove is the
// only normal-lifecycle clear, so without it the binding would keep authorizing a
// pre-exec FetchSecrets (AccountForContainer) for a container that is gone.
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
// enrolled AND the ctx is request-scoped. Every enroll reaps that Runner's rows
// across tenants, so such a session that predates the enroll stays a fail-closed miss, while a
// binding another Server instance recorded after it resolves from the row. With no
// Runner enrolled the gate skips the table round-trip. store.ErrNotFound (and any
// store fault) maps to ok=false, so CodeNotFound behaviour is byte-identical to today.
//
// The read-through is refused under a system-role ctx: SessionBindingStore's
// reads are single-valued only because RLS narrows them to the acting tenant,
// and a BYPASSRLS read could return a plausible row from an ARBITRARY tenant
// (store/session_bindings_pgtest_test.go::TestSessionForAccountUnderSystemRoleIsUnscoped).
// The ack arms resolve BEFORE escalating to the system role, so they never reach
// this refusal; the guard is defence in depth against a future system-role
// caller.
func (h *Hub) accountForSession(ctx context.Context, sessionID string) (store.AccountID, bool) {
	binding, ok := h.resolveSessionBinding(ctx, sessionID)
	return binding.account, ok
}

// accountForRunnerSession is accountForSession for a Runner-originated call: a
// session owned by another Runner reads as unbound, so a foreign Runner token
// cannot act as that agent nor learn which session ids are live.
func (h *Hub) accountForRunnerSession(ctx context.Context, runnerID, sessionID string) (store.AccountID, bool) {
	if runnerID == "" {
		return "", false
	}
	binding, ok := h.resolveSessionBinding(ctx, sessionID)
	if !ok || binding.runnerID != runnerID {
		return "", false
	}
	return binding.account, true
}

// frameSession resolves the binding a published session frame speaks for and
// whether runnerID may publish it. An unbound session is allowed only from the
// enrolled Runner: frames precede Start's bind, but an unverifiable owner (no
// Runner enrolled, a refused read-through) must not let any token through.
// A trace-only frame from a Runner that is not enrolled is dropped on a cache
// miss without a store read, keeping a foreign output stream off the table.
func (h *Hub) frameSession(ctx context.Context, runnerID, sessionID string, lifecycle bool) (binding sessionBinding, bound, allowed bool) {
	if runnerID == "" {
		return sessionBinding{}, false, false
	}
	h.mu.Lock()
	binding, bound = h.sessionAccounts[sessionID]
	enrolled := h.runner != nil && h.runner.id == runnerID
	h.mu.Unlock()
	if !bound {
		if !lifecycle && !enrolled {
			return sessionBinding{}, false, false
		}
		var state bindingLookup
		binding, state = h.lookupSessionBinding(ctx, sessionID)
		if state == bindingUnverifiable {
			return sessionBinding{}, false, false
		}
		bound = state == bindingFound
	}
	if bound {
		return binding, true, binding.runnerID == runnerID
	}
	return binding, false, enrolled
}

// bindingLookup is the outcome of a cache-then-durable session binding read.
type bindingLookup int

const (
	bindingFound bindingLookup = iota
	bindingNotFound
	// bindingUnverifiable: a refused read-through or a store fault, so a durable
	// row naming another Runner may exist unseen.
	bindingUnverifiable
)

// resolveSessionBinding is the shared cache-then-durable resolution behind both
// resolvers above. A not-found and an unverifiable read both fail closed here.
func (h *Hub) resolveSessionBinding(ctx context.Context, sessionID string) (sessionBinding, bool) {
	binding, state := h.lookupSessionBinding(ctx, sessionID)
	return binding, state == bindingFound
}

// lookupSessionBinding is resolveSessionBinding keeping not-found apart from
// unverifiable. With no store wired the cache is the only record, so a miss is
// not-found.
func (h *Hub) lookupSessionBinding(ctx context.Context, sessionID string) (sessionBinding, bindingLookup) {
	h.mu.Lock()
	if binding, ok := h.sessionAccounts[sessionID]; ok {
		h.mu.Unlock()
		return binding, bindingFound
	}
	bindings := h.bindings
	enrolled := h.runner != nil
	bindingEpoch := h.bindingEpoch
	h.mu.Unlock()

	if bindings == nil {
		return sessionBinding{}, bindingNotFound
	}
	if !h.readThroughAllowed(ctx, bindings, enrolled) {
		return sessionBinding{}, bindingUnverifiable
	}
	account, runnerID, version, err := bindings.ResolveSessionBinding(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return sessionBinding{}, bindingNotFound
	}
	if err != nil {
		return sessionBinding{}, bindingUnverifiable
	}
	// Populate the forward cache so a subsequent comms call for this restarted
	// session hits without a table round-trip. Re-check under the lock: a
	// concurrent promote/unbind may have run, so a live map entry wins over the
	// row just read (avoids clobbering a fresher binding with a staler one).
	h.mu.Lock()
	if live, ok := h.sessionAccounts[sessionID]; ok {
		h.mu.Unlock()
		return live, bindingFound
	}
	// Refuse a row read across an enroll of its Runner, or while that Runner's reap is
	// faulted. The refusal is final for this call; a later lookup re-reads under a fresh epoch.
	if h.reapStale[runnerID] != 0 || h.runnerEpoch[runnerID] > bindingEpoch {
		h.mu.Unlock()
		return sessionBinding{}, bindingUnverifiable
	}
	resolved := h.newBindingLocked(account, runnerID, version)
	h.sessionAccounts[sessionID] = resolved
	h.mu.Unlock()
	return resolved, bindingFound
}

// readThroughAllowed permits a store read only for an enrolled Runner and a
// request-scoped context; the per-Runner reap check happens after the read.
func (h *Hub) readThroughAllowed(ctx context.Context, bindings SessionBindingStore, enrolled bool) bool {
	return bindings != nil && enrolled && !store.IsSystemRole(ctx)
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
// request-scoped caller resolves a binding recorded since the last enroll from
// the row. store.ErrNotFound and any store fault map to ok=false, as do rows
// refused by that Runner's reap or by a concurrent enroll.
func (h *Hub) SessionForAccount(ctx context.Context, account store.AccountID) (string, bool) {
	h.mu.Lock()
	if sessionID, ok := h.accountSessions[account]; ok {
		binding, live := h.sessionAccounts[sessionID]
		if live && (h.runner == nil || binding.runnerID == h.runner.id) {
			h.mu.Unlock()
			return sessionID, true
		}
		delete(h.accountSessions, account)
	}
	bindings := h.bindings
	enrolled := h.runner != nil
	var runnerID string
	if h.runner != nil {
		runnerID = h.runner.id
	}
	bindingEpoch := h.bindingEpoch
	h.mu.Unlock()
	if !h.readThroughAllowed(ctx, bindings, enrolled) {
		return "", false
	}
	sessionID, ownerID, err := bindings.SessionForAccount(ctx, account)
	if err != nil || ownerID != runnerID {
		return "", false
	}
	h.mu.Lock()
	if live, ok := h.accountSessions[account]; ok {
		binding, exists := h.sessionAccounts[live]
		if exists && h.runner != nil && binding.runnerID == h.runner.id {
			h.mu.Unlock()
			return live, true
		}
		delete(h.accountSessions, account)
	}
	if (h.runner != nil && (h.runner.id != runnerID || h.runnerEpoch[ownerID] > bindingEpoch)) || h.reapStale[ownerID] != 0 {
		h.mu.Unlock()
		return "", false
	}
	h.accountSessions[account] = sessionID
	h.sessionAccounts[sessionID] = sessionBinding{account: account, runnerID: ownerID}
	h.mu.Unlock()
	return sessionID, true
}

// CachedSessionForAccount is SessionForAccount without the durable read-through:
// it answers only from this hub's live bindings. A wake's not-live check uses it,
// because a row this hub has not promoted may name a session that is gone, and
// reading it back would skip the wake that delivers the agent's message.
func (h *Hub) CachedSessionForAccount(account store.AccountID) (string, bool) {
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
	if binding, ok := h.sessionAccounts[sessionID]; ok {
		if h.accountSessions[binding.account] == sessionID {
			delete(h.accountSessions, binding.account)
		}
		delete(h.sessionAccounts, sessionID)
	}
}

// AccountForLiveSession returns the account for sessionID only when runnerID owns
// the live binding. It checks the in-memory cache only: a miss does not read the
// durable binding table because FetchSecrets permits re-fetches only for sessions
// currently held by this hub.
func (h *Hub) AccountForLiveSession(runnerID, sessionID string) (store.AccountID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	binding, ok := h.sessionAccounts[sessionID]
	if !ok || runnerID == "" || binding.runnerID != runnerID {
		return "", false
	}
	return binding.account, true
}

// AccountForContainer returns the account for containerName only when runnerID
// owns its provisioned binding. The check is limited to the in-memory binding
// created at Provision and cleared at Remove or re-enroll.
func (h *Hub) AccountForContainer(runnerID, containerName string) (store.AccountID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	binding, ok := h.containerAccounts[containerName]
	if !ok || runnerID == "" || binding.runnerID != runnerID {
		return "", false
	}
	return binding.account, true
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
	runnerID string,
	req *compassv1internal.RelayCommsCallRequest,
) (*compassv1internal.RelayCommsCallResponse, error) {
	if h.comms == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errCommsUnavailable)
	}
	account, ok := h.accountForRunnerSession(ctx, runnerID, req.GetSessionId())
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
	runnerID string,
	req *compassv1internal.CommitConversationFrameRequest,
) (*compassv1internal.CommitConversationFrameResponse, error) {
	h.mu.Lock()
	transcripts := h.transcripts
	h.mu.Unlock()
	if transcripts == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errTranscriptsUnavailable)
	}
	sessionID := req.GetSessionId()
	if _, ok := h.accountForRunnerSession(ctx, runnerID, sessionID); !ok {
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
		// Ordered write-then-publish: Store.SetActivity COMMITS first (returning the
		// server-truncated value that landed), THEN a best-effort PublishActivity
		// fires the live event carrying exactly that string. A lost publish
		// self-heals on the next set_status; the publish is never gated on.
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
		resp, err := h.comms.UpdateChannelMembersAsAccountByName(ctx, account, c.UpdateMembers)
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

// dropLostSession unbinds, archives and reports a session its Runner lost. Only
// the owning Runner may unbind it. A non-nil seen limits it to that binding: a
// resume may have re-bound the session id since the loss was observed.
func (h *Hub) dropLostSession(ctx context.Context, runnerID, sessionID string, seen *sessionBinding, errored bool) {
	ctx, scoped := h.runnerSessionCtx(ctx, runnerID, sessionID)
	if !scoped {
		return
	}
	binding, ok := h.resolveSessionBinding(ctx, sessionID)
	if !ok || runnerID == "" || binding.runnerID != runnerID || (seen != nil && !sameBinding(binding, *seen)) {
		return
	}
	if !h.releaseSession(ctx, sessionID, &binding) {
		return
	}
	h.mu.Lock()
	lost := h.lost
	h.mu.Unlock()
	h.archiveEnded(ctx, sessionID)
	h.log.Warn("runner reports bound session lost; released binding",
		"session_id", sessionID, "agent_account_id", binding.account, "errored", errored)
	if lost != nil {
		lost.OnSessionLost(sessionID, binding.account, errored)
	}
}

// dropLostSessionDetached runs dropLostSession off the caller's receive loop: it
// does store work, and the stream ctx dies with the stream.
// gen is the enrollment the loss was observed under.
func (h *Hub) dropLostSessionDetached(ctx context.Context, gen uint64, runnerID, sessionID string, seen *sessionBinding, errored bool) {
	go func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lostSessionTimeout)
		defer cancel()
		h.dropLostSessionIfCurrent(dctx, gen, runnerID, sessionID, seen, errored)
	}()
}

// dropLostSessionIfCurrent runs dropLostSession only while enrollment gen is
// current: a re-enroll since the loss may have re-bound sessionID.
func (h *Hub) dropLostSessionIfCurrent(ctx context.Context, gen uint64, runnerID, sessionID string, seen *sessionBinding, errored bool) {
	h.enrollMu.RLock()
	defer h.enrollMu.RUnlock()
	if h.enrollGen != gen {
		return
	}
	h.dropLostSession(ctx, runnerID, sessionID, seen, errored)
}

// sameBinding reports whether a and b are one binding of a session id: the same
// durable row when either has one, else the same cache entry.
func sameBinding(a, b sessionBinding) bool {
	if a.version != "" || b.version != "" {
		return a.version == b.version
	}
	return a.lifetime == b.lifetime
}

// runnerSessionCtx scopes a Runner-originated ctx to the tenant that binds
// sessionID to runnerID. Runners are shared across tenants, so the door sets none;
// without this, binding reads and deletes run under the bootstrap tenant. The
// tenant is read under the system role; everything after runs tenant-scoped.
// An ambiguous session fails closed. A miss or a store fault keeps ctx, whose
// own binding read then fails closed for a non-bootstrap session.
func (h *Hub) runnerSessionCtx(ctx context.Context, runnerID, sessionID string) (context.Context, bool) {
	if _, ok := store.TenantFromContext(ctx); ok {
		return ctx, true
	}
	h.mu.Lock()
	bindings := h.bindings
	h.mu.Unlock()
	if bindings == nil || runnerID == "" || sessionID == "" {
		return ctx, true
	}
	tenant, err := bindings.SessionBindingTenant(store.WithSystemRole(ctx), sessionID, runnerID)
	switch {
	case err == nil:
		return store.WithTenant(ctx, tenant), true
	case errors.Is(err, store.ErrConflict):
		h.log.Error("session bound in several tenants; refusing the runner call",
			"session_id", sessionID, "runner_id", runnerID)
		return ctx, false
	case !errors.Is(err, store.ErrNotFound):
		h.log.Warn("resolve session tenant failed; continuing unscoped",
			"session_id", sessionID, "error", err)
	}
	return ctx, true
}

// archiveEnded hands a session that ended without Stop to the transcript archive.
// It runs detached and bounded: the caller is a Runner stream loop, and an
// object-store PUT must not stall it nor die with the stream.
func (h *Hub) archiveEnded(ctx context.Context, sessionID string) {
	h.mu.Lock()
	ended := h.ended
	h.mu.Unlock()
	if ended == nil || sessionID == "" {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionEndArchiveTimeout)
	go func() {
		defer cancel()
		ended.OnSessionEnded(actx, sessionID)
	}()
}

// sessionEndArchiveTimeout bounds one detached session-end archive.
const sessionEndArchiveTimeout = 2 * time.Minute
