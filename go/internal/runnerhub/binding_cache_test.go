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
//   - a Server RESTART reaps the Runner's pre-restart rows on its first enroll;
//   - a cold cache resolves a post-enroll binding from the durable row, both
//     directions;
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
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
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
	tenantErr            error          // if set, SessionBindingTenant returns it
	effective            store.TenantID // if set, EffectiveTenant returns it instead of tenant
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
	f.bindings[sessionID] = store.SessionBinding{TenantID: f.tenant, SessionID: sessionID, AccountID: accountID, RunnerID: runnerID}
	return displaced, nil
}

func (f *fakeBindingStore) ResolveSessionBinding(ctx context.Context, sessionID string) (store.AccountID, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalled = true
	f.resolveCtxSystemRole = store.IsSystemRole(ctx)
	if f.resolveErr != nil {
		return "", "", f.resolveErr
	}
	b, ok := f.bindings[sessionID]
	if !ok {
		return "", "", store.ErrNotFound
	}
	return b.AccountID, b.RunnerID, nil
}

func (f *fakeBindingStore) SessionForAccount(_ context.Context, accountID store.AccountID) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reverseErr != nil {
		return "", "", f.reverseErr
	}
	for sid, b := range f.bindings {
		if b.AccountID == accountID {
			return sid, b.RunnerID, nil
		}
	}
	return "", "", store.ErrNotFound
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
	if f.effective != "" {
		return f.effective
	}
	return f.tenant
}

// testRunnerID is the default Runner id used by fakeBindingStore.seed.
const testRunnerID = "runner-1"

// seed inserts a binding directly (test setup), bypassing the displacement path.
func (f *fakeBindingStore) SessionBindingTenant(_ context.Context, sessionID, runnerID string) (store.TenantID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tenantErr != nil {
		return "", f.tenantErr
	}
	if b, ok := f.bindings[sessionID]; ok && b.RunnerID == runnerID {
		return f.tenant, nil
	}
	return "", store.ErrNotFound
}

func (f *fakeBindingStore) seed(sessionID string) {
	f.seedBinding(sessionID, testAgentAccount, testRunnerID)
}

func (f *fakeBindingStore) seedBinding(sessionID string, accountID store.AccountID, runnerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindings[sessionID] = store.SessionBinding{TenantID: f.tenant, SessionID: sessionID, AccountID: accountID, RunnerID: runnerID}
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

// RIG-4223: a Server restart brings up a fresh hub, so the Runner's enroll is a
// first one. Its pre-restart sessions are dead (the Runner exits with its stream
// and sweeps containers), so the first enroll must reap their rows too, or a wake
// reads a dead session back as live and silently no-ops.
func TestFirstEnrollReapsPreRestartBindings(t *testing.T) {
	hub := newHubOnly()
	reap := &fakeSessionReapSink{}
	hub.SetSessionReapSink(reap)
	bindings := newFakeBindingStore()
	bindings.seed("sess-1")
	hub.SetSessionBindingStore(bindings)

	if reattached, err := hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err != nil || reattached {
		t.Fatalf("first enroll = (%v, %v), want (false, nil)", reattached, err)
	}

	if _, ok := bindings.bindings["sess-1"]; ok {
		t.Fatal("the pre-restart row for sess-1 survived the first enroll; it names a dead session")
	}
	if account, ok := hub.accountForSession(context.Background(), "sess-1"); ok {
		t.Fatalf("accountForSession(sess-1) after restart = (%q, true), want ok=false", account)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), testAgentAccount); ok {
		t.Fatalf("SessionForAccount(%s) after restart = (%q, true), want ok=false — a wake would skip the dead agent", testAgentAccount, sessionID)
	}
	if calls := reap.snapshot(); len(calls) != 1 || !slices.Equal(calls[0], []string{"sess-1"}) {
		t.Fatalf("reap edges = %v, want one naming sess-1 (held delivers for the dead session must be dropped)", calls)
	}
}

// A row bound after enroll (another Server instance promoted it) resolves from
// the durable table in both directions on a cold cache.
func TestColdCacheResolvesPostEnrollBindingBothDirections(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindings.seed("sess-1")

	account, ok := hub.accountForSession(context.Background(), "sess-1")
	if !ok || account != testAgentAccount {
		t.Fatalf("accountForSession(sess-1) = (%q, %v), want (%s, true) — the binding must resolve from the durable table", account, ok, testAgentAccount)
	}
	sessionID, ok := hub.SessionForAccount(context.Background(), testAgentAccount)
	if !ok || sessionID != "sess-1" {
		t.Fatalf("SessionForAccount(%s) = (%q, %v), want (sess-1, true) — the reverse binding must resolve from the durable table too", testAgentAccount, sessionID, ok)
	}
}
func TestFaultedReapForOneRunnerKeepsOtherRunnersReadThrough(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	bindings.seedBinding("sess-1", "acct-runner-1", "runner-1")
	bindings.deleteForRunnerErr = errors.New("durable fault")
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	// Model the other attached Runner in this single-Runner hub test.
	hub.mu.Lock()
	hub.runner = &attachedRunner{id: "runner-2", router: newCommandRouter()}
	hub.runnerEpoch["runner-2"] = hub.bindingEpoch
	hub.mu.Unlock()
	bindings.seedBinding("sess-y", "acct-runner-2", "runner-2")

	account, ok := hub.accountForSession(context.Background(), "sess-y")
	if !ok || account != "acct-runner-2" {
		t.Fatalf("accountForSession(sess-y) = (%q, %v), want (acct-runner-2, true)", account, ok)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), "acct-runner-2"); !ok || sessionID != "sess-y" {
		t.Fatalf("SessionForAccount(acct-runner-2) = (%q, %v), want (sess-y, true)", sessionID, ok)
	}
	if account, ok := hub.accountForSession(context.Background(), "sess-1"); ok {
		t.Fatalf("accountForSession(sess-1) = (%q, true), want fail-closed after runner-1 reap fault", account)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), "acct-runner-1"); ok {
		t.Fatalf("SessionForAccount(acct-runner-1) = (%q, true), want fail-closed after runner-1 reap fault", sessionID)
	}
}

func TestSuccessfulReenrollClearsOnlyItsOwnReapFault(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.enroll(context.Background(), "runner-2", store.Subject{Kind: store.SubjectRunner, ID: "runner-2"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	bindings.seedBinding("sess-runner-2-stale", "acct-runner-2-stale", "runner-2")
	bindings.deleteForRunnerErr = errors.New("runner-2 durable fault")
	hub.enroll(context.Background(), "runner-2", store.Subject{Kind: store.SubjectRunner, ID: "runner-2"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	bindings.deleteForRunnerErr = nil
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindings.seedBinding("sess-runner-1-fresh", "acct-runner-1-fresh", "runner-1")

	if account, ok := hub.accountForSession(context.Background(), "sess-runner-1-fresh"); !ok || account != "acct-runner-1-fresh" {
		t.Fatalf("accountForSession(sess-runner-1-fresh) = (%q, %v), want runner-1's fresh binding", account, ok)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), "acct-runner-1-fresh"); !ok || sessionID != "sess-runner-1-fresh" {
		t.Fatalf("SessionForAccount(acct-runner-1-fresh) = (%q, %v), want runner-1's fresh binding", sessionID, ok)
	}
	if account, ok := hub.accountForSession(context.Background(), "sess-runner-2-stale"); ok {
		t.Fatalf("accountForSession(sess-runner-2-stale) = (%q, true), want fail-closed after runner-2 reap fault", account)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), "acct-runner-2-stale"); ok {
		t.Fatalf("SessionForAccount(acct-runner-2-stale) = (%q, true), want fail-closed after runner-2 reap fault", sessionID)
	}
}

func TestSuccessfulReenrollRestoresItsOwnReadThrough(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindings.seedBinding("sess-runner-1-stale", "acct-runner-1-stale", "runner-1")
	bindings.deleteForRunnerErr = errors.New("durable fault")
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	if account, ok := hub.accountForSession(context.Background(), "sess-runner-1-stale"); ok {
		t.Fatalf("accountForSession(sess-runner-1-stale) = (%q, true), want refused after faulted reap", account)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), "acct-runner-1-stale"); ok {
		t.Fatalf("SessionForAccount(acct-runner-1-stale) = (%q, true), want refused after faulted reap", sessionID)
	}

	bindings.deleteForRunnerErr = nil
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindings.seedBinding("sess-runner-1-fresh", "acct-runner-1-fresh", "runner-1")
	if account, ok := hub.accountForSession(context.Background(), "sess-runner-1-fresh"); !ok || account != "acct-runner-1-fresh" {
		t.Fatalf("accountForSession(sess-runner-1-fresh) = (%q, %v), want fresh binding after successful reap", account, ok)
	}
	if sessionID, ok := hub.SessionForAccount(context.Background(), "acct-runner-1-fresh"); !ok || sessionID != "sess-runner-1-fresh" {
		t.Fatalf("SessionForAccount(acct-runner-1-fresh) = (%q, %v), want fresh binding after successful reap", sessionID, ok)
	}
}

// After a Server restart the owner check must hold on the durable read-through,
// not only on the in-RAM cache.
func TestAccountForRunnerSessionReadThroughChecksOwner(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindings.seed("sess-1")

	if _, ok := hub.accountForRunnerSession(context.Background(), "runner-2", "sess-1"); ok {
		t.Fatal("foreign Runner resolved durable session binding, want false")
	}
	if account, ok := hub.accountForRunnerSession(context.Background(), testRunnerID, "sess-1"); !ok || account != testAgentAccount {
		t.Fatalf("owner Runner resolved durable binding = (%q, %v), want (%s, true)", account, ok, testAgentAccount)
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
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	// Never-seen: no binding anywhere.
	if acct, ok := hub.accountForSession(context.Background(), "ghost"); ok {
		t.Fatalf("accountForSession(ghost) = (%q, true), want ok=false (never seen)", acct)
	}

	// Stopped: bound, then Stop's unbindSession removes it (durable delete too).
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
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
	if reattached, err := hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err != nil || !reattached {
		t.Fatalf("second enroll = (%v, %v), want (true, nil)", reattached, err)
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
	hubA.sessionAccounts["sess-old"] = sessionBinding{account: testAgentAccount, runnerID: testRunnerID}
	hubA.accountSessions[testAgentAccount] = "sess-old"
	hubA.mu.Unlock()

	// Instance B: the writer. A durable store seeded with the same account->old
	// binding (so recording the new one displaces it), an enrolled Runner (so the
	// record fires), and the shared routing fabric.
	hubB := newHubOnly()
	bindingsB := newFakeBindingStore()
	hubB.SetSessionBindingStore(bindingsB)
	hubB.SetRoutingFabric(routing)
	hubB.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindingsB.seed("sess-old")
	hubB.bindContainer("cont-new", testAgentAccount, "runner-1")

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
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	// Bind the account to sess-old.
	hub.bindContainer("cont-old", testAgentAccount, "runner-1")
	hub.promoteSession(context.Background(), "cont-old", "sess-old")
	if acct, ok := hub.accountForSession(context.Background(), "sess-old"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-old) = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}

	// Re-point the SAME account onto sess-new: displaces sess-old.
	hub.bindContainer("cont-new", testAgentAccount, "runner-1")
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
		hub.SetSessionBindingStore(bindings)
		del := newFakeDeliveryStore()
		del.channels["m1"] = "chan-1"
		hub.SetDeliveryStore(del)
		// Enrolled Runner + empty maps (no bindSession) => the ack's account
		// resolve is a cache MISS that falls through to the read-through table.
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		bindings.seed("sess-1")

		if err := hub.Deliver(context.Background(), RunnerEvent{
			RunnerID:  testRunnerID,
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
		hub.SetSessionBindingStore(bindings)
		del := newFakeDeliveryStore()
		hub.SetDeliveryStore(del)
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		bindings.seed("sess-1")

		if err := hub.Deliver(context.Background(), RunnerEvent{
			RunnerID:  testRunnerID,
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
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

		hub.bindContainer("cont-1", testAgentAccount, "runner-1")
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
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		hub.bindContainer("cont-1", testAgentAccount, "runner-1")
		hub.promoteSession(context.Background(), "cont-1", "sess-1")

		// The reconnect sweep now faults; the in-RAM snapshot must still drive it.
		bindings.deleteForRunnerErr = faultErr
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

		if acct, ok := hub.accountForSession(context.Background(), "sess-1"); ok {
			t.Fatalf("accountForSession(sess-1) = (%q, true) after a reconnect, want fail-closed: a reap fault must not leave a dead session resolvable", acct)
		}
	})

	t.Run("first enroll with a reap fault keeps read-through refused", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seed("sess-1")
		bindings.deleteForRunnerErr = faultErr
		hub.SetSessionBindingStore(bindings)
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

		if acct, ok := hub.accountForSession(context.Background(), "sess-1"); ok {
			t.Fatalf("accountForSession(sess-1) = (%q, true), want fail-closed: the unreaped pre-restart row names a dead session", acct)
		}
		if sess, ok := hub.SessionForAccount(context.Background(), testAgentAccount); ok {
			t.Fatalf("SessionForAccount = (%q, true), want fail-closed: the unreaped pre-restart row names a dead session", sess)
		}
	})

	t.Run("a resolve fault fails closed", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seed("sess-1")
		bindings.resolveErr = faultErr
		hub.SetSessionBindingStore(bindings)
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

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
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

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
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	// A row from before the joint restart, under a DIFFERENT account.
	bindings.mu.Lock()
	bindings.bindings["sess-1"] = store.SessionBinding{TenantID: bindings.tenant, SessionID: "sess-1", AccountID: "acct-stale", RunnerID: "runner-1"}
	bindings.mu.Unlock()

	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
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
func TestReadDuringSuccessfulReapCannotResurrect(t *testing.T) {
	for _, direction := range []string{"forward", "reverse"} {
		t.Run(direction, func(t *testing.T) {
			hub := newHubOnly()
			plain := newFakeBindingStore()
			hub.SetSessionBindingStore(plain)
			hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
			plain.seed("sess-racing")

			bindings := &reapAndReadBlockingBindingStore{
				fakeBindingStore: plain,
				reapEntered:      make(chan struct{}),
				reapRelease:      make(chan struct{}),
				readEntered:      make(chan struct{}),
				readRelease:      make(chan struct{}),
			}
			hub.SetSessionBindingStore(bindings)
			reapDone := make(chan struct{})
			go func() {
				defer close(reapDone)
				hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
			}()
			<-bindings.reapEntered

			resolved := make(chan string, 1)
			go func() {
				if direction == "forward" {
					account, _ := hub.accountForSession(store.WithTenant(context.Background(), "tenant-a"), "sess-racing")
					resolved <- string(account)
					return
				}
				sessionID, _ := hub.SessionForAccount(store.WithTenant(context.Background(), "tenant-a"), testAgentAccount)
				resolved <- sessionID
			}()
			<-bindings.readEntered

			close(bindings.reapRelease)
			<-reapDone
			close(bindings.readRelease)
			if got := <-resolved; got != "" {
				t.Fatalf("%s lookup returned %q after successful reap, want fail-closed", direction, got)
			}
			hub.mu.Lock()
			_, forwardCached := hub.sessionAccounts["sess-racing"]
			_, reverseCached := hub.accountSessions[testAgentAccount]
			hub.mu.Unlock()
			if forwardCached || reverseCached {
				t.Fatalf("%s lookup cached a row deleted by the successful reap", direction)
			}
		})
	}
}

// Enrolls are serialized, so a newer faulted reap always completes last and its fault stands.
func TestOlderSuccessfulReapCannotClearNewerFault(t *testing.T) {
	hub := newHubOnly()
	plain := newFakeBindingStore()
	hub.SetSessionBindingStore(plain)
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	plain.mu.Lock()
	plain.deleteForRunnerErr = errors.New("newer reap fault")
	plain.mu.Unlock()
	if _, err := hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err == nil {
		t.Fatal("faulting reap succeeded, want durable error")
	}
	plain.seedBinding("sess-after", "acct-runner-1-after", "runner-1")
	if account, ok := hub.accountForSession(store.WithTenant(context.Background(), "tenant-a"), "sess-after"); ok {
		t.Fatalf("accountForSession(sess-after) = (%q, true), want refusal while newer enroll reap remains faulted", account)
	}
	if sessionID, ok := hub.SessionForAccount(store.WithTenant(context.Background(), "tenant-a"), "acct-runner-1-after"); ok {
		t.Fatalf("SessionForAccount(acct-runner-1-after) = (%q, true), want refusal while newer enroll reap remains faulted", sessionID)
	}
}

// A successful reap after a faulted one restores read-through.
func TestOlderFaultedReapAfterNewerSuccessKeepsReadThrough(t *testing.T) {
	hub := newHubOnly()
	plain := newFakeBindingStore()
	hub.SetSessionBindingStore(plain)
	plain.mu.Lock()
	plain.deleteForRunnerErr = errors.New("older reap fault")
	plain.mu.Unlock()
	if _, err := hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err == nil {
		t.Fatal("faulting reap succeeded, want error")
	}
	plain.mu.Lock()
	plain.deleteForRunnerErr = nil
	plain.mu.Unlock()
	if _, err := hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err != nil {
		t.Fatalf("newer successful reap = %v, want nil", err)
	}
	plain.seedBinding("sess-runner-1-after", "acct-runner-1-after", "runner-1")
	if account, ok := hub.accountForSession(store.WithTenant(context.Background(), "tenant-a"), "sess-runner-1-after"); !ok || account != "acct-runner-1-after" {
		t.Fatalf("accountForSession(sess-runner-1-after) = (%q, %v), want read-through after newer successful reap", account, ok)
	}
	if sessionID, ok := hub.SessionForAccount(store.WithTenant(context.Background(), "tenant-a"), "acct-runner-1-after"); !ok || sessionID != "sess-runner-1-after" {
		t.Fatalf("SessionForAccount(acct-runner-1-after) = (%q, %v), want read-through after newer successful reap", sessionID, ok)
	}
}

type reapAndReadBlockingBindingStore struct {
	*fakeBindingStore
	reapEntered chan struct{}
	reapRelease chan struct{}
	readEntered chan struct{}
	readRelease chan struct{}
}

func (b *reapAndReadBlockingBindingStore) DeleteSessionBindingsForRunner(ctx context.Context, runnerID string) ([]store.SessionBinding, error) {
	close(b.reapEntered)
	<-b.reapRelease
	return b.fakeBindingStore.DeleteSessionBindingsForRunner(ctx, runnerID)
}

func (b *reapAndReadBlockingBindingStore) ResolveSessionBinding(ctx context.Context, sessionID string) (store.AccountID, string, error) {
	account, runnerID, err := b.fakeBindingStore.ResolveSessionBinding(ctx, sessionID)
	close(b.readEntered)
	<-b.readRelease
	return account, runnerID, err
}

func (b *reapAndReadBlockingBindingStore) SessionForAccount(ctx context.Context, accountID store.AccountID) (string, string, error) {
	sessionID, runnerID, err := b.fakeBindingStore.SessionForAccount(ctx, accountID)
	close(b.readEntered)
	<-b.readRelease
	return sessionID, runnerID, err
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
	plain := newFakeBindingStore()
	hub.SetSessionBindingStore(plain)
	hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(context.Background(), "cont-1", "sess-1")

	// Park only the reconnect's reap; it will fault, leaving the sess-1 row in place.
	bindings := &blockingBindingStore{
		fakeBindingStore: plain,
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	hub.SetSessionBindingStore(bindings)
	bindings.deleteForRunnerErr = errors.New("durable fault")

	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.enroll(context.Background(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
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
	once    sync.Once
}

func (b *blockingBindingStore) DeleteSessionBindingsForRunner(ctx context.Context, runnerID string) ([]store.SessionBinding, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.fakeBindingStore.DeleteSessionBindingsForRunner(ctx, runnerID)
}

// A promotion racing an in-flight reap waits for the whole enroll; on main the write
// landed mid-delete and the reap's WHERE runner_id sweep then erased the live binding.
func TestOlderEnrollReapCannotDeleteNewerPromotion(t *testing.T) {
	hub := newHubOnly()
	plain := newFakeBindingStore()
	hub.SetSessionBindingStore(plain)
	hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	bindings := &overlapDetectingBindingStore{
		fakeBindingStore: plain,
		entered:          make(chan struct{}),
		recorded:         make(chan struct{}),
	}
	hub.SetSessionBindingStore(bindings)
	oldReapDone := make(chan struct{})
	go func() {
		defer close(oldReapDone)
		hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	}()
	<-bindings.entered
	// Bound after the reap's map-clear, as a Start on the re-enrolled Runner would be.
	hub.bindContainer("cont-new", testAgentAccount, testRunnerID)
	promoteDone := make(chan struct{})
	go func() {
		defer close(promoteDone)
		hub.promoteSession(context.Background(), "cont-new", "sess-new")
	}()
	waitParkedOrDone(t, "(*Hub).promoteSession", bindings.recorded)
	bindings.releaseDelete()
	<-oldReapDone
	<-promoteDone

	if bindings.overlapped() {
		t.Fatal("promotion wrote its binding while the older reap's delete was in flight")
	}
	// Ordered after the reap, the live session's binding survives in the store and cache.
	if sessionID, runnerID, err := plain.SessionForAccount(store.WithTenant(context.Background(), "tenant-a"), testAgentAccount); err != nil || sessionID != "sess-new" || runnerID != testRunnerID {
		t.Fatalf("durable SessionForAccount = (%q, %q, %v), want (sess-new, %s, nil)", sessionID, runnerID, err, testRunnerID)
	}
	if sessionID, ok := hub.CachedSessionForAccount(testAgentAccount); !ok || sessionID != "sess-new" {
		t.Fatalf("CachedSessionForAccount = (%q, %v), want sess-new", sessionID, ok)
	}
}

// overlapDetectingBindingStore parks the first reap and records whether a binding
// write arrived while it was parked.
type overlapDetectingBindingStore struct {
	*fakeBindingStore
	entered  chan struct{}
	recorded chan struct{}
	mu       sync.Mutex
	parked   bool
	overlap  bool
	release  chan struct{}
	once     sync.Once
}

func (b *overlapDetectingBindingStore) DeleteSessionBindingsForRunner(ctx context.Context, runnerID string) ([]store.SessionBinding, error) {
	b.once.Do(func() {
		b.mu.Lock()
		b.parked = true
		b.release = make(chan struct{})
		release := b.release
		b.mu.Unlock()
		close(b.entered)
		<-release
	})
	return b.fakeBindingStore.DeleteSessionBindingsForRunner(ctx, runnerID)
}

func (b *overlapDetectingBindingStore) RecordSessionBinding(ctx context.Context, sessionID string, accountID store.AccountID, runnerID string) (string, error) {
	b.mu.Lock()
	if b.parked {
		b.overlap = true
	}
	b.mu.Unlock()
	close(b.recorded)
	return b.fakeBindingStore.RecordSessionBinding(ctx, sessionID, accountID, runnerID)
}

func (b *overlapDetectingBindingStore) releaseDelete() {
	b.mu.Lock()
	b.parked = false
	close(b.release)
	b.mu.Unlock()
}

func (b *overlapDetectingBindingStore) overlapped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overlap
}

// waitParkedOrDone returns once fn's goroutine is parked acquiring a mutex (read from
// goroutine stacks rather than a timer) or done is closed.
func waitParkedOrDone(t *testing.T, fn string, done <-chan struct{}) {
	t.Helper()
	deadline := timeAfter()
	buf := make([]byte, 1<<20)
	for {
		select {
		case <-done:
			return
		case <-deadline:
			t.Fatalf("%s neither finished nor parked on a mutex", fn)
		default:
		}
		stacks := string(buf[:runtime.Stack(buf, true)])
		for g := range strings.SplitSeq(stacks, "\n\n") {
			if strings.Contains(g, fn) && strings.Contains(g, "[sync.Mutex.Lock") {
				return
			}
		}
		runtime.Gosched()
	}
}

func TestEnrollDuringPromotionCannotResurrectBinding(t *testing.T) {
	hub := newHubOnly()
	plain := newFakeBindingStore()
	hub.SetSessionBindingStore(plain)
	hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-racing", testAgentAccount, testRunnerID)

	bindings := &blockingRecordBindingStore{
		fakeBindingStore: plain,
		recordEntered:    make(chan struct{}),
		recordRelease:    make(chan struct{}),
	}
	hub.SetSessionBindingStore(bindings)
	promoteDone := make(chan struct{})
	go func() {
		defer close(promoteDone)
		hub.promoteSession(context.Background(), "cont-racing", "sess-racing")
	}()
	<-bindings.recordEntered

	enrollDone := make(chan struct{})
	go func() {
		defer close(enrollDone)
		hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	}()
	waitParkedOrDone(t, "(*Hub).enroll", enrollDone)
	close(bindings.recordRelease)
	<-promoteDone
	<-enrollDone

	hub.mu.Lock()
	_, forwardCached := hub.sessionAccounts["sess-racing"]
	_, reverseCached := hub.accountSessions[testAgentAccount]
	hub.mu.Unlock()
	if forwardCached || reverseCached {
		t.Fatalf("promotion repopulated cache after enroll: forward=%v reverse=%v", forwardCached, reverseCached)
	}
	if sessionID, _, err := plain.SessionForAccount(store.WithTenant(context.Background(), "tenant-a"), testAgentAccount); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("durable SessionForAccount = (%q, %v), want ErrNotFound after enroll", sessionID, err)
	}
}

func TestSessionForAccountRejectsForeignRunnerBinding(t *testing.T) {
	t.Run("read-through", func(t *testing.T) {
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		bindings.seedBinding("sess-foreign", testAgentAccount, "runner-foreign")
		hub.SetSessionBindingStore(bindings)
		hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

		if sessionID, ok := hub.SessionForAccount(store.WithTenant(context.Background(), "tenant-a"), testAgentAccount); ok {
			t.Fatalf("SessionForAccount returned foreign session %q from durable read-through", sessionID)
		}
		if _, ok := hub.CachedSessionForAccount(testAgentAccount); ok {
			t.Fatal("foreign durable row was cached")
		}
	})

	t.Run("cache-hit", func(t *testing.T) {
		hub := newHubOnly()
		hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		hub.mu.Lock()
		hub.sessionAccounts["sess-foreign"] = sessionBinding{account: testAgentAccount, runnerID: "runner-foreign"}
		hub.accountSessions[testAgentAccount] = "sess-foreign"
		hub.mu.Unlock()

		if sessionID, ok := hub.SessionForAccount(context.Background(), testAgentAccount); ok {
			t.Fatalf("SessionForAccount returned foreign cached session %q", sessionID)
		}
	})
	t.Run("dispatch-owner", func(t *testing.T) {
		hub := newHubOnly()
		hub.enroll(context.Background(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		router, _, err := hub.routerFor("sess-foreign")
		if err != nil {
			t.Fatal(err)
		}
		recorded := newRecordingSend()
		router.attach(recorded.send)
		t.Cleanup(func() { router.detach(errStreamClosed) })
		hub.mu.Lock()
		hub.sessionAccounts["sess-foreign"] = sessionBinding{account: testAgentAccount, runnerID: "runner-foreign"}
		hub.accountSessions[testAgentAccount] = "sess-foreign"
		hub.mu.Unlock()
		if sessionID, ok := hub.SessionForAccount(context.Background(), testAgentAccount); ok {
			t.Fatalf("SessionForAccount returned foreign cached session %q", sessionID)
		}
		if err := hub.DispatchControl(context.Background(), "sess-foreign", &compassv1internal.AgentControl{
			Control: &compassv1internal.AgentControl_Deliver{Deliver: &compassv1internal.DeliverControl{}},
		}); err != nil {
			t.Fatal(err)
		}
		waitRecorded(t, recorded, 1)
	})
}

type blockingRecordBindingStore struct {
	*fakeBindingStore
	recordEntered chan struct{}
	recordRelease chan struct{}
}

func (b *blockingRecordBindingStore) RecordSessionBinding(ctx context.Context, sessionID string, accountID store.AccountID, runnerID string) (string, error) {
	close(b.recordEntered)
	<-b.recordRelease
	return b.fakeBindingStore.RecordSessionBinding(ctx, sessionID, accountID, runnerID)
}
