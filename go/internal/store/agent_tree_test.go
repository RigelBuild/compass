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
