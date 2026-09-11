//go:build unix

package runnerhub

// RIG-3108 / RIG-2861 §T4 — the session-binding read-through cache. The hub's
// in-RAM sessionAccounts/accountSessions maps are now a cache over the durable
// session_bindings table (a SessionBindingStore), invalidated across Server
// instances by a RoutingFabric. These white-box tests drive the unexported
// binding lifecycle (promoteSession/unbindSession/enroll) and the two resolvers
// (accountForSession + SessionForAccount) against hand-written fakes — the same
// fake-double style as deliveryarm_test.go — so each pins one contract a
// plausible regression would break:
//
//   - a Server RESTART resolves a pre-restart session from the durable row,
//     both directions;
//   - a stopped / never-seen / post-RECONNECT session fails closed;
//   - a binding change on one instance evicts a peer instance's cache;
//   - a displaced session (an account re-pointed onto a new one) resolves
//     nowhere;
//   - the ack path's binding read never runs under the BYPASSRLS system role.
//
// The durable SQL itself is proven in store/session_bindings_pgtest_test.go; the
// cross-instance fabric fan-out in fabric/routing_fabric_test.go. These tests own
// the HUB's cache logic — the read-through gate, the reap, the eviction wiring —
// which neither of those exercises.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/fabric"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakeBindingStore is an in-memory SessionBindingStore double keyed on session
// id, modelling the durable table's account-keyed UPSERT: RecordSessionBinding
// displaces any prior binding for the same account and returns that session, and
// DeleteSessionBindingsForRunner returns the rows it removed (the reconnect
// sweep's authoritative reap set). It also models session_bindings_session_key:
// re-binding a session id that belongs to a DIFFERENT account is ErrConflict,
// never a silent steal. resolveCtxSystemRole records whether the LAST
// ResolveSessionAccount ran under the system role — the direct probe for the
// ack-path hazard fix. Concurrency-safe for parity with the real store.
type fakeBindingStore struct {
	mu       sync.Mutex
	tenant   store.TenantID
	bindings map[string]store.SessionBinding // session id -> binding

	resolveCalled        bool
	resolveCtxSystemRole bool
	recordErr            error
	resolveErr           error
	reverseErr           error
	deleteForRunnerErr   error
}

func newFakeBindingStore() *fakeBindingStore {
	return &fakeBindingStore{tenant: "tenant-a", bindings: map[string]store.SessionBinding{}}
}

func (f *fakeBindingStore) RecordSessionBinding(_ context.Context, sessionID string, accountID store.AccountID, runnerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return "", f.recordErr
	}
	// session_bindings_session_key: this session id already belongs to a
	// DIFFERENT account. The real store raises ErrConflict rather than
	// stealing the id, which is what keeps ResolveSessionAccount
	// single-valued; a fake that silently overwrote would hide the whole
	// class (a Runner restart re-mints "sess-1", so id reuse is routine).
	if b, ok := f.bindings[sessionID]; ok && b.AccountID != accountID {
		return "", fmt.Errorf("%w: session %q is already bound to a different agent", store.ErrConflict, sessionID)
	}
	var displaced string
	// Account-keyed UPSERT: a prior binding for this account is displaced.
	for sid, b := range f.bindings {
		if b.AccountID == accountID && sid != sessionID {
			displaced = sid
			delete(f.bindings, sid)
			break
		}
	}
	f.bindings[sessionID] = store.SessionBinding{SessionID: sessionID, AccountID: accountID, RunnerID: runnerID}
	return displaced, nil
}

func (f *fakeBindingStore) ResolveSessionAccount(ctx context.Context, sessionID string) (store.AccountID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalled = true
	f.resolveCtxSystemRole = store.IsSystemRole(ctx)
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	b, ok := f.bindings[sessionID]
	if !ok {
		return "", store.ErrNotFound
	}
	return b.AccountID, nil
}

func (f *fakeBindingStore) SessionForAccount(_ context.Context, accountID store.AccountID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reverseErr != nil {
		return "", f.reverseErr
	}
	for sid, b := range f.bindings {
		if b.AccountID == accountID {
			return sid, nil
		}
	}
	return "", store.ErrNotFound
}

func (f *fakeBindingStore) DeleteSessionBinding(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.bindings, sessionID)
	return nil
}

func (f *fakeBindingStore) DeleteSessionBindingsForRunner(_ context.Context, runnerID string) ([]store.SessionBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteForRunnerErr != nil {
		return nil, f.deleteForRunnerErr
	}
	var removed []store.SessionBinding
	for sid, b := range f.bindings {
		if b.RunnerID == runnerID {
			removed = append(removed, b)
			delete(f.bindings, sid)
		}
	}
	return removed, nil
}

func (f *fakeBindingStore) EffectiveTenant(context.Context) store.TenantID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tenant
}

// testRunnerID is the single Runner every fake binding is seeded under.
const testRunnerID = "runner-1"

// seed inserts a binding directly (test setup), bypassing the displacement path.
func (f *fakeBindingStore) seed(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[sessionID] = store.SessionBinding{SessionID: sessionID, AccountID: testAgentAccount, RunnerID: testRunnerID}
}

// fakeRoutingFabric is an in-memory RoutingFabric double: PublishBindingChange
// fans SYNCHRONOUSLY to every registered receiver, modelling the real fabric's
// fan-out-to-every-instance contract (fabric/routing_fabric_test.go proves the
// NATS wire; this proves the HUB's publish->peer-evict wiring without a broker).
// Synchronous fan-out is what lets a two-instance test assert eviction with no
// sleep. published records every change for the publish-side assertions.
type fakeRoutingFabric struct {
	mu        sync.Mutex
	receivers []func(fabric.BindingChange)
	published []fabric.BindingChange
}

// PublishBindingChange mirrors the real fabric's REJECTION contract before
// recording: (*Fabric).PublishBindingChange drops a change that fails
// BindingChange.valid() (empty tenant or session id, an op outside
// bound/unbound) and one whose Tenant disagrees with the subject tenant, and
// the hub only LOGS that error — so a malformed publish would silently leave
// every peer cache stale. A fake that accepted anything could not see it.
func (f *fakeRoutingFabric) PublishBindingChange(_ context.Context, tenant string, b fabric.BindingChange) error {
	if tenant == "" || b.Tenant != tenant {
		return fmt.Errorf("publish on tenant %q with change tenant %q: must agree and be non-empty", tenant, b.Tenant)
	}
	if b.SessionID == "" {
		return fmt.Errorf("binding change for tenant %q has an empty session id", tenant)
	}
	if b.Op != fabric.BindingBound && b.Op != fabric.BindingUnbound {
		return fmt.Errorf("binding change %s/%s has op %q, want %q or %q", b.Tenant, b.SessionID, b.Op, fabric.BindingBound, fabric.BindingUnbound)
	}
	f.mu.Lock()
	f.published = append(f.published, b)
	recv := make([]func(fabric.BindingChange), len(f.receivers))
	copy(recv, f.receivers)
	f.mu.Unlock()
	for _, fn := range recv {
		fn(b)
	}
	return nil
}

func (f *fakeRoutingFabric) subscribe(fn func(fabric.BindingChange)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivers = append(f.receivers, fn)
}

func (f *fakeRoutingFabric) publishedSnapshot() []fabric.BindingChange {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fabric.BindingChange, len(f.published))
	copy(out, f.published)
	return out
}

func runnerSubject() store.Subject {
	return store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}
}

// TestRestartResolvesPreRestartBindingBothDirections is the availability
// property this whole PR exists for. A Server restart brings up a FRESH hub with
// EMPTY maps, then the Runner (whose sessions are still alive) enrolls for the
// FIRST time on that hub — so no durable reap runs and the pre-restart rows
// survive. Both resolvers must then fall through the cache miss to the durable
// table: accountForSession (session->account) AND SessionForAccount
// (account->session), the two directions delivery + comms depend on.
func TestRestartResolvesPreRestartBindingBothDirections(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	bindings.seed("sess-1")
	hub.SetSessionBindingStore(bindings)

	// A FIRST enroll on a fresh hub (the restart case): reattached is false, so
	// enroll does NOT reap the durable rows — they are still valid.
	if reattached := hub.enroll(context.Background(), "runner-1", runnerSubject()); reattached {
		t.Fatal("first enroll on a fresh hub reported reattached=true, want false (no durable reap)")
	}

	// Forward: session -> account, resolved from the durable row (map is empty).
	account, ok := hub.accountForSession(context.Background(), "sess-1")
	if !ok || account != testAgentAccount {
		t.Fatalf("accountForSession(sess-1) after restart = (%q, %v), want (%s, true) — the pre-restart binding must resolve from the durable table", account, ok, testAgentAccount)
	}
	// Reverse: account -> session, the delivery consumer's direction.
	sessionID, ok := hub.SessionForAccount(context.Background(), testAgentAccount)
	if !ok || sessionID != "sess-1" {
		t.Fatalf("SessionForAccount(%s) after restart = (%q, %v), want (sess-1, true) — the reverse binding must resolve from the durable table too", testAgentAccount, sessionID, ok)
	}
}

// TestFailClosedStoppedNeverSeenAndPostReconnect pins all three fail-closed
// misses in one place, each returning ok=false (the CodeNotFound the caller
// mints): a STOPPED session, a NEVER-SEEN session, and a session that predates a
// Runner RECONNECT. The reconnect case is the crux of the read-through design:
// the durable rows SURVIVE a process death, so a naive "cache miss -> read the
// table" would resolve a dead session; the re-enroll's durable reap
// (DeleteSessionBindingsForRunner) is what keeps it fail-closed.
func TestFailClosedStoppedNeverSeenAndPostReconnect(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject())

	// Never-seen: no binding anywhere.
	if acct, ok := hub.accountForSession(context.Background(), "ghost"); ok {
		t.Fatalf("accountForSession(ghost) = (%q, true), want ok=false (never seen)", acct)
	}

	// Stopped: bound, then Stop's unbindSession removes it (durable delete too).
	hub.bindContainer("cont-1", testAgentAccount)
	hub.promoteSession(context.Background(), "cont-1", "sess-stop")
	if acct, ok := hub.accountForSession(context.Background(), "sess-stop"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-stop) before stop = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}
	hub.unbindSession(context.Background(), "sess-stop")
	if acct, ok := hub.accountForSession(context.Background(), "sess-stop"); ok {
		t.Fatalf("accountForSession(sess-stop) after stop = (%q, true), want ok=false (stopped, durable row deleted)", acct)
	}

	// Post-reconnect: a live binding whose row SURVIVES process death, then the
	// Runner reconnects (a re-enroll on the SAME hub). The re-enroll durably
	// reaps the rows, so the pre-reconnect session must fail closed even though a
	// row existed a moment ago.
	bindings.seed("sess-pre")
	// Populate the cache so the assertion is not merely a cold miss.
	if acct, ok := hub.accountForSession(context.Background(), "sess-pre"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-pre) before reconnect = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}
	// Runner reconnects: a re-enroll (reattached=true) durably reaps.
	if reattached := hub.enroll(context.Background(), "runner-1", runnerSubject()); !reattached {
		t.Fatal("second enroll reported reattached=false, want true (a Runner reconnect)")
	}
	if acct, ok := hub.accountForSession(context.Background(), "sess-pre"); ok {
		t.Fatalf("accountForSession(sess-pre) after reconnect = (%q, true), want ok=false — the reconnect reap must fail-close a pre-reconnect session", acct)
	}
	if _, ok := bindings.bindings["sess-pre"]; ok {
		t.Fatal("the durable row for sess-pre survived the reconnect reap; DeleteSessionBindingsForRunner must have removed it")
	}
}

// TestPeerBindingChangeEvictsOtherInstanceCache is the two-instance property:
// every Server keeps its own cache, so a binding change on ONE must invalidate
// the others. hubB re-points an account onto a new session; its promoteSession
// publishes BindingUnbound(old)+BindingBound(new) over the shared fabric, and
// hubA — subscribed to the fabric via OnBindingChange — must drop its now-stale
// cache entry for the old session. Without the subscribe-side eviction hubA
// would serve the stale binding forever, with no error to reveal it.
func TestPeerBindingChangeEvictsOtherInstanceCache(t *testing.T) {
	routing := &fakeRoutingFabric{}

	// Instance A: holds a cached binding for sess-old and receives peer changes.
	hubA := newHubOnly()
	routing.subscribe(hubA.OnBindingChange)
	hubA.mu.Lock()
	hubA.sessionAccounts["sess-old"] = testAgentAccount
	hubA.accountSessions[testAgentAccount] = "sess-old"
	hubA.mu.Unlock()

	// Instance B: the writer. A durable store seeded with the same account->old
	// binding (so recording the new one displaces it), an enrolled Runner (so the
	// record fires), and the shared routing fabric.
	hubB := newHubOnly()
	bindingsB := newFakeBindingStore()
	bindingsB.seed("sess-old")
	hubB.SetSessionBindingStore(bindingsB)
	hubB.SetRoutingFabric(routing)
	hubB.enroll(context.Background(), "runner-1", runnerSubject())
	hubB.bindContainer("cont-new", testAgentAccount)

	// B re-points the account onto sess-new: displaces sess-old, publishes the
	// two changes, which fan synchronously to hubA.OnBindingChange.
	hubB.promoteSession(context.Background(), "cont-new", "sess-new")

	// A published both an unbound-old and a bound-new (the publish-side contract).
	pub := routing.publishedSnapshot()
	var sawUnboundOld, sawBoundNew bool
	for _, c := range pub {
		if c.SessionID == "sess-old" && c.Op == fabric.BindingUnbound {
			sawUnboundOld = true
		}
		if c.SessionID == "sess-new" && c.Op == fabric.BindingBound {
			sawBoundNew = true
		}
	}
	if !sawUnboundOld || !sawBoundNew {
		t.Fatalf("promote published %+v, want a BindingUnbound(sess-old) and a BindingBound(sess-new)", pub)
	}

	// The receive-side property: hubA evicted its stale sess-old entry. hubA has
	// no store and no enrolled Runner, so a resolved sess-old could only come
	// from the cache — ok=false proves the eviction landed.
	if acct, ok := hubA.accountForSession(context.Background(), "sess-old"); ok {
		t.Fatalf("hubA.accountForSession(sess-old) after peer re-point = (%q, true), want ok=false — the peer's BindingUnbound must have evicted hubA's cache", acct)
	}
}

// TestDisplacedSessionResolvesNowhere pins the displacement contract on a SINGLE
// instance: promoting an account onto a NEW session displaces the account's
// PRIOR session, which must then resolve nowhere (the account moved off it). The
// store's RecordSessionBinding returns the displaced session; promoteSession
// evicts it from the forward map. Without that eviction sess-old would keep
// resolving to an account it no longer speaks for.
func TestDisplacedSessionResolvesNowhere(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject())

	// Bind the account to sess-old.
	hub.bindContainer("cont-old", testAgentAccount)
	hub.promoteSession(context.Background(), "cont-old", "sess-old")
	if acct, ok := hub.accountForSession(context.Background(), "sess-old"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-old) = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}

	// Re-point the SAME account onto sess-new: displaces sess-old.
	hub.bindContainer("cont-new", testAgentAccount)
	hub.promoteSession(context.Background(), "cont-new", "sess-new")

	// sess-new resolves; sess-old resolves nowhere.
	if acct, ok := hub.accountForSession(context.Background(), "sess-new"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-new) = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}
	if acct, ok := hub.accountForSession(context.Background(), "sess-old"); ok {
		t.Fatalf("accountForSession(sess-old) after displacement = (%q, true), want ok=false — the displaced session must resolve nowhere", acct)
	}
	// And the reverse points only at the new session.
	if sess, ok := hub.SessionForAccount(context.Background(), testAgentAccount); !ok || sess != "sess-new" {
		t.Fatalf("SessionForAccount(%s) = (%q, %v), want (sess-new, true)", testAgentAccount, sess, ok)
	}
}

// TestAckPathBindingReadNeverRunsUnderSystemRole is the security property PR3
// closes. deliverAck and forgeNotificationAck escalate ctx to the BYPASSRLS
// system role for the cursor advance — but the binding read must run BEFORE that
// escalation, on the request ctx, or a cache-miss table read could return a
// plausible row from an ARBITRARY tenant
// (store/session_bindings_pgtest_test.go::TestSessionForAccountUnderSystemRoleIsUnscoped).
// This forces a cache MISS (so the read-through table read actually runs) and
// asserts the ctx that read saw was NOT system-role, for BOTH ack arms.
func TestAckPathBindingReadNeverRunsUnderSystemRole(t *testing.T) {
	// deliverAck arm.
	t.Run("delivery_ack", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seed("sess-1")
		hub.SetSessionBindingStore(bindings)
		del := newFakeDeliveryStore()
		del.channels["m1"] = "chan-1"
		hub.SetDeliveryStore(del)
		// Enrolled Runner + empty maps (no bindSession) => the ack's account
		// resolve is a cache MISS that falls through to the read-through table.
		hub.enroll(context.Background(), "runner-1", runnerSubject())

		if err := hub.Deliver(context.Background(), RunnerEvent{
			RunnerSeq: 1, SessionID: "sess-1", Frame: deliveryAckFrame("m1"),
		}); err != nil {
			t.Fatalf("Deliver(delivery_ack) = %v, want nil", err)
		}

		if !bindings.resolveCalled {
			t.Fatal("the ack path never read the binding table; the cache-miss read-through did not run, so this test proves nothing")
		}
		if bindings.resolveCtxSystemRole {
			t.Fatal("the ack path's binding read ran under the system role — a BYPASSRLS read can return a foreign tenant's row; it must resolve on the request ctx BEFORE escalation")
		}
		// And the ack still applied (the resolve fed a real cursor advance).
		if acks := del.ackSnapshot(); len(acks) != 1 || acks[0].agent != testAgentAccount {
			t.Fatalf("ack cursor advances = %+v, want exactly one for %s", acks, testAgentAccount)
		}
	})

	// forgeNotificationAck arm.
	t.Run("forge_notification_ack", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seed("sess-1")
		hub.SetSessionBindingStore(bindings)
		del := newFakeDeliveryStore()
		hub.SetDeliveryStore(del)
		hub.enroll(context.Background(), "runner-1", runnerSubject())

		if err := hub.Deliver(context.Background(), RunnerEvent{
			RunnerSeq: 1, SessionID: "sess-1", Frame: forgeAckFrame("sub-1"),
		}); err != nil {
			t.Fatalf("Deliver(forge_notification_ack) = %v, want nil", err)
		}

		if !bindings.resolveCalled {
			t.Fatal("the forge-ack path never read the binding table; the cache-miss read-through did not run")
		}
		if bindings.resolveCtxSystemRole {
			t.Fatal("the forge-ack path's binding read ran under the system role — it must resolve on the request ctx BEFORE escalation")
		}
		if adv := del.forgeSnapshot(); len(adv) != 1 || adv[0].agent != testAgentAccount || adv[0].revision != testForgeRevision {
			t.Fatalf("forge cursor advances = %+v, want exactly one (%s, sub-1, rev-1)", adv, testAgentAccount)
		}
	})
}

// TestStoreFaultsFallBackWithoutLosingFailClosed drives the four durable-fault
// branches the design's fail-closed reasoning rests on. Each fallback is a
// deliberate availability choice (a store fault must not fail a Start that
// already succeeded on the Runner, nor wedge a reconnect), and each is only
// safe because it degrades toward the in-RAM truth rather than toward an
// unbound session resolving. Nothing else in the suite sets these error fields.
func TestStoreFaultsFallBackWithoutLosingFailClosed(t *testing.T) {
	faultErr := errors.New("durable fault")

	t.Run("promote with a record fault still resolves on this instance", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.recordErr = faultErr
		routing := &fakeRoutingFabric{}
		hub.SetSessionBindingStore(bindings)
		hub.SetRoutingFabric(routing)
		hub.enroll(context.Background(), "runner-1", runnerSubject())

		hub.bindContainer("cont-1", testAgentAccount)
		hub.promoteSession(context.Background(), "cont-1", "sess-1")

		if acct, ok := hub.accountForSession(context.Background(), "sess-1"); !ok || acct != testAgentAccount {
			t.Fatalf("accountForSession(sess-1) = (%q, %v), want (%s, true): a durable fault must not lose the live session", acct, ok, testAgentAccount)
		}
		// The write never landed, so there is nothing for a peer to invalidate.
		if pub := routing.publishedSnapshot(); len(pub) != 0 {
			t.Fatalf("published = %+v, want none: a failed durable write must not announce a binding that does not exist", pub)
		}
	})

	t.Run("re-enroll with a reap fault still clears and fires offline", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		hub.SetSessionBindingStore(bindings)
		hub.enroll(context.Background(), "runner-1", runnerSubject())
		hub.bindContainer("cont-1", testAgentAccount)
		hub.promoteSession(context.Background(), "cont-1", "sess-1")

		// The reconnect sweep now faults; the in-RAM snapshot must still drive it.
		bindings.deleteForRunnerErr = faultErr
		hub.enroll(context.Background(), "runner-1", runnerSubject())

		if acct, ok := hub.accountForSession(context.Background(), "sess-1"); ok {
			t.Fatalf("accountForSession(sess-1) = (%q, true) after a reconnect, want fail-closed: a reap fault must not leave a dead session resolvable", acct)
		}
	})

	t.Run("a resolve fault fails closed", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seed("sess-1")
		bindings.resolveErr = faultErr
		hub.SetSessionBindingStore(bindings)
		hub.enroll(context.Background(), "runner-1", runnerSubject())

		if acct, ok := hub.accountForSession(context.Background(), "sess-1"); ok {
			t.Fatalf("accountForSession(sess-1) = (%q, true), want fail-closed: a store fault must never resolve", acct)
		}
	})

	t.Run("a reverse-resolve fault fails closed", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seed("sess-1")
		bindings.reverseErr = faultErr
		hub.SetSessionBindingStore(bindings)
		hub.enroll(context.Background(), "runner-1", runnerSubject())

		if sess, ok := hub.SessionForAccount(context.Background(), testAgentAccount); ok {
			t.Fatalf("SessionForAccount = (%q, true), want fail-closed: a store fault must never resolve", sess)
		}
	})
}

// TestReusedSessionIDConflictIsSwallowed WITNESSES a known gap rather than a
// guarantee: production mints session ids with a counter that resets on every
// Runner restart, so a joint Server+Runner restart re-mints "sess-1" over a
// surviving row. The store answers ErrConflict to keep the binding
// single-valued; promoteSession currently treats it as a transient fault and
// falls back to RAM, leaving the durable row pointing at the OLD account. The
// single-instance MVP shadows that row and later retires it, but a second
// Server instance would read the stale durable truth. Pinned so the behaviour
// cannot change silently while the fix is decided (RIG-3108 review F1).
func TestReusedSessionIDConflictIsSwallowed(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject())

	// A row from before the joint restart, under a DIFFERENT account.
	bindings.mu.Lock()
	bindings.bindings["sess-1"] = store.SessionBinding{SessionID: "sess-1", AccountID: "acct-stale", RunnerID: "runner-1"}
	bindings.mu.Unlock()

	hub.bindContainer("cont-1", testAgentAccount)
	hub.promoteSession(context.Background(), "cont-1", "sess-1")

	// This instance resolves the NEW account from RAM.
	if acct, ok := hub.accountForSession(context.Background(), "sess-1"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-1) = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}
	// ...but the durable row still names the stale account: the divergence.
	bindings.mu.Lock()
	got := bindings.bindings["sess-1"].AccountID
	bindings.mu.Unlock()
	if got != "acct-stale" {
		t.Fatalf("durable binding for sess-1 = %q, want %q — if this now agrees with the cache, the ErrConflict gap was fixed and this witness test should become a real assertion", got, "acct-stale")
	}
}

// TestConcurrentResolveDuringAFaultingReapCannotResurrect fences the window
// between the re-enroll map-clear and the reap's return. The reap is a store
// round-trip that can block for seconds, and a resolver arriving in that gap
// misses the just-cleared cache — so if read-through were still permitted it
// would read back a row the failing reap never deleted, resurrecting a
// session the reconnect declared dead. blockingBindingStore holds the reap
// open until the concurrent resolve has run, which makes the interleaving
// deterministic rather than hoping the scheduler produces it.
func TestConcurrentResolveDuringAFaultingReapCannotResurrect(t *testing.T) {
	hub := newHubOnly()
	bindings := &blockingBindingStore{
		fakeBindingStore: newFakeBindingStore(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject())
	hub.bindContainer("cont-1", testAgentAccount)
	hub.promoteSession(context.Background(), "cont-1", "sess-1")

	// The reconnect's reap will fault, leaving the sess-1 row in place.
	bindings.deleteForRunnerErr = errors.New("durable fault")

	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.enroll(context.Background(), "runner-1", runnerSubject())
	}()

	<-bindings.entered // maps are cleared; the reap is in flight and will fail
	acct, ok := hub.accountForSession(context.Background(), "sess-1")
	close(bindings.release)
	<-done

	if ok {
		t.Fatalf("accountForSession(sess-1) = (%q, true) while a faulting reap was in flight, want fail-closed: the surviving row must not be readable between the map-clear and the reap's return", acct)
	}
}

// blockingBindingStore parks DeleteSessionBindingsForRunner so a test can act
// inside the reap window; every other method is the plain fake's.
type blockingBindingStore struct {
	*fakeBindingStore
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBindingStore) DeleteSessionBindingsForRunner(ctx context.Context, runnerID string) ([]store.SessionBinding, error) {
	close(b.entered)
	<-b.release
	return b.fakeBindingStore.DeleteSessionBindingsForRunner(ctx, runnerID)
}
