//go:build pgtest

package store

// Session bindings: the durable (session -> agent account, Runner) record the
// RunnerHub has so far held only in RAM (RIG-3108 / RIG-2861 §T4). Four things
// must hold, and all four are DATABASE invariants rather than Go logic — the PK,
// the unique index on agent_account_id, the FK to agent_accounts, and the RLS
// policy — so all four are pgtest-backed; a mock would only re-assert the Go.
//
// A binding is SINGULAR per session (a rebind replaces it), an account holds AT
// MOST ONE live session (the unique index), a miss FAILS CLOSED (a comms call
// resolves its scope through these reads, so an empty AccountID with a nil error
// would flow onward as a real principal), and the reconnect sweep RETURNS what
// it removed (the returned rows drive presence DISCONNECTED and the held-deliver
// reap — a bare DELETE would satisfy the invariant while dropping both).

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

// countBindings counts binding rows for an account, so a rebind test can prove
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
// session resolvable from the account (the delivery-dispatch read).
func TestRecordSessionBindingRoundTripsBothDirections(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding: %v", err)
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
	ctx := t.Context()
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

// TestRecordSessionBindingRebindsSessionInPlace pins the ON CONFLICT path: a
// session id names exactly ONE binding, so rebinding it onto a different Runner
// must REPLACE the row — updating account and runner TOGETHER — not add a
// second. If bindings accumulated, the reconnect sweep on the OLD Runner would
// retire a binding that has already moved, and SessionForAccount would resolve
// whichever of two rows Postgres happened to return.
func TestRecordSessionBindingRebindsSessionInPlace(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-1"); err != nil {
		t.Fatalf("first RecordSessionBinding: %v", err)
	}
	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-2"); err != nil {
		t.Fatalf("rebind onto runner-2: %v", err)
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

// TestRecordSessionBindingRejectsASecondSessionForABoundAccount proves the
// UNIQUE index on agent_account_id. The in-RAM accountSessions map this table
// replaces is 1:1, so a second concurrent session for one account would split
// that account's deliveries across two sessions and make SessionForAccount's
// answer depend on which row Postgres returned. The index refuses the write as
// ErrConflict rather than letting the double-bind land.
func TestRecordSessionBindingRejectsASecondSessionForABoundAccount(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-1"); err != nil {
		t.Fatalf("first binding: %v", err)
	}

	err := s.RecordSessionBinding(ctx, "sess-2", agent.ID, "runner-1")
	sentinelIs(t, err, ErrConflict, "a second live session for an already-bound account")

	// The refused write changed nothing: the original session still owns the
	// account, in both directions.
	gotSession, err := s.SessionForAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SessionForAccount after the refused bind: %v", err)
	}
	if gotSession != "sess-1" {
		t.Fatalf("the agent resolves to session %q, want the original %q", gotSession, "sess-1")
	}
	if _, err := s.ResolveSessionAccount(ctx, "sess-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveSessionAccount(sess-2) err = %v, want ErrNotFound — the refused bind must not have landed", err)
	}
}

// TestRecordSessionBindingUnknownAgentIsInvalidArgument pins the FK: a binding
// for an account that is not an agent cannot land. Without it a session could
// resolve to a nonexistent principal, and every authz check downstream would be
// evaluating an account that does not exist.
func TestRecordSessionBindingUnknownAgentIsInvalidArgument(t *testing.T) {
	s := newTestStore(t)

	err := s.RecordSessionBinding(t.Context(), "sess-1", "no-such-agent", "runner-1")
	sentinelIs(t, err, ErrInvalidArgument, "binding for an unknown agent")
}

// TestDeleteSessionBindingReleasesAndIsIdempotent covers the single-session
// release path: the delete removes the row (both directions stop resolving, and
// the account's unique slot is free for its next session), and a SECOND delete
// succeeds. Idempotency is load-bearing — a session teardown may be retried, and
// an error on zero rows would strand the retried teardown mid-way.
func TestDeleteSessionBindingReleasesAndIsIdempotent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding: %v", err)
	}
	if err := s.DeleteSessionBinding(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSessionBinding: %v", err)
	}

	if _, err := s.ResolveSessionAccount(ctx, "sess-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveSessionAccount after delete err = %v, want ErrNotFound", err)
	}
	if _, err := s.SessionForAccount(ctx, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionForAccount after delete err = %v, want ErrNotFound", err)
	}

	// The released slot is reusable: the same agent may bind a NEW session
	// without hitting the unique-index conflict.
	if err := s.RecordSessionBinding(ctx, "sess-2", agent.ID, "runner-1"); err != nil {
		t.Fatalf("binding a new session after release: %v (want nil — the slot was freed)", err)
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
func TestDeleteSessionBindingsForRunnerReturnsEverySweptBinding(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "agent-a")
	b := mustAgent(t, s, owner.ID, "agent-b")
	elsewhere := mustAgent(t, s, owner.ID, "agent-elsewhere")

	for _, bind := range []SessionBinding{
		{SessionID: "sess-a", AccountID: a.ID, RunnerID: "runner-1"},
		{SessionID: "sess-b", AccountID: b.ID, RunnerID: "runner-1"},
		{SessionID: "sess-elsewhere", AccountID: elsewhere.ID, RunnerID: "runner-2"},
	} {
		if err := s.RecordSessionBinding(ctx, bind.SessionID, bind.AccountID, bind.RunnerID); err != nil {
			t.Fatalf("RecordSessionBinding(%+v): %v", bind, err)
		}
	}

	swept, err := s.DeleteSessionBindingsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner: %v", err)
	}

	// The returned rows ARE the side-effect inputs, so assert them exactly —
	// every swept binding, with the account each DISCONNECTED edge needs.
	want := []SessionBinding{
		{SessionID: "sess-a", AccountID: a.ID, RunnerID: "runner-1"},
		{SessionID: "sess-b", AccountID: b.ID, RunnerID: "runner-1"},
	}
	if len(swept) != len(want) {
		t.Fatalf("swept = %+v, want exactly the %d bindings on runner-1 (a :exec sweep would return none)", swept, len(want))
	}
	for i, w := range want {
		if swept[i] != w {
			t.Fatalf("swept[%d] = %+v, want %+v (sorted by session id)", i, swept[i], w)
		}
	}

	// The swept bindings are actually gone in both directions.
	for _, sessionID := range []string{"sess-a", "sess-b"} {
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
// trigger fires through a real store method: the ON CONFLICT rebind advances
// updated_at while created_at stays put. It is also the only proof the column is
// live at all — secrets.updated_at rotted precisely because no write statement
// set it, so the value could only ever equal created_at and every reader was
// reading a lie.
//
// No time.Sleep: now() is TRANSACTION time in Postgres, so the read-then-rebind
// below spans two transactions and the two values differ on their own.
func TestRecordSessionBindingTriggerAdvancesUpdatedAtOnly(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-1"); err != nil {
		t.Fatalf("first RecordSessionBinding: %v", err)
	}
	createdBefore, updatedBefore := bindingTimes(t, s, "sess-1")
	if !createdBefore.Equal(updatedBefore) {
		t.Fatalf("on INSERT created_at = %v and updated_at = %v, want the same DEFAULT now()", createdBefore, updatedBefore)
	}

	if err := s.RecordSessionBinding(ctx, "sess-1", agent.ID, "runner-2"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
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
	ctxA := t.Context() // no tenant set → the bootstrap tenant
	ctxB := WithTenant(t.Context(), tenantB)

	ownerA := mustUser(t, s, "owner-a")
	agentA := mustAgent(t, s, ownerA.ID, "agent-a")
	if err := s.RecordSessionBinding(ctxA, "sess-a", agentA.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding under tenant A: %v", err)
	}

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
