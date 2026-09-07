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
	"testing"
	"time"
)

// mustBind records a binding a test only needs to SUCCEED, returning the session
// id it displaced. Most cases below care about a later assertion, not this call.
func mustBind(t *testing.T, s *Store, ctx context.Context, sessionID string, accountID AccountID, runnerID string) string {
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
func bindingTimes(t *testing.T, s *Store, sessionID string) (createdAt, updatedAt time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		"SELECT created_at, updated_at FROM session_bindings WHERE session_id = $1", sessionID,
	).Scan(&createdAt, &updatedAt); err != nil {
		t.Fatalf("read timestamps of binding %q: %v", sessionID, err)
	}
	return createdAt, updatedAt
}

// countBindings counts binding rows for an account, so a re-point test can prove
// the row was REPLACED rather than accumulated.
func countBindings(t *testing.T, s *Store, accountID AccountID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM session_bindings WHERE agent_account_id = $1", string(accountID),
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

	mustBind(t, s, ctx, "sess-old", agent.ID, "runner-1")

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
	if n := countBindings(t, s, agent.ID); n != 1 {
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

	mustBind(t, s, ctx, "sess-1", agent.ID, "runner-1")
	displaced := mustBind(t, s, ctx, "sess-1", agent.ID, "runner-2")
	if displaced != "sess-1" {
		t.Fatalf("re-binding the same session displaced %q, want %q — the account's previous session is this same session", displaced, "sess-1")
	}

	if n := countBindings(t, s, agent.ID); n != 1 {
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

	mustBind(t, s, ctx, "sess-1", agentA.ID, "runner-1")

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

	mustBind(t, s, ctx, "sess-1", agent.ID, "runner-1")
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
	if displaced := mustBind(t, s, ctx, "sess-2", agent.ID, "runner-1"); displaced != "" {
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
		mustBind(t, s, ctx, bind.SessionID, bind.AccountID, bind.RunnerID)
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

	mustBind(t, s, ctx, "sess-1", agent.ID, "runner-1")
	createdBefore, updatedBefore := bindingTimes(t, s, "sess-1")
	if !createdBefore.Equal(updatedBefore) {
		t.Fatalf("on INSERT created_at = %v and updated_at = %v, want the same DEFAULT now()", createdBefore, updatedBefore)
	}

	mustBind(t, s, ctx, "sess-1", agent.ID, "runner-2")
	createdAfter, updatedAfter := bindingTimes(t, s, "sess-1")

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
	mustBind(t, s, ctxA, "sess-a", agentA.ID, "runner-1")

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
