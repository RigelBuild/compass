//go:build pgtest

package store

// Agent-tree store contracts (Record C, T2/T3): parent_agent_id round-trips
// through CreateAgent (set + empty=NULL) and every projection, and ReparentAgent
// is the serialized validate-and-write — happy path, promote-to-root, and each
// §Server validation clause mapped to its sentinel (caller authority /
// same-owner → ErrPermissionDenied; cycle → ErrFailedPrecondition; missing
// parent → ErrNotFound).

import (
	"context"
	"testing"
	"time"
)

// mustAgentWithParent creates an owned agent whose parent is set at creation, or
// fails the test. The set-at-creation path is CreateAgent + NewAgent.ParentAgentID.
func mustAgentWithParent(t *testing.T, s *Store, owner, parent AccountID, handle string) Account {
	t.Helper()
	a, err := s.CreateAgent(context.Background(), owner, NewAgent{
		Handle:        handle,
		DisplayName:   handle,
		ParentAgentID: parent,
	})
	if err != nil {
		t.Fatalf("CreateAgent(%q, parent=%q): %v", handle, parent, err)
	}
	return a
}

// readParent reads an agent's parent_agent_id straight from the row (COALESCE to
// empty) so the assertion checks the persisted edge, not a returned struct.
func readParent(t *testing.T, s *Store, agent AccountID) string {
	t.Helper()
	var parent string
	err := s.pool.QueryRow(context.Background(),
		"SELECT COALESCE(parent_agent_id, '') FROM agent_accounts WHERE account_id = $1",
		string(agent),
	).Scan(&parent)
	if err != nil {
		t.Fatalf("read parent_agent_id for %s: %v", agent, err)
	}
	return parent
}

func TestCreateAgentRoundTripsParent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	root := mustAgent(t, s, owner.ID, "root")

	// A root agent (no parent) persists NULL and reads back empty.
	if got := readParent(t, s, root.ID); got != "" {
		t.Fatalf("root agent parent = %q, want empty (NULL)", got)
	}
	if root.Agent.ParentAgentID != "" {
		t.Fatalf("returned root ParentAgentID = %q, want empty", root.Agent.ParentAgentID)
	}

	// A child created with an explicit parent persists it and reads it back.
	child := mustAgentWithParent(t, s, owner.ID, root.ID, "child")
	if child.Agent.ParentAgentID != root.ID {
		t.Fatalf("returned child ParentAgentID = %q, want %q", child.Agent.ParentAgentID, root.ID)
	}
	if got := readParent(t, s, child.ID); got != string(root.ID) {
		t.Fatalf("persisted child parent = %q, want %q", got, root.ID)
	}

	// A projection read (GetAccount) returns the parent too.
	got, err := s.GetAccount(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetAccount(child): %v", err)
	}
	if got.Agent == nil || got.Agent.ParentAgentID != root.ID {
		t.Fatalf("GetAccount child ParentAgentID = %+v, want %q", got.Agent, root.ID)
	}
}

func TestReparentAgentHappyPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")
	b := mustAgent(t, s, owner.ID, "b")

	// b starts a root; move it under a.
	acc, err := s.ReparentAgent(ctx, owner.ID, b.ID, a.ID)
	if err != nil {
		t.Fatalf("ReparentAgent(b under a): %v", err)
	}
	if acc.Agent == nil || acc.Agent.ParentAgentID != a.ID {
		t.Fatalf("returned account ParentAgentID = %+v, want %q", acc.Agent, a.ID)
	}
	if got := readParent(t, s, b.ID); got != string(a.ID) {
		t.Fatalf("persisted b parent = %q, want %q", got, a.ID)
	}
}

func TestReparentAgentPromoteToRoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")
	b := mustAgentWithParent(t, s, owner.ID, a.ID, "b")

	// Promote b to a root (empty new parent).
	acc, err := s.ReparentAgent(ctx, owner.ID, b.ID, "")
	if err != nil {
		t.Fatalf("ReparentAgent(b to root): %v", err)
	}
	if acc.Agent.ParentAgentID != "" {
		t.Fatalf("returned account ParentAgentID = %q, want empty", acc.Agent.ParentAgentID)
	}
	if got := readParent(t, s, b.ID); got != "" {
		t.Fatalf("persisted b parent = %q, want empty (NULL)", got)
	}
}

func TestReparentAgentByOwnersAgentIsAuthorized(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")
	b := mustAgent(t, s, owner.ID, "b")
	mover := mustAgent(t, s, owner.ID, "mover")

	// Clause 0: an agent of the owner may re-parent a sibling the owner owns.
	if _, err := s.ReparentAgent(ctx, mover.ID, b.ID, a.ID); err != nil {
		t.Fatalf("ReparentAgent by owner's agent: %v", err)
	}
	if got := readParent(t, s, b.ID); got != string(a.ID) {
		t.Fatalf("persisted b parent = %q, want %q", got, a.ID)
	}
}

func TestReparentAgentSelfParentRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")

	// Clause 2: an agent cannot be its own parent (the degenerate cycle).
	_, err := s.ReparentAgent(ctx, owner.ID, a.ID, a.ID)
	sentinelIs(t, err, ErrFailedPrecondition, "self-parent")
}

func TestReparentAgentCycleRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")
	b := mustAgentWithParent(t, s, owner.ID, a.ID, "b")
	c := mustAgentWithParent(t, s, owner.ID, b.ID, "c")

	// Clause 2: moving a under c would make a its own descendant's child
	// (a -> b -> c -> a). The parent-chain walk from c reaches a and rejects.
	_, err := s.ReparentAgent(ctx, owner.ID, a.ID, c.ID)
	sentinelIs(t, err, ErrFailedPrecondition, "cycle")

	// The rejected move left the tree untouched.
	if got := readParent(t, s, a.ID); got != "" {
		t.Fatalf("a parent after rejected cycle = %q, want empty", got)
	}
}

func TestReparentAgentMissingParentRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")

	// Clause 3: a non-empty parent that does not resolve to an agent account.
	_, err := s.ReparentAgent(ctx, owner.ID, a.ID, "no-such-agent")
	sentinelIs(t, err, ErrNotFound, "missing parent")
}

func TestReparentAgentCrossOwnerParentRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	other := mustUser(t, s, "other")
	a := mustAgent(t, s, owner.ID, "a")
	foreign := mustAgent(t, s, other.ID, "foreign")

	// Clause 1: a parent under a different owner is refused (PermissionDenied),
	// not leaked as not-found.
	_, err := s.ReparentAgent(ctx, owner.ID, a.ID, foreign.ID)
	sentinelIs(t, err, ErrPermissionDenied, "cross-owner parent")
}

func TestReparentAgentForeignCallerRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	other := mustUser(t, s, "other")
	a := mustAgent(t, s, owner.ID, "a")
	b := mustAgent(t, s, owner.ID, "b")
	intruder := mustAgent(t, s, other.ID, "intruder")

	// Clause 0: a caller whose resolved owner differs from the moved agent's
	// owner may not re-parent it — even to a legal parent under the target owner.
	_, err := s.ReparentAgent(ctx, intruder.ID, b.ID, a.ID)
	sentinelIs(t, err, ErrPermissionDenied, "foreign caller")
}

func TestReparentAgentUnknownAgentRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	// Clause 0 folds unknown-agent into the authority failure: the moved agent
	// has no owner to authorize against, so it is PermissionDenied (never a
	// distinct existence probe).
	_, err := s.ReparentAgent(ctx, owner.ID, "no-such-agent", "")
	sentinelIs(t, err, ErrPermissionDenied, "unknown moved agent")
}

func TestReparentAgentConcurrentCycleSerialized(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")
	b := mustAgent(t, s, owner.ID, "b")

	// tx1: hold the per-owner advisory lock and stage the A-under-B move, but do
	// not commit — the "first" reparent, mid-flight under the lock. lockKey is
	// the owner id (A/B are both owned by owner), matching ReparentAgent's key.
	tx1, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	// Rollback is a no-op after the Commit below; discarded because this cleanup
	// path only runs if the test fails before committing.
	defer func() { _ = tx1.Rollback(ctx) }()
	if _, err := tx1.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, string(owner.ID)); err != nil {
		t.Fatalf("tx1 acquire advisory lock: %v", err)
	}
	if _, err := tx1.Exec(ctx,
		"UPDATE agent_accounts SET parent_agent_id=$2 WHERE account_id=$1",
		string(a.ID), string(b.ID),
	); err != nil {
		t.Fatalf("tx1 move a under b: %v", err)
	}

	// tx2: the concurrent public reparent (B under A). With the advisory lock in
	// ReparentAgent present, this BLOCKS at the lock acquire until tx1 commits;
	// without it, tx2 races ahead and both moves persist into a cycle.
	type result struct {
		acc Account
		err error
	}
	done := make(chan result, 1)
	go func() {
		acc, err := s.ReparentAgent(ctx, owner.ID, b.ID, a.ID)
		done <- result{acc, err}
	}()

	// Readiness gate (bounded poll-until, not a fixed sleep): wait until tx2 is
	// actually parked on the advisory lock — an ungranted advisory lock row in
	// pg_locks. If it never appears within the deadline (the RED case where the
	// lock line was removed, so tx2 never waits), fall through and commit anyway.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitLoop:
	for {
		var waiters int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`,
		).Scan(&waiters); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiters >= 1 {
			break waitLoop
		}
		select {
		case <-deadline:
			break waitLoop
		case <-tick.C:
		}
	}

	// Release the lock: tx2 unblocks, re-reads under READ COMMITTED, now sees
	// A.parent = B, and its cycle walk from new-parent A reaches moved-agent B.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	res := <-done
	sentinelIs(t, res.err, ErrFailedPrecondition, "concurrent cycle")

	// The tree stayed acyclic: A-under-B persisted (tx1), B stayed a root because
	// tx2's cyclic move was rejected.
	if got := readParent(t, s, a.ID); got != string(b.ID) {
		t.Fatalf("a parent = %q, want %q (tx1's move persisted)", got, b.ID)
	}
	if got := readParent(t, s, b.ID); got != "" {
		t.Fatalf("b parent = %q, want empty (B stayed root, cyclic move rejected)", got)
	}
}

func TestCreateAgentUnknownParentIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	// A valid owner but a parent id that does not resolve to an agent account:
	// the parent FK fires and the store maps it to ErrNotFound (distinct from the
	// owner FK's ErrInvalidArgument). This exercises the store contract directly;
	// the comms pre-validation makes it unreachable via RPC.
	_, err := s.CreateAgent(ctx, owner.ID, NewAgent{
		Handle:        "orphan",
		DisplayName:   "orphan",
		ParentAgentID: "no-such-agent",
	})
	sentinelIs(t, err, ErrNotFound, "unknown parent")
}
