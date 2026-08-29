//go:build pgtest

package store

// account_handles resolution index contracts (RIG-2751 handle cutover, T0 + T2).
//
// T0 — the storage shape and its two partial-unique indexes: user/system handles
// are globally unique, agent handles are unique only per owner, an agent handle
// may overlap a global user handle, and both tiers rename-in-place and reclaim.
// The uniqueness is enforced by the two partial-unique indexes authored into
// 0001_init.sql; CreateUser/CreateAgent write the rows, and a rename/reclaim is a
// direct UPDATE/DELETE against the row (no store rename API exists yet — the
// forward-looking write path is the index-level UPDATE these tests exercise).
//
// T2 — AccountsByHandles: owner-qualified and bare resolution, disambiguation by
// owner, bare-agent defaulting to callerOwner, the atomic all-or-nothing miss
// naming every unresolved handle, system exclusion, and the OQ-6 visibility clip
// (invisible ≡ unknown).
//
// context.Background() is the test root (test-root ctx exemption).

import (
	"context"
	"strings"
	"testing"
)

// ---- T0: uniqueness invariants ----

// TestUserHandleGloballyUnique: two user accounts cannot share a handle — the
// global (owner_user_id IS NULL) partial-unique index rejects the second.
func TestUserHandleGloballyUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateUser(ctx, NewUser{Handle: "matt", DisplayName: "Matt"}); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	_, err := s.CreateUser(ctx, NewUser{Handle: "matt", DisplayName: "Matt Two"})
	sentinelIs(t, err, ErrConflict, "second global user handle")
}

// TestAgentHandleUniquePerOwnerRejectsSecondSameOwner: two agents under the SAME
// owner cannot share a handle — the per-owner agent index rejects the second.
func TestAgentHandleUniquePerOwnerRejectsSecondSameOwner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "matt")
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "compass-ux", DisplayName: "UX"}); err != nil {
		t.Fatalf("first CreateAgent: %v", err)
	}
	_, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "compass-ux", DisplayName: "UX Two"})
	sentinelIs(t, err, ErrConflict, "second matt/compass-ux")
}

// TestAgentHandleCoexistsAcrossOwners: `matt/compass-ux` and `alice/compass-ux`
// coexist — the per-owner index keys on (owner_user_id, handle), so the same
// agent handle under different owners is two distinct rows.
func TestAgentHandleCoexistsAcrossOwners(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	alice := mustUser(t, s, "alice")
	mattUX := mustAgent(t, s, matt.ID, "compass-ux")
	aliceUX := mustAgent(t, s, alice.ID, "compass-ux")

	got, err := s.AgentByHandle(ctx, matt.ID, "compass-ux")
	if err != nil {
		t.Fatalf("AgentByHandle(matt, compass-ux): %v", err)
	}
	if got.ID != mattUX.ID {
		t.Fatalf("matt/compass-ux resolved to %q, want %q", got.ID, mattUX.ID)
	}
	got, err = s.AgentByHandle(ctx, alice.ID, "compass-ux")
	if err != nil {
		t.Fatalf("AgentByHandle(alice, compass-ux): %v", err)
	}
	if got.ID != aliceUX.ID {
		t.Fatalf("alice/compass-ux resolved to %q, want %q", got.ID, aliceUX.ID)
	}
}

// TestAgentHandleMayOverlapUserHandle: an agent handle may equal a global user
// handle with no collision — a user is looked up bare (global index), an agent
// owner-qualified (per-owner index); the two indexes never contend.
func TestAgentHandleMayOverlapUserHandle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// A global user named "atlas".
	user := mustUser(t, s, "atlas")
	// An agent also named "atlas" under an owner — no collision.
	owner := mustUser(t, s, "owner")
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "atlas", DisplayName: "Atlas Agent"})
	if err != nil {
		t.Fatalf("CreateAgent(atlas) overlapping user handle: %v", err)
	}
	if agent.ID == user.ID {
		t.Fatal("agent and user share an id; they must be distinct accounts")
	}
	// The user resolves bare in the global index; the agent owner-qualified.
	gotUser, err := s.UserByHandle(ctx, "atlas")
	if err != nil || gotUser.ID != user.ID {
		t.Fatalf("UserByHandle(atlas) = (%v, %v), want the user %q", gotUser.ID, err, user.ID)
	}
	gotAgent, err := s.AgentByHandle(ctx, owner.ID, "atlas")
	if err != nil || gotAgent.ID != agent.ID {
		t.Fatalf("AgentByHandle(owner, atlas) = (%v, %v), want the agent %q", gotAgent.ID, err, agent.ID)
	}
}

// TestHandleRenameInPlaceBothTiers: an in-place rename (UPDATE account_handles)
// frees the old handle and claims the new one, for both a user (global index)
// and an agent (per-owner index). Simulates the forward-looking rename write path
// (no store rename API exists yet) against the resolution index directly.
func TestHandleRenameInPlaceBothTiers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := mustUser(t, s, "oldname")
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "old-agent")

	// User rename (global tier).
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(user.ID), "newname"); err != nil {
		t.Fatalf("rename user handle: %v", err)
	}
	if _, err := s.UserByHandle(ctx, "oldname"); err == nil {
		t.Fatal("old user handle still resolves after rename")
	}
	got, err := s.UserByHandle(ctx, "newname")
	if err != nil || got.ID != user.ID {
		t.Fatalf("UserByHandle(newname) = (%v, %v), want %q", got.ID, err, user.ID)
	}

	// Agent rename (per-owner tier).
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(agent.ID), "new-agent"); err != nil {
		t.Fatalf("rename agent handle: %v", err)
	}
	if _, err := s.AgentByHandle(ctx, owner.ID, "old-agent"); err == nil {
		t.Fatal("old agent handle still resolves after rename")
	}
	gotA, err := s.AgentByHandle(ctx, owner.ID, "new-agent")
	if err != nil || gotA.ID != agent.ID {
		t.Fatalf("AgentByHandle(owner, new-agent) = (%v, %v), want %q", gotA.ID, err, agent.ID)
	}
}

// TestHandleReclaimBothTiers: a freed handle (rename away) may be re-registered
// by a NEW account, for both tiers — no tombstone, no reservation.
func TestHandleReclaimBothTiers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := mustUser(t, s, "matt")
	// Free the user handle by renaming it away, then a new user reclaims "matt".
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(user.ID), "matt-old"); err != nil {
		t.Fatalf("rename away: %v", err)
	}
	reclaimer, err := s.CreateUser(ctx, NewUser{Handle: "matt", DisplayName: "New Matt"})
	if err != nil {
		t.Fatalf("reclaim user handle: %v", err)
	}
	got, err := s.UserByHandle(ctx, "matt")
	if err != nil || got.ID != reclaimer.ID {
		t.Fatalf("UserByHandle(matt) after reclaim = (%v, %v), want %q", got.ID, err, reclaimer.ID)
	}

	// Agent tier: free `owner/ux`, a new agent reclaims it under the same owner.
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "ux")
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(agent.ID), "ux-old"); err != nil {
		t.Fatalf("rename agent away: %v", err)
	}
	reAgent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "ux", DisplayName: "New UX"})
	if err != nil {
		t.Fatalf("reclaim agent handle: %v", err)
	}
	gotA, err := s.AgentByHandle(ctx, owner.ID, "ux")
	if err != nil || gotA.ID != reAgent.ID {
		t.Fatalf("AgentByHandle(owner, ux) after reclaim = (%v, %v), want %q", gotA.ID, err, reAgent.ID)
	}
}

// ---- T2: AccountsByHandles resolver ----

// qh is a terse QualifiedHandle constructor mirroring ParseQualifiedHandle, so a
// test can state the parsed shape directly.
func qh(raw string) QualifiedHandle { return ParseQualifiedHandle(raw) }

// TestAccountsByHandlesRoundTripAndDisambiguation: a bare user handle resolves in
// the global index, a bare agent handle in the CALLER's own owner namespace, and
// an owner-qualified handle resolves in the NAMED owner's namespace (matt/… picks
// matt's agent, never alice's homonym). All targets are visible to the viewer —
// the OQ-6 clip on cross-owner-qualified resolution is exercised separately in
// TestAccountsByHandlesInvisibleEqualsUnknown; owner-qualified disambiguation
// WITHOUT a visibility clip is TestAgentHandleCoexistsAcrossOwners.
func TestAccountsByHandlesRoundTripAndDisambiguation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	alice := mustUser(t, s, "alice")
	mattUX := mustAgent(t, s, matt.ID, "compass-ux")
	// alice also owns a "compass-ux" — matt's owner-qualified lookup must pick
	// matt's, proving the owner segment is the disambiguator.
	mustAgent(t, s, alice.ID, "compass-ux")

	// viewer=matt (a user sees every user + its own agents), callerOwner=matt.
	// matt/compass-ux (own agent, visible) + bare alice (global user).
	got, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{
		qh("matt/compass-ux"), qh("alice"),
	})
	if err != nil {
		t.Fatalf("AccountsByHandles: %v", err)
	}
	if got["matt/compass-ux"] != mattUX.ID {
		t.Errorf("matt/compass-ux = %q, want matt's %q (not alice's homonym)", got["matt/compass-ux"], mattUX.ID)
	}
	if got["alice"] != alice.ID {
		t.Errorf("bare alice = %q, want the user %q", got["alice"], alice.ID)
	}

	// A bare agent handle from a matt-owned caller defaults to matt's namespace.
	bare, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{qh("compass-ux")})
	if err != nil {
		t.Fatalf("AccountsByHandles(bare agent): %v", err)
	}
	if bare["compass-ux"] != mattUX.ID {
		t.Errorf("bare compass-ux = %q, want matt's %q (callerOwner default)", bare["compass-ux"], mattUX.ID)
	}
}

// TestAccountsByHandlesBareUserAndSystemGlobal: a bare user handle resolves in the
// global index; the system account is EXCLUDED (never a member/owner target).
func TestAccountsByHandlesBareUserAndSystemGlobal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	if _, err := s.EnsureSystemAccount(ctx); err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}

	got, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{qh("matt")})
	if err != nil {
		t.Fatalf("AccountsByHandles(matt): %v", err)
	}
	if got["matt"] != matt.ID {
		t.Errorf("bare matt = %q, want %q", got["matt"], matt.ID)
	}

	// The system handle is never a resolvable member/owner target.
	_, err = s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{qh(SystemAccountHandle)})
	sentinelIs(t, err, ErrNotFound, "system handle as a member/owner target")
}

// TestAccountsByHandlesAtomicMissNamesAll: one unresolved handle fails the whole
// call, and the error names EVERY unresolved handle in its submitted spelling
// (OQ-2 atomic), never the resolved ones.
func TestAccountsByHandlesAtomicMissNamesAll(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")

	_, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{
		qh("matt"), qh("ghost-one"), qh("ghost-two"),
	})
	sentinelIs(t, err, ErrNotFound, "atomic miss")
	if !strings.Contains(err.Error(), "ghost-one") || !strings.Contains(err.Error(), "ghost-two") {
		t.Fatalf("error %q must name BOTH unresolved handles (ghost-one, ghost-two)", err)
	}
	if strings.Contains(err.Error(), "matt") {
		t.Fatalf("error %q must NOT name the resolved handle (matt)", err)
	}
}

// TestAccountsByHandlesInvisibleEqualsUnknown (OQ-6 SCOPED): a real agent the
// viewer cannot see misses exactly like an unknown handle — the visibility clip
// intersects the resolution.
func TestAccountsByHandlesInvisibleEqualsUnknown(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	alice := mustUser(t, s, "alice")
	// alice's agent, which matt shares no channel with → invisible to matt.
	mustAgent(t, s, alice.ID, "secret")

	// viewer=matt, resolving alice/secret: the agent is real but invisible to
	// matt, so it misses like an unknown handle.
	_, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{qh("alice/secret")})
	sentinelIs(t, err, ErrNotFound, "invisible agent ≡ unknown")

	// Contrast: alice herself resolves alice/secret (she owns it → visible).
	got, err := s.AccountsByHandles(ctx, alice.ID, alice.ID, []QualifiedHandle{qh("alice/secret")})
	if err != nil {
		t.Fatalf("AccountsByHandles(alice sees own agent): %v", err)
	}
	if got["alice/secret"] == "" {
		t.Fatal("alice cannot resolve her own agent alice/secret")
	}
}

// TestAccountsByHandlesEmptyInputNoOp: empty input is a no-op — an empty map and
// a nil error, never a spurious miss.
func TestAccountsByHandlesEmptyInputNoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	got, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, nil)
	if err != nil {
		t.Fatalf("AccountsByHandles(empty) = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("AccountsByHandles(empty) = %v, want empty map", got)
	}
}
