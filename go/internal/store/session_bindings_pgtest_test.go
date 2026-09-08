//go:build pgtest

package store

// Session bindings: the durable (session -> agent account, Runner) record the
// RunnerHub has so far held only in RAM (RIG-3108 / RIG-2861 §T4). Four things
// must hold, and all four are DATABASE invariants rather than Go logic — the
// account-keyed PK, the unique index on (tenant_id, session_id), the FK to
// agent_accounts, and the RLS policy — so all four are pgtest-backed; a mock
// would only re-assert the Go.
//
// The table is keyed on the ACCOUNT, so re-pointing an account at a newer
// session is an UPSERT that returns what it displaced (the hub does exactly this
// and has nowhere to put a refusal), one session id speaks for at most one
// account (the unique index), a miss FAILS CLOSED (a comms call resolves its
// scope through these reads, so an empty AccountID with a nil error would flow
// onward as a real principal), and the reconnect sweep RETURNS what it removed
// (the returned rows drive presence DISCONNECTED and the held-deliver reap — a
// bare DELETE would satisfy the invariant while dropping both).
//
// context.Background is the test root (the pgtest-suite convention, sibling
// updated_at_pgtest_test.go and forge_cursors_pgtest_test.go).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mustBind records a binding a test only needs to SUCCEED, returning the session
// id it displaced. Most cases below care about a later assertion, not this call.
// ctx is the SECOND parameter, after t, matching mustTopic (messages_test.go).
func mustBind(t *testing.T, ctx context.Context, s *Store, sessionID string, accountID AccountID, runnerID string) string {
	t.Helper()
	displaced, err := s.RecordSessionBinding(ctx, sessionID, accountID, runnerID)
	if err != nil {
		t.Fatalf("RecordSessionBinding(%q, %q, %q): %v", sessionID, accountID, runnerID, err)
	}
	return displaced
}

// bindingTimes reads a binding row's created_at/updated_at directly, so a test
// asserts the persisted timestamps rather than trusting a return value — the
// same posture as tenantOf.
//
// It goes through s.pool, which is the OWNER connection: no SET LOCAL ROLE, no
// GUC, so RLS does not apply and the read is cross-tenant. The tenant_id
// predicate is therefore explicit and NOT optional — without it a same-session-id
// row in another tenant would make the scan ambiguous, which is exactly the
// coexistence the tenant-fold cases below create.
func bindingTimes(t *testing.T, ctx context.Context, s *Store, sessionID string) (createdAt, updatedAt time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(ctx,
		"SELECT created_at, updated_at FROM session_bindings WHERE session_id = $1 AND tenant_id = $2",
		sessionID, string(s.resolveTenant(ctx)),
	).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatalf("read timestamps of binding %q: %v", sessionID, err)
	}
	return createdAt, updatedAt
}

// countBindings counts binding rows for an account IN ctx's tenant, so a
// re-point test can prove the row was REPLACED rather than accumulated. Same
// owner-connection caveat as bindingTimes: the tenant predicate is what keeps
// "exactly 1" a per-tenant count rather than a cross-tenant one, so a second
// tenant holding a binding for the SAME account id does not inflate it.
func countBindings(t *testing.T, ctx context.Context, s *Store, accountID AccountID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM session_bindings WHERE agent_account_id = $1 AND tenant_id = $2",
		string(accountID), string(s.resolveTenant(ctx)),
	).Scan(&n); err != nil {
		t.Fatalf("count bindings of %q: %v", accountID, err)
	}
	return n
}

// TestRecordSessionBindingRoundTripsBothDirections pins the base contract both
// reads depend on: what the relay bound is what both directions read back — the
// account resolvable from the session id (the inbound comms-call read) and the
// session resolvable from the account (the delivery-dispatch read). A FIRST bind
// displaces nothing, so it must report the empty session id rather than any
// placeholder the caller would try to reap.
func TestRecordSessionBindingRoundTripsBothDirections(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	displaced, err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-1")
	if err != nil {
		t.Fatalf("RecordSessionBinding: %v", err)
	}
	if displaced != "" {
		t.Fatalf("first bind displaced %q, want \"\" — the account held no prior session, and a caller reaping a phantom id would clear a live registry entry", displaced)
	}

	gotAccount, err := s.ResolveSessionAccount(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ResolveSessionAccount: %v", err)
	}
	if gotAccount != agent.ID {
		t.Fatalf("ResolveSessionAccount = %q, want the bound agent %q", gotAccount, agent.ID)
	}

	gotSession, err := s.SessionForAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SessionForAccount: %v", err)
	}
	if gotSession != "sess-1" {
		t.Fatalf("SessionForAccount = %q, want the bound session %q", gotSession, "sess-1")
	}
}

// TestSessionBindingLookupsFailClosed is the security-relevant assertion. Both
// reads resolve the scope a call runs under, so an unbound session (and an
// account with no live session) MUST be ErrNotFound — never a zero-value
// AccountID or session id with a nil error, which the caller would treat as a
// real principal and dispatch to.
func TestSessionBindingLookupsFailClosed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	account, err := s.ResolveSessionAccount(ctx, "never-bound")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveSessionAccount(never-bound) err = %v, want errors.Is(_, ErrNotFound)", err)
	}
	if account != "" {
		t.Fatalf("ResolveSessionAccount(never-bound) = %q, want the empty AccountID — a miss must resolve no principal", account)
	}

	// The reverse direction fails closed the same way: a known agent that never
	// started a session resolves nothing to dispatch to.
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	session, err := s.SessionForAccount(ctx, agent.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionForAccount(unbound agent) err = %v, want errors.Is(_, ErrNotFound)", err)
	}
	if session != "" {
		t.Fatalf("SessionForAccount(unbound agent) = %q, want the empty session id", session)
	}
}

// TestRecordSessionBindingRePointsAccountAndReportsDisplaced is the reason the
// table is keyed on the account. The hub promotes a NEW session onto an account
// while the old one is still bound (relay_comms_test.go's repoint case) and
// promoteSession returns nothing, so this MUST NOT be a conflict: it is one
// upsert that moves the account onto the new session and hands back the old
// session id for the caller to reap from the held-deliver registry.
//
// Both halves of the SET list are asserted here — the new session AND the new
// runner — so dropping either assignment fails: session_id via SessionForAccount
// and the reap value, runner_id via which Runner's sweep finds the binding.
func TestRecordSessionBindingRePointsAccountAndReportsDisplaced(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	mustBind(t, ctx, s, "sess-old", agent.ID, "runner-1")

	displaced, err := s.RecordSessionBinding(ctx, "sess-new", agent.ID, "runner-2")
	if err != nil {
		t.Fatalf("re-point onto sess-new: %v (a re-point must never be refused — promoteSession has nowhere to put an error)", err)
	}
	if displaced != "sess-old" {
		t.Fatalf("re-point displaced %q, want %q — the caller reaps this session from the held-deliver registry, so a wrong value strands or wrongly clears deliveries", displaced, "sess-old")
	}

	// The account now resolves to the NEW session (the session_id assignment).
	gotSession, err := s.SessionForAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SessionForAccount after the re-point: %v", err)
	}
	if gotSession != "sess-new" {
		t.Fatalf("the agent resolves to session %q, want the re-pointed %q", gotSession, "sess-new")
	}

	// Exactly one row: a re-point REPLACES, it does not accumulate a second
	// binding whose sweep would retire a session that has already moved.
	if n := countBindings(t, ctx, s, agent.ID); n != 1 {
		t.Fatalf("bindings for the agent = %d, want exactly 1 (a re-point must replace, not accumulate)", n)
	}

	// The displaced session id no longer resolves: it named the same row, which
	// now carries the new session.
	if _, err := s.ResolveSessionAccount(ctx, "sess-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveSessionAccount(sess-old) err = %v, want ErrNotFound — the displaced session must stop resolving", err)
	}

	// The runner_id assignment: the binding moved to runner-2, so the OLD
	// Runner's sweep finds nothing and the new one finds it.
	stale, err := s.DeleteSessionBindingsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner(runner-1): %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("runner-1 still holds %+v after the re-point, want none — runner_id was not updated with session_id", stale)
	}
	swept, err := s.DeleteSessionBindingsForRunner(ctx, "runner-2")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner(runner-2): %v", err)
	}
	if len(swept) != 1 || swept[0].SessionID != "sess-new" {
		t.Fatalf("runner-2 sweep = %+v, want exactly the sess-new binding", swept)
	}
}

// TestRecordSessionBindingRebindsSameSessionOntoANewRunner covers the other
// re-point axis: the SAME session moving Runners (a session id is stable across
// a re-attach). It must update runner_id in place rather than adding a row, and
// it displaces itself — the account's previous session IS this session.
func TestRecordSessionBindingRebindsSameSessionOntoANewRunner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	mustBind(t, ctx, s, "sess-1", agent.ID, "runner-1")
	displaced := mustBind(t, ctx, s, "sess-1", agent.ID, "runner-2")
	if displaced != "sess-1" {
		t.Fatalf("re-binding the same session displaced %q, want %q — the account's previous session is this same session", displaced, "sess-1")
	}

	if n := countBindings(t, ctx, s, agent.ID); n != 1 {
		t.Fatalf("bindings for the agent = %d, want exactly 1 (the rebind must replace, not accumulate)", n)
	}

	// The old Runner no longer holds it: a sweep on runner-1 must retire nothing.
	swept, err := s.DeleteSessionBindingsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner(runner-1): %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("runner-1 still holds %+v after the session moved, want no bindings", swept)
	}
}

// TestRecordSessionBindingRejectsASessionClaimedByAnotherAccount proves the
// UNIQUE index on (tenant_id, session_id) — the one conflict this table still
// has, now that the account path is an upsert. Two accounts sharing a live
// session id would make ResolveSessionAccount's answer depend on which row
// Postgres returned, and that read resolves the PRINCIPAL a comms call runs
// under. The index refuses the write as ErrConflict rather than letting the
// ambiguity land.
func TestRecordSessionBindingRejectsASessionClaimedByAnotherAccount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agentA := mustAgent(t, s, owner.ID, "agent-a")
	agentB := mustAgent(t, s, owner.ID, "agent-b")

	mustBind(t, ctx, s, "sess-1", agentA.ID, "runner-1")

	displaced, err := s.RecordSessionBinding(ctx, "sess-1", agentB.ID, "runner-1")
	sentinelIs(t, err, ErrConflict, "a session id already bound to a different agent")
	if displaced != "" {
		t.Fatalf("the refused bind returned displaced = %q, want \"\" — a failed write displaced nothing", displaced)
	}

	// The refused write changed nothing: the session still speaks for agent A,
	// and agent B still has no live session.
	gotAccount, err := s.ResolveSessionAccount(ctx, "sess-1")
	if err != nil {
		t.Fatalf("ResolveSessionAccount after the refused bind: %v", err)
	}
	if gotAccount != agentA.ID {
		t.Fatalf("sess-1 resolves to %q, want the original owner %q", gotAccount, agentA.ID)
	}
	if _, err := s.SessionForAccount(ctx, agentB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionForAccount(agent-b) err = %v, want ErrNotFound — the refused bind must not have landed", err)
	}
}

// TestRecordSessionBindingUnknownAgentIsInvalidArgument pins the FK: a binding
// for an account that is not an agent cannot land. Without it a session could
// resolve to a nonexistent principal, and every authz check downstream would be
// evaluating an account that does not exist.
func TestRecordSessionBindingUnknownAgentIsInvalidArgument(t *testing.T) {
	s := newTestStore(t)

	_, err := s.RecordSessionBinding(context.Background(), "sess-1", "no-such-agent", "runner-1")
	sentinelIs(t, err, ErrInvalidArgument, "binding for an unknown agent")
}

// TestDeleteSessionBindingReleasesAndIsIdempotent covers the single-session
// release path: the delete removes the row (both directions stop resolving), and
// a SECOND delete succeeds. Idempotency is load-bearing — a session teardown may
// be retried, and an error on zero rows would strand the retried teardown
// mid-way.
func TestDeleteSessionBindingReleasesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	mustBind(t, ctx, s, "sess-1", agent.ID, "runner-1")
	if err := s.DeleteSessionBinding(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSessionBinding: %v", err)
	}

	if _, err := s.ResolveSessionAccount(ctx, "sess-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveSessionAccount after delete err = %v, want ErrNotFound", err)
	}
	if _, err := s.SessionForAccount(ctx, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionForAccount after delete err = %v, want ErrNotFound", err)
	}

	// The released account is bindable again, and displaces nothing — the row is
	// gone, so this is a fresh insert rather than a re-point.
	if displaced := mustBind(t, ctx, s, "sess-2", agent.ID, "runner-1"); displaced != "" {
		t.Fatalf("binding after a release displaced %q, want \"\" — the row was deleted, so there is nothing to reap", displaced)
	}

	// A second delete of an already-released session is a no-op, and so is
	// deleting one that never existed.
	if err := s.DeleteSessionBinding(ctx, "sess-1"); err != nil {
		t.Fatalf("second DeleteSessionBinding(sess-1) = %v, want nil (idempotent)", err)
	}
	if err := s.DeleteSessionBinding(ctx, "never-bound"); err != nil {
		t.Fatalf("DeleteSessionBinding(never-bound) = %v, want nil (idempotent)", err)
	}
}

// TestDeleteSessionBindingsForRunnerReturnsEverySweptBinding is the reconnect
// sweep itself, and the test that would catch a `:exec` regression. The returned
// rows are NOT diagnostic: each drives a presence DISCONNECTED edge for its
// account (RIG-1569 T8) and each session id must be reaped from the delivery
// held-deliver registry (RIG-1569 T3). A bare DELETE would clear the bindings —
// satisfying the invariant — while silently dropping both side-effects, leaving
// a long-WORKING agent stuck WORKING in the projection forever.
//
// It must also sweep ONLY the re-enrolling Runner's bindings: too few leaves a
// stale session resolving a re-minted id to the wrong account; too many drops
// live sessions on a healthy Runner.
//
// The seed order is REVERSE-SORTED on purpose (sess-z, then sess-b, then
// sess-a). DELETE ... RETURNING emits physical heap order, which for freshly
// inserted rows is insertion order — so seeding in sorted order would make the
// sort assertion below pass with slices.SortFunc removed. Seeding backwards is
// what makes the sort load-bearing.
func TestDeleteSessionBindingsForRunnerReturnsEverySweptBinding(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "agent-a")
	b := mustAgent(t, s, owner.ID, "agent-b")
	z := mustAgent(t, s, owner.ID, "agent-z")
	elsewhere := mustAgent(t, s, owner.ID, "agent-elsewhere")

	for _, bind := range []SessionBinding{
		{SessionID: "sess-z", AccountID: z.ID, RunnerID: "runner-1"},
		{SessionID: "sess-b", AccountID: b.ID, RunnerID: "runner-1"},
		{SessionID: "sess-a", AccountID: a.ID, RunnerID: "runner-1"},
		{SessionID: "sess-elsewhere", AccountID: elsewhere.ID, RunnerID: "runner-2"},
	} {
		mustBind(t, ctx, s, bind.SessionID, bind.AccountID, bind.RunnerID)
	}

	swept, err := s.DeleteSessionBindingsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner: %v", err)
	}

	// The returned rows ARE the side-effect inputs, so assert them exactly —
	// every swept binding, with the account each DISCONNECTED edge needs, in
	// sorted order (which is NOT the order they were seeded in).
	want := []SessionBinding{
		{SessionID: "sess-a", AccountID: a.ID, RunnerID: "runner-1"},
		{SessionID: "sess-b", AccountID: b.ID, RunnerID: "runner-1"},
		{SessionID: "sess-z", AccountID: z.ID, RunnerID: "runner-1"},
	}
	if len(swept) != len(want) {
		t.Fatalf("swept = %+v, want exactly the %d bindings on runner-1 (a :exec sweep would return none)", swept, len(want))
	}
	for i, w := range want {
		if swept[i] != w {
			t.Fatalf("swept[%d] = %+v, want %+v (sorted by session id; the rows were seeded in reverse order, so an unsorted sweep returns them backwards)", i, swept[i], w)
		}
	}

	// The swept bindings are actually gone in both directions.
	for _, sessionID := range []string{"sess-a", "sess-b", "sess-z"} {
		if _, err := s.ResolveSessionAccount(ctx, sessionID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ResolveSessionAccount(%s) after the sweep err = %v, want ErrNotFound", sessionID, err)
		}
	}

	// The other Runner's binding SURVIVES: a re-enroll must not drop live
	// sessions on a Runner that never reconnected.
	gotAccount, err := s.ResolveSessionAccount(ctx, "sess-elsewhere")
	if err != nil {
		t.Fatalf("runner-2's binding did not survive the runner-1 sweep: %v", err)
	}
	if gotAccount != elsewhere.ID {
		t.Fatalf("sess-elsewhere resolves to %q, want %q", gotAccount, elsewhere.ID)
	}

	// A Runner holding no bindings sweeps nothing quietly — a first-ever enroll.
	empty, err := s.DeleteSessionBindingsForRunner(ctx, "runner-never-seen")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner(unknown) = %v, want nil (nothing to sweep is not a failure)", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown runner sweep = %+v, want empty", empty)
	}
}

// TestRecordSessionBindingTriggerAdvancesUpdatedAtOnly proves the RIG-3495
// trigger fires through a real store method: the ON CONFLICT re-point advances
// updated_at while created_at stays put. It is also the only proof the column is
// live at all — secrets.updated_at rotted precisely because no write statement
// set it, so the value could only ever equal created_at and every reader was
// reading a lie.
//
// No time.Sleep: now() is TRANSACTION time in Postgres, so the read-then-rebind
// below spans two transactions and the two values differ on their own.
func TestRecordSessionBindingTriggerAdvancesUpdatedAtOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	mustBind(t, ctx, s, "sess-1", agent.ID, "runner-1")
	createdBefore, updatedBefore := bindingTimes(t, ctx, s, "sess-1")
	if !createdBefore.Equal(updatedBefore) {
		t.Fatalf("on INSERT created_at = %v and updated_at = %v, want the same DEFAULT now()", createdBefore, updatedBefore)
	}

	mustBind(t, ctx, s, "sess-1", agent.ID, "runner-2")
	createdAfter, updatedAfter := bindingTimes(t, ctx, s, "sess-1")

	if !createdAfter.Equal(createdBefore) {
		t.Fatalf("created_at moved from %v to %v across a rebind; it must record the row's birth", createdBefore, createdAfter)
	}
	if !updatedAfter.After(updatedBefore) {
		t.Fatalf("updated_at = %v after the rebind, want later than %v — the set_updated_at trigger did not fire", updatedAfter, updatedBefore)
	}
}

// TestSessionBindingIsTenantIsolated is the RLS proof: a binding written under
// one tenant is invisible to another, and the isolation is the database policy
// rather than an application-layer WHERE — both rows live in the same physical
// table and only the per-transaction compass.tenant_id GUC differs. It matters
// more here than for most tables: these reads resolve the PRINCIPAL a comms call
// runs under, so a cross-tenant hit would run a foreign tenant's session under a
// local account.
//
// Follows the established pattern in rls_pgtest_test.go — seedTenant for a
// second tenant, WithTenant for its context, every write through the normal
// store API so rows land stamped exactly as a real request would.
func TestSessionBindingIsTenantIsolated(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background() // no tenant set → the bootstrap tenant
	ctxB := WithTenant(context.Background(), tenantB)

	ownerA := mustUser(t, s, "owner-a")
	agentA := mustAgent(t, s, ownerA.ID, "agent-a")
	mustBind(t, ctxA, s, "sess-a", agentA.ID, "runner-1")

	// Tenant B resolves A's session id: the row is not in B's view, so this must
	// fail closed rather than hand B tenant A's account.
	gotAccount, err := s.ResolveSessionAccount(ctxB, "sess-a")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B ResolveSessionAccount(sess-a) err = %v, want ErrNotFound — cross-tenant read leak", err)
	}
	if gotAccount != "" {
		t.Fatalf("tenant B resolved tenant A's account %q from A's session — cross-tenant read leak", gotAccount)
	}

	// The reverse direction leaks nothing either.
	if gotSession, err := s.SessionForAccount(ctxB, agentA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B SessionForAccount(A's agent) = (%q, %v), want ErrNotFound — cross-tenant read leak", gotSession, err)
	}

	// Nor does B's sweep of the SAME runner id retire A's binding — a shared
	// runner id across tenants must not let one tenant drop another's sessions.
	swept, err := s.DeleteSessionBindingsForRunner(ctxB, "runner-1")
	if err != nil {
		t.Fatalf("tenant B sweep of runner-1: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("tenant B's sweep returned %+v, want empty — it reached tenant A's bindings", swept)
	}

	// Control: tenant A still sees its OWN binding, proving the policy is not
	// simply hiding everything.
	if gotAccount, err := s.ResolveSessionAccount(ctxA, "sess-a"); err != nil {
		t.Fatalf("tenant A cannot see its OWN binding — policy over-blocks: %v", err)
	} else if gotAccount != agentA.ID {
		t.Fatalf("tenant A's own binding resolves to %q, want %q", gotAccount, agentA.ID)
	}
}

// seedTenantAgent creates an owner user and an owned agent UNDER ctx's tenant,
// so a multi-tenant case can WRITE as a second tenant rather than only read as
// one. mustUser/mustAgent are hard-wired to context.Background() (the bootstrap
// tenant), which is precisely why every pre-existing multi-tenant assertion here
// could only ever write under tenant A.
func seedTenantAgent(t *testing.T, ctx context.Context, s *Store, handle string) Account {
	t.Helper()
	owner, err := s.CreateUser(ctx, NewUser{Handle: handle + "-owner", DisplayName: handle + "-owner"})
	if err != nil {
		t.Fatalf("CreateUser(%s-owner): %v", handle, err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%s): %v", handle, err)
	}
	return agent
}

// TestSessionBindingSameSessionIDInTwoTenantsCoexist pins the SESSION half of the
// tenant fold: session_bindings_session_key is (tenant_id, session_id), not
// (session_id). Two tenants mint session ids independently — nothing coordinates
// them — so a collision between them is routine, and a global unique index would
// refuse tenant B's perfectly valid bind because tenant A happened to pick the
// same string first. That is a cross-tenant denial of service through an id
// namespace neither tenant can see.
//
// This is the case the round-1 fold had NO test for. TestSessionBindingIsTenantIsolated
// only ever WRITES under tenant A; tenant B appears solely in reads and a
// returns-nothing sweep, so reverting the index to a global UNIQUE (session_id)
// left every one of its assertions passing. This one WRITES under tenant B and
// fails on that mutation with the ErrConflict the un-folded index raises.
func TestSessionBindingSameSessionIDInTwoTenantsCoexist(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background() // no tenant set → the bootstrap tenant
	ctxB := WithTenant(context.Background(), tenantB)

	ownerA := mustUser(t, s, "owner-a")
	agentA := mustAgent(t, s, ownerA.ID, "agent-a")
	agentB := seedTenantAgent(t, ctxB, s, "agent-b")

	mustBind(t, ctxA, s, "sess-shared", agentA.ID, "runner-a")

	// The load-bearing write: tenant B claims the SAME session id. It must
	// SUCCEED — under a global unique index this is ErrConflict.
	if displaced := mustBind(t, ctxB, s, "sess-shared", agentB.ID, "runner-b"); displaced != "" {
		t.Fatalf("tenant B's first bind displaced %q, want \"\" — B's agent held no prior session, and a cross-tenant displaced id would mean B just overwrote A's row", displaced)
	}

	// Each tenant resolves its OWN account from the shared session id. A single
	// surviving row would make one of these two answer with the other tenant's
	// account — the relay resolving a foreign principal.
	gotA, err := s.ResolveSessionAccount(ctxA, "sess-shared")
	if err != nil {
		t.Fatalf("tenant A ResolveSessionAccount(sess-shared) after B's bind: %v (B's write clobbered A's binding)", err)
	}
	if gotA != agentA.ID {
		t.Fatalf("tenant A resolves sess-shared to %q, want its own agent %q", gotA, agentA.ID)
	}
	gotB, err := s.ResolveSessionAccount(ctxB, "sess-shared")
	if err != nil {
		t.Fatalf("tenant B ResolveSessionAccount(sess-shared): %v", err)
	}
	if gotB != agentB.ID {
		t.Fatalf("tenant B resolves sess-shared to %q, want its own agent %q", gotB, agentB.ID)
	}

	// And the reverse direction, per tenant.
	if got, err := s.SessionForAccount(ctxA, agentA.ID); err != nil || got != "sess-shared" {
		t.Fatalf("tenant A SessionForAccount = (%q, %v), want (sess-shared, nil)", got, err)
	}
	if got, err := s.SessionForAccount(ctxB, agentB.ID); err != nil || got != "sess-shared" {
		t.Fatalf("tenant B SessionForAccount = (%q, %v), want (sess-shared, nil)", got, err)
	}
}

// TestSessionBindingSameAccountIDInTwoTenantsCoexist pins the PRIMARY KEY half of
// the fold: PRIMARY KEY (tenant_id, agent_account_id), not (agent_account_id).
// Two rows for one account id, one per tenant, must COEXIST — under an un-folded
// PK the second bind is not a conflict but something worse: it resolves as
// ON CONFLICT DO UPDATE and OVERWRITES the other tenant's binding, silently
// moving a foreign tenant's account onto this tenant's session.
//
// accounts.id is a GLOBAL primary key, so the two tenants cannot own two distinct
// agent rows sharing an id; the second tenant necessarily binds the first
// tenant's account id. The FK to agent_accounts permits it — referential-integrity
// checks are not RLS-constrained — and that is the point: the DEFENCE against one
// tenant's write reaching another's binding is the tenant in the KEY, not the FK.
func TestSessionBindingSameAccountIDInTwoTenantsCoexist(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background() // no tenant set → the bootstrap tenant
	ctxB := WithTenant(context.Background(), tenantB)

	ownerA := mustUser(t, s, "owner-a")
	shared := mustAgent(t, s, ownerA.ID, "agent-shared")

	mustBind(t, ctxA, s, "sess-a", shared.ID, "runner-a")

	// The load-bearing write: tenant B binds the SAME account id to its own
	// session. Under PRIMARY KEY (agent_account_id) this upserts over A's row.
	if displaced := mustBind(t, ctxB, s, "sess-b", shared.ID, "runner-b"); displaced != "" {
		t.Fatalf("tenant B's bind of the shared account id displaced %q, want \"\" — a non-empty value means it read (and overwrote) tenant A's row", displaced)
	}

	// Both rows survive, one per tenant, each resolving its own session.
	if got, err := s.SessionForAccount(ctxA, shared.ID); err != nil || got != "sess-a" {
		t.Fatalf("tenant A SessionForAccount = (%q, %v), want (sess-a, nil) — B's write reached A's row", got, err)
	}
	if got, err := s.SessionForAccount(ctxB, shared.ID); err != nil || got != "sess-b" {
		t.Fatalf("tenant B SessionForAccount = (%q, %v), want (sess-b, nil)", got, err)
	}

	// Exactly one row PER TENANT — countBindings carries the tenant predicate,
	// so this is 1 and 1, not a cross-tenant 2.
	if n := countBindings(t, ctxA, s, shared.ID); n != 1 {
		t.Fatalf("tenant A holds %d bindings for the shared account, want 1", n)
	}
	if n := countBindings(t, ctxB, s, shared.ID); n != 1 {
		t.Fatalf("tenant B holds %d bindings for the shared account, want 1", n)
	}

	// Each tenant's session resolves only in its own tenant.
	if _, err := s.ResolveSessionAccount(ctxA, "sess-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant A resolved B's session err = %v, want ErrNotFound", err)
	}
	if _, err := s.ResolveSessionAccount(ctxB, "sess-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B resolved A's session err = %v, want ErrNotFound", err)
	}
}

// TestSessionForAccountUnderSystemRoleIsUnscoped PINS A HAZARD rather than a
// desired property, and the assertions are written to say so.
//
// WithSystemRole arms the BYPASSRLS compass_system role with no tenant GUC
// (tenant_tx.go). SessionBindingForAccount is :one and its only predicate is the
// account id — single-valued ONLY because RLS normally narrows the visible rows
// to one tenant's. Strip that and two tenants' rows for one account id both
// match; pgx's QueryRow takes the first and discards the rest WITHOUT error, so
// the caller gets a plausible session id from an arbitrary tenant.
//
// Nothing calls this under the system role today — WithSystemRole is set at
// delivery/consumer.go:316 and runnerhub/hub.go:753,:814, i.e. on PR3's path — so
// this test exists so the behaviour is RECORDED, not discovered later. Deciding
// what to do about it (a tenant predicate, a :many + explicit refusal, a
// structural guard) is PR3's call, and this test is what will fail loudly when
// PR3 changes it.
func TestSessionForAccountUnderSystemRoleIsUnscoped(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background() // no tenant set → the bootstrap tenant
	ctxB := WithTenant(context.Background(), tenantB)

	ownerA := mustUser(t, s, "owner-a")
	shared := mustAgent(t, s, ownerA.ID, "agent-shared")
	mustBind(t, ctxA, s, "sess-a", shared.ID, "runner-a")
	mustBind(t, ctxB, s, "sess-b", shared.ID, "runner-b")

	// Under the system role BOTH rows are visible, so the :one read is ambiguous.
	// It does NOT error — that is the hazard. It returns one of the two, and
	// which one is not something the caller can control or detect.
	got, err := s.SessionForAccount(WithSystemRole(context.Background()), shared.ID)
	if err != nil {
		t.Fatalf("SessionForAccount under the system role: %v — the current behaviour is a SILENT pick, not an error; if this now errors, PR3 changed the contract and this test must be updated deliberately", err)
	}
	if got != "sess-a" && got != "sess-b" {
		t.Fatalf("SessionForAccount under the system role = %q, want one of the two tenants' sessions", got)
	}
	t.Logf("system-role SessionForAccount(%q) returned %q with two tenants holding that account id — unscoped and silently single-valued", shared.ID, got)

	// The same read on the REQUEST path is exact in both tenants: the hazard is
	// the system role's missing scoping, NOT anything about the data.
	if v, err := s.SessionForAccount(ctxA, shared.ID); err != nil || v != "sess-a" {
		t.Fatalf("tenant A request-path SessionForAccount = (%q, %v), want (sess-a, nil)", v, err)
	}
	if v, err := s.SessionForAccount(ctxB, shared.ID); err != nil || v != "sess-b" {
		t.Fatalf("tenant B request-path SessionForAccount = (%q, %v), want (sess-b, nil)", v, err)
	}

	// A system-role WRITE is worse than a refusal: it SUCCEEDS and lands an
	// ORPHAN. tenant_id DEFAULTs to current_setting('compass.tenant_id', TRUE),
	// and under this role no GUC is armed — but the pooled connection has served
	// armed request-path statements before, and a SET LOCAL that has ended leaves
	// the custom GUC defined-and-EMPTY on that backend rather than undefined. So
	// the DEFAULT resolves to '' instead of NULL, the NOT NULL is satisfied, and
	// the row lands stamped with a tenant that does not exist.
	//
	// session_bindings.tenant_id has no FK to tenants (only accounts.tenant_id
	// does), so nothing catches it. The row is then invisible to EVERY tenant —
	// no RLS policy matches '' — and reachable only by another system-role read
	// or the owner pool: an unswept binding no request path can see or release.
	//
	// It is also NOT deterministic. On a backend that never carried an armed
	// statement the GUC is genuinely undefined, the DEFAULT is NULL, and the
	// insert fails not-null instead. Which one a caller gets depends on the
	// pooled connection it draws, so this asserts the DISJUNCTION rather than
	// pretending either branch is the contract. Both are defects; PR3 owns the
	// fix, and this test is what will fail when it lands one.
	sysDisplaced, sysErr := s.RecordSessionBinding(WithSystemRole(context.Background()), "sess-sys", shared.ID, "runner-sys")
	switch {
	case sysErr != nil:
		t.Logf("system-role RecordSessionBinding failed (connection had no prior armed statement, so the GUC was undefined and the DEFAULT was NULL): %v", sysErr)
	default:
		t.Logf("system-role RecordSessionBinding SUCCEEDED, displaced=%q — it landed an untenanted row", sysDisplaced)
		var orphans int
		if err := s.pool.QueryRow(ctxA,
			"SELECT count(*) FROM session_bindings WHERE session_id = $1 AND tenant_id = ''", "sess-sys",
		).Scan(&orphans); err != nil {
			t.Fatalf("count untenanted bindings: %v", err)
		}
		if orphans != 1 {
			t.Fatalf("system-role write left %d rows stamped tenant_id = '', want 1 — the observed failure mode is an ORPHAN row, so if it is now stamped with a real tenant the write path grew scoping and this test must be updated deliberately", orphans)
		}
		// And it is invisible to every tenant: no policy matches ''.
		if _, err := s.ResolveSessionAccount(ctxA, "sess-sys"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tenant A sees the untenanted binding err = %v, want ErrNotFound", err)
		}
		if _, err := s.ResolveSessionAccount(ctxB, "sess-sys"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tenant B sees the untenanted binding err = %v, want ErrNotFound", err)
		}
	}
}

// TestRecordSessionBindingConcurrentRePointsReportDistinctDisplaced is the
// regression test for the round-2 HIGH defect, and it drives the interleaving
// DETERMINISTICALLY rather than hoping a goroutine race lands on it.
//
// The setup: account X is bound to sess-A, then two callers re-point it — one to
// sess-B, one to sess-C. Exactly two sessions get displaced: sess-A by whichever
// caller wins, and the WINNER'S session by the loser. Each caller must be told
// the one IT displaced, because that id is what it reaps from the delivery
// held-deliver registry, and a session reported to nobody keeps its held
// deliveries forever.
//
// The determinism comes from a THIRD connection holding the SAME per-account
// advisory lock the bind takes first. Both callers park on it, so both are
// provably in flight before either can proceed, and releasing it starts the real
// contention. Without that the two calls would usually just serialize and the
// test would pass on a schedule that never exercised the bug.
//
// The defect this catches: with an unlocked prior-value read (the `prev` CTE this
// replaced), the second caller's read takes the statement-start snapshot while
// its write blocks on the first's row lock and then re-reads the latest committed
// row — so both callers report sess-A and the middle session is destroyed
// unreported. Removing either lock from the bind fails this test.
func TestRecordSessionBindingConcurrentRePointsReportDistinctDisplaced(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	mustBind(t, ctx, s, "sess-A", agent.ID, "runner-0")

	// A gate transaction holding the bind's per-account advisory lock, armed
	// exactly as the store's own transactions are (SET LOCAL ROLE + the tenant
	// GUC). The lock key is character-identical to the one in
	// queries/session_bindings.sql — if that key changes, this gate stops
	// blocking and the test fails loudly rather than silently going green.
	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() { _ = gate.Rollback(ctx) }() // no-op once released below
	if _, err := gate.Exec(ctx, "SET LOCAL ROLE "+appRole); err != nil {
		t.Fatalf("gate arm role: %v", err)
	}
	if _, err := gate.Exec(ctx, "SELECT set_config($1, $2, true)", tenantGUC, string(s.resolveTenant(ctx))); err != nil {
		t.Fatalf("gate arm guc: %v", err)
	}
	if _, err := gate.Exec(ctx,
		"SELECT pg_advisory_xact_lock(hashtext('binding:' || $1 || ':' || $2))",
		string(s.resolveTenant(ctx)), string(agent.ID),
	); err != nil {
		t.Fatalf("gate advisory lock: %v", err)
	}

	type outcome struct {
		session   string
		displaced string
		err       error
	}
	results := make(chan outcome, 2)
	launch := func(sessionID, runnerID string) {
		go func() {
			d, err := s.RecordSessionBinding(ctx, sessionID, agent.ID, runnerID)
			results <- outcome{sessionID, d, err}
		}()
	}
	launch("sess-B", "runner-1")
	launch("sess-C", "runner-2")

	// Both callers must be BLOCKED on the gate's lock. If either completes now,
	// the bind is not serializing per account and displaced values cannot be
	// correct under concurrency.
	select {
	case r := <-results:
		t.Fatalf("a re-point completed (session=%q displaced=%q err=%v) while the gate held the per-account advisory lock — the bind is not taking it, so two concurrent binds can read the same prior value", r.session, r.displaced, r.err)
	case <-time.After(750 * time.Millisecond):
	}

	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	got := make(map[string]string, 2)
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent re-point failed: %v (a re-point must never be refused)", r.err)
		}
		got[r.session] = r.displaced
	}

	// Which caller wins the lock is not determined, so the assertion is stated
	// on the SHAPE, not on a fixed pairing: the winner displaces sess-A (the
	// seeded binding), and the loser displaces THE WINNER'S session, because
	// that is what it actually overwrote. So {sess-A, <winner's session>}, one
	// each. Both callers reporting the SAME id is the unlocked-read defect.
	b, c := got["sess-B"], got["sess-C"]
	var winner, loser string
	switch {
	case b == "sess-A" && c == "sess-B":
		winner, loser = "sess-B", "sess-C"
	case c == "sess-A" && b == "sess-C":
		winner, loser = "sess-C", "sess-B"
	default:
		t.Fatalf("the two concurrent re-points reported displaced sess-B=%q sess-C=%q; want one of them naming sess-A and the OTHER naming the first one's session. Every displaced session must be reported to exactly one caller or its held deliveries are stranded forever; both callers naming the same id is the unlocked-read defect (the prior value must be read under a lock inside the write's transaction)", b, c)
	}
	t.Logf("caller %s won the lock (displaced sess-A); caller %s displaced %s", winner, loser, winner)

	// One row survives, holding the LOSER's session — it committed last.
	if n := countBindings(t, ctx, s, agent.ID); n != 1 {
		t.Fatalf("bindings after two concurrent re-points = %d, want 1", n)
	}
	live, err := s.SessionForAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SessionForAccount after the race: %v", err)
	}
	if live != loser {
		t.Fatalf("live session is %q, want %q — the caller that committed last owns the binding", live, loser)
	}

	// Both displaced sessions are gone, and between them they were reported to
	// exactly the two callers: nothing was destroyed unreported.
	for _, dead := range []string{"sess-A", winner} {
		if _, err := s.ResolveSessionAccount(ctx, dead); !errors.Is(err, ErrNotFound) {
			t.Fatalf("displaced session %q still resolves (err = %v), want ErrNotFound", dead, err)
		}
	}
}

// TestRecordSessionBindingConcurrentFirstBindsReportTheDestroyedSession covers
// the case the ROW lock cannot cover, and it is why the bind takes a per-account
// advisory lock as well.
//
// SELECT ... FOR UPDATE on a row that does not yet exist locks NOTHING. So two
// first-ever binds for one account both read no prior value, and the row lock
// never brings them into contact. The PRIMARY KEY does serialize their WRITES —
// the second INSERT waits on the first's uncommitted tuple and resolves as
// ON CONFLICT DO UPDATE — but by then both have already READ, so with only those
// two mechanisms both callers report "displaced nothing" while the second has
// destroyed the first's live binding. That session is reported to NOBODY and its
// held deliveries are stranded: the same failure as the re-point case, reached
// by a different route. Measured, not assumed — the PK orders writes, and the
// displaced value is a read.
//
// The advisory lock is taken BEFORE the read, so it covers this: exactly one
// caller reports "" (it genuinely displaced nothing) and the other reports the
// first caller's session, which it really did overwrite and must reap.
//
// Gated the same way as the re-point case, on the same lock, so the interleaving
// is driven rather than hoped for. Removing the advisory lock from the bind makes
// this test fail with both callers reporting "".
func TestRecordSessionBindingConcurrentFirstBindsReportTheDestroyedSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// The gate: hold the bind's per-account advisory lock so both callers are
	// provably in flight before either reads. No binding row exists yet, which
	// is the entire point — there is nothing for FOR UPDATE to lock.
	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() { _ = gate.Rollback(ctx) }() // no-op once released below
	if _, err := gate.Exec(ctx, "SET LOCAL ROLE "+appRole); err != nil {
		t.Fatalf("gate arm role: %v", err)
	}
	if _, err := gate.Exec(ctx, "SELECT set_config($1, $2, true)", tenantGUC, string(s.resolveTenant(ctx))); err != nil {
		t.Fatalf("gate arm guc: %v", err)
	}
	if _, err := gate.Exec(ctx,
		"SELECT pg_advisory_xact_lock(hashtext('binding:' || $1 || ':' || $2))",
		string(s.resolveTenant(ctx)), string(agent.ID),
	); err != nil {
		t.Fatalf("gate advisory lock: %v", err)
	}

	type outcome struct {
		session   string
		displaced string
		err       error
	}
	results := make(chan outcome, 2)
	for _, b := range []struct{ session, runner string }{
		{"sess-1", "runner-1"},
		{"sess-2", "runner-2"},
	} {
		go func() {
			d, err := s.RecordSessionBinding(ctx, b.session, agent.ID, b.runner)
			results <- outcome{b.session, d, err}
		}()
	}

	select {
	case r := <-results:
		t.Fatalf("a first bind completed (session=%q displaced=%q err=%v) while the gate held the per-account advisory lock — with no row to lock, that lock is the ONLY thing serializing two first binds", r.session, r.displaced, r.err)
	case <-time.After(750 * time.Millisecond):
	}

	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	got := make(map[string]string, 2)
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent first bind of %q failed: %v — the PK conflict must resolve as an upsert, never surface to the caller", r.session, r.err)
		}
		got[r.session] = r.displaced
	}

	// Which caller wins is not determined, so accept either assignment and
	// reject the two broken shapes: both "" (a live session destroyed and
	// reported to nobody) and both non-empty (one session reaped twice).
	first, second := got["sess-1"], got["sess-2"]
	var winner string
	switch {
	case first == "" && second == "sess-1":
		winner = "sess-1"
	case second == "" && first == "sess-2":
		winner = "sess-2"
	default:
		t.Fatalf("concurrent first binds reported displaced sess-1=%q sess-2=%q; want exactly one \"\" and the other naming the session it overwrote. Both empty means a live session was destroyed and reported to NOBODY — the defect the per-account advisory lock exists to prevent, and one the PK cannot cover because it orders WRITES while the displaced value is a READ", first, second)
	}
	t.Logf("caller %s bound first; the other displaced it and can reap it", winner)

	if n := countBindings(t, ctx, s, agent.ID); n != 1 {
		t.Fatalf("bindings after two concurrent first binds = %d, want 1 — the PK must fold them into one row", n)
	}
	// The winner's session is gone, and it WAS reported, so it can be reaped.
	if _, err := s.ResolveSessionAccount(ctx, winner); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the overwritten session %q still resolves (err = %v), want ErrNotFound", winner, err)
	}
}

// TestRecordSessionBindingReportsDisplacedExactlyOnceAgainstARunnerSweep is what
// makes the FOR UPDATE row lock on the bind's prior-value read load-bearing, and
// it is the only case in this file that can be.
//
// The two concurrency tests above cannot reach that lock AT ALL. Both pit a bind
// against another BIND, and the bind takes the per-account advisory lock FIRST,
// before it reads — so the two callers are already serialized by the time either
// one evaluates SELECT ... FOR UPDATE. Remove FOR UPDATE and both still pass:
// the advisory lock alone is sufficient for bind-vs-bind. The row lock earns its
// place only against a writer that reaches the row WITHOUT holding the advisory
// lock, and DeleteSessionBindingsForRunner is exactly that writer — the reconnect
// sweep is a single :many DELETE and takes no advisory lock at all.
//
// So this pits a re-point against a sweep of the account's CURRENT Runner. Both
// destroy the same binding, and the displaced session id drives PR3's reap from
// the delivery held-deliver registry, so it must be handed to EXACTLY ONE of
// them:
//
//   - reported to BOTH is a double reap — the bind returns sess-old AND the
//     sweep returns sess-old, so PR3 reaps the same session twice.
//   - reported to NEITHER strands it — its held deliveries are kept forever,
//     the mirror of the defect the advisory lock covers.
//
// With the row lock, one side blocks on the other's uncommitted tuple and sees
// the committed truth: if the sweep commits first the row is gone, the bind's
// read misses, and it reports "" while the sweep reports sess-old; if the bind
// commits first it reports sess-old and the sweep — re-reading the latest
// committed row, which now names sess-new on runner-2 — matches nothing. Without
// it the bind's read runs on the statement-start snapshot and still sees
// sess-old while the sweep concurrently deletes and returns it, so both report.
//
// WHICH side wins is not asserted and must not be: the assertion is on the SHAPE
// (exactly one reporter), so it holds on every iteration regardless of schedule.
// No gate transaction drives the interleaving here — unlike the two tests above,
// the contended resource IS the row lock under test, so a gate holding it would
// beg the question. An unsynchronized loop is enough at this iteration count:
// measured 108/120 and 113/120 iterations reporting the double-reap with
// FOR UPDATE removed, and 0/120 with it present. A SINGLE iteration would be a
// ~90% test, which is why the count is 120 rather than one.
func TestRecordSessionBindingReportsDisplacedExactlyOnceAgainstARunnerSweep(t *testing.T) {
	// context.Background is the test root, the pgtest-suite convention.
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	const iterations = 120
	var doubleReported, unreported int
	for i := range iterations {
		old := fmt.Sprintf("sess-old-%d", i)
		fresh := fmt.Sprintf("sess-new-%d", i)

		// Fresh session ids every iteration, and the seed bind RE-POINTS the
		// account's one row back onto runner-1. That is what keeps each
		// iteration independent AND non-vacuous: the previous iteration leaves a
		// row on runner-2 (which the runner-1 sweep below would not see), so
		// without this re-point iteration i+1 would race a bind against a sweep
		// that had nothing to find and "exactly one reporter" would hold
		// trivially. After this call there is exactly one binding, it names
		// `old`, and it sits on the Runner the sweep targets.
		mustBind(t, ctx, s, old, agent.ID, "runner-1")

		type bindResult struct {
			displaced string
			err       error
		}
		type sweepResult struct {
			swept []SessionBinding
			err   error
		}
		binds := make(chan bindResult, 1)
		sweeps := make(chan sweepResult, 1)

		// Both goroutines park on `start` until each is scheduled and ready, so
		// they are released together rather than sequentially. This narrows the
		// window, it does not close it — the interleaving stays up to Postgres
		// and the scheduler, which is why the loop runs many iterations instead
		// of trusting one.
		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		go func() {
			ready.Done()
			<-start
			displaced, err := s.RecordSessionBinding(ctx, fresh, agent.ID, "runner-2")
			binds <- bindResult{displaced, err}
		}()
		go func() {
			ready.Done()
			<-start
			swept, err := s.DeleteSessionBindingsForRunner(ctx, "runner-1")
			sweeps <- sweepResult{swept, err}
		}()
		ready.Wait()
		close(start)

		bind, sweep := <-binds, <-sweeps
		if bind.err != nil {
			t.Fatalf("iteration %d: RecordSessionBinding(%q, runner-2): %v — a re-point must never be refused, whatever a concurrent sweep is doing", i, fresh, bind.err)
		}
		if sweep.err != nil {
			t.Fatalf("iteration %d: DeleteSessionBindingsForRunner(runner-1): %v", i, sweep.err)
		}

		// The bind may only ever name the session it actually displaced or ""
		// (the sweep beat it to the row). Anything else is a wrong id handed to
		// the reap, which is worse than either failure counted below.
		if bind.displaced != old && bind.displaced != "" {
			t.Fatalf("iteration %d: the bind reported displaced %q; the only correct answers are %q (it overwrote the seeded binding) or \"\" (the sweep removed it first)", i, bind.displaced, old)
		}
		// Likewise the sweep: the account holds one binding and it is the only
		// row on runner-1, so the sweep returns that row or nothing.
		if len(sweep.swept) > 1 {
			t.Fatalf("iteration %d: the runner-1 sweep returned %d bindings, want at most 1 — the account holds a single binding", i, len(sweep.swept))
		}
		for _, b := range sweep.swept {
			if b.SessionID != old {
				t.Fatalf("iteration %d: the runner-1 sweep returned session %q, want %q", i, b.SessionID, old)
			}
		}

		reportedByBind := bind.displaced == old
		reportedBySweep := len(sweep.swept) == 1
		switch {
		case reportedByBind && reportedBySweep:
			doubleReported++
		case !reportedByBind && !reportedBySweep:
			unreported++
		}

		// Whoever won, the account ends the iteration holding exactly the new
		// binding. This is the per-iteration proof the race actually ran: a
		// no-op iteration could not move the account onto `fresh`.
		if n := countBindings(t, ctx, s, agent.ID); n != 1 {
			t.Fatalf("iteration %d: bindings after the race = %d, want 1 — the bind must land whether or not the sweep removed the prior row", i, n)
		}
		live, err := s.SessionForAccount(ctx, agent.ID)
		if err != nil {
			t.Fatalf("iteration %d: SessionForAccount after the race: %v", i, err)
		}
		if live != fresh {
			t.Fatalf("iteration %d: live session is %q, want %q — the bind commits last in both orderings, so its session owns the binding", i, live, fresh)
		}
		// And the displaced session is gone in every ordering, which is what
		// makes "reported to nobody" a genuine strand rather than a deferral.
		if _, err := s.ResolveSessionAccount(ctx, old); !errors.Is(err, ErrNotFound) {
			t.Fatalf("iteration %d: displaced session %q still resolves (err = %v), want ErrNotFound", i, old, err)
		}
	}

	if doubleReported > 0 || unreported > 0 {
		t.Fatalf("over %d iterations of a re-point racing a sweep of the account's Runner: %d reported the displaced session to BOTH callers (a double reap — PR3 reaps it from the held-deliver registry twice) and %d reported it to NEITHER (stranded held deliveries). Every displaced session must be reported to exactly one caller. The prior-value read must take a ROW LOCK (SELECT ... FOR UPDATE in queries/session_bindings.sql): the per-account advisory lock cannot cover this, because the sweep is a bare DELETE that never takes it",
			iterations, doubleReported, unreported)
	}
	t.Logf("%d iterations: the displaced session was reported to exactly one caller every time", iterations)
}
