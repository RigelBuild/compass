//go:build pgtest

package store

import (
	"context"
	"sort"
	"testing"
)

// Agent-tree READ contracts (Record C, T2): AgentSubtree walks transitive
// descendants, AgentNeighborhood returns parent + siblings (self-inclusive) +
// children, AgentsByOwner returns the owner's flat agent set, and none of the
// three ever leaks an unrelated / non-owned agent.

// idSet collects the account ids of a []Account into a set for order-free
// membership assertions (the reads order by id, but the tests assert content).
func idSet(accs []Account) map[AccountID]bool {
	set := make(map[AccountID]bool, len(accs))
	for _, a := range accs {
		set[a.ID] = true
	}
	return set
}

// assertIDs fails unless got holds exactly the wanted ids (no more, no less).
func assertIDs(t *testing.T, what string, got []Account, want ...AccountID) {
	t.Helper()
	set := idSet(got)
	if len(set) != len(got) {
		t.Fatalf("%s returned duplicate ids: %v", what, ids(got))
	}
	wantSet := make(map[AccountID]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	for id := range set {
		if !wantSet[id] {
			t.Fatalf("%s returned unexpected id %q; got %v want %v", what, id, ids(got), want)
		}
	}
	for _, id := range want {
		if !set[id] {
			t.Fatalf("%s missing id %q; got %v want %v", what, id, ids(got), want)
		}
	}
}

func ids(accs []Account) []string {
	out := make([]string, len(accs))
	for i, a := range accs {
		out[i] = string(a.ID)
	}
	sort.Strings(out)
	return out
}

// buildTree builds owner U's tree — A(root), B(child of A), C(child of A),
// D(child of B) — plus an unrelated agent X under a DIFFERENT owner, and returns
// the ids. Parents are set at creation via mustAgentWithParent.
func buildTree(t *testing.T, s *Store) (u, a, b, c, d, otherOwner, x AccountID) {
	t.Helper()
	owner := mustUser(t, s, "owner")
	agentA := mustAgent(t, s, owner.ID, "a")
	agentB := mustAgentWithParent(t, s, owner.ID, agentA.ID, "b")
	agentC := mustAgentWithParent(t, s, owner.ID, agentA.ID, "c")
	agentD := mustAgentWithParent(t, s, owner.ID, agentB.ID, "d")

	other := mustUser(t, s, "other")
	agentX := mustAgent(t, s, other.ID, "x")

	return owner.ID, agentA.ID, agentB.ID, agentC.ID, agentD.ID, other.ID, agentX.ID
}

func TestAgentSubtree(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, a, b, c, d, _, x := buildTree(t, s)

	subA, err := s.AgentSubtree(ctx, a)
	if err != nil {
		t.Fatalf("AgentSubtree(a): %v", err)
	}
	assertIDs(t, "AgentSubtree(a)", subA, a, b, c, d)
	if idSet(subA)[x] {
		t.Fatalf("AgentSubtree(a) leaked unrelated agent x")
	}

	subB, err := s.AgentSubtree(ctx, b)
	if err != nil {
		t.Fatalf("AgentSubtree(b): %v", err)
	}
	assertIDs(t, "AgentSubtree(b)", subB, b, d)
}

func TestAgentNeighborhood(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, a, b, c, d, _, x := buildTree(t, s)

	// B's neighborhood: parent A, siblings under A (self-inclusive: B and C),
	// and B's own child D.
	nbr, err := s.AgentNeighborhood(ctx, b)
	if err != nil {
		t.Fatalf("AgentNeighborhood(b): %v", err)
	}
	assertIDs(t, "AgentNeighborhood(b)", nbr, a, b, c, d)
	if idSet(nbr)[x] {
		t.Fatalf("AgentNeighborhood(b) leaked unrelated agent x")
	}
}

// TestAgentNeighborhoodOfRoot pins the deliberately BROAD raw behavior of a root
// seed: a root's parent is NULL, so the sibling clause (IS NOT DISTINCT FROM NULL)
// matches every root across ALL owners. AgentNeighborhood(a) therefore returns
// a's children (b, c) AND the other-owner root x — cross-owner breadth by design,
// which the roster handler removes with its accountVisibleFromWhere clip. This
// test documents that contract at the store layer: it would redden if the
// root-sibling clause were narrowed (e.g. owner-scoped) so the clip decision is
// not silently moved into the store.
func TestAgentNeighborhoodOfRoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, a, b, c, d, _, x := buildTree(t, s)

	// a is a root (parent NULL); x is a root under a DIFFERENT owner.
	nbr, err := s.AgentNeighborhood(ctx, a)
	if err != nil {
		t.Fatalf("AgentNeighborhood(a): %v", err)
	}
	// a's children b, c + a itself, plus x (all roots are co-siblings on the raw
	// read). d (a grandchild, not a's child and not a root) is NOT included.
	assertIDs(t, "AgentNeighborhood(a)", nbr, a, b, c, x)
	if idSet(nbr)[d] {
		t.Fatalf("AgentNeighborhood(a) included grandchild d")
	}
}

func TestAgentsByOwner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, a, b, c, d, other, x := buildTree(t, s)

	owned, err := s.AgentsByOwner(ctx, u)
	if err != nil {
		t.Fatalf("AgentsByOwner(u): %v", err)
	}
	assertIDs(t, "AgentsByOwner(u)", owned, a, b, c, d)
	if idSet(owned)[x] {
		t.Fatalf("AgentsByOwner(u) leaked other-owner agent x")
	}

	otherOwned, err := s.AgentsByOwner(ctx, other)
	if err != nil {
		t.Fatalf("AgentsByOwner(other): %v", err)
	}
	assertIDs(t, "AgentsByOwner(other)", otherOwned, x)
}
