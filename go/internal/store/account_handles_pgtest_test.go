//go:build pgtest

package store

// account_handles resolution index contracts (RIG-2751 handle cutover, T0 + T2).
//
// T0 — the storage shape and its two partial-unique indexes: user/system handles
// are globally unique, agent handles are unique only per owner, an agent handle
// may overlap a global user handle, and both tiers rename-in-place and reclaim.
// The uniqueness is enforced by the two partial-unique indexes authored into
// 0001_init.sql; CreateUser/CreateAgent write the rows and RenameHandle renames
// them in place.
//
// T2 — AccountsByHandles: owner-qualified and bare resolution, disambiguation by
// owner, bare-agent defaulting to callerOwner, the atomic all-or-nothing miss
// naming every unresolved handle, system exclusion, and the OQ-6 visibility clip
// (invisible ≡ unknown).
//
// context.Background() is the test root (test-root ctx exemption).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/pgtest"
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

// TestHandleRenameInPlaceBothTiers: rename updates both the resolution key and
// the accounts.handle display copy, while freeing the previous handle.
//
// Mutation: skipping RenameAccountDisplayHandle leaves accounts.handle stale;
// the Account.Handle assertions below then fail.
func TestHandleRenameInPlaceBothTiers(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	user := mustUser(t, s, "oldname")
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "old-agent")

	// User rename (global tier).
	got, _, err := s.RenameHandle(ctx, user.ID, "newname")
	if err != nil || got.Handle != "newname" {
		t.Fatalf("RenameHandle(user, newname) = (%+v, %v), want handle newname", got, err)
	}
	if _, err := s.UserByHandle(ctx, "oldname"); err == nil {
		t.Fatal("old user handle still resolves after rename")
	}
	got, err = s.UserByHandle(ctx, "newname")
	if err != nil || got.ID != user.ID {
		t.Fatalf("UserByHandle(newname) = (%v, %v), want %q", got.ID, err, user.ID)
	}
	assertAccountDisplayHandle(t, ctx, s, user.ID, "newname")

	// Agent rename (per-owner tier).
	gotA, _, err := s.RenameHandle(ctx, agent.ID, "new-agent")
	if err != nil || gotA.Handle != "new-agent" {
		t.Fatalf("RenameHandle(agent, new-agent) = (%+v, %v), want handle new-agent", gotA, err)
	}
	if _, err := s.AgentByHandle(ctx, owner.ID, "old-agent"); err == nil {
		t.Fatal("old agent handle still resolves after rename")
	}
	gotA, err = s.AgentByHandle(ctx, owner.ID, "new-agent")
	if err != nil || gotA.ID != agent.ID {
		t.Fatalf("AgentByHandle(owner, new-agent) = (%v, %v), want %q", gotA.ID, err, agent.ID)
	}
	assertAccountDisplayHandle(t, ctx, s, agent.ID, "new-agent")
}

// TestRenameHandleRekeysSameOwnerDMWithoutChangingMembership proves an agent
// rename keeps the channel ID and parties while deriving its name from the new
// handle.
func TestRenameHandleRekeysSameOwnerDMWithoutChangingMembership(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "dm-owner")
	alice := mustAgent(t, s, owner.ID, "alice")
	bob := mustAgent(t, s, owner.ID, "bob")
	var channelID ChannelID
	if err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := LockOwnerDMTx(ctx, tx, owner.ID); err != nil {
			return err
		}
		group, err := s.EnsureOwnerDMGroupTx(ctx, tx, owner.ID)
		if err != nil {
			return err
		}
		channelID, _, err = s.UpsertDMChannelTx(ctx, tx, DMChannelSpec{
			GroupID: group,
			Name:    DMChannelName(alice.Handle, bob.Handle),
			Members: []AccountID{alice.ID, bob.ID},
		})
		return err
	}); err != nil {
		t.Fatalf("open DM: %v", err)
	}
	renamed, rekeyed, err := s.RenameHandle(ctx, alice.ID, "alice-new")
	if err != nil {
		t.Fatalf("RenameHandle: %v", err)
	}
	if renamed.Handle != "alice-new" || len(rekeyed) != 1 || rekeyed[0] != channelID {
		t.Fatalf("RenameHandle = (%q, %v), want handle alice-new and the same DM ID %q", renamed.Handle, rekeyed, channelID)
	}
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if channel.Name != DMChannelName("alice-new", bob.Handle) {
		t.Fatalf("renamed DM name = %q, want %q", channel.Name, DMChannelName("alice-new", bob.Handle))
	}
	if !slices.Contains(channel.MemberAccountIDs, alice.ID) || !slices.Contains(channel.MemberAccountIDs, bob.ID) || !slices.Contains(channel.MemberAccountIDs, owner.ID) {
		t.Fatalf("renamed DM members = %v, want both agents and their owner", channel.MemberAccountIDs)
	}
}

// TestHandleReclaimBothTiers: a freed handle may be re-registered at either tier.
func TestHandleReclaimBothTiers(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	user := mustUser(t, s, "matt")
	// Free the user handle by renaming it away, then a new user reclaims "matt".
	if _, _, err := s.RenameHandle(ctx, user.ID, "matt-old"); err != nil {
		t.Fatalf("rename user away: %v", err)
	}
	reclaimer, err := s.CreateUser(ctx, NewUser{Handle: "matt", DisplayName: "New Matt"})
	if err != nil {
		t.Fatalf("reclaim user handle: %v", err)
	}
	got, err := s.UserByHandle(ctx, "matt")
	if err != nil || got.ID != reclaimer.ID {
		t.Fatalf("UserByHandle(matt) after reclaim = (%v, %v), want %q", got.ID, err, reclaimer.ID)
	}
	assertAccountDisplayHandle(t, ctx, s, user.ID, "matt-old")

	// Agent tier: free `owner/ux`, a new agent reclaims it under the same owner.
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "ux")
	if _, _, err := s.RenameHandle(ctx, agent.ID, "ux-old"); err != nil {
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
	assertAccountDisplayHandle(t, ctx, s, agent.ID, "ux-old")
}

// TestRenameHandleCollisionsRespectHandleTiers proves conflicts remain local to
// each namespace and a failed update leaves both accounts unchanged.
//
// Mutation: skipping mapAccountHandleUniqueViolation changes ErrConflict into a
// raw database error; the first assertion then fails.
func TestRenameHandleCollisionsRespectHandleTiers(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	createUser := func(handle string) Account {
		t.Helper()
		account, err := s.CreateUser(ctx, NewUser{Handle: handle, DisplayName: handle})
		if err != nil {
			t.Fatalf("CreateUser(%q): %v", handle, err)
		}
		return account
	}
	createAgent := func(owner AccountID, handle string) Account {
		t.Helper()
		account, err := s.CreateAgent(ctx, owner, NewAgent{Handle: handle, DisplayName: handle})
		if err != nil {
			t.Fatalf("CreateAgent(%q): %v", handle, err)
		}
		return account
	}
	assertBothHandles := func(account Account, want string) {
		t.Helper()
		if got, err := s.AccountHandle(ctx, account.ID); err != nil || got != want {
			t.Fatalf("AccountHandle(%q) = (%q, %v), want %q", account.ID, got, err, want)
		}
		assertAccountDisplayHandle(t, ctx, s, account.ID, want)
	}

	t.Run("user collision rolls back", func(t *testing.T) {
		user := createUser("user-before")
		taken := createUser("user-taken")
		if _, _, err := s.RenameHandle(ctx, user.ID, taken.Handle); !errors.Is(err, ErrConflict) {
			t.Fatalf("RenameHandle(user, taken) = %v, want ErrConflict", err)
		}
		assertBothHandles(user, "user-before")
		assertBothHandles(taken, "user-taken")
	})

	t.Run("same owner agent collision rolls back", func(t *testing.T) {
		owner := createUser("same-owner")
		agent := createAgent(owner.ID, "agent-before")
		sibling := createAgent(owner.ID, "agent-taken")
		if _, _, err := s.RenameHandle(ctx, agent.ID, sibling.Handle); !errors.Is(err, ErrConflict) {
			t.Fatalf("RenameHandle(agent, sibling handle) = %v, want ErrConflict", err)
		}
		assertBothHandles(agent, "agent-before")
		assertBothHandles(sibling, "agent-taken")
	})

	t.Run("different owner agent handle succeeds", func(t *testing.T) {
		ownerA := createUser("owner-a")
		ownerB := createUser("owner-b")
		agent := createAgent(ownerA.ID, "agent-a")
		other := createAgent(ownerB.ID, "agent-b")
		got, _, err := s.RenameHandle(ctx, agent.ID, other.Handle)
		if err != nil || got.Handle != other.Handle {
			t.Fatalf("RenameHandle(agent, different owner handle) = (%+v, %v)", got, err)
		}
		assertBothHandles(agent, "agent-b")
		assertBothHandles(other, "agent-b")
	})

	t.Run("agent may use user handle", func(t *testing.T) {
		user := createUser("shared-name")
		owner := createUser("agent-owner")
		agent := createAgent(owner.ID, "agent-name")
		got, _, err := s.RenameHandle(ctx, agent.ID, user.Handle)
		if err != nil || got.Handle != user.Handle {
			t.Fatalf("RenameHandle(agent, user handle) = (%+v, %v)", got, err)
		}
		assertBothHandles(user, "shared-name")
		assertBothHandles(agent, "shared-name")
	})
}

// TestRenameHandleRejectsInvalidHandleAndUnknownID checks validation precedes
// the lookup and valid renames still report missing accounts.
func TestRenameHandleRejectsInvalidHandleAndUnknownID(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	user := mustUser(t, s, "rename-target")
	if _, _, err := s.RenameHandle(ctx, user.ID, "owner/agent"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("RenameHandle(qualified handle) = %v, want ErrInvalidArgument", err)
	}
	if _, _, err := s.RenameHandle(ctx, "missing-account", "valid-name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RenameHandle(unknown id) = %v, want ErrNotFound", err)
	}
}

func TestRenameHandleCurrentHandleIsNoop(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	user := mustUser(t, s, "same-name")
	got, _, err := s.RenameHandle(ctx, user.ID, user.Handle)
	if err != nil || got.ID != user.ID || got.Handle != user.Handle {
		t.Fatalf("RenameHandle(current handle) = (%+v, %v), want unchanged account", got, err)
	}
	assertAccountDisplayHandle(t, ctx, s, user.ID, user.Handle)
}

func assertAccountDisplayHandle(t *testing.T, ctx context.Context, s *Store, id AccountID, want string) {
	t.Helper()
	var got string
	if err := s.pool.QueryRow(ctx, "SELECT handle FROM accounts WHERE id = $1", string(id)).Scan(&got); err != nil {
		t.Fatalf("read accounts.handle for %q: %v", id, err)
	}
	if got != want {
		t.Fatalf("accounts.handle for %q = %q, want %q", id, got, want)
	}
}

// TestAccountHandleReadsResolutionIndex: AccountHandle returns the
// account_handles row, so it follows an index rename that leaves the
// accounts.handle display column behind; an unknown id is ErrNotFound.
//
// Mutation: reading accounts.handle instead returns the stale "matt" after the
// rename, reddening the "matt-renamed" assertion.
func TestAccountHandleReadsResolutionIndex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := mustUser(t, s, "matt")
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(user.ID), "matt-renamed"); err != nil {
		t.Fatalf("rename user handle: %v", err)
	}
	got, err := s.AccountHandle(ctx, user.ID)
	if err != nil || got != "matt-renamed" {
		t.Fatalf("AccountHandle(matt) = (%q, %v), want (%q, nil)", got, err, "matt-renamed")
	}
	_, err = s.AccountHandle(ctx, "no-such-account")
	sentinelIs(t, err, ErrNotFound, "AccountHandle(unknown)")
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

// TestAccountsByHandlesMalformedQualifierIsMiss (OQ-7 grammar): a separator with
// an empty owner, an empty agent segment, or a nested '/' is a clean miss naming
// the submitted spelling. "/alice" and "/compass-ux" must never fall back to the
// bare arm, where the user and the caller's own agent would resolve.
//
// Mutation: branching on a non-empty Owner instead of the separator resolves the
// two leading-'/' spellings bare and reddens their ErrNotFound assertions.
func TestAccountsByHandlesMalformedQualifierIsMiss(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	mustUser(t, s, "alice")
	mustAgent(t, s, matt.ID, "compass-ux")

	for _, raw := range []string{"/alice", "/compass-ux", "matt/compass-ux/x", "matt/", "/"} {
		t.Run(raw, func(t *testing.T) {
			got, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{qh(raw)})
			sentinelIs(t, err, ErrNotFound, raw)
			if got != nil {
				t.Fatalf("AccountsByHandles(%q) hits = %v, want none on an atomic miss", raw, got)
			}
			want := fmt.Sprintf("%v: handle %q", ErrNotFound, raw)
			if err.Error() != want {
				t.Fatalf("AccountsByHandles(%q) error = %q, want %q", raw, err.Error(), want)
			}
		})
	}
}

// TestAccountsByHandlesBareCollisionUserWins (OQ-7): when a user and the caller's
// own agent share a handle, the bare spelling resolves to the user and the
// owner-qualified spelling to the agent.
func TestAccountsByHandlesBareCollisionUserWins(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	matt := mustUser(t, s, "matt")
	user := mustUser(t, s, "atlas")
	agent := mustAgent(t, s, matt.ID, "atlas")

	got, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, []QualifiedHandle{qh("atlas"), qh("matt/atlas")})
	if err != nil {
		t.Fatalf("AccountsByHandles(atlas, matt/atlas): %v", err)
	}
	if got["atlas"] != user.ID {
		t.Errorf("bare atlas = %q, want the user %q (global index wins a bare collision)", got["atlas"], user.ID)
	}
	if got["matt/atlas"] != agent.ID {
		t.Errorf("matt/atlas = %q, want matt's agent %q", got["matt/atlas"], agent.ID)
	}
}

// TestAccountsByHandlesQueryCountBounded (§T2 "one query per namespace"): a mixed
// batch that hits every arm costs exactly three queries however many handles it
// carries, and still names every miss in submitted order.
//
// Mutation: a per-handle loop spends up to two queries per handle (10 here).
func TestAccountsByHandlesQueryCountBounded(t *testing.T) {
	ctx := context.Background()
	counter := &pgtest.SQLCQueryCounter{}
	s := newTracedTestStore(t, counter)
	matt := mustUser(t, s, "matt")
	mustUser(t, s, "alice")
	mustAgent(t, s, matt.ID, "ux")
	mustAgent(t, s, matt.ID, "ux-two")

	handles := []QualifiedHandle{
		qh("alice"),            // bare user
		qh("ux"),               // bare own agent
		qh("matt/ux-two"),      // qualified agent
		qh("ghost"),            // bare miss
		qh("nobody/ux"),        // unknown owner
		qh("matt/ghost-agent"), // qualified agent miss
	}
	counter.Reset()
	_, err := s.AccountsByHandles(ctx, matt.ID, matt.ID, handles)
	queries := counter.Count()

	want := fmt.Sprintf("%v: handle %q", ErrNotFound, "ghost, nobody/ux, matt/ghost-agent")
	if err == nil || err.Error() != want {
		t.Fatalf("AccountsByHandles error = %v, want %q", err, want)
	}
	if queries != 3 {
		t.Fatalf("AccountsByHandles ran %d queries for %d handles, want 3 (one per namespace)", queries, len(handles))
	}

	// No caller namespace: a bare handle that exists only as matt's agent must
	// miss, and the agent arm must not run at all.
	counter.Reset()
	_, err = s.AccountsByHandles(ctx, matt.ID, "", []QualifiedHandle{qh("ux")})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("AccountsByHandles(no callerOwner, ux) error = %v, want ErrNotFound", err)
	}
	if n := counter.Count(); n != 1 {
		t.Fatalf("AccountsByHandles(no callerOwner) ran %d queries, want 1 (global only)", n)
	}
}

func TestAccountsByHandlesOwnFleetAndPeeredOwners(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "owner-a")
	b := mustUser(t, s, "owner-b")
	aAgent := mustAgent(t, s, a.ID, "agent-a")
	aSibling := mustAgent(t, s, a.ID, "sibling-a")
	bAgent := mustAgent(t, s, b.ID, "agent-b")

	assertMissLikeUnknown := func(viewer, callerOwner AccountID, handle string) {
		t.Helper()
		_, gotErr := s.AccountsByHandles(ctx, viewer, callerOwner, []QualifiedHandle{ParseQualifiedHandle(handle)})
		_, unknownErr := s.AccountsByHandles(ctx, viewer, callerOwner, []QualifiedHandle{ParseQualifiedHandle("missing-agent")})
		if gotErr == nil || unknownErr == nil {
			t.Fatalf("AccountsByHandles(%q) error = %v, unknown error = %v; want not-found misses", handle, gotErr, unknownErr)
		}
		gotMessage := strings.Replace(gotErr.Error(), handle, "<handle>", 1)
		unknownMessage := strings.Replace(unknownErr.Error(), "missing-agent", "<handle>", 1)
		if gotMessage != unknownMessage {
			t.Fatalf("AccountsByHandles(%q) error = %v, unknown error = %v; want byte-identical misses modulo handle", handle, gotErr, unknownErr)
		}
	}

	assertMissLikeUnknown(a.ID, a.ID, "owner-b/agent-b")
	assertMissLikeUnknown(aAgent.ID, a.ID, "owner-b/agent-b")
	if _, err := s.ApprovePeer(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("ApprovePeer(a->b): %v", err)
	}
	assertMissLikeUnknown(a.ID, a.ID, "owner-b/agent-b")
	assertMissLikeUnknown(aAgent.ID, a.ID, "owner-b/agent-b")
	if _, err := s.ApprovePeer(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("ApprovePeer(b->a): %v", err)
	}
	for _, viewer := range []AccountID{a.ID, aAgent.ID} {
		got, err := s.AccountsByHandles(ctx, viewer, a.ID, []QualifiedHandle{ParseQualifiedHandle("owner-b/agent-b")})
		if err != nil || got["owner-b/agent-b"] != bAgent.ID {
			t.Fatalf("AccountsByHandles(%q, peered) = %v, %v; want %q", viewer, got, err, bAgent.ID)
		}
	}
	if _, err := s.RevokePeer(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("RevokePeer(b->a): %v", err)
	}
	assertMissLikeUnknown(a.ID, a.ID, "owner-b/agent-b")
	assertMissLikeUnknown(aAgent.ID, a.ID, "owner-b/agent-b")

	got, err := s.AccountsByHandles(ctx, aAgent.ID, a.ID, []QualifiedHandle{ParseQualifiedHandle("sibling-a")})
	if err != nil || got["sibling-a"] != aSibling.ID {
		t.Fatalf("AccountsByHandles(agent own sibling) = %v, %v; want %q", got, err, aSibling.ID)
	}
}

// TestAccountsByHandlesPeeringDoesNotLeakAnotherPair resolves only mutual peers of the viewer.
func TestAccountsByHandlesPeeringDoesNotLeakAnotherPair(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "peering-isolation-a")
	b := mustUser(t, s, "peering-isolation-b")
	c := mustUser(t, s, "peering-isolation-c")
	mustAgent(t, s, b.ID, "x")

	for _, edge := range [][2]AccountID{{b.ID, c.ID}, {c.ID, b.ID}} {
		if _, err := s.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	if _, err := s.AccountsByHandles(ctx, a.ID, a.ID, []QualifiedHandle{ParseQualifiedHandle("peering-isolation-b/x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AccountsByHandles(A, B/x) error = %v; want ErrNotFound when only B and C are peers", err)
	}
}
